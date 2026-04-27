// Package logring provides a thread-safe in-memory ring-buffer slog.Handler
// that stores recent log entries per named category for display in the admin UI.
package logring

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"
)

// DefaultCap is the default ring-buffer capacity per category.
const DefaultCap = 500

var validCategories = []string{"router", "hub", "scheduler", "api", "admin"}

// Entry is a single captured log record.
type Entry struct {
	Time    time.Time         `json:"time"`
	Level   string            `json:"level"`
	Message string            `json:"msg"`
	Attrs   map[string]string `json:"attrs,omitempty"`
}

// Sink holds per-category ring buffers.
type Sink struct {
	mu    sync.Mutex
	rings map[string][]Entry
	pos   map[string]int
	lens  map[string]int
	cap   int
}

// New creates a Sink with the given capacity per category.
func New(capacity int) *Sink {
	s := &Sink{
		rings: make(map[string][]Entry, len(validCategories)),
		pos:   make(map[string]int, len(validCategories)),
		lens:  make(map[string]int, len(validCategories)),
		cap:   capacity,
	}
	for _, cat := range validCategories {
		s.rings[cat] = make([]Entry, capacity)
	}
	return s
}

// add appends an entry to the named category ring. Unknown categories are dropped.
func (s *Sink) add(category string, e Entry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring, ok := s.rings[category]
	if !ok {
		return
	}
	idx := s.pos[category]
	ring[idx] = e
	s.pos[category] = (idx + 1) % s.cap
	if s.lens[category] < s.cap {
		s.lens[category]++
	}
}

// Query returns up to limit entries for category in newest-first order.
func (s *Sink) Query(category string, limit int) []Entry {
	s.mu.Lock()
	defer s.mu.Unlock()
	ring, ok := s.rings[category]
	if !ok {
		return nil
	}
	n := s.lens[category]
	if limit > 0 && limit < n {
		n = limit
	}
	out := make([]Entry, n)
	pos := s.pos[category]
	for i := 0; i < n; i++ {
		idx := (pos - 1 - i + s.cap) % s.cap
		out[i] = ring[idx]
	}
	return out
}

// Categories returns the list of known category names.
func Categories() []string {
	out := make([]string, len(validCategories))
	copy(out, validCategories)
	return out
}

// ─── slog.Handler ────────────────────────────────────────────────────────────

// handler is a slog.Handler that writes to a Sink for a specific category.
type handler struct {
	sink      *Sink
	category  string
	level     slog.Level
	attrs     []slog.Attr
	groupPath string // dot-separated group prefix for attr keys, e.g. "request.http"
}

// newHandler returns a handler for the given category.
func (s *Sink) newHandler(category string, level slog.Level) *handler {
	return &handler{sink: s, category: category, level: level}
}

func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *handler) prefixKey(key string) string {
	if h.groupPath == "" {
		return key
	}
	return h.groupPath + "." + key
}

func (h *handler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]string, len(h.attrs)+r.NumAttrs())
	for _, a := range h.attrs {
		attrs[h.prefixKey(a.Key)] = fmt.Sprintf("%v", a.Value.Any())
	}
	r.Attrs(func(a slog.Attr) bool {
		attrs[h.prefixKey(a.Key)] = fmt.Sprintf("%v", a.Value.Any())
		return true
	})
	if len(attrs) == 0 {
		attrs = nil
	}
	h.sink.add(h.category, Entry{
		Time:    r.Time,
		Level:   r.Level.String(),
		Message: r.Message,
		Attrs:   attrs,
	})
	return nil
}

func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	h2 := *h
	h2.attrs = append(append([]slog.Attr(nil), h.attrs...), attrs...)
	return &h2
}

func (h *handler) WithGroup(name string) slog.Handler {
	h2 := *h
	if h.groupPath == "" {
		h2.groupPath = name
	} else {
		h2.groupPath = h.groupPath + "." + name
	}
	return &h2
}

// ─── Multi-handler ────────────────────────────────────────────────────────────

// multiHandler fans a log record out to multiple slog.Handlers.
type multiHandler struct {
	handlers []slog.Handler
}

func (m *multiHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, h := range m.handlers {
		if h.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

func (m *multiHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range m.handlers {
		if h.Enabled(ctx, r.Level) {
			if err := h.Handle(ctx, r); err != nil && first == nil {
				first = err
			}
		}
	}
	return first
}

func (m *multiHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithAttrs(attrs)
	}
	return &multiHandler{handlers: hs}
}

func (m *multiHandler) WithGroup(name string) slog.Handler {
	hs := make([]slog.Handler, len(m.handlers))
	for i, h := range m.handlers {
		hs[i] = h.WithGroup(name)
	}
	return &multiHandler{handlers: hs}
}

// NewLogger creates a *slog.Logger that writes to both os.Stderr (text format)
// and the shared ring-buffer sink under the given category.
// If sink is nil, output goes to stderr only.
func NewLogger(sink *Sink, category string, level slog.Level) *slog.Logger {
	stderr := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})
	if sink == nil {
		return slog.New(stderr)
	}
	ring := sink.newHandler(category, level)
	return slog.New(&multiHandler{handlers: []slog.Handler{stderr, ring}})
}
