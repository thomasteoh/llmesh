package hub

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"llmesh/pkg/types"
)

// LeaseDuration is the maximum time a dispatched job may remain in-flight
// before the lease reaper reclaims the slot. Matches the worst-case HTTP handler
// timeout: 15 min TTFT + 5 min activity = 20 min.
const LeaseDuration = 20 * time.Minute

// MaxAttempts is the total number of times a request may be dispatched to a
// client before being failed back to the caller. Counts the initial attempt
// plus any retries triggered by client errors or disconnects.
const MaxAttempts = 3

// isValidOrigin validates the Origin header against the Host header.
// It allows empty origin (non-browser clients) and host-based matching
// (scheme+host from Origin must match Host).
func isValidOrigin(origin, host string) bool {
	if origin == "" {
		return true // non-browser clients (curl, SDK) may not send Origin
	}
	u, err := parseOrigin(origin)
	if err != nil {
		return false // malformed origin
	}
	return u.Host == host
}

// parseOrigin extracts scheme and host from an origin string.
func parseOrigin(origin string) (schemeHost, error) {
	const sep = "://"
	idx := indexStr(origin, sep)
	if idx < 0 {
		return schemeHost{}, errMalformedOrigin
	}
	scheme := origin[:idx]
	rest := origin[idx+len(sep):]
	if scheme == "" || rest == "" {
		return schemeHost{}, errMalformedOrigin
	}
	return schemeHost{Scheme: scheme, Host: rest}, nil
}

type schemeHost struct {
	Scheme string
	Host   string
}

var errMalformedOrigin = fmt.Errorf("malformed origin")

func indexStr(s, substr string) int {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return i
		}
	}
	return -1
}

// Client represents a connected llmesh-client node.
type Client struct {
	ID                string
	conn              *websocket.Conn
	send              chan []byte
	Models            map[string]bool // nil until "register" message received
	ModelContextSizes map[string]int  // model name → context size in tokens (0 = unknown)
	MaxConcurrent     int             // 0 until "register" message received
	inFlight          atomic.Int32
	Name             string
	Owner            string
	Token            string
	Version          string // client version from register message
	ExclusiveOwner   bool   // if true, only dispatch jobs from this client's owner
	wg               sync.WaitGroup // tracks writeLoop + readLoop goroutines
	closeOnce        sync.Once      // ensures conn.Close() happens exactly once
}

