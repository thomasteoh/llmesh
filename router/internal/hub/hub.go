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
	"llmesh/router/internal/latency"
)

// LeaseDuration is the maximum time a dispatched job may remain in-flight
// before the lease reaper reclaims the slot. Should be >= TTFT + activity timeouts.
// Configurable via config.yaml (timeouts.lease_minutes); default 20 min.
var LeaseDuration = 20 * time.Minute

// MaxAttempts aliases types.MaxAttempts for backward compatibility within this package.
const MaxAttempts = types.MaxAttempts

// maxReadBytes caps incoming WebSocket frame size to prevent a malicious client
// from sending a single oversized message that OOMs the router process.
const maxReadBytes = 16 << 20 // 16 MiB

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
	ID                     string
	conn                   *websocket.Conn
	send                   chan []byte
	Models                 map[string]bool // nil until "register" message received
	ModelContextSizes      map[string]int  // model name → n_ctx in tokens (0 = unknown)
	ModelContextTrainSizes map[string]int  // model name → n_ctx_train in tokens (0 = unknown)
	MaxConcurrent          int             // 0 until "register" message received
	inFlight               atomic.Int32
	Name                   string
	Owner                  string
	Token                  string         // token hash (SHA-256 hex) — the hub never holds plaintext tokens
	Version                string         // client version from register message
	OwnerSlots             map[string]int // model → slots reserved for owner; 0/unset = fully shared
	wg                     sync.WaitGroup // tracks writeLoop + readLoop goroutines
	closeOnce              sync.Once      // ensures conn.Close() happens exactly once
	sendMu                 sync.Mutex     // guards send channel close/send to prevent data race
	sendClosed             bool
}

// ClientSummary is an alias for types.ClientSummary for backward compatibility.
// Defined in pkg/types to allow the scheduler to reference it without importing hub.
type ClientSummary = types.ClientSummary

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

// closeSend closes the send channel exactly once, guarded by sendMu.
func (c *Client) closeSend() {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()
	if !c.sendClosed {
		c.sendClosed = true
		close(c.send)
	}
}

// jobLiveStats holds per-job counters updated atomically under read lock,
// avoiding write-lock contention on the hub mutex for every streamed chunk.
type jobLiveStats struct {
	firstChunkAt atomic.Pointer[time.Time]
	deltaCount   atomic.Int64
}

// InFlightRecord is a snapshot of a job currently being processed by a client.
type InFlightRecord struct {
	ClientID     string
	ClientToken  string
	ClientOwner  string // owner of the client that holds this job
	Req          types.InferenceRequest
	DispatchedAt time.Time // when the job was dispatched to this client
	LeaseExpiry  time.Time // DispatchedAt + LeaseDuration; slot reclaimed after this
	live         *jobLiveStats
}

// FirstChunkAt returns when the first non-empty delta arrived, or nil if not yet.
func (r InFlightRecord) FirstChunkAt() *time.Time {
	if r.live == nil {
		return nil
	}
	return r.live.firstChunkAt.Load()
}

// DeltaCount returns the number of non-empty deltas received (≈ tokens generated).
func (r InFlightRecord) DeltaCount() int64 {
	if r.live == nil {
		return 0
	}
	return r.live.deltaCount.Load()
}

// Hub manages WebSocket client connections and acts as the client registry.
type Hub struct {
	mu           sync.RWMutex
	clients      map[string]*Client
	lastSeen     map[string]time.Time           // token → last disconnect time
	jobs         map[string]InFlightRecord      // requestID → in-flight record
	jobsByClient map[string]map[string]struct{} // clientID → set of requestIDs
	log          *slog.Logger

	// OnChunk is called when a client sends a ChunkMsg.
	OnChunk func(msg types.ChunkMsg)
	// OnError is called when a client sends an ErrorMsg.
	OnError func(msg types.ErrorMsg)
	// OnAvailable is called when a client becomes available (registered or finished a job).
	OnAvailable func()
	// OnRelease is called when a client releases a job back to the queue.
	// The caller should push the request back to the queue and wake the scheduler.
	OnRelease func(req types.InferenceRequest)

	// Latency records per-model queue wait, TTFT, and job duration observations.
	// Optional; nil disables latency tracking.
	Latency *latency.Recorder
}

