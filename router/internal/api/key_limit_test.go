package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"llmesh/pkg/types"
)

type limitKeys struct{ labels map[string]string }

func (k limitKeys) ValidAPIKey(key string) bool       { _, ok := k.labels[key]; return ok }
func (k limitKeys) PriorityFor(string) types.Priority { return types.PriorityNormal }
func (k limitKeys) OwnerFor(string) string            { return "alice" }
func (k limitKeys) LabelFor(key string) string        { return k.labels[key] }
func (k limitKeys) MaxConcurrentFor(key string) int   { return 1 }

type inFlightByKey map[string]int

func (m inFlightByKey) KeyInFlight(label string) int { return m[label] }

// The per-key limit counts that key's jobs, not every job its owner has in
// flight: one busy key must not lock the owner's other keys out.
func TestPerKeyConcurrencyCountsTheKey(t *testing.T) {
	keys := limitKeys{labels: map[string]string{"sk-a": "alice/busy", "sk-b": "alice/idle"}}
	h := &Handler{Keys: keys, Limits: keys, InFlight: inFlightByKey{"alice/busy": 1}}

	send := func(key string) int {
		r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("not json"))
		r.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		h.OpenAI()(w, r)
		return w.Code
	}
	if code := send("sk-a"); code != http.StatusTooManyRequests {
		t.Fatalf("the key at its limit got %d, want 429", code)
	}
	// The other key passes the limit and fails later, on the body.
	if code := send("sk-b"); code == http.StatusTooManyRequests {
		t.Fatal("a key with nothing in flight was limited by its sibling")
	}
}
