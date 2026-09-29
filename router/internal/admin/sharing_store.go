package admin

import (
	"encoding/json"
	"net/http"
	"strconv"

	"llmesh/pkg/types"
	"llmesh/router/internal/scheduler"
	"strings"
	"sync"
	"sync/atomic"

	"llmesh/router/internal/authz"
)

// Capacity sharing (design §7): each client token's sharing setting, and the
// pairing decisions the scheduler asks for on every drain.
//
// The scheduler asks once per requester per client per drain, so answers are
// served from a cache that any change to access — roles, teams, policies,
// users, sharing, tokens — clears wholesale. Clearing is cheap and the cache
// refills on the next drain; being clever about which entries a change
// touches is not worth a stale permission.

type pairingCache struct {
	subjects sync.Map // requester owner → authz.Subject
	sharing  sync.Map // token hash → *authz.Sharing (nil entry = shared default)
	owners   sync.Map // token hash → owner column value
}

func (s *State) pairCache() *pairingCache {
	if c := s.pairingCache.Load(); c != nil {
		return c
	}
	c := &pairingCache{}
	if s.pairingCache.CompareAndSwap(nil, c) {
		return c
	}
	return s.pairingCache.Load()
}

// invalidateAccess drops cached pairing inputs after any access change.
func (s *State) invalidateAccess() { s.pairingCache.Store(nil) }

// requesterSubject is the subject a queued request runs as. Owner values are
// a username or "team:<id>" for a team key.
func (s *State) requesterSubject(owner string) authz.Subject {
	c := s.pairCache()
	if v, ok := c.subjects.Load(owner); ok {
		return v.(authz.Subject)
	}
	subj := s.keySubject(owner)
	c.subjects.Store(owner, subj)
	return subj
}

// ClientSharing returns a client token's sharing setting, or nil for the
// default (shared, owner preferred).
func (s *State) ClientSharing(tokenHash string) *authz.Sharing {
	c := s.pairCache()
	if v, ok := c.sharing.Load(tokenHash); ok {
		return v.(*authz.Sharing)
	}
	var raw string
	_ = s.db.QueryRow(`SELECT sharing FROM client_tokens WHERE token_hash = ?`, tokenHash).Scan(&raw)
	var sh *authz.Sharing
	if raw != "" {
		sh = &authz.Sharing{}
		if err := json.Unmarshal([]byte(raw), sh); err != nil {
			// Unreadable means a bad write; private is the safe reading.
			sh = &authz.Sharing{Mode: authz.SharePrivate}
		}
	}
	c.sharing.Store(tokenHash, sh)
	return sh
}

// SetClientSharing stores a client token's sharing setting.
func (s *State) SetClientSharing(tokenHash string, sh authz.Sharing) error {
	if err := sh.Validate(); err != nil {
		return err
	}
	b, _ := json.Marshal(sh)
	if _, err := s.db.Exec(`UPDATE client_tokens SET sharing = ? WHERE token_hash = ?`, string(b), tokenHash); err != nil {
		return err
	}
	s.invalidateAccess()
	return nil
}

// PairClient decides whether a queued request's owner may run on a client
// connected with tokenHash and owned by clientOwner.
func (s *State) PairClient(reqOwner, clientOwner, tokenHash string) authz.ClientPairing {
	e := s.Authz()
	if e == nil {
		return authz.ClientPairing{}
	}
	client := ownedResource("client", tokenHash, clientOwner)
	client.Sharing = s.ClientSharing(tokenHash)
	return e.PairClient(s.requesterSubject(reqOwner), client, authz.Context{})
}

// pairingCacheHolder is embedded in State.
type pairingCacheHolder struct {
	pairingCache atomic.Pointer[pairingCache]
}

// Isolation flags are policies now (design §10). The flags stay on the user
// row for display; these policies are what the scheduler enforces.

func isolationPolicyIDs(username string) (send, receive string) {
	return "isolation-send-" + username, "isolation-receive-" + username
}

func condFromJSON(s string) *authz.Cond {
	var c authz.Cond
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		panic("bad built-in condition: " + err.Error())
	}
	return &c
}

// isolationPolicies builds the deny policies equivalent to a user's
// isolation flags: send isolation keeps their requests on their own clients;
// receive isolation keeps their clients serving only them.
func isolationPolicies(username string) (send, receive authz.Policy) {
	p := userPrincipal(username)
	sendID, recvID := isolationPolicyIDs(username)
	quoted, _ := json.Marshal(p)
	send = authz.Policy{
		ID: sendID, Name: username + ": requests stay on own clients", Effect: authz.Deny, Enabled: true,
		Subject:   authz.SubjectMatcher{IDs: []string{p}},
		Actions:   []string{"client.use"},
		Resource:  authz.ResourceMatcher{Type: "client"},
		Condition: condFromJSON(`{"ne": ["resource.owner", ` + string(quoted) + `]}`),
	}
	receive = authz.Policy{
		ID: recvID, Name: username + ": clients serve only their owner", Effect: authz.Deny, Enabled: true,
		Actions:   []string{"client.use"},
		Resource:  authz.ResourceMatcher{Type: "client", Owners: []string{p}},
		Condition: condFromJSON(`{"ne": ["subject.id", ` + string(quoted) + `]}`),
	}
	return send, receive
}

