// router/internal/upstream/upstream.go
//
// Connector manages outbound WebSocket connections to upstream (orchestrator) routers.
// Each configured UpstreamRouter results in a persistent connection that presents
// this router as a GPU client — advertising locally-available models and forwarding
// dispatched jobs through the local queue and scheduler.
//
// Flow for a job received from upstream:
//
//	upstream JobMsg → correlation.Store.Create → queue.Push → sched.Wake
//	local hub.OnChunk → correlation.Store.Send → Connector → upstream ChunkMsg
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
	"llmesh/router/internal/admin"
	"llmesh/router/internal/correlation"
	"llmesh/router/internal/hub"
	"llmesh/router/internal/queue"
	"llmesh/router/internal/scheduler"
)

const (
	pingInterval      = 30 * time.Second
	pongWait          = 60 * time.Second
	modelPollInterval = 30 * time.Second
)

// Connector manages outbound connections to upstream routers.
type Connector struct {
	h     *hub.Hub
	q     *queue.Queue
	store *correlation.Store
	sched *scheduler.Scheduler
	log   *slog.Logger

	mu        sync.Mutex
	cancels   map[string]context.CancelFunc
	connected map[string]bool
}

// New creates a Connector. Call Reload to start connections.
func New(h *hub.Hub, q *queue.Queue, store *correlation.Store, sched *scheduler.Scheduler, log *slog.Logger) *Connector {
	return &Connector{
		h:         h,
		q:         q,
		store:     store,
		sched:     sched,
		log:       log,
		cancels:   make(map[string]context.CancelFunc),
		connected: make(map[string]bool),
	}
}

// Reload updates the set of active upstream connections to match the given list.
// Additions are started immediately; removed entries are stopped gracefully.
func (c *Connector) Reload(ctx context.Context, upstreams []admin.UpstreamRouter) {
	c.mu.Lock()
	defer c.mu.Unlock()

	want := make(map[string]admin.UpstreamRouter)
	for _, u := range upstreams {
		if u.URL != "" && u.Token != "" {
			want[u.URL] = u
		}
	}

	// Stop goroutines for removed upstreams.
	for url, cancel := range c.cancels {
		if _, ok := want[url]; !ok {
			cancel()
			delete(c.cancels, url)
			delete(c.connected, url)
			c.log.Info("upstream: removed", "url", url)
		}
	}

	// Start goroutines for new upstreams.
	for url, u := range want {
		if _, already := c.cancels[url]; !already {
			gCtx, cancel := context.WithCancel(ctx)
			c.cancels[url] = cancel
			go c.run(gCtx, u)
			c.log.Info("upstream: connecting", "url", url, "name", u.Name)
		}
	}
}

// Connected reports whether the given upstream URL currently has an active session.
func (c *Connector) Connected(url string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.connected[url]
}

func (c *Connector) setConnected(url string, v bool) {
	c.mu.Lock()
	c.connected[url] = v
	c.mu.Unlock()
}

// run maintains a persistent connection to a single upstream router, reconnecting
// with exponential backoff on failure.
func (c *Connector) run(ctx context.Context, u admin.UpstreamRouter) {
	backoff := time.Second
	for {
		if ctx.Err() != nil {
			return
		}
		err := c.connect(ctx, u)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			c.log.Warn("upstream: connection failed", "url", u.URL, "error", err, "retry_in", backoff)
		} else {
			c.log.Info("upstream: disconnected — reconnecting", "url", u.URL)
		}
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		if backoff < 60*time.Second {
			backoff *= 2
		}
	}
}