// ClientSummary is a snapshot of an available client used by the scheduler.
type ClientSummary struct {
	ID             string
	Owner          string
	Models         map[string]bool
	ExclusiveOwner bool // if true, only dispatch jobs from this client's owner
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

// close safely closes the WebSocket connection exactly once.
func (c *Client) close() {
	c.closeOnce.Do(func() { c.conn.Close() })
}

// InFlightRecord is a snapshot of a job currently being processed by a client.
type InFlightRecord struct {
	ClientID     string
	ClientToken  string
	Req          types.InferenceRequest
	DispatchedAt time.Time // when the job was dispatched to this client
	LeaseExpiry  time.Time // DispatchedAt + LeaseDuration; slot reclaimed after this
}

// Hub manages WebSocket client connections and acts as the client registry.
type Hub struct {
	mu       sync.RWMutex
	clients  map[string]*Client
	lastSeen map[string]time.Time      // token → last disconnect time
	jobs     map[string]InFlightRecord // requestID → in-flight record
	log      *slog.Logger

	// OnChunk is called when a client sends a ChunkMsg.
	OnChunk func(msg types.ChunkMsg)
	// OnError is called when a client sends an ErrorMsg.
	OnError func(msg types.ErrorMsg)
	// OnAvailable is called when a client becomes available (registered or finished a job).
	OnAvailable func()
	// OnRelease is called when a client releases a job back to the queue.
	// The caller should push the request back to the queue and wake the scheduler.
	OnRelease func(req types.InferenceRequest)
}

// New creates and returns a new Hub.
func New(logger *slog.Logger) *Hub {
	return &Hub{
		clients:  make(map[string]*Client),
		lastSeen: make(map[string]time.Time),
		jobs:     make(map[string]InFlightRecord),
		log:      logger,
	}
}

// ServeWS upgrades an HTTP connection to WebSocket and registers the client.
// The caller should have already validated auth.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, name, owner, token string, exclusiveOwner bool) {
	origin := r.Header.Get("Origin")
	if !isValidOrigin(origin, r.Host) {
		h.log.Warn("hub: ws origin rejected", "origin", origin, "host", r.Host)
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return
	}
	conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
	if err != nil {
		h.log.Error("hub: ws upgrade error", "error", err)
		return
	}

	client := &Client{
		ID:             uuid.New().String(),
		conn:           conn,
		send:           make(chan []byte, 64),
		Name:           name,
		Owner:          owner,
		Token:          token,
		ExclusiveOwner: exclusiveOwner,
	}

	h.mu.Lock()
	h.clients[client.ID] = client
	h.mu.Unlock()

	h.log.Info("hub: client connected", "id", client.ID, "name", name, "owner", owner)

	client.wg.Add(2)
	go h.writeLoop(client)
	h.readLoop(client)

	close(client.send)
	client.wg.Wait()

	// Collect in-flight jobs before removing the client so we can fail them immediately.
	// dispatch() only runs from within readLoop (which has already returned), so there
	// is no race between this cleanup and job tracking.
	orphaned := h.InFlightJobsByClientID(client.ID)

	h.mu.Lock()
	delete(h.clients, client.ID)
	if token != "" {
		h.lastSeen[token] = time.Now()
	}
	h.mu.Unlock()
	h.log.Info("hub: client disconnected", "id", client.ID)

	// Immediately fail (or retry) any jobs that were in-flight when the client disconnected.
	// Without this the caller would wait for the router's activity timer (2 min).
	for _, rec := range orphaned {
		if !h.untrackJob(rec.Req.ID) {
			continue // lease reaper already handled this job
		}
		client.DecrInFlight()
		req := rec.Req
		req.Attempts++
		if req.Attempts < MaxAttempts && h.OnRelease != nil {
			h.log.Warn("hub: client disconnected during inference, retrying",
				"request_id", req.ID, "client_id", client.ID,
				"attempt", req.Attempts, "max_attempts", MaxAttempts)
			h.OnRelease(req)
		} else {
			h.log.Warn("hub: failing orphaned job on disconnect",
				"request_id", req.ID, "client_id", client.ID,
				"attempt", req.Attempts, "max_attempts", MaxAttempts)
			if h.OnError != nil {
				h.OnError(types.ErrorMsg{
					Type:      "error",
					RequestID: req.ID,
					Message:   "client disconnected during inference",
				})
			}
		}
	}

	// OnAvailable signals that this client's slots are free; the scheduler
	// will also be woken by any OnRelease calls above (via sched.Wake), but
	// this covers the case where all orphaned jobs were already expired.
	if h.OnAvailable != nil {
		h.OnAvailable()
	}
}

func (h *Hub) readLoop(client *Client) {
	defer client.wg.Done()
	defer client.close()
	for {
		_, data, err := client.conn.ReadMessage()
		if err != nil {
			return
		}
		h.dispatch(client, data)
	}
}

func (h *Hub) writeLoop(client *Client) {
	defer client.wg.Done()
	for msg := range client.send {
		if err := client.conn.WriteMessage(websocket.TextMessage, msg); err != nil {
			h.log.Error("hub: write error", "id", client.ID, "error", err)
			client.close() // force readLoop to exit immediately
			return
		}
	}
}

