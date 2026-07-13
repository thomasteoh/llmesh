package stats

import (
	"expvar"
	"sync/atomic"
	"time"
)

// Stats holds live counters for the shim status line and metrics.
type Stats struct {
	ActiveJobs  atomic.Int64
	TotalDone   atomic.Int64
	TotalErrors atomic.Int64
	TotalTokens atomic.Int64
	Reconnects  atomic.Int64
	StartTime   time.Time
	connected   atomic.Bool
}

func New() *Stats {
	return &Stats{StartTime: time.Now()}
}

// Register publishes the stats counters to expvar. Call once at process startup
// when a metrics endpoint is enabled.
func Register(s *Stats) {
	expvar.Publish("llmshim_active_jobs", expvar.Func(func() any { return s.ActiveJobs.Load() }))
	expvar.Publish("llmshim_jobs_done", expvar.Func(func() any { return s.TotalDone.Load() }))
	expvar.Publish("llmshim_jobs_errors", expvar.Func(func() any { return s.TotalErrors.Load() }))
	expvar.Publish("llmshim_tokens", expvar.Func(func() any { return s.TotalTokens.Load() }))
	expvar.Publish("llmshim_reconnects", expvar.Func(func() any { return s.Reconnects.Load() }))
}

func (s *Stats) Connected() bool     { return s.connected.Load() }
func (s *Stats) SetConnected(v bool) { s.connected.Store(v) }

// ConnStats interface methods — used by pkg/wsclient.Conn.
func (s *Stats) IncrReconnects() { s.Reconnects.Add(1) }
func (s *Stats) IncrActive()     { s.ActiveJobs.Add(1) }
func (s *Stats) DecrActive()     { s.ActiveJobs.Add(-1) }
func (s *Stats) IncrDone()       { s.TotalDone.Add(1) }
func (s *Stats) IncrError()      { s.TotalErrors.Add(1) }
