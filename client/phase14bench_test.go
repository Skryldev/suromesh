package client

// Phase 14 benchmarks: entity conversion, identity lookup, read state,
// cross-path message conversion.

import (
	"testing"

	"github.com/Skryldev/suromesh/tl/gen"
)

func benchWireMessage() *gen.Message {
	return &gen.Message{ID: 1, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		Message: "bench \U0001F600 text",
		Entities: []gen.MessageEntityClass{
			&gen.MessageEntityBold{Offset: 0, Length: 5},
			&gen.MessageEntityCode{Offset: 9, Length: 4},
			&gen.MessageEntityTextUrl{Offset: 0, Length: 5, URL: "https://x.example"},
		}}
}

func BenchmarkEntityConversion(b *testing.B) {
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	wire := benchWireMessage()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		m := c.messageFromTL(wire)
		if len(m.TextEntities) != 3 {
			b.Fatal("entities lost")
		}
	}
}

func BenchmarkMessageKeyLookup(b *testing.B) {
	m := &Message{ID: 42, PeerID: PeerID{PeerChannel, 7}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = m.Key()
	}
}

func BenchmarkReadStateLookup(b *testing.B) {
	cs := newConversationStore(16)
	peer := PeerID{PeerChat, 55}
	cs.seed(peer, 100, 0, 0, 50, 60)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if !cs.IsRead(peer, 40, false) {
			b.Fatal("unexpected unread")
		}
	}
}

func BenchmarkCrossPathConversion(b *testing.B) {
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	wire := benchWireMessage()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Two full conversions per iteration — the cross-path invariant.
		m1 := c.messageFromTL(wire)
		m2 := c.messageFromTL(wire)
		if len(m1.TextEntities) != len(m2.TextEntities) {
			b.Fatal("cross-path divergence")
		}
	}
}

func BenchmarkReplyMetadata(b *testing.B) {
	hdr := &gen.MessageReplyHeader{ReplyToMsgID: ptr32B(1), ReplyToTopID: ptr32B(0), ForumTopic: true}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = replyInfoFromWire(hdr)
	}
}

func ptr32B(v int32) *int32 { return &v }
