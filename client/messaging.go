package client

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// Message is the domain-level message.
type Message struct {
	ID     int64
	PeerID PeerID
	FromID PeerID
	Text   string
	Date   time.Time
	Out    bool
	// EditDate is the server-provided last-edit time (nil = never edited).
	EditDate *time.Time
	// ReplyToMessageID carries reply header information when present.
	ReplyToMessageID int64
	// Entities are the server-provided formatting entities (advanced
	// users; the raw constructors stay reachable here by design).
	// DEPRECATED for reading: prefer TextEntities (typed, Phase 14).
	Entities []gen.MessageEntityClass
	// TextEntities are the typed formatting entities (Phase 14). Offsets
	// and lengths are UTF-16 code units on the message text (wire
	// semantics — see docs/rich-text.md).
	TextEntities []TextEntity
	// UnknownTextEntities preserves unrecognized wire entities
	// (forward compatibility).
	UnknownTextEntities []UnknownTextEntity
	// Reply carries the reply metadata when the message replies to
	// another message or thread (Phase 14; nil = not a reply). Metadata
	// only — resolution is an explicit operation.
	Reply *ReplyInfo
	// Media is the canonical domain media (Phase 13): populated by the
	// single classifier path for every message source — history, search,
	// dialogs, mutation responses, updates. nil = no media.
	Media Media
	// GroupedID / InAlbum: album membership (server-provided grouped_id,
	// message.py:152-154/267).
	GroupedID int64
	InAlbum   bool
	// Views: view counter when the server provides one.
	Views int64
	// Forward: forward header when the message was forwarded (nil = not
	// forwarded). Only server-provided fields.
	Forward *ForwardInfo
	// Service / IsService: service-message representation (Phase 16).
	// IsService marks a MessageService payload; Service carries the typed
	// action (nil for normal messages).
	IsService bool
	Service   *ServiceInfo
	// Raw carries the underlying TL message object (advanced users may
	// inspect it; the domain fields above are the primary API).
	Raw tl.Object
}

// ForwardInfo is the domain form of MessageFwdHeader (server-provided
// fields only; zero values = absent).
type ForwardInfo struct {
	FromID      PeerID
	FromName    string
	Date        int32
	ChannelPost int64
}

// nilDate is the zero marker for "never edited".
var nilDate *time.Time

// SendMessageResult carries the server's answer to a send: either the
// short-update form (UpdateShortSentMessage — no full message object) or
// the full Updates form resolved to a Message.
type SendMessageResult struct {
	Message *Message
	Raw     tl.Object
}

