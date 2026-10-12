package client_test

// Phase 11 test matrix — peer resolution, dialogs, history, conversations.
// Reference traces for every scenario live in docs/peers.md,
// docs/dialogs.md, docs/history.md, docs/conversations.md.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func ptrI32(v int32) *int32 { return &v }

// selfUserAnswer answers users.getUsers with the Phase 9 test user.
func selfUserAnswer() tl.Object {
	id := int64(7001)
	hash := int64(0xabc123)
	first := "Golden"
	return &rpc.VectorResult{Items: []tl.Object{&gen.User{
		ID: id, UserType: &gen.UserTypeNormal{}, AccessHash: &hash, FirstName: &first,
	}}}
}

// ---- peer resolution -------------------------------------------------------

func TestPeerResolveTypedRefs(t *testing.T) {
	c, fake, df := newDialogsClient(t)
	defer c.Close()

	// Existing entity passes through unchanged.
	ch := &client.Channel{ID: 42, AccessHash: 0xabc}
	p, err := c.ResolvePeer(context.Background(), ch)
	if err != nil || p != ch {
		t.Fatalf("entity passthrough: %v %v", p, err)
	}
	// Self resolution (GetMe authoritative) — the fake must answer
	// users.getUsers alongside the dialogs family.
	installAuthHooks(t, fake, newAuthFake("54321"))
	fake.SetResponder(combineResponders(
		authUsersResponder(),
		dialogsResponder(t, df),
	))
	if _, err := c.ResolvePeerString(context.Background(), "me"); err != nil {
		t.Fatalf("self resolution: %v", err)
	}
	if _, err := c.ResolvePeer(context.Background(), client.SelfRef{}); err != nil {
		t.Fatalf("SelfRef resolution: %v", err)
	}
}

// authUsersResponder answers users.getUsers with the self user.
func authUsersResponder() responderFunc {
	return func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == 0x0d91a548 { // users.getUsers (Phase 9 fake family)
			return selfUserAnswer(), nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_USERS"}
	}
}

// dialogsResponder isolates the dialogs/history family routing.
func dialogsResponder(t testing.TB, df *dialogsFake) responderFunc {
	return func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case dialogsCtorGetDialogs, historyCtorGetHistory:
			return df.handle(t, ctor, body)
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_DIALOGS"}
	}
}

// combineResponders tries each responder in order; the first that
// accepts (non-nil error meaning "not mine" is skipped) wins.
func combineResponders(rs ...responderFunc) responderFunc {
	return func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		for _, r := range rs {
			obj, rpcErr := r(ctor, body)
			if rpcErr == nil || rpcErr.Message != "NOT_USERS" && rpcErr.Message != "NOT_DIALOGS" {
				return obj, rpcErr
			}
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
	}
}

func TestPeerResolveMissingHash(t *testing.T) {
	c, _, _ := newDialogsClient(t)
	defer c.Close()

	// Unknown user/channel without a cached hash: never fabricated.
	_, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 99})
	var ah *client.ErrAccessHashUnavailable
	if !errors.As(err, &ah) {
		t.Fatalf("err = %v, want ErrAccessHashUnavailable", err)
	}
	_, err = c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerChannel, ID: 99})
	if !errors.As(err, &ah) {
		t.Fatalf("channel err = %v, want ErrAccessHashUnavailable", err)
	}
	// Chats need no hash (reference PeerChat branch).
	chat, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55})
	if err != nil {
		t.Fatalf("chat resolution: %v", err)
	}
	if c2, ok := chat.(*client.Chat); !ok || c2.ID != 55 {
		t.Fatalf("chat = %+v", chat)
	}
	// Invalid refs.
	if _, err := c.ResolvePeer(context.Background(), nil); !errors.Is(err, client.ErrInvalidPeer) {
		t.Fatalf("nil ref err = %v", err)
	}
	if _, err := c.ResolvePeer(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 0}); !errors.Is(err, client.ErrInvalidPeer) {
		t.Fatalf("zero id err = %v", err)
	}
}

