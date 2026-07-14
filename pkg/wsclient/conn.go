// pkg/wsclient/conn.go
//
// Shared WebSocket connection lifecycle for llmesh-client and llmesh-shim.
// Both components connect to the router over WebSocket, register, dispatch jobs,
// and handle reconnection with exponential backoff. This package captures that
// shared logic; callers supply the model list and job dispatcher via interfaces.
package wsclient

import (
	"context"
	"encoding/json"
	"log/slog"
	"math/rand"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

const (
	pingInterval   = 30 * time.Second
	pongWait       = 60 * time.Second
	initialBackoff = time.Second
	maxBackoff     = 60 * time.Second
)

// jitter returns d perturbed by ±20% to avoid a thundering herd of workers all
// reconnecting in lockstep after a router restart.
func jitter(d time.Duration) time.Duration {
	delta := (rand.Float64()*2 - 1) * 0.2 * float64(d)
	return d + time.Duration(delta)
}

// ConnStats abstracts the live counters updated during connection lifecycle.
// Satisfied by *client/internal/stats.Stats and *shim/internal/stats.Stats.
type ConnStats interface {
	SetConnected(bool)
	IncrReconnects()
	IncrActive()
	DecrActive()
	IncrDone()
	IncrError()
}

// ModelProvider returns the models this connection should advertise on registration,
// plus the total number of inference slots detected (e.g. from llama.cpp total_slots).
// For llmesh-client it probes llama.cpp; for llmesh-shim it reads from config (slots=0).
type ModelProvider interface {
	Models(ctx context.Context) ([]types.ModelInfo, int)
}

// JobDispatcher handles a single job received from the router.
//
// Try is called in the read loop before acquiring a concurrency slot.
// It should perform any quick checks (e.g. backend availability) and return
// true to proceed or false to reject the job. On false, Try should send a
// suitable error back to the router itself. Try must be fast and non-blocking.
//
// Dispatch is called only when Try returns true and a concurrency slot is
// acquired. It runs inference and returns a non-nil error only when inference
// itself failed (not on ctx cancellation).
type JobDispatcher interface {
	Try(job types.JobMsg, send func(any) error) bool
	Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error
}

// Conn manages a WebSocket connection to the router with automatic reconnection.
// It is safe to call Run from a single goroutine.
type Conn struct {
	routerURL   string
	routerToken string
	maxConc     int
	version     string
	st          ConnStats
	models      ModelProvider
	jobs        JobDispatcher
	log         *slog.Logger
	onUpdate    func() // called when the router sends an "update" message
	pool        *SlotPool

	mu        sync.Mutex
	ws        *websocket.Conn
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc
}

// New creates a Conn. Call Run to start the connection loop.
func New(
	routerURL, routerToken string,
	maxConc int,
	version string,
	st ConnStats,
	models ModelProvider,
	jobs JobDispatcher,
	log *slog.Logger,
) *Conn {
	return &Conn{
		routerURL:   routerURL,
		routerToken: routerToken,
		maxConc:     maxConc,
		version:     version,
		st:          st,
		models:      models,
		jobs:        jobs,
		log:         log,
		pool:        newSlotPool(),
		cancels:     make(map[string]context.CancelFunc),
	}
}

// Pool returns the shared slot pool used to limit concurrency across both
// router-dispatched jobs and local API requests. Pass this to the local API
// server so local requests compete for the same slots and take priority over
// queued router jobs.
func (c *Conn) Pool() *SlotPool { return c.pool }

// SetOnUpdate registers a callback invoked when the router sends an "update" message.
// Must be called before Run. Safe to call with nil to clear.
func (c *Conn) SetOnUpdate(fn func()) {
	c.onUpdate = fn
}

// Run connects to the router and reconnects on disconnect. Blocks until ctx is cancelled.
func (c *Conn) Run(ctx context.Context) {
	backoff := initialBackoff
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first {
			c.st.IncrReconnects()
		}
		first = false
		registered, err := c.connect(ctx)
		c.st.SetConnected(false)
		if ctx.Err() != nil {
			return // graceful shutdown completed
		}
		// A session that got as far as registering was healthy; reset the
		// backoff so a long-lived connection that later drops reconnects
		// promptly instead of inheriting a backoff grown during an earlier
		// outage. Only a never-registered attempt (router down, bad token)
		// keeps escalating.
		if registered {
			backoff = initialBackoff
		}
		if err != nil {
			wait := jitter(backoff)
			c.log.Error("ws: connect error", "error", err, "backoff", wait.String())
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return
			}
			if backoff < maxBackoff {
				backoff *= 2
			}
			continue
		}
		c.log.Info("ws: disconnected — reconnecting")
	}
}

