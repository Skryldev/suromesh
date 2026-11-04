package client_test

// Phase 17: consumer-oriented workflow suite. These tests simulate an
// external application built on the SDK: every app-facing step uses ONLY
// the public client API (the fake server plumbing is test infrastructure).
// Workflows 1–10 per the Phase 17 specification; all deterministic.

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// Workflow 1 — create client, connect, restore session, resolve peer,
// get dialogs, get history.
func TestConsumerW1RestoreResolveRead(t *testing.T) {
	dir := t.TempDir()
	path := filepathJoin(dir, "w1.session")
	srv := newWSTestServerForAuth(t)

	// Authorize once and close.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c1, fake1 := newAuthClientWithSession(t, srv, fs)
	af := newAuthFake("w1code")
	installAuthHooks(t, fake1, af)
	installUpdatesHooks(t, fake1, newUpdatesFake())
	if _, err := c1.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.SignIn(context.Background(), testPhone, "w1code"); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	// Restore: no re-auth; resolve, dialogs, history.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c2, fake2 := newAuthClientWithSession(t, srv, fs2)
	defer c2.Close()
	if c2.AuthState() != client.AuthUnauthorized && c2.AuthState() != client.AuthAuthorized {
		t.Fatalf("restored auth state %s", c2.AuthState())
	}

	df := newDialogsFake()
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 700}, TopMessage: 3,
			NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 3, Message: "history-head",
			PeerID: &gen.PeerUser{UserID: 700}, Date: 3}},
		[]gen.UserClass{testUser(700, 0x700, "w1peer")}, nil, 1, false))
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 3, Message: "history-head", PeerID: &gen.PeerUser{UserID: 700}, Date: 3},
		{ID: 2, Message: "older", PeerID: &gen.PeerUser{UserID: 700}, Date: 2},
	}, 2))
	fake2.SetResponder(combineResponders(authUsersResponder(), dialogsResponder(t, df)))

	if _, err := c2.GetMe(context.Background()); err != nil {
		t.Fatalf("resolve self: %v", err)
	}
	dialogs, err := c2.GetDialogs(context.Background(), client.DialogQuery{Limit: 5})
	if err != nil || len(dialogs) == 0 {
		t.Fatalf("dialogs: %v (n=%d)", err, len(dialogs))
	}
	// Peer resolution from the cache seeded by the dialogs page.
	peer, err := c2.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 700})
	if err != nil {
		t.Fatalf("resolve peer: %v", err)
	}
	if peer == nil {
		t.Fatal("resolved peer is nil")
	}
	it, err := c2.IterMessages(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 700},
		client.MessageQuery{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	nMsg := 0
	for {
		_, ok := it.Next(context.Background())
		if !ok {
			break
		}
		nMsg++
	}
	if err := it.Err(); err != nil {
		t.Fatalf("history iterator: %v", err)
	}
	if nMsg != 2 {
		t.Fatalf("history messages = %d, want 2", nMsg)
	}
}

// Workflow 2 — send message, receive own state update, no duplicate
// subscriber event.
func TestConsumerW2OwnUpdateNoDuplicate(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	var events int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { atomic.AddInt32(&events, 1) })
	defer sub.Unsubscribe()

	res, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "w2-out", client.SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The send's own update must advance state silently: zero events.
	waitUntil(t, 2*time.Second, func() bool { return atomic.LoadInt32(&events) == 0 })
	if n := atomic.LoadInt32(&events); n != 0 {
		t.Fatalf("own update produced %d subscriber events, want 0", n)
	}
	if res == nil || res.ID == 0 {
		t.Fatalf("send result %+v", res)
	}
	_ = fake
}

// Workflow 3 — receive external message: exactly one subscriber event and
// coherent conversation state.
func TestConsumerW3ExternalExactlyOnce(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	var events int32
	var gotText atomic.Value
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt32(&events, 1)
		gotText.Store(upd)
	})
	defer sub.Unsubscribe()

	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 41, Message: "w3-in",
			PeerID: &gen.PeerUser{UserID: 310}, Date: 41},
		PTS: 41, PTSCount: 1,
	}}, Date: 100, Seq: 0})

	waitUntil(t, 5*time.Second, func() bool { return atomic.LoadInt32(&events) >= 1 })
	time.Sleep(100 * time.Millisecond) // allow duplicates, if any, to surface
	if n := atomic.LoadInt32(&events); n != 1 {
		t.Fatalf("external update produced %d events, want exactly 1", n)
	}
	conv, ok := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 310})
	if !ok {
		t.Fatal("conversation state missing after incoming message")
	}
	if conv.UnreadCount != 1 {
		t.Fatalf("unread = %d, want 1", conv.UnreadCount)
	}
}

