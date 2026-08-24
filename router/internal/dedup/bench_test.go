package dedup

import (
	"fmt"
	"log/slog"
	"testing"

	"llmesh/pkg/types"
)

// These exist to settle whether Forward's locking is worth restructuring: it
// takes a registry-wide lock for every chunk of every coalesced request, and
// holds the per-entry lock across the fan-out to subscribers. Both are cheap
// enough to leave alone — see the note on Forward — and these keep that claim
// checkable rather than asserted.

func quietRegistry() *Registry { return New(slog.New(slog.DiscardHandler)) }

// Cost of one Forward with n attached followers, all keeping up. Flat in n is
// the result that matters: it means the fan-out under the entry lock is not
// what a chunk spends its time on.
func BenchmarkForward(b *testing.B) {
	for _, n := range []int{0, 1, 4, 16} {
		b.Run(fmt.Sprintf("followers=%d", n), func(b *testing.B) {
			r := quietRegistry()
			r.RegisterOrSubscribe("h")
			for i := 0; i < n; i++ {
				_, _, live := r.RegisterOrSubscribe("h")
				go func() {
					for range live {
					}
				}()
			}
			c := types.ChunkMsg{Type: "chunk", Delta: "hello world"}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r.Forward("h", c)
			}
			b.StopTimer()
			r.Forward("h", types.ChunkMsg{Type: "chunk", Done: true})
		})
	}
}

// Many concurrent generations forwarding at once: this is what contends on the
// registry-wide lock Forward takes for every chunk of every request.
func BenchmarkForwardConcurrentEntries(b *testing.B) {
	for _, streams := range []int{8, 64, 256} {
		b.Run(fmt.Sprintf("streams=%d", streams), func(b *testing.B) {
			r := quietRegistry()
			hashes := make([]string, streams)
			for i := range hashes {
				hashes[i] = fmt.Sprintf("hash-%d", i)
				r.RegisterOrSubscribe(hashes[i])
			}
			c := types.ChunkMsg{Type: "chunk", Delta: "hello world"}
			b.ReportAllocs()
			b.ResetTimer()
			var n int64
			b.RunParallel(func(pb *testing.PB) {
				i := int(n)
				n++
				for pb.Next() {
					r.Forward(hashes[i%streams], c)
					i++
				}
			})
		})
	}
}
