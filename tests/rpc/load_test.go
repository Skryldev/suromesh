package rpc_test

import (
	"context"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestLoadConcurrentRequests drives 10 → 100 000 concurrent Invokes through
// the full stack against an immediately-responding fake server. Measures
// throughput, latency percentiles, goroutine bounds, and registry hygiene.
//
// Skipped in -short mode.
func TestLoadConcurrentRequests(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short mode")
	}
	levels := []struct {
		name      string
		total     int
		producers int
	}{
		{"10", 10, 10},
		{"100", 100, 100},
		{"1k", 1000, 1000},
		{"10k", 10_000, 1000},
		{"100k", 100_000, 1000},
	}

	for _, lv := range levels {
		lv := lv
		t.Run(lv.name, func(t *testing.T) {
			h := startHarness(t, rpc.Config{}, nil)
			h.SetResponder(func(peer *wstest.SoroushPeer, env wstest.ClientEnvelope) {
				_ = replyWith(peer, env, h.AuthKey, &gen.BoolTrue{}, nil)
			})

			before := runtime.NumGoroutine()
			var ms runtime.MemStats
			runtime.ReadMemStats(&ms)
			heapBefore := ms.TotalAlloc

			latencies := make([]time.Duration, 0, lv.total)
			var latMu sync.Mutex
			var failed atomic.Uint64

			per := lv.total / lv.producers
			if per == 0 {
				per = 1
			}
			start := time.Now()
			var wg sync.WaitGroup
			wg.Add(lv.producers)
			for p := 0; p < lv.producers; p++ {
				go func() {
					defer wg.Done()
					local := make([]time.Duration, 0, per)
					for i := 0; i < per; i++ {
						s := time.Now()
						ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
						_, err := h.Engine.Invoke(ctx, &gen.PingRequest{PingID: 1})
						cancel()
						local = append(local, time.Since(s))
						if err != nil {
							failed.Add(1)
						}
					}
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)

			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			pct := func(p float64) time.Duration {
				idx := int(float64(len(latencies)-1) * p)
				return latencies[idx]
			}
			runtime.ReadMemStats(&ms)

			t.Logf("requests=%d failed=%d throughput=%.0f req/s",
				len(latencies), failed.Load(), float64(len(latencies))/elapsed.Seconds())
			t.Logf("  latency: p50=%v p95=%v p99=%v max=%v",
				pct(0.50), pct(0.95), pct(0.99), latencies[len(latencies)-1])
			t.Logf("  goroutines after=%d (before=%d) pending=%d heap/req=%.0f B",
				runtime.NumGoroutine(), before, h.Engine.Pending(),
				float64(ms.TotalAlloc-heapBefore)/float64(max(len(latencies), 1)))

			if failed.Load() > 0 {
				t.Fatalf("%d requests failed", failed.Load())
			}
			if h.Engine.Pending() != 0 {
				t.Fatalf("registry leak: %d pending", h.Engine.Pending())
			}
			if g := runtime.NumGoroutine(); g > before+16 {
				t.Fatalf("goroutines grew: %d > %d+16", g, before)
			}
		})
	}
}
