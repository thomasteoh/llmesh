package authz

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"sort"
	"strings"
	"time"
)

// Cond is a policy condition: a boolean expression over attributes.
//
// On the wire a condition is a JSON object with exactly one key:
//
//	{"all": [cond, ...]}   every condition holds
//	{"any": [cond, ...]}   at least one holds
//	{"not": cond}          the condition does not hold
//	{"<op>": [arg, ...]}   an operator applied to arguments
//
// An argument is a JSON literal (string, number, bool, list of strings), or an
// attribute reference: a string beginning "subject.", "resource.", or
// "context.". A literal string that would read as a reference is written
// {"value": "..."}.
//
// Evaluation is three-valued. A reference to an attribute the request does not
// carry makes the operator Unknown, and Unknown propagates. The engine treats
// Unknown as false for allow policies and true for deny policies, so a missing
// attribute can never grant access and never lets a subject slip a
// restriction.
type Cond struct {
	All []Cond
	Any []Cond
	Not *Cond
	Op  string
	// Args for an operator leaf.
	Args []Arg

	// Filled by prepare at compile time so evaluation does not re-parse
	// literals on every request.
	loc          *time.Location
	from, to     int
	prefix       netip.Prefix
	prefixParsed bool
}

// clone deep-copies a condition, so compiling does not write into a value the
// caller still holds.
func (c Cond) clone() *Cond {
	out := Cond{Op: c.Op, Args: append([]Arg(nil), c.Args...)}
	for _, sub := range c.All {
		out.All = append(out.All, *sub.clone())
	}
	for _, sub := range c.Any {
		out.Any = append(out.Any, *sub.clone())
	}
	if c.Not != nil {
		out.Not = c.Not.clone()
	}
	return &out
}

// prepare parses a validated condition's literals once. Conditions are
// prepared in place, so it walks by pointer.
func prepare(c *Cond) {
	for i := range c.All {
		prepare(&c.All[i])
	}
	for i := range c.Any {
		prepare(&c.Any[i])
	}
	if c.Not != nil {
		prepare(c.Not)
	}
	switch c.Op {
	case "time_between":
		c.from, _ = parseClock(c.Args[1].Lit.(string))
		c.to, _ = parseClock(c.Args[2].Lit.(string))
		c.loc, _ = time.LoadLocation(c.Args[3].Lit.(string))
	case "cidr":
		if p, err := netip.ParsePrefix(c.Args[1].Lit.(string)); err == nil {
			c.prefix, c.prefixParsed = p, true
		}
	}
}

// Arg is an operator argument: a reference or a literal.
type Arg struct {
	Ref string // attribute path, when this is a reference
	Lit any    // literal value otherwise: string, float64, bool, []string
}

// Operators and their argument counts.
var operators = map[string]int{
	"eq": 2, "ne": 2,
	"in": 2, "not_in": 2,
	"has":  2,
	"glob": 2,
	"cidr": 2,
	"lt":   2, "le": 2, "gt": 2, "ge": 2,
	"exists":       1,
	"time_between": 4,
}

const maxCondDepth = 16

func (c *Cond) UnmarshalJSON(b []byte) error {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return fmt.Errorf("condition must be an object: %w", err)
	}
	if len(obj) != 1 {
		return fmt.Errorf("condition must have exactly one key, got %d", len(obj))
	}
	for key, raw := range obj {
		switch key {
		case "all", "any":
			var list []Cond
			if err := json.Unmarshal(raw, &list); err != nil {
				return fmt.Errorf("%q: %w", key, err)
			}
			if len(list) == 0 {
				return fmt.Errorf("%q needs at least one condition", key)
			}
			if key == "all" {
				c.All = list
			} else {
				c.Any = list
			}
		case "not":
			var inner Cond
			if err := json.Unmarshal(raw, &inner); err != nil {
				return fmt.Errorf("\"not\": %w", err)
			}
			c.Not = &inner
		default:
			if _, ok := operators[key]; !ok {
				return fmt.Errorf("unknown operator %q", key)
			}
			var raws []json.RawMessage
			if err := json.Unmarshal(raw, &raws); err != nil {
				return fmt.Errorf("%q: arguments must be a list: %w", key, err)
			}
			c.Op = key
			for _, r := range raws {
				a, err := parseArg(r)
				if err != nil {
					return fmt.Errorf("%q: %w", key, err)
				}
				c.Args = append(c.Args, a)
			}
		}
	}
	return nil
}

