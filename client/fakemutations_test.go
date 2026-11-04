package client_test

// Fake mutation/search RPC family (Phase 12): deterministic answers for
// send/edit/delete/forward/pin/search, including the
// UpdateMessageID → UpdateNewMessage pairing the real server uses.

import (
	"context"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func isMutationCtor(ctor uint32) bool {
	// Route by the actual generated constructor IDs (single source of
	// truth — no hand-copied constants).
	for _, probe := range []tl.Object{
		&gen.MessagesSendMessageRequest{}, &gen.MessagesEditMessageRequest{},
		&gen.MessagesDeleteMessagesRequest{}, &gen.ChannelsDeleteMessagesRequest{},
		&gen.MessagesForwardMessagesRequest{}, &gen.MessagesSearchRequest{},
		&gen.MessagesUpdatePinnedMessageRequest{}, &gen.MessagesUnpinAllMessagesRequest{},
		&gen.MessagesSendMediaRequest{}, &gen.MessagesSendMultiMediaRequest{},
	} {
		if probe.CtorID() == ctor {
			return true
		}
	}
	return false
}

// mutationsFake scripts mutation RPC answers.
type mutationsFake struct {
	mu sync.Mutex

	// Queued answers, popped per mutation call; nil entries → default
	// Updates answer carrying the standard sent-message pairing.
	answers []tl.Object

	// Default send answer components.
	nextMsgID int32

	sendCalls, editCalls, deleteCalls, forwardCalls, pinCalls, searchCalls int
	lastSendText                                                           string
	lastReplyTo                                                            *gen.InputReplyToMessage
	lastEditID                                                             int32
	lastEditText                                                           string
	lastDeleteIDs                                                          []int32
	lastDeleteRevoke                                                       bool
	lastForwardIDs                                                         []int32
	lastSearchQ, lastSearchOffsetID                                        string
	searchPages                                                            []tl.Object
	repeatSearch                                                           bool
}

func newMutationsFake() *mutationsFake {
	return &mutationsFake{nextMsgID: 100}
}

func (f *mutationsFake) pushAnswer(o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers = append(f.answers, o)
}

func (f *mutationsFake) pushSearchPage(o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.searchPages = append(f.searchPages, o)
}

// sentUpdatesLocked builds the canonical send answer: Updates{UpdateMessageID,
// UpdateNewMessage(out=true)} pairing with the request's random id. The
// echoed message carries the DESTINATION peer and the reply header, as
// the real server does. Caller must hold f.mu.
func (f *mutationsFake) sentUpdatesLocked(randomID int64, text string, peer gen.InputPeerClass, replyTo gen.InputReplyToClass) (tl.Object, *rpc.RPCError) {
	id := f.nextMsgID
	f.nextMsgID++
	echoPeer := peerWire(peer)
	msg := &gen.Message{ID: id, Message: text, Out: true, PeerID: echoPeer, Date: 100}
	if rt, ok := replyTo.(*gen.InputReplyToMessage); ok {
		rid := rt.ReplyToMsgID
		msg.ReplyTo = &gen.MessageReplyHeader{ReplyToMsgID: &rid}
	}
	return &gen.Updates{
		Updates: []gen.UpdateClass{
			&gen.UpdateMessageID{RandomID: randomID, ID: id},
			&gen.UpdateNewMessage{Message: msg, PTS: id, PTSCount: 1},
		},
		Users: []gen.UserClass{testUser(7001, 0xabc123, "self")},
		Chats: []gen.ChatClass{},
		Date:  100, Seq: 0,
	}, nil
}

// peerWire converts an input peer to the wire Peer for echoes.
func peerWire(p gen.InputPeerClass) gen.PeerClass {
	switch v := p.(type) {
	case *gen.InputPeerChat:
		return &gen.PeerChat{ChatID: v.ChatID}
	case *gen.InputPeerUser:
		return &gen.PeerUser{UserID: v.UserID}
	case *gen.InputPeerChannel:
		return &gen.PeerChannel{ChannelID: v.ChannelID}
	}
	return &gen.PeerUser{UserID: 7001}
}

func (f *mutationsFake) handle(t testing.TB, ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch o := obj.(type) {
	case *gen.MessagesSendMessageRequest:
		f.sendCalls++
		f.lastSendText = o.Message
		if rt, ok := o.ReplyTo.(*gen.InputReplyToMessage); ok {
			f.lastReplyTo = rt
		}
		if len(f.answers) > 0 {
			a := f.answers[0]
			f.answers = f.answers[1:]
			return a, nil
		}
		return f.sentUpdatesLocked(o.RandomID, o.Message, o.Peer, o.ReplyTo)

	case *gen.MessagesEditMessageRequest:
		f.editCalls++
		f.lastEditID = o.ID
		if o.Message != nil {
			f.lastEditText = *o.Message
		}
		if len(f.answers) > 0 {
			a := f.answers[0]
			f.answers = f.answers[1:]
			return a, nil
		}
		// Default: answer with updateEditMessage carrying the edited text.
		return &gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateEditMessage{
			Message: &gen.Message{ID: o.ID, Message: derefStr(o.Message),
				PeerID: &gen.PeerUser{UserID: 7001}, Date: 100},
			PTS: 1, PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0}, nil

	case *gen.MessagesDeleteMessagesRequest:
		f.deleteCalls++
		f.lastDeleteIDs = o.ID
		f.lastDeleteRevoke = o.Revoke
		return &gen.MessagesAffectedMessages{PTS: 1, PTSCount: int32(len(o.ID))}, nil

	case *gen.ChannelsDeleteMessagesRequest:
		f.deleteCalls++
		f.lastDeleteIDs = o.ID
		return &gen.MessagesAffectedMessages{PTS: 1, PTSCount: int32(len(o.ID))}, nil

	case *gen.MessagesForwardMessagesRequest:
		f.forwardCalls++
		f.lastForwardIDs = o.ID
		if len(f.answers) > 0 {
			a := f.answers[0]
			f.answers = f.answers[1:]
			return a, nil
		}
		updates := make([]gen.UpdateClass, 0, len(o.ID)*2)
		users := []gen.UserClass{testUser(7001, 0xabc123, "self")}
		for i := range o.ID {
			rid := o.RandomID[i]
			newID := f.nextMsgID
			f.nextMsgID++
			updates = append(updates,
				&gen.UpdateMessageID{RandomID: rid, ID: newID},
				&gen.UpdateNewMessage{Message: &gen.Message{
					ID: newID, Message: "fwd", Out: true,
					PeerID: &gen.PeerUser{UserID: 7001}, Date: 100,
					FwdFrom: &gen.MessageFwdHeader{},
				}, PTS: newID, PTSCount: 1})
		}
		return &gen.Updates{Updates: updates, Users: users, Chats: []gen.ChatClass{}, Date: 100, Seq: 0}, nil

	case *gen.MessagesUpdatePinnedMessageRequest:
		f.pinCalls++
		if len(f.answers) > 0 {
			a := f.answers[0]
			f.answers = f.answers[1:]
			return a, nil
		}
		return &gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.MessageService{
				ID: o.ID, Action: &gen.MessageActionPinMessage{},
				PeerID: &gen.PeerUser{UserID: 7001}, Date: 100,
			},
			PTS: 1, PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0}, nil

	case *gen.MessagesUnpinAllMessagesRequest:
		f.pinCalls++
		return &gen.Updates{Updates: []gen.UpdateClass{}, Users: []gen.UserClass{},
			Chats: []gen.ChatClass{}, Date: 100, Seq: 0}, nil

	case *gen.MessagesSendMediaRequest:
		f.sendCalls++
		f.lastSendText = o.Message
		var rid int64
		rid = o.RandomID
		if len(f.answers) > 0 {
			a := f.answers[0]
			f.answers = f.answers[1:]
			return a, nil
		}
		return f.sentUpdatesLocked(rid, o.Message, o.Peer, o.ReplyTo)

	case *gen.MessagesSendMultiMediaRequest:
		f.forwardCalls++
		updates := make([]gen.UpdateClass, 0, len(o.MultiMedia)*2)
		for _, smc := range o.MultiMedia {
			sm, ok := smc.(*gen.InputSingleMedia)
			if !ok {
				continue
			}
			newID := f.nextMsgID
			f.nextMsgID++
			updates = append(updates,
				&gen.UpdateMessageID{RandomID: sm.RandomID, ID: newID},
				&gen.UpdateNewMessage{Message: &gen.Message{
					ID: newID, Message: sm.Message, Out: true,
					PeerID: peerWire(o.Peer), Date: 100,
				}, PTS: newID, PTSCount: 1})
		}
		return &gen.Updates{Updates: updates, Users: []gen.UserClass{},
			Chats: []gen.ChatClass{}, Date: 100, Seq: 0}, nil

	case *gen.MessagesSearchRequest:
		f.searchCalls++
		f.lastSearchQ = o.Q
		f.lastSearchOffsetID = itoaI32(o.OffsetID)
		if len(f.searchPages) == 0 {
			return &gen.MessagesMessages{Messages: []gen.MessageClass{},
				Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
		}
		p := f.searchPages[0]
		if !f.repeatSearch {
			f.searchPages = f.searchPages[1:]
		}
		return p, nil
	}
	return nil, &rpc.RPCError{Code: 400, Message: "UNKNOWN_MUTATION_METHOD"}
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func itoaI32(v int32) string {
	if v == 0 {
		return "0"
	}
	digits := ""
	neg := v < 0
	if neg {
		v = -v
	}
	for v > 0 {
		digits = string(rune('0'+v%10)) + digits
		v /= 10
	}
	if neg {
		digits = "-" + digits
	}
	return digits
}

// installMutationHooks routes the mutation family to the fake.
func installMutationHooks(t testing.TB, fake *appFake, mf *mutationsFake) {
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if isMutationCtor(ctor) {
			return mf.handle(t, ctor, body)
		}
		if prev != nil {
			return prev(ctor, body)
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
	})
}

// newMutationsClient: ready client with the mutation fake wired.
func newMutationsClient(t testing.TB) (*client.Client, *appFake, *mutationsFake) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	mf := newMutationsFake()
	installMutationHooks(t, fake, mf)
	return c, fake, mf
}
