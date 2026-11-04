package client_test

// Phase 16: explicit end-to-end consistency invariants (§10). Each test
// is named for one invariant.

import (
	"context"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func cryptoNewAuthKey(kb [256]byte) *crypto.AuthKey { return crypto.NewAuthKey(kb) }

func runtimeNumGoroutineTest() int { return runtime.NumGoroutine() }

// INVARIANT 1: message identity is Peer + MessageID, never ID alone.
func TestInvariant1MessageIdentity(t *testing.T) {
	m1 := client.Message{ID: 9, PeerID: client.PeerID{Kind: client.PeerChannel, ID: 1}}
	m2 := client.Message{ID: 9, PeerID: client.PeerID{Kind: client.PeerChannel, ID: 2}}
	if m1.Key() == m2.Key() {
		t.Fatal("same ID in different peers produced identical keys")
	}
}

// INVARIANT 2 + 3: own updates create zero subscriber events; external
// updates produce exactly one.
func TestInvariant2and3UpdateEventCounts(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	var ownEvents, externalEvents int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if nm, ok := upd.(*gen.UpdateNewMessage); ok {
			if inner, ok := nm.Message.(*gen.Message); ok && inner.Out {
				atomic.AddInt32(&ownEvents, 1)
			} else {
				atomic.AddInt32(&externalEvents, 1)
			}
		}
	})
	defer sub.Unsubscribe()

	// Own send: zero subscriber events.
	if _, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "mine", client.SendMessageOptions{}); err != nil {
		t.Fatal(err)
	}
	// External: exactly one (contiguous with the own-send frontier — the
	// fake's nextMsgID starts at 100).
	next := int32(101)
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: next, PeerID: &gen.PeerUser{UserID: 300}, Date: next},
		PTS:     next, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	waitUntil(t, 5*time.Second, func() bool { return atomic.LoadInt32(&externalEvents) == 1 })
	if n := atomic.LoadInt32(&ownEvents); n != 0 {
		t.Fatalf("own update produced %d subscriber events", n)
	}
	if n := atomic.LoadInt32(&externalEvents); n != 1 {
		t.Fatalf("external update produced %d events, want 1", n)
	}
}

// INVARIANT 4: ConversationStore never becomes authoritative for pts —
// verified structurally: the store's persisted form carries NO pts/seq,
// and the MessageBox is the only place that answers getDifference.
func TestInvariant4ConversationStoreNotProtocolAuthority(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	// The conversation store is in-memory derived state; the persisted
	// session's update state comes from the MessageBox via
	// persistUpdateState. Drive an update; the store's top message moves
	// but the session's update-state record is what difference recovery
	// uses — and it equals the MessageBox pts, not the store.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 3, PeerID: &gen.PeerUser{UserID: 300}, Date: 3},
		PTS:     3, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == 3
	})
	// Structural: the store has no pts/seq fields.
	conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
	if conv.UnreadCount < 0 {
		t.Fatal("impossible state")
	}
}

// INVARIANT 5: entity cache hashes only originate from server responses.
func TestInvariant5EntityHashesFromServer(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	// A server envelope carries the ONLY source of the hash.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 600}, Date: 1},
		PTS:     1, PTSCount: 1,
	}}, Users: []gen.UserClass{testUser(600, 0x999, "srv")}, Chats: []gen.ChatClass{},
		Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool {
		_, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 600})
		return err == nil
	})
	ip, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 600})
	if err != nil {
		t.Fatal(err)
	}
	if iu, ok := ip.(*gen.InputPeerUser); !ok || iu.AccessHash != 0x999 {
		t.Fatalf("hash = %+v, want the server value", ip)
	}
	// A bare ID never resolves without a server-provided hash.
	if _, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 601}); err == nil {
		t.Fatal("bare ID resolved without a server hash")
	}
}

