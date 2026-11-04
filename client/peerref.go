package client

// Peer resolution (Phase 11): strongly typed peer references and the
// resolution cascade, ported from the reference:
//
//	get_input_entity        reference/SPlusthon/splusthon/client/users.py:364-490
//	_get_entity_from_string users.py:534-596
//	get_input_peer          utils.py:136-231 (hash validation, min-entity rule)
//	get_peer_id             utils.py:1004-1053 (marked IDs)
//	resolve_id              utils.py:1055-1066
//
// The three concepts stay distinct:
//
//	Entity      server-returned domain info (User/Chat/Channel)
//	PeerRef     what the caller uses to ADDRESS an entity
//	InputPeer   the minimal wire identity (InputPeerUser/Chat/Channel/Self)
//
// Access hashes are never fabricated: users/channels resolve from the
// cache or from server responses that carry the hash; without one the
// resolution fails with ErrAccessHashUnavailable. Basic chats need no
// hash (reference utils.get_input_peer / get_input_entity PeerChat branch).

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Skryldev/suromesh/tl/gen"
)

// PeerRef is anything that can be resolved to a Peer. Every domain Peer
// (*User, *Chat, *Channel, PeerID) implements it, plus the
// reference-style string forms (UsernameRef, SelfRef).
type PeerRef interface{ peerRef() }

func (u *User) peerRef()     {}
func (c *Chat) peerRef()     {}
func (ch *Channel) peerRef() {}
func (p PeerID) peerRef()    {}

// UsernameRef addresses an entity by username ("@name" or "name").
// Resolution goes through contacts.resolveUsername; the server response
// decides the peer type — never the string's shape.
type UsernameRef string

func (u UsernameRef) peerRef() {}

// SelfRef addresses the authenticated account ("me"/"self" in the
// reference; resolves to InputPeerSelf on the wire).
type SelfRef struct{}

func (SelfRef) peerRef() {}

// ErrPeerNotFound mirrors the reference's final ValueError
// (users.py:485-490): nothing in the cache and no server path applies.
var ErrPeerNotFound = errors.New("peer: could not find the input entity")

// ErrInvalidPeer reports a malformed peer reference (empty username,
// non-positive ID, unknown shape).
var ErrInvalidPeer = errors.New("peer: invalid peer reference")

// MarkedID returns the bot-API style marked identifier for a peer kind:
// +id for users, -id for chats, -(1e12 + id) for channels
// (reference utils.get_peer_id, utils.py:1004-1053).
func MarkedID(p PeerID) int64 {
	switch p.Kind {
	case PeerUser:
		return p.ID
	case PeerChat:
		return -p.ID
	default: // PeerChannel
		return -(1000000000000 + p.ID)
	}
}

// ResolveMarkedID splits a marked ID back into its peer kind and raw ID
// (reference utils.resolve_id, utils.py:1055-1066).
func ResolveMarkedID(marked int64) (PeerID, bool) {
	switch {
	case marked >= 0:
		return PeerID{PeerUser, marked}, true
	case marked > -1000000000000: // -(1..1e12): chat
		return PeerID{PeerChat, -marked}, true
	case marked < -1000000000000:
		return PeerID{PeerChannel, -(marked + 1000000000000)}, true
	}
	return PeerID{}, false
}

// peerIDFromWire converts a wire PeerClass into the domain PeerID.
func peerIDFromWire(p gen.PeerClass) PeerID {
	switch v := p.(type) {
	case *gen.PeerUser:
		return PeerID{PeerUser, v.UserID}
	case *gen.PeerChat:
		return PeerID{PeerChat, v.ChatID}
	case *gen.PeerChannel:
		return PeerID{PeerChannel, v.ChannelID}
	}
	return PeerID{}
}

