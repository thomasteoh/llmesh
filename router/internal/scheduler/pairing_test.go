package scheduler

import (
	"encoding/json"
	"log/slog"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"llmesh/pkg/types"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
)

// tablePairing answers from a table keyed by requester and client owner;
// anything not listed is allowed as a plain non-owner.
type tablePairing map[string]Pairing

func (t tablePairing) Pair(reqOwner string, c types.ClientSummary) Pairing {
	if reqOwner == c.Owner {
		return Pairing{Allowed: true, OwnerSide: true}
	}
	if p, ok := t[reqOwner+"@"+c.Owner]; ok {
		return p
	}
	return Pairing{Allowed: true}
}

func finishJob(t *testing.T, conn *websocket.Conn, id string) {
	t.Helper()
	msg, _ := json.Marshal(types.ChunkMsg{Type: "chunk", RequestID: id, Done: true, FinishReason: "stop"})
	if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
		t.Fatal(err)
	}
	time.Sleep(50 * time.Millisecond)
}

func pairingSetup(t *testing.T, p tablePairing) (*hub.Hub, *queue.Queue, *Scheduler, *websocket.Conn) {
	t.Helper()
	h := hub.New(slog.Default())
	q := queue.New()
	s := New(q, h, noAlias{}, slog.Default())
	s.SetPairingPolicy(p)
	// A pairing policy supersedes isolation: this map must be ignored.
	s.SetIsolationProvider(mapIso{"bob": {SendIsolated: true}})
	conn := dialClient(t, h, "alice", "ct-alice", nil)
	registerModels(t, conn, "llama3") // max_concurrent 2
	return h, q, s, conn
}

func TestPairing_PrivateClientRefusesOthers(t *testing.T) {
	_, q, s, conn := pairingSetup(t, tablePairing{"bob@alice": {Allowed: false}})
	q.Push(types.InferenceRequest{ID: "bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	// (A read that times out poisons a gorilla connection, so absence is
	// checked on the queue rather than the socket.)
	if q.Len() != 1 {
		t.Fatal("a refused requester was dispatched")
	}
	// The owner's own request is unaffected, and is not shadowed by bob's.
	q.Push(types.InferenceRequest{ID: "alice-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil || job.Request.ID != "alice-1" {
		t.Fatal("the owner's request did not run")
	}
}

func TestPairing_ShareWhenIdle(t *testing.T) {
	_, q, s, conn := pairingSetup(t, tablePairing{"bob@alice": {Allowed: true, IdleOnly: true}})

	// Alice is using her machine: bob waits even though a slot is free.
	q.Push(types.InferenceRequest{ID: "alice-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil || job.Request.ID != "alice-1" {
		t.Fatal("the owner's job did not run")
	}
	q.Push(types.InferenceRequest{ID: "bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if q.Len() != 1 {
		t.Fatal("bob ran on a busy share-when-idle client")
	}

	// Once she is done, the machine is idle and bob's job runs.
	finishJob(t, conn, "alice-1")
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil || job.Request.ID != "bob-1" {
		t.Fatal("bob did not run once the client was idle")
	}
}

func TestPairing_PerRequesterCap(t *testing.T) {
	_, q, s, conn := pairingSetup(t, tablePairing{"bob@alice": {Allowed: true, PerRequesterMax: 1}})
	q.Push(types.InferenceRequest{ID: "bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	q.Push(types.InferenceRequest{ID: "bob-2", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now().Add(time.Millisecond)})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil || job.Request.ID != "bob-1" {
		t.Fatal("bob's first job did not run")
	}
	if q.Len() != 1 {
		t.Fatalf("queue length %d, want bob-2 still waiting", q.Len())
	}
}

func TestPairing_ReservedSlots(t *testing.T) {
	// One of two slots is held back from non-owners.
	_, q, s, conn := pairingSetup(t, tablePairing{"bob@alice": {Allowed: true, Reserved: map[string]int{"*": 1}}})
	q.Push(types.InferenceRequest{ID: "bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	q.Push(types.InferenceRequest{ID: "bob-2", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now().Add(time.Millisecond)})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil {
		t.Fatal("bob's first job did not run")
	}
	if q.Len() != 1 {
		t.Fatal("bob took the reserved slot")
	}
	// The reserved slot is still there for alice.
	q.Push(types.InferenceRequest{ID: "alice-1", Model: "llama3", Owner: "alice", EnqueuedAt: time.Now()})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil || job.Request.ID != "alice-1" {
		t.Fatal("the owner could not use her reserved slot")
	}
}

// With a pairing policy, the legacy isolation map is not consulted: bob is
// send-isolated in it, but the policy allows him.
func TestPairing_SupersedesIsolation(t *testing.T) {
	_, q, s, conn := pairingSetup(t, tablePairing{})
	q.Push(types.InferenceRequest{ID: "bob-1", Model: "llama3", Owner: "bob", EnqueuedAt: time.Now()})
	s.drainQueue()
	if job := readJob(t, conn, 300*time.Millisecond); job == nil {
		t.Fatal("the legacy isolation flag still applied under a pairing policy")
	}
}
