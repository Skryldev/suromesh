package client

// Conversation state (Phase 11): bounded, DERIVED application state kept
// in sync with the Phase 10 ordered update stream.
//
// Ownership model (§17/§19):
//   - MessageBox = the protocol synchronization authority (pts/qts/seq);
//     this store never duplicates it.
//   - conversationStore = derived per-peer view (top message, unread
//     counts, read maxima, last activity). Single-writer: the update
//     engine's sync loop mutates it while walking the ordered stream;
//     readers take the store's RWMutex. No second state machine.
//   - Bounded: at most ConversationCacheSize entries (default 1024).
//     Eviction drops arbitrary entries — the store holds derived state
//     only, so eviction can never break MessageBox, entity cache, auth,
//     or transport (§20). Evicted conversations reappear on the next
//     dialog fetch or qualifying update.
//
// Seeding: IterDialogs seeds each dialog's server-reported state
// (top message, unread counts, read maxima) — the same numbers official
// applications display.

import (
	"context"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/tl/gen"
)

// Conversation is the derived application state for one peer.
type Conversation struct {
	Peer            PeerID
	TopMessageID    int64
	UnreadCount     int32
	UnreadMentions  int32
	ReadInboxMaxID  int32
	ReadOutboxMaxID int32
	LastActivity    time.Time
}

// conversationStore is the bounded derived-state container.
type conversationStore struct {
	mu  sync.RWMutex
	m   map[PeerID]*Conversation
	cap int
}

func newConversationStore(capacity int) *conversationStore {
	if capacity <= 0 {
		capacity = 1024
	}
	return &conversationStore{m: make(map[PeerID]*Conversation, 64), cap: capacity}
}

// seed installs (or refreshes) server-reported dialog state. Called from
// the dialog iterator — the server answer is authoritative.
func (cs *conversationStore) seed(peer PeerID, top int64, unread, mentions, readIn, readOut int32) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.m[peer]
	if c == nil {
		if len(cs.m) >= cs.cap {
			cs.evictLocked()
		}
		c = &Conversation{Peer: peer}
		cs.m[peer] = c
	}
	if top > c.TopMessageID {
		c.TopMessageID = top
	}
	if unread > c.UnreadCount {
		c.UnreadCount = unread
	} else {
		c.UnreadCount = unread // server state replaces the derived guess
	}
	c.UnreadMentions = mentions
	if readIn > c.ReadInboxMaxID {
		c.ReadInboxMaxID = readIn
	}
	if readOut > c.ReadOutboxMaxID {
		c.ReadOutboxMaxID = readOut
	}
}

// onNewMessage applies one ordered NewMessage-class update.
// Outgoing messages advance the top message and outbox maximum but never
// the unread count; inbox messages increment it (best-effort derivation —
// the server's dialog answers remain authoritative).
func (cs *conversationStore) onNewMessage(peer PeerID, msgID int64, out bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.m[peer]
	if c == nil {
		if len(cs.m) >= cs.cap {
			cs.evictLocked()
		}
		c = &Conversation{Peer: peer}
		cs.m[peer] = c
	}
	if msgID > c.TopMessageID {
		c.TopMessageID = msgID
	}
	if out {
		if int32(msgID) > c.ReadOutboxMaxID {
			c.ReadOutboxMaxID = int32(msgID)
		}
	} else {
		c.UnreadCount++
	}
	c.LastActivity = time.Now()
}

// onEditMessage refreshes last activity (message content changes carry
// no count semantics).
func (cs *conversationStore) onEditMessage(peer PeerID, msgID int64) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c := cs.m[peer]; c != nil {
		c.LastActivity = time.Now()
	}
}

// onDeleteMessage: deletion removes messages the store never held
// (message bodies are not cached here); only last activity moves.
func (cs *conversationStore) onDeleteMessages(peer PeerID) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if c := cs.m[peer]; c != nil {
		c.LastActivity = time.Now()
	}
}

