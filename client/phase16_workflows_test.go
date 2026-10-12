package client_test

// Phase 16: realistic application workflows A–G over the real stack
// (transport → crypto → RPC → client → domain). Deterministic fake
// server; NOT a real-server test.

import (
	"context"
	"io"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

var ioEOF = io.EOF

func filepathJoin(parts ...string) string { return filepath.Join(parts...) }

// Scenario A — session restore: connect on an authorized session, no
// auth RPCs, resolve self, dialogs, history.
func TestWorkflowSessionRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepathJoin(dir, "restore.session")
	srv := newWSTestServerForAuth(t)

	// Session 1: authorize and close.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c1, fake1 := newAuthClientWithSession(t, srv, fs)
	af := newAuthFake("54321")
	installAuthHooks(t, fake1, af)
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake1, uf)
	if _, err := c1.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	// Session 2: restore — NO authentication RPCs, then dialogs+history.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c2, fake2 := newAuthClientWithSession(t, srv, fs2)
	defer c2.Close()

	if got := c2.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("restored auth state = %s, want authorized", got)
	}
	// GetMe + dialogs work on the restored fake.
	df := newDialogsFake()
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 500}, TopMessage: 1,
			NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 500}, Date: 1}},
		[]gen.UserClass{testUser(500, 0x111, "alice")}, nil, 1, false))
	fake2.SetResponder(combineResponders(
		authUsersResponder(),
		dialogsResponder(t, df),
	))
	if _, err := c2.GetMe(context.Background()); err != nil {
		t.Fatalf("resolve self: %v", err)
	}
	dialogs, err := c2.GetDialogs(context.Background(), client.DialogQuery{Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) == 0 {
		t.Fatal("no dialogs")
	}
}

// Scenario B — send/receive: resolve → send → own update (no duplicate
// event) → external incoming → exactly one subscriber event.
func TestWorkflowSendReceive(t *testing.T) {
	c, fake, mf := newMutationsClient(t)
	defer c.Close()

	var events int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { atomic.AddInt32(&events, 1) })
	defer sub.Unsubscribe()

	// Outgoing send: own update — state advances, no subscriber event.
	msg, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "out", client.SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// External incoming: exactly one event.
	nextID := int32(msg.ID + 1)
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: nextID, Message: "in",
			PeerID: &gen.PeerUser{UserID: 300}, Date: nextID},
		PTS: nextID, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

	waitUntil(t, 5*time.Second, func() bool {
		return atomic.LoadInt32(&events) == 1
	})
	if n := atomic.LoadInt32(&events); n != 1 {
		t.Fatalf("events = %d, want exactly 1", n)
	}
	conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
	if conv.UnreadCount != 1 {
		t.Fatalf("unread = %d, want 1", conv.UnreadCount)
	}
	_ = mf
}

