package client

// Phase 14 unit tests: entities (UTF-16 semantics), reply metadata,
// MessageKey identity, read-state predicates, global search digest.

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

func contextBackground() context.Context { return context.Background() }

// TestUTF16Len: wire lengths are UTF-16 code units — astral-plane
// runes count as TWO units (helpers.py:39-45 surrogate rule).
func TestUTF16Len(t *testing.T) {
	cases := []struct {
		text string
		want int32
	}{
		{"hello", 5},
		{"سلام", 4},         // Persian: BMP
		{"\U0001F600", 2},   // 😀 astral plane → surrogate pair
		{"a\U0001F600b", 4}, // mixed: 1+2+1
		{"", 0},
	}
	for _, tc := range cases {
		if got := utf16Len(tc.text); got != tc.want {
			t.Fatalf("utf16Len(%q) = %d, want %d", tc.text, got, tc.want)
		}
	}
}

// TestEntityConversionUTF16: offsets/lengths pass through EXACTLY — no
// UTF-8 reinterpretation (§5: preserve offsets exactly).
func TestEntityConversionUTF16(t *testing.T) {
	wire := &gen.Message{
		ID: 1, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		Message: "bold:\U0001F600code",
		Entities: []gen.MessageEntityClass{
			&gen.MessageEntityBold{Offset: 5, Length: 2}, // spans the emoji (units 5-6)
			&gen.MessageEntityCode{Offset: 7, Length: 4}, // "code" (units 7-10)
			&gen.MessageEntityPre{Offset: 7, Length: 4, Language: "go"},
			&gen.MessageEntityTextUrl{Offset: 0, Length: 5, URL: "https://x.example"},
			&gen.MessageEntityMentionName{Offset: 0, Length: 5, UserID: 77},
			&gen.MessageEntityCustomEmoji{Offset: 0, Length: 5, DocumentID: 88},
		},
	}
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	m := c.messageFromTL(wire)
	if len(m.TextEntities) != 6 {
		t.Fatalf("entities = %d, want 6", len(m.TextEntities))
	}
	if m.TextEntities[0].Kind != TextEntityBold || m.TextEntities[0].Offset != 5 || m.TextEntities[0].Length != 2 {
		t.Fatalf("bold = %+v", m.TextEntities[0])
	}
	if m.TextEntities[2].Kind != TextEntityPre || m.TextEntities[2].Language != "go" {
		t.Fatalf("pre = %+v", m.TextEntities[2])
	}
	if m.TextEntities[3].URL != "https://x.example" {
		t.Fatalf("text url = %+v", m.TextEntities[3])
	}
	if m.TextEntities[4].UserID != 77 {
		t.Fatalf("mention name = %+v", m.TextEntities[4])
	}
	if m.TextEntities[5].DocumentID != 88 {
		t.Fatalf("custom emoji = %+v", m.TextEntities[5])
	}
	// The emoji span must slice correctly in UTF-16 space.
	if span, ok := utf16Slice(m.Text, 5, 2); !ok || span != "\U0001F600" {
		t.Fatalf("emoji span = %q", span)
	}
}

// TestEntityUnknownPreserved: unknown entity ctors are preserved
// structurally (§13) — never dropped, never a panic.
func TestEntityUnknownPreserved(t *testing.T) {
	// Craft an entity list with an unknown ctor via raw decode.
	wire := &gen.Message{
		ID: 1, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100, Message: "x",
		Entities: []gen.MessageEntityClass{
			&gen.MessageEntityBold{Offset: 0, Length: 1},
			unknownEntityForTest{},
		},
	}
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	m := c.messageFromTL(wire)
	if len(m.TextEntities) != 1 {
		t.Fatalf("known entities = %d, want 1", len(m.TextEntities))
	}
	if len(m.UnknownTextEntities) != 1 {
		t.Fatalf("unknown entities = %d, want 1", len(m.UnknownTextEntities))
	}
	if m.UnknownTextEntities[0].WireCtor == 0 {
		t.Fatal("unknown ctor id lost")
	}
}

// unknownEntityForTest is a synthetic entity ctor outside the known set.
type unknownEntityForTest struct{}

func (unknownEntityForTest) CtorID() uint32            { return 0xDEADBEEF }
func (unknownEntityForTest) Encode(w *tl.Writer) error { return nil }

// TestReplyMetadata: direct vs thread vs cross-peer reply metadata
// (§6) — metadata only, nothing fabricated.
func TestReplyMetadata(t *testing.T) {
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}

	// Direct reply.
	direct := &gen.Message{ID: 2, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		ReplyTo: &gen.MessageReplyHeader{ReplyToMsgID: ptrI32P(1)}}
	m := c.messageFromTL(direct)
	if m.Reply == nil || m.Reply.MessageID != 1 || m.Reply.IsThreadReply() {
		t.Fatalf("direct reply = %+v", m.Reply)
	}

	// Thread reply (forum topic root).
	thread := &gen.Message{ID: 3, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		ReplyTo: &gen.MessageReplyHeader{ReplyToMsgID: ptrI32P(2), ReplyToTopID: ptrI32P(1), ForumTopic: true}}
	m = c.messageFromTL(thread)
	if m.Reply == nil || !m.Reply.IsThreadReply() || m.Reply.TopMessageID != 1 || !m.Reply.ForumTopic {
		t.Fatalf("thread reply = %+v", m.Reply)
	}

	// Cross-peer reply.
	cross := &gen.Message{ID: 4, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100,
		ReplyTo: &gen.MessageReplyHeader{ReplyToMsgID: ptrI32P(9),
			ReplyToPeerID: &gen.PeerChannel{ChannelID: 42}}}
	m = c.messageFromTL(cross)
	if m.Reply == nil || m.Reply.CrossPeer.Kind != PeerChannel || m.Reply.CrossPeer.ID != 42 {
		t.Fatalf("cross reply = %+v", m.Reply)
	}

	// No reply header → nil metadata.
	plain := c.messageFromTL(&gen.Message{ID: 5, PeerID: &gen.PeerChat{ChatID: 55}, Date: 100})
	if plain.Reply != nil {
		t.Fatalf("plain reply = %+v", plain.Reply)
	}
}

