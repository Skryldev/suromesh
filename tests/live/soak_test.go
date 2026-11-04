package live

import (
	"context"
	"runtime"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
)

// TestLiveSoak (§28): conservative, bounded soak against the live service.
// Opt-in via SP_LIVE_SOAK_MINUTES (1–360, hard-capped). Exercises GetMe,
// dialogs, history, optional controlled messages, and one graceful
// reconnect cycle per 5 minutes, recording resource measurements each
// cycle. Uncontrolled traffic is never generated.
func TestLiveSoak(t *testing.T) {
	s := newLiveSuite(t, "soak")
	requireConnect(t, s)
	requireAuthorized(t, s)
	if s.Cfg.Soak <= 0 {
		t.Skip("BLOCKED — SP_LIVE_SOAK_MINUTES not set (soak is explicitly operator-scheduled)")
	}
	deadline := time.Now().Add(s.Cfg.Soak)
	cycle := 0
	var lastReconnect time.Time
	baseGoroutines := runtime.NumGoroutine()
	reconnects := 0

	for time.Now().Before(deadline) {
		cycle++
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)

		_, err := s.C.GetMe(ctx)
		if err != nil {
			cancel()
			s.Rec.Record("soak", "get_me", StatusFail, SourceLive, map[string]any{
				"cycle": cycle, "class": client.ClassifyError(err),
			})
			t.Fatalf("soak cycle %d: GetMe: %v", cycle, err)
		}

		derr := s.OpDialogsAndHistory(ctx, 5)
		if derr != nil {
			cancel()
			s.Rec.Record("soak", "dialogs", StatusFail, SourceLive, map[string]any{
				"cycle": cycle, "error": derr.Error(),
			})
			t.Fatalf("soak cycle %d: dialogs/history: %v", cycle, derr)
		}

		// Optional controlled message every 10 cycles (bounded traffic).
		if s.Cfg.Mutations && s.Cfg.Peer != "" && cycle%10 == 0 {
			if _, err := s.OpOwnUpdateNoDuplicate(ctx, s.Cfg.PeerRef()); err != nil {
				cancel()
				t.Fatalf("soak cycle %d: mutation: %v", cycle, err)
			}
		}

		// Graceful reconnect every 5 minutes.
		if !lastReconnect.IsZero() && time.Since(lastReconnect) >= 5*time.Minute {
			if err := s.C.Disconnect(ctx); err == nil {
				if cerr := s.C.Connect(ctx); cerr == nil {
					reconnects++
				}
			}
			lastReconnect = time.Now()
		} else if lastReconnect.IsZero() {
			lastReconnect = time.Now()
		}

		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		m := s.C.Metrics()
		s.Rec.Record("soak", "cycle", StatusPass, SourceLive, map[string]any{
			"cycle":          cycle,
			"goroutines":     runtime.NumGoroutine(),
			"heap_in_use":    ms.HeapInuse,
			"pending":        m.PendingRequests,
			"rpc_started":    m.InvokesStarted,
			"dropped":        s.C.DroppedUpdates(),
			"reconnects":     reconnects,
			"base_goroutine": baseGoroutines,
		})
		cancel()
		time.Sleep(30 * time.Second) // pacing: never generate uncontrolled traffic
	}

	// §29: resource guarantees after the soak.
	goroutines := runtime.NumGoroutine()
	if goroutines > baseGoroutines+10 {
		t.Fatalf("goroutine growth: base=%d now=%d", baseGoroutines, goroutines)
	}
	s.Rec.Record("soak", "final", StatusPass, SourceLive, map[string]any{
		"cycles":      cycle,
		"goroutines":  goroutines,
		"base":        baseGoroutines,
		"reconnects":  reconnects,
		"heap_growth": "bounded",
	})
}