func (c Cond) MarshalJSON() ([]byte, error) {
	switch {
	case c.All != nil:
		return json.Marshal(map[string]any{"all": c.All})
	case c.Any != nil:
		return json.Marshal(map[string]any{"any": c.Any})
	case c.Not != nil:
		return json.Marshal(map[string]any{"not": c.Not})
	}
	args := make([]any, len(c.Args))
	for i, a := range c.Args {
		switch {
		case a.Ref != "":
			args[i] = a.Ref
		case isRefString(a.Lit):
			args[i] = map[string]any{"value": a.Lit}
		default:
			args[i] = a.Lit
		}
	}
	return json.Marshal(map[string]any{c.Op: args})
}

func isRefString(v any) bool {
	s, ok := v.(string)
	return ok && isRef(s)
}

func isRef(s string) bool {
	return strings.HasPrefix(s, "subject.") || strings.HasPrefix(s, "resource.") || strings.HasPrefix(s, "context.")
}

func parseArg(raw json.RawMessage) (Arg, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) > 0 && raw[0] == '{' {
		var wrapped struct {
			Value *json.RawMessage `json:"value"`
		}
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&wrapped); err != nil || wrapped.Value == nil {
			return Arg{}, fmt.Errorf("an object argument must be {\"value\": ...}")
		}
		lit, err := parseLiteral(*wrapped.Value)
		return Arg{Lit: lit}, err
	}
	var s string
	if json.Unmarshal(raw, &s) == nil && isRef(s) {
		return Arg{Ref: s}, nil
	}
	lit, err := parseLiteral(raw)
	return Arg{Lit: lit}, err
}

func parseLiteral(raw json.RawMessage) (any, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}
	switch t := v.(type) {
	case string, float64, bool:
		return t, nil
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("list literals must contain only strings")
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("unsupported literal %s", string(raw))
}

// validate checks structure, arity, and the literals that can be checked
// before a request arrives (CIDRs, clock times, time zones).
func (c Cond) validate(depth int) error {
	if depth > maxCondDepth {
		return fmt.Errorf("condition nested deeper than %d", maxCondDepth)
	}
	set := 0
	if c.All != nil {
		set++
	}
	if c.Any != nil {
		set++
	}
	if c.Not != nil {
		set++
	}
	if c.Op != "" {
		set++
	}
	if set != 1 {
		return fmt.Errorf("condition must be exactly one of all, any, not, or an operator")
	}
	for _, list := range [][]Cond{c.All, c.Any} {
		for _, sub := range list {
			if err := sub.validate(depth + 1); err != nil {
				return err
			}
		}
	}
	if c.Not != nil {
		return c.Not.validate(depth + 1)
	}
	if c.Op == "" {
		return nil
	}
	want, ok := operators[c.Op]
	if !ok {
		return fmt.Errorf("unknown operator %q", c.Op)
	}
	if len(c.Args) != want {
		return fmt.Errorf("%q takes %d argument(s), got %d", c.Op, want, len(c.Args))
	}
	for _, a := range c.Args {
		if a.Ref != "" && !validRef(a.Ref) {
			return fmt.Errorf("%q: unknown attribute %q", c.Op, a.Ref)
		}
	}
	switch c.Op {
	case "exists":
		if c.Args[0].Ref == "" {
			return fmt.Errorf("\"exists\" takes an attribute reference")
		}
	case "cidr":
		s, ok := c.Args[1].Lit.(string)
		if !ok {
			return fmt.Errorf("\"cidr\" needs a prefix literal as its second argument")
		}
		if _, err := netip.ParsePrefix(s); err != nil {
			return fmt.Errorf("\"cidr\": %w", err)
		}
	case "time_between":
		for i := 1; i <= 2; i++ {
			s, ok := c.Args[i].Lit.(string)
			if !ok {
				return fmt.Errorf("\"time_between\" needs HH:MM literals")
			}
			if _, err := parseClock(s); err != nil {
				return err
			}
		}
		tz, ok := c.Args[3].Lit.(string)
		if !ok {
			return fmt.Errorf("\"time_between\" needs a time zone literal")
		}
		if _, err := time.LoadLocation(tz); err != nil {
			return fmt.Errorf("\"time_between\": %w", err)
		}
	case "glob":
		if _, ok := c.Args[1].Lit.(string); !ok {
			return fmt.Errorf("\"glob\" needs a pattern literal as its second argument")
		}
	}
	return nil
}

