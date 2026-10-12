package client

// Peer metadata retrieval (Phase 16): network refresh of user/chat/
// channel metadata plus full-info variants, ported from the reference:
//
//	get_entity (network path)   users.py:239-320
//	users.getUsers fallback     users.py:463-474
//	channels.getChannels        users.py:477-483
//
// Rules: access hashes come from the cache (server-provided) — never
// fabricated. Responses seed the entity cache BEFORE conversion.

import (
	"context"
	"fmt"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl/gen"
)

// GetUser refreshes a user's metadata from the server via
// users.getUsers (reference get_entity network path). The access hash
// MUST already be known (cached from a prior server response); bare IDs
// fail with ErrAccessHashUnavailable — never fabricated.
func (c *Client) GetUser(ctx context.Context, ref PeerRef) (*User, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	user, ok := resolved.(*User)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a user", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	res, err := c.Invoke(ctx, &gen.UsersGetUsersRequest{
		ID: []gen.InputUserClass{&gen.InputUser{
			UserID:     user.ID,
			AccessHash: user.AccessHash,
		}},
	})
	if err != nil {
		return nil, err
	}
	vr, ok := res.(*rpc.VectorResult)
	if !ok || len(vr.Items) == 0 {
		return nil, fmt.Errorf("client: getUsers answered %T", res)
	}
	u, ok := vr.Items[0].(*gen.User)
	if !ok || u.ID != user.ID {
		// UserEmpty → the hash was stale/invalid; the cache is not updated.
		return nil, fmt.Errorf("client: getUsers answered empty user")
	}
	c.entities.seedUser(u)
	if ent, found := c.entities.get(PeerID{PeerUser, u.ID}); found {
		if eu, ok := ent.(*User); ok {
			return eu, nil
		}
	}
	return &User{ID: u.ID}, nil
}

// GetChannel refreshes a channel/supergroup's metadata via
// channels.getChannels (reference users.py:477-483). Hash required from
// the cache.
func (c *Client) GetChannel(ctx context.Context, ref PeerRef) (*Channel, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	ch, ok := resolved.(*Channel)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a channel", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	res, err := c.Invoke(ctx, &gen.ChannelsGetChannelsRequest{
		ID: []gen.InputChannelClass{&gen.InputChannel{
			ChannelID:  ch.ID,
			AccessHash: ch.AccessHash,
		}},
	})
	if err != nil {
		return nil, err
	}
	chats, ok := res.(*gen.MessagesChats)
	if !ok {
		return nil, fmt.Errorf("client: getChannels answered %T", res)
	}
	c.entities.seedUpdateEntities(nil, chats.Chats)
	if ent, found := c.entities.get(PeerID{PeerChannel, ch.ID}); found {
		if ec, ok := ent.(*Channel); ok {
			return ec, nil
		}
	}
	return nil, fmt.Errorf("client: getChannels answered without the requested channel")
}

// GetChat refreshes a basic chat's metadata via the legacy
// messages.getFullChat path is a full-info call; the lightweight refresh
// for basic chats comes from dialog/envelope seeding — there is no
// bare-chat refresh RPC in layer 182 (chats have no access hash to
// refresh). Returns the cached entity.
func (c *Client) GetChat(ctx context.Context, ref PeerRef) (*Chat, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	chat, ok := resolved.(*Chat)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a chat", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	return chat, nil
}

// UserFullInfo is the typed domain form of users.UserFull
// (server-provided fields only).
type UserFullInfo struct {
	User                   *User
	About                  string
	PinnedMessageID        int64
	CommonChatsCount       int32
	PhoneCallsAvailable    bool
	VideoCallsAvailable    bool
	VoiceMessagesForbidden bool
	CanPinMessage          bool
	HasScheduled           bool
	TTLPeriod              int32
}

// GetUserFull returns extended user info (users.getFullUser).
func (c *Client) GetUserFull(ctx context.Context, ref PeerRef) (*UserFullInfo, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	user, ok := resolved.(*User)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a user", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	res, err := c.Invoke(ctx, &gen.UsersGetFullUserRequest{
		ID: &gen.InputUser{UserID: user.ID, AccessHash: user.AccessHash},
	})
	if err != nil {
		return nil, err
	}
	uf, ok := res.(*gen.UsersUserFull)
	if !ok {
		return nil, fmt.Errorf("client: getFullUser answered %T", res)
	}
	c.entities.seedUpdateEntities(uf.Users, uf.Chats)
	out := &UserFullInfo{User: user}
	if full, ok := uf.FullUser.(*gen.UserFull); ok {
		if full.About != nil {
			out.About = *full.About
		}
		if full.PinnedMsgID != nil {
			out.PinnedMessageID = int64(*full.PinnedMsgID)
		}
		out.CommonChatsCount = full.CommonChatsCount
		out.PhoneCallsAvailable = full.PhoneCallsAvailable
		out.VideoCallsAvailable = full.VideoCallsAvailable
		out.VoiceMessagesForbidden = full.VoiceMessagesForbidden
		out.CanPinMessage = full.CanPinMessage
		out.HasScheduled = full.HasScheduled
		if full.TTLPeriod != nil {
			out.TTLPeriod = *full.TTLPeriod
		}
	}
	return out, nil
}

