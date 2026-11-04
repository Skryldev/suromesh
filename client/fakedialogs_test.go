package client_test

// Fake Soroush dialogs/history server (Phase 11): scripts
// messages.getDialogs and messages.getHistory with deterministic page
// queues, including pagination edge cases (stalled pages, NotModified,
// duplicate pages).

import (
	"context"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

const (
	dialogsCtorGetDialogs = 0xa0f4cb4f
	historyCtorGetHistory = 0x4423e6c5
	channelsCtorReadHist  = 0xcc104937
	messagesCtorReadHist  = 0xbb8b531e // messages.readHistory (unused value; routed by kind)
)

// dialogsFake scripts dialog and history listings.
type dialogsFake struct {
	mu sync.Mutex

	// Dialog pages, popped in order.
	dialogPages    []tl.Object
	repeatLastPage bool // serve the last page forever (stall scenarios)
	getCalls       int
	lastReq        *gen.MessagesGetDialogsRequest

	// History answers per peer kind: queue of responses.
	historyPages   []tl.Object
	repeatLastHist bool // serve the last history page forever
	histCalls      int
	lastHistReq    *gen.MessagesGetHistoryRequest
}

func newDialogsFake() *dialogsFake { return &dialogsFake{} }

func (f *dialogsFake) pushDialogPage(o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dialogPages = append(f.dialogPages, o)
}

func (f *dialogsFake) pushHistoryPage(o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.historyPages = append(f.historyPages, o)
}

func (f *dialogsFake) handle(t testing.TB, ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch ctor {
	case dialogsCtorGetDialogs:
		f.getCalls++
		if req, ok := obj.(*gen.MessagesGetDialogsRequest); ok {
			f.lastReq = req
		}
		if len(f.dialogPages) == 0 {
			return &gen.MessagesDialogs{Dialogs: []gen.DialogClass{},
				Messages: []gen.MessageClass{}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
		}
		p := f.dialogPages[0]
		if !f.repeatLastPage {
			f.dialogPages = f.dialogPages[1:]
		}
		return p, nil

	case historyCtorGetHistory:
		f.histCalls++
		if req, ok := obj.(*gen.MessagesGetHistoryRequest); ok {
			f.lastHistReq = req
		}
		if len(f.historyPages) == 0 {
			return &gen.MessagesMessages{Messages: []gen.MessageClass{},
				Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
		}
		p := f.historyPages[0]
		if !f.repeatLastHist {
			f.historyPages = f.historyPages[1:]
		}
		return p, nil
	}
	return nil, &rpc.RPCError{Code: 400, Message: "UNKNOWN_DIALOGS_METHOD"}
}

// installDialogsHooks routes the dialogs/history family to the fake.
func installDialogsHooks(t testing.TB, fake *appFake, df *dialogsFake) {
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case dialogsCtorGetDialogs, historyCtorGetHistory:
			return df.handle(t, ctor, body)
		}
		if prev != nil {
			return prev(ctor, body)
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
	})
}

// newDialogsClient: ready client with the dialogs/history fake wired.
func newDialogsClient(t testing.TB) (*client.Client, *appFake, *dialogsFake) {
	t.Helper()
	return newDialogsClientCfg(t, nil)
}

// newDialogsClientCfg is newDialogsClient with a Config mutator.
func newDialogsClientCfg(t testing.TB, mut func(*client.Config)) (*client.Client, *appFake, *dialogsFake) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	key := testAuthKeyOf(9)
	cfg := client.Config{
		Session:    session.NewWithKey(key),
		Host:       srv.Addr,
		TLSEnabled: boolPtr(false),
	}
	if mut != nil {
		mut(&cfg)
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	df := newDialogsFake()
	installDialogsHooks(t, fake, df)
	return c, fake, df
}
