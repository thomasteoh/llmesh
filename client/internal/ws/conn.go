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

// clientModelProvider probes llama.cpp for model capabilities on each (re)connection.
type clientModelProvider struct {
	cfg *clientPkg.Config
}

func (p *clientModelProvider) Models(ctx context.Context) ([]types.ModelInfo, int) {
	models := make([]types.ModelInfo, 0, len(p.cfg.Models))
	totalSlots := 0
	for _, m := range p.cfg.Models {
		lc := llamacpp.New(p.cfg.EndpointFor(m.Name))
		props := lc.ProbeProps(ctx)
		models = append(models, types.ModelInfo{Name: m.Name, ContextSize: props.NCtx, ContextTrain: props.NCtxTrain})
		if props.NCtx > 0 {
			log.Info("ws: model props", "model", m.Name,
				"context_size", props.NCtx, "context_train", props.NCtxTrain,
				"total_slots", props.TotalSlots)
		}
		if props.ChatTemplate != "" {
			p.cfg.SetDetectedTemplate(m.Name, props.ChatTemplate)
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
