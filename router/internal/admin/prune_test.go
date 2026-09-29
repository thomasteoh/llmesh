package admin

import (
	"path/filepath"
	"testing"
)

// A database from before federation was removed can hold upstream.manage in
// a custom role or policy, and its upstream router tokens. The router must
// still start, keep what remains meaningful, and drop the tokens.
func TestRetiredFederationStateStillLoads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO roles (id, name, description, permissions, updated_at) VALUES ('fleet', 'Fleet', '', '["alias.manage","upstream.manage"]', '')`,
		`INSERT INTO policies (id, body, created_by, updated_at) VALUES ('only-up', '{"effect":"deny","enabled":true,"actions":["upstream.manage"]}', 't', '')`,
		`INSERT INTO policies (id, body, created_by, updated_at) VALUES ('mixed', '{"effect":"deny","enabled":true,"actions":["upstream.manage","alias.manage"]}', 't', '')`,
		`INSERT INTO policies (id, body, created_by, updated_at) VALUES ('via', '{"effect":"deny","enabled":true,"actions":["model.use"],"condition":{"eq":["context.via_upstream",true]}}', 't', '')`,
		`CREATE TABLE upstream_routers (url TEXT PRIMARY KEY, name TEXT, token TEXT, priority TEXT)`,
		`INSERT INTO upstream_routers VALUES ('https://hq.example.com', 'hq', 'ct-secret', 'normal')`,
	} {
		if _, err := s.db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	s.db.Close()

	s, err = LoadState(path)
	if err != nil {
		t.Fatalf("a database naming upstream.manage no longer loads: %v", err)
	}
	roles, _ := s.CustomRoles()
	if len(roles) != 1 || len(roles[0].Permissions) != 1 || roles[0].Permissions[0] != "alias.manage" {
		t.Errorf("custom role not pruned to alias.manage: %+v", roles)
	}
	byID := map[string][]string{}
	policies, _ := s.Policies()
	for _, p := range policies {
		byID[p.ID] = p.Actions
	}
	if _, ok := byID["only-up"]; ok {
		t.Error("a policy that governed only upstream.manage survived")
	}
	if a := byID["mixed"]; len(a) != 1 || a[0] != "alias.manage" {
		t.Errorf("mixed policy actions = %v, want [alias.manage]", a)
	}
	if _, ok := byID["via"]; !ok {
		t.Error("a policy reading context.via_upstream was dropped")
	}
	var n int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name = 'upstream_routers'`).Scan(&n); err != nil || n != 0 {
		t.Errorf("upstream_routers table (with its plaintext tokens) still present: n=%d err=%v", n, err)
	}
}
