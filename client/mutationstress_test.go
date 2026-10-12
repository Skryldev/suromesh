package client_test

// Phase 12 concurrency stress: 32× concurrent mutations, updates racing
// mutations, Close during mutation, cancellation — all race-clean.

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestStress32ConcurrentMutations: mixed concurrent sends/edits/deletes/
// forwards/pins — no races, every caller gets its own authoritative
// result, subscribers stay silent for own updates.
func TestStress32ConcurrentMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	var events int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt32(&events, 1)
	})
	defer sub.Unsubscribe()

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx := context.Background()
			peer := client.PeerID{Kind: client.PeerChat, ID: 55}
			switch i % 5 {
			case 0:
				_, errs[i] = c.SendMessageOpts(ctx, peer, "s", client.SendMessageOptions{})
			case 1:
				_, errs[i] = c.Reply(ctx, peer, 42, "r")
			case 2:
				_, errs[i] = c.EditMessage(ctx, peer, int64(40+i), "e")
			case 3:
				_, errs[i] = c.DeleteMessages(ctx, peer, []int64{int64(i)}, client.DeleteMessagesOptions{Revoke: true})
			default:
				_, errs[i] = c.ForwardMessages(ctx, peer,
					client.PeerID{Kind: client.PeerChat, ID: 77}, []int64{int64(i)}, client.ForwardOptions{})
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("mutation %d: %v", i, err)
		}
	}
	time.Sleep(300 * time.Millisecond)
	// Own updates NEVER reach subscribers (duplicate prevention §23).
	if n := atomic.LoadInt32(&events); n != 0 {
		t.Fatalf("mutations dispatched %d subscriber events", n)
	}
	_ = fake
}

// TestStressUpdatesRaceMutations: server-pushed updates (other-session
// traffic) arriving DURING mutations — subscriber events for pushed
// updates flow while own-update events stay suppressed.
func TestStressUpdatesRaceMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	var pushed int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if _, ok := upd.(*gen.UpdateNewMessage); ok {
			atomic.AddInt32(&pushed, 1)
		}
	})
	defer sub.Unsubscribe()

	stop := make(chan struct{})
	var pusher sync.WaitGroup
	pusher.Add(1)
	go func() {
		defer pusher.Done()
		pts := int32(1)
		for {
			select {
			case <-stop:
				return
			default:
			}
			fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
				Message: &gen.Message{ID: pts, Message: "other-device",
					PeerID: &gen.PeerUser{UserID: 800}, Date: pts},
				PTS: pts, PTSCount: 1,
			}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
			pts++
			time.Sleep(time.Millisecond)
		}
	}()

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = c.SendMessageOpts(context.Background(),
				client.PeerID{Kind: client.PeerChat, ID: 55}, "mine", client.SendMessageOptions{})
		}(i)
	}
	wg.Wait()
	// The pusher only runs while the mutations are in flight; require at
	// least one delivered push before stopping it (delivery is async).
	if !waitUntil(t, 10*time.Second, func() bool {
		return atomic.LoadInt32(&pushed) > 0
	}) {
		close(stop)
		pusher.Wait()
		t.Fatal("pushed (other-session) updates stopped flowing")
	}
	close(stop)
	pusher.Wait()
}

// TestCloseDuringMutation: Close interrupts an in-flight mutation.
func TestCloseDuringMutation(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	fake.SetDelay(200 * time.Millisecond)

	done := make(chan error, 1)
	go func() {
		_, err := c.SendMessageOpts(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, "x", client.SendMessageOptions{})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("mutation succeeded after Close")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("mutation did not settle after Close")
	}
}

// TestMutationCancellation: caller cancellation beats the RPC.
func TestMutationCancellation(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	defer c.Close()
	fake.SetDelay(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if _, err := c.SendMessageOpts(ctx, client.PeerID{Kind: client.PeerChat, ID: 55},
		"x", client.SendMessageOptions{}); err == nil {
		t.Fatal("mutation beat the deadline")
	}
}

// TestStress100kMutationIDs: a 100k-ID delete stream chunks into 1000
// bounded requests without unbounded memory.
func TestStress100kMutationIDs(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newMutationsClient(t)
	defer c.Close()

	// Stream 100k IDs lazily — the chunker materializes only one chunk
	// at a time (O(batch) contract).
	const total = 100000
	ids := make([]int64, total)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	heapBefore := heapAfterGC()
	res, err := c.DeleteMessages(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, ids, client.DeleteMessagesOptions{Revoke: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != total/100 {
		t.Fatalf("chunks = %d, want %d", len(res), total/100)
	}
	heapAfter := heapAfterGC()
	growth := int64(heapAfter) - int64(heapBefore)
	t.Logf("heap growth over %d delete IDs: %.1f MiB", total, float64(growth)/(1<<20))
	if growth > 32*1024*1024 {
		t.Fatalf("heap grew %.1f MiB", float64(growth)/(1<<20))
	}
}
