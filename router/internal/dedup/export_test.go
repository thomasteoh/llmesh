package dedup

// Test-only accessors for entry state that is deliberately unexported. Kept in
// an _test.go file so they are not part of the package's real API.

// bufferDropped reports whether hash's replay buffer has been released.
func (r *Registry) bufferDropped(hash string) bool {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.replayDropped && e.chunks == nil
}

// forceDropReplay releases hash's replay buffer without having to push
// maxReplayChunks chunks through Forward first.
func (r *Registry) forceDropReplay(hash string) {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replayDropped = true
	e.chunks = nil
}

// subscriberCount returns the number of live followers on hash.
func (r *Registry) subscriberCount(hash string) int {
	r.mu.Lock()
	e, ok := r.entries[hash]
	r.mu.Unlock()
	if !ok {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.subs)
}
