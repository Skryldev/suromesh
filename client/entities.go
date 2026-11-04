package client

import (
	"errors"
	"github.com/Skryldev/suromesh/tl"
	"sync"

	"github.com/Skryldev/suromesh/tl/gen"
)

// PeerKind discriminates peer types.
type PeerKind int

const (
	PeerUser PeerKind = iota
	PeerChat
	PeerChannel
)

func (k PeerKind) String() string {
	switch k {
	case PeerUser:
		return "user"
	case PeerChat:
		return "chat"
	case PeerChannel:
		return "channel"
	}
	return "?"
}

// PeerID is the stable identifier of a peer (marked like the reference:
// chat/channel share the negative-space convention in helpers, but here we
// keep the kind explicit to avoid fabricating hashes).
type PeerID struct {
	Kind PeerKind
	ID   int64
}

// Peer is anything addressable as a message destination. Every Peer is
// also a PeerRef (Phase 11): domain entities flow straight into the
// resolution layer.
type Peer interface {
	peerID() PeerID
	peerRef()
}

// User is a domain-level user (converted from gen.User).
type User struct {
	ID         int64
	AccessHash int64
	Username   string
	FirstName  string
	LastName   string
	IsBot      bool
}

func (u *User) peerID() PeerID { return PeerID{PeerUser, u.ID} }

// Chat is a basic group (no access hash required). MigratedTo carries
// the server-provided migration target when the group upgraded to a
// supergroup (nil otherwise; never fabricated).
type Chat struct {
	ID         int64
	Title      string
	MigratedTo *Channel
}

func (c *Chat) peerID() PeerID { return PeerID{PeerChat, c.ID} }

// Channel is a supergroup/channel (access hash required for most RPCs).
type Channel struct {
	ID         int64
	AccessHash int64
	Username   string
	Title      string
	MigratedTo *Channel
}

func (c *Channel) peerID() PeerID { return PeerID{PeerChannel, c.ID} }

// peerID implements Peer for the bare identifier form.
func (p PeerID) peerID() PeerID { return p }

// Compile-time interface checks.
var (
	_ Peer = (*User)(nil)
	_ Peer = (*Chat)(nil)
	_ Peer = (*Channel)(nil)
	_ Peer = PeerID{}
)

// entityCache stores access hashes and domain entities learned from
// traffic. Bounded; insert evicts nothing until capacity, then the cache
// refuses new entries (retention favors early-learned core peers — see
// docs/client.md for the trade-off discussion).
type entityCache struct {
	mu  sync.RWMutex
	m   map[PeerID]any // *User | *Chat | *Channel
	cap int
}

func newEntityCache(capacity int) *entityCache {
	return &entityCache{m: make(map[PeerID]any, 64), cap: capacity}
}

// put stores or UPDATES an entry: a re-resolution replaces the cached
// entity (access hash rotation is normal server behavior). Capacity only
// gates NEW keys — updates to known peers always apply.
func (e *entityCache) put(p PeerID, v any) {
	e.mu.Lock()
	if _, exists := e.m[p]; exists || len(e.m) < e.cap {
		e.m[p] = v
	}
	e.mu.Unlock()
}

// invalidate drops one entity (e.g. an authorization revocation for that
// peer).
func (e *entityCache) invalidate(p PeerID) {
	e.mu.Lock()
	delete(e.m, p)
	e.mu.Unlock()
}

// clear drops every cached entity.
func (e *entityCache) clear() {
	e.mu.Lock()
	e.m = make(map[PeerID]any, 64)
	e.mu.Unlock()
}

// size reports the cached count (diagnostics/tests).
func (e *entityCache) size() int {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return len(e.m)
}

func (e *entityCache) get(p PeerID) (any, bool) {
	e.mu.RLock()
	v, ok := e.m[p]
	e.mu.RUnlock()
	return v, ok
}

