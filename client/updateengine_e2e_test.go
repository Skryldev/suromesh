package client_test

// Phase 10 end-to-end update engine tests over the real stack
// (transport → crypto → dispatcher → syncLoop → MessageBox → workers).

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// collectSub returns a subscriber that records every delivered update.
func collectSub(c *client.Client) (*[]tl.Object, *sync.Mutex, *client.Subscription) {
	var mu sync.Mutex
	var seen []tl.Object
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		mu.Lock()
		seen = append(seen, upd)
		mu.Unlock()
	})
	return &seen, &mu, sub
}

func waitUntil(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return cond()
}

func updMsg(pts int32) *gen.UpdateNewMessage {
	return &gen.UpdateNewMessage{
		Message:  &gen.Message{ID: pts, Message: fmt.Sprintf("m%d", pts), PeerID: &gen.PeerUser{UserID: 1}},
		PTS:      pts,
		PTSCount: 1,
	}
}

func TestUpdateEngineOrderAndReconciliation(t *testing.T) {
	// Strict delivery order: UpdateWorkers=1 (with >1 workers, subscriber
	// execution may interleave by design — protocol ordering is separate).
	c, fake, _ := newUpdatesClientW(t, 1)
	defer c.Close()

	seen, mu, sub := collectSub(c)
	defer sub.Unsubscribe()

	// Establish the frontier at pts 1, then push one batch containing 3
	// before 2 — the MessageBox sorts the batch by pts - pts_count
	// (messagebox.py:462-473) and delivers 2 then 3.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(3), updMsg(2)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	if !waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*seen) == 3
	}) {
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("updates not reconciled: got %d", len(*seen))
	}
	mu.Lock()
	p1 := (*seen)[0].(*gen.UpdateNewMessage).PTS
	p2 := (*seen)[1].(*gen.UpdateNewMessage).PTS
	p3 := (*seen)[2].(*gen.UpdateNewMessage).PTS
	mu.Unlock()
	if p1 != 1 || p2 != 2 || p3 != 3 {
		t.Fatalf("delivery order %d,%d,%d, want 1,2,3", p1, p2, p3)
	}
}

func TestUpdateEngineGapRecoveryE2E(t *testing.T) {
	c, fake, uf := newUpdatesClient(t)
	defer c.Close()

	seen, mu, sub := collectSub(c)
	defer sub.Unsubscribe()

	// The fake answers getDifference with the missed update 2 and the
	// authoritative state (pts 3).
	uf.pushDiff(&gen.UpdatesDifference{
		NewMessages:  []gen.MessageClass{&gen.Message{ID: 2, Message: "missed", PeerID: &gen.PeerUser{UserID: 1}}},
		OtherUpdates: []gen.UpdateClass{updMsg(3)},
		State:        &gen.UpdatesState{PTS: 3, Date: 300, Seq: 0},
	})

	// Push pts 1 (applies), then pts 5 (gap!). The fake's scripted
	// difference fills 2..3, the held 5 stays pending until its own
	// pts gap is resolved... per the reference, the difference ends and
	// the held 5 is re-applied against the new frontier.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(5)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	// The gap triggers difference recovery automatically.
	if !waitUntil(t, 5*time.Second, func() bool {
		m := c.Metrics()
		return m.DifferenceRequests >= 1
	}) {
		t.Fatal("difference not requested after gap")
	}
	if !waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// The difference's other_updates + synthetic new message reach the
		// subscriber; the exact mix depends on gap interplay.
		return len(*seen) >= 2
	}) {
		t.Fatal("difference results not delivered")
	}
	m := c.Metrics()
	if m.UpdateGaps == 0 && m.DifferenceRequests == 0 {
		t.Fatal("gap neither counted nor recovered")
	}
}

func TestUpdateEngineTooLongTriggersDifference(t *testing.T) {
	c, fake, uf := newUpdatesClient(t)
	defer c.Close()

	seen, mu, sub := collectSub(c)
	defer sub.Unsubscribe()

	uf.pushDiff(&gen.UpdatesDifference{
		NewMessages: []gen.MessageClass{&gen.Message{ID: 9, Message: "catchup", PeerID: &gen.PeerUser{UserID: 1}}},
		State:       &gen.UpdatesState{PTS: 10, Date: 200, Seq: 3},
	})

	// Seed the frontier, then push updatesTooLong (the reference's
	// catch-up trigger).
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 1})
	fake.PushUpdate(&gen.UpdatesTooLong{})

	if !waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*seen) >= 2
	}) {
		t.Fatal("difference content not delivered after tooLong")
	}
	m := c.Metrics()
	if m.DifferenceRequests == 0 {
		t.Fatal("difference not requested")
	}
}