// syncIsolationPolicies writes or removes a user's isolation policies to
// match their flags.
func (s *State) syncIsolationPolicies(username string, send, receive bool) error {
	sendP, recvP := isolationPolicies(username)
	for _, x := range []struct {
		on bool
		p  authz.Policy
	}{{send, sendP}, {receive, recvP}} {
		if x.on {
			if err := s.SavePolicy(x.p, "isolation"); err != nil {
				return err
			}
		} else if err := s.DeletePolicy(x.p.ID); err != nil {
			return err
		}
	}
	return nil
}

// migrateSharing converts the pre-v2 capacity controls once: per-token
// reserved slots become the token's sharing setting, and per-user isolation
// flags become policies. Reserved slots for "any" requests become the
// default for every model, which can only hold back more, never less.
func (s *State) migrateSharing() error {
	const key = "authz.migrated_sharing"
	if s.setting(key) == "1" {
		return nil
	}
	rows, err := s.db.Query(`SELECT token_hash, owner_slots FROM client_tokens WHERE sharing = '' AND owner_slots NOT IN ('', '{}', 'null')`)
	if err != nil {
		return err
	}
	type tok struct{ hash, slots string }
	var toks []tok
	for rows.Next() {
		var t tok
		if err := rows.Scan(&t.hash, &t.slots); err != nil {
			rows.Close()
			return err
		}
		toks = append(toks, t)
	}
	rows.Close()
	for _, t := range toks {
		var slots map[string]int
		if json.Unmarshal([]byte(t.slots), &slots) != nil || len(slots) == 0 {
			continue
		}
		reserved := reservedFromOwnerSlots(slots)
		if reserved == nil {
			continue
		}
		if err := s.SetClientSharing(t.hash, authz.Sharing{Mode: authz.ShareOpen, ReservedSlots: reserved}); err != nil {
			return err
		}
	}
	urows, err := s.db.Query(`SELECT username, send_isolation, receive_isolation FROM users WHERE send_isolation = 1 OR receive_isolation = 1`)
	if err != nil {
		return err
	}
	type iso struct {
		name       string
		send, recv bool
	}
	var isos []iso
	for urows.Next() {
		var i iso
		if err := urows.Scan(&i.name, &i.send, &i.recv); err != nil {
			urows.Close()
			return err
		}
		isos = append(isos, i)
	}
	urows.Close()
	for _, i := range isos {
		if err := s.syncIsolationPolicies(i.name, i.send, i.recv); err != nil {
			return err
		}
	}
	return s.putSettings(map[string]string{key: "1"})
}

// ReservedSlotsFor returns a client token's reserved slots, for the hub's
// slot report (/v1/models/slots).
func (s *State) ReservedSlotsFor(tokenHash string) map[string]int {
	if sh := s.ClientSharing(tokenHash); sh != nil {
		return sh.ReservedSlots
	}
	return nil
}

// SchedulerPairing adapts State to the scheduler's PairingPolicy.
type SchedulerPairing struct{ State *State }

// Pair implements scheduler.PairingPolicy.
func (p SchedulerPairing) Pair(reqOwner string, c types.ClientSummary) scheduler.Pairing {
	d := p.State.PairClient(reqOwner, c.Owner, c.Token)
	return scheduler.Pairing{Allowed: d.Allowed, OwnerSide: d.OwnerSide, IdleOnly: d.IdleOnly,
		Reserved: d.Reserved, PerRequesterMax: d.PerRequesterMax}
}

// handleClientSharing sets a client's sharing preset and Advanced fields.
func (a *Admin) handleClientSharing(w http.ResponseWriter, r *http.Request) {
	u := ctxGetUser(r)
	tokenHash := r.FormValue("token_hash")
	t, ok := a.state.LookupClientTokenByHash(tokenHash)
	if !ok || !a.can(r, "client.share", ownedResource("client", t.TokenHash, t.Owner)) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	sh := authz.Sharing{Mode: authz.ShareOpen}
	if cur := a.state.ClientSharing(tokenHash); cur != nil {
		sh = *cur
	}
	sh.Mode = authz.SharingMode(r.FormValue("mode"))
	// The Advanced fields are submitted with the preset; a form without them
	// (the one-click preset buttons) leaves them as they were.
	if r.Form.Has("with") {
		sh.With = nil
		for _, e := range splitList(r.FormValue("with")) {
			if !strings.Contains(e, ":") {
				e = userPrincipal(e)
			}
			sh.With = append(sh.With, e)
		}
	}
	if r.Form.Has("per_requester_max") {
		sh.PerRequesterMax, _ = strconv.Atoi(strings.TrimSpace(r.FormValue("per_requester_max")))
	}
	if r.Form.Has("reserved_default") {
		n, _ := strconv.Atoi(strings.TrimSpace(r.FormValue("reserved_default")))
		if sh.ReservedSlots == nil {
			sh.ReservedSlots = map[string]int{}
		}
		if n > 0 {
			sh.ReservedSlots["*"] = n
		} else {
			delete(sh.ReservedSlots, "*")
		}
		a.hub.SetClientOwnerSlots(tokenHash, "*", n)
	}
	if err := a.state.SetClientSharing(tokenHash, sh); err != nil {
		a.renderClientTokens(w, r, u, "", err.Error())
		return
	}
	a.state.RecordAudit(u.Username, "client_token.sharing", t.Owner+"/"+t.Name+" mode="+string(sh.Mode), a.clientIP(r))
	redirectOrRefresh(w, r, "/portal/clients")
}
