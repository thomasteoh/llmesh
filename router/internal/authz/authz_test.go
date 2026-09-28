package authz

import (
	"encoding/json"
	"net/netip"
	"strings"
	"sync"
	"testing"
	"time"
)

func mustCompile(t *testing.T, policies ...Policy) *Engine {
	t.Helper()
	e, err := Compile(BuiltinRoles(), policies)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func cond(t *testing.T, s string) *Cond {
	t.Helper()
	var c Cond
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return &c
}

var (
	alice  = Subject{ID: "user:alice", Kind: KindUser, Bindings: []Binding{{Role: RoleMember}}, Teams: []string{"research"}}
	bob    = Subject{ID: "user:bob", Kind: KindUser, Bindings: []Binding{{Role: RoleMember}}}
	admin  = Subject{ID: "user:root", Kind: KindUser, Bindings: []Binding{{Role: RoleAdmin}}}
	viewer = Subject{ID: "user:vic", Kind: KindUser, Bindings: []Binding{{Role: RoleViewer}}}
)

// allModels is the grant every migrated router starts with.
var allModels = Policy{
	ID: "models-all", Name: "Everyone may use every model", Effect: Allow, Enabled: true,
	Actions: []string{"model.use"}, Resource: ResourceMatcher{Type: "model", IDs: []string{"*"}},
}

func model(name string) Resource { return Resource{Type: "model", ID: name} }

func TestGlob(t *testing.T) {
	for _, tc := range []struct {
		pattern, name string
		want          bool
	}{
		{"*", "", true},
		{"*", "unsloth/qwen3-30b", true},
		{"qwen3-*", "qwen3-30b-a3b", true},
		{"qwen3-*", "qwen2.5", false},
		{"unsloth/*", "unsloth/qwen3", true},
		{"*/qwen3*", "unsloth/qwen3-30b", true},
		{"gpt-4?", "gpt-4o", true},
		{"gpt-4?", "gpt-4", false},
		{"a*b*c", "aXXbYYc", true},
		{"a*b*c", "aXXbYY", false},
		{"exact", "exact", true},
		{"exact", "exactly", false},
		{strings.Repeat("*a", 20), strings.Repeat("a", 40), true},
		{strings.Repeat("*a", 20) + "b", strings.Repeat("a", 60), false},
	} {
		if got := Glob(tc.pattern, tc.name); got != tc.want {
			t.Errorf("Glob(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

func TestParsePermission(t *testing.T) {
	for s, ok := range map[string]bool{
		"model.use":       true,
		"key.manage.own":  true,
		"key.manage.team": true,
		"key.manage.any":  true,
		"key.manage":      false, // scoped action without a scope
		"fleet.view.any":  false, // unscoped action with a scope
		"key.manage.all":  false,
		"nonsense":        false,
		"nonsense.any":    false,
	} {
		if _, err := ParsePermission(s); (err == nil) != ok {
			t.Errorf("ParsePermission(%q) error = %v, want ok=%v", s, err, ok)
		}
	}
}

func TestBuiltinRolesCompile(t *testing.T) {
	if _, err := Compile(BuiltinRoles(), nil); err != nil {
		t.Fatal(err)
	}
	e := mustCompile(t)
	if e.roles[RoleAdmin].grants["owner.manage"] != nil {
		t.Error("admin can manage owners")
	}
	if e.roles[RoleOwner].grants["owner.manage"] == nil {
		t.Error("owner cannot manage owners")
	}
}

func TestRoleScopes(t *testing.T) {
	e := mustCompile(t)
	aliceKey := Resource{Type: "key", ID: "k1", Owner: "user:alice"}
	teamKey := Resource{Type: "key", ID: "k2", Owner: "team:research", Team: "research"}
	bobKey := Resource{Type: "key", ID: "k3", Owner: "user:bob"}

	check := func(s Subject, action string, r Resource, want bool) {
		t.Helper()
		if got := e.Can(Request{Subject: s, Action: action, Resource: r}); got != want {
			t.Errorf("%s %s %s/%s = %v, want %v", s.ID, action, r.Type, r.ID, got, want)
		}
	}
	check(alice, "key.manage", aliceKey, true)
	check(alice, "key.manage", bobKey, false)
	// Belonging to a team is not a team role: plain members do not manage
	// team keys.
	check(alice, "key.manage", teamKey, false)
	check(admin, "key.manage", bobKey, true)
	check(alice, "audit.view", Resource{Type: "audit"}, false)
	check(admin, "audit.view", Resource{Type: "audit"}, true)
	check(admin, "owner.manage", Resource{Type: "user"}, false)

	maint := Subject{ID: "user:mia", Kind: KindUser, Teams: []string{"research"},
		Bindings: []Binding{{Role: RoleMember}, {Role: RoleTeamMaintainer, Team: "research"}}}
	check(maint, "key.manage", teamKey, true)
	check(maint, "key.manage", Resource{Type: "key", Owner: "team:ops", Team: "ops"}, false)
	check(maint, "key.manage", bobKey, false)
}

// A custom role bound within a team must not grant router-wide permissions.
func TestTeamBindingDoesNotLeak(t *testing.T) {
	custom := Role{ID: "team-snoop", Permissions: []string{"fleet.view", "audit.view", "usage.view.any"}}
	e, err := Compile(append(BuiltinRoles(), custom), nil)
	if err != nil {
		t.Fatal(err)
	}
	s := Subject{ID: "user:eve", Kind: KindUser, Teams: []string{"research"},
		Bindings: []Binding{{Role: "team-snoop", Team: "research"}}}
	if e.Can(Request{Subject: s, Action: "fleet.view", Resource: Resource{Type: "client"}}) {
		t.Error("a team-scoped binding granted fleet.view router-wide")
	}
	if e.Can(Request{Subject: s, Action: "audit.view", Resource: Resource{Type: "audit"}}) {
		t.Error("a team-scoped binding granted audit.view router-wide")
	}
	// Its scoped permission still reaches that team's resources, and only those.
	if !e.Can(Request{Subject: s, Action: "usage.view", Resource: Resource{Type: "usage", Team: "research"}}) {
		t.Error("a team-scoped binding did not reach its own team")
	}
	if e.Can(Request{Subject: s, Action: "usage.view", Resource: Resource{Type: "usage", Team: "ops"}}) {
		t.Error("a team-scoped usage.view.any reached another team")
	}
}

func TestModelUseNeedsRoleAndGrant(t *testing.T) {
	gptFinance := Policy{
		ID: "gpt-finance", Effect: Allow, Enabled: true,
		Subject: SubjectMatcher{Teams: []string{"finance"}},
		Actions: []string{"model.use"}, Resource: ResourceMatcher{Type: "model", IDs: []string{"gpt-*"}},
	}
	e := mustCompile(t, gptFinance)
	fin := Subject{ID: "user:fay", Kind: KindUser, Bindings: []Binding{{Role: RoleMember}}, Teams: []string{"finance"}}

	if !e.Can(Request{Subject: fin, Action: "model.use", Resource: model("gpt-4o")}) {
		t.Error("finance was refused the model granted to it")
	}
	d := e.Decide(Request{Subject: alice, Action: "model.use", Resource: model("gpt-4o")})
	if d.Allowed || !strings.Contains(d.Reason, "no grant") {
		t.Errorf("a member outside finance got gpt-4o: %+v", d)
	}
	// A grant without a role is not enough.
	finViewer := fin
	finViewer.Bindings = []Binding{{Role: RoleViewer}}
	if e.Can(Request{Subject: finViewer, Action: "model.use", Resource: model("gpt-4o")}) {
		t.Error("a viewer used a model on the strength of a grant alone")
	}
	// Even admins need a grant: model access is the model's list, not a role.
	if e.Can(Request{Subject: admin, Action: "model.use", Resource: model("qwen3")}) {
		t.Error("admin used a model nothing granted")
	}
}

func TestDenyOverrides(t *testing.T) {
	noPaid := Policy{
		ID: "no-paid", Name: "Contractors: local models only", Effect: Deny, Enabled: true,
		Subject: SubjectMatcher{Attrs: map[string]string{"employment": "contractor"}},
		Actions: []string{"model.use"}, Resource: ResourceMatcher{Type: "model"},
		Condition: cond(t, `{"ne": ["resource.served_by_kind", "llama.cpp"]}`),
	}
	e := mustCompile(t, allModels, noPaid)
	con := Subject{ID: "user:carl", Kind: KindUser, Bindings: []Binding{{Role: RoleMember}},
		Attrs: map[string]string{"employment": "contractor"}}

	paid := Resource{Type: "model", ID: "gpt-4o", Attrs: map[string]any{"served_by_kind": "shim"}}
	local := Resource{Type: "model", ID: "qwen3", Attrs: map[string]any{"served_by_kind": "llama.cpp"}}
	unknown := Resource{Type: "model", ID: "mystery"}

	d := e.Decide(Request{Subject: con, Action: "model.use", Resource: paid})
	if d.Allowed || d.By != "no-paid" || !strings.Contains(d.Reason, "Contractors") {
		t.Errorf("paid model: %+v", d)
	}
	if !e.Can(Request{Subject: con, Action: "model.use", Resource: local}) {
		t.Error("contractor refused a local model")
	}
	// A model whose kind is unknown cannot be shown to be local, so the deny
	// holds: absence never lets a subject slip a restriction.
	if e.Can(Request{Subject: con, Action: "model.use", Resource: unknown}) {
		t.Error("contractor used a model of unknown kind")
	}
	// Employees are not selected by the matcher at all.
	if !e.Can(Request{Subject: alice, Action: "model.use", Resource: paid}) {
		t.Error("the contractor rule caught an employee")
	}
}

func TestMissingAttributeNeverGrants(t *testing.T) {
	financeOnly := Policy{
		ID: "finance-gpt", Effect: Allow, Enabled: true, Actions: []string{"model.use"},
		Resource:  ResourceMatcher{Type: "model", IDs: []string{"gpt-*"}},
		Condition: cond(t, `{"eq": ["subject.attrs.department", "finance"]}`),
	}
	e := mustCompile(t, financeOnly)
	if e.Can(Request{Subject: alice, Action: "model.use", Resource: model("gpt-4o")}) {
		t.Error("a subject with no department satisfied a department condition")
	}
	fin := alice
	fin.Attrs = map[string]string{"department": "finance"}
	if !e.Can(Request{Subject: fin, Action: "model.use", Resource: model("gpt-4o")}) {
		t.Error("finance refused")
	}
}

func TestConditionOperators(t *testing.T) {
	mel, _ := time.LoadLocation("Australia/Melbourne")
	req := &Request{
		Subject: Subject{ID: "user:a", Kind: KindUser, Teams: []string{"x", "y"},
			Bindings: []Binding{{Role: "member"}}, Attrs: map[string]string{"level": "3"}},
		Resource: Resource{Type: "model", ID: "m", Attrs: map[string]any{
			"context_size": float64(32768), "tags": []string{"gpu", "home"}}},
		Context: Context{
			Time:         time.Date(2026, 9, 29, 23, 30, 0, 0, mel),
			SourceIP:     netip.MustParseAddr("::ffff:10.1.2.3"),
			PromptTokens: 1200, Endpoint: "/v1/chat/completions",
		},
	}
	for _, tc := range []struct {
		expr string
		want tri
	}{
		{`{"eq": ["subject.id", "user:a"]}`, triTrue},
		{`{"eq": ["subject.attrs.level", 3]}`, triTrue},
		{`{"ne": ["subject.kind", "user"]}`, triFalse},
		{`{"has": ["subject.teams", "y"]}`, triTrue},
		{`{"has": ["resource.tags", "cloud"]}`, triFalse},
		{`{"in": ["subject.id", ["user:a", "user:b"]]}`, triTrue},
		{`{"not_in": ["subject.id", ["user:b"]]}`, triTrue},
		{`{"eq": ["subject.teams", ["y", "x"]]}`, triTrue},
		{`{"glob": ["context.endpoint", "/v1/*"]}`, triTrue},
		{`{"cidr": ["context.source_ip", "10.0.0.0/8"]}`, triTrue},
		{`{"cidr": ["context.source_ip", "192.168.0.0/16"]}`, triFalse},
		{`{"gt": ["resource.context_size", 8192]}`, triTrue},
		{`{"le": ["context.prompt_tokens", 1000]}`, triFalse},
		{`{"exists": ["subject.attrs.level"]}`, triTrue},
		{`{"exists": ["subject.attrs.missing"]}`, triFalse},
		{`{"eq": ["subject.attrs.missing", "x"]}`, triUnknown},
		{`{"time_between": ["context.time", "22:00", "06:00", "Australia/Melbourne"]}`, triTrue},
		{`{"time_between": ["context.time", "08:00", "18:00", "Australia/Melbourne"]}`, triFalse},
		// 23:30 in Melbourne is 13:30 UTC.
		{`{"time_between": ["context.time", "13:00", "14:00", "UTC"]}`, triTrue},
		{`{"all": [{"eq": ["subject.id", "user:a"]}, {"eq": ["subject.attrs.missing", "x"]}]}`, triUnknown},
		{`{"all": [{"eq": ["subject.id", "user:z"]}, {"eq": ["subject.attrs.missing", "x"]}]}`, triFalse},
		{`{"any": [{"eq": ["subject.id", "user:a"]}, {"eq": ["subject.attrs.missing", "x"]}]}`, triTrue},
		{`{"any": [{"eq": ["subject.id", "user:z"]}, {"eq": ["subject.attrs.missing", "x"]}]}`, triUnknown},
		{`{"not": {"eq": ["subject.attrs.missing", "x"]}}`, triUnknown},
		{`{"not": {"eq": ["subject.id", "user:a"]}}`, triFalse},
		// A literal that looks like a reference.
		{`{"eq": ["subject.id", {"value": "subject.id"}]}`, triFalse},
	} {
		c := cond(t, tc.expr)
		if err := c.validate(0); err != nil {
			t.Errorf("%s: validate: %v", tc.expr, err)
			continue
		}
		prepare(c)
		if got := c.eval(req); got != tc.want {
			t.Errorf("%s = %v, want %v", tc.expr, got, tc.want)
		}
	}
}

func TestConditionRejects(t *testing.T) {
	for _, expr := range []string{
		`{}`,
		`{"eq": ["a", "b"], "ne": ["a", "b"]}`,
		`{"regex": ["subject.id", ".*"]}`,
		`{"eq": ["subject.id"]}`,
		`{"cidr": ["context.source_ip", "not-a-prefix"]}`,
		`{"time_between": ["context.time", "25:00", "06:00", "UTC"]}`,
		`{"time_between": ["context.time", "22:00", "06:00", "Mars/Olympus"]}`,
		`{"eq": ["subject.nonsense", "x"]}`,
		`{"eq": ["context.nonsense", "x"]}`,
		`{"eq": ["subject.id", {"lit": "x"}]}`,
		`{"eq": ["subject.id", [1, 2]]}`,
		`{"all": []}`,
		`{"exists": ["literal"]}`,
		`{"glob": ["subject.id", 3]}`,
		strings.Repeat(`{"not": `, 20) + `{"eq": ["subject.id", "x"]}` + strings.Repeat(`}`, 20),
	} {
		var c Cond
		err := json.Unmarshal([]byte(expr), &c)
		if err == nil {
			err = c.validate(0)
		}
		if err == nil {
			t.Errorf("accepted %s", expr)
		}
	}
}

func TestConditionRoundTrip(t *testing.T) {
	src := `{"all":[{"eq":["subject.attrs.department","finance"]},{"not":{"in":["resource.id",["a","b"]]}},{"eq":["resource.id",{"value":"context.time"}]}]}`
	c := cond(t, src)
	out, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != src {
		t.Fatalf("round trip changed the condition:\n got %s\nwant %s", out, src)
	}
}

func TestCompileRejects(t *testing.T) {
	ok := allModels
	for name, p := range map[string]Policy{
		"no id":           {Effect: Allow, Actions: []string{"model.use"}},
		"bad effect":      {ID: "x", Effect: "maybe", Actions: []string{"model.use"}},
		"no actions":      {ID: "x", Effect: Allow},
		"unknown action":  {ID: "x", Effect: Allow, Actions: []string{"model.fly"}},
		"bad condition":   {ID: "x", Effect: Allow, Actions: []string{"model.use"}, Condition: &Cond{Op: "eq"}},
		"disabled broken": {ID: "x", Effect: "maybe", Actions: []string{"model.use"}},
	} {
		if _, err := Compile(BuiltinRoles(), []Policy{ok, p}); err == nil {
			t.Errorf("%s: compiled", name)
		}
	}
	if _, err := Compile(BuiltinRoles(), []Policy{ok, ok}); err == nil {
		t.Error("duplicate policy ids compiled")
	}
	if _, err := Compile(append(BuiltinRoles(), Role{ID: RoleAdmin}), nil); err == nil {
		t.Error("duplicate role ids compiled")
	}
	if _, err := Compile([]Role{{ID: "r", Permissions: []string{"key.manage"}}}, nil); err == nil {
		t.Error("a role with an unscoped scoped permission compiled")
	}
}

func TestDisabledPolicyIgnored(t *testing.T) {
	deny := Policy{ID: "off", Effect: Deny, Enabled: false, Actions: []string{"model.use"}}
	e := mustCompile(t, allModels, deny)
	if !e.Can(Request{Subject: alice, Action: "model.use", Resource: model("m")}) {
		t.Error("a disabled deny policy applied")
	}
}

func TestActionGlobs(t *testing.T) {
	readOnly := Policy{ID: "ro", Effect: Deny, Enabled: true,
		Subject: SubjectMatcher{IDs: []string{"user:bob"}}, Actions: []string{"*.manage"}}
	e := mustCompile(t, readOnly)
	bobAdmin := Subject{ID: "user:bob", Kind: KindUser, Bindings: []Binding{{Role: RoleAdmin}}}
	if e.Can(Request{Subject: bobAdmin, Action: "user.manage", Resource: Resource{Type: "user"}}) {
		t.Error("*.manage deny did not cover user.manage")
	}
	if !e.Can(Request{Subject: bobAdmin, Action: "audit.view", Resource: Resource{Type: "audit"}}) {
		t.Error("*.manage deny covered audit.view")
	}
}

func TestSharing(t *testing.T) {
	e := mustCompile(t, allModels)
	m := model("qwen3")
	client := func(owner, team string, sh *Sharing) Resource {
		return Resource{Type: "client", ID: "c", Owner: owner, Team: team, Sharing: sh}
	}
	research := Subject{ID: "user:rita", Kind: KindUser, Bindings: []Binding{{Role: RoleMember}}, Teams: []string{"research"}}

	for _, tc := range []struct {
		name     string
		subject  Subject
		client   Resource
		allowed  bool
		idleOnly bool
		reserved int
	}{
		{"no setting is shared", bob, client("user:alice", "", nil), true, false, 0},
		{"owner on private", alice, client("user:alice", "", &Sharing{Mode: SharePrivate}), true, false, 0},
		{"other on private", bob, client("user:alice", "", &Sharing{Mode: SharePrivate}), false, false, 0},
		{"team on team-owned private", research, client("team:research", "research", &Sharing{Mode: SharePrivate}), true, false, 0},
		{"other on idle", bob, client("user:alice", "", &Sharing{Mode: ShareIdle}), true, true, 0},
		{"owner on idle is never idle-only", alice, client("user:alice", "", &Sharing{Mode: ShareIdle}), true, false, 0},
		{"shared", bob, client("user:alice", "", &Sharing{Mode: ShareOpen}), true, false, 0},
		{"allowlist miss", bob, client("user:alice", "", &Sharing{Mode: ShareOpen, With: []string{"team:research"}}), false, false, 0},
		{"allowlist team", research, client("user:alice", "", &Sharing{Mode: ShareOpen, With: []string{"team:research"}}), true, false, 0},
		{"allowlist user", bob, client("user:alice", "", &Sharing{Mode: ShareOpen, With: []string{"user:bob"}}), true, false, 0},
		{"allowlist role", admin, client("user:alice", "", &Sharing{Mode: ShareOpen, With: []string{"role:admin"}}), true, false, 0},
		{"reserved for model", bob, client("user:alice", "", &Sharing{Mode: ShareOpen, ReservedSlots: map[string]int{"qwen3": 2, "*": 1}}), true, false, 2},
		{"reserved any", bob, client("user:alice", "", &Sharing{Mode: ShareOpen, ReservedSlots: map[string]int{"*": 1}}), true, false, 1},
		{"owner has no reservation against it", alice, client("user:alice", "", &Sharing{Mode: ShareOpen, ReservedSlots: map[string]int{"*": 1}}), true, false, 0},
		{"viewer cannot use models", viewer, client("user:alice", "", nil), false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := e.CanPair(tc.subject, m, tc.client, Context{})
			if p.Allowed != tc.allowed || p.IdleOnly != tc.idleOnly || p.Reserved != tc.reserved {
				t.Fatalf("got %+v", p)
			}
		})
	}

	// Keeping a team on its own hardware, as send isolation does today.
	ownHardware := Policy{ID: "research-own-hw", Effect: Deny, Enabled: true,
		Subject:   SubjectMatcher{Teams: []string{"research"}},
		Actions:   []string{"client.use"},
		Condition: cond(t, `{"ne": ["resource.team", "research"]}`)}
	e = mustCompile(t, allModels, ownHardware)
	if p := e.CanPair(research, m, client("user:bob", "", nil), Context{}); p.Allowed || p.Decision.By != "research-own-hw" {
		t.Errorf("research reached outside hardware: %+v", p)
	}
	if p := e.CanPair(research, m, client("team:research", "research", nil), Context{}); !p.Allowed {
		t.Errorf("research refused its own hardware: %+v", p)
	}
}

func TestSharingValidate(t *testing.T) {
	for _, tc := range []struct {
		s  Sharing
		ok bool
	}{
		{Sharing{Mode: ShareIdle}, true},
		{Sharing{Mode: "sometimes"}, false},
		{Sharing{Mode: ShareOpen, With: []string{"bob"}}, false},
		{Sharing{Mode: ShareOpen, ReservedSlots: map[string]int{"m": -1}}, false},
		{Sharing{Mode: ShareOpen, PerRequesterMax: -1}, false},
	} {
		if err := tc.s.Validate(); (err == nil) != tc.ok {
			t.Errorf("%+v: %v", tc.s, err)
		}
	}
}

func TestCache(t *testing.T) {
	e := mustCompile(t, allModels)
	s := alice
	s.Rev = "r1"
	req := Request{Subject: s, Action: "model.use", Resource: model("m")}
	if !e.Can(req) {
		t.Fatal("refused")
	}
	if len(e.cache) != 1 {
		t.Fatalf("expected one cached decision, got %d", len(e.cache))
	}
	// A subject with no Rev is never cached.
	e.Can(Request{Subject: bob, Action: "model.use", Resource: model("m")})
	if len(e.cache) != 1 {
		t.Fatal("an unidentified subject state was cached")
	}

	// A rule reading the clock makes its action uncacheable.
	night := Policy{ID: "night", Effect: Deny, Enabled: true, Actions: []string{"model.use"},
		Condition: cond(t, `{"time_between": ["context.time", "22:00", "06:00", "UTC"]}`)}
	e = mustCompile(t, allModels, night)
	day := Request{Subject: s, Action: "model.use", Resource: model("m"),
		Context: Context{Time: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}}
	late := day
	late.Context.Time = time.Date(2026, 1, 1, 23, 0, 0, 0, time.UTC)
	if !e.Can(day) || e.Can(late) {
		t.Fatal("a time-dependent decision was served from cache")
	}
	// With no time at all, the deny's condition is unknown and it holds.
	noTime := day
	noTime.Context = Context{}
	if e.Can(noTime) {
		t.Fatal("a missing clock let a time restriction slip")
	}
}

// Compile must not write into the caller's policies, so one set can be
// compiled concurrently (run with -race).
func TestCompileIsConcurrencySafe(t *testing.T) {
	p := Policy{ID: "t", Effect: Deny, Enabled: true, Actions: []string{"model.use"},
		Condition: cond(t, `{"all": [{"cidr": ["context.source_ip", "10.0.0.0/8"]}, {"time_between": ["context.time", "01:00", "02:00", "UTC"]}]}`)}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := Compile(BuiltinRoles(), []Policy{p}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if p.Condition.All[0].prefixParsed || p.Condition.All[1].loc != nil {
		t.Fatal("Compile prepared the caller's condition in place")
	}
}

func FuzzCondition(f *testing.F) {
	for _, s := range []string{
		`{"eq": ["subject.id", "x"]}`,
		`{"all": [{"has": ["subject.teams", "a"]}, {"not": {"cidr": ["context.source_ip", "10.0.0.0/8"]}}]}`,
		`{"time_between": ["context.time", "22:00", "06:00", "UTC"]}`,
		`{"any": [{"gt": ["resource.size", 3]}, {"glob": ["resource.id", "a*"]}]}`,
	} {
		f.Add(s)
	}
	req := &Request{
		Subject:  Subject{ID: "user:a", Kind: KindUser, Attrs: map[string]string{"k": "v"}},
		Resource: Resource{Type: "model", ID: "m", Attrs: map[string]any{"size": float64(4), "tags": []string{"a"}}},
		Context:  Context{Time: time.Unix(0, 0), SourceIP: netip.MustParseAddr("10.0.0.1")},
	}
	f.Fuzz(func(t *testing.T, s string) {
		var c Cond
		if json.Unmarshal([]byte(s), &c) != nil || c.validate(0) != nil {
			return
		}
		prepare(&c)
		c.eval(req) // must not panic
		if _, err := json.Marshal(c); err != nil {
			t.Fatalf("a valid condition failed to marshal: %v", err)
		}
	})
}
