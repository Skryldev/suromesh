package client_test

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ---- updates ---------------------------------------------------------------

func TestUpdatesDeliveredToHandlers(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	got := make(chan tl.Object, 4)
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { got <- upd })
	defer sub.Unsubscribe()

	// Inject an unsolicited update over the REAL wire path (the reference
	// socket flow). Answering an RPC with Updates would be an own update
	// (_store_own_updates) and is deliberately not dispatched.
	fake.PushUpdate(&gen.Updates{
		Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: 1, Message: "hello", PeerID: &gen.PeerUser{UserID: 1}},
			PTS:     1, PTSCount: 1,
		}},
		Users: []gen.UserClass{},
		Chats: []gen.ChatClass{},
		Date:  1700000000,
		Seq:   1,
	})

	select {
	case upd := <-got:
		// Phase 10 contract: subscribers receive the state-reconciled
		// individual update, not the raw Updates wrapper.
		nm, ok := upd.(*gen.UpdateNewMessage)
		if !ok {
			t.Fatalf("update type %T, want UpdateNewMessage", upd)
		}
		if nm.PTS != 1 || nm.PTSCount != 1 {
			t.Fatalf("pts %d count %d", nm.PTS, nm.PTSCount)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("update not delivered")
	}
}

func TestUpdateUnsubscribe(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	var calls int64
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt64(&calls, 1)
	})
	sub.Unsubscribe()

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.Updates{Updates: []gen.UpdateClass{}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 1, Seq: 1}, nil
	})
	if _, err := c.Invoke(context.Background(), &pingLike{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(300 * time.Millisecond)
	if atomic.LoadInt64(&calls) != 0 {
		t.Fatalf("handler fired after unsubscribe: %d", calls)
	}
}

func TestUpdateHandlerPanicIsolated(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	var good int64
	c.OnUpdate(func(ctx context.Context, upd tl.Object) { panic("handler bug") })
	c.OnUpdate(func(ctx context.Context, upd tl.Object) { atomic.AddInt64(&good, 1) })

	fake.PushUpdate(&gen.Updates{
		Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: 5, Message: "x", PeerID: &gen.PeerUser{UserID: 2}},
			PTS:     1, PTSCount: 1,
		}},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 1, Seq: 1,
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt64(&good) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if atomic.LoadInt64(&good) != 1 {
		t.Fatal("panicking handler killed delivery to healthy handler")
	}
}

// ---- entity resolution -------------------------------------------------------

func TestResolveUsername(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.ContactsResolvedPeer{
			Peer:  &gen.PeerUser{UserID: 42},
			Users: []gen.UserClass{&gen.User{ID: 42, AccessHash: i64p(777), Username: strp("alice"), FirstName: strp("Alice"), UserType: &gen.UserTypeNormal{}}},
			Chats: []gen.ChatClass{},
		}, nil
	})

	p, err := c.ResolveUsername(context.Background(), "@alice")
	if err != nil {
		t.Fatal(err)
	}
	u, ok := p.(*client.User)
	if !ok || u.ID != 42 || u.AccessHash != 777 || u.Username != "alice" {
		t.Fatalf("resolved %+v", p)
	}

	// Cached: ID-only peer now resolves without the server.
	ip, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if iu, ok := ip.(*gen.InputPeerUser); !ok || iu.AccessHash != 777 {
		t.Fatalf("cached input peer %+v", ip)
	}
}

func TestAccessHashNeverFabricated(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	_, err = c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 999})
	var ahErr *client.ErrAccessHashUnavailable
	if !errors.As(err, &ahErr) {
		t.Fatalf("error %v, want ErrAccessHashUnavailable", err)
	}
}

// ---- messaging ----------------------------------------------------------------

func TestSendMessageFullUpdates(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.Updates{
			Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
				Message: &gen.Message{ID: 5, Message: "echo", Date: 1700000000,
					PeerID: &gen.PeerUser{UserID: 1}, FromID: &gen.PeerUser{UserID: 1}, Out: true},
				PTS: 1, PTSCount: 1,
			}},
			Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
			Date: 1700000000, Seq: 1,
		}, nil
	})

	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 42}}, nil, nil)
	res, err := c.SendMessage(context.Background(),
		client.PeerID{Kind: client.PeerUser, ID: 1}, "echo")
	if err != nil {
		t.Fatal(err)
	}
	if res.Message == nil || res.Message.ID != 5 || res.Message.Text != "echo" || !res.Message.Out {
		t.Fatalf("result %+v", res.Message)
	}
	if res.Message.PeerID.ID != 1 || res.Message.FromID.ID != 1 {
		t.Fatalf("peer/from: %+v", res.Message)
	}
}

func TestGetMessages(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &gen.MessagesMessages{
			Messages: []gen.MessageClass{
				&gen.Message{ID: 2, Message: "second", Date: 1700000002, PeerID: &gen.PeerUser{UserID: 1}},
				&gen.Message{ID: 1, Message: "first", Date: 1700000001, PeerID: &gen.PeerUser{UserID: 1}},
			},
			Chats: []gen.ChatClass{}, Users: []gen.UserClass{},
		}, nil
	})

	c.SeedEntities([]*client.User{{ID: 1, AccessHash: 42}}, nil, nil)
	msgs, err := c.GetMessages(context.Background(),
		client.PeerID{Kind: client.PeerUser, ID: 1}, client.MessageQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 || msgs[0].ID != 2 || msgs[1].Text != "first" {
		t.Fatalf("messages %+v", msgs)
	}
}

func TestGetMe(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return &rpc.VectorResult{Items: []tl.Object{
			&gen.User{ID: 7, AccessHash: i64p(1), Username: strp("me"), FirstName: strp("Me"), UserType: &gen.UserTypeNormal{}},
		}}, nil
	})

	me, err := c.GetMe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if me.ID != 7 || me.Username != "me" {
		t.Fatalf("me %+v", me)
	}
	// GetMe seeds the cache: self is now resolvable by ID.
	if _, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 7}); err != nil {
		t.Fatalf("self not cached after GetMe: %v", err)
	}
}

// ---- concurrency ----------------------------------------------------------------

func TestConcurrentInvokes(t *testing.T) {
	srv, err := wstestNew()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	c, key := newTestClient(t, srv.Addr)
	wireFakeAppServer(t, srv, key)
	defer c.Close()
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	const n = 200
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			res, err := c.Invoke(ctx, &gen.PingRequest{PingID: id})
			if err != nil {
				errs <- err
				return
			}
			if pong, ok := res.(*gen.Pong); !ok || pong.PingID != id {
				errs <- errors.New("correlated response mismatch")
			}
		}(int64(i))
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent invoke: %v", err)
	}
	if c.DroppedUpdates() != 0 {
		t.Fatal("unexpected update drops")
	}
}