func (h *Hub) dispatch(client *Client, data []byte) {
	var envelope struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &envelope); err != nil {
		h.log.Warn("hub: bad message", "id", client.ID, "error", err)
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
		client.ModelContextSizes = make(map[string]int)
		for _, m := range msg.Models {
			client.Models[m.Name] = true
			if m.ContextSize > 0 {
				client.ModelContextSizes[m.Name] = m.ContextSize
			}
		}
		client.MaxConcurrent = msg.MaxConcurrent
		client.Version = msg.Version
		h.mu.Unlock()
		h.log.Info("hub: client registered", "id", client.ID, "models", msg.Models, "max_concurrent", msg.MaxConcurrent, "version", msg.Version)
		if h.OnAvailable != nil {
			h.OnAvailable()
		}

	case "chunk":
		var msg types.ChunkMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		if msg.Done {
			if h.untrackJob(msg.RequestID) {
				client.DecrInFlight()
				if h.OnAvailable != nil {
					h.OnAvailable()
				}
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
		h.mu.RLock()
		rec, hasRec := h.jobs[msg.RequestID]
		h.mu.RUnlock()
		if h.untrackJob(msg.RequestID) {
			client.DecrInFlight()
			if h.OnAvailable != nil {
				h.OnAvailable()
			}
		}
		if hasRec {
			req := rec.Req
			req.Attempts++
			if req.Attempts < MaxAttempts && h.OnRelease != nil {
				h.log.Warn("hub: client inference error, retrying",
					"request_id", req.ID, "client_id", client.ID,
					"message", msg.Message,
					"attempt", req.Attempts, "max_attempts", MaxAttempts)
				h.OnRelease(req)
				return
			}
		}
		if h.OnError != nil {
			h.OnError(msg)
		}

	case "release":
		var msg types.ReleaseMsg
		if err := json.Unmarshal(data, &msg); err != nil {
			return
		}
		h.mu.RLock()
		rec, ok := h.jobs[msg.RequestID]
		h.mu.RUnlock()
		if !ok {
			return // already completed, expired, or unknown
		}
		client.DecrInFlight()
		h.untrackJob(msg.RequestID)
		h.log.Info("hub: client released job",
			"request_id", msg.RequestID,
			"client_id", client.ID,
			"reason", msg.Reason,
		)
		if h.OnRelease != nil {
			h.OnRelease(rec.Req)
		}
		if h.OnAvailable != nil {
			h.OnAvailable()
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
	if client.send == nil {
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
		h.log.Warn("hub: send buffer full", "client_id", clientID)
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

// AvailableClientList returns a snapshot of clients that have spare capacity.
// The returned Models map is safe to read without holding the hub lock.
func (h *Hub) AvailableClientList() []ClientSummary {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []ClientSummary
	for _, c := range h.clients {
		if c.Models == nil || c.InFlight() >= c.MaxConcurrent {
			continue
		}
		models := make(map[string]bool, len(c.Models))
		for k, v := range c.Models {
			models[k] = v
		}
		out = append(out, ClientSummary{ID: c.ID, Owner: c.Owner, Models: models, ExclusiveOwner: c.ExclusiveOwner})
	}
	return out
}

// ActiveModels returns all model names currently advertised by connected clients.
func (h *Hub) ActiveModels() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]bool)
	for _, c := range h.clients {
		for m := range c.Models {
			seen[m] = true
		}
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	return out
}

// ActiveModelInfos returns ModelInfo for all models advertised by connected clients.
// When multiple clients serve the same model, the largest reported context size wins.
func (h *Hub) ActiveModelInfos() []types.ModelInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]int) // model → best context size
	for _, c := range h.clients {
		for m := range c.Models {
			if existing, ok := seen[m]; !ok || c.ModelContextSizes[m] > existing {
				seen[m] = c.ModelContextSizes[m]
			}
		}
	}
	out := make([]types.ModelInfo, 0, len(seen))
	for m, ctxSize := range seen {
		out = append(out, types.ModelInfo{Name: m, ContextSize: ctxSize})
	}
	return out
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

// TrackJob registers an in-flight job for the given client. Called by the scheduler after dispatch.
func (h *Hub) TrackJob(clientID string, req types.InferenceRequest) {
	h.mu.Lock()
	defer h.mu.Unlock()
	token := ""
	if c, ok := h.clients[clientID]; ok {
		token = c.Token
	}
	now := time.Now()
	h.jobs[req.ID] = InFlightRecord{
		ClientID:     clientID,
		ClientToken:  token,
		Req:          req,
		DispatchedAt: now,
		LeaseExpiry:  now.Add(LeaseDuration),
	}
}

// untrackJob removes the job record for requestID. Returns true if the record
// existed (and was deleted), false if it was already gone (e.g. lease reaper
// beat us to it). Callers must only DecrInFlight when this returns true.
func (h *Hub) untrackJob(requestID string) bool {
	h.mu.Lock()
	_, existed := h.jobs[requestID]
	delete(h.jobs, requestID)
	h.mu.Unlock()
	return existed
}

// LookupInFlightJob returns the in-flight record for requestID, if any.
func (h *Hub) LookupInFlightJob(requestID string) (InFlightRecord, bool) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	rec, ok := h.jobs[requestID]
	return rec, ok
}

// InFlightJobsByClientID returns all currently tracked jobs for the given client connection.
func (h *Hub) InFlightJobsByClientID(clientID string) []InFlightRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []InFlightRecord
	for _, rec := range h.jobs {
		if rec.ClientID == clientID {
			out = append(out, rec)
		}
	}
	return out
}

// AllInFlightJobs returns a snapshot of all currently tracked in-flight jobs.
func (h *Hub) AllInFlightJobs() []InFlightRecord {
	h.mu.RLock()
	defer h.mu.RUnlock()
	out := make([]InFlightRecord, 0, len(h.jobs))
	for _, rec := range h.jobs {
		out = append(out, rec)
	}
	return out
}