// SendMessage sends text to a peer.
func (c *Client) SendMessage(ctx context.Context, peer Peer, text string) (*SendMessageResult, error) {
	input, err := c.inputPeer(peer)
	if err != nil {
		return nil, err
	}
	var rid [8]byte
	if _, err := randRead(rid[:]); err != nil {
		return nil, fmt.Errorf("client: random_id: %w", err)
	}
	req := &gen.MessagesSendMessageRequest{
		Peer:     input,
		Message:  text,
		RandomID: int64BE(rid),
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.messageFromUpdates(res)
}

// messageFromUpdates converts the send-result update family into a
// Message where one exists (UpdateShortSentMessage carries only pts state;
// the message object arrives via updates afterwards).
func (c *Client) messageFromUpdates(res tl.Object) (*SendMessageResult, error) {
	switch u := res.(type) {
	case *gen.UpdateShortSentMessage:
		return &SendMessageResult{Raw: u}, nil
	case *gen.Updates:
		if msg := c.firstMessage(u.Updates, u.Users); msg != nil {
			return &SendMessageResult{Message: msg, Raw: res}, nil
		}
		return &SendMessageResult{Raw: res}, nil
	case *gen.UpdatesCombined:
		if msg := c.firstMessage(u.Updates, u.Users); msg != nil {
			return &SendMessageResult{Message: msg, Raw: res}, nil
		}
		return &SendMessageResult{Raw: res}, nil
	case *gen.UpdateShort:
		return &SendMessageResult{Raw: res}, nil
	}
	return &SendMessageResult{Raw: res}, nil
}

func (c *Client) firstMessage(updates []gen.UpdateClass, users []gen.UserClass) *Message {
	for _, u := range updates {
		var m *gen.Message
		switch g := u.(type) {
		case *gen.UpdateNewMessage:
			m, _ = g.Message.(*gen.Message)
		case *gen.UpdateNewChannelMessage:
			m, _ = g.Message.(*gen.Message)
		}
		if m == nil {
			continue
		}
		return c.messageFromTL(m)
	}
	_ = users
	return nil
}

// messageFromTL converts one wire message into the domain form. Only
// server-provided fields are populated; the raw constructor stays
// reachable via Raw (§15). Accepts gen.MessageClass: a MessageService
// payload produces a domain Message with Service populated (Phase 16
// service-message representation).
func (c *Client) messageFromTL(m gen.MessageClass) *Message {
	switch sm := m.(type) {
	case *gen.MessageService:
		out := &Message{
			ID:        int64(sm.ID),
			Raw:       sm,
			Date:      time.Unix(int64(sm.Date), 0),
			Out:       sm.Out,
			IsService: true,
		}
		if p, ok := sm.PeerID.(*gen.PeerUser); ok {
			out.PeerID = PeerID{PeerUser, p.UserID}
		} else if p, ok := sm.PeerID.(*gen.PeerChat); ok {
			out.PeerID = PeerID{PeerChat, p.ChatID}
		} else if p, ok := sm.PeerID.(*gen.PeerChannel); ok {
			out.PeerID = PeerID{PeerChannel, p.ChannelID}
		}
		if f, ok := sm.FromID.(*gen.PeerUser); ok {
			out.FromID = PeerID{PeerUser, f.UserID}
		}
		out.Service = serviceInfoFromWire(sm.Action)
		return out
	}
	return c.messageFromTLInner(m.(*gen.Message))
}

// serviceInfoFromWire converts the service-message action into the typed
// domain form (known actions get flags; unknown ctors get their name).
func serviceInfoFromWire(a gen.MessageActionClass) *ServiceInfo {
	out := &ServiceInfo{}
	switch act := a.(type) {
	case *gen.MessageActionPinMessage:
		out.Pinned = true
	case *gen.MessageActionChatEditPhoto:
		out.ChatPhotoChanged = true
	case *gen.MessageActionChatCreate:
		out.ChatCreated = true
		out.Title = act.Title
	case *gen.MessageActionChannelCreate:
		out.ChatCreated = true
		out.Title = act.Title
	case *gen.MessageActionChatAddUser:
		out.MemberAdded = true
	case *gen.MessageActionChatDeleteUser:
		out.MemberRemoved = true
	case *gen.MessageActionChatJoinedByLink:
		out.MemberJoinedByLink = true
	default:
		out.KindName = fmt.Sprintf("%T", a)
	}
	return out
}

// ServiceInfo is the typed representation of a service message's action
// (server-provided semantics only; unknown actions carry KindName).
type ServiceInfo struct {
	Pinned             bool
	ChatPhotoChanged   bool
	ChatCreated        bool
	MemberAdded        bool
	MemberRemoved      bool
	MemberJoinedByLink bool
	Title              string
	KindName           string
}

func (c *Client) messageFromTLInner(m *gen.Message) *Message {
	out := &Message{
		Raw:      m,
		ID:       int64(m.ID),
		Text:     m.Message,
		Date:     time.Unix(int64(m.Date), 0),
		Out:      m.Out,
		EditDate: nilDate,
	}
	if m.EditDate != nil {
		ed := time.Unix(int64(*m.EditDate), 0)
		out.EditDate = &ed
	}
	if m.ReplyTo != nil {
		if rt, ok := m.ReplyTo.(*gen.MessageReplyHeader); ok {
			if rt.ReplyToMsgID != nil {
				out.ReplyToMessageID = int64(*rt.ReplyToMsgID)
			}
			out.Reply = replyInfoFromWire(rt)
		}
	}
	out.Entities = make([]gen.MessageEntityClass, 0, len(m.Entities))
	out.Entities = append(out.Entities, m.Entities...)
	out.TextEntities, out.UnknownTextEntities = textEntitiesFromWire(m.Entities)
	out.Media = classifyMedia(m.Media)
	if m.GroupedID != nil {
		out.GroupedID = *m.GroupedID
		out.InAlbum = true
	}
	if m.Views != nil {
		out.Views = int64(*m.Views)
	}
	if f, ok := m.FwdFrom.(*gen.MessageFwdHeader); ok {
		fwd := ForwardInfo{Date: int32(0)}
		if f.Date != 0 {
			fwd.Date = f.Date
		}
		if f.FromID != nil {
			fwd.FromID = peerIDFromWire(f.FromID)
		}
		if f.FromName != nil {
			fwd.FromName = *f.FromName
		}
		if f.ChannelPost != nil {
			fwd.ChannelPost = int64(*f.ChannelPost)
		}
		out.Forward = &fwd
	}
	if p, ok := m.PeerID.(*gen.PeerUser); ok {
		out.PeerID = PeerID{PeerUser, p.UserID}
	} else if p, ok := m.PeerID.(*gen.PeerChat); ok {
		out.PeerID = PeerID{PeerChat, p.ChatID}
	} else if p, ok := m.PeerID.(*gen.PeerChannel); ok {
		out.PeerID = PeerID{PeerChannel, p.ChannelID}
	}
	if f, ok := m.FromID.(*gen.PeerUser); ok {
		out.FromID = PeerID{PeerUser, f.UserID}
	} else if f, ok := m.FromID.(*gen.PeerChat); ok {
		out.FromID = PeerID{PeerChat, f.ChatID}
	} else if f, ok := m.FromID.(*gen.PeerChannel); ok {
		out.FromID = PeerID{PeerChannel, f.ChannelID}
	}
	return out
}

// GetMe returns the logged-in user (reference users.getUsers([self])).
// The authenticated account is integrated into the entity cache and
// recorded so peers address it as InputPeerSelf (Phase 11 §8).
func (c *Client) GetMe(ctx context.Context) (*User, error) {
	res, err := c.Invoke(ctx, &gen.UsersGetUsersRequest{
		ID: []gen.InputUserClass{&gen.InputUserSelf{}},
	})
	if err != nil {
		return nil, err
	}
	// users.getUsers = Vector<User>: the dispatcher answers with
	// *rpc.VectorResult (reference tgread_object list semantics).
	vr, ok := res.(*rpc.VectorResult)
	if !ok || len(vr.Items) == 0 {
		return nil, fmt.Errorf("client: getMe answered %T", res)
	}
	u, ok := vr.Items[0].(*gen.User)
	if !ok {
		return nil, fmt.Errorf("client: getMe answered empty user (%T)", vr.Items[0])
	}
	c.entities.seedUser(u)
	atomic.StoreInt64(&c.selfID, u.ID)
	if ent, found := c.entities.get(PeerID{PeerUser, u.ID}); found {
		if eu, ok := ent.(*User); ok {
			return eu, nil
		}
	}
	return &User{ID: u.ID}, nil
}

// GetMessages fetches message history for a peer (newest → oldest).
// Backward-compatible Phase 6 API: implemented on top of the Phase 11
// history iterator (single pagination implementation, docs/history.md).
func (c *Client) GetMessages(ctx context.Context, peer Peer, opts MessageQuery) ([]*Message, error) {
	it, err := c.IterMessages(ctx, peer, MessageQuery{
		Limit:    opts.Limit,
		OffsetID: opts.OffsetID,
		MaxID:    opts.MaxID,
		MinID:    opts.MinID,
	})
	if err != nil {
		return nil, err
	}
	var out []*Message
	for {
		m, ok := it.Next(ctx)
		if !ok {
			return out, it.Err()
		}
		out = append(out, m)
		if opts.Limit > 0 && int32(len(out)) >= opts.Limit {
			return out, nil
		}
	}
}

// ---- small helpers ---------------------------------------------------------

func randRead(p []byte) (int, error) {
	return cryptoRandRead(p)
}

func int64BE(b [8]byte) int64 {
	var v int64
	for _, x := range b {
		v = v<<8 | int64(x)
	}
	return v
}

// classMessageID extracts the wire id from any message-class payload
// (Message, MessageService, MessageEmpty).
func classMessageID(mc gen.MessageClass) int32 {
	switch m := mc.(type) {
	case *gen.Message:
		return m.ID
	case *gen.MessageService:
		return m.ID
	case *gen.MessageEmpty:
		return m.ID
	}
	return 0
}
