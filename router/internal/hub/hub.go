package hub

import (
	"encoding/json"
	"log"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Client represents a connected llm-client node.
type Client struct {
	ID            string
	conn          *websocket.Conn
	send          chan []byte
	Models        map[string]bool // nil until "register" message received
	MaxConcurrent int             // 0 until "register" message received
	inFlight      atomic.Int32
	Name          string
	Owner         string
	Token         string
}

func (c *Client) InFlight() int {
	return int(c.inFlight.Load())
}

func (c *Client) IncrInFlight() {
	c.inFlight.Add(1)
}

func (c *Client) DecrInFlight() {
	c.inFlight.Add(-1)
}

// Hub manages WebSocket client connections and acts as the client registry.
type Hub struct {
	mu       sync.RWMutex
	clients  map[string]*Client
	lastSeen map[string]time.Time // token → last disconnect time

	// OnChunk is called when a client sends a ChunkMsg.
	OnChunk func(msg types.ChunkMsg)
	// OnError is called when a client sends an ErrorMsg.
	OnError func(msg types.ErrorMsg)
	// OnAvailable is called when a client becomes available (registered or finished a job).
	OnAvailable func()
}

// New creates and returns a new Hub.
func New() *Hub {
	return &Hub{
		clients:  make(map[string]*Client),
		lastSeen: make(map[string]time.Time),
	}
}

// ServeWS upgrades an HTTP connection to WebSocket and registers the client.
// The caller should have already validated auth.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, name, owner, token string) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("hub: ws upgrade error: %v", err)
		return
	}

	client := &Client{
		ID:    uuid.New().String(),
		conn:  conn,
		send:  make(chan []byte, 64),
		Name:  name,
		Owner: owner,
		Token: token,
	}

	h.mu.Lock()
	h.clients[client.ID] = client
	h.mu.Unlock()

	log.Printf("hub: client connected: %s name=%s owner=%s", client.ID, name, owner)

	go h.writeLoop(client)
	h.readLoop(client)

	h.mu.Lock()
	delete(h.clients, client.ID)
	if token != "" {
		h.lastSeen[token] = time.Now()
	}
	h.mu.Unlock()
	close(client.send)
	log.Printf("hub: client disconnected: %s", client.ID)
	if h.OnAvailable != nil {
		h.OnAvailable()
	}
}

func (h *Hub) readLoop(client *Client) {
	defer client.conn.Close()
	for {
		_, data, err := client.conn.ReadMessage()
		if err != nil {
			return
		}
		h.dispatch(client, data)
	}
}

func (h *Hub) writeLoop(client *Client) {
	for msg := range client.send {
		if err := client.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			log.Printf("hub: write error to %s: %v", client.ID, err)
			client.conn.Close() // force readLoop to exit immediately
			return
		}
	}
}

func (h *Hub) dispatch(client *Client, data []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		log.Printf("hub: bad message from %s: %v", client.ID, err)
		return
	}

	switch envelope.Type {
	case "register":
		var msg types.RegisterMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		h.mu.Lock()
		client.Models = make(map[string]bool)
		for _, m := range msg.Models {
			client.Models[m.Name] = true
		}
		client.MaxConcurrent = msg.MaxConcurrent
		h.mu.Unlock()
		log.Printf("hub: client %s registered models=%v cap=%d", client.ID, msg.Models, msg.MaxConcurrent)
		if h.OnAvailable != nil {
			h.OnAvailable()
		}

	case "chunk":
		var msg types.ChunkMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		if msg.Done {
			client.DecrInFlight()
			if h.OnAvailable != nil {
				h.OnAvailable()
			}
		}
		if h.OnChunk != nil {
			h.OnChunk(msg)
		}

	case "error":
		var msg types.ErrorMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		client.DecrInFlight()
		if h.OnAvailable != nil {
			h.OnAvailable()
		}
		if h.OnError != nil {
			h.OnError(msg)
		}
	}
}

// SendToClient sends a JSON-encodable message to the client with the given ID.
// Returns false if the client is not connected or the send buffer is full.
func (h *Hub) SendToClient(clientID string, msg any) bool {
	h.mu.RLock()
	client, ok := h.clients[clientID]
	h.mu.RUnlock()
	if !ok {
		return false
	}
	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	select {
	case client.send <- data:
		return true
	default:
		log.Printf("hub: send buffer full for client %s", clientID)
		return false
	}
}

// FindAvailable returns the ID of a connected client that supports model and has capacity.
// Returns "" if none available.
func (h *Hub) FindAvailable(model string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for id, c := range h.clients {
		if c.Models[model] && c.InFlight() < c.MaxConcurrent {
			return id
		}
	}
	return ""
}

// AvailableModels returns the set of all models currently served by clients with available capacity.
func (h *Hub) AvailableModels() map[string]bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	models := make(map[string]bool)
	for _, c := range h.clients {
		if c.InFlight() < c.MaxConcurrent {
			for m := range c.Models {
				models[m] = true
			}
		}
	}
	return models
}

// IncrInFlight increments the in-flight counter for clientID.
// No-op if the client is not connected.
func (h *Hub) IncrInFlight(clientID string) {
	h.mu.RLock()
	client, ok := h.clients[clientID]
	h.mu.RUnlock()
	if ok {
		client.IncrInFlight()
	}
}

// DecrInFlight decrements the in-flight counter for clientID.
// No-op if the client is not connected. Used to undo a prior IncrInFlight
// when a job could not be delivered (e.g. send buffer full).
func (h *Hub) DecrInFlight(clientID string) {
	h.mu.RLock()
	client, ok := h.clients[clientID]
	h.mu.RUnlock()
	if ok {
		client.DecrInFlight()
	}
}

// IsConnected reports whether a client with the given token is currently connected.
func (h *Hub) IsConnected(token string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.Token == token {
			return true
		}
	}
	return false
}

// LastSeenTime returns the last disconnect time for token, or zero if never connected.
func (h *Hub) LastSeenTime(token string) time.Time {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.lastSeen[token]
}

// ConnectedModels returns the models advertised by the currently-connected client with token.
func (h *Hub) ConnectedModels(token string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.Token == token {
			var out []string
			for m := range c.Models {
				out = append(out, m)
			}
			return out
		}
	}
	return nil
}

// CloseByToken closes the WebSocket connection for the client with the given token.
func (h *Hub) CloseByToken(token string) {
	h.mu.RLock()
	var target *Client
	for _, c := range h.clients {
		if c.Token == token {
			target = c
			break
		}
	}
	h.mu.RUnlock()
	if target != nil {
		target.conn.Close()
	}
}

// ActiveClientCount returns the number of currently connected clients.
func (h *Hub) ActiveClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}