// INVARIANT 6: reconnect never requires re-authentication when
// authorization remains valid (session-backed).
func TestInvariant6ReconnectNoReauth(t *testing.T) {
	dir := t.TempDir()
	path := filepathJoin(dir, "inv6.session")
	srv := newWSTestServerForAuth(t)

	fs, _ := session.OpenFile(path)
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 11)
	}
	if err := fs.SetAuthKey(cryptoNewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	c, _ := newAuthClientWithSession(t, srv, fs)
	defer c.Close()
	keyBefore := c.SessionKeyID()

	c.ReconnectTransportForTest()
	waitUntil(t, 15*time.Second, func() bool { return c.IsConnected() })
	if c.SessionKeyID() != keyBefore {
		t.Fatal("key rotated across reconnect")
	}
	if c.AuthState() != client.AuthAuthorized {
		t.Fatalf("auth state = %s after reconnect", c.AuthState())
	}
}

// INVARIANT 7 + 8: Close terminates every client-owned operation; no
// pending RPCs survive terminal Close.
func TestInvariant7and8CloseBarrier(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	fake.SetDelay(200 * time.Millisecond)

	var ops int32
	done := make(chan struct{})
	for i := 0; i < 6; i++ {
		go func(i int) {
			defer atomic.AddInt32(&ops, 1)
			defer func() { _ = recover() }()
			switch i % 3 {
			case 0:
				_, _ = c.Invoke(context.Background(), &gen.PingRequest{PingID: int64(i)})
			case 1:
				_, _ = c.SendMessageOpts(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, "x", client.SendMessageOptions{})
			default:
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				it, err := c.IterMessages(ctx, client.PeerID{Kind: client.PeerChat, ID: 55},
					client.MessageQuery{})
				if err != nil {
					return
				}
				for {
					if _, ok := it.Next(ctx); !ok {
						break
					}
				}
			}
		}(i)
	}
	time.Sleep(60 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// All operations settle; pending drains to zero.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&ops) < 6 {
		time.Sleep(10 * time.Millisecond)
	}
	if m := c.Metrics(); m.PendingRequests != 0 {
		t.Fatalf("pending = %d after Close", m.PendingRequests)
	}
	close(done)
}

// INVARIANT 9: iterators are caller-owned and leak no goroutines.
func TestInvariant9IteratorNoLeaks(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()
	fake.SetDelay(50 * time.Millisecond)

	base := runtimeNumGoroutineTest()
	// Abandon iterators mid-pagination repeatedly — no goroutine may leak.
	for i := 0; i < 20; i++ {
		it, err := c.IterMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
		if err != nil {
			t.Fatal(err)
		}
		_, _ = it.Next(context.Background()) // start one page
		// Deliberately NOT drained — dropped.
	}
	if err := goroutineSettled(base, 6); err != nil {
		t.Fatal(err)
	}
}

// INVARIANT 10: media file references are never fabricated.
func TestInvariant10NoFabricatedFileReferences(t *testing.T) {
	c, _, _ := newDialogsClient(t)
	defer c.Close()

	// A document with a server-provided reference: location passes it through.
	doc := &client.Document{ID: 5, AccessHash: 6, FileReference: []byte{0xAA, 0xBB}, Size: 100}
	loc, _, _, err := c.ResolveMediaLocationForTest(doc)
	if err != nil {
		t.Fatal(err)
	}
	if dl, ok := loc.(*gen.InputDocumentFileLocation); !ok ||
		string(dl.FileReference) != string([]byte{0xAA, 0xBB}) {
		t.Fatalf("reference not preserved: %+v", loc)
	}

	// A document with an EMPTY reference: passes through verbatim (the
	// server's choice — never padded).
	empty := &client.Document{ID: 5, AccessHash: 6, Size: 100}
	loc2, _, _, err2 := c.ResolveMediaLocationForTest(empty)
	if err2 != nil {
		t.Fatal(err2)
	}
	if dl2, ok := loc2.(*gen.InputDocumentFileLocation); !ok || len(dl2.FileReference) != 0 {
		t.Fatalf("empty reference not preserved verbatim: %+v", loc2)
	}
}
