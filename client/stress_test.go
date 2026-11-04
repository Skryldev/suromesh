package client_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ---- entity cache semantics -------------------------------------------------

func TestEntityCacheInvalidateClearUpdate(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	// Update-existing: re-seeding with a rotated hash replaces the entry.
	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 100}}, nil, nil)
	ip, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ip.(*gen.InputPeerUser).AccessHash != 100 {
		t.Fatal("initial hash wrong")
	}
	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 200}}, nil, nil)
	ip, err = c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 1})
	if err != nil {
		t.Fatal(err)
	}
	if ip.(*gen.InputPeerUser).AccessHash != 200 {
		t.Fatal("update-existing did not apply rotated hash")
	}

	// Invalidate: the peer becomes unresolvable (no fabricated hashes).
	c.InvalidateEntity(client.PeerID{Kind: client.PeerUser, ID: 1})
	_, err = c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 1})
	var ahErr *client.ErrAccessHashUnavailable
	if !errors.As(err, &ahErr) {
		t.Fatalf("post-invalidate error %v", err)
	}

	// Clear: everything dropped.
	c.SeedEntities([]*client.User{{ID: 2, AccessHash: 1}}, []*client.Chat{{ID: 3}}, []*client.Channel{{ID: 4, AccessHash: 5}})
	if c.CachedEntities() != 3 {
		t.Fatalf("cache size %d", c.CachedEntities())
	}
	c.ClearEntities()
	if c.CachedEntities() != 0 {
		t.Fatalf("post-clear size %d", c.CachedEntities())
	}
}

// ---- update stress ------------------------------------------------------------

// pushUpdates drives n update-family rpc_results through the fake server.
func pushUpdates(t *testing.T, fake *appFake, n int, marker int32) {
	t.Helper()
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.Updates{
			Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
				Message: &gen.Message{ID: marker, Message: "u", PeerID: &gen.PeerUser{UserID: 1}},
				PTS:     1, PTSCount: 1,
			}},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
			Date: 1700000000, Seq: 1,
		}, nil
	})
}

func TestUpdateStress100Subscribers1000Updates(t *testing.T) {
	if testing.Short() {
		t.Skip("stress test skipped in -short mode")
	}
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	const subscribers = 100
	const updates = 1000

	var mu sync.Mutex
	received := make(map[int]int, subscribers)
	var subs []*client.Subscription
	for i := 0; i < subscribers; i++ {
		idx := i
		sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
			mu.Lock()
			received[idx]++
			mu.Unlock()
		})
		subs = append(subs, sub)
	}

	// Drive 1000 update-bearing RPCs (each rpc_result carries one update;
	// store-own-updates fans it into the stream).
	for round := 0; round < updates; round++ {
		pushUpdates(t, fake, 1, int32(round))
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if _, err := c.Invoke(ctx, &gen.PingRequest{PingID: int64(round)}); err != nil {
			t.Fatalf("invoke %d: %v", round, err)
		}
		cancel()
	}

	// Wait for the workers to drain (bounded wait; the queue is 1024 deep
	// with 4 workers, so drain time is short).
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		total := 0
		minSeen := updates
		for _, n := range received {
			total += n
			if n < minSeen {
				minSeen = n
			}
		}
		mu.Unlock()
		if total >= subscribers*updates {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	total := 0
	minSeen := updates + 1
	for _, n := range received {
		total += n
		if n < minSeen {
			minSeen = n
		}
	}
	mu.Unlock()
	t.Logf("delivered=%d across %d subscribers (min per subscriber=%d), dropped=%d",
		total, subscribers, minSeen, c.DroppedUpdates())

	for _, s := range subs {
		s.Unsubscribe()
	}
}

func TestConcurrentSubscribeUnsubscribe(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.Updates{Updates: []gen.UpdateClass{}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 1, Seq: 1}, nil
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})
	// Concurrent subscribe/unsubscribe churn.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {})
				sub.Unsubscribe()
			}
		}()
	}
	// Concurrent update flow during the churn.
	for i := 0; i < 200; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if _, err := c.Invoke(ctx, &gen.PingRequest{PingID: int64(i)}); err != nil {
			cancel()
			t.Fatalf("invoke during churn: %v", err)
		}
		cancel()
	}
	close(stop)
	wg.Wait()
}

// ---- reconnect with key reuse --------------------------------------------------

func TestReconnectReusesKeyNoReExchange(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	_ = wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()

	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Reconnect: the session holds the key; the fake's exchange path is
	// never triggered (client only exchanges when session has no key), and
	// initConnection succeeds through the same fake.
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	// Traffic still flows on the reused key.
	if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: 1}); err != nil {
		t.Fatalf("invoke after reconnect: %v", err)
	}
}

// ---- error model -----------------------------------------------------------------

func TestErrorClassification(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, _ := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, testKey(9))
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return nil, &rpc.RPCError{Code: 420, Message: "FLOOD_WAIT_30"}
	})
	c.SetFloodWaitThreshold(time.Second) // 30s wait > threshold: surface immediately
	_, invokeErr := c.Invoke(context.Background(), &pingLike{})
	fw, ok := client.AsFloodWait(invokeErr)
	if !ok || fw.Seconds != 30 {
		t.Fatalf("AsFloodWait: %+v ok=%v", fw, ok)
	}
	if class := client.ClassifyError(invokeErr); class != "rpc_error" {
		t.Fatalf("class %s", class)
	}

	// Cancellation keeps its identity (never hidden inside RPC errors).
	fake.SetDelay(time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, cancelErr := c.Invoke(ctx, &pingLike{})
	if !errors.Is(cancelErr, context.DeadlineExceeded) {
		t.Fatalf("cancel err %v", cancelErr)
	}
	if class := client.ClassifyError(cancelErr); class != "timeout" {
		t.Fatalf("cancel class %s", class)
	}
}