// seedUser/chat/channel record learned entities.
func (e *entityCache) seedUser(u *gen.User) {
	if u == nil || u.AccessHash == nil {
		return // min entities lack usable hashes; never fabricate
	}
	d := &User{
		ID:         u.ID,
		AccessHash: *u.AccessHash,
		Username:   derefString(u.Username),
		FirstName:  derefString(u.FirstName),
		LastName:   derefString(u.LastName),
		IsBot:      u.Bot,
	}
	e.put(PeerID{PeerUser, u.ID}, d)
}

// seedUpdateEntities stores the users/chats carried by update envelopes
// (reference _preprocess_updates → _mb_entity_cache.extend). Only
// server-provided entities are stored; access hashes rotate when the
// server sends newer values (put's update-in-place rule).
func (e *entityCache) seedUpdateEntities(users []gen.UserClass, chats []gen.ChatClass) {
	for i := range users {
		if u, ok := users[i].(*gen.User); ok {
			e.seedUser(u)
		}
	}
	for i := range chats {
		switch c := chats[i].(type) {
		case *gen.Chat:
			e.seedChat(c)
		case *gen.ChatForbidden:
			e.seedChatForbidden(c)
		case *gen.Channel:
			e.seedChannel(c)
		case *gen.ChannelForbidden:
			e.seedChannelForbidden(c)
		}
	}
}

func (e *entityCache) seedChat(c *gen.Chat) {
	if c == nil {
		return
	}
	d := &Chat{ID: c.ID, Title: c.Title, MigratedTo: channelFromWire(c.MigratedTo)}
	e.put(PeerID{PeerChat, c.ID}, d)
}

func (e *entityCache) seedChannel(c *gen.Channel) {
	if c == nil {
		return
	}
	d := &Channel{ID: c.ID, AccessHash: derefI64(c.AccessHash), Username: derefString(c.Username), Title: c.Title}
	e.put(PeerID{PeerChannel, c.ID}, d)
}

// seedChatForbidden records the existence of a forbidden chat (title
// only, no access hash exists for it — nothing to fabricate).
func (e *entityCache) seedChatForbidden(c *gen.ChatForbidden) {
	if c == nil {
		return
	}
	e.put(PeerID{PeerChat, c.ID}, &Chat{ID: c.ID, Title: c.Title})
}

// seedChannelForbidden records the existence of a forbidden channel. The
// schema gives it an access hash; store it so later difference requests
// for this channel can use the real value.
func (e *entityCache) seedChannelForbidden(c *gen.ChannelForbidden) {
	if c == nil {
		return
	}
	e.put(PeerID{PeerChannel, c.ID}, &Channel{
		ID: c.ID, AccessHash: derefI64(&c.AccessHash), Title: c.Title,
	})
}

