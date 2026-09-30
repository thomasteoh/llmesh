// client/internal/ws/conn.go
//
// Thin client-specific wrapper around pkg/wsclient.Conn.
// Provides the model list (via llama.cpp context-size probing) and
// job dispatcher (via client/internal/worker).
package ws

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	clientPkg "llmesh/client"
	"llmesh/client/internal/health"
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
	br := health.NewBreaker(nil)
	models := &clientModelProvider{cfg: cfg, breaker: br, backends: map[string]*backendState{}}
	jobs := &clientJobDispatcher{cfg: cfg, st: st, breaker: br}
	inner := wsclient.New(cfg.RouterURL, cfg.RouterToken, cfg.MaxConcurrent, version, st, models, jobs, log)
	inner.SetKind("llama.cpp")
	// Withdraw a model from the router as soon as it trips, not at the next
	// health check, so it stops being sent jobs that will fail.
	br.SetOnChange(inner.Recheck)
	return &Conn{inner: inner}
}

// Run connects to the router and reconnects on disconnect. Blocks until ctx is cancelled.
func (c *Conn) Run(ctx context.Context) {
	c.inner.Run(ctx)
}

// SetDrain makes shutdown let jobs in flight finish for up to timeout; see
// wsclient.Conn.SetDrain.
func (c *Conn) SetDrain(timeout time.Duration, force <-chan struct{}) {
	c.inner.SetDrain(timeout, force)
}

// SlotPool returns the shared concurrency pool. Pass this to the local API
// server so local requests share the same slot budget as router-dispatched jobs.
func (c *Conn) SlotPool() *wsclient.SlotPool { return c.inner.Pool() }

// Health checks: how many failed in a row withdraw a backend's model, and how
// long one round of probes against a backend may take.
const (
	downAfter    = 3
	probeTimeout = 15 * time.Second
)

// backendState is what the provider remembers about one backend between
// probes.
type backendState struct {
	failures int              // readiness probes failed in a row
	last     *types.ModelInfo // what it last advertised; nil while withdrawn
	slots    int              // its total_slots when last probed
	up       bool             // has ever been ready
	props    string           // last props logged, to log only changes
	tripped  bool             // withdrawn by the breaker when last probed
}

// clientModelProvider probes the backends on each (re)connection and, as a
// wsclient.ModelWatcher, again every few seconds while connected: a backend
// that is down, or whose model keeps failing requests, is not advertised.
type clientModelProvider struct {
	cfg     *clientPkg.Config
	breaker *health.Breaker

	mu       sync.Mutex // serialises Models; guards backends
	backends map[string]*backendState
}

// WatchModels makes wsclient call Models periodically while connected.
func (p *clientModelProvider) WatchModels() bool { return true }

func (p *clientModelProvider) Models(ctx context.Context) ([]types.ModelInfo, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	models := make([]types.ModelInfo, 0, len(p.cfg.Models))
	totalSlots := 0
	for _, m := range p.cfg.Models {
		st := p.backends[m.Endpoint]
		if st == nil {
			st = &backendState{}
			p.backends[m.Endpoint] = st
		}
		info, slots := p.probe(ctx, m, st)
		if info == nil {
			continue
		}
		if p.breaker.Open(info.Name) {
			if !st.tripped {
				log.Warn("ws: model withdrawn after repeated failures", "model", info.Name,
					"until", p.breaker.OpenUntil(info.Name).Format(time.TimeOnly))
			}
			st.tripped = true
			continue
		}
		if st.tripped {
			log.Info("ws: offering model again after its cooldown", "model", info.Name)
			st.tripped = false
		}
		models = append(models, *info)
		totalSlots += slots
	}
	return models, totalSlots
}

// probe checks one backend and returns what to advertise for it, or nil to
// advertise nothing.
func (p *clientModelProvider) probe(ctx context.Context, m clientPkg.ModelConfig, st *backendState) (*types.ModelInfo, int) {
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	lc := llamacpp.New(m.Endpoint, m.RequestHeaders())

	if !lc.Ready(pctx) {
		// A backend busy with requests can miss a health check without being
		// down; if it has crashed those requests fail, and the breaker acts.
		if st.last != nil && p.breaker.Active(st.last.Name) > 0 {
			return st.last, st.slots
		}
		st.failures++
		switch {
		case st.last != nil && st.failures < downAfter:
			return st.last, st.slots // one missed check is not an outage
		case st.last != nil:
			log.Warn("ws: backend not answering health checks, withdrawing its model",
				"model", st.last.Name, "endpoint", m.Endpoint, "failed_checks", st.failures)
			st.last = nil
		case st.failures == 1:
			log.Warn("ws: backend not ready yet, will keep checking", "endpoint", m.Endpoint)
		}
		return nil, 0
	}
	if st.failures > 0 && st.up {
		log.Info("ws: backend answering again", "endpoint", m.Endpoint)
	}
	st.failures = 0
	st.up = true

	// Resolve the model name: explicit config name wins; otherwise ask the
	// endpoint what model it serves via /v1/models.
	name := m.Name
	if name == "" {
		name = lc.ProbeModelID(pctx)
		if name == "" {
			if st.last != nil {
				log.Warn("ws: backend no longer names its model, withdrawing it", "endpoint", m.Endpoint)
			}
			st.last = nil
			return nil, 0
		}
		p.cfg.SetResolvedName(m.Endpoint, name)
	}

	props := lc.ProbeProps(pctx)
	modalities := m.EffectiveModalities(props.Modalities)
	info := &types.ModelInfo{
		Name:         name,
		ContextSize:  props.NCtx,
		ContextTrain: props.NCtxTrain,
		Modalities:   modalities,
	}
	if key := fmt.Sprintf("%s %d %d %d %v", name, props.NCtx, props.NCtxTrain, props.TotalSlots, modalities); key != st.props {
		st.props = key
		log.Info("ws: model props", "model", name, "endpoint", m.Endpoint,
			"context_size", props.NCtx, "context_train", props.NCtxTrain,
			"total_slots", props.TotalSlots, "modalities", modalities)
	}
	if props.ChatTemplate != "" {
		p.cfg.SetDetectedTemplate(name, props.ChatTemplate)
	}
	st.last = info
	st.slots = props.TotalSlots
	return info, props.TotalSlots
}

// clientJobDispatcher dispatches jobs via the llama.cpp worker.
type clientJobDispatcher struct {
	cfg     *clientPkg.Config
	st      *stats.Stats
	breaker *health.Breaker
}

// Try always accepts jobs — llama.cpp validation happens at inference time.
func (d *clientJobDispatcher) Try(_ types.JobMsg, _ func(any) error) bool { return true }

func (d *clientJobDispatcher) Dispatch(ctx context.Context, job types.JobMsg, send func(any) error) error {
	return worker.Handle(ctx, job, d.cfg, send, d.st, d.breaker)
}