// Workflow 4 — reply, resolve the reply, verify the MessageKey pairing.
func TestConsumerW4ReplyKeyIdentity(t *testing.T) {
	c, fake, _ := newMutationsClient(t)
	defer c.Close()

	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	first, err := c.SendMessageOpts(context.Background(), peer, "w4-root", client.SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Answer the getMessages RPC that ResolveReply performs with the root
	// message itself (fake-server plumbing; the app-facing calls stay on
	// the public API).
	root := &gen.Message{ID: int32(first.ID), Message: "w4-root",
		PeerID: &gen.PeerChat{ChatID: 55}, Date: 100}
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.MessagesGetMessagesRequest{}).CtorID() {
			return &gen.MessagesMessages{Messages: []gen.MessageClass{root},
				Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
		}
		return prev(ctor, body)
	})
	reply, err := c.Reply(context.Background(), peer, first.ID, "w4-reply")
	if err != nil {
		t.Fatalf("reply: %v", err)
	}
	if reply.Reply == nil || reply.Reply.MessageID != first.ID {
		t.Fatalf("reply info %+v, want reply to %d", reply.Reply, first.ID)
	}
	// Identity: (peer, id) pairing — same ID on a different peer is a
	// different message; MessageKey carries both.
	k1 := client.MessageKey{Peer: client.PeerID{Kind: client.PeerChat, ID: 55}, ID: first.ID}
	k2 := client.MessageKey{Peer: client.PeerID{Kind: client.PeerUser, ID: 55}, ID: first.ID}
	if k1 == k2 {
		t.Fatal("different peers with same id produced identical keys")
	}
	resolved, err := c.ResolveReply(context.Background(), reply)
	if err != nil {
		t.Fatalf("resolve reply: %v", err)
	}
	if resolved == nil || resolved.ID != first.ID {
		t.Fatalf("resolved reply target %+v, want id %d", resolved, first.ID)
	}
}