func derefString(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func derefI64(v *int64) int64 {
	if v == nil {
		return 0
	}
	return *v
}

// extractEntities feeds the cache from a decoded RPC result. It covers
// the response families this phase produces (user vectors, resolved peers,
// updates with user/chat vectors, message history). Unknown shapes are
// skipped (documented); SeedEntities exists for anything else.
func (c *Client) extractEntities(res interface{ CtorID() uint32 }) {
	switch r := res.(type) {
	case *gen.User:
		c.entities.seedUser(r)
	case *gen.ContactsResolvedPeer:
		for _, u := range r.Users {
			if gu, ok := u.(*gen.User); ok {
				c.entities.seedUser(gu)
			}
		}
		for _, ch := range r.Chats {
			seedChatAny(c.entities, ch)
		}
	case *gen.Updates:
		for _, u := range r.Users {
			if gu, ok := u.(*gen.User); ok {
				c.entities.seedUser(gu)
			}
		}
		for _, ch := range r.Chats {
			seedChatAny(c.entities, ch)
		}
	case *gen.UpdatesCombined:
		for _, u := range r.Users {
			if gu, ok := u.(*gen.User); ok {
				c.entities.seedUser(gu)
			}
		}
		for _, ch := range r.Chats {
			seedChatAny(c.entities, ch)
		}
	case *gen.MessagesMessages:
		seedUsersChats(c.entities, r.Users, r.Chats)
	case *gen.MessagesMessagesSlice:
		seedUsersChats(c.entities, r.Users, r.Chats)
	case *gen.MessagesChannelMessages:
		seedUsersChats(c.entities, r.Users, r.Chats)
	case *gen.MessagesDialogs:
		seedUsersChats(c.entities, r.Users, r.Chats)
	case *gen.MessagesDialogsSlice:
		seedUsersChats(c.entities, r.Users, r.Chats)
	}
}

func seedUsersChats(e *entityCache, users []gen.UserClass, chats []gen.ChatClass) {
	for _, u := range users {
		if gu, ok := u.(*gen.User); ok {
			e.seedUser(gu)
		}
	}
	for _, ch := range chats {
		seedChatAny(e, ch)
	}
}

func seedChatAny(e *entityCache, ch gen.ChatClass) {
	switch g := ch.(type) {
	case *gen.Chat:
		e.seedChat(g)
	case *gen.Channel:
		e.seedChannel(g)
	}
}

// ExportedEntity is one entity for SeedEntities.
type ExportedEntity struct{ Raw tl.Object }

// SeedRaw feeds a raw TL object through the entity extractor (used by
// benchmarks and by callers holding raw results).
func (c *Client) SeedRaw(objs ...tl.Object) {
	for _, o := range objs {
		c.extractEntities(o)
	}
}

// InvalidateEntity drops one cached peer (test/advanced use).
func (c *Client) InvalidateEntity(p PeerID) { c.entities.invalidate(p) }

// ClearEntities drops every cached peer.
func (c *Client) ClearEntities() { c.entities.clear() }

// CachedEntities reports the cache size (diagnostics).
func (c *Client) CachedEntities() int { return c.entities.size() }

// SeedEntities lets callers pre-populate resolution state (e.g. from a
// persisted entity database) for peers the traffic has not revealed.
func (c *Client) SeedEntities(users []*User, chats []*Chat, channels []*Channel) {
	for _, u := range users {
		c.entities.put(PeerID{PeerUser, u.ID}, u)
	}
	for _, ch := range chats {
		c.entities.put(PeerID{PeerChat, ch.ID}, ch)
	}
	for _, ch := range channels {
		c.entities.put(PeerID{PeerChannel, ch.ID}, ch)
	}
}

// inputPeer converts a Peer to the wire InputPeer, consulting the entity
// cache when only a bare PeerID is available.
func (c *Client) inputPeer(p Peer) (gen.InputPeerClass, error) {
	switch v := p.(type) {
	case *User:
		return &gen.InputPeerUser{UserID: v.ID, AccessHash: v.AccessHash}, nil
	case *Chat:
		return &gen.InputPeerChat{ChatID: v.ID}, nil
	case *Channel:
		return &gen.InputPeerChannel{ChannelID: v.ID, AccessHash: v.AccessHash}, nil
	case PeerID:
		// Reference: the authenticated account is addressed as
		// InputPeerSelf, never InputPeerUser (users.py:442-444).
		if v.Kind == PeerUser && v.ID == c.selfUserID() {
			return &gen.InputPeerSelf{}, nil
		}
		if ent, ok := c.entities.get(v); ok {
			switch e := ent.(type) {
			case *User:
				return &gen.InputPeerUser{UserID: e.ID, AccessHash: e.AccessHash}, nil
			case *Chat:
				return &gen.InputPeerChat{ChatID: e.ID}, nil
			case *Channel:
				return &gen.InputPeerChannel{ChannelID: e.ID, AccessHash: e.AccessHash}, nil
			}
		}
		if v.Kind == PeerChat {
			// Basic chats need no access hash.
			return &gen.InputPeerChat{ChatID: v.ID}, nil
		}
		return nil, &ErrAccessHashUnavailable{Peer: v}
	case gen.InputPeerClass:
		return v, nil
	}
	return nil, errors.New("client: unsupported peer type")
}

// channelFromWire converts a wire InputChannel (server-provided ID+hash)
// into the domain form; nil stays nil — nothing is fabricated.
func channelFromWire(ic gen.InputChannelClass) *Channel {
	if in, ok := ic.(*gen.InputChannel); ok {
		return &Channel{ID: in.ChannelID, AccessHash: in.AccessHash}
	}
	return nil
}
