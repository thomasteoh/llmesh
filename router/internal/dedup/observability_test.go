package dedup

import (
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
)

// capture collects log records so a test can assert on what an operator would
// see, rather than on internal state the operator has no access to.
type capture struct {
	mu      sync.Mutex
	records []slog.Record
}

func (c *capture) Enabled(context.Context, slog.Level) bool { return true }
func (c *capture) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *capture) WithGroup(string) slog.Handler            { return c }

func (c *capture) Handle(_ context.Context, r slog.Record) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.records = append(c.records, r)
	return nil
}

// find returns the first record at or above level whose message contains want,
// along with its attributes.
func (c *capture) find(level slog.Level, want string) (slog.Record, map[string]slog.Value, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, r := range c.records {
		if r.Level < level || !strings.Contains(r.Message, want) {
			continue
		}
		attrs := make(map[string]slog.Value, r.NumAttrs())
		r.Attrs(func(a slog.Attr) bool {
			attrs[a.Key] = a.Value
			return true
		})
		return r, attrs, true
	}
	return slog.Record{}, nil, false
}

func (c *capture) countAtLeast(level slog.Level) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, r := range c.records {
		if r.Level >= level {
			n++
		}
	}
	return n
}

func newCaptured() (*Registry, *capture) {
	c := &capture{}
	return New(slog.New(c)), c
}

// subscribeSilently adds a follower that never reads, which is how a follower
// overflows in production: its HTTP client stops consuming the response.
func subscribeSilently(t *testing.T, r *Registry, hash string) {
	t.Helper()
	if _, _, live := r.RegisterOrSubscribe(hash); live == nil {
		t.Fatal("expected a live channel for the follower")
	}
}

// A follower dropped for falling behind used to leave no trace at all. Its own
// request failed with a truncated stream, which looks like a worker fault, so
// nothing pointed at the real cause.
func TestForward_LogsFollowerOverflow(t *testing.T) {
	r, logs := newCaptured()
	hash := strings.Repeat("a", 64)
	r.RegisterOrSubscribe(hash)
	subscribeSilently(t, r, hash)

	// The follower's channel holds 256; one past that is the first drop.
	for i := 0; i < 300; i++ {
		r.Forward(hash, chunk("x"))
	}

	_, attrs, ok := logs.find(slog.LevelWarn, "fell behind")
	if !ok {
		t.Fatal("no warning logged for a follower that was dropped")
	}
	if got := attrs["followers_failed"].Int64(); got != 1 {
		t.Errorf("followers_failed = %d, want 1", got)
	}
	if got := attrs["hash"].String(); got != hash[:12] {
		t.Errorf("hash = %q, want %q", got, hash[:12])
	}
}

// The drop is permanent, so it must be reported once and not once per chunk —
// a per-chunk warning would bury the rest of the log for the whole generation.
func TestForward_LogsOverflowOncePerFollower(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")
	subscribeSilently(t, r, "h")

	for i := 0; i < 1000; i++ {
		r.Forward("h", chunk("x"))
	}

	if n := logs.countAtLeast(slog.LevelWarn); n != 1 {
		t.Errorf("got %d warnings, want exactly 1", n)
	}
}

// Releasing the replay buffer changes how later duplicates are served, so it is
// worth a line — but it is routine, not a fault, so it must not be a warning.
func TestForward_LogsReplayBufferRelease(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")

	for i := 0; i < maxReplayChunks+5; i++ {
		r.Forward("h", chunk("x"))
	}

	if _, _, ok := logs.find(slog.LevelInfo, "replay buffer released"); !ok {
		t.Error("no line logged when the replay buffer was released")
	}
	if n := logs.countAtLeast(slog.LevelWarn); n != 0 {
		t.Errorf("got %d warnings for a routine buffer release, want 0", n)
	}
}

// When an inference dies, the follower count is the blast radius, and it is
// only knowable here: each failing caller sees only its own request.
func TestUnregister_LogsAbandonedFollowers(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")
	subscribeSilently(t, r, "h")
	subscribeSilently(t, r, "h")

	r.Unregister("h")

	_, attrs, ok := logs.find(slog.LevelWarn, "without completion")
	if !ok {
		t.Fatal("no warning logged when an entry died with followers attached")
	}
	if got := attrs["followers_failed"].Int64(); got != 2 {
		t.Errorf("followers_failed = %d, want 2", got)
	}
}

func TestReset_LogsFollowersLosingTheirPartialAnswer(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")
	subscribeSilently(t, r, "h")
	r.Forward("h", chunk("partial"))

	r.Reset("h")

	_, attrs, ok := logs.find(slog.LevelWarn, "retry cost coalesced followers")
	if !ok {
		t.Fatal("no warning logged when a retry invalidated a follower's output")
	}
	if got := attrs["followers_failed"].Int64(); got != 1 {
		t.Errorf("followers_failed = %d, want 1", got)
	}
}

// A follower that has not been sent anything yet rides the retry normally, so
// it is not a casualty and must not be counted as one.
func TestReset_DoesNotCountUntouchedFollowers(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")
	subscribeSilently(t, r, "h")

	r.Reset("h")

	if n := logs.countAtLeast(slog.LevelWarn); n != 0 {
		t.Errorf("got %d warnings when no follower had been served, want 0", n)
	}
}

// The common case — a follower that keeps up and a request that finishes — has
// to stay silent, or the warnings above mean nothing.
func TestForward_SuccessfulCoalescingIsQuiet(t *testing.T) {
	r, logs := newCaptured()
	r.RegisterOrSubscribe("h")
	_, _, live := r.RegisterOrSubscribe("h")

	// Fewer chunks than the subscriber channel holds, and read straight from
	// it. The point here is that a healthy run is quiet, so the test must not
	// depend on the reader being scheduled promptly enough to avoid an
	// overflow — that dependency makes it fail under load and report a
	// scheduling artefact as a logging fault.
	const chunks = 200
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i := 0; i < chunks; i++ {
			r.Forward("h", chunk("x"))
		}
		r.Forward("h", doneChunk())
		r.Unregister("h")
	}()

	// drain runs on the test goroutine so its t.Fatal on stall is legal.
	text, sawDone := drain(t, live)
	<-sent

	if len(text) != chunks {
		t.Errorf("follower received %d chunks of text, want %d", len(text), chunks)
	}
	if !sawDone {
		t.Error("follower never saw a terminal chunk")
	}
	if n := logs.countAtLeast(slog.LevelWarn); n != 0 {
		t.Errorf("got %d warnings on a clean coalesced run, want 0", n)
	}
}
