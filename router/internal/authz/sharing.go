package authz

import (
	"fmt"
	"strings"
)

// SharingMode is how a client offers its capacity to others.
type SharingMode string

const (
	// SharePrivate serves only the owner (and, for a team-owned client, the
	// team).
	SharePrivate SharingMode = "private"
	// ShareIdle serves others only while the owner has nothing waiting for
	// it. The scheduler enforces the timing; this package only says a pairing
	// is idle-only.
	ShareIdle SharingMode = "idle"
	// ShareOpen serves anyone policy permits, preferring the owner.
	ShareOpen SharingMode = "shared"
)

// Sharing is a client's sharing setting. The three modes are the whole of the
// simple interface; the remaining fields are the "Advanced" section.
type Sharing struct {
	Mode SharingMode `json:"mode"`
	// With, when non-empty, limits who besides the owner may be served:
	// principal ids ("user:bob", "team:research") or "role:<id>".
	With []string `json:"with,omitempty"`
	// ReservedSlots keeps slots back for the owner, per model ("*" for any).
	ReservedSlots map[string]int `json:"reserved_slots,omitempty"`
	// PerRequesterMax caps concurrent jobs from any one non-owner principal;
	// 0 means no cap.
	PerRequesterMax int `json:"per_requester_max,omitempty"`
	// Reclaim lets owner work displace a non-owner job that has not yet
	// produced its first token.
	Reclaim bool `json:"reclaim,omitempty"`
}

// Validate checks a sharing setting.
func (s Sharing) Validate() error {
	switch s.Mode {
	case SharePrivate, ShareIdle, ShareOpen:
	default:
		return fmt.Errorf("unknown sharing mode %q", s.Mode)
	}
	for _, w := range s.With {
		if !strings.HasPrefix(w, "user:") && !strings.HasPrefix(w, "team:") &&
			!strings.HasPrefix(w, "role:") && !strings.HasPrefix(w, "router:") {
			return fmt.Errorf("sharing entry %q must start with user:, team:, role:, or router:", w)
		}
	}
	for m, n := range s.ReservedSlots {
		if n < 0 {
			return fmt.Errorf("reserved slots for %q cannot be negative", m)
		}
	}
	if s.PerRequesterMax < 0 {
		return fmt.Errorf("per-requester maximum cannot be negative")
	}
	return nil
}

// isOwnerSide reports whether the subject is the client's owner, or a member
// of the team that owns it.
func isOwnerSide(s Subject, client Resource) bool {
	if client.Owner != "" && s.ID == client.Owner {
		return true
	}
	return client.Team != "" && s.HasTeam(client.Team)
}

// sharingAdmits reports whether a client's own sharing setting lets the
// subject use it. A client with no setting is shared, which is what every
// client was before sharing existed.
func sharingAdmits(s Subject, client Resource) bool {
	if isOwnerSide(s, client) {
		return true
	}
	sh := client.Sharing
	if sh == nil {
		return true
	}
	if sh.Mode == SharePrivate {
		return false
	}
	if len(sh.With) == 0 {
		return true
	}
	for _, w := range sh.With {
		switch {
		case w == s.ID:
			return true
		case strings.HasPrefix(w, "team:") && s.HasTeam(strings.TrimPrefix(w, "team:")):
			return true
		case strings.HasPrefix(w, "role:"):
			role := strings.TrimPrefix(w, "role:")
			for _, b := range s.Bindings {
				if b.Role == role && b.Team == "" {
					return true
				}
			}
		}
	}
	return false
}

// Pairing is whether a request may run on a client, and on what terms.
type Pairing struct {
	Allowed bool
	// IdleOnly means the client serves this request only while its owner
	// has nothing waiting for it.
	IdleOnly bool
	// Reserved is the number of the client's slots held back from this
	// request for the owner, for the model in question.
	Reserved int
	// Decision explains a refusal: which of the model or client checks said
	// no.
	Decision Decision
}

// CanPair decides whether a request made as subject for model may be served by
// client. Both sides must agree: the subject may use the model and may use the
// client (policy can keep a team on its own hardware), and the client's
// sharing admits the subject.
func (e *Engine) CanPair(subject Subject, model, client Resource, ctx Context) Pairing {
	d := e.Decide(Request{Subject: subject, Action: "model.use", Resource: model, Context: ctx})
	if !d.Allowed {
		return Pairing{Decision: d}
	}
	d = e.Decide(Request{Subject: subject, Action: "client.use", Resource: client, Context: ctx})
	if !d.Allowed {
		return Pairing{Decision: d}
	}
	p := Pairing{Allowed: true, Decision: d}
	if isOwnerSide(subject, client) || client.Sharing == nil {
		return p
	}
	p.IdleOnly = client.Sharing.Mode == ShareIdle
	if n, ok := client.Sharing.ReservedSlots[model.ID]; ok {
		p.Reserved = n
	} else {
		p.Reserved = client.Sharing.ReservedSlots["*"]
	}
	return p
}

// ClientPairing is the model-independent half of CanPair, for the scheduler:
// whether a subject may use a client at all and on what terms. Model access
// is decided at admission and carried on the request, so the scheduler only
// needs the client side, and needs it once per requester per client rather
// than once per model.
type ClientPairing struct {
	Allowed bool
	// OwnerSide means the subject owns the client or belongs to the team
	// that does. Owner-side work is never limited by sharing.
	OwnerSide bool
	// IdleOnly means the client serves this subject only while no owner-side
	// work is running on it.
	IdleOnly bool
	// Reserved is slots held back from this subject, per model; "*" applies
	// to models not listed.
	Reserved map[string]int
	// PerRequesterMax caps this subject's concurrent jobs on the client; 0
	// means no cap.
	PerRequesterMax int
	Decision        Decision
}

// ReservedFor returns the slots held back for model.
func (p ClientPairing) ReservedFor(model string) int {
	if n, ok := p.Reserved[model]; ok {
		return n
	}
	return p.Reserved["*"]
}

// PairClient decides whether subject may use client.
func (e *Engine) PairClient(subject Subject, client Resource, ctx Context) ClientPairing {
	d := e.Decide(Request{Subject: subject, Action: "client.use", Resource: client, Context: ctx})
	if !d.Allowed {
		return ClientPairing{Decision: d}
	}
	p := ClientPairing{Allowed: true, Decision: d, OwnerSide: isOwnerSide(subject, client)}
	if p.OwnerSide || client.Sharing == nil {
		return p
	}
	p.IdleOnly = client.Sharing.Mode == ShareIdle
	p.Reserved = client.Sharing.ReservedSlots
	p.PerRequesterMax = client.Sharing.PerRequesterMax
	return p
}
