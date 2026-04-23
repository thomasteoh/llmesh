package stats

import "sync"

// Summary holds cumulative token counts for a single entity (model or user).
type Summary struct {
	Requests         int64
	PromptTokens     int64
	CompletionTokens int64
}

// Row is a named Summary used for sorted display.
type Row struct {
	Name string
	Summary
}

type entry struct {
	requests         int64
	promptTokens     int64
	completionTokens int64
}

// Stats accumulates per-model and per-user token usage in memory.
// Values reset on process restart — same lifecycle as TotalRequests.
type Stats struct {
	mu      sync.RWMutex
	byModel map[string]*entry
	byUser  map[string]*entry
}

func New() *Stats {
	return &Stats{
		byModel: make(map[string]*entry),
		byUser:  make(map[string]*entry),
	}
}

func add(m map[string]*entry, key string, prompt, completion int) {
	e, ok := m[key]
	if !ok {
		e = &entry{}
		m[key] = e
	}
	e.requests++
	e.promptTokens += int64(prompt)
	e.completionTokens += int64(completion)
}

// Record adds token usage for one completed request.
func (s *Stats) Record(model, user string, prompt, completion int) {
	if model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	add(s.byModel, model, prompt, completion)
	if user != "" {
		add(s.byUser, user, prompt, completion)
	}
}

func snapshot(m map[string]*entry) []Row {
	out := make([]Row, 0, len(m))
	for k, v := range m {
		out = append(out, Row{
			Name: k,
			Summary: Summary{
				Requests:         v.requests,
				PromptTokens:     v.promptTokens,
				CompletionTokens: v.completionTokens,
			},
		})
	}
	return out
}

// ByModel returns all per-model stats.
func (s *Stats) ByModel() []Row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshot(s.byModel)
}

// ByUser returns all per-user stats.
func (s *Stats) ByUser() []Row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return snapshot(s.byUser)
}