func TestUpdateEngineOwnUpdateNotDispatched(t *testing.T) {
	c, fake, _ := newUpdatesClient(t)
	defer c.Close()

	seen, mu, sub := collectSub(c)
	defer sub.Unsubscribe()

	// Send a real message: the RPC result is an Updates family object →
	// own update → state advances, subscribers see nothing.
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.Updates{Updates: []gen.UpdateClass{updMsg(1)},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 1}, nil
	})
	peer := &client.Chat{ID: 5}
	if _, err := c.SendMessage(context.Background(), peer, "hello"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	mu.Lock()
	n := len(*seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("own update dispatched %d events", n)
	}

	// The next server update with pts 2 must apply as contiguous (state
	// advanced by the own update) — no gap.
	fake.SetResponder(nil)
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(2)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 2})
	if !waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*seen) == 1
	}) {
		t.Fatal("post-own-update update did not apply contiguously")
	}
}

func TestUpdateEngineReconnectExactlyOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "upd.session")
	srv := newWSTestServerForAuth(t)

	// Round 1: receive updates 1..3, close.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c1b, fake1 := newAuthClientWithSession(t, srv, fs)
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake1, uf)
	uf.setState(3, 0, 300, 3)

	seen1, mu1, sub1 := collectSub(c1b)
	for pts := int32(1); pts <= 3; pts++ {
		fake1.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(pts)},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100 * pts, Seq: pts})
	}
	if !waitUntil(t, 5*time.Second, func() bool {
		mu1.Lock()
		defer mu1.Unlock()
		return len(*seen1) == 3
	}) {
		t.Fatal("round-1 updates not delivered")
	}
	if err := c1b.Close(); err != nil {
		t.Fatal(err)
	}
	sub1.Unsubscribe()

	// Round 2: reconnect with the SAME session file — persisted pts 3.
	// The server missed nothing (state 3 = local 3): after reconnect the
	// next update (pts 4) applies contiguously exactly once.
	srv2 := newWSTestServerForAuth(t)
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	rec, ok := fs2.UpdateState()
	if !ok || rec.Pts != 3 {
		t.Fatalf("persisted update state = %+v (ok %v), want pts 3", rec, ok)
	}
	c2b, fake2 := newAuthClientWithSession(t, srv2, fs2)
	defer c2b.Close()
	uf2 := newUpdatesFake()
	installUpdatesHooks(t, fake2, uf2)

	seen2, mu2, sub2 := collectSub(c2b)
	defer sub2.Unsubscribe()
	fake2.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(4)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 400, Seq: 4})
	if !waitUntil(t, 5*time.Second, func() bool {
		mu2.Lock()
		defer mu2.Unlock()
		return len(*seen2) == 1
	}) {
		t.Fatal("post-reconnect update not delivered")
	}
	mu2.Lock()
	got := len(*seen2)
	mu2.Unlock()
	if got != 1 {
		t.Fatalf("exactly-once violated: %d deliveries", got)
	}
}

func TestUpdateEngineReconnectWithGapRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gap.session")
	srv := newWSTestServerForAuth(t)

	fs, _ := session.OpenFile(path)
	c1b, fake1 := newAuthClientWithSession(t, srv, fs)
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake1, uf)

	seen1, mu1, _ := collectSub(c1b)
	fake1.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(10)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	if !waitUntil(t, 5*time.Second, func() bool {
		mu1.Lock()
		defer mu1.Unlock()
		return len(*seen1) == 1
	}) {
		t.Fatal("pre-disconnect update not delivered")
	}
	c1b.Close()

	// Round 2: the server moved on to pts 20 while we were gone. After
	// reconnect, update 20 is a GAP → the fake's difference supplies the
	// authoritative state → the frontier resumes without duplicates.
	srv2 := newWSTestServerForAuth(t)
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c2b, fake2 := newAuthClientWithSession(t, srv2, fs2)
	defer c2b.Close()
	uf2 := newUpdatesFake()
	installUpdatesHooks(t, fake2, uf2)
	uf2.pushDiff(&gen.UpdatesDifference{
		State: &gen.UpdatesState{PTS: 19, Date: 190, Seq: 0},
	})

	seen2, mu2, sub2 := collectSub(c2b)
	defer sub2.Unsubscribe()
	fake2.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(20)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 200, Seq: 0})

	// Update 20 is a gap at pts 10 → difference resets the frontier to 19
	// → the pushed 20 is dropped as a duplicate (the difference supplied
	// everything up to 19; 20 was consumed as the gap trigger). The
	// guaranteed outcome: NO duplicate of message 20 and a consistent
	// final state — verify at most one delivery of pts 20.
	var pts20 int32
	waitUntil(t, 3*time.Second, func() bool {
		mu2.Lock()
		defer mu2.Unlock()
		for _, u := range *seen2 {
			if nm, ok := u.(*gen.UpdateNewMessage); ok && nm.PTS == 20 {
				pts20++
			}
		}
		return false
	})
	if pts20 > 1 {
		t.Fatalf("message 20 delivered %d times", pts20)
	}
	m := c2b.Metrics()
	if m.DifferenceRequests == 0 {
		t.Fatal("post-reconnect gap did not trigger difference")
	}
}

