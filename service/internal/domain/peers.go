package domain

import (
	"fmt"
	"strconv"
	"strings"
)

// PeerKind classifies addressable upstream entities.
type PeerKind int32

const (
	PeerKindUser    PeerKind = 1
	PeerKindChat    PeerKind = 2
	PeerKindChannel PeerKind = 3
)

func (k PeerKind) String() string {
	switch k {
	case PeerKindUser:
		return "user"
	case PeerKindChat:
		return "chat"
	case PeerKindChannel:
		return "channel"
	default:
		return "unspecified"
	}
}

// PeerID is the stable identity of a peer: (kind, upstream id).
type PeerID struct {
	Kind PeerKind
	ID   int64
}

// Canonical renders the wire-safe canonical form ("user:123").
func (p PeerID) Canonical() string {
	return p.Kind.String() + ":" + strconv.FormatInt(p.ID, 10)
}

// ParsePeerID parses the canonical form.
func ParsePeerID(s string) (PeerID, error) {
	kind, id, ok := strings.Cut(s, ":")
	if !ok {
		return PeerID{}, fmt.Errorf("%w: peer id %q is not kind:id", ErrInvalidInput, s)
	}
	var k PeerKind
	switch kind {
	case "user":
		k = PeerKindUser
	case "chat":
		k = PeerKindChat
	case "channel":
		k = PeerKindChannel
	default:
		return PeerID{}, fmt.Errorf("%w: unknown peer kind %q", ErrInvalidInput, kind)
	}
	id64, err := strconv.ParseInt(id, 10, 64)
	if err != nil || id64 <= 0 {
		return PeerID{}, fmt.Errorf("%w: peer id %q", ErrInvalidInput, s)
	}
	return PeerID{Kind: k, ID: id64}, nil
}

// PeerRefKind discriminates the reference form.
type PeerRefKind int32

const (
	RefKindPeerID   PeerRefKind = 0
	RefKindUsername PeerRefKind = 1
	RefKindSelf     PeerRefKind = 2
)

// PeerRef references a peer for resolution: canonical id, username, or
// the bound account itself. Exactly one form is meaningful (the Kind
// field selects it).
type PeerRef struct {
	Kind     PeerRefKind
	ID       PeerID // when Kind == RefKindPeerID
	Username string // when Kind == RefKindUsername (no leading '@')
}

// RefPeerID builds an id reference.
func RefPeerID(id PeerID) PeerRef { return PeerRef{Kind: RefKindPeerID, ID: id} }

// RefUsername builds a username reference (no leading '@').
func RefUsername(u string) PeerRef { return PeerRef{Kind: RefKindUsername, Username: u} }

// RefSelf builds the self reference.
func RefSelf() PeerRef { return PeerRef{Kind: RefKindSelf} }

func (r PeerRef) String() string {
	switch r.Kind {
	case RefKindPeerID:
		return r.ID.Canonical()
	case RefKindUsername:
		return "@" + r.Username
	default:
		return "self"
	}
}

// Peer is a resolved peer summary (server-provided data only; the
// service never fabricates identity material).
type Peer struct {
	ID          PeerID
	DisplayName string
	Username    string // optional
}

func (p Peer) Ref() PeerRef { return RefPeerID(p.ID) }

// PeerView selects BASIC (cached/refreshed summary) or FULL (full-info
// upstream query).
type PeerView int32

const (
	PeerViewUnspecified PeerView = 0
	PeerViewBasic       PeerView = 1
	PeerViewFull        PeerView = 2
)

// UserDetails is the user projection (GetUser).
type UserDetails struct {
	Peer  Peer
	IsBot bool
	// FULL-view extensions are added by the adapter when available.
}

// ChannelDetails is the channel projection (GetChannel).
type ChannelDetails struct {
	Peer              Peer
	ParticipantsCount int32
}

// ChatDetails is the basic-chat projection (GetChat).
type ChatDetails struct {
	Peer              Peer
	ParticipantsCount int32
}
