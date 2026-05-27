package stats

import (
	"expvar"
	"sync/atomic"
	"time"
)

// Stats holds runtime counters for the llmesh-client. All fields are safe for
// concurrent use. Register must be called once to publish them to expvar.
type Stats struct {
	ActiveJobs  atomic.Int64
	TotalDone   atomic.Int64
	TotalErrors atomic.Int64
	TotalTokens atomic.Int64
	Reconnects  atomic.Int64
	connected   atomic.Bool
	StartTime   time.Time
}

// New creates a Stats instance. Call Register to expose metrics via expvar.
func New() *Stats {
	return &Stats{StartTime: time.Now()}
}

// Register publishes the stats counters to expvar. Call once at process startup.
func Register(s *Stats) {
	expvar.Publish("llmclient_active_jobs", expvar.Func(func() any { return s.ActiveJobs.Load() }))
	expvar.Publish("llmclient_jobs_done", expvar.Func(func() any { return s.TotalDone.Load() }))
	expvar.Publish("llmclient_jobs_errors", expvar.Func(func() any { return s.TotalErrors.Load() }))
	expvar.Publish("llmclient_tokens_total", expvar.Func(func() any { return s.TotalTokens.Load() }))
	expvar.Publish("llmclient_reconnects", expvar.Func(func() any { return s.Reconnects.Load() }))
	expvar.Publish("llmclient_connected", expvar.Func(func() any { return s.connected.Load() }))
	expvar.Publish("llmclient_uptime_seconds", expvar.Func(func() any { return int64(time.Since(s.StartTime).Seconds()) }))
}

func (s *Stats) Connected() bool     { return s.connected.Load() }
func (s *Stats) SetConnected(v bool) { s.connected.Store(v) }

// ConnStats interface methods — used by pkg/wsclient.Conn.
func (s *Stats) IncrReconnects() { s.Reconnects.Add(1) }
func (s *Stats) IncrActive()     { s.ActiveJobs.Add(1) }
func (s *Stats) DecrActive()     { s.ActiveJobs.Add(-1) }
func (s *Stats) IncrDone()       { s.TotalDone.Add(1) }
func (s *Stats) IncrError()      { s.TotalErrors.Add(1) }