// ResolvePeer resolves any PeerRef to the domain entity (User, Chat or
// Channel), following the reference's cascade:
//
//  1. already an entity → validated as-is
//  2. cache hit by marked ID (no network)
//  3. self → GetMe (authoritative; cached)
//  4. username → contacts.resolveUsername (server answer is authoritative)
//  5. numeric user/channel without a hash → ErrAccessHashUnavailable
//     (deviation: the reference's bot-only hash=0 fallback is not
//     attempted — see docs/peers.md)
//  6. numeric chat → always addressable (no hash in the protocol)
func (c *Client) ResolvePeer(ctx context.Context, ref PeerRef) (Peer, error) {
	switch v := ref.(type) {
	case nil:
		return nil, ErrInvalidPeer
	case *User:
		return v, nil
	case *Chat:
		return v, nil
	case *Channel:
		return v, nil
	case PeerID:
		return c.resolvePeerID(ctx, v)
	case SelfRef:
		return c.resolveSelf(ctx)
	case UsernameRef:
		return c.ResolveUsername(ctx, string(v))
	default:
		return nil, fmt.Errorf("%w: unsupported ref %T", ErrInvalidPeer, ref)
	}
}

// ResolvePeerString resolves the reference's string entity forms:
// "me"/"self", usernames (with or without @), and marked numeric IDs.
func (c *Client) ResolvePeerString(ctx context.Context, s string) (Peer, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "me", "self":
		return c.resolveSelf(ctx)
	}
	if strings.HasPrefix(s, "@") {
		return c.ResolveUsername(ctx, s[1:])
	}
	// Marked numeric ID (reference get_input_entity int branch: users are
	// positive, chats negated, channels -100-prefixed).
	if isAllDigits(s) {
		marked := parseI64(s)
		id, ok := ResolveMarkedID(marked)
		if !ok {
			return nil, fmt.Errorf("%w: marked id %d", ErrInvalidPeer, marked)
		}
		return c.resolvePeerID(ctx, id)
	}
	return c.ResolveUsername(ctx, s)
}

// resolvePeerID resolves a bare peer ID through the entity cache. This
// never touches the network for users/channels: without a cached access
// hash there is nothing safe to send (ErrAccessHashUnavailable). Chats
// need no hash and are always addressable.
func (c *Client) resolvePeerID(ctx context.Context, id PeerID) (Peer, error) {
	_ = ctx
	if id.ID <= 0 {
		return nil, fmt.Errorf("%w: non-positive id %d", ErrInvalidPeer, id.ID)
	}
	if ent, ok := c.entities.get(id); ok {
		switch e := ent.(type) {
		case *User:
			return e, nil
		case *Chat:
			return e, nil
		case *Channel:
			return e, nil
		}
	}
	if id.Kind == PeerChat {
		// Basic groups carry no access hash on the wire
		// (reference: PeerChat branch returns InputPeerChat directly).
		return &Chat{ID: id.ID}, nil
	}
	return nil, &ErrAccessHashUnavailable{Peer: id}
}

// resolveSelf returns the authenticated account via GetMe (authoritative)
// and integrates it into the entity cache (reference get_input_entity
// 'me'/'self' branch → InputPeerSelf; get_me seeds the cache).
func (c *Client) resolveSelf(ctx context.Context) (Peer, error) {
	me, err := c.GetMe(ctx)
	if err != nil {
		return nil, err
	}
	return me, nil
}

// InputPeerSelf reports whether this peer should be addressed as
// InputPeerSelf (reference semantics: the wire form for self).
func (c *Client) InputPeerSelf() gen.InputPeerClass {
	return &gen.InputPeerSelf{}
}

// isSelfInputPeer reports whether the wire peer is InputPeerSelf.

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseI64(s string) int64 {
	var v int64
	for _, r := range s {
		v = v*10 + int64(r-'0')
	}
	return v
}

// selfUserID returns the authenticated account's ID (0 = unknown).
func (c *Client) selfUserID() int64 { return atomic.LoadInt64(&c.selfID) }