// onRead applies a read acknowledgement (ReadHistory /
// ReadChannelInbox / ReadMessagesContents): the read maximum advances
// and the unread count is reduced by the covered range (bounded below
// by zero; derived state only).
func (cs *conversationStore) onRead(peer PeerID, maxID int32, inbox bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	c := cs.m[peer]
	if c == nil {
		return
	}
	if inbox {
		if maxID > c.ReadInboxMaxID {
			covered := maxID - c.ReadInboxMaxID
			if covered > c.UnreadCount {
				covered = c.UnreadCount
			}
			c.UnreadCount -= covered
			c.ReadInboxMaxID = maxID
		}
	} else if maxID > c.ReadOutboxMaxID {
		c.ReadOutboxMaxID = maxID
	}
	c.LastActivity = time.Now()
}

// IsRead reports whether a message id is read for a peer, using the
// reference's read rule: inbox messages are read when id ≤
// ReadInboxMaxID, outgoing messages when id ≤ ReadOutboxMaxID
// (message read state = max-id coverage, not per-message flags).
// Unknown peers report false (nothing read).
func (cs *conversationStore) IsRead(peer PeerID, messageID int64, out bool) bool {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	c := cs.m[peer]
	if c == nil {
		return false
	}
	if out {
		return messageID <= int64(c.ReadOutboxMaxID)
	}
	return messageID <= int64(c.ReadInboxMaxID)
}

// Lookup returns a copy of the conversation state for a peer.
func (cs *conversationStore) Lookup(peer PeerID) (Conversation, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if c := cs.m[peer]; c != nil {
		return *c, true
	}
	return Conversation{}, false
}

// Len reports the number of tracked conversations.
func (cs *conversationStore) Len() int {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return len(cs.m)
}

// evictLocked drops one entry (arbitrary: map iteration order). The
// store holds derived state only — eviction never breaks protocol layers
// (§20).
func (cs *conversationStore) evictLocked() {
	for k := range cs.m {
		delete(cs.m, k)
		return
	}
}

// Client surface -------------------------------------------------------------

// Conversation returns a copy of the derived state for a peer (false
// when nothing is tracked — call GetDialogs or wait for updates).
func (c *Client) Conversation(peer Peer) (Conversation, bool) {
	return c.conversations.Lookup(peer.peerID())
}

// TrackedConversations reports how many conversations are currently
// tracked (bounded by ConversationCacheSize).
func (c *Client) TrackedConversations() int { return c.conversations.Len() }

// IsMessageRead reports whether a message id is read for a peer
// (derived state; docs/read-state.md).
func (c *Client) IsMessageRead(peer Peer, messageID int64) bool {
	return c.conversations.IsRead(peer.peerID(), messageID, false)
}

// IsOutgoingRead reports the outbox-side read rule for a message id.
func (c *Client) IsOutgoingRead(peer Peer, messageID int64) bool {
	return c.conversations.IsRead(peer.peerID(), messageID, true)
}

// MarkAsRead acknowledges messages up to maxID (0 = the currently known
// top message). The RPC follows the reference's per-kind split
// (§21): channels use channels.readHistory, everything else
// messages.readHistory. No optimistic local state: the store updates
// only when the ordered update stream carries the read event back.
func (c *Client) MarkAsRead(ctx context.Context, peer PeerRef, maxID int64) error {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return err
	}
	if maxID <= 0 {
		// Reference permissiveness (send_read_acknowledge): proceed with
		// the known top message when tracked; an unknown peer sends
		// max_id=0 (the server answers without effect).
		if conv, ok := c.conversations.Lookup(ref.peerID()); ok {
			maxID = conv.TopMessageID
		}
	}
	switch v := ref.(type) {
	case *Channel:
		_, err = c.Invoke(ctx, &gen.ChannelsReadHistoryRequest{
			Channel: &gen.InputChannel{ChannelID: v.ID, AccessHash: v.AccessHash},
			MaxID:   int32(maxID),
		})
	case PeerID:
		if v.Kind == PeerChannel {
			return &ErrAccessHashUnavailable{Peer: v}
		}
		_, err = c.Invoke(ctx, &gen.MessagesReadHistoryRequest{Peer: input, MaxID: int32(maxID)})
	default:
		_, err = c.Invoke(ctx, &gen.MessagesReadHistoryRequest{Peer: input, MaxID: int32(maxID)})
	}
	return err
}
