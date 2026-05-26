package stats

import (
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

func (s *Stats) Connected() bool        { return s.connected.Load() }
func (s *Stats) SetConnected(v bool)    { s.connected.Store(v) }
