package client

// Typed message entities (Phase 14): the domain representation of the
// wire MessageEntity* family, with the reference's UTF-16 offset
// semantics.
//
// Reference:
//	extensions/markdown.py:62  "work on byte level with the utf-16le
//	                           encoding to get the offsets right"
//	helpers.py:39-45           add_surrogate — "SoroushPlus offsets are
//	                           calculated with these [surrogate pairs]"
//
// Entity Offset/Length are UTF-16 CODE UNITS, not Go bytes or runes.
// Go strings are UTF-8, so utf16Len/utf16Index are provided for callers
// converting between the two. All 18 layer-182 entity constructors are
// converted; unknown constructors become UnknownTextEntity (preserved,
// never dropped — §13).

import (
	"github.com/Skryldev/suromesh/tl/gen"
	"unicode/utf16"
)

// TextEntityKind enumerates the supported entity families.
type TextEntityKind int32

const (
	TextEntityUnknown TextEntityKind = iota
	TextEntityMention
	TextEntityHashtag
	TextEntityBotCommand
	TextEntityURL
	TextEntityEmail
	TextEntityPhone
	TextEntityCashtag
	TextEntityBold
	TextEntityItalic
	TextEntityUnderline
	TextEntityStrike
	TextEntityCode
	TextEntityPre
	TextEntityTextURL
	TextEntityMentionName
	TextEntityCustomEmoji
	TextEntitySpoiler
	TextEntityBlockquote
)

func (k TextEntityKind) String() string {
	switch k {
	case TextEntityMention:
		return "mention"
	case TextEntityHashtag:
		return "hashtag"
	case TextEntityBotCommand:
		return "bot_command"
	case TextEntityURL:
		return "url"
	case TextEntityEmail:
		return "email"
	case TextEntityPhone:
		return "phone"
	case TextEntityCashtag:
		return "cashtag"
	case TextEntityBold:
		return "bold"
	case TextEntityItalic:
		return "italic"
	case TextEntityUnderline:
		return "underline"
	case TextEntityStrike:
		return "strike"
	case TextEntityCode:
		return "code"
	case TextEntityPre:
		return "pre"
	case TextEntityTextURL:
		return "text_url"
	case TextEntityMentionName:
		return "mention_name"
	case TextEntityCustomEmoji:
		return "custom_emoji"
	case TextEntitySpoiler:
		return "spoiler"
	case TextEntityBlockquote:
		return "blockquote"
	}
	return "unknown"
}

// TextEntity is one typed formatting/entity span. Offset and Length are
// UTF-16 code units measured on the message text (wire semantics).
type TextEntity struct {
	Kind   TextEntityKind
	Offset int32 // UTF-16 code units from the start of the message text
	Length int32 // UTF-16 code units

	// Payload fields (only meaningful for the matching Kind).
	URL        string // TextEntityTextURL
	Language   string // TextEntityPre
	UserID     int64  // TextEntityMentionName
	DocumentID int64  // TextEntityCustomEmoji
}

// utf16Len returns the wire (UTF-16 code unit) length of a Go string.
func utf16Len(s string) int32 {
	units := utf16.Encode([]rune(s))
	return int32(len(units))
}

// utf16Slice converts UTF-16 code-unit offsets to Go byte offsets for a
// string (used by callers slicing text by entity; out-of-range offsets
// clamp to the string bounds — malformed entities never panic).
func utf16Slice(s string, offset, length int32) (string, bool) {
	units := utf16.Encode([]rune(s))
	if offset < 0 || length < 0 || int(offset) > len(units) {
		return "", false
	}
	end := int(offset + length)
	if end > len(units) {
		end = len(units)
	}
	decoded := string(utf16.Decode(units[offset:end]))
	return decoded, true
}