// connect establishes one WebSocket session with the upstream router.
func (c *Connector) connect(ctx context.Context, u admin.UpstreamRouter) error {
	wsURL := toWSURL(u.URL) + "/ws/client"
	header := http.Header{"Authorization": {"Bearer " + u.Token}}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, header)
	if err != nil {
		return err
	}
	defer conn.Close()

	connCtx, connCancel := context.WithCancel(ctx)
	defer connCancel()

	// All writes go through sendCh so they are serialised onto the single WS conn.
	sendCh := make(chan []byte, 64)
	go func() {
		for data := range sendCh {
			if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
				connCancel()
				return
			}
		}
	}()

	send := func(msg any) bool {
		data, err := json.Marshal(msg)
		if err != nil {
			return false
		}
		select {
		case sendCh <- data:
			return true
		default:
			c.log.Warn("upstream: send buffer full, dropping message", "url", u.URL)
			return false
		}
	}

	// Keepalive.
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})
	go func() {
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Register with the upstream — advertise all locally available models.
	models := c.h.ActiveModelInfos()
	slots := c.h.TotalSlots()
	if slots == 0 {
		slots = 1 // always claim at least one slot; local queue handles overflow
	}
	if !send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: slots,
		Version:       "router",
	}) {
		return fmt.Errorf("send register failed")
	}
	c.log.Info("upstream: registered", "url", u.URL, "models", len(models), "slots", slots)
	c.setConnected(u.URL, true)
	defer c.setConnected(u.URL, false)

	lastKey := modelInfoKey(models)

	// Poll for local model changes and reconnect to re-register when they occur.
	go func() {
		ticker := time.NewTicker(modelPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if modelInfoKey(c.h.ActiveModelInfos()) != lastKey {
					c.log.Info("upstream: model set changed, reconnecting to re-register", "url", u.URL)
					conn.Close()
				}
			case <-connCtx.Done():
				return
			}
		}
	}()

	// Per-job cancel functions for this connection.
	var jobsMu sync.Mutex
	jobs := make(map[string]context.CancelFunc)

	// Read loop.
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			jobsMu.Lock()
			for _, cancel := range jobs {
				cancel()
			}
			jobsMu.Unlock()
			close(sendCh)
			return err
		}
		conn.SetReadDeadline(time.Now().Add(pongWait))

		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}

		switch env.Type {
		case "job":
			var msg types.JobMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				c.log.Warn("upstream: bad job message", "url", u.URL, "error", err)
				continue
			}
			jobCtx, jobCancel := context.WithCancel(connCtx)
			jobsMu.Lock()
			jobs[msg.Request.ID] = jobCancel
			jobsMu.Unlock()
			go func(req types.InferenceRequest) {
				defer func() {
					jobsMu.Lock()
					delete(jobs, req.ID)
					jobsMu.Unlock()
					jobCancel()
				}()
				c.handleJob(jobCtx, send, req)
			}(msg.Request)

		case "cancel":
			var msg types.CancelMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			jobsMu.Lock()
			if cancel, ok := jobs[msg.RequestID]; ok {
				cancel()
				delete(jobs, msg.RequestID)
			}
			jobsMu.Unlock()
			c.h.CancelRequest(msg.RequestID)
		}
	}
}

// handleJob processes a single job received from the upstream router.
// It creates a correlation entry so local hub chunks are routed back here,
// pushes the job to the local queue, and streams results back upstream.
func (c *Connector) handleJob(ctx context.Context, send func(any) bool, req types.InferenceRequest) {
	ch := c.store.Create(req.ID)
	defer c.store.Delete(req.ID)

	c.q.Push(req)
	c.sched.Wake()

	for {
		select {
		case msg, ok := <-ch:
			if !ok {
				return
			}
			send(msg)
			if msg.Done {
				return
			}
		case <-ctx.Done():
			// Upstream cancelled or connection dropped; cancel locally too.
			c.h.CancelRequest(req.ID)
			return
		}
	}
}

// toWSURL converts http(s):// to ws(s)://, stripping trailing slashes.
func toWSURL(u string) string {
	u = strings.TrimRight(u, "/")
	u = strings.Replace(u, "https://", "wss://", 1)
	u = strings.Replace(u, "http://", "ws://", 1)
	return u
}

// modelInfoKey returns a canonical string representing a set of models,
// used to detect when the local model set has changed.
func modelInfoKey(models []types.ModelInfo) string {
	names := make([]string, len(models))
	for i, m := range models {
		names[i] = m.Name
	}
	sort.Strings(names)
	return strings.Join(names, ",")
}