func (c *Conn) connect(outerCtx context.Context) (registered bool, err error) {
	header := map[string][]string{
		"Authorization": {"Bearer " + c.routerToken},
	}
	// DialContext (not Dial) so a hanging TLS handshake is aborted promptly on
	// shutdown instead of blocking for the dialer's handshake timeout.
	conn, _, err := websocket.DefaultDialer.DialContext(outerCtx, c.routerURL, header)
	if err != nil {
		return false, err
	}
	conn.SetReadLimit(16 << 20) // 16 MiB — prevents OOM from oversized router frames

	// connCtx is cancelled when this connection closes or graceful shutdown completes.
	// Intentionally not derived from outerCtx so we control when job goroutines are cancelled.
	connCtx, connCancel := context.WithCancel(context.Background())

	c.mu.Lock()
	c.ws = conn
	c.mu.Unlock()

	defer func() {
		connCancel()
		conn.Close()
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
	}()

	// send targets this specific connection. After a reconnect a lingering job
	// goroutine from the old connection would otherwise write stale frames onto
	// the new socket; the c.ws == conn check drops those writes.
	send := func(msg any) error {
		data, err := json.Marshal(msg)
		if err != nil {
			return err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.ws != conn {
			return nil // connection replaced; drop stale write
		}
		return conn.WriteMessage(websocket.TextMessage, data)
	}

	// Keepalive: refresh read deadline on every pong.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Ping goroutine.
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				c.mu.Lock()
				ws := c.ws
				c.mu.Unlock()
				if ws == nil {
					return
				}
				if err := ws.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Graceful shutdown watcher: on SIGTERM, notify the router about in-flight jobs
	// before cancelling job contexts and closing the connection.
	go func() {
		select {
		case <-outerCtx.Done():
			c.log.Info("ws: shutdown signal received, notifying router of in-flight jobs")
			c.cancelsMu.Lock()
			ids := make([]string, 0, len(c.cancels))
			for id := range c.cancels {
				ids = append(ids, id)
			}
			c.cancelsMu.Unlock()
			for _, id := range ids {
				_ = send(types.ReleaseMsg{
					Type:      "release",
					RequestID: id,
					Reason:    "client_shutdown",
				})
			}
			// Cancel job goroutines, then send WS close so the read loop exits.
			connCancel()
			c.mu.Lock()
			if c.ws == conn {
				conn.WriteMessage(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
			}
			c.mu.Unlock()
		case <-connCtx.Done():
		}
	}()

	// Probe models and resolve concurrency limit.
	// detected slots from llama.cpp total_slots; c.maxConc=0 means auto-detect.
	models, detectedSlots := c.models.Models(connCtx)
	maxConc := c.maxConc
	if maxConc <= 0 {
		maxConc = detectedSlots
	}
	if maxConc < 1 {
		maxConc = 1
	}
	c.pool.Init(maxConc)

	if err := send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: maxConc,
		Version:       c.version,
	}); err != nil {
		return registered, err
	}
	registered = true
	c.st.SetConnected(true)
	c.log.Info("ws: registered with router", "models", models, "max_concurrent", maxConc)

	// Read loop.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if outerCtx.Err() != nil {
				return registered, nil // graceful shutdown — not a connection error
			}
			return registered, err
		}
		conn.SetReadDeadline(time.Now().Add(pongWait))

		// Decode once into the union of all router→client message types rather
		// than decoding the envelope and then re-decoding the full message (which
		// re-parses the entire InferenceRequest for every job).
		var in struct {
			Type      string                 `json:"type"`
			Request   types.InferenceRequest `json:"request"`
			RequestID string                 `json:"request_id"`
		}
		if err := json.Unmarshal(data, &in); err != nil {
			continue
		}

		switch in.Type {
		case "job":
			job := types.JobMsg{Type: in.Type, Request: in.Request}
			// Pre-check before accepting the job.
			// Try must be fast and non-blocking; it sends its own error if needed.
			if !c.jobs.Try(job, send) {
				continue
			}
			// Register the cancel func before spawning so a "cancel" message can
			// abort the job even while it is still waiting for a slot.
			jobCtx, jobCancel := context.WithCancel(connCtx)
			c.cancelsMu.Lock()
			c.cancels[job.Request.ID] = jobCancel
			c.cancelsMu.Unlock()
			// Acquire the slot inside the goroutine — never on the read loop.
			// Blocking here would stall pong processing (dropping the connection
			// after pongWait) and cancel handling. This matters because local API
			// requests consume slots the router does not know about, so a router
			// job can wait arbitrarily long for a slot.
			go func(j types.JobMsg, jobCtx context.Context, jobCancel context.CancelFunc) {
				defer func() {
					c.cancelsMu.Lock()
					delete(c.cancels, j.Request.ID)
					c.cancelsMu.Unlock()
					jobCancel()
				}()
				defer func() {
					if r := recover(); r != nil {
						c.log.Error("ws: job dispatch panicked", "request_id", j.Request.ID, "panic", r)
						_ = send(types.ErrorMsg{Type: "error", RequestID: j.Request.ID, Message: "internal worker error"})
						c.st.IncrError()
					}
				}()
				// acquireRouter returns false when jobCtx is cancelled (shutdown,
				// disconnect, or an explicit cancel), yielding to waiting local
				// requests. The router requeues the job on disconnect, so no
				// notification is needed on that path.
				if !c.pool.acquireRouter(jobCtx) {
					return
				}
				defer c.pool.Release()
				c.st.IncrActive()
				defer func() {
					c.st.DecrActive()
					c.st.IncrDone()
				}()
				if err := c.jobs.Dispatch(jobCtx, j, send); err != nil {
					c.st.IncrError()
				}
			}(job, jobCtx, jobCancel)

		case "cancel":
			c.cancelsMu.Lock()
			if cancel, ok := c.cancels[in.RequestID]; ok {
				c.log.Info("ws: cancelling job", "request_id", in.RequestID)
				cancel()
				delete(c.cancels, in.RequestID)
			}
			c.cancelsMu.Unlock()

		case "update":
			if fn := c.onUpdate; fn != nil {
				go fn()
			}
		}
	}
}