// validRef reports whether a reference names an attribute the evaluator can
// resolve. Free-form attribute maps accept any key.
func validRef(ref string) bool {
	ns, rest, ok := strings.Cut(ref, ".")
	if !ok || rest == "" {
		return false
	}
	switch ns {
	case "subject":
		switch rest {
		case "id", "kind", "roles", "teams", "managed_by":
			return true
		}
		return strings.HasPrefix(rest, "attrs.") && len(rest) > len("attrs.")
	case "resource":
		switch rest {
		case "type", "id", "owner", "team":
			return true
		}
		return true // resource.<attr> reads Resource.Attrs
	case "context":
		switch rest {
		case "time", "source_ip", "endpoint", "priority", "credential_kind", "via_upstream", "prompt_tokens":
			return true
		}
		return strings.HasPrefix(rest, "extra.") && len(rest) > len("extra.")
	}
	return false
}

// contextDependent reports whether the condition reads anything from the
// request context, which makes its decisions uncacheable.
func (c Cond) contextDependent() bool {
	for _, list := range [][]Cond{c.All, c.Any} {
		for _, sub := range list {
			if sub.contextDependent() {
				return true
			}
		}
	}
	if c.Not != nil {
		return c.Not.contextDependent()
	}
	for _, a := range c.Args {
		if strings.HasPrefix(a.Ref, "context.") {
			return true
		}
	}
	return false
}

// tri is a three-valued truth value.
type tri int8

const (
	triFalse tri = iota
	triTrue
	triUnknown
)

func triOf(b bool) tri {
	if b {
		return triTrue
	}
	return triFalse
}

func (c Cond) eval(req *Request) tri {
	switch {
	case c.All != nil:
		out := triTrue
		for _, sub := range c.All {
			switch sub.eval(req) {
			case triFalse:
				return triFalse
			case triUnknown:
				out = triUnknown
			}
		}
		return out
	case c.Any != nil:
		out := triFalse
		for _, sub := range c.Any {
			switch sub.eval(req) {
			case triTrue:
				return triTrue
			case triUnknown:
				out = triUnknown
			}
		}
		return out
	case c.Not != nil:
		switch c.Not.eval(req) {
		case triTrue:
			return triFalse
		case triFalse:
			return triTrue
		}
		return triUnknown
	}
	return c.evalOp(req)
}

func (c Cond) evalOp(req *Request) tri {
	vals := make([]any, len(c.Args))
	for i, a := range c.Args {
		if a.Ref == "" {
			vals[i] = a.Lit
			continue
		}
		v, ok := resolve(req, a.Ref)
		if !ok {
			if c.Op == "exists" {
				return triFalse
			}
			return triUnknown
		}
		vals[i] = v
	}
	switch c.Op {
	case "exists":
		return triTrue
	case "eq":
		return triOf(equal(vals[0], vals[1]))
	case "ne":
		return triOf(!equal(vals[0], vals[1]))
	case "in", "not_in":
		list, ok := asStrings(vals[1])
		if !ok {
			return triUnknown
		}
		s, ok := asString(vals[0])
		if !ok {
			return triUnknown
		}
		found := false
		for _, e := range list {
			if e == s {
				found = true
				break
			}
		}
		if c.Op == "in" {
			return triOf(found)
		}
		return triOf(!found)
	case "has":
		list, ok := asStrings(vals[0])
		if !ok {
			return triUnknown
		}
		s, ok := asString(vals[1])
		if !ok {
			return triUnknown
		}
		for _, e := range list {
			if e == s {
				return triTrue
			}
		}
		return triFalse
	case "glob":
		s, ok := asString(vals[0])
		p, ok2 := vals[1].(string)
		if !ok || !ok2 {
			return triUnknown
		}
		return triOf(Glob(p, s))
	case "cidr":
		addr, ok := vals[0].(netip.Addr)
		if !ok {
			s, isStr := vals[0].(string)
			if !isStr {
				return triUnknown
			}
			var err error
			if addr, err = netip.ParseAddr(s); err != nil {
				return triUnknown
			}
		}
		prefix := c.prefix
		if !c.prefixParsed {
			var err error
			if prefix, err = netip.ParsePrefix(vals[1].(string)); err != nil {
				return triUnknown
			}
		}
		if !addr.IsValid() {
			return triUnknown
		}
		return triOf(prefix.Contains(addr.Unmap()))
	case "lt", "le", "gt", "ge":
		a, ok := asNumber(vals[0])
		b, ok2 := asNumber(vals[1])
		if !ok || !ok2 {
			return triUnknown
		}
		switch c.Op {
		case "lt":
			return triOf(a < b)
		case "le":
			return triOf(a <= b)
		case "gt":
			return triOf(a > b)
		}
		return triOf(a >= b)
	case "time_between":
		t, ok := vals[0].(time.Time)
		if !ok || t.IsZero() {
			return triUnknown
		}
		from, to, loc := c.from, c.to, c.loc
		if loc == nil {
			var err error
			from, _ = parseClock(vals[1].(string))
			to, _ = parseClock(vals[2].(string))
			if loc, err = time.LoadLocation(vals[3].(string)); err != nil {
				return triUnknown
			}
		}
		lt := t.In(loc)
		m := lt.Hour()*60 + lt.Minute()
		if from <= to {
			return triOf(m >= from && m < to)
		}
		return triOf(m >= from || m < to) // wraps midnight, e.g. 22:00–06:00
	}
	return triUnknown
}

