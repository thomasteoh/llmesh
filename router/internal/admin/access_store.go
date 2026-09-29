package admin

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"llmesh/router/internal/authz"
)

// Persistence for access management v2 (docs/superpowers/specs/
// 2026-09-29-access-management-v2.md §2): teams, custom roles, role bindings,
// and policies, plus the compiled authz.Engine built from them.
//
// Principal ids are "user:<username>" and "team:<id>". Existing owner columns
// (api_keys.owner, client_tokens.owner) hold a bare username for user-owned
// rows, as they always have, and "team:<id>" for team-owned ones;
// ownerPrincipal translates.

const (
	authzMigratedKey = "authz.migrated_v2"
	authzVersionKey  = "authz.version"
)

// defaultModelPolicy is the grant every router starts with: everyone may use
// every model. Admins narrow it. (Design decision 2.)
var defaultModelPolicy = authz.Policy{
	ID:          "models-default",
	Name:        "Everyone may use every model",
	Description: "The starting grant. Narrow it, or add deny rules, to restrict models.",
	Effect:      authz.Allow,
	Enabled:     true,
	Actions:     []string{"model.use"},
	Resource:    authz.ResourceMatcher{Type: "model", IDs: []string{"*"}},
}

func createAccessSchema(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS teams (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			disabled    INTEGER NOT NULL DEFAULT 0,
			managed_by  TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS team_members (
			team_id  TEXT NOT NULL,
			username TEXT NOT NULL,
			added_at TEXT NOT NULL,
			PRIMARY KEY (team_id, username)
		);
		CREATE INDEX IF NOT EXISTS idx_team_members_username ON team_members(username);
		CREATE TABLE IF NOT EXISTS roles (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL,
			description TEXT NOT NULL DEFAULT '',
			permissions TEXT NOT NULL DEFAULT '[]',
			updated_at  TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS role_bindings (
			principal TEXT NOT NULL,
			role      TEXT NOT NULL,
			team      TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (principal, role, team)
		);
		CREATE INDEX IF NOT EXISTS idx_role_bindings_role ON role_bindings(role);
		CREATE TABLE IF NOT EXISTS policies (
			id         TEXT PRIMARY KEY,
			body       TEXT NOT NULL,
			created_by TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL
		);
		CREATE TABLE IF NOT EXISTS sessions (
			id_hash      TEXT PRIMARY KEY,
			username     TEXT NOT NULL,
			csrf_token   TEXT NOT NULL DEFAULT '',
			ip           TEXT NOT NULL DEFAULT '',
			user_agent   TEXT NOT NULL DEFAULT '',
			created_at   TEXT NOT NULL,
			expires_at   TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);
		CREATE INDEX IF NOT EXISTS idx_sessions_username ON sessions(username);
	`); err != nil {
		return fmt.Errorf("create access schema: %w", err)
	}
	// Additive columns on existing tables. Errors mean "already there".
	for _, stmt := range []string{
		`ALTER TABLE users ADD COLUMN attrs TEXT NOT NULL DEFAULT '{}'`,
		`ALTER TABLE api_keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN last_used_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE api_keys ADD COLUMN scope TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE client_tokens ADD COLUMN sharing TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE client_tokens ADD COLUMN tags TEXT NOT NULL DEFAULT '[]'`,
		`ALTER TABLE client_tokens ADD COLUMN last_used_at TEXT NOT NULL DEFAULT ''`,
		`ALTER TABLE client_tokens ADD COLUMN created_by TEXT NOT NULL DEFAULT ''`,
	} {
		_, _ = db.Exec(stmt)
	}
	return nil
}

// migrateAccess converts the pre-v2 model once: each user's role becomes a
// role binding, and the default model grant is written. Every existing admin
// becomes an owner, since today any admin can do everything an owner can,
// including demoting other admins; granting less would take power away on
// upgrade. Isolation flags and reserved slots are converted in phase 5, when
// the scheduler moves to the new model.
func (s *State) migrateAccess() error {
	if s.setting(authzMigratedKey) == "1" {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.Query(`SELECT username, role FROM users`)
	if err != nil {
		return err
	}
	type ur struct{ username, role string }
	var users []ur
	for rows.Next() {
		var u ur
		if err := rows.Scan(&u.username, &u.role); err != nil {
			rows.Close()
			return err
		}
		users = append(users, u)
	}
	rows.Close()
	for _, u := range users {
		role := authz.RoleMember
		if u.role == "admin" {
			role = authz.RoleOwner
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO role_bindings (principal, role, team) VALUES (?, ?, '')`,
			userPrincipal(u.username), role); err != nil {
			return err
		}
	}
	body, _ := json.Marshal(defaultModelPolicy)
	now := nowString()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO policies (id, body, created_by, updated_at) VALUES (?, ?, 'migration', ?)`,
		defaultModelPolicy.ID, string(body), now); err != nil {
		return err
	}
	for k, v := range map[string]string{authzMigratedKey: "1", authzVersionKey: "1"} {
		if _, err := tx.Exec(`INSERT INTO settings (key, value) VALUES (?, ?)
			ON CONFLICT(key) DO UPDATE SET value = excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func nowString() string { return time.Now().UTC().Format(time.RFC3339) }

func userPrincipal(username string) string { return "user:" + username }
func teamPrincipal(id string) string       { return "team:" + id }

// ownerPrincipal maps an owner column value to a principal id. A bare name is
// a username, as every owner column held before teams existed.
func ownerPrincipal(owner string) string {
	if owner == "" || strings.Contains(owner, ":") {
		return owner
	}
	return userPrincipal(owner)
}

// ValidUsername rejects names that could be mistaken for a principal id or
// break the places a username is embedded: ':' separates a principal's kind
// from its name, '/' separates owner from label in key labels.
func ValidUsername(name string) error {
	if name == "" {
		return fmt.Errorf("a username is required")
	}
	if len(name) > 64 {
		return fmt.Errorf("usernames are at most 64 characters")
	}
	if strings.ContainsAny(name, ":/ \t\r\n") {
		return fmt.Errorf("usernames cannot contain ':', '/', or spaces")
	}
	return nil
}

// --- Engine ---

// Authz returns the compiled engine for the current roles and policies.
func (s *State) Authz() *authz.Engine {
	return s.authzEngine.Load()
}

// ReloadAuthz compiles roles and policies from the database and swaps the
// result in. On failure the previous engine stays in place and the error is
// returned: a bad row should be impossible (every write validates), and if one
// appears anyway, keeping the last good engine beats serving with none.
func (s *State) ReloadAuthz() error {
	roles := authz.BuiltinRoles()
	custom, err := s.CustomRoles()
	if err != nil {
		return err
	}
	roles = append(roles, custom...)
	policies, err := s.Policies()
	if err != nil {
		return err
	}
	e, err := authz.Compile(roles, policies)
	if err != nil {
		return fmt.Errorf("compile access policies: %w", err)
	}
	s.authzEngine.Store(e)
	s.invalidateAccess()
	return nil
}

// bumpAuthz records a change to roles, bindings, teams, or policies and
// recompiles. Bindings and memberships are read per request rather than
// compiled, but the version still moves so caches keyed on it turn over.
func (s *State) bumpAuthz() error {
	if _, err := s.db.Exec(`INSERT INTO settings (key, value) VALUES (?, '1')
		ON CONFLICT(key) DO UPDATE SET value = CAST(value AS INTEGER) + 1`, authzVersionKey); err != nil {
		return err
	}
	return s.ReloadAuthz()
}

// AuthzVersion is the counter bumped on every access-model change.
func (s *State) AuthzVersion() string { return s.setting(authzVersionKey) }

// --- Subjects and liveness ---

// SubjectFor builds the authz subject for a user.
func (s *State) SubjectFor(username string) (authz.Subject, error) {
	u, ok := s.LookupUser(username)
	if !ok {
		return authz.Subject{}, fmt.Errorf("user not found: %s", username)
	}
	subj := authz.Subject{ID: userPrincipal(username), Kind: authz.KindUser, ManagedBy: u.ManagedBy}
	var err error
	if subj.Bindings, err = s.bindingsFor(subj.ID); err != nil {
		return authz.Subject{}, err
	}
	if subj.Teams, err = s.TeamsOf(username); err != nil {
		return authz.Subject{}, err
	}
	var attrs string
	_ = s.db.QueryRow(`SELECT attrs FROM users WHERE username = ?`, username).Scan(&attrs)
	if attrs != "" && attrs != "{}" {
		_ = json.Unmarshal([]byte(attrs), &subj.Attrs)
	}
	return subj, nil
}

// SubjectForTeam builds the authz subject a team-owned credential acts as.
func (s *State) SubjectForTeam(teamID string) (authz.Subject, error) {
	t, ok := s.LookupTeam(teamID)
	if !ok {
		return authz.Subject{}, fmt.Errorf("team not found: %s", teamID)
	}
	subj := authz.Subject{ID: teamPrincipal(t.ID), Kind: authz.KindTeam, Teams: []string{t.ID}, ManagedBy: t.ManagedBy}
	var err error
	subj.Bindings, err = s.bindingsFor(subj.ID)
	return subj, err
}

// PrincipalActive is the one liveness check every credential goes through
// (design §5): a user or team that exists and is not disabled. An empty
// principal is a legacy credential with no owner, which has always been
// accepted and still is.
func (s *State) PrincipalActive(principal string) bool {
	kind, name, ok := strings.Cut(principal, ":")
	if principal == "" {
		return true
	}
	if !ok {
		return false
	}
	var disabled int
	var err error
	switch kind {
	case "user":
		err = s.db.QueryRow(`SELECT disabled FROM users WHERE username = ?`, name).Scan(&disabled)
	case "team":
		err = s.db.QueryRow(`SELECT disabled FROM teams WHERE id = ?`, name).Scan(&disabled)
	default:
		return false
	}
	if err == sql.ErrNoRows {
		// A user row that does not exist cannot have been disabled; this is
		// the pre-accounts case of a key whose owner was never a user. A team
		// that does not exist, though, has been deleted.
		return kind == "user"
	}
	return err == nil && disabled == 0
}

// --- Role bindings ---

// RoleBinding is a stored binding.
type RoleBinding struct {
	Principal string
	Role      string
	Team      string
}

func (s *State) bindingsFor(principal string) ([]authz.Binding, error) {
	rows, err := s.db.Query(`SELECT role, team FROM role_bindings WHERE principal = ? ORDER BY role, team`, principal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Binding
	for rows.Next() {
		var b authz.Binding
		if err := rows.Scan(&b.Role, &b.Team); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Bindings returns every binding for a principal.
func (s *State) Bindings(principal string) ([]RoleBinding, error) {
	bs, err := s.bindingsFor(principal)
	if err != nil {
		return nil, err
	}
	out := make([]RoleBinding, len(bs))
	for i, b := range bs {
		out[i] = RoleBinding{Principal: principal, Role: b.Role, Team: b.Team}
	}
	return out, nil
}

// Bind grants a role to a principal, router-wide or within a team.
func (s *State) Bind(b RoleBinding) error {
	if err := s.checkBinding(b); err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT OR IGNORE INTO role_bindings (principal, role, team) VALUES (?, ?, ?)`,
		b.Principal, b.Role, b.Team); err != nil {
		return err
	}
	s.refreshLegacyRole(b.Principal)
	return s.bumpAuthz()
}

// Unbind removes a binding. The last active owner cannot lose the owner role.
func (s *State) Unbind(b RoleBinding) error {
	if b.Team == "" && (b.Role == authz.RoleOwner || b.Role == authz.RoleAdmin) {
		if b.Role == authz.RoleOwner && s.activeOwnerCount(b.Principal) == 0 {
			return fmt.Errorf("cannot remove the last active owner")
		}
		// Removing this binding must not leave the router with no enabled
		// owner or admin at all.
		if username, ok := strings.CutPrefix(b.Principal, "user:"); ok && s.otherActivePrivileged(username) == 0 {
			var n int
			_ = s.db.QueryRow(`SELECT COUNT(*) FROM role_bindings WHERE principal = ? AND team = '' AND role IN (?, ?) AND role <> ?`,
				b.Principal, authz.RoleOwner, authz.RoleAdmin, b.Role).Scan(&n)
			if n == 0 {
				return fmt.Errorf("cannot remove the last active admin")
			}
		}
	}
	if _, err := s.db.Exec(`DELETE FROM role_bindings WHERE principal = ? AND role = ? AND team = ?`,
		b.Principal, b.Role, b.Team); err != nil {
		return err
	}
	s.refreshLegacyRole(b.Principal)
	return s.bumpAuthz()
}

// refreshLegacyRole rewrites users.role from a user's bindings, for the code
// that still reads it (OIDC role sync, the promote/demote endpoints): "admin"
// when they hold owner or admin router-wide, "member" otherwise.
func (s *State) refreshLegacyRole(principal string) {
	username, ok := strings.CutPrefix(principal, "user:")
	if !ok {
		return
	}
	role := "member"
	if s.isPrivileged(username) {
		role = "admin"
	}
	_, _ = s.db.Exec(`UPDATE users SET role = ? WHERE username = ?`, role, username)
}

// activeOwnerCount counts enabled users holding the owner role, excluding one
// principal.
func (s *State) activeOwnerCount(excluding string) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM role_bindings b JOIN users u ON b.principal = 'user:' || u.username
		WHERE b.role = ? AND b.team = '' AND u.disabled = 0 AND b.principal <> ?`, authz.RoleOwner, excluding).Scan(&n)
	return n
}

func (s *State) checkBinding(b RoleBinding) error {
	if !s.roleExists(b.Role) {
		return fmt.Errorf("unknown role %q", b.Role)
	}
	kind, name, ok := strings.Cut(b.Principal, ":")
	if !ok {
		return fmt.Errorf("invalid principal %q", b.Principal)
	}
	switch kind {
	case "user":
		if _, found := s.LookupUser(name); !found {
			return fmt.Errorf("user not found: %s", name)
		}
	case "team":
		if _, found := s.LookupTeam(name); !found {
			return fmt.Errorf("team not found: %s", name)
		}
	default:
		return fmt.Errorf("invalid principal %q", b.Principal)
	}
	if b.Team != "" {
		if _, found := s.LookupTeam(b.Team); !found {
			return fmt.Errorf("team not found: %s", b.Team)
		}
	}
	return nil
}

func (s *State) roleExists(id string) bool {
	for _, r := range authz.BuiltinRoles() {
		if r.ID == id {
			return true
		}
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM roles WHERE id = ?`, id).Scan(&n)
	return n > 0
}

// syncLegacyRole keeps a user's router-wide admin/member binding in step with
// users.role while the portal still reads the latter (until phase 3). A
// promotion grants admin, not owner; a demotion removes both.
func (s *State) syncLegacyRole(username, role string) error {
	p := userPrincipal(username)
	if role == "admin" {
		if _, err := s.db.Exec(`DELETE FROM role_bindings WHERE principal = ? AND team = '' AND role = ?`, p, authz.RoleMember); err != nil {
			return err
		}
		var n int
		_ = s.db.QueryRow(`SELECT COUNT(*) FROM role_bindings WHERE principal = ? AND team = '' AND role IN (?, ?)`,
			p, authz.RoleOwner, authz.RoleAdmin).Scan(&n)
		if n == 0 {
			if _, err := s.db.Exec(`INSERT OR IGNORE INTO role_bindings (principal, role, team) VALUES (?, ?, '')`, p, authz.RoleAdmin); err != nil {
				return err
			}
		}
	} else {
		if _, err := s.db.Exec(`DELETE FROM role_bindings WHERE principal = ? AND team = '' AND role IN (?, ?)`,
			p, authz.RoleOwner, authz.RoleAdmin); err != nil {
			return err
		}
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO role_bindings (principal, role, team) VALUES (?, ?, '')`, p, authz.RoleMember); err != nil {
			return err
		}
	}
	return s.bumpAuthz()
}

// --- Teams ---

// Team is a group of users that can own keys and clients.
type Team struct {
	ID          string
	Name        string
	Description string
	Disabled    bool
	ManagedBy   string
	CreatedAt   time.Time
}

var teamIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,47}$`)

// CreateTeam creates a team and makes creator its maintainer.
func (s *State) CreateTeam(id, name, description, creator string) (Team, error) {
	id = strings.ToLower(strings.TrimSpace(id))
	name = strings.TrimSpace(name)
	if !teamIDPattern.MatchString(id) {
		return Team{}, fmt.Errorf("team ids are 1–48 lowercase letters, digits, '-' or '_', starting with a letter or digit")
	}
	if name == "" {
		name = id
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Team{}, err
	}
	defer tx.Rollback()
	now := nowString()
	if _, err := tx.Exec(`INSERT INTO teams (id, name, description, created_at) VALUES (?, ?, ?, ?)`,
		id, name, strings.TrimSpace(description), now); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return Team{}, fmt.Errorf("a team with that id or name already exists")
		}
		return Team{}, err
	}
	// A team's own keys act as the team, so it needs a role to use models at
	// all. Member is what a person gets by default, and admins can change it.
	if _, err := tx.Exec(`INSERT INTO role_bindings (principal, role, team) VALUES (?, ?, '')`,
		teamPrincipal(id), authz.RoleMember); err != nil {
		return Team{}, err
	}
	if creator != "" {
		if _, err := tx.Exec(`INSERT INTO team_members (team_id, username, added_at) VALUES (?, ?, ?)`, id, creator, now); err != nil {
			return Team{}, err
		}
		if _, err := tx.Exec(`INSERT INTO role_bindings (principal, role, team) VALUES (?, ?, ?)`,
			userPrincipal(creator), authz.RoleTeamMaintainer, id); err != nil {
			return Team{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Team{}, err
	}
	t, _ := s.LookupTeam(id)
	return t, s.bumpAuthz()
}

// LookupTeam returns a team by id.
func (s *State) LookupTeam(id string) (Team, bool) {
	var t Team
	var disabled int
	var created string
	err := s.db.QueryRow(`SELECT id, name, description, disabled, managed_by, created_at FROM teams WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Description, &disabled, &t.ManagedBy, &created)
	if err != nil {
		return Team{}, false
	}
	t.Disabled = disabled != 0
	t.CreatedAt, _ = time.Parse(time.RFC3339, created)
	return t, true
}

