package client_test

// Phase 15 soak: sustained concurrent workload across every subsystem —
// RPC, updates, mutations, media, reconnects, subscriber churn, session
// persistence — with growth assertions at the end.

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestSoakProductionRuntime: the Phase 15 soak matrix.
//
//	10,000 RPCs · 10,000 updates · 1,000 mutations · 500 media ops
//	100 reconnect cycles · 100 subscriber churn cycles · 50 persistence cycles
//
// All concurrent, under -race clean, with bounded end state.
func TestSoakProductionRuntime(t *testing.T) {
	if testing.Short() {
		t.Skip("soak (short)")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "soak.session")
	srv := newWSTestServerForAuth(t)

	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c, fake := newAuthClientWithSession(t, srv, fs)
	mf := newMutationsFake()
	installMutationHooks(t, fake, mf)
	uf := newUpdatesFake()
	uf.enableGapCorrection()
	installUpdatesHooks(t, fake, uf)

	var rpcOK, updatesSeen, mutationsOK, mediaOK int64
	var subscriberEvents int64
	base := runtime_NumGoroutine()
	heapBefore := heapAfterGC()

	// --- workload axes ----------------------------------------------------

	var wg sync.WaitGroup

	// 10,000 RPCs (32 workers). Injected reconnects legitimately fail
	// in-flight RPCs with connection loss (non-replayable by policy) —
	// workers retry until the target is met.
	for w := 0; w < 32; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			done := int64(0)
			for done < (10000+31)/32 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := c.Invoke(ctx, &gen.PingRequest{PingID: int64(w)*1000 + int64(done)})
				cancel()
				if err != nil {
					if client.AsConnectionLost(err) || errors.Is(err, client.ErrClosed) {
						time.Sleep(20 * time.Millisecond)
						continue // expected during reconnect windows
					}
					t.Errorf("rpc %d/%d: %v", w, done, err)
					return
				}
				done++
			}
			atomic.AddInt64(&rpcOK, (10000+31)/32)
		}(w)
	}

	// 10,000 updates pushed from the fake (single wire feeder), paced to
	// the wire's absorption rate (a real server is paced by TCP
	// backpressure) and recorded in the fake's push log so injected
	// reconnect windows produce gap-correcting differences.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10000; i++ {
			upd := &gen.UpdateNewMessage{
				Message: &gen.Message{ID: int32(i + 1), Message: "soak",
					PeerID: &gen.PeerUser{UserID: 300}, Date: int32(i + 1)},
				PTS: int32(i + 1), PTSCount: 1,
			}
			uf.recordPush(upd)
			fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{upd},
				Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
			time.Sleep(100 * time.Microsecond)
		}
	}()
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if _, ok := upd.(*gen.UpdateNewMessage); ok {
			atomic.AddInt64(&updatesSeen, 1)
		}
	})
	defer sub.Unsubscribe()

	// 1,000 mutations (4 workers) — retry through reconnect windows.
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			done := 0
			for done < 1000/4 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := c.SendMessageOpts(ctx,
					client.PeerID{Kind: client.PeerChat, ID: 55}, "mutate",
					client.SendMessageOptions{})
				cancel()
				if err != nil {
					if client.AsConnectionLost(err) || errors.Is(err, client.ErrClosed) {
						time.Sleep(20 * time.Millisecond)
						continue
					}
					t.Errorf("mutation %d/%d: %v", w, done, err)
					return
				}
				done++
			}
			atomic.AddInt64(&mutationsOK, int64((1000+3)/4))
		}(w)
	}

	// 500 media operations (existing-document resends, 8 workers).
	toDomain := func(id int64) *client.Document {
		return c.ClassifyMediaForTest(mediaDocWire(
			testDocument(id, 0xdead, []byte{byte(id)}, "application/octet-stream"))).(*client.Document)
	}
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			done := 0
			for done < (500+7)/8 {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				_, err := c.SendMediaInput(ctx,
					client.PeerID{Kind: client.PeerChat, ID: 55},
					client.ExistingDocumentInput{Document: toDomain(int64(5000 + w*100 + done))})
				cancel()
				if err != nil {
					if client.AsConnectionLost(err) || errors.Is(err, client.ErrClosed) {
						time.Sleep(20 * time.Millisecond)
						continue
					}
					t.Errorf("media %d/%d: %v", w, done, err)
					return
				}
				done++
			}
			atomic.AddInt64(&mediaOK, int64((500+7)/8))
		}(w)
	}

	// 100 reconnect cycles — STARTED only after the update flood has
	// fully converged (MessageBox frontier == 10000, verified through the
	// conversation store). Reconnects then race mutations, media,
	// subscriber churn and persistence — workloads whose individual
	// operations tolerate connection loss (bounded retry semantics).
	reconnectsDone := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		waitUntil(t, 120*time.Second, func() bool {
			conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
			return conv.TopMessageID == 10000
		})
		for i := 0; i < 100; i++ {
			time.Sleep(20 * time.Millisecond)
			c.ReconnectTransportForTest()
		}
		close(reconnectsDone)
	}()

	// 100 subscriber churn cycles.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			s := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
				atomic.AddInt64(&subscriberEvents, 1)
			})
			time.Sleep(5 * time.Millisecond)
			s.Unsubscribe()
		}
	}()

	// 50 persistence cycles.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			time.Sleep(30 * time.Millisecond)
			_ = fs.Save()
		}
	}()

	wg.Wait()

	// --- settle & verify ---------------------------------------------------
	// Wait for the reconnect cycle to finish its 100 forced losses, then
	// for the straggler RPC/mutation/media workers to complete.
	select {
	case <-reconnectsDone:
	case <-time.After(120 * time.Second):
		t.Fatal("reconnect cycles did not complete")
	}
	waitUntil(t, 60*time.Second, func() bool {
		return atomic.LoadInt64(&rpcOK) >= 10000 &&
			atomic.LoadInt64(&mutationsOK) >= 1000 &&
			atomic.LoadInt64(&mediaOK) >= 500
	})
	// The update flood must be FULLY converged (MessageBox frontier at
	// 10000) — verified via the conversation store, which counts committed
	// state, not wire deliveries.
	if !waitUntil(t, 60*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == 10000
	}) {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		t.Fatalf("update flood did not converge: top=%d want 10000", conv.TopMessageID)
	}

	m := c.Metrics()
	t.Logf("soak: rpc=%d updates=%d mutations=%d media=%d reconnects=%d drops=%d pending=%d",
		atomic.LoadInt64(&rpcOK), atomic.LoadInt64(&updatesSeen),
		atomic.LoadInt64(&mutationsOK), atomic.LoadInt64(&mediaOK),
		m.Reconnects, m.UpdatesDropped, m.PendingRequests)

	// The MessageBox committed state must reflect all 10,000 updates.
	conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
	if conv.TopMessageID != 10000 {
		t.Fatalf("conversation top = %d, want 10000 (state consistency)", conv.TopMessageID)
	}
	if m.PendingRequests != 0 {
		t.Fatalf("pending = %d after soak", m.PendingRequests)
	}

	if err := c.Close(); err != nil {
		t.Fatal(err)
	}

	// Persistence: the final flush must have persisted the frontier.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fs2.Close()
	rec, ok := fs2.UpdateState()
	if !ok || rec.Pts != 10000 {
		t.Fatalf("persisted pts = %+v (ok %v), want 10000", rec, ok)
	}

	// Bounded growth: goroutines settle; heap growth within documented
	// bounds (entity cache + conversation store + GC headroom).
	if err := goroutineSettled(base, 12); err != nil {
		t.Fatal(err)
	}
	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("soak heap growth: %.1f MiB", float64(growth)/(1<<20))
	if growth > 48*1024*1024 {
		t.Fatalf("heap grew %.1f MiB across the soak (unbounded growth?)",
			float64(growth)/(1<<20))
	}
}
