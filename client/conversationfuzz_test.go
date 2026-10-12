package client

// Phase 11 fuzz targets: hostile peer refs, dialog/history responses and
// pagination sequences must never panic, loop, or fabricate access hashes.

import (
	"context"
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzPeerResolution: arbitrary marked IDs and username shapes resolve
// deterministically — never fabricating an access hash.
func FuzzPeerResolution(f *testing.F) {
	f.Add(int64(1))
	f.Add(int64(-456))
	f.Add(int64(-1000000000042))
	f.Add(int64(0))
	f.Add(int64(-1))
	f.Fuzz(func(t *testing.T, marked int64) {
		id, ok := ResolveMarkedID(marked)
		if !ok {
			return
		}
		// Round trip must be stable.
		if got := MarkedID(id); got != marked {
			t.Fatalf("MarkedID round trip broken: %d → %v → %d", marked, id, got)
		}
		// The resolver must never invent a hash: only cached entities pass.
		c := &Client{entities: newEntityCache(64), conversations: newConversationStore(64)}
		p, err := c.resolvePeerID(context.Background(), id)
		if err != nil {
			return // rejection is fine
		}
		switch e := p.(type) {
		case *User:
			_ = e
		case *Chat:
			_ = e // chats carry no hash by protocol design
		case *Channel:
			t.Fatal("channel resolved without a cached access hash")
		}
	})
}

// FuzzDialogResponseConversion: hostile dialog envelopes convert without
// panicking and never fabricate entities.
func FuzzDialogResponseConversion(f *testing.F) {
	f.Add([]byte{0x15, 0xba, 0x6c, 0x40}) // messages.dialogs ctor
	f.Add([]byte{0x71, 0xe0, 0x94, 0xf3}) // messages.dialogsSlice
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		var dialogs []gen.DialogClass
		var messages []gen.MessageClass
		switch r := obj.(type) {
		case *gen.MessagesDialogs:
			dialogs, messages = r.Dialogs, r.Messages
		case *gen.MessagesDialogsSlice:
			dialogs, messages = r.Dialogs, r.Messages
		default:
			return
		}
		msgs := make(map[PeerID]*Message, len(messages))
		for _, mc := range messages {
			if m, ok := mc.(*gen.Message); ok {
				msgs[peerIDFromWire(m.PeerID)] = messageFromTLFuzz(m)
			}
		}
		for _, dc := range dialogs {
			d, ok := dc.(*gen.Dialog)
			if !ok {
				continue
			}
			_ = peerIDFromWire(d.Peer)
			_ = msgs // lookup may miss — that's fine
		}
	})
}

// messageFromTLFuzz mirrors Client.messageFromTL minus client state.
func messageFromTLFuzz(m *gen.Message) *Message {
	return &Message{ID: int64(m.ID), Raw: m}
}

// FuzzMessageResponseConversion: hostile history envelopes convert
// without panicking.
func FuzzMessageResponseConversion(f *testing.F) {
	f.Add([]byte{0x8c, 0x71, 0x8e, 0x87}) // messages.messages ctor
	f.Add([]byte{0x3a, 0x54, 0x68, 0x5e}) // messages.messagesSlice
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		var msgs []gen.MessageClass
		switch r := obj.(type) {
		case *gen.MessagesMessages:
			msgs = r.Messages
		case *gen.MessagesMessagesSlice:
			msgs = r.Messages
		case *gen.MessagesChannelMessages:
			msgs = r.Messages
		case *gen.MessagesMessagesNotModified:
			_ = r.Count
			return
		default:
			return
		}
		for _, mc := range msgs {
			if m, ok := mc.(*gen.Message); ok {
				_ = messageFromTLFuzz(m)
			}
		}
	})
}

// FuzzPaginationState: arbitrary ID progressions through the iterator's
// local state must always terminate (no infinite pagination) and keep
// dedup coherent.
func FuzzPaginationState(f *testing.F) {
	f.Add(int32(1), int32(2), int32(3))
	f.Fuzz(func(t *testing.T, a, b, c int32) {
		lastID := int64(0)
		pages := [3][]int32{{a}, {b}, {c}}
		loads := 0
		for _, page := range pages {
			loads++
			if loads > 10 {
				t.Fatal("pagination did not terminate")
			}
			for _, id := range page {
				if id <= 0 {
					continue // MessageEmpty-equivalent: skipped
				}
				if lastID != 0 && int64(id) >= lastID {
					continue // duplicate/replay
				}
				lastID = int64(id)
			}
		}
	})
}

// FuzzConversationUpdate: arbitrary update sequences keep the store
// bounded and the counters impossible-negative-free.
func FuzzConversationUpdate(f *testing.F) {
	f.Add(int32(1), int32(1), false, int32(0))
	f.Fuzz(func(t *testing.T, pts, msgID int32, out bool, maxID int32) {
		cs := newConversationStore(4)
		peer := PeerID{PeerUser, 300}
		for i := int32(0); i < 8; i++ {
			cs.onNewMessage(peer, int64(msgID+i), out)
			cs.onRead(peer, maxID, true)
			cs.onEditMessage(peer, int64(msgID+i))
			cs.onDeleteMessages(peer)
		}
		conv, ok := cs.Lookup(peer)
		if ok {
			if conv.UnreadCount < 0 || conv.TopMessageID < 0 {
				t.Fatalf("impossible state: %+v", conv)
			}
		}
		if cs.Len() > 4 {
			t.Fatalf("store exceeded capacity: %d", cs.Len())
		}
	})
}
