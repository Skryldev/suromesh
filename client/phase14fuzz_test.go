package client

// Phase 14 fuzz targets: hostile entities, reply metadata, identity,
// read-state transitions, and global-search attribution.

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzMessageEntities: any wire entity constructor converts without
// panicking; unknown ctors are preserved structurally.
func FuzzMessageEntities(f *testing.F) {
	f.Add([]byte{0x30, 0x88, 0x94, 0xbf}) // messageEntityBold-ish prefix
	f.Add([]byte(nil))
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		ec, ok := obj.(gen.MessageEntityClass)
		if !ok {
			return
		}
		te, known := textEntityFromWire(ec)
		if known {
			if te.Offset < 0 || te.Length < 0 {
				t.Fatalf("negative span from %T: %+v", ec, te)
			}
		}
		known2, unknown := textEntitiesFromWire([]gen.MessageEntityClass{ec})
		if len(known2)+len(unknown) != 1 {
			t.Fatalf("conversion arity broken: %d known, %d unknown", len(known2), len(unknown))
		}
	})
}

// FuzzReplyMetadata: hostile reply headers convert without panicking
// and never fabricate ids.
func FuzzReplyMetadata(f *testing.F) {
	f.Add(int32(1), int32(0), false, false, false)
	f.Fuzz(func(t *testing.T, msgID, topID int32, crossPeer, forum, sched bool) {
		hdr := &gen.MessageReplyHeader{
			ReplyToMsgID:     &msgID,
			ReplyToTopID:     &topID,
			ReplyToPeerID:    nil,
			ForumTopic:       forum,
			ReplyToScheduled: sched,
		}
		if crossPeer {
			hdr.ReplyToPeerID = &gen.PeerChannel{ChannelID: 42}
		}
		ri := replyInfoFromWire(hdr)
		if ri.MessageID != int64(msgID) || ri.TopMessageID != int64(topID) {
			t.Fatalf("metadata mangled: %+v", ri)
		}
		if ri.IsThreadReply() != (topID != 0) {
			t.Fatalf("thread flag broken: %+v", ri)
		}
	})
}

// FuzzMessageIdentity: keys pair peer+id exactly; distinct peers never
// collide for equal ids.
func FuzzMessageIdentity(f *testing.F) {
	f.Add(int64(1), int64(1), int64(2))
	f.Fuzz(func(t *testing.T, peerID, msgID, otherPeer int64) {
		k1 := MessageKey{Peer: PeerID{PeerChannel, peerID}, ID: msgID}
		k2 := MessageKey{Peer: PeerID{PeerChannel, otherPeer}, ID: msgID}
		if peerID == otherPeer && k1 != k2 {
			t.Fatal("equal peers produced unequal keys")
		}
		if peerID != otherPeer && k1 == k2 {
			t.Fatal("distinct peers produced equal keys")
		}
	})
}

// FuzzReadState: arbitrary max-id sequences keep the read rule coherent.
// Maxima only ADVANCE (seed/onRead never lower them), so a hostile
// negative max-id leaves the effective maximum at its prior value — the
// assertion checks against that effective maximum.
func FuzzReadState(f *testing.F) {
	f.Add(int64(5), int32(3), int32(1))
	f.Fuzz(func(t *testing.T, seedMax int64, inboxMax, outboxMax int32) {
		cs := newConversationStore(4)
		peer := PeerID{PeerChat, 55}
		cs.seed(peer, seedMax, 0, 0, inboxMax, outboxMax)
		// Effective maxima: advance-only semantics.
		if inboxMax < 0 {
			inboxMax = 0
		}
		if outboxMax < 0 {
			outboxMax = 0
		}
		for id := int64(0); id < 10; id++ {
			inRead := cs.IsRead(peer, id, false)
			outRead := cs.IsRead(peer, id, true)
			if inRead && id > int64(inboxMax) {
				t.Fatalf("id %d read beyond inbox max %d", id, inboxMax)
			}
			if outRead && id > int64(outboxMax) {
				t.Fatalf("id %d read beyond outbox max %d", id, outboxMax)
			}
		}
	})
}

// FuzzGlobalSearchAttribution: hostile pages attribute every message to
// its own peer — no ID-only attribution (§10).
func FuzzGlobalSearchAttribution(f *testing.F) {
	f.Add(int64(9), int64(1), int64(2))
	f.Fuzz(func(t *testing.T, msgID int64, peerA, peerB int64) {
		if peerA == peerB {
			peerB = peerA + 1
		}
		c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
		m1 := c.messageFromTL(&gen.Message{ID: int32(msgID),
			PeerID: &gen.PeerUser{UserID: peerA}, Date: 1})
		m2 := c.messageFromTL(&gen.Message{ID: int32(msgID),
			PeerID: &gen.PeerUser{UserID: peerB}, Date: 1})
		if m1.Key() == m2.Key() {
			t.Fatal("attribution collided across peers")
		}
	})
}
