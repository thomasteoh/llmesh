package admin

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// Portal sessions, stored in the state database (design §5).
//
// A session id is a 256-bit random value held only by the browser; the
// database keeps its SHA-256, so a copy of the database does not hand anyone a
// live session. Every lookup reads the row: portal traffic is light, an
// indexed read is cheap, and reading through means revocation — sign out
// everywhere, a password change, a disabled account — takes effect on the very
// next request, on every router sharing the database.
type sessionStore struct {
	state *State
	// lastSeen throttles last_seen_at writes to one per session per minute.
	lastSeen sync.Map
}

func newSessionStore(state *State) *sessionStore {
	return &sessionStore{state: state}
}

// create starts a session for username and returns its id.
func (s *sessionStore) create(username string) string {
	return s.createWithMeta(username, "", "")
}

// createWithMeta starts a session, recording where it came from for the
// user's list of active sessions.
func (s *sessionStore) createWithMeta(username, ip, userAgent string) string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	id := hex.EncodeToString(b)
	now := time.Now().UTC()
	if len(userAgent) > 256 {
		userAgent = userAgent[:256]
	}
	db := s.state.db
	// Expired rows are swept on each sign-in, which is often enough to keep the
	// table small without a background job.
	_, _ = db.Exec(`DELETE FROM sessions WHERE expires_at <= ?`, now.Format(time.RFC3339))
	_, _ = db.Exec(`INSERT INTO sessions (id_hash, username, ip, user_agent, created_at, expires_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		hashToken(id), username, ip, userAgent,
		now.Format(time.RFC3339), now.Add(sessionTTL).Format(time.RFC3339), now.Format(time.RFC3339))
	return id
}

// lookup returns the session's username if it exists and has not expired.
func (s *sessionStore) lookup(id string) (string, bool) {
	if id == "" {
		return "", false
	}
	h := hashToken(id)
	var username, expires string
	if err := s.state.db.QueryRow(`SELECT username, expires_at FROM sessions WHERE id_hash = ?`, h).
		Scan(&username, &expires); err != nil {
		return "", false
	}
	if expired(expires) {
		_, _ = s.state.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, h)
		return "", false
	}
	now := time.Now()
	if prev, ok := s.lastSeen.Load(h); !ok || now.Sub(prev.(time.Time)) >= time.Minute {
		s.lastSeen.Store(h, now)
		_, _ = s.state.db.Exec(`UPDATE sessions SET last_seen_at = ? WHERE id_hash = ?`, now.UTC().Format(time.RFC3339), h)
	}
	return username, true
}

func (s *sessionStore) delete(id string) {
	_, _ = s.state.db.Exec(`DELETE FROM sessions WHERE id_hash = ?`, hashToken(id))
}

func (s *sessionStore) setCSRF(id, token string) {
	_, _ = s.state.db.Exec(`UPDATE sessions SET csrf_token = ? WHERE id_hash = ?`, token, hashToken(id))
}

func (s *sessionStore) getCSRF(id string) (string, bool) {
	var token string
	if err := s.state.db.QueryRow(`SELECT csrf_token FROM sessions WHERE id_hash = ?`, hashToken(id)).Scan(&token); err != nil {
		return "", false
	}
	return token, true
}

// RevokeSessions ends every session a user has, except the one whose id is
// keep (empty keeps none). A password change keeps the session that made it;
// everything else — disabling, resetting, "sign out everywhere" — keeps none.
func (s *State) RevokeSessions(username, keep string) error {
	if keep == "" {
		_, err := s.db.Exec(`DELETE FROM sessions WHERE username = ?`, username)
		return err
	}
	_, err := s.db.Exec(`DELETE FROM sessions WHERE username = ? AND id_hash <> ?`, username, hashToken(keep))
	return err
}

// SessionInfo describes one active session, for the user's own list.
type SessionInfo struct {
	IDHash     string
	IP         string
	UserAgent  string
	CreatedAt  time.Time
	LastSeenAt time.Time
	ExpiresAt  time.Time
}

// Sessions lists a user's unexpired sessions, most recently used first.
func (s *State) Sessions(username string) ([]SessionInfo, error) {
	rows, err := s.db.Query(`SELECT id_hash, ip, user_agent, created_at, last_seen_at, expires_at FROM sessions
		WHERE username = ? AND expires_at > ? ORDER BY last_seen_at DESC`, username, nowString())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SessionInfo
	for rows.Next() {
		var si SessionInfo
		var c, l, e string
		if err := rows.Scan(&si.IDHash, &si.IP, &si.UserAgent, &c, &l, &e); err != nil {
			return nil, err
		}
		si.CreatedAt, _ = time.Parse(time.RFC3339, c)
		si.LastSeenAt, _ = time.Parse(time.RFC3339, l)
		si.ExpiresAt, _ = time.Parse(time.RFC3339, e)
		out = append(out, si)
	}
	return out, rows.Err()
}