// New creates and returns a new Hub.
func New(logger *slog.Logger) *Hub {
	return &Hub{
		clients:      make(map[string]*Client),
		lastSeen:     make(map[string]time.Time),
		jobs:         make(map[string]InFlightRecord),
		jobsByClient: make(map[string]map[string]struct{}),
		log:          logger,
	}
}

// ServeWS upgrades an HTTP connection to WebSocket and registers the client.
// The caller should have already validated auth.
func (h *Hub) ServeWS(w http.ResponseWriter, r *http.Request, name, owner, token string, ownerSlots map[string]int) {
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
	conn.SetReadLimit(maxReadBytes)

	client := &Client{
		ID:         uuid.New().String(),
		conn:       conn,
		send:       make(chan []byte, 64),
		Name:       name,
		Owner:      owner,
		Token:      token,
		OwnerSlots: ownerSlots,
	}

	h.mu.Lock()
	h.clients[client.ID] = client
	h.mu.Unlock()

	h.log.Info("hub: client connected", "id", client.ID, "name", name, "owner", owner)

	client.wg.Add(2)
	go h.writeLoop(client)
	h.readLoop(client)

	client.closeSend()
	client.wg.Wait()

	// Remove the client from the registry *before* snapshotting its in-flight
	// jobs. A job dispatched concurrently by the scheduler is tracked via
	// TrackJob, which now refuses to track for a client that is no longer
	// registered (returning false so the scheduler requeues it). Removing the
	// client first therefore guarantees every still-tracked job is captured
	// here and no new job can be tracked against this dead connection.
	h.mu.Lock()
	delete(h.clients, client.ID)
	if token != "" {
		h.lastSeen[token] = time.Now()
	}
	h.mu.Unlock()

	orphaned := h.InFlightJobsByClientID(client.ID)
	h.log.Info("hub: client disconnected", "id", client.ID)

	// Immediately fail (or retry) any jobs that were in-flight when the client disconnected.
	// Without this the caller would wait for the router's activity timer (2 min).
	for _, orphan := range orphaned {
		rec, ok := h.untrackJob(orphan.Req.ID, client.ID)
		if !ok {
			continue // lease reaper or a stale message already handled this job
		}
		client.DecrInFlight()
		req := rec.Req
		req.Attempts++
		// Only retry when no output was delivered; retrying a partially-streamed
		// request would concatenate the retry onto the partial response.
		if req.Attempts < MaxAttempts && rec.FirstChunkAt() == nil && h.OnRelease != nil {
			h.log.Warn("hub: client disconnected during inference, retrying",
				"request_id", req.ID, "client_id", client.ID,
				"attempt", req.Attempts, "max_attempts", MaxAttempts)
			h.OnRelease(req)
		} else {
			h.log.Warn("hub: failing orphaned job on disconnect",
				"request_id", req.ID, "client_id", client.ID,
				"attempt", req.Attempts, "max_attempts", MaxAttempts,
				"partial", rec.FirstChunkAt() != nil)
			if h.OnError != nil {
				h.OnError(types.ErrorMsg{
					Type:      "error",
					RequestID: req.ID,
					Message:   "client disconnected during inference",
				})
			}
		}
	}

	// Drop the (now-empty) per-client job index so it does not accumulate one
	// stale entry per reconnecting worker over the process lifetime.
	h.mu.Lock()
	delete(h.jobsByClient, client.ID)
	h.mu.Unlock()

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

// inboundMsg is the union of every message type a client may send. Decoding
// once into this struct avoids a second json.Unmarshal of the full message on
// the per-token chunk hot path (previously: an envelope decode followed by a
// type-specific decode).
type inboundMsg struct {
	Type string `json:"type"`
	// register
	Models        []types.ModelInfo `json:"models"`
	MaxConcurrent int               `json:"max_concurrent"`
	Version       string            `json:"version"`
	// chunk
	RequestID      string           `json:"request_id"`
	Delta          string           `json:"delta"`
	ToolCallsDelta json.RawMessage  `json:"tool_calls_delta"`
	Done           bool             `json:"done"`
	FinishReason   string           `json:"finish_reason"`
	Usage          *types.UsageInfo `json:"usage"`
	// error
	Message string `json:"message"`
	// release
	Reason string `json:"reason"`
}

func (h *Hub) dispatch(client *Client, data []byte) {
	var in inboundMsg
	if err := json.Unmarshal(data, &in); err != nil {
		h.log.Warn("hub: bad message", "id", client.ID, "error", err)
		return
	}

	switch in.Type {
	case "register":
		msg := types.RegisterMsg{
			Type:          in.Type,
			Models:        in.Models,
			MaxConcurrent: in.MaxConcurrent,
			Version:       in.Version,
		}
		h.mu.Lock()
		client.Models = make(map[string]bool)
		client.ModelContextSizes = make(map[string]int)
		client.ModelContextTrainSizes = make(map[string]int)
		for _, m := range msg.Models {
			client.Models[m.Name] = true
			if m.ContextSize > 0 {
				client.ModelContextSizes[m.Name] = m.ContextSize
			}
			if m.ContextTrain > 0 {
				client.ModelContextTrainSizes[m.Name] = m.ContextTrain
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
		msg := types.ChunkMsg{
			Type:           in.Type,
			RequestID:      in.RequestID,
			Delta:          in.Delta,
			ToolCallsDelta: in.ToolCallsDelta,
			Done:           in.Done,
			FinishReason:   in.FinishReason,
			Usage:          in.Usage,
		}
		if msg.Delta != "" {
			h.mu.RLock()
			rec, ok := h.jobs[msg.RequestID]
			h.mu.RUnlock()
			if ok && rec.live != nil {
				rec.live.deltaCount.Add(1)
				if rec.live.firstChunkAt.Load() == nil {
					now := time.Now()
					if rec.live.firstChunkAt.CompareAndSwap(nil, &now) {
						// First non-empty token — record TTFT.
						if h.Latency != nil {
							h.Latency.RecordTTFT(rec.Req.Model, now.Sub(rec.DispatchedAt))
						}
					}
				}
			}
		}
		if msg.Done {
			if _, ok := h.untrackJob(msg.RequestID, client.ID); ok {
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
		msg := types.ErrorMsg{
			Type:      in.Type,
			RequestID: in.RequestID,
			Message:   in.Message,
		}
		rec, ok := h.untrackJob(msg.RequestID, client.ID)
		if !ok {
			// Stale error for a job this client no longer holds (already
			// completed, expired, cancelled, or re-dispatched elsewhere).
			// Dropping it avoids failing a request that is now running on
			// another client.
			return
		}
		client.DecrInFlight()
		if h.OnAvailable != nil {
			h.OnAvailable()
		}
		req := rec.Req
		req.Attempts++
		// Only retry if no output has been delivered yet. Retrying after the
		// caller has already received partial tokens would concatenate the new
		// attempt's full response onto the partial one.
		if req.Attempts < MaxAttempts && rec.FirstChunkAt() == nil && h.OnRelease != nil {
			h.log.Warn("hub: client inference error, retrying",
				"request_id", req.ID, "client_id", client.ID,
				"message", msg.Message,
				"attempt", req.Attempts, "max_attempts", MaxAttempts)
			h.OnRelease(req)
			return
		}
		if rec.FirstChunkAt() != nil {
			h.log.Warn("hub: inference error after partial output, failing without retry",
				"request_id", req.ID, "client_id", client.ID, "message", msg.Message)
		}
		if h.OnError != nil {
			h.OnError(msg)
		}

	case "release":
		rec, ok := h.untrackJob(in.RequestID, client.ID)
		if !ok {
			return // already completed, expired, reassigned, or unknown
		}
		client.DecrInFlight()
		h.log.Info("hub: client released job",
			"request_id", in.RequestID,
			"client_id", client.ID,
			"reason", in.Reason,
		)
		req := rec.Req
		req.Attempts++
		// Re-dispatch on release (e.g. graceful shutdown), but only while
		// attempts remain and no output was delivered — otherwise a client that
		// deterministically releases a job would bounce it between queue and
		// client forever, and a partial stream must not be retried.
		if req.Attempts < MaxAttempts && rec.FirstChunkAt() == nil && h.OnRelease != nil {
			h.OnRelease(req)
		} else if h.OnError != nil {
			h.OnError(types.ErrorMsg{
				Type:      "error",
				RequestID: req.ID,
				Message:   "client released job without completing it: " + in.Reason,
			})
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
	data, err := json.Marshal(msg)
	if err != nil {
		return false
	}
	client.sendMu.Lock()
	defer client.sendMu.Unlock()
	if client.sendClosed {
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
		ownerSlots := make(map[string]int, len(c.OwnerSlots))
		for k, v := range c.OwnerSlots {
			ownerSlots[k] = v
		}
		ctxSizes := make(map[string]int, len(c.ModelContextSizes))
		for k, v := range c.ModelContextSizes {
			ctxSizes[k] = v
		}
		out = append(out, ClientSummary{
			ID:                c.ID,
			Owner:             c.Owner,
			Models:            models,
			MaxConcurrent:     c.MaxConcurrent,
			InFlight:          c.InFlight(),
			ModelContextSizes: ctxSizes,
			OwnerSlots:        ownerSlots,
		})
	}
	return out
}

// AvailableSlotsByModel reports, for every model currently served by a
// connected client, how many inference slots the given owner could acquire
// right now and the caller-usable capacity ceiling. It mirrors the scheduler's
// dispatch accounting: an owner may take any free slot on its own clients,
// while a non-owner is bounded by each client's per-model OwnerSlots
// reservation. Models served only by clients exclusively reserved to other
// owners yield TotalSlots == 0 for this caller (no access).
func (h *Hub) AvailableSlotsByModel(owner string) []types.ModelSlots {
	h.mu.RLock()
	defer h.mu.RUnlock()

	type acc struct {
		available int
		total     int
		ctx       int
	}
	byModel := make(map[string]*acc)
	at := func(model string) *acc {
		a := byModel[model]
		if a == nil {
			a = &acc{}
			byModel[model] = a
		}
		return a
	}

	for _, c := range h.clients {
		if c.Models == nil {
			continue // not yet registered
		}
		free := c.MaxConcurrent - c.InFlight()
		if free < 0 {
			free = 0
		}
		for m := range c.Models {
			a := at(m)
			if c.ModelContextSizes[m] > a.ctx {
				a.ctx = c.ModelContextSizes[m]
			}
			if owner == c.Owner {
				// The client owner is not subject to its own reservation and may
				// use any free slot, whoever currently holds the others.
				a.total += c.MaxConcurrent
				a.available += free
				continue
			}
			// Non-owner: capped by MaxConcurrent minus slots reserved for the
			// client's owner on this model, then by how many non-owner jobs are
			// already running, then by the overall free-slot count.
			nonOwnerCap := c.MaxConcurrent - c.OwnerSlots[m]
			if nonOwnerCap <= 0 {
				continue // exclusively reserved for the client owner
			}
			a.total += nonOwnerCap
			used := 0
			for id := range h.jobsByClient[c.ID] {
				if rec, ok := h.jobs[id]; ok && rec.Req.Owner != c.Owner && rec.Req.Model == m {
					used++
				}
			}
			remaining := nonOwnerCap - used
			if remaining > free {
				remaining = free
			}
			if remaining > 0 {
				a.available += remaining
			}
		}
	}

	out := make([]types.ModelSlots, 0, len(byModel))
	for m, a := range byModel {
		out = append(out, types.ModelSlots{
			Model:          m,
			AvailableSlots: a.available,
			TotalSlots:     a.total,
			ContextSize:    a.ctx,
		})
	}
	return out
}

// MaxContextForModel returns the largest n_ctx reported by any connected and registered
// client serving model (or any of its aliases). Checks all clients, not just available
// ones, so a busy client with large context still counts. Returns 0 if no client is
// connected for this model or no context sizes have been reported.
func (h *Hub) MaxContextForModel(model string, aliases map[string][]string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	best := 0
	check := func(name string) {
		for _, c := range h.clients {
			if c.Models != nil && c.Models[name] && c.ModelContextSizes[name] > best {
				best = c.ModelContextSizes[name]
			}
		}
	}
	check(model)
	for _, target := range aliases[model] {
		check(target)
	}
	return best
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
// When multiple clients serve the same model, the largest reported sizes win.
func (h *Hub) ActiveModelInfos() []types.ModelInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	ctxSizes := make(map[string]int)
	ctxTrain := make(map[string]int)
	for _, c := range h.clients {
		for m := range c.Models {
			if _, ok := ctxSizes[m]; !ok {
				ctxSizes[m] = 0
				ctxTrain[m] = 0
			}
			if c.ModelContextSizes[m] > ctxSizes[m] {
				ctxSizes[m] = c.ModelContextSizes[m]
			}
			if c.ModelContextTrainSizes[m] > ctxTrain[m] {
				ctxTrain[m] = c.ModelContextTrainSizes[m]
			}
		}
	}
	out := make([]types.ModelInfo, 0, len(ctxSizes))
	for m := range ctxSizes {
		out = append(out, types.ModelInfo{Name: m, ContextSize: ctxSizes[m], ContextTrain: ctxTrain[m]})
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

// TrackJob registers an in-flight job for the given client. Called by the
// scheduler after dispatch. Returns false if the client is no longer connected
// — in that case the job is not tracked and the caller must requeue it, so a
// job dispatched into a connection that drops concurrently is never lost.
func (h *Hub) TrackJob(clientID string, req types.InferenceRequest) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	c, ok := h.clients[clientID]
	if !ok {
		return false // connection dropped between dispatch and tracking
	}
	token := c.Token
	clientOwner := c.Owner
	now := time.Now()
	h.jobs[req.ID] = InFlightRecord{
		ClientID:     clientID,
		ClientToken:  token,
		ClientOwner:  clientOwner,
		Req:          req,
		DispatchedAt: now,
		LeaseExpiry:  now.Add(LeaseDuration),
		live:         &jobLiveStats{},
	}
	// Record time from enqueue to dispatch (queue wait latency).
	if h.Latency != nil {
		h.Latency.RecordQueueWait(req.Model, now.Sub(req.EnqueuedAt))
	}
	if _, ok := h.jobsByClient[clientID]; !ok {
		h.jobsByClient[clientID] = make(map[string]struct{})
	}
	h.jobsByClient[clientID][req.ID] = struct{}{}
	return true
}

// untrackJob removes the job record for requestID, but only if it is currently
// held by clientID. Returns the removed record and true when the record existed
// and belonged to clientID; otherwise a zero record and false. Verifying the
// holder prevents a stale message from a previous attempt (after the request was
// re-dispatched to another client with the same ID) from untracking the new
// attempt's record and corrupting in-flight accounting. Callers must only
// DecrInFlight when this returns true.
func (h *Hub) untrackJob(requestID, clientID string) (InFlightRecord, bool) {
	h.mu.Lock()
	rec, existed := h.jobs[requestID]
	if !existed || rec.ClientID != clientID {
		h.mu.Unlock()
		return InFlightRecord{}, false
	}
	delete(h.jobs, requestID)
	delete(h.jobsByClient[rec.ClientID], requestID)
	h.mu.Unlock()
	if h.Latency != nil {
		h.Latency.RecordDuration(rec.Req.Model, time.Since(rec.DispatchedAt))
	}
	return rec, true
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
	reqIDs := h.jobsByClient[clientID]
	out := make([]InFlightRecord, 0, len(reqIDs))
	for id := range reqIDs {
		if rec, ok := h.jobs[id]; ok {
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

// CancelRequest untracks the in-flight job and sends a cancel message to the
// llmesh-client holding it. The slot is freed immediately rather than waiting
// for the 20-minute lease — this is the correct behaviour when the HTTP client
// has already given up (timeout or disconnect).
//
// If the request is not currently in-flight (already completed, expired, or
// never dispatched) the cancel message is still broadcast so any stale
// processing is stopped, but there is nothing to untrack.
func (h *Hub) CancelRequest(requestID string) {
	// Atomically remove the job record so we get the clientID for the decrement.
	h.mu.Lock()
	rec, existed := h.jobs[requestID]
	if existed {
		delete(h.jobs, requestID)
		delete(h.jobsByClient[rec.ClientID], requestID)
	}
	h.mu.Unlock()

	if existed {
		// Free the client slot immediately.
		h.mu.RLock()
		client, ok := h.clients[rec.ClientID]
		h.mu.RUnlock()
		if ok {
			client.DecrInFlight()
		}
		h.log.Info("hub: cancel freed slot", "request_id", requestID, "client_id", rec.ClientID)
		if h.OnAvailable != nil {
			h.OnAvailable()
		}
	}

	// Broadcast cancel so the llmesh-client stops processing (saves compute).
	// Clients not holding this request ID silently ignore it.
	msg := types.CancelMsg{Type: "cancel", RequestID: requestID}
	data, err := json.Marshal(msg)
	if err != nil {
		return
	}
	h.mu.RLock()
	for _, c := range h.clients {
		c.sendMu.Lock()
		if !c.sendClosed {
			select {
			case c.send <- data:
			default:
			}
		}
		c.sendMu.Unlock()
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

// TriggerClientUpdate sends an update request to all connected clients with the given token.
// Returns the number of clients the message was delivered to.
func (h *Hub) TriggerClientUpdate(token string) int {
	msg := types.UpdateMsg{Type: "update"}
	data, err := json.Marshal(msg)
	if err != nil {
		return 0
	}
	h.mu.RLock()
	var targets []*Client
	for _, c := range h.clients {
		if c.Token == token {
			targets = append(targets, c)
		}
	}
	h.mu.RUnlock()
	sent := 0
	for _, c := range targets {
		c.sendMu.Lock()
		if !c.sendClosed {
			select {
			case c.send <- data:
				sent++
			default:
				h.log.Warn("hub: send buffer full, dropping update message", "client_id", c.ID)
			}
		}
		c.sendMu.Unlock()
	}
	return sent
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

// SetClientOwnerSlots updates the OwnerSlots map for the given model on all currently
// connected clients that authenticated with the given token. Takes effect immediately
// for subsequent scheduler cycles; in-flight jobs are not affected.
// slots <= 0 removes the model key (restores full sharing for that model).
func (h *Hub) SetClientOwnerSlots(token, model string, slots int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, c := range h.clients {
		if c.Token == token {
			if c.OwnerSlots == nil {
				c.OwnerSlots = make(map[string]int)
			}
			if slots <= 0 {
				delete(c.OwnerSlots, model)
			} else {
				c.OwnerSlots[model] = slots
			}
		}
	}
}

// NonOwnerInFlight returns the number of in-flight jobs on clientID for the given
// model whose request owner differs from owner. Used by the scheduler to enforce
// per-model OwnerSlots limits.
func (h *Hub) NonOwnerInFlight(clientID, owner, model string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for id := range h.jobsByClient[clientID] {
		if rec, ok := h.jobs[id]; ok && rec.Req.Owner != owner && rec.Req.Model == model {
			n++
		}
	}
	return n
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

// HasWorkerForModel reports whether any connected and registered client currently
// serves model (direct name, alias resolution, or the "any" pseudo-model).
func (h *Hub) HasWorkerForModel(model string, aliases map[string][]string) bool {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for _, c := range h.clients {
		if c.Models == nil {
			continue // not yet registered
		}
		if model == "any" {
			return true
		}
		if c.Models[model] {
			return true
		}
		for _, target := range aliases[model] {
			if c.Models[target] {
				return true
			}
		}
	}
	return false
}

// OwnerInFlight returns the number of jobs currently in flight whose
// request owner matches owner.
func (h *Hub) OwnerInFlight(owner string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	n := 0
	for _, rec := range h.jobs {
		if rec.Req.Owner == owner {
			n++
		}
	}
	return n
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
			delete(h.jobsByClient[rec.ClientID], id)
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
	ID                     string
	Name                   string
	Version                string
	Models                 []string
	ModelContextSizes      map[string]int
	ModelContextTrainSizes map[string]int
	MaxConcurrent          int
	InFlight               int
	OwnerSlots             map[string]int // model → slots reserved for owner; 0/unset = fully shared
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
			ownerSlots := make(map[string]int, len(c.OwnerSlots))
			for k, v := range c.OwnerSlots {
				ownerSlots[k] = v
			}
			out = append(out, ConnectedClientInfo{
				ID:                     c.ID,
				Name:                   c.Name,
				Version:                c.Version,
				Models:                 models,
				ModelContextSizes:      c.ModelContextSizes,
				ModelContextTrainSizes: c.ModelContextTrainSizes,
				MaxConcurrent:          c.MaxConcurrent,
				InFlight:               c.InFlight(),
				OwnerSlots:             ownerSlots,
			})
		}
	}
	return out
}