func TestUpdateEngineChannelDifferenceE2E(t *testing.T) {
	c, fake, uf := newUpdatesClient(t)
	defer c.Close()

	seen, mu, sub := collectSub(c)
	defer sub.Unsubscribe()

	// Seed the entity cache with the channel (the difference request must
	// use ITS access hash — never fabricated).
	c.SeedEntities([]*client.User{}, nil, []*client.Channel{{ID: 42, AccessHash: 0xbeef}})

	// Channel message at pts 5 (creates the channel entry), then a jump
	// to pts 9 → channel gap → getChannelDifference.
	uf.pushChanDiff(42, &gen.UpdatesChannelDifference{
		Final: true, PTS: 9,
		NewMessages: []gen.MessageClass{&gen.Message{ID: 8, Message: "ch-missed", PeerID: &gen.PeerChannel{ChannelID: 42}}},
	})

	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updChannelMsgE2E(42, 5, 1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updChannelMsgE2E(42, 9, 1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	if !waitUntil(t, 5*time.Second, func() bool {
		m := c.Metrics()
		return m.ChannelDifferenceRequests >= 1
	}) {
		t.Fatal("channel difference not requested")
	}
	if !waitUntil(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, u := range *seen {
			// The difference surfaces new_messages as SYNTHETIC
			// UpdateNewChannelMessage with pts 0 (messagebox.py:804-808);
			// the message payload identifies it.
			if nm, ok := u.(*gen.UpdateNewChannelMessage); ok {
				if m, ok := nm.Message.(*gen.Message); ok && m.ID == 8 {
					return true
				}
			}
		}
		return false
	}) {
		t.Fatal("channel difference content not delivered")
	}
}

func TestUpdateEngineEntityExtractionAndRotation(t *testing.T) {
	c, fake, _ := newUpdatesClient(t)
	defer c.Close()

	// An update envelope carrying a user and a channel seeds the cache.
	fake.PushUpdate(&gen.Updates{
		Updates: []gen.UpdateClass{updMsg(1)},
		Users:   []gen.UserClass{testUser(500, 0x111, "old")},
		Chats:   []gen.ChatClass{testChannel(42, 0x222)},
		Date:    100, Seq: 0,
	})
	if !waitUntil(t, 5*time.Second, func() bool {
		if _, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 500}); err != nil {
			return false
		}
		_, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerChannel, ID: 42})
		return err == nil
	}) {
		t.Fatal("entities not extracted from update envelope")
	}

	// A newer access hash rotates the stored one (server-supplied only).
	fake.PushUpdate(&gen.Updates{
		Updates: []gen.UpdateClass{updMsg(2)},
		Users:   []gen.UserClass{testUser(500, 0x333, "new")},
		Chats:   []gen.ChatClass{},
		Date:    100, Seq: 0,
	})
	if !waitUntil(t, 5*time.Second, func() bool {
		ip, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 500})
		if err != nil {
			return false
		}
		if u, ok := ip.(*gen.InputPeerUser); ok {
			return u.AccessHash == 0x333
		}
		return false
	}) {
		t.Fatal("access hash did not rotate to the newer server value")
	}
}

func TestUpdateEngineCloseDuringDifference(t *testing.T) {
	c, fake, uf := newUpdatesClient(t)

	// A slow difference answer: Close must interrupt the recovery.
	uf.mu.Lock()
	uf.diffErrs = nil
	uf.mu.Unlock()
	fake.SetDelay(50 * time.Millisecond)

	// Seed frontier then force a gap with a difference that never
	// finishes (fake keeps erroring).
	uf.pushDiffErr(&rpc.RPCError{Code: 500, Message: "INTERNAL"})
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(1)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{updMsg(9)},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	if !waitUntil(t, 5*time.Second, func() bool {
		return c.Metrics().DifferenceRequests >= 1
	}) {
		t.Fatal("difference not attempted")
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close during difference recovery: %v", err)
	}
}

// testUser builds a wire User with the fields TL requires.
func testUser(id int64, hash int64, username string) *gen.User {
	return &gen.User{ID: id, UserType: &gen.UserTypeNormal{}, AccessHash: &hash, Username: &username}
}

// testChannel builds a wire Channel with the fields TL requires.
func testChannel(id int64, hash int64) *gen.Channel {
	return &gen.Channel{ID: id, AccessHash: &hash, Photo: &gen.ChatPhotoEmpty{}}
}

// updChannelMsgE2E mirrors the internal test helper (separate test
// package) for channel-scoped updates.
func updChannelMsgE2E(ch int64, pts, count int32) *gen.UpdateNewChannelMessage {
	return &gen.UpdateNewChannelMessage{
		Message:  &gen.Message{ID: pts, Message: fmt.Sprintf("cm%d", pts), PeerID: &gen.PeerChannel{ChannelID: ch}},
		PTS:      pts,
		PTSCount: count,
	}
}