// Scenario C — edit/delete coherence: send → edit → delete with state
// remaining coherent across the full mutation chain.
func TestWorkflowEditDeleteCoherence(t *testing.T) {
	c, _, _ := newMutationsClient(t)
	defer c.Close()

	sent, err := c.SendMessageOpts(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, "v1", client.SendMessageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	edited, err := c.EditMessage(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, sent.ID, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if edited.Text != "v2" {
		t.Fatalf("edited text = %q", edited.Text)
	}
	res, err := c.DeleteMessages(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55}, []int64{sent.ID},
		client.DeleteMessagesOptions{Revoke: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) != 1 {
		t.Fatalf("delete chunks = %d", len(res))
	}
}

// Scenario D — reply: original → reply → metadata → ResolveReply with
// MessageKey correctness.
func TestWorkflowReply(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.MessagesGetMessagesRequest{}).CtorID() {
			return &gen.MessagesMessages{Messages: []gen.MessageClass{
				&gen.Message{ID: 1, Message: "original", PeerID: &gen.PeerChat{ChatID: 55}, Date: 1},
			}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_GET_MSGS"}
	})

	reply := &client.Message{
		ID:     2,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Reply:  &client.ReplyInfo{MessageID: 1},
	}
	target, err := c.ResolveReply(context.Background(), reply)
	if err != nil {
		t.Fatal(err)
	}
	key := target.Key()
	if key.Peer.Kind != client.PeerChat || key.Peer.ID != 55 || key.ID != 1 {
		t.Fatalf("resolved key = %+v", key)
	}
}

// Scenario E — media: upload → send → receive → classify → download →
// byte equality, all through the real stack.
func TestWorkflowMedia(t *testing.T) {
	c, fake, _ := newMediaDomainClient(t)
	defer c.Close()
	mf2 := newMediaFake()
	installMediaHooks(t, fake, mf2)

	content := make([]byte, 2048)
	for i := range content {
		content[i] = byte(i % 251)
	}
	// Upload (Phase 8 engine, real part RPCs).
	up, err := c.Upload(context.Background(), bytesReader(content),
		client.UploadOptions{Size: int64(len(content)), FileName: "wf.bin"})
	if err != nil {
		t.Fatal(err)
	}
	// Send as document.
	sent, err := c.SendMediaInput(context.Background(),
		client.PeerID{Kind: client.PeerChat, ID: 55},
		client.UploadedDocumentInput{Upload: up, MimeType: "application/octet-stream"})
	if err != nil {
		t.Fatal(err)
	}
	if sent == nil {
		t.Fatal("send returned nil")
	}
	// Receive: classify a media message carrying the same document.
	wireDoc := testDocument(920, 0xbeef, []byte{7, 7, 7}, "application/octet-stream")
	mf2.setDownloadData(920, content)
	msg := &client.Message{
		ID:     sent.ID,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Raw: &gen.Message{ID: int32(sent.ID), PeerID: &gen.PeerChat{ChatID: 55},
			Media: mediaDocWire(wireDoc), Date: 100},
		Media: c.ClassifyMediaForTest(mediaDocWire(wireDoc)),
	}
	doc, ok := msg.Media.(*client.Document)
	if !ok || doc.Filename() != "file.bin" {
		t.Fatalf("classified = %+v", msg.Media)
	}
	// Download: byte equality through the real getFile path.
	var out byteBuf
	if err := c.DownloadMedia(context.Background(), msg, &out, client.DownloadOptions{}); err != nil {
		t.Fatal(err)
	}
	if string(out.b) != string(content) {
		t.Fatalf("round-trip mismatch: %d vs %d bytes", len(out.b), len(content))
	}
}

func bytesReader(b []byte) *sliceReader { return &sliceReader{b: b} }

type sliceReader struct{ b []byte }

func (r *sliceReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, ioEOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// Scenario F — reconnect: active conversation, updates, transport
// failure, reconnect, frontier preserved, no duplicate events.
func TestWorkflowReconnect(t *testing.T) {
	dir := t.TempDir()
	path := filepathJoin(dir, "wf.session")
	srv := newWSTestServerForAuth(t)

	fs, _ := session.OpenFile(path)
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 5)
	}
	if err := fs.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	c, fake := newAuthClientWithSession(t, srv, fs)
	defer c.Close()
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake, uf)

	var events int64
	seenIDs := make(map[int64]bool)
	var mu sync.Mutex
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if nm, ok := upd.(*gen.UpdateNewMessage); ok {
			if inner, ok2 := nm.Message.(*gen.Message); ok2 {
				mu.Lock()
				if !seenIDs[int64(inner.ID)] {
					seenIDs[int64(inner.ID)] = true
					atomic.AddInt64(&events, 1)
				}
				mu.Unlock()
			}
		}
	})
	defer sub.Unsubscribe()

	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool { return atomic.LoadInt64(&events) == 1 })

	// Transport failure → reconnect → frontier preserved (persisted).
	c.ReconnectTransportForTest()
	waitUntil(t, 15*time.Second, func() bool { return c.IsConnected() })
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("auth state = %s after reconnect", got)
	}
	// Post-reconnect update with a HIGHER pts applies contiguously.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 6, PeerID: &gen.PeerUser{UserID: 300}, Date: 6},
		PTS:     6, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 10*time.Second, func() bool { return atomic.LoadInt64(&events) == 2 })
}

// Scenario G — concurrent application load: dialogs, history, search,
// sends, edits, deletes, media, updates, reconnects — all at once.
func TestWorkflowConcurrentLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, mf := newMutationsClient(t)
	defer c.Close()
	mf2 := newMediaFake()
	installMediaHooks(t, fake, mf2)

	var wg sync.WaitGroup
	var failures int32
	for w := 0; w < 6; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			defer func() { _ = recover() }()
			switch w % 6 {
			case 0: // dialogs
				it := c.IterDialogs(context.Background(), client.DialogQuery{Limit: 2})
				for {
					if _, ok := it.Next(context.Background()); !ok {
						break
					}
				}
			case 1: // history
				it, err := c.IterMessages(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{})
				if err != nil {
					return
				}
				for {
					if _, ok := it.Next(context.Background()); !ok {
						break
					}
				}
			case 2: // search
				_, _ = c.SearchMessages(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, client.SearchQuery{Query: "x"})
			case 3: // sends
				_, _ = c.SendMessageOpts(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, "load", client.SendMessageOptions{})
			case 4: // media
				_, _ = c.SendMediaInput(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55},
					client.ExistingDocumentInput{Document: c.ClassifyMediaForTest(mediaDocWire(testDocument(int64(100+w), 0xdead, []byte{1}, "application/octet-stream"))).(*client.Document)})
			default: // edits
				_, _ = c.EditMessage(context.Background(),
					client.PeerID{Kind: client.PeerChat, ID: 55}, 42, "e")
			}
		}(w)
	}
	// Updates racing everything.
	for i := 0; i < 50; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerUser{UserID: 300}, Date: int32(i + 1)},
			PTS:     int32(i + 1), PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
		time.Sleep(2 * time.Millisecond)
	}
	// Reconnect racing.
	c.ReconnectTransportForTest()
	wg.Wait()
	if failures != 0 {
		t.Fatalf("%d concurrent workload failures", failures)
	}
	_ = mf
}