func TestPeerMarkedIDs(t *testing.T) {
	// Reference utils.get_peer_id / resolve_id round trip
	// (utils.py:1004-1066): +user, -chat, -100prefix channel.
	cases := []struct {
		marked int64
		want   client.PeerID
	}{
		{123, client.PeerID{client.PeerUser, 123}},
		{-456, client.PeerID{client.PeerChat, 456}},
		{-1000000000042, client.PeerID{client.PeerChannel, 42}},
	}
	for _, tc := range cases {
		if got := client.MarkedID(tc.want); got != tc.marked {
			t.Fatalf("MarkedID(%v) = %d, want %d", tc.want, got, tc.marked)
		}
		got, ok := client.ResolveMarkedID(tc.marked)
		if !ok || got != tc.want {
			t.Fatalf("ResolveMarkedID(%d) = %v (%v), want %v", tc.marked, got, ok, tc.want)
		}
	}
}

func TestPeerSelfUsesInputPeerSelf(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return selfUserAnswer(), nil
	})
	if _, err := c.GetMe(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The self peer must address as InputPeerSelf (§8, users.py:442-444).
	ip, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 7001})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := ip.(*gen.InputPeerSelf); !ok {
		t.Fatalf("self input peer = %T, want InputPeerSelf", ip)
	}
}

// ---- dialogs ---------------------------------------------------------------

func dlgPage(dialogs []gen.DialogClass, messages []gen.MessageClass,
	users []gen.UserClass, chats []gen.ChatClass, count int32, slice bool) tl.Object {
	if slice {
		return &gen.MessagesDialogsSlice{Count: count, Dialogs: dialogs,
			Messages: messages, Users: users, Chats: chats}
	}
	return &gen.MessagesDialogs{Dialogs: dialogs, Messages: messages,
		Users: users, Chats: chats}
}

func TestDialogsSinglePageMixed(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	u := testUser(500, 0x111, "alice")
	ch := testChannel(42, 0x222)
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{
			&gen.Dialog{Peer: &gen.PeerUser{UserID: 500}, TopMessage: 3, UnreadCount: 2, ReadInboxMaxID: 1, ReadOutboxMaxID: 3, NotifySettings: &gen.PeerNotifySettings{}},
			&gen.Dialog{Peer: &gen.PeerChannel{ChannelID: 42}, TopMessage: 9, UnreadCount: 0, Pinned: true, PTS: ptrI32(77), NotifySettings: &gen.PeerNotifySettings{}},
		},
		[]gen.MessageClass{
			&gen.Message{ID: 3, Message: "hello", PeerID: &gen.PeerUser{UserID: 500}, Date: 100},
			&gen.Message{ID: 9, Message: "chan", PeerID: &gen.PeerChannel{ChannelID: 42}, Date: 200},
		},
		[]gen.UserClass{u},
		[]gen.ChatClass{ch},
		2, false,
	))

	dialogs, err := c.GetDialogs(context.Background(), client.DialogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2", len(dialogs))
	}
	if _, ok := dialogs[0].Peer.(*client.User); !ok {
		t.Fatalf("dialog 0 peer = %T", dialogs[0].Peer)
	}
	if dialogs[0].TopMessage == nil || dialogs[0].TopMessage.Text != "hello" {
		t.Fatalf("top message = %+v", dialogs[0].TopMessage)
	}
	if dialogs[0].UnreadCount != 2 || dialogs[0].ReadOutboxMaxID != 3 {
		t.Fatalf("dialog 0 numbers = %+v", dialogs[0])
	}
	if dialogs[1].ChannelPTS == nil || *dialogs[1].ChannelPTS != 77 {
		t.Fatalf("channel pts = %v", dialogs[1].ChannelPTS)
	}
	// Conversation state seeded from server numbers.
	conv, ok := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 500})
	if !ok || conv.UnreadCount != 2 || conv.TopMessageID != 3 {
		t.Fatalf("conversation = %+v (%v)", conv, ok)
	}
}

func TestDialogsPaginationTermination(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// Two slice pages where the second is smaller than requested → end.
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 1}, TopMessage: 1, NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 1}, Date: 1}},
		[]gen.UserClass{testUser(1, 1, "a")}, nil, 3, true))
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 2}, TopMessage: 2, NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 2, PeerID: &gen.PeerUser{UserID: 2}, Date: 2}},
		[]gen.UserClass{testUser(2, 2, "b")}, nil, 3, true))

	dialogs, err := c.GetDialogs(context.Background(), client.DialogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 2 {
		t.Fatalf("dialogs = %d, want 2 (second page < requested terminates)", len(dialogs))
	}
}

