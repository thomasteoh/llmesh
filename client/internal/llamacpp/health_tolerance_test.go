package llamacpp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// healthSrv serves /health, failing the first failures probes and succeeding
// after. Returns the server and a counter of probes received.
func healthSrv(failures int32) (*httptest.Server, *atomic.Int32) {
	var seen atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen.Add(1) <= failures {
			// Hijack and drop the connection: a probe that gets no reply is
			// what a saturated llama.cpp actually produces.
			hj, ok := w.(http.Hijacker)
			if !ok {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	return srv, &seen
}

// probeInterval is the tick these tests drive watchHealth at, so they exercise
// the real loop without waiting out healthCheckInterval.
const probeInterval = 10 * time.Millisecond

// watchHealth aborts the inference on a failed probe. A box busy prefilling a
// large prompt can miss probes without being hung, and treating that as fatal
// killed the long requests the health check exists to protect.
func TestWatchHealth_ToleratesTransientFailures(t *testing.T) {
	srv, seen := healthSrv(healthCheckFailures - 1)
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cancelled := make(chan struct{})
	go New(srv.URL, nil).watchHealth(ctx, func() { close(cancelled) }, make(chan struct{}), probeInterval)

	// Long enough for every failing probe plus several recovered ones.
	deadline := time.After(2 * time.Second)
	for seen.Load() <= int32(healthCheckFailures) {
		select {
		case <-cancelled:
			t.Fatalf("inference cancelled after %d probes, before the backend had a chance to recover", seen.Load())
		case <-deadline:
			t.Fatalf("only %d probes reached the server", seen.Load())
		case <-time.After(5 * time.Millisecond):
		}
	}

	select {
	case <-cancelled:
		t.Fatal("inference cancelled despite the backend recovering")
	case <-time.After(50 * time.Millisecond):
	}
}

// A genuinely hung backend must still be caught, just not on the first miss.
func TestWatchHealth_CancelsAfterConsecutiveFailures(t *testing.T) {
	srv, seen := healthSrv(1 << 30) // never recovers
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cancelled := make(chan struct{})
	go New(srv.URL, nil).watchHealth(ctx, func() { close(cancelled) }, make(chan struct{}), probeInterval)

	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("a backend that never answered was never given up on")
	}
	if got := seen.Load(); got < int32(healthCheckFailures) {
		t.Errorf("cancelled after %d probes, want at least %d", got, healthCheckFailures)
	}
}