// ChannelFullInfo is the typed domain form of channels.ChannelFull.
type ChannelFullInfo struct {
	Channel             *Channel
	About               string
	ParticipantsCount   int32
	AdminsCount         int32
	PinnedMessageID     int64
	CanViewParticipants bool
	CanDeleteChannel    bool
	HasScheduled        bool
	TTLPeriod           int32
}

// GetChannelFull returns extended channel info (channels.getFullChannel).
func (c *Client) GetChannelFull(ctx context.Context, ref PeerRef) (*ChannelFullInfo, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	ch, ok := resolved.(*Channel)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a channel", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	res, err := c.Invoke(ctx, &gen.ChannelsGetFullChannelRequest{
		Channel: &gen.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash},
	})
	if err != nil {
		return nil, err
	}
	cf, ok := res.(*gen.MessagesChatFull)
	if !ok {
		return nil, fmt.Errorf("client: getFullChannel answered %T", res)
	}
	c.entities.seedUpdateEntities(nil, cf.Chats)
	out := &ChannelFullInfo{Channel: ch}
	if full, ok := cf.FullChat.(*gen.ChannelFull); ok {
		out.About = full.About
		if full.ParticipantsCount != nil {
			out.ParticipantsCount = *full.ParticipantsCount
		}
		if full.AdminsCount != nil {
			out.AdminsCount = *full.AdminsCount
		}
		if full.PinnedMsgID != nil {
			out.PinnedMessageID = int64(*full.PinnedMsgID)
		}
		out.CanViewParticipants = full.CanViewParticipants
		out.CanDeleteChannel = full.CanDeleteChannel
		out.HasScheduled = full.HasScheduled
		if full.TTLPeriod != nil {
			out.TTLPeriod = *full.TTLPeriod
		}
	}
	return out, nil
}

// ChatFullInfo is the typed domain form of messages.ChatFull for basic
// groups.
type ChatFullInfo struct {
	Chat              *Chat
	About             string
	ParticipantsCount int32
	HasScheduled      bool
	TTLPeriod         int32
}

// GetChatFull returns extended basic-group info (messages.getFullChat).
func (c *Client) GetChatFull(ctx context.Context, ref PeerRef) (*ChatFullInfo, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	chat, ok := resolved.(*Chat)
	if !ok {
		return nil, fmt.Errorf("%w: peer %s/%d is not a chat", ErrInvalidPeer, resolved.peerID().Kind, resolved.peerID().ID)
	}
	res, err := c.Invoke(ctx, &gen.MessagesGetFullChatRequest{ChatID: chat.ID})
	if err != nil {
		return nil, err
	}
	cf, ok := res.(*gen.MessagesChatFull)
	if !ok {
		return nil, fmt.Errorf("client: getFullChat answered %T", res)
	}
	c.entities.seedUpdateEntities(cf.Users, cf.Chats)
	out := &ChatFullInfo{Chat: chat}
	if full, ok := cf.FullChat.(*gen.ChatFull); ok {
		out.About = full.About
		out.HasScheduled = full.HasScheduled
		if full.TTLPeriod != nil {
			out.TTLPeriod = *full.TTLPeriod
		}
		// Participant count: derived from the server-provided
		// participants list when present (ChatFull carries the list, not
		// a count — the count lives on the legacy Chat envelope).
		if cp, ok := full.Participants.(*gen.ChatParticipants); ok {
			out.ParticipantsCount = int32(len(cp.Participants))
		}
	}
	return out, nil
}

// SetTyping sends a typing indicator to the peer (reference sends
// messages.setTyping with sendMessageTypingAction; messages.py:631's
// send path uses the same peer resolution).
func (c *Client) SetTyping(ctx context.Context, ref PeerRef) error {
	input, err := c.inputPeerForRef(ctx, ref)
	if err != nil {
		return err
	}
	_, err = c.Invoke(ctx, &gen.MessagesSetTypingRequest{
		Peer:   input,
		Action: &gen.SendMessageTypingAction{},
	})
	return err
}

// inputPeerForRef resolves any PeerRef to the wire InputPeer.
func (c *Client) inputPeerForRef(ctx context.Context, ref PeerRef) (gen.InputPeerClass, error) {
	resolved, err := c.ResolvePeer(ctx, ref)
	if err != nil {
		return nil, err
	}
	return c.inputPeer(resolved)
}
