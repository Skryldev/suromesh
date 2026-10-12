package client

// Message identity + reply resolution (Phase 14).
//
// Identity (§11): a message is identified by (peer, message id) —
// MessageKey. Message IDs are ONLY unique within a peer's sequence;
// channels share ID spaces across peers. Every cross-message lookup
// (reply resolution, global search attribution) keys on MessageKey.
//
// Reply resolution (§6): metadata never triggers network calls; the
// explicit ResolveReply fetches the replied-to message via
// messages.getMessages (reference _IDsIter, messages.py:288-322 —
// channels use channels.getMessages, others messages.getMessages;
// MessageIdsEmptyError → NotModified-style empty result).

import (
	"context"
	"errors"
	"fmt"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// MessageKey is the globally-unambiguous identity of a message:
// (peer, message id). Message IDs are NOT globally unique across peers —
// channels in particular share ID spaces — so any cross-message state
// must key on this pair (§11/§24).
type MessageKey struct {
	Peer PeerID
	ID   int64
}

// Key returns the message's identity.
func (m *Message) Key() MessageKey { return MessageKey{Peer: m.PeerID, ID: m.ID} }

// String renders the key for diagnostics (peer kind + id + message id
// only — no content).
func (k MessageKey) String() string {
	return fmt.Sprintf("%s/%d#%d", k.Peer.Kind, k.Peer.ID, k.ID)
}

// ErrReplyUnavailable reports that a reply target could not be resolved:
// the message id is invalid, the target was deleted, or it is
// inaccessible. Never fabricated — the caller receives exactly this
// signal instead of a guessed message.
var ErrReplyUnavailable = errors.New("replies: reply target unavailable")

// ResolveReply fetches the message a given message replies to
// (explicit resolution; reference messages.getMessages via _IDsIter,
// messages.py:288-322). The peer for the target is the message's own
// peer unless the reply header names a different one (reply_to_peer_id
// — cross-chat reply, layer 182).
//
// Returns ErrReplyUnavailable when the target does not exist (server
// answered empty/NotModified) or when the message carries no reply
// metadata at all.
func (c *Client) ResolveReply(ctx context.Context, msg *Message) (*Message, error) {
	if msg == nil {
		return nil, ErrReplyUnavailable
	}
	if msg.Reply == nil || msg.Reply.MessageID <= 0 {
		return nil, ErrReplyUnavailable
	}
	targetPeer := msg.PeerID
	if msg.Reply.CrossPeer.Kind != 0 {
		targetPeer = msg.Reply.CrossPeer
	}
	fetched, err := c.getMessagesByIDs(ctx, targetPeer, []int64{msg.Reply.MessageID})
	if err != nil {
		return nil, err
	}
	for _, m := range fetched {
		if m != nil && m.ID == msg.Reply.MessageID {
			return m, nil
		}
	}
	return nil, ErrReplyUnavailable
}

// getMessagesByIDs fetches specific messages by id (reference _IDsIter,
// messages.py:300-322): channels route to channels.getMessages,
// everything else to messages.getMessages with InputMessage* IDs.
func (c *Client) getMessagesByIDs(ctx context.Context, peer PeerID, ids []int64) ([]*Message, error) {
	input, err := c.inputPeer(peer)
	if err != nil {
		return nil, err
	}
	var res tl.Object
	switch peer.Kind {
	case PeerChannel:
		ch, ok := input.(*gen.InputPeerChannel)
		if !ok {
			return nil, &ErrAccessHashUnavailable{Peer: peer}
		}
		inIDs := make([]gen.InputMessageClass, 0, len(ids))
		for _, id := range ids {
			if id <= 0 {
				continue // reference: invalid ids are skipped, not fabricated
			}
			inIDs = append(inIDs, &gen.InputMessageID{ID: int32(id)})
		}
		if len(inIDs) == 0 {
			return nil, nil
		}
		res, err = c.Invoke(ctx, &gen.ChannelsGetMessagesRequest{
			Channel: &gen.InputChannel{ChannelID: ch.ChannelID, AccessHash: ch.AccessHash},
			ID:      inIDs,
		})
	default:
		inIDs := make([]gen.InputMessageClass, 0, len(ids))
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			inIDs = append(inIDs, &gen.InputMessageID{ID: int32(id)})
		}
		if len(inIDs) == 0 {
			return nil, nil
		}
		res, err = c.Invoke(ctx, &gen.MessagesGetMessagesRequest{ID: inIDs})
	}
	if err != nil {
		return nil, err
	}
	var msgs []gen.MessageClass
	var users []gen.UserClass
	var chats []gen.ChatClass
	switch r := res.(type) {
	case *gen.MessagesMessages:
		msgs, users, chats = r.Messages, r.Users, r.Chats
	case *gen.MessagesMessagesSlice:
		msgs, users, chats = r.Messages, r.Users, r.Chats
	case *gen.MessagesChannelMessages:
		msgs, users, chats = r.Messages, r.Users, r.Chats
	case *gen.MessagesMessagesNotModified:
		return nil, nil // ids unknown to the server → unavailable
	default:
		return nil, fmt.Errorf("replies: getMessages answered %T", res)
	}
	c.entities.seedUpdateEntities(users, chats)

	out := make([]*Message, 0, len(msgs))
	for _, mc := range msgs {
		if m, ok := mc.(*gen.Message); ok {
			out = append(out, c.messageFromTL(m))
		}
		// MessageEmpty / MessageService entries are skipped: a service
		// message is not returned as a domain Message today (documented;
		// reply targets that are service messages surface as
		// ErrReplyUnavailable).
	}
	return out, nil
}
