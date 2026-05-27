// shim/internal/ws/conn.go
//
// Thin shim-specific wrapper around pkg/wsclient.Conn.
// Provides the model list (from config, no probing) and job dispatcher
// (via shim/internal/worker + backend routing).
package ws

import (
	"context"
	"log/slog"
	"os"

	shimPkg "llmesh/shim"
	"llmesh/shim/internal/backend"
	"llmesh/shim/internal/stats"
	"llmesh/shim/internal/worker"
	"llmesh/pkg/types"
	"llmesh/pkg/wsclient"
)

var log = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

// Conn is the shim-specific WebSocket connection.
type Conn struct {
	inner *wsclient.Conn
}

// New creates a Conn wired to the given config and stats.
func New(cfg *shimPkg.Config, version string, st *stats.Stats) *Conn {
	models := &shimModelProvider{cfg: cfg}
	jobs := &shimJobDispatcher{cfg: cfg, st: st}
	inner := wsclient.New(cfg.RouterURL, cfg.RouterToken, cfg.MaxConcurrent, version, st, models, jobs, log)
	return &Conn{inner: inner}
}

// Run connects to the router and reconnects on disconnect. Blocks until ctx is cancelled.
func (c *Conn) Run(ctx context.Context) {
	c.inner.Run(ctx)
}

// shimModelProvider reads model names and context sizes directly from config.
type shimModelProvider struct {
	cfg *shimPkg.Config
}

func (p *shimModelProvider) Models(_ context.Context) []types.ModelInfo {
	models := make([]types.ModelInfo, 0, len(p.cfg.Models))
	for _, m := range p.cfg.Models {
		models = append(models, types.ModelInfo{Name: m.Name, ContextSize: m.ContextSize})
	}
	return models
}

// shimJobDispatcher routes jobs to the appropriate backend via the shim worker.
type shimJobDispatcher struct {
	cfg *shimPkg.Config
	st  *stats.Stats
}

// Try rejects jobs whose model has no configured backend, without acquiring a
// concurrency slot. This keeps the read loop responsive to cancel messages.
func (d *shimJobDispatcher) Try(job types.JobMsg, send func(any) error) bool {
	if d.cfg.BackendFor(job.Request.Model) == nil {
		log.Warn("ws: no backend configured for model", "model", job.Request.Model)
		_ = send(types.ErrorMsg{
			Type:      "error",
			RequestID: job.Request.ID,
			Message:   "model not available on this shim",
		})
		return false
	}
	return true
}

func (d *shimJobDispatcher) Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error {
	bc := d.cfg.BackendFor(job.Request.Model)
	if bc == nil {
		return nil // Try already handled this; shouldn't normally reach here
	}
	return worker.Handle(ctx, job, specFromBackend(bc), send, d.st)
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
