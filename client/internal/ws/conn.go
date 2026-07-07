// client/internal/ws/conn.go
//
// Thin client-specific wrapper around pkg/wsclient.Conn.
// Provides the model list (via llama.cpp context-size probing) and
// job dispatcher (via client/internal/worker).
package ws

import (
	"context"
	"log/slog"
	"os"

	clientPkg "llmesh/client"
	"llmesh/client/internal/llamacpp"
	"llmesh/client/internal/stats"
	"llmesh/client/internal/worker"
	"llmesh/pkg/types"
	"llmesh/pkg/wsclient"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Conn is the client-specific WebSocket connection.
type Conn struct {
	inner *wsclient.Conn
}

// New creates a Conn wired to the given config and stats.
func New(cfg *clientPkg.Config, version string, st *stats.Stats) *Conn {
	models := &clientModelProvider{cfg: cfg}
	jobs := &clientJobDispatcher{cfg: cfg, st: st}
	inner := wsclient.New(cfg.RouterURL, cfg.RouterToken, cfg.MaxConcurrent, version, st, models, jobs, log)
	return &Conn{inner: inner}
}

// Run connects to the router and reconnects on disconnect. Blocks until ctx is cancelled.
func (c *Conn) Run(ctx context.Context) {
	c.inner.Run(ctx)
}

// SetOnUpdate registers a callback invoked when the router requests an in-place update.
// Must be called before Run.
func (c *Conn) SetOnUpdate(fn func()) {
	c.inner.SetOnUpdate(fn)
}

// SlotPool returns the shared concurrency pool. Pass this to the local API
// server so local requests share the same slot budget as router-dispatched jobs.
func (c *Conn) SlotPool() *wsclient.SlotPool { return c.inner.Pool() }

// clientModelProvider probes llama.cpp for model capabilities on each (re)connection.
type clientModelProvider struct {
	cfg *clientPkg.Config
}

func (p *clientModelProvider) Models(ctx context.Context) ([]types.ModelInfo, int) {
	models := make([]types.ModelInfo, 0, len(p.cfg.Models))
	totalSlots := 0
	for _, m := range p.cfg.Models {
		lc := llamacpp.New(m.Endpoint)

		// Resolve the model name: explicit config name wins; otherwise ask the
		// endpoint what model it serves via /v1/models.
		name := m.Name
		if name == "" {
			name = lc.ProbeModelID(ctx)
			if name == "" {
				log.Warn("ws: could not auto-detect model name from endpoint, skipping", "endpoint", m.Endpoint)
				continue
			}
			p.cfg.SetResolvedName(m.Endpoint, name)
			log.Info("ws: auto-detected model name", "endpoint", m.Endpoint, "model", name)
		}

		props := lc.ProbeProps(ctx)
		models = append(models, types.ModelInfo{Name: name, ContextSize: props.NCtx, ContextTrain: props.NCtxTrain})
		if props.NCtx > 0 {
			log.Info("ws: model props", "model", name,
				"context_size", props.NCtx, "context_train", props.NCtxTrain,
				"total_slots", props.TotalSlots)
		}
		if props.ChatTemplate != "" {
			p.cfg.SetDetectedTemplate(name, props.ChatTemplate)
		}
		totalSlots += props.TotalSlots
	}
	return models, totalSlots
}

// clientJobDispatcher dispatches jobs via the llama.cpp worker.
type clientJobDispatcher struct {
	cfg *clientPkg.Config
	st  *stats.Stats
}

// Try always accepts jobs — llama.cpp validation happens at inference time.
func (d *clientJobDispatcher) Try(_ types.JobMsg, _ func(any) error) bool { return true }

func (d *clientJobDispatcher) Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error {
	return worker.Handle(ctx, job, d.cfg, send, d.st)
}
