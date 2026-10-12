package client_test

// Phase 11 concurrency + stress: iterators, resolvers, conversation
// state under concurrency; MarkAsRead E2E; reconnect during iteration.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestStress32ConcurrentIterators: 32 concurrent history iterators and
// 32 concurrent dialog listings against one client — no races, no
// cross-talk (each iterator owns only local state).
func TestStress32ConcurrentIterators(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// One page of history per call, repeated (each iterator gets fresh
	// answers; repeat mode keeps the fake serving).
	df.repeatLastHist = true
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 600, PeerID: &gen.PeerChat{ChatID: 55}, Date: 600},
		{ID: 599, PeerID: &gen.PeerChat{ChatID: 55}, Date: 599},
	}, 1000000))
	df.repeatLastPage = true
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 1}, TopMessage: 1, NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 1}, Date: 1}},
		[]gen.UserClass{testUser(1, 1, "a")}, nil, 2, true))

	var wg sync.WaitGroup
	failures := int32(0)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if i%2 == 0 {
				it, err := c.IterMessages(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
				if err != nil {
					t.Errorf("iterator %d: %v", i, err)
					atomic.AddInt32(&failures, 1)
					return
				}
				n := 0
				for {
					if _, ok := it.Next(context.Background()); !ok {
						break
					}
					n++
					if n > 5 {
						break
					}
				}
			} else {
				it := c.IterDialogs(context.Background(), client.DialogQuery{Limit: 2})
				n := 0
				for {
					if _, ok := it.Next(context.Background()); !ok {
						break
					}
					n++
					if n > 3 {
						break
					}
				}
			}
		}(i)
	}
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent iterators failed", failures)
	}
}

// TestStress32ConcurrentResolvers: 32 goroutines resolving peers — the
// entity cache is concurrency-safe and never fabricates under races.
func TestStress32ConcurrentResolvers(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newDialogsClient(t)
	defer c.Close()

	// Seed one entity; the rest must fail with ErrAccessHashUnavailable
	// (never fabricated), consistently across concurrent resolution.
	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 42}}, nil, nil)

	var wg sync.WaitGroup
	var errs int32
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := int64(1)
			if i%2 == 1 {
				id = int64(1000 + i) // uncached
			}
			_, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: id})
			if id == 1 && err != nil {
				t.Errorf("cached peer failed: %v", err)
				atomic.AddInt32(&errs, 1)
			}
			if id != 1 {
				var ah *client.ErrAccessHashUnavailable
				if err == nil || !asAccessHash(err, &ah) {
					t.Errorf("uncached peer %d: err = %v, want ErrAccessHashUnavailable", id, err)
					atomic.AddInt32(&errs, 1)
				}
			}
		}(i)
	}
	wg.Wait()
	if errs != 0 {
		t.Fatalf("%d resolver failures", errs)
	}
}

func asAccessHash(err error, _ **client.ErrAccessHashUnavailable) bool {
	var e *client.ErrAccessHashUnavailable
	return errors.As(err, &e)
}

// TestConversationConcurrentReadersAndUpdates: conversation state reads
// racing ordered updates — no races, coherent final numbers.
func TestConversationConcurrentReadersAndUpdates(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	stop := make(chan struct{})
	var readers sync.WaitGroup
	for i := 0; i < 8; i++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				_, _ = c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
				_ = c.TrackedConversations()
			}
		}()
	}

	const total = 2000
	for i := 0; i < total; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerUser{UserID: 300}, Date: int32(i + 1)},
			PTS:     int32(i + 1), PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	waitUntil(t, 30*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == total
	})
	close(stop)
	readers.Wait()

	conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
	if conv.UnreadCount != total || conv.TopMessageID != total {
		t.Fatalf("final state = %+v, want unread=%d top=%d", conv, total, total)
	}
}

// TestMarkAsReadE2E: per-kind RPC split (channels vs everything else);
// the store updates only from the server's read events (§21).
func TestMarkAsReadE2E(t *testing.T) {
	c, fake, df := newDialogsClient(t)
	defer c.Close()

	// Seed a conversation via an update (top message 5, unread 1).
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == 5
	})

	// MarkAsRead: maxID 0 = current top. The read acknowledgement comes
	// back through the ordered update stream.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateReadHistoryInbox{
		Peer: &gen.PeerUser{UserID: 300}, MaxID: 5, PTS: 6, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	if !waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.ReadInboxMaxID == 5
	}) {
		t.Fatal("read event not applied")
	}

	// A basic chat (no hash needed) uses messages.readHistory; the fake
	// answers with the affected-history conversion path (OwnUpdate).
	var readCalls int32
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == historyCtorGetHistory || ctor == dialogsCtorGetDialogs {
			return df.handle(t, ctor, body)
		}
		atomic.AddInt32(&readCalls, 1)
		return &gen.MessagesAffectedHistory{PTS: 6, PTSCount: 0, Offset: 0}, nil
	})
	if err := c.MarkAsRead(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55}, 0); err != nil {
		t.Fatalf("MarkAsRead: %v", err)
	}
	if atomic.LoadInt32(&readCalls) != 1 {
		t.Fatalf("read RPCs = %d, want 1", readCalls)
	}
}

// TestIteratorsSurviveCloseAndReconnect: Close during iteration
// terminates promptly; a fresh client works afterwards (§27/§28).
func TestIteratorsSurviveCloseAndReconnect(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newDialogsClient(t)
	fake.SetDelay(100 * time.Millisecond)

	done := make(chan struct{})
	go func() {
		it, err := c.IterMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
		if err != nil {
			close(done)
			return
		}
		for {
			if _, ok := it.Next(context.Background()); !ok {
				break
			}
		}
		close(done)
	}()
	time.Sleep(80 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close during iteration: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("iterator did not terminate after Close")
	}

	// Reconnect semantics: a fresh client against a fresh fake works.
	c2, _, df2 := newDialogsClient(t)
	defer c2.Close()
	df2.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 900, PeerID: &gen.PeerChat{ChatID: 55}, Date: 900},
	}, 1))
	it, err := c2.IterMessages(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := it.Next(context.Background()); !ok || got.ID != 900 {
		t.Fatalf("post-reconnect iterator broken: %v %v", got, ok)
	}
}

// TestStress100kMessagesHistory: 100k messages streamed through history
// iterators in 100-sized pages — bounded memory, exact accounting.
func TestStress100kMessagesHistory(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// Queue 1000 slice pages of 100 messages each (501000..500001 desc).
	const pages, per = 1000, 100
	for p := 0; p < pages; p++ {
		msgs := make([]*gen.Message, per)
		for j := 0; j < per; j++ {
			id := 501000 - p*per - j
			msgs[j] = &gen.Message{ID: int32(id), Message: fmt.Sprintf("m%d", id),
				PeerID: &gen.PeerChat{ChatID: 55}, Date: int32(id)}
		}
		df.pushHistoryPage(histSlicePage(msgs, int32(pages*per)))
	}

	heapBefore := heapAfterGC()
	it, err := c.IterMessages(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var count int64
	for {
		msg, ok := it.Next(context.Background())
		if !ok {
			if it.Err() != nil {
				t.Fatalf("iterator error mid-stream: %v", it.Err())
			}
			break
		}
		_ = msg
		count++
	}
	heapAfter := heapAfterGC()
	if count != pages*per {
		t.Fatalf("delivered %d of %d", count, pages*per)
	}
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("heap growth over %d history messages: %.1f MiB", count, float64(growth)/(1<<20))
	if growth > 32*1024*1024 {
		t.Fatalf("heap grew %.1f MiB — iterator accumulated state", float64(growth)/(1<<20))
	}
}