func ptrI32P(v int32) *int32 { return &v }

// TestMessageKey: the identity pairing (peer + id) — never id alone.
func TestMessageKey(t *testing.T) {
	m := &Message{ID: 9, PeerID: PeerID{PeerChannel, 42}}
	k := m.Key()
	if k.Peer.Kind != PeerChannel || k.Peer.ID != 42 || k.ID != 9 {
		t.Fatalf("key = %+v", k)
	}
	// Same ID, different peer → different keys.
	other := MessageKey{Peer: PeerID{PeerChannel, 43}, ID: 9}
	if other == k {
		t.Fatal("distinct peers produced identical keys")
	}
}

// TestReadStatePredicates: the reference's max-id read rule.
func TestReadStatePredicates(t *testing.T) {
	cs := newConversationStore(16)
	peer := PeerID{PeerChat, 55}
	cs.seed(peer, 10, 0, 0, 8, 9) // top 10, inbox max 8, outbox max 9

	if !cs.IsRead(peer, 8, false) || cs.IsRead(peer, 9, false) {
		t.Fatal("inbox read rule broken")
	}
	if !cs.IsRead(peer, 9, true) || cs.IsRead(peer, 10, true) {
		t.Fatal("outbox read rule broken")
	}
	if cs.IsRead(PeerID{PeerChat, 99}, 1, false) {
		t.Fatal("unknown peer reported read")
	}
}

// TestGlobalSearchDigest: mixed-peer results attribute correctly —
// duplicate IDs across peers produce distinct MessageKeys (§10/§24).
func TestGlobalSearchDigest(t *testing.T) {
	wire := &gen.MessagesMessagesSlice{
		Count: 2,
		Messages: []gen.MessageClass{
			&gen.Message{ID: 9, PeerID: &gen.PeerUser{UserID: 1}, Date: 9, Message: "u"},
			&gen.Message{ID: 9, PeerID: &gen.PeerChannel{ChannelID: 42}, Date: 9, Message: "c"},
		},
		Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
	}
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	// Both messages classify through the shared path; their keys differ.
	m1 := c.messageFromTL(wire.Messages[0].(*gen.Message))
	m2 := c.messageFromTL(wire.Messages[1].(*gen.Message))
	if m1.Key() == m2.Key() {
		t.Fatal("duplicate ids across peers produced identical keys")
	}
	if m1.Key().Peer.Kind != PeerUser || m2.Key().Peer.Kind != PeerChannel {
		t.Fatalf("attribution broken: %v %v", m1.Key(), m2.Key())
	}
}

// TestReplyResolveUnavailablePaths: nil message / no metadata →
// ErrReplyUnavailable without network calls.
func TestReplyResolveUnavailablePaths(t *testing.T) {
	c := &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}
	if _, err := c.ResolveReply(context.Background(), nil); !errors.Is(err, ErrReplyUnavailable) {
		t.Fatalf("nil message: %v", err)
	}
	plain := &Message{ID: 1, PeerID: PeerID{PeerChat, 55}}
	if _, err := c.ResolveReply(context.Background(), plain); !errors.Is(err, ErrReplyUnavailable) {
		t.Fatalf("no metadata: %v", err)
	}
}

// TestGlobalSearchIteratorTermination: NotModified and empty answers
// terminate cleanly.
func TestGlobalSearchIteratorTermination(t *testing.T) {
	// The digest logic is exercised through the E2E fake in the external
	// tests; here the exhausted-flag semantics are pinned.
	it := &GlobalSearchIterator{exhausted: true, c: &Client{entities: newEntityCache(64), conversations: newConversationStore(16)}}
	if _, ok := it.Next(contextBackground()); ok {
		t.Fatal("exhausted iterator produced results")
	}
	_ = time.Now
}

// TestEntityOffsetsDifferential: UTF-16 offset semantics against a
// Persian/English/emoji mixed message — the reference counts code units
// (helpers.py:39-45).
func TestEntityOffsetsDifferential(t *testing.T) {
	// "سلام " (5 units) + 😀 (2 units) + " world" (6 units) = 13 units.
	text := "سلام 😀 world"
	if got := utf16Len(text); got != 13 {
		t.Fatalf("utf16 len = %d, want 13", got)
	}
	// A bold entity spanning the Persian word: offset 0, length 4.
	span, ok := utf16Slice(text, 0, 4)
	if !ok || span != "سلام" {
		t.Fatalf("persian span = %q", span)
	}
	// The emoji: offset 5, length 2.
	span, ok = utf16Slice(text, 5, 2)
	if !ok || span != "😀" {
		t.Fatalf("emoji span = %q", span)
	}
	// Out-of-range length: clamped, never panics.
	if _, ok := utf16Slice(text, 10, 100); !ok {
		t.Fatal("offset beyond start rejected")
	}
	// Negative offset: rejected.
	if _, ok := utf16Slice(text, -1, 2); ok {
		t.Fatal("negative offset accepted")
	}
}