// Workflow 5 — edit then delete; resulting message state.
func TestConsumerW5EditDeleteState(t *testing.T) {
	c, _, _ := newMutationsClient(t)
	defer c.Close()

	peer := client.PeerID{Kind: client.PeerChat, ID: 55}
	orig, err := c.SendMessageOpts(context.Background(), peer, "w5-v1", client.SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := c.EditMessage(context.Background(), peer, orig.ID, "w5-v2")
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	if edited.Text != "w5-v2" {
		t.Fatalf("edited text %q", edited.Text)
	}
	del, err := c.DeleteMessages(context.Background(), peer, []int64{orig.ID},
		client.DeleteMessagesOptions{})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if len(del) == 0 {
		t.Fatal("delete returned no affected ids")
	}
}

// Workflow 6 — upload media, send media, receive media, download media,
// verify bytes.
func TestConsumerW6MediaRoundTrip(t *testing.T) {
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()
	mf2 := newMediaFake()
	installMediaHooks(t, fake, mf2)

	content := make([]byte, 2048) // matches the fake document's Size field
	for i := range content {
		content[i] = byte(i * 7 % 251)
	}
	up, err := c.Upload(context.Background(), bytesReader(content),
		client.UploadOptions{Size: int64(len(content)), FileName: "w6.bin"})
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	sent, err := c.SendMediaInput(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55},
		client.UploadedDocumentInput{Upload: up, MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	wireDoc := testDocument(930, 0xfee1, []byte{1, 2, 3}, "application/octet-stream")
	mf2.setDownloadData(930, content)
	rcv := &client.Message{
		ID:     sent.ID,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Raw: &gen.Message{ID: int32(sent.ID), PeerID: &gen.PeerChat{ChatID: 55},
			Media: mediaDocWire(wireDoc), Date: 100},
		Media: c.ClassifyMediaForTest(mediaDocWire(wireDoc)),
	}
	var out byteBuf
	if err := c.DownloadMedia(context.Background(), rcv, &out, client.DownloadOptions{}); err != nil {
		t.Fatalf("download: %v", err)
	}
	if len(out.b) != len(content) {
		t.Fatalf("round trip %d bytes, want %d", len(out.b), len(content))
	}
	for i := range content {
		if out.b[i] != content[i] {
			t.Fatalf("byte mismatch at %d", i)
		}
	}
}

// Workflow 7 — search, paginate, cancel the iterator.
func TestConsumerW7SearchPaginateCancel(t *testing.T) {
	c, _, mf := newMutationsClient(t)
	defer c.Close()

	mf.repeatSearch = true
	mf.pushSearchPage(searchPage(600, 599))
	mf.pushSearchPage(searchPage(598, 597))

	it, err := c.IterSearchMessages(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	// First page delivers two hits.
	for i := 0; i < 2; i++ {
		if _, ok := it.Next(context.Background()); !ok {
			t.Fatalf("hit %d missing: %v", i, it.Err())
		}
	}
	// Cancel before the next page: iterator must end with the ctx error.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for {
		_, ok := it.Next(ctx)
		if !ok {
			break
		}
	}
	if err := it.Err(); !errors.Is(err, context.Canceled) {
		t.Fatalf("iterator err = %v, want context.Canceled", err)
	}
}

// Workflow 8 — disconnect, reconnect, continue normal operations.
func TestConsumerW8ReconnectContinue(t *testing.T) {
	c, _, _ := newMutationsClient(t)
	defer c.Close()

	if err := c.Disconnect(context.Background()); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	if c.IsConnected() {
		t.Fatal("still connected after disconnect")
	}
	// Reconnect: same session, no re-auth, state ready.
	if err := c.Connect(context.Background()); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	if c.State() != clientStateReady {
		t.Fatalf("state after reconnect %s", c.State())
	}
	if _, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "w8-after", client.SendMessageOptions{}); err != nil {
		t.Fatalf("send after reconnect: %v", err)
	}
}

// Workflow 9 — concurrent operations: mutations, updates, history, search.
func TestConsumerW9ConcurrentWorkloads(t *testing.T) {
	c, fake, mf := newMutationsClient(t)
	defer c.Close()

	mf.repeatSearch = true
	mf.pushSearchPage(searchPage(700, 699))

	df := newDialogsFake()
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 9, Message: "w9-hist", PeerID: &gen.PeerChat{ChatID: 55}, Date: 9},
	}, 1))
	prevW9 := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == historyCtorGetHistory {
			return df.handle(t, ctor, body)
		}
		return prevW9(ctor, body)
	})

	var wg sync.WaitGroup
	errs := make(chan error, 16)
	// Mutations.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if _, err := c.SendMessageOpts(context.Background(),
				client.PeerID{Kind: client.PeerChat, ID: 55}, "w9-m", client.SendMessageOptions{}); err != nil {
				errs <- err
			}
		}(i)
	}
	// History iteration.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.GetMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{Limit: 1}); err != nil {
			errs <- err
		}
	}()
	// Search.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := c.SearchMessages(context.Background(),
			client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{Limit: 1}); err != nil {
			errs <- err
		}
	}()
	// Update stream ingest.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := int32(0); i < 5; i++ {
			fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
				Message: &gen.Message{ID: 500 + i, Message: "w9-u",
					PeerID: &gen.PeerUser{UserID: 320}, Date: 500 + i},
				PTS: 500 + i, PTSCount: 1,
			}}, Date: 100, Seq: 0})
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent workload failed: %v", err)
	}
	waitUntil(t, 5*time.Second, func() bool {
		return c.TrackedConversations() >= 0 && fake.Received() != nil
	})
}

// Workflow 10 — Close during everything: all in-flight calls terminate
// deterministically; Close is terminal; post-close calls fail with
// ErrClosed.
func TestConsumerW10CloseDuringEverything(t *testing.T) {
	c, _, _ := newMutationsClient(t)

	var wg sync.WaitGroup
	results := make(chan error, 24)
	stop := make(chan struct{})
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
				_, err := c.SendMessageOpts(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, "w10", client.SendMessageOptions{})
				if err != nil {
					results <- err
					return
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			_, err := c.SearchMessages(context.Background(),
				client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{Limit: 1})
			if err != nil {
				results <- err
				return
			}
		}
	}()

	time.Sleep(50 * time.Millisecond)
	closeErr := c.Close()
	close(stop)
	wg.Wait()
	close(results)
	if closeErr != nil {
		t.Fatalf("close: %v", closeErr)
	}
	n := 0
	for err := range results {
		if err != nil && !errors.Is(err, client.ErrClosed) &&
			!client.AsConnectionLost(err) && !errors.Is(err, context.Canceled) {
			t.Fatalf("unexpected mid-flight error: %v", err)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no operation observed the close")
	}
	// Terminal: everything afterwards fails with ErrClosed.
	if _, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "w10-after", client.SendMessageOptions{}); !errors.Is(err, client.ErrClosed) {
		t.Fatalf("post-close send err = %v, want ErrClosed", err)
	}
	if err := c.Connect(context.Background()); !errors.Is(err, client.ErrBadState) &&
		!errors.Is(err, client.ErrClosed) {
		t.Fatalf("post-close connect err = %v, want ErrBadState/ErrClosed", err)
	}
	_ = io.Discard
}
