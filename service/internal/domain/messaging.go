package domain

import (
	"fmt"
	"time"
)

// Direction of a message relative to the bound account.
type Direction int32

const (
	DirectionIncoming Direction = 1
	DirectionOutgoing Direction = 2
)

// EntityKind classifies message text entities. Offsets/lengths are
// UTF-16 code units (protocol parity — matters for Persian text and
// emoji).
type EntityKind int32

const (
	EntityMention EntityKind = 1
	EntityHashtag EntityKind = 2
	EntityURL     EntityKind = 3
	EntityBold    EntityKind = 4
	EntityItalic  EntityKind = 5
	EntityCode    EntityKind = 6
	EntityPre     EntityKind = 7
	EntityOther   EntityKind = 8
)

// Entity is one text entity span.
type Entity struct {
	Kind   EntityKind
	Offset int32
	Length int32
	URL    string // payload for URL/mention kinds when provided
}

// MediaKind classifies message media.
type MediaKind int32

const (
	MediaKindPhoto       MediaKind = 1
	MediaKindDocument    MediaKind = 2
	MediaKindUnsupported MediaKind = 3
)

// MediaSummary is the shape-safe media description carried on messages.
type MediaSummary struct {
	Kind         MediaKind
	SizeBytes    int64
	MimeType     string
	FileName     string
	Downloadable bool // false for cached/stripped inline content
}

// Message is the domain message. Identity is (PeerID, ID) — never the
// id alone (upstream ids are per-peer sequences).
type Message struct {
	ID          int64
	PeerID      PeerID
	Direction   Direction
	Text        string
	Entities    []Entity
	ReplyTo     *int64
	Date        time.Time
	Media       *MediaSummary
	IsService   bool
	ServiceKind string // "pinned", "member_added", ... when IsService
}

// Key renders the message identity as peer id + ":" + message id.
func (m Message) Key() string {
	return m.PeerID.Canonical() + "#" + fmt.Sprintf("%d", m.ID)
}

// Dialog is one entry of the account's conversation list (derived view;
// never the authority).
type Dialog struct {
	Peer            Peer
	TopMessageID    int64
	UnreadCount     int32
	UnreadMentions  int32
	ReadInboxMaxID  int64
	ReadOutboxMaxID int64
	Pinned          bool
	LastActivity    time.Time
}

// DialogPage is one page of dialogs with an opaque continuation token.
type DialogPage struct {
	Dialogs       []Dialog
	NextPageToken string
	TotalEstimate int32 // server estimate; 0 when unknown
}
