package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	shimPkg "llmesh/shim"
	"llmesh/shim/internal/backend"
	"llmesh/shim/internal/stats"
	"llmesh/shim/internal/worker"
	"llmesh/pkg/types"
)

const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Conn manages the WebSocket connection from shim to router with automatic reconnection.
type Conn struct {
	cfg       *shimPkg.Config
	version   string
	st        *stats.Stats
	sem       chan struct{}
	mu        sync.Mutex
	ws        *websocket.Conn
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc
}

func New(cfg *shimPkg.Config, version string, st *stats.Stats) *Conn {
	return &Conn{
		cfg:     cfg,
		version: version,
		st:      st,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		cancels: make(map[string]context.CancelFunc),
	}
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
			c.st.Reconnects.Add(1)
		}
		first = false
		err := c.connect(ctx)
		c.st.SetConnected(false)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			log.Error("ws: connect error", "error", err, "backoff", backoff.String())
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
		log.Info("ws: disconnected — reconnecting")
	}
}

func (c *Conn) connect(outerCtx context.Context) error {
	header := map[string][]string{
		"Authorization": {"Bearer " + c.cfg.RouterToken},
	}
	conn, _, err := websocket.DefaultDialer.Dial(c.cfg.RouterURL, header)
	if err != nil {
		return err
	}

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

	go func() {
		select {
		case <-outerCtx.Done():
			log.Info("ws: shutdown signal received, notifying router of in-flight jobs")
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

	// Register with router using model names and context sizes from config.
	// No probing needed — context sizes are declared in config (unlike the llmesh-client
	// which probes llama.cpp's /props endpoint).
	models := make([]types.ModelInfo, 0, len(c.cfg.Models))
	for _, m := range c.cfg.Models {
		models = append(models, types.ModelInfo{Name: m.Name, ContextSize: m.ContextSize})
	}
	if err := c.send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: c.cfg.MaxConcurrent,
		Version:       c.version,
	}); err != nil {
		return err
	}
	c.st.SetConnected(true)
	log.Info("ws: registered with router", "models", models, "max_concurrent", c.cfg.MaxConcurrent)

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			if outerCtx.Err() != nil {
				return nil
			}
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
			var job types.JobMsg
			if err := json.Unmarshal(data, &job); err != nil {
				log.Warn("ws: bad job message", "error", err)
				continue
			}
			bc := c.cfg.BackendFor(job.Request.Model)
			if bc == nil {
				log.Warn("ws: no backend configured for model", "model", job.Request.Model)
				_ = c.send(types.ErrorMsg{
					Type:      "error",
					RequestID: job.Request.ID,
					Message:   "model not available on this shim",
				})
				continue
			}
			spec := specFromBackend(bc)
			// Acquire a concurrency slot. Use select so a shutdown (connCtx.Done)
			// can interrupt the wait and allow cancel messages to be processed.
			select {
			case c.sem <- struct{}{}:
			case <-connCtx.Done():
				continue
			}
			jobCtx, jobCancel := context.WithCancel(connCtx)
			c.cancelsMu.Lock()
			c.cancels[job.Request.ID] = jobCancel
			c.cancelsMu.Unlock()
			c.st.ActiveJobs.Add(1)
			go func(j types.JobMsg, sp *backend.Spec, jCtx context.Context, jCancel context.CancelFunc) {
				defer func() {
					c.st.ActiveJobs.Add(-1)
					c.st.TotalDone.Add(1)
					<-c.sem
					c.cancelsMu.Lock()
					delete(c.cancels, j.Request.ID)
					c.cancelsMu.Unlock()
					jCancel()
				}()
				if err := worker.Handle(jCtx, j, sp, c.send, c.st); err != nil {
					c.st.TotalErrors.Add(1)
				}
			}(job, spec, jobCtx, jobCancel)
		case "cancel":
			var msg types.CancelMsg
			if err := json.Unmarshal(data, &msg); err != nil {
				continue
			}
			c.cancelsMu.Lock()
			if cancel, ok := c.cancels[msg.RequestID]; ok {
				log.Info("ws: cancelling job", "request_id", msg.RequestID)
				cancel()
				delete(c.cancels, msg.RequestID)
			}
			c.cancelsMu.Unlock()
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

// specFromBackend converts a config BackendConfig to a backend.Spec.
func specFromBackend(bc *shimPkg.BackendConfig) *backend.Spec {
	return &backend.Spec{
		Type:       bc.Type,
		URL:        bc.URL,
		Format:     bc.Format,
		AuthType:   bc.AuthType,
		AuthHeader: bc.AuthHeader,
		AuthValue:  bc.AuthValue,
		Command:    bc.Command,
	}
}
