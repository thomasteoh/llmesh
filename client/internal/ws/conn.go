package ws

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/client/internal/worker"
	"llmesh/pkg/types"
)

const (
	pingInterval = 30 * time.Second
	pongWait     = 60 * time.Second
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Conn manages the WebSocket connection from client to router with reconnection.
type Conn struct {
	cfg       *clientPkg.Config
	version   string
	sem       chan struct{} // limits concurrent jobs
	mu        sync.Mutex
	ws        *websocket.Conn
	cancelsMu sync.Mutex
	cancels   map[string]context.CancelFunc // requestID → cancel for in-flight jobs
}

func New(cfg *clientPkg.Config, version string) *Conn {
	return &Conn{
		cfg:     cfg,
		version: version,
		sem:     make(chan struct{}, cfg.MaxConcurrent),
		cancels: make(map[string]context.CancelFunc),
	}
}

// Run connects to the router and reconnects on disconnect. Blocks forever.
func (c *Conn) Run() {
	backoff := time.Second
	for {
		err := c.connect()
		if err != nil {
			log.Error("ws: connect error", "error", err, "backoff", backoff.String())
			time.Sleep(backoff)
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		log.Info("ws: disconnected — reconnecting")
	}
}

func (c *Conn) connect() error {
	header := map[string][]string{
		"Authorization": {"Bearer " + c.cfg.RouterToken},
	}
	conn, _, err := websocket.DefaultDialer.Dial(c.cfg.RouterURL, header)
	if err != nil {
		return err
	}

	// Create a context cancelled when this connection closes
	ctx, cancel := context.WithCancel(context.Background())

	c.mu.Lock()
	c.ws = conn
	c.mu.Unlock()

	defer func() {
		cancel()
		conn.Close()
		c.mu.Lock()
		c.ws = nil
		c.mu.Unlock()
	}()

	// Keepalive: refresh read deadline on every pong
	conn.SetReadDeadline(time.Now().Add(pongWait))
	conn.SetPongHandler(func(string) error {
		conn.SetReadDeadline(time.Now().Add(pongWait))
		return nil
	})

	// Ping goroutine
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
			case <-ctx.Done():
				return
			}
		}
	}()

	// Register with router — probe context size for each model first.
	models := make([]types.ModelInfo, 0, len(c.cfg.Models))
	for _, m := range c.cfg.Models {
		lc := llamacpp.New(c.cfg.EndpointFor(m.Name))
		ctxSize := lc.ProbeContextSize(ctx)
		models = append(models, types.ModelInfo{Name: m.Name, ContextSize: ctxSize})
		if ctxSize > 0 {
			log.Info("ws: model context_size", "model", m.Name, "context_size", ctxSize)
		}
	}
	if err := c.send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: c.cfg.MaxConcurrent,
		Version:       c.version,
	}); err != nil {
		return err
	}
	log.Info("ws: registered with router", "models", models, "max_concurrent", c.cfg.MaxConcurrent)

	// Read loop
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
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
			c.sem <- struct{}{} // acquire slot
			jobCtx, jobCancel := context.WithCancel(ctx)
			c.cancelsMu.Lock()
			c.cancels[job.Request.ID] = jobCancel
			c.cancelsMu.Unlock()
			go func(j types.JobMsg, jCtx context.Context, jCancel context.CancelFunc) {
				defer func() {
					<-c.sem
					c.cancelsMu.Lock()
					delete(c.cancels, j.Request.ID)
					c.cancelsMu.Unlock()
					jCancel()
				}()
				worker.Handle(jCtx, j, c.cfg, c.send)
			}(job, jobCtx, jobCancel)
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
