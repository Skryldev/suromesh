package client_test

// Phase 14 E2E tests over the real stack: reply resolution,
// global-search attribution, read-state predicates via updates.

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

// TestReplyResolutionE2E: a message with reply metadata resolves its
// target through the real getMessages path (reference _IDsIter).
func TestReplyResolutionE2E(t *testing.T) {
	c, fake, mf := newDialogsClient(t)
	defer c.Close()

	// The fake answers messages.getMessages with the target message.
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case (&gen.MessagesGetMessagesRequest{}).CtorID():
			obj, err := tl.Unmarshal(body)
			if err != nil {
				return nil, &rpc.RPCError{Code: 400, Message: "BAD"}
			}
			req := obj.(*gen.MessagesGetMessagesRequest)
			for _, im := range req.ID {
				if idm, ok := im.(*gen.InputMessageID); ok && idm.ID == 1 {
					return &gen.MessagesMessages{Messages: []gen.MessageClass{
						&gen.Message{ID: 1, Message: "the target", PeerID: &gen.PeerChat{ChatID: 55}, Date: 1},
					}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}}, nil
				}
			}
			return &gen.MessagesMessagesNotModified{Count: 0}, nil
		}
		return mf.handle(t, ctor, body)
	})

	// A reply message (metadata only — no target locally).
	reply := &client.Message{
		ID:     2,
		PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Reply:  &client.ReplyInfo{MessageID: 1},
	}
	target, err := c.ResolveReply(context.Background(), reply)
	if err != nil {
		t.Fatal(err)
	}
	if target.ID != 1 || target.Text != "the target" {
		t.Fatalf("resolved = %+v", target)
	}

	// Unknown target: server says NotModified → ErrReplyUnavailable.
	reply2 := &client.Message{ID: 3, PeerID: client.PeerID{Kind: client.PeerChat, ID: 55},
		Reply: &client.ReplyInfo{MessageID: 999}}
	if _, err := c.ResolveReply(context.Background(), reply2); !errors.Is(err, client.ErrReplyUnavailable) {
		t.Fatalf("err = %v, want ErrReplyUnavailable", err)
	}
}

// TestGlobalSearchE2E: cross-peer results attribute correctly over the
// real wire (duplicate IDs across peers → distinct keys).
func TestGlobalSearchE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	page1 := &gen.MessagesMessagesSlice{
		Count: 2,
		Messages: []gen.MessageClass{
			&gen.Message{ID: 9, Message: "from user", PeerID: &gen.PeerUser{UserID: 500}, Date: 9},
			&gen.Message{ID: 9, Message: "from channel", PeerID: &gen.PeerChannel{ChannelID: 42}, Date: 9},
		},
		Users: []gen.UserClass{testUser(500, 0x111, "alice")},
		Chats: []gen.ChatClass{},
	}
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.MessagesSearchGlobalRequest{}).CtorID() {
			return page1, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_GLOBAL"}
	})

	out, err := c.SearchGlobal(context.Background(), client.GlobalSearchQuery{Query: "x", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) != 2 {
		t.Fatalf("results = %d, want 2", len(out))
	}
	// Attribution: same message ID, DIFFERENT peers → distinct keys.
	if out[0].Key == out[1].Key {
		t.Fatal("duplicate ids across peers were attributed identically")
	}
	if out[0].Key.Peer.Kind != client.PeerUser || out[1].Key.Peer.Kind != client.PeerChannel {
		t.Fatalf("attribution = %v / %v", out[0].Key, out[1].Key)
	}
	// The envelope user is cached (attribution carries entity context).
	if _, err := c.InputPeerFor(context.Background(),
		client.PeerID{Kind: client.PeerUser, ID: 500}); err != nil {
		t.Fatalf("envelope entity not seeded: %v", err)
	}
}

// TestReadStateViaUpdatesE2E: inbox read maxima flow through ordered
// updates; IsMessageRead reflects the max-id rule.
func TestReadStateViaUpdatesE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	// Incoming messages 1..3.
	for i := int32(1); i <= 3; i++ {
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: i, Message: "m", PeerID: &gen.PeerUser{UserID: 300}, Date: i},
			PTS:     i, PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	}
	waitUntil(t, 5*time.Second, func() bool {
		conv, _ := c.Conversation(client.PeerID{Kind: client.PeerUser, ID: 300})
		return conv.TopMessageID == 3
	})

	// Messages 1..2 are NOT read (no read update yet).
	if c.IsMessageRead(client.PeerID{Kind: client.PeerUser, ID: 300}, 2) {
		t.Fatal("unread message reported read before the read update")
	}

	// Read update covers everything up to 2.
	fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateReadHistoryInbox{
		Peer: &gen.PeerUser{UserID: 300}, MaxID: 2, PTS: 4, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool {
		return c.IsMessageRead(client.PeerID{Kind: client.PeerUser, ID: 300}, 2)
	})
	// Message 3 is still unread (beyond the max id).
	if c.IsMessageRead(client.PeerID{Kind: client.PeerUser, ID: 300}, 3) {
		t.Fatal("message 3 reported read before its read update")
	}
}