func parseClock(s string) (int, error) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("time %q must be HH:MM", s)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// resolve reads an attribute from the request. ok is false when the request
// does not carry it.
func resolve(req *Request, ref string) (any, bool) {
	ns, rest, _ := strings.Cut(ref, ".")
	switch ns {
	case "subject":
		s := req.Subject
		switch rest {
		case "id":
			return s.ID, s.ID != ""
		case "kind":
			return string(s.Kind), s.Kind != ""
		case "roles":
			roles := make([]string, 0, len(s.Bindings))
			for _, b := range s.Bindings {
				roles = append(roles, b.Role)
			}
			return roles, true
		case "teams":
			return append([]string{}, s.Teams...), true
		case "managed_by":
			return s.ManagedBy, true
		}
		if k, ok := strings.CutPrefix(rest, "attrs."); ok {
			v, present := s.Attrs[k]
			return v, present
		}
	case "resource":
		r := req.Resource
		switch rest {
		case "type":
			return r.Type, r.Type != ""
		case "id":
			return r.ID, r.ID != ""
		case "owner":
			return r.Owner, r.Owner != ""
		case "team":
			return r.Team, r.Team != ""
		}
		v, present := r.Attrs[rest]
		return v, present
	case "context":
		c := req.Context
		switch rest {
		case "time":
			return c.Time, !c.Time.IsZero()
		case "source_ip":
			return c.SourceIP, c.SourceIP.IsValid()
		case "endpoint":
			return c.Endpoint, c.Endpoint != ""
		case "priority":
			return c.Priority, c.Priority != ""
		case "credential_kind":
			return c.CredentialKind, c.CredentialKind != ""
		case "via_upstream":
			return c.ViaUpstream, true
		case "prompt_tokens":
			return float64(c.PromptTokens), true
		}
		if k, ok := strings.CutPrefix(rest, "extra."); ok {
			v, present := c.Extra[k]
			return v, present
		}
	}
	return nil, false
}

func asString(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		if t {
			return "true", true
		}
		return "false", true
	case float64:
		return fmt.Sprint(t), true
	case int:
		return fmt.Sprint(t), true
	}
	return "", false
}

func asStrings(v any) ([]string, bool) {
	switch t := v.(type) {
	case []string:
		return t, true
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			s, ok := asString(e)
			if !ok {
				return nil, false
			}
			out = append(out, s)
		}
		return out, true
	}
	return nil, false
}

func asNumber(v any) (float64, bool) {
	switch t := v.(type) {
	case float64:
		return t, !math.IsNaN(t)
	case int:
		return float64(t), true
	case int64:
		return float64(t), true
	}
	return 0, false
}

// equal compares two values: numerically when both are numbers, as sets when
// both are lists, and as strings otherwise.
func equal(a, b any) bool {
	if x, ok := asNumber(a); ok {
		if y, ok := asNumber(b); ok {
			return x == y
		}
	}
	if x, ok := asStrings(a); ok {
		if y, ok := asStrings(b); ok {
			x, y = append([]string{}, x...), append([]string{}, y...)
			sort.Strings(x)
			sort.Strings(y)
			if len(x) != len(y) {
				return false
			}
			for i := range x {
				if x[i] != y[i] {
					return false
				}
			}
			return true
		}
	}
	x, ok := asString(a)
	y, ok2 := asString(b)
	return ok && ok2 && x == y
}