// TestDialogsStalledPagination: a server that keeps serving the same
// content must not spin the iterator. The reference terminates cleanly
// when a page yields nothing new (dialogs.py:97-102); the defensive
// ErrPaginationStalled backstop covers pathological continuations.
func TestDialogsStalledPagination(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	df.repeatLastPage = true
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{&gen.Dialog{Peer: &gen.PeerUser{UserID: 1}, TopMessage: 1, NotifySettings: &gen.PeerNotifySettings{}}},
		[]gen.MessageClass{&gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 1}, Date: 1}},
		[]gen.UserClass{testUser(1, 1, "a")}, nil, 1000000, true))

	it := c.IterDialogs(context.Background(), client.DialogQuery{})
	count := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := it.Next(context.Background())
		if !ok {
			break
		}
		count++
		if count > 10 {
			t.Fatal("iterator did not terminate (infinite pagination)")
		}
	}
	if count != 1 {
		t.Fatalf("saw %d dialogs, want 1", count)
	}
	if err := it.Err(); err != nil && !errors.Is(err, client.ErrPaginationStalled) {
		t.Fatalf("terminal err = %v", err)
	}
}
func TestDialogsNotModifiedTerminates(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	df.pushDialogPage(&gen.MessagesDialogsNotModified{Count: 5})
	dialogs, err := c.GetDialogs(context.Background(), client.DialogQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 0 {
		t.Fatalf("dialogs = %d, want 0", len(dialogs))
	}
}

func TestDialogsIgnoreMigrated(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	migrated := &gen.Chat{ID: 7, Title: "old", Photo: &gen.ChatPhotoEmpty{}, MigratedTo: &gen.InputChannel{ChannelID: 99, AccessHash: 5}}
	df.pushDialogPage(dlgPage(
		[]gen.DialogClass{
			&gen.Dialog{Peer: &gen.PeerChat{ChatID: 7}, TopMessage: 1, NotifySettings: &gen.PeerNotifySettings{}},
			&gen.Dialog{Peer: &gen.PeerUser{UserID: 1}, TopMessage: 2, NotifySettings: &gen.PeerNotifySettings{}},
		},
		[]gen.MessageClass{
			&gen.Message{ID: 1, PeerID: &gen.PeerChat{ChatID: 7}, Date: 1},
			&gen.Message{ID: 2, PeerID: &gen.PeerUser{UserID: 1}, Date: 2},
		},
		[]gen.UserClass{testUser(1, 1, "a")},
		[]gen.ChatClass{migrated},
		2, false))

	dialogs, err := c.GetDialogs(context.Background(), client.DialogQuery{IgnoreMigrated: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dialogs) != 1 {
		t.Fatalf("dialogs = %d, want 1 (migrated chat skipped)", len(dialogs))
	}
}

func TestDialogsContextCancel(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()
	fake.SetDelay(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	it := c.IterDialogs(ctx, client.DialogQuery{})
	_, ok := it.Next(ctx)
	if ok {
		t.Fatal("iterator produced a dialog past cancellation")
	}
	if it.Err() == nil {
		t.Fatal("iterator lost the cancellation cause")
	}
}

// ---- history ---------------------------------------------------------------

func histSlicePage(msgs []*gen.Message, count int32) tl.Object {
	mcs := make([]gen.MessageClass, len(msgs))
	for i, m := range msgs {
		mcs[i] = m
	}
	return &gen.MessagesMessagesSlice{Count: count, Messages: mcs,
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}
}

func TestHistoryOrderingAndPagination(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// Page 1: ids 10..9 (server order: newest → oldest); page 2: 8..7;
	// the final page is a non-slice answer → clean end.
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 502, Message: "a", PeerID: &gen.PeerChat{ChatID: 55}, Date: 502},
		{ID: 501, Message: "b", PeerID: &gen.PeerChat{ChatID: 55}, Date: 501},
	}, 12))
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 500, Message: "c", PeerID: &gen.PeerChat{ChatID: 55}, Date: 500},
		{ID: 499, Message: "d", PeerID: &gen.PeerChat{ChatID: 55}, Date: 499},
	}, 12))
	df.pushHistoryPage(&gen.MessagesMessages{
		Messages: []gen.MessageClass{&gen.Message{ID: 498, Message: "e", PeerID: &gen.PeerChat{ChatID: 55}, Date: 498}},
		Users:    []gen.UserClass{}, Chats: []gen.ChatClass{},
	})

	it, err := c.IterMessages(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55},
		client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for {
		m, ok := it.Next(context.Background())
		if !ok {
			break
		}
		ids = append(ids, m.ID)
	}
	want := []int64{502, 501, 500, 499, 498}
	if len(ids) != len(want) {
		t.Fatalf("ids = %v, want %v", ids, want)
	}
	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("ids = %v, want %v (ordering violated at %d)", ids, want, i)
		}
	}
}