// Teams returns every team, by name.
func (s *State) Teams() ([]Team, error) {
	rows, err := s.db.Query(`SELECT id FROM teams ORDER BY name`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := make([]Team, 0, len(ids))
	for _, id := range ids {
		if t, ok := s.LookupTeam(id); ok {
			out = append(out, t)
		}
	}
	return out, nil
}

// SetTeamDisabled disables or enables a team. A disabled team's keys and
// client tokens stop authenticating through PrincipalActive.
func (s *State) SetTeamDisabled(id string, disabled bool) error {
	res, err := s.db.Exec(`UPDATE teams SET disabled = ? WHERE id = ?`, boolInt(disabled), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("team not found: %s", id)
	}
	return s.bumpAuthz()
}

// DeleteTeam removes a team, its memberships, the bindings scoped to it, and
// the keys and client tokens it owns, in one transaction.
func (s *State) DeleteTeam(id string) error {
	if _, ok := s.LookupTeam(id); !ok {
		return fmt.Errorf("team not found: %s", id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM team_members WHERE team_id = ?`,
		`DELETE FROM role_bindings WHERE team = ?`,
		`DELETE FROM role_bindings WHERE principal = 'team:' || ?`,
		`DELETE FROM api_keys WHERE owner = 'team:' || ?`,
		`DELETE FROM client_tokens WHERE owner = 'team:' || ?`,
		`DELETE FROM teams WHERE id = ?`,
	} {
		if _, err := tx.Exec(q, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// AddTeamMember adds a user to a team with the team-member role, or the
// team-maintainer role when maintainer is set.
func (s *State) AddTeamMember(teamID, username string, maintainer bool) error {
	if _, ok := s.LookupTeam(teamID); !ok {
		return fmt.Errorf("team not found: %s", teamID)
	}
	if _, ok := s.LookupUser(username); !ok {
		return fmt.Errorf("user not found: %s", username)
	}
	role := authz.RoleTeamMember
	if maintainer {
		role = authz.RoleTeamMaintainer
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT OR IGNORE INTO team_members (team_id, username, added_at) VALUES (?, ?, ?)`,
		teamID, username, nowString()); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM role_bindings WHERE principal = ? AND team = ? AND role IN (?, ?)`,
		userPrincipal(username), teamID, authz.RoleTeamMember, authz.RoleTeamMaintainer); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO role_bindings (principal, role, team) VALUES (?, ?, ?)`,
		userPrincipal(username), role, teamID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// RemoveTeamMember removes a user from a team and every binding scoped to it.
func (s *State) RemoveTeamMember(teamID, username string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM team_members WHERE team_id = ? AND username = ?`, teamID, username); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM role_bindings WHERE principal = ? AND team = ?`, userPrincipal(username), teamID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// TeamsOf returns the ids of the teams a user belongs to.
func (s *State) TeamsOf(username string) ([]string, error) {
	rows, err := s.db.Query(`SELECT team_id FROM team_members m JOIN teams t ON t.id = m.team_id
		WHERE m.username = ? AND t.disabled = 0 ORDER BY team_id`, username)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TeamMember is one member of a team, with whether they maintain it.
type TeamMember struct {
	Username   string
	Maintainer bool
}

// TeamMembers returns a team's members.
func (s *State) TeamMembers(teamID string) ([]TeamMember, error) {
	rows, err := s.db.Query(`SELECT m.username,
		EXISTS(SELECT 1 FROM role_bindings b WHERE b.principal = 'user:' || m.username AND b.team = m.team_id AND b.role = ?)
		FROM team_members m WHERE m.team_id = ? ORDER BY m.username`, authz.RoleTeamMaintainer, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TeamMember
	for rows.Next() {
		var m TeamMember
		var maint int
		if err := rows.Scan(&m.Username, &maint); err != nil {
			return nil, err
		}
		m.Maintainer = maint != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// --- Custom roles ---

// CustomRoles returns the admin-defined roles.
func (s *State) CustomRoles() ([]authz.Role, error) {
	rows, err := s.db.Query(`SELECT id, name, description, permissions FROM roles ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Role
	for rows.Next() {
		var r authz.Role
		var perms string
		if err := rows.Scan(&r.ID, &r.Name, &r.Description, &perms); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(perms), &r.Permissions); err != nil {
			return nil, fmt.Errorf("role %q: %w", r.ID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SaveRole creates or replaces a custom role. Built-in role ids are reserved.
func (s *State) SaveRole(r authz.Role) error {
	for _, b := range authz.BuiltinRoles() {
		if b.ID == r.ID {
			return fmt.Errorf("%q is a built-in role and cannot be changed; clone it instead", r.ID)
		}
	}
	if !teamIDPattern.MatchString(r.ID) {
		return fmt.Errorf("role ids are 1–48 lowercase letters, digits, '-' or '_'")
	}
	sort.Strings(r.Permissions)
	if _, err := authz.Compile(append(authz.BuiltinRoles(), r), nil); err != nil {
		return err
	}
	perms, _ := json.Marshal(r.Permissions)
	if _, err := s.db.Exec(`INSERT INTO roles (id, name, description, permissions, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET name = excluded.name, description = excluded.description,
		permissions = excluded.permissions, updated_at = excluded.updated_at`,
		r.ID, strings.TrimSpace(r.Name), strings.TrimSpace(r.Description), string(perms), nowString()); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// DeleteRole removes a custom role and every binding to it.
func (s *State) DeleteRole(id string) error {
	for _, b := range authz.BuiltinRoles() {
		if b.ID == id {
			return fmt.Errorf("%q is a built-in role and cannot be deleted", id)
		}
	}
	if _, err := s.db.Exec(`DELETE FROM role_bindings WHERE role = ?`, id); err != nil {
		return err
	}
	if _, err := s.db.Exec(`DELETE FROM roles WHERE id = ?`, id); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// --- Policies ---

// Policies returns every stored policy, enabled or not.
func (s *State) Policies() ([]authz.Policy, error) {
	rows, err := s.db.Query(`SELECT id, body FROM policies ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []authz.Policy
	for rows.Next() {
		var id, body string
		if err := rows.Scan(&id, &body); err != nil {
			return nil, err
		}
		var p authz.Policy
		if err := json.Unmarshal([]byte(body), &p); err != nil {
			return nil, fmt.Errorf("policy %q: %w", id, err)
		}
		p.ID = id
		out = append(out, p)
	}
	return out, rows.Err()
}

// SavePolicy validates a policy against the rest and stores it. Validation
// compiles the whole set, so a policy that would break compilation is refused
// here rather than discovered at the next reload.
func (s *State) SavePolicy(p authz.Policy, actor string) error {
	existing, err := s.Policies()
	if err != nil {
		return err
	}
	next := make([]authz.Policy, 0, len(existing)+1)
	for _, e := range existing {
		if e.ID != p.ID {
			next = append(next, e)
		}
	}
	next = append(next, p)
	roles, err := s.CustomRoles()
	if err != nil {
		return err
	}
	if _, err := authz.Compile(append(authz.BuiltinRoles(), roles...), next); err != nil {
		return err
	}
	body, err := json.Marshal(p)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(`INSERT INTO policies (id, body, created_by, updated_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET body = excluded.body, updated_at = excluded.updated_at`,
		p.ID, string(body), actor, nowString()); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// DeletePolicy removes a policy.
func (s *State) DeletePolicy(id string) error {
	if _, err := s.db.Exec(`DELETE FROM policies WHERE id = ?`, id); err != nil {
		return err
	}
	return s.bumpAuthz()
}

// isPrivileged reports whether a user holds the owner or admin role
// router-wide.
func (s *State) isPrivileged(username string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM role_bindings WHERE principal = ? AND team = '' AND role IN (?, ?)`,
		userPrincipal(username), authz.RoleOwner, authz.RoleAdmin).Scan(&n)
	return n > 0
}

// otherActivePrivileged counts enabled users other than excluding who hold
// the owner or admin role router-wide. The guards that keep a router from
// losing its last administrator use it.
func (s *State) otherActivePrivileged(excluding string) int {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(DISTINCT u.username) FROM role_bindings b
		JOIN users u ON b.principal = 'user:' || u.username
		WHERE b.team = '' AND b.role IN (?, ?) AND u.disabled = 0 AND u.username <> ?`,
		authz.RoleOwner, authz.RoleAdmin, excluding).Scan(&n)
	return n
}

// IsOwner reports whether a user holds the owner role router-wide.
func (s *State) IsOwner(username string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM role_bindings WHERE principal = ? AND team = '' AND role = ?`,
		userPrincipal(username), authz.RoleOwner).Scan(&n)
	return n > 0
}
