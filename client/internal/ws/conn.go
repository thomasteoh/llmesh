package ws

import (
	"context"
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	clientPkg "llmesh/client"
	"llmesh/client/internal/worker"
	"llmesh/pkg/types"
)

// Conn manages the WebSocket connection from client to router with reconnection.
type Conn struct {
	cfg *clientPkg.Config
	sem chan struct{} // limits concurrent jobs
	mu  sync.Mutex
	ws  *websocket.Conn
}

func New(cfg *clientPkg.Config) *Conn {
	return &Conn{
		cfg: cfg,
		sem: make(chan struct{}, cfg.MaxConcurrent),
	}
}

// Run connects to the router and reconnects on disconnect. Blocks forever.
func (c *Conn) Run() {
	backoff := time.Second
	for {
		err := c.connect()
		if err != nil {
			log.Printf("ws: connect error: %v — retrying in %s", err, backoff)
			time.Sleep(backoff)
			if backoff < 60*time.Second {
				backoff *= 2
			}
			continue
		}
		backoff = time.Second
		log.Printf("ws: disconnected — reconnecting")
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

	// Register with router
	models := make([]types.ModelInfo, 0, len(c.cfg.Models))
	for _, m := range c.cfg.Models {
		models = append(models, types.ModelInfo{Name: m.Name})
	}
	if err := c.send(types.RegisterMsg{
		Type:          "register",
		Models:        models,
		MaxConcurrent: c.cfg.MaxConcurrent,
	}); err != nil {
		return err
	}
	log.Printf("ws: registered with router, models=%v", c.cfg.Models)

	// Read loop
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		var env struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		if env.Type != "job" {
			continue
		}
		var job types.JobMsg
		if err := json.Unmarshal(data, &job); err != nil {
			log.Printf("ws: bad job message: %v", err)
			continue
		}
		c.sem <- struct{}{} // acquire slot
		go func(j types.JobMsg) {
			defer func() { <-c.sem }()
			worker.Handle(ctx, j, c.cfg, c.send)
		}(job)
	}
}

func (c *Conn) send(msg any) error {
	data, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.mu.Lock()
	ws := c.ws
	c.mu.Unlock()
	if ws == nil {
		return nil
	}
	return ws.WriteMessage(websocket.TextMessage, data)
}
