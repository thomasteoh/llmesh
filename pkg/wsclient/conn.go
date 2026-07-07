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
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
)

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
	backoff := time.Second
	first := true
	for {
		if ctx.Err() != nil {
			return
		}
		if !first {
			c.st.IncrReconnects()
		}
		first = false
		err := c.connect(ctx)
		c.st.SetConnected(false)
		if ctx.Err() != nil {
			return // graceful shutdown completed
		}
		if err != nil {
			c.log.Error("ws: connect error", "error", err, "backoff", backoff.String())
			select {
			case <-time.After(backoff):
			case <-ctx.Done():
				return
			}
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		c.log.Info("ws: disconnected — reconnecting")
	}
}

func (c *Conn) connect(outerCtx context.Context) error {
	header := map[string][]string{
		"Authorization": {"Bearer " + c.routerToken},
	}
	conn, _, err := websocket.DefaultDialer.Dial(c.routerURL, header)
	if err != nil {
		return err
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
				_ = c.send(types.ReleaseMsg{
					Type:      "release",
					RequestID: id,
					Reason:    "client_shutdown",
				})
			}
			// Cancel job goroutines, then send WS close so the read loop exits.
			connCancel()
			c.mu.Lock()
			if ws := c.ws; ws != nil {
				ws.WriteMessage(websocket.CloseMessage,
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
	c.pool.init(maxConc)

	if err := c.send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: maxConc,
		Version:       c.version,
	}); err != nil {
		return err
	}
	c.st.SetConnected(true)
	c.log.Info("ws: registered with router", "models", models, "max_concurrent", maxConc)

	// Read loop.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if outerCtx.Err() != nil {
				return nil // graceful shutdown — not a connection error
			}
			return err
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
			// Pre-check before acquiring a concurrency slot.
			// Try must be fast and non-blocking; it sends its own error if needed.
			if !c.jobs.Try(job, c.send) {
				continue
			}
			// Acquire a concurrency slot. acquireRouter returns false when
			// connCtx is cancelled (shutdown or disconnect), yielding to local
			// requests that are also waiting.
			if !c.pool.acquireRouter(connCtx) {
				continue
			}
			jobCtx, jobCancel := context.WithCancel(connCtx)
			c.cancelsMu.Lock()
			c.cancels[job.Request.ID] = jobCancel
			c.cancelsMu.Unlock()
			c.st.IncrActive()
			go func(j types.JobMsg, jCtx context.Context, jCancel context.CancelFunc) {
				defer func() {
					c.st.DecrActive()
					c.st.IncrDone()
					c.pool.Release()
					c.cancelsMu.Lock()
					delete(c.cancels, j.Request.ID)
					c.cancelsMu.Unlock()
					jCancel()
				}()
				if err := c.jobs.Dispatch(jCtx, j, c.send); err != nil {
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

func (c *Conn) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	ws := c.ws
	if ws == nil {
		c.mu.Unlock()
		return nil
	}
	err = ws.WriteMessage(websocket.TextMessage, data)
	c.mu.Unlock()
	return err
}