// CancelRequest broadcasts a cancel message to all connected clients for the given requestID.
// Clients not currently processing that request silently ignore it.
func (h *Hub) CancelRequest(requestID string) {
	msg := types.CancelMsg{Type: "cancel", RequestID: requestID}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	for _, c := range h.clients {
		if c.send == nil {
			continue
		}
		select {
		case c.send <- data:
		default:
		}
	}
	h.mu.RUnlock()
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

// ConnectedModels returns the union of models advertised by all connected clients with token.
func (h *Hub) ConnectedModels(token string) []string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	seen := make(map[string]bool)
	for _, c := range h.clients {
		if c.Token == token {
			for m := range c.Models {
				seen[m] = true
			}
		}
	}
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for m := range seen {
		out = append(out, m)
	}
	return out
}

// ConnectedVersion returns the version string from any connected client with token.
// If multiple clients report different versions, returns "mixed".
func (h *Hub) ConnectedVersion(token string) string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var version string
	for _, c := range h.clients {
		if c.Token == token {
			if version == "" {
				version = c.Version
			} else if version != c.Version {
				return "mixed"
			}
		}
	}
	return version
}

// CloseByToken closes ALL WebSocket connections for clients with the given token.
func (h *Hub) CloseByToken(token string) {
	h.mu.RLock()
	var targets []*Client
	for _, c := range h.clients {
		if c.Token == token {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range targets {
		c.close()
	}
}

// SetClientExclusive updates the ExclusiveOwner flag on all currently connected
// clients that authenticated with the given token. Takes effect immediately for
// subsequent scheduler cycles; in-flight jobs are not affected.
func (h *Hub) SetClientExclusive(token string, exclusive bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.Token == token {
			c.ExclusiveOwner = exclusive
		}
	}
}

// TotalSlots returns the sum of MaxConcurrent across all registered clients.
// Used by the upstream connector to advertise aggregate capacity.
func (h *Hub) TotalSlots() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	total := 0
	for _, c := range h.clients {
		if c.Models != nil {
			total += c.MaxConcurrent
		}
	}
	return total
}

// ActiveClientCount returns the number of currently connected clients.
func (h *Hub) ActiveClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// handleExpiredLeases scans all tracked jobs and reclaims slots for any whose
// LeaseExpiry has passed. Called by the lease reaper goroutine; also exposed for
// testing so tests can trigger it directly without waiting for the ticker.
func (h *Hub) handleExpiredLeases() {
	now := time.Now()

	h.mu.Lock()
	var expired []InFlightRecord
	for id, rec := range h.jobs {
		if rec.LeaseExpiry.Before(now) {
			expired = append(expired, rec)
			delete(h.jobs, id)
		}
	}
	h.mu.Unlock()

	if len(expired) == 0 {
		return
	}

	for _, rec := range expired {
		h.log.Warn("hub: lease expired, reclaiming slot",
			"request_id", rec.Req.ID,
			"client_id", rec.ClientID,
			"dispatched_at", rec.DispatchedAt,
		)
		h.DecrInFlight(rec.ClientID)
		// Cancel the job on the client (it may still be processing).
		h.SendToClient(rec.ClientID, types.CancelMsg{
			Type:      "cancel",
			RequestID: rec.Req.ID,
		})
	}

	if h.OnAvailable != nil {
		h.OnAvailable()
	}
}

// StartLeaseReaper starts a background goroutine that calls handleExpiredLeases
// every 30 seconds. It runs until the process exits.
func (h *Hub) StartLeaseReaper() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			h.handleExpiredLeases()
		}
	}()
}

// ConnectedCountByToken returns the number of currently connected clients with the given token.
func (h *Hub) ConnectedCountByToken(token string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	count := 0
	for _, c := range h.clients {
		if c.Token == token {
			count++
		}
	}
	return count
}

// ConnectedClientInfo holds a snapshot of a connected client for display.
type ConnectedClientInfo struct {
	ID              string
	Name            string
	Version         string
	Models          []string
	ModelContextSizes map[string]int
	MaxConcurrent   int
	InFlight        int
}

// ConnectedClientsByToken returns a snapshot of all currently connected clients
// that authenticated with the given token.
func (h *Hub) ConnectedClientsByToken(token string) []ConnectedClientInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	var out []ConnectedClientInfo
	for _, c := range h.clients {
		if c.Token == token {
			var models []string
			for m := range c.Models {
				models = append(models, m)
			}
			sort.Strings(models)
			out = append(out, ConnectedClientInfo{
				ID:                c.ID,
				Name:              c.Name,
				Version:           c.Version,
				Models:            models,
				ModelContextSizes: c.ModelContextSizes,
				MaxConcurrent:     c.MaxConcurrent,
				InFlight:          c.InFlight(),
			})
		}
	}
	return out
}
