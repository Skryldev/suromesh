package transport_test

import (
	"context"
	"fmt"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/transport"
)

// TestLoadSaturation drives synthetic workloads at 1/10/100/1000 concurrent
// producers through the transport against a draining peer, reporting
// throughput, latency percentiles (producer-side enqueue latency, which is
// where backpressure materializes), and goroutine/queue observations.
//
// Run with -short to skip (CI smoke) or without for the full sweep.
func TestLoadSaturation(t *testing.T) {
	if testing.Short() {
		t.Skip("load test skipped in -short mode")
	}
	levels := []struct {
		producers int
		packets   int
	}{
		{1, 50_000},
		{10, 50_000},
		{100, 100_000},
		{1000, 100_000},
	}

	for _, lv := range levels {
		lv := lv
		t.Run(fmt.Sprintf("producers=%d", lv.producers), func(t *testing.T) {
			srv, err := wstest.New()
			if err != nil {
				t.Fatal(err)
			}
			defer srv.Close()

			var received atomic.Uint64
			srv.Handler = func(c *wstest.ServerConn) {
				peer := wstest.HandleSoroush(c, func(_ *wstest.SoroushPeer, _ []byte) { received.Add(1) })
				peer.Wait()
			}

			cfg := testConfig(srv.Addr)
			cfg.OutboundQueueSize = 1024
			tr := transport.New(cfg)
			if err := tr.Connect(context.Background()); err != nil {
				t.Fatal(err)
			}
			defer tr.Close()

			payload := packet(7) // 64 bytes
			latencies := make([]time.Duration, 0, lv.packets)
			var latMu sync.Mutex
			var rejected atomic.Uint64

			per := lv.packets / lv.producers
			before := runtime.NumGoroutine()
			start := time.Now()

			var wg sync.WaitGroup
			wg.Add(lv.producers)
			for p := 0; p < lv.producers; p++ {
				go func(id int) {
					defer wg.Done()
					local := make([]time.Duration, 0, per)
					ctx := context.Background()
					for i := 0; i < per; i++ {
						s := time.Now()
						err := tr.Send(ctx, payload)
						local = append(local, time.Since(s))
						if err != nil {
							rejected.Add(1)
						}
					}
					latMu.Lock()
					latencies = append(latencies, local...)
					latMu.Unlock()
				}(p)
			}
			wg.Wait()
			elapsed := time.Since(start)

			// Deliverability: the peer must have received essentially all
			// non-rejected packets.
			waitFor(t, func() bool {
				return received.Load() >= uint64(lv.packets)-rejected.Load()
			})

			sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
			pct := func(p float64) time.Duration {
				idx := int(float64(len(latencies)-1) * p)
				return latencies[idx]
			}

			goroutines := runtime.NumGoroutine()
			m := tr.Metrics().Snapshot()
			t.Logf("producers=%d packets=%d rejected=%d", lv.producers, lv.packets, rejected.Load())
			t.Logf("  throughput: %.0f packets/s (%.1f MB/s)",
				float64(len(latencies))/elapsed.Seconds(),
				float64(len(latencies))*float64(len(payload))/elapsed.Seconds()/1e6)
			t.Logf("  enqueue latency: p50=%v p95=%v p99=%v max=%v",
				pct(0.50), pct(0.95), pct(0.99), latencies[len(latencies)-1])
			t.Logf("  goroutines during load: %d (baseline %d), queue full events: %d",
				goroutines, before, m.QueueFullEvents)

			if rejected.Load() > 0 {
				t.Fatalf("PolicyBlock must not reject: %d rejected", rejected.Load())
			}
		})
	}
}