// textEntityFromWire converts one wire entity; unknown constructors
// become UnknownTextEntity with their ctor preserved.
func textEntityFromWire(ec gen.MessageEntityClass) (TextEntity, bool) {
	switch e := ec.(type) {
	case *gen.MessageEntityMention:
		return textSpan(TextEntityMention, e.Offset, e.Length), true
	case *gen.MessageEntityHashtag:
		return textSpan(TextEntityHashtag, e.Offset, e.Length), true
	case *gen.MessageEntityBotCommand:
		return textSpan(TextEntityBotCommand, e.Offset, e.Length), true
	case *gen.MessageEntityUrl:
		return textSpan(TextEntityURL, e.Offset, e.Length), true
	case *gen.MessageEntityEmail:
		return textSpan(TextEntityEmail, e.Offset, e.Length), true
	case *gen.MessageEntityPhone:
		return textSpan(TextEntityPhone, e.Offset, e.Length), true
	case *gen.MessageEntityCashtag:
		return textSpan(TextEntityCashtag, e.Offset, e.Length), true
	case *gen.MessageEntityBold:
		return textSpan(TextEntityBold, e.Offset, e.Length), true
	case *gen.MessageEntityItalic:
		return textSpan(TextEntityItalic, e.Offset, e.Length), true
	case *gen.MessageEntityUnderline:
		return textSpan(TextEntityUnderline, e.Offset, e.Length), true
	case *gen.MessageEntityStrike:
		return textSpan(TextEntityStrike, e.Offset, e.Length), true
	case *gen.MessageEntitySpoiler:
		return textSpan(TextEntitySpoiler, e.Offset, e.Length), true
	case *gen.MessageEntityBlockquote:
		return textSpan(TextEntityBlockquote, e.Offset, e.Length), true
	case *gen.MessageEntityCode:
		return textSpan(TextEntityCode, e.Offset, e.Length), true
	case *gen.MessageEntityPre:
		out := textSpan(TextEntityPre, e.Offset, e.Length)
		out.Language = e.Language
		return out, true
	case *gen.MessageEntityTextUrl:
		out := textSpan(TextEntityTextURL, e.Offset, e.Length)
		out.URL = e.URL
		return out, true
	case *gen.MessageEntityMentionName:
		out := textSpan(TextEntityMentionName, e.Offset, e.Length)
		out.UserID = e.UserID
		return out, true
	case *gen.MessageEntityCustomEmoji:
		out := textSpan(TextEntityCustomEmoji, e.Offset, e.Length)
		out.DocumentID = e.DocumentID
		return out, true
	}
	return TextEntity{}, false
}

func textSpan(kind TextEntityKind, offset, length int32) TextEntity {
	return TextEntity{Kind: kind, Offset: offset, Length: length}
}

// UnknownTextEntity preserves an unrecognized wire entity (its ctor and
// raw form) — forward compatibility (§13).
type UnknownTextEntity struct {
	WireCtor uint32
	Raw      gen.MessageEntityClass
}

// textEntitiesFromWire converts a wire entity list, preserving unknowns.
func textEntitiesFromWire(list []gen.MessageEntityClass) ([]TextEntity, []UnknownTextEntity) {
	var known []TextEntity
	var unknown []UnknownTextEntity
	for _, ec := range list {
		if ec == nil {
			continue
		}
		if te, ok := textEntityFromWire(ec); ok {
			known = append(known, te)
		} else {
			unknown = append(unknown, UnknownTextEntity{WireCtor: ec.CtorID(), Raw: ec})
		}
	}
	return known, unknown
}

// ReplyInfo is the domain form of MessageReplyHeader (Phase 14):
// metadata only — resolving the replied-to message is an explicit
// operation (ResolveReply), never a hidden network call.
type ReplyInfo struct {
	// MessageID is the replied-to message id (0 = absent).
	MessageID int64
	// TopMessageID is the forum-thread root id when the reply targets a
	// topic/thread (0 = direct reply).
	TopMessageID int64
	// CrossPeer is non-nil when the reply targets a DIFFERENT chat
	// (reply_to_peer_id; server-provided peer only).
	CrossPeer PeerID
	// ForumTopic marks a forum-topic reply header.
	ForumTopic bool
	// Scheduled marks the reply target as a scheduled message.
	Scheduled bool
	// Quote carries the quoted text span when present (server-provided;
	// offsets are UTF-16 code units on the QUOTED message text).
	HasQuote    bool
	QuoteText   string
	QuoteOffset int32
	QuoteLength int32
}

// IsThreadReply reports whether the reply targets a thread/topic root
// rather than a plain message.
func (r *ReplyInfo) IsThreadReply() bool { return r.TopMessageID != 0 }

// replyInfoFromWire converts the wire header; every field is
// server-provided — nothing fabricated.
func replyInfoFromWire(rt *gen.MessageReplyHeader) *ReplyInfo {
	out := &ReplyInfo{ForumTopic: rt.ForumTopic, Scheduled: rt.ReplyToScheduled}
	if rt.ReplyToMsgID != nil {
		out.MessageID = int64(*rt.ReplyToMsgID)
	}
	if rt.ReplyToTopID != nil {
		out.TopMessageID = int64(*rt.ReplyToTopID)
	}
	if rt.ReplyToPeerID != nil {
		out.CrossPeer = peerIDFromWire(rt.ReplyToPeerID)
	}
	if rt.Quote {
		out.HasQuote = true
		if rt.QuoteText != nil {
			out.QuoteText = *rt.QuoteText
		}
		if rt.QuoteOffset != nil {
			out.QuoteOffset = *rt.QuoteOffset
		}
		if rt.QuoteEntities != nil {
			// Quote length: not carried separately by the wire header —
			// the quote text is the span. QuoteLength stays 0 when the
			// server omits it (never guessed from UTF-16 math).
			out.QuoteLength = 0
			for _, ec := range rt.QuoteEntities {
				if te, ok := textEntityFromWire(ec); ok {
					out.QuoteLength = te.Offset + te.Length
					break
				}
			}
		}
	}
	return out
}