func TestHistoryWindowEmulation(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// MaxID/MinID are emulated locally (reference messages.py:36-53): the
	// iterator stops once IDs pass the bound.
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 502, PeerID: &gen.PeerChat{ChatID: 55}, Date: 502},
		{ID: 501, PeerID: &gen.PeerChat{ChatID: 55}, Date: 501},
		{ID: 500, PeerID: &gen.PeerChat{ChatID: 55}, Date: 500},
	}, 20))
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 499, PeerID: &gen.PeerChat{ChatID: 55}, Date: 499},
		{ID: 498, PeerID: &gen.PeerChat{ChatID: 55}, Date: 498},
	}, 20))

	it, err := c.IterMessages(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55},
		client.MessageQuery{MinID: 499})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for {
		m, ok := it.Next(context.Background())
		if !ok {
			break
		}
		ids = append(ids, m.ID)
	}
	// 502..500 in range; MinID 499 is below the page (exclusive bound).
	if len(ids) != 3 || ids[0] != 502 || ids[2] != 500 {
		t.Fatalf("window ids = %v, want [502 501 500]", ids)
	}
}

func TestHistoryStalledPagination(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	// The fake always answers the same slice (repeat mode): the dedup
	// drops every replay → the iterator terminates instead of spinning
	// (reference messages.py:231-242: no fresh message → give up).
	df.repeatLastHist = true
	df.pushHistoryPage(histSlicePage([]*gen.Message{
		{ID: 500, PeerID: &gen.PeerChat{ChatID: 55}, Date: 500},
		{ID: 499, PeerID: &gen.PeerChat{ChatID: 55}, Date: 499},
	}, 1000000))

	it, err := c.IterMessages(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55},
		client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		_, ok := it.Next(context.Background())
		if !ok {
			break
		}
		count++
		if count > 10 {
			t.Fatal("iterator did not terminate (infinite pagination)")
		}
	}
	if count != 2 {
		t.Fatalf("saw %d messages, want 2", count)
	}
}

func TestHistoryNotModified(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	df.pushHistoryPage(&gen.MessagesMessagesNotModified{Count: 4})
	it, err := c.IterMessages(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55},
		client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := it.Next(context.Background()); ok {
		t.Fatal("NotModified produced messages")
	}
	if it.Err() != nil {
		t.Fatalf("NotModified is a clean end, got %v", it.Err())
	}
}

// ---- conversations ---------------------------------------------------------

func TestConversationStateFromUpdates(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	// New inbox message → unread++, top message advances.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, Message: "hi", PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	if !waitUntil(t, 5*time.Second, func() bool {
		conv, ok := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return ok && conv.UnreadCount == 1 && conv.TopMessageID == 5
	}) {
		t.Fatal("inbox message did not update conversation state")
	}

	// Read event → unread cleared, inbox max advanced (server update is
	// authoritative; MarkAsRead does no optimistic local state).
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateReadHistoryInbox{
		Peer: &gen.PeerUser{UserID: 300}, MaxID: 5, PTS: 6, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	if !waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.UnreadCount == 0 && conv.ReadInboxMaxID == 5
	}) {
		t.Fatal("read event did not update conversation state")
	}

	// Outgoing message: top advances, unread unchanged.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 6, Message: "me", PeerID: &gen.PeerUser{UserID: 300}, Out: true, Date: 6},
		PTS:     7, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	if !waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == 6 && conv.UnreadCount == 0 && conv.ReadOutboxMaxID == 6
	}) {
		t.Fatal("outgoing message handling wrong")
	}
}

func TestConversationCacheEviction(t *testing.T) {
	c, fake, _ := newDialogsClientCfg(t, func(cfg *client.Config) {
		cfg.ConversationCacheSize = 8
	})
	defer c.Close()

	// More distinct peers than the capacity → bounded store. The pts
	// advances across pushes: one account sequence carries them all.
	for i := 0; i < 20; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: int32(i + 1), PeerID: &gen.PeerUser{UserID: int64(100 + i)}, Date: int32(i + 1)},
			PTS:     int32(i + 1), PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	if !waitUntil(t, 5*time.Second, func() bool {
		return c.TrackedConversations() == 8
	}) {
		t.Fatalf("store size = %d, want bounded 8", c.TrackedConversations())
	}
}
