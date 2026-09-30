package wsclient

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

func TestDialURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://llm.example.com":               "wss://llm.example.com/ws/client",
		"https://llm.example.com/":              "wss://llm.example.com/ws/client",
		"http://localhost:53002/ws/client":      "ws://localhost:53002/ws/client",
		"wss://llm.example.com/ws/client":       "wss://llm.example.com/ws/client",
		"wss://llm.example.com/proxy/ws/client": "wss://llm.example.com/proxy/ws/client",
	} {
		if got := DialURL(in); got != want {
			t.Errorf("DialURL(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSlotPool_YieldsOnlyToLocalLoad(t *testing.T) {
	p := newSlotPool()
	p.Init(1)
	var kicks atomic.Int32
	p.SetLocalHook(func() { kicks.Add(1) })

	// A free slot is taken, local load or not.
	if ok, busy := p.acquireRouterOrYield(context.Background(), true); !ok || busy {
		t.Fatalf("free slot: ok=%v busy=%v", ok, busy)
	}
	p.Release()

	if !p.AcquireLocal(context.Background()) {
		t.Fatal("local acquire failed")
	}
	if p.LocalBusy() != 1 || kicks.Load() == 0 {
		t.Fatalf("local load %d, hook calls %d", p.LocalBusy(), kicks.Load())
	}
	if ok, busy := p.acquireRouterOrYield(context.Background(), true); ok || !busy {
		t.Fatalf("slot held locally: ok=%v busy=%v, want a yield", ok, busy)
	}
	// Without yield (an older router) the job waits for the slot, as before.
	got := make(chan bool, 1)
	go func() { got <- p.acquireRouter(context.Background()) }()
	select {
	case <-got:
		t.Fatal("router job took a slot held locally")
	case <-time.After(50 * time.Millisecond):
	}
	p.ReleaseLocal()
	if !<-got {
		t.Fatal("router job did not get the slot once released")
	}
	if p.LocalBusy() != 0 {
		t.Fatalf("local load after release: %d", p.LocalBusy())
	}
}

// fakeRouter accepts one worker connection, acknowledges registration with
// the given features, and records what the worker sends.
type fakeRouter struct {
	srv      *httptest.Server
	features []string
	mu       sync.Mutex
	conn     *websocket.Conn
	got      []map[string]any
}

func newFakeRouter(t *testing.T, features []string) *fakeRouter {
	f := &fakeRouter{features: features}
	up := websocket.Upgrader{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conn = c
		f.mu.Unlock()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			json.Unmarshal(data, &m)
			f.mu.Lock()
			f.got = append(f.got, m)
			f.mu.Unlock()
			if m["type"] == "register" {
				ack, _ := json.Marshal(types.RegisteredMsg{Type: "registered", Features: f.features})
				f.write(ack)
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeRouter) write(data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.conn.WriteMessage(websocket.TextMessage, data)
}

func (f *fakeRouter) ofType(typ string) []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []map[string]any
	for _, m := range f.got {
		if m["type"] == typ {
			out = append(out, m)
		}
	}
	return out
}

func (f *fakeRouter) waitFor(t *testing.T, typ string, n int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if got := f.ofType(typ); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d %q messages; got %v", n, typ, f.got)
	return nil
}

type nopStats struct{}

func (nopStats) SetConnected(bool) {}
func (nopStats) IncrReconnects()   {}
func (nopStats) IncrActive()       {}
func (nopStats) DecrActive()       {}
func (nopStats) IncrDone()         {}
func (nopStats) IncrError()        {}

// lateModels reports no models until ready is set.
type lateModels struct{ ready atomic.Bool }

func (m *lateModels) Models(context.Context) ([]types.ModelInfo, int) {
	if !m.ready.Load() {
		return nil, 0
	}
	return []types.ModelInfo{{Name: "llama3", ContextSize: 8192}}, 2
}
func (m *lateModels) WatchModels() bool { return true }

type countingJobs struct{ dispatched atomic.Int32 }

func (*countingJobs) Try(types.JobMsg, func(any) error) bool { return true }
func (j *countingJobs) Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error {
	j.dispatched.Add(1)
	return send(types.ChunkMsg{Type: "chunk", RequestID: job.Request.ID, Done: true})
}

func runConn(t *testing.T, f *fakeRouter, models ModelProvider, jobs JobDispatcher) *Conn {
	t.Helper()
	c := New(f.srv.URL, "ct-test", 0, "test", nopStats{}, models, jobs, slog.Default())
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	return c
}

func TestLateBackendIsRegisteredWhenReady(t *testing.T) {
	old := reprobeInterval
	reprobeInterval = 20 * time.Millisecond
	t.Cleanup(func() { reprobeInterval = old })

	f := newFakeRouter(t, nil)
	models := &lateModels{}
	runConn(t, f, models, &countingJobs{})
	first := f.waitFor(t, "register", 1)[0]
	if ms, _ := first["models"].([]any); len(ms) != 0 {
		t.Fatalf("first registration: %v", first)
	}
	models.ready.Store(true)
	again := f.waitFor(t, "register", 2)[1]
	if ms, _ := again["models"].([]any); len(ms) != 1 || again["max_concurrent"] != float64(2) {
		t.Fatalf("registration once ready: %v", again)
	}
	time.Sleep(100 * time.Millisecond)
	if n := len(f.ofType("register")); n != 2 {
		t.Errorf("kept registering after the backend was ready: %d registrations", n)
	}
}

func TestJobReturnedWhileLocalRequestsHoldSlots(t *testing.T) {
	f := newFakeRouter(t, []string{types.FeatureLocalBusy})
	jobs := &countingJobs{}
	models := &lateModels{}
	models.ready.Store(true)
	c := runConn(t, f, models, jobs)
	f.waitFor(t, "register", 1)
	f.waitFor(t, "local_busy", 1) // the initial report after the acknowledgement

	// Hold both slots locally.
	for i := 0; i < 2; i++ {
		if !c.Pool().AcquireLocal(context.Background()) {
			t.Fatal("local acquire")
		}
	}
	job, _ := json.Marshal(types.JobMsg{Type: "job", Request: types.InferenceRequest{ID: "j1", Model: "llama3"}})
	f.write(job)
	rel := f.waitFor(t, "release", 1)[0]
	if rel["reason"] != types.ReleaseBusy || rel["request_id"] != "j1" {
		t.Fatalf("release: %v", rel)
	}
	if reports := f.ofType("local_busy"); reports[len(reports)-1]["slots"] != float64(2) {
		t.Errorf("router not told of the local load before the release: %v", reports)
	}
	if jobs.dispatched.Load() != 0 {
		t.Error("the job ran as well as being returned")
	}
	c.Pool().ReleaseLocal()
	c.Pool().ReleaseLocal()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		reports := f.ofType("local_busy")
		if reports[len(reports)-1]["slots"] == float64(0) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Error("router not told when the local load ended")
}

// A router that announced nothing gets the old behaviour: no reports, and a
// job waits for its slot instead of being returned.
func TestOlderRouterGetsNoLocalReports(t *testing.T) {
	f := newFakeRouter(t, nil)
	jobs := &countingJobs{}
	models := &lateModels{}
	models.ready.Store(true)
	c := runConn(t, f, models, jobs)
	f.waitFor(t, "register", 1)
	if !c.Pool().AcquireLocal(context.Background()) || !c.Pool().AcquireLocal(context.Background()) {
		t.Fatal("local acquire")
	}
	job, _ := json.Marshal(types.JobMsg{Type: "job", Request: types.InferenceRequest{ID: "j1", Model: "llama3"}})
	f.write(job)
	time.Sleep(100 * time.Millisecond)
	if len(f.ofType("release")) != 0 || len(f.ofType("local_busy")) != 0 {
		t.Fatalf("an older router was sent messages it does not know: %v", f.got)
	}
	c.Pool().ReleaseLocal()
	f.waitFor(t, "chunk", 1)
	if jobs.dispatched.Load() != 1 {
		t.Error("the waiting job did not run once a slot was free")
	}
}

// switchModels serves whichever model list is current.
type switchModels struct {
	mu     sync.Mutex
	models []types.ModelInfo
}

func (m *switchModels) set(ms ...types.ModelInfo) {
	m.mu.Lock()
	m.models = ms
	m.mu.Unlock()
}
func (m *switchModels) Models(context.Context) ([]types.ModelInfo, int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]types.ModelInfo(nil), m.models...), 1
}
func (m *switchModels) WatchModels() bool { return true }

func TestRecheckRegistersAtOnce(t *testing.T) {
	old := reprobeInterval
	reprobeInterval = time.Hour
	t.Cleanup(func() { reprobeInterval = old })

	f := newFakeRouter(t, nil)
	models := &switchModels{}
	models.set(types.ModelInfo{Name: "a"}, types.ModelInfo{Name: "b"})
	c := runConn(t, f, models, &countingJobs{})
	f.waitFor(t, "register", 1)
	time.Sleep(50 * time.Millisecond) // let the watcher start
	models.set(types.ModelInfo{Name: "a"})
	c.Recheck()
	again := f.waitFor(t, "register", 2)[1]
	if ms, _ := again["models"].([]any); len(ms) != 1 {
		t.Fatalf("after Recheck: %v", again)
	}
}

// blockingJobs runs each job until release is closed.
type blockingJobs struct {
	started chan string
	release chan struct{}
}

func (*blockingJobs) Try(types.JobMsg, func(any) error) bool { return true }
func (j *blockingJobs) Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error {
	j.started <- job.Request.ID
	select {
	case <-j.release:
		return send(types.ChunkMsg{Type: "chunk", RequestID: job.Request.ID, Delta: "done", Done: true})
	case <-ctx.Done():
		return nil
	}
}

func drainSetup(t *testing.T, features []string, timeout time.Duration) (*fakeRouter, *Conn, *blockingJobs, context.CancelFunc, chan struct{}, chan struct{}) {
	t.Helper()
	f := newFakeRouter(t, features)
	jobs := &blockingJobs{started: make(chan string, 4), release: make(chan struct{})}
	models := &switchModels{}
	models.set(types.ModelInfo{Name: "llama3"})
	c := New(f.srv.URL, "ct-test", 2, "test", nopStats{}, models, jobs, slog.Default())
	force := make(chan struct{})
	c.SetDrain(timeout, force)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	t.Cleanup(func() { cancel(); <-done })
	f.waitFor(t, "register", 1)
	time.Sleep(50 * time.Millisecond) // the acknowledgement
	job, _ := json.Marshal(types.JobMsg{Type: "job", Request: types.InferenceRequest{ID: "j1", Model: "llama3"}})
	f.write(job)
	<-jobs.started
	return f, c, jobs, cancel, force, done
}

func TestShutdownLetsJobsFinish(t *testing.T) {
	f, _, jobs, cancel, _, done := drainSetup(t, []string{types.FeatureDrain}, 5*time.Second)
	cancel()
	f.waitFor(t, "draining", 1)
	// A job the router sent before it saw the drain goes straight back.
	late, _ := json.Marshal(types.JobMsg{Type: "job", Request: types.InferenceRequest{ID: "j2", Model: "llama3"}})
	f.write(late)
	rel := f.waitFor(t, "release", 1)[0]
	if rel["request_id"] != "j2" || rel["reason"] != types.ReleaseShutdown {
		t.Fatalf("late job: %v", rel)
	}
	select {
	case <-done:
		t.Fatal("shut down without waiting for the running job")
	case <-time.After(100 * time.Millisecond):
	}
	close(jobs.release)
	f.waitFor(t, "chunk", 1)
	<-done
	for _, r := range f.ofType("release") {
		if r["request_id"] == "j1" {
			t.Error("the finished job was handed back too")
		}
	}
}

func TestShutdownHandsBackWhatDoesNotFinish(t *testing.T) {
	f, _, _, cancel, force, done := drainSetup(t, []string{types.FeatureDrain}, 5*time.Second)
	cancel()
	f.waitFor(t, "draining", 1)
	close(force) // a second signal
	rel := f.waitFor(t, "release", 1)[0]
	if rel["request_id"] != "j1" {
		t.Fatalf("release: %v", rel)
	}
	<-done
}

func TestShutdownWithoutDrainSupportIsImmediate(t *testing.T) {
	f, _, _, cancel, _, done := drainSetup(t, nil, 5*time.Second)
	cancel()
	rel := f.waitFor(t, "release", 1)[0]
	if rel["request_id"] != "j1" {
		t.Fatalf("release: %v", rel)
	}
	<-done
	if len(f.ofType("draining")) != 0 {
		t.Error("an older router was sent a draining message")
	}
}
