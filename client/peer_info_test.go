package client_test

// Phase 16 tests: metadata retrieval, full info, typing, service
// messages.

import (
	"context"
	"errors"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestGetUserMetadataRefresh: a cached user refreshes via the real
// users.getUsers path and the cache rotates to the server's values.
func TestGetUserMetadataRefresh(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	c.SeedEntities([]*client.User{{ID: 500, AccessHash: 0x111, Username: "old"}}, nil, nil)

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.UsersGetUsersRequest{}).CtorID() {
			return &rpc.VectorResult{Items: []tl.Object{
				testUser(500, 0x222, "new"),
			}}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_USERS"}
	})

	u, err := c.GetUser(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 500})
	if err != nil {
		t.Fatal(err)
	}
	if u.Username != "new" {
		t.Fatalf("username = %q, want rotated value", u.Username)
	}

	// Unknown user (no cached hash) → ErrAccessHashUnavailable.
	_, err = c.GetUser(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 999})
	var ah *client.ErrAccessHashUnavailable
	if !errors.As(err, &ah) {
		t.Fatalf("err = %v, want ErrAccessHashUnavailable", err)
	}
}

// TestGetChannelMetadataRefresh: channel refresh via channels.getChannels.
func TestGetChannelMetadataRefresh(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	c.SeedEntities(nil, nil, []*client.Channel{{ID: 42, AccessHash: 0xabc}})

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.ChannelsGetChannelsRequest{}).CtorID() {
			return &gen.MessagesChats{Chats: []gen.ChatClass{
				testChannel(42, 0xdef),
			}}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_CHANNELS"}
	})

	ch, err := c.GetChannel(context.Background(), client.PeerID{Kind: client.PeerChannel, ID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if ch.AccessHash != 0xdef {
		t.Fatalf("hash not rotated: %x", ch.AccessHash)
	}
}

func TestGetChatBasicGroup(t *testing.T) {
	c, _, _ := newDialogsClient(t)
	defer c.Close()
	chat, err := c.GetChat(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55})
	if err != nil {
		t.Fatal(err)
	}
	if chat.ID != 55 {
		t.Fatalf("chat = %+v", chat)
	}
}

// TestGetUserFullE2E: users.getFullUser with typed field extraction.
func TestGetUserFullE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	c.SeedEntities([]*client.User{{ID: 500, AccessHash: 0x111}}, nil, nil)
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.UsersGetFullUserRequest{}).CtorID() {
			about := "bio text"
			pinned := int32(5)
			return &gen.UsersUserFull{
				FullUser: &gen.UserFull{
					ID:               500,
					About:            &about,
					PinnedMsgID:      &pinned,
					CommonChatsCount: 3,
					CanPinMessage:    true,
					Settings:         &gen.PeerSettings{},
					NotifySettings:   &gen.PeerNotifySettings{},
				},
				Users: []gen.UserClass{testUser(500, 0x111, "alice")},
				Chats: []gen.ChatClass{},
			}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_FULL"}
	})

	full, err := c.GetUserFull(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 500})
	if err != nil {
		t.Fatal(err)
	}
	if full.About != "bio text" || full.PinnedMessageID != 5 || full.CommonChatsCount != 3 || !full.CanPinMessage {
		t.Fatalf("full = %+v", full)
	}
	if full.User == nil || full.User.ID != 500 {
		t.Fatalf("full.User = %+v", full.User)
	}
}

// TestGetChannelFullE2E.
func TestGetChannelFullE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()
	c.SeedEntities(nil, nil, []*client.Channel{{ID: 42, AccessHash: 0xabc}})

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.ChannelsGetFullChannelRequest{}).CtorID() {
			admins := int32(4)
			return &gen.MessagesChatFull{
				FullChat: &gen.ChannelFull{
					ID:                  42,
					About:               "channel about",
					AdminsCount:         &admins,
					CanViewParticipants: true,
					ChatPhoto:           &gen.ChatPhotoEmpty{},
					NotifySettings:      &gen.PeerNotifySettings{},
				},
				Chats: []gen.ChatClass{testChannel(42, 0xdef)},
				Users: []gen.UserClass{},
			}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_FULL"}
	})

	full, err := c.GetChannelFull(context.Background(), client.PeerID{Kind: client.PeerChannel, ID: 42})
	if err != nil {
		t.Fatal(err)
	}
	if full.About != "channel about" || full.AdminsCount != 4 || !full.CanViewParticipants {
		t.Fatalf("full = %+v", full)
	}
}

// TestGetChatFullE2E.
func TestGetChatFullE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.MessagesGetFullChatRequest{}).CtorID() {
			return &gen.MessagesChatFull{
				FullChat: &gen.ChatFull{
					ID:             55,
					About:          "group about",
					NotifySettings: &gen.PeerNotifySettings{},
					Participants: &gen.ChatParticipants{
						ChatID: 55,
						Participants: []gen.ChatParticipantClass{
							&gen.ChatParticipant{UserID: 1, InviterID: 1, Date: 1},
							&gen.ChatParticipant{UserID: 2, InviterID: 1, Date: 2},
						},
					},
				},
				Chats: []gen.ChatClass{}, Users: []gen.UserClass{},
			}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_FULL"}
	})

	full, err := c.GetChatFull(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55})
	if err != nil {
		t.Fatal(err)
	}
	if full.About != "group about" || full.ParticipantsCount != 2 {
		t.Fatalf("full = %+v", full)
	}
}

// TestSetTypingE2E: messages.setTyping rides the real wire.
func TestSetTypingE2E(t *testing.T) {
	c, fake, _ := newDialogsClient(t)
	defer c.Close()

	var seen bool
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if ctor == (&gen.MessagesSetTypingRequest{}).CtorID() {
			seen = true
			return &gen.BoolTrue{}, nil
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_TYPING"}
	})
	if err := c.SetTyping(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55}); err != nil {
		t.Fatal(err)
	}
	if !seen {
		t.Fatal("setTyping never reached the wire")
	}
}

// TestServiceMessageRepresentation: a MessageService payload converts
// with IsService/Service populated and survives the history digest.
func TestServiceMessageRepresentation(t *testing.T) {
	c, _, df := newDialogsClient(t)
	defer c.Close()

	svc := &gen.MessageService{
		ID: 7, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		Action: &gen.MessageActionPinMessage{},
	}
	df.pushHistoryPage(&gen.MessagesMessages{
		Messages: []gen.MessageClass{
			&gen.Message{ID: 8, PeerID: &gen.PeerChat{ChatID: 55}, Date: 8},
			svc,
		},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
	})

	it, err := c.IterMessages(context.Background(), client.PeerID{Kind: client.PeerChat, ID: 55},
		client.MessageQuery{})
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	var sawService bool
	for {
		m, ok := it.Next(context.Background())
		if !ok {
			break
		}
		ids = append(ids, m.ID)
		if m.IsService {
			sawService = true
			if m.Service == nil || !m.Service.Pinned {
				t.Fatalf("service info = %+v", m.Service)
			}
		}
	}
	if len(ids) != 2 || !sawService {
		t.Fatalf("history = %v (service seen: %v), want both messages with service", ids, sawService)
	}
}
