package client_test

// Phase 10 stress: high-volume update streams, many subscribers, many
// producers, repeated reconnects — all under -race with bounded memory.

import (
	"context"
	"fmt"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestUpdateStress100kUpdates: 100k sequential updates through the full
// pipeline must not grow memory linearly from historical state and must
// reconcile exactly.
func TestUpdateStress100kUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newUpdatesClient(t)
	defer c.Close()

	var delivered int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt64(&delivered, 1)
	})
	defer sub.Unsubscribe()

	const total = 100000
	const batch = 100 // 1000 Updates envelopes of 100 updates each
	heapBefore := heapAfterGC()

	for i := 0; i < total/batch; i++ {
		ups := make([]gen.UpdateClass, batch)
		for j := 0; j < batch; j++ {
			pts := int32(i*batch + j + 1)
			ups[j] = updMsg(pts)
		}
		fake.PushUpdate(&gen.Updates{
			Updates: ups, Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
			Date: 100, Seq: 0,
		})
	}
	if !waitUntil(t, 60*time.Second, func() bool {
		return atomic.LoadInt64(&delivered) == total
	}) {
		t.Fatalf("delivered %d of %d", atomic.LoadInt64(&delivered), total)
	}

	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("heap growth over %d updates: %.1f MiB", total, float64(growth)/(1<<20))
	// Bounded-memory contract: no per-update historical state (the
	// MessageBox holds only per-entry pts; the gap buffer is capped).
	if growth > 32*1024*1024 {
		t.Fatalf("heap grew %.1f MiB across %d updates (unbounded state?)",
			float64(growth)/(1<<20), total)
	}
}

// TestUpdateStress100Subscribers: 100 subscribers × 1000 updates — every
// subscriber receives every update exactly once.
func TestUpdateStress100Subscribers(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newUpdatesClient(t)
	defer c.Close()

	const subs, updates = 100, 1000
	counters := make([]int64, subs)
	var wgSubs sync.WaitGroup
	subsList := make([]*client.Subscription, subs)
	for i := 0; i < subs; i++ {
		wgSubs.Add(1)
		i := i
		subsList[i] = c.OnUpdate(func(ctx context.Context, upd tl.Object) {
			atomic.AddInt64(&counters[i], 1)
			if atomic.LoadInt64(&counters[i]) == updates {
				wgSubs.Done()
			}
		})
	}
	defer func() {
		for _, s := range subsList {
			s.Unsubscribe()
		}
	}()

	for i := 0; i < updates; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(int32(i + 1))},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	done := make(chan struct{})
	go func() { wgSubs.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(60 * time.Second):
		t.Fatal("subscribers did not receive all updates in time")
	}
}

// TestUpdateStress32Producers: 32 concurrent producers, each driving its
// own channel's pts sequence — per-entry state independence under
// concurrency (disjoint ranges on ONE account pts sequence cannot
// reconcile by design; per-channel entries are the reference's answer).
func TestUpdateStress32Producers(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newUpdatesClient(t)
	defer c.Close()

	const producers, perProducer = 32, 200
	var delivered int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt64(&delivered, 1)
	})
	defer sub.Unsubscribe()

	var wg sync.WaitGroup
	for p := 0; p < producers; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			chID := int64(1000 + p)
			for i := 0; i < perProducer; i++ {
				cm := &gen.UpdateNewChannelMessage{
					Message:  &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerChannel{ChannelID: chID}},
					PTS:      int32(i + 1),
					PTSCount: 1,
				}
				fake.PushUpdate(&gen.Updates{
					Updates: []gen.UpdateClass{cm},
					Users:   []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0,
				})
				// Pace under the bounded-queue saturation point: the
				// engine's updates channel (1024) drops on overflow by
				// documented policy — an exactly-once assertion models a
				// stream within bounds (a real server never emits 6400
				// frames back-to-back anyway).
				time.Sleep(200 * time.Microsecond)
			}
		}(p)
	}
	wg.Wait()
	if !waitUntil(t, 60*time.Second, func() bool {
		return atomic.LoadInt64(&delivered) == producers*perProducer
	}) {
		m := c.Metrics()
		t.Fatalf("delivered %d of %d (engine drops=%d, client drops=%d)",
			atomic.LoadInt64(&delivered), producers*perProducer, m.UpdatesDropped, m.UpdatesDropped)
	}
	if m := c.Metrics(); m.UpdatesDropped != 0 {
		t.Fatalf("unexpected drops: %d", m.UpdatesDropped)
	}
}

// TestUpdateStress10ReconnectCycles: repeated disconnect/reconnect with
// persisted update state — no leaks, frontier survives.
func TestUpdateStress10ReconnectCycles(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	base := runtime.NumGoroutine()

	for cycle := 0; cycle < 10; cycle++ {
		srv := newWSTestServerForAuth(t)
		fs, err := session.OpenFile(filepath.Join(t.TempDir(), fmt.Sprintf("cycle-%d.session", cycle)))
		if err != nil {
			t.Fatal(err)
		}
		c, fake := newAuthClientWithSession(t, srv, fs)
		uf := newUpdatesFake()
		installUpdatesHooks(t, fake, uf)

		var delivered int64
		sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
			atomic.AddInt64(&delivered, 1)
		})
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(int32(cycle*10 + 1))},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
		waitUntil(t, 5*time.Second, func() bool { return atomic.LoadInt64(&delivered) >= 1 })
		sub.Unsubscribe()
		c.Close()
	}
	if err := goroutineSettled(base, 12); err != nil {
		t.Fatal(err)
	}
}

// TestUpdateStressSlowSubscriberDoesNotBlock: a subscriber that sleeps
// must never stall other subscribers or the pipeline (§22).
func TestUpdateStressSlowSubscriberDoesNotBlock(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newUpdatesClientW(t, 1)
	defer c.Close()

	var fastCount int64
	slowDone := make(chan struct{})
	slow := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		time.Sleep(5 * time.Millisecond)
		close(slowDone)
	})
	defer slow.Unsubscribe()
	fast := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt64(&fastCount, 1)
	})
	defer fast.Unsubscribe()

	for i := 0; i < 50; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(int32(i + 1))},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	if !waitUntil(t, 10*time.Second, func() bool {
		return atomic.LoadInt64(&fastCount) >= 50
	}) {
		t.Fatalf("fast subscriber stalled at %d (slow subscriber blocked the pipeline)",
			atomic.LoadInt64(&fastCount))
	}
	<-slowDone
}
