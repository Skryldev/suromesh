package client

// Mutation response mapping — the port of _get_response_message
// (reference/SPlusthon/splusthon/client/messageparse.py:113-231):
//
//	UpdateShort                  → single-update form
//	Updates / UpdatesCombined    → full update list + entity envelope
//	UpdateMessageID              → random_id → message id mapping
//	UpdateNewMessage /           → the message itself (id-keyed when the
//	UpdateNewChannelMessage        request carries random ids)
//	UpdateEditMessage            → edit result for non-channel peers,
//	                               matched by request id
//	UpdateEditChannelMessage     → edit result for channels, matched by
//	                               peer + request id
//
// The final random_id → id → message mapping (messageparse.py:196-231):
// sends/forwards map each request random_id; missing mappings surface as
// nil entries (never fabricated).

import (
	"fmt"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// responseMessage maps one mutation result to its domain message.
// randomIDs carries the request's random ids (sends/forwards); reqID is
// the explicit request id (edits/pins); either may be unused.
func (c *Client) responseMessage(ref Peer, randomIDs []int64, reqID int64, result tl.Object) (*Message, error) {
	msgs, err := c.responseMessageList(ref, randomIDs, reqID, result)
	if err != nil {
		return nil, err
	}
	// Edit/pin mapping matches by request id (messageparse.py:155-171);
	// the list-form conversion below handles it by id lookup, so for
	// single-id requests return the first non-nil.
	for _, m := range msgs {
		if m != nil {
			return m, nil
		}
	}
	// Fallback for edits answered with an update whose message id equals
	// the request id but arrived without random mapping.
	if reqID > 0 {
		if m := c.messageFromUpdatesByID(result, reqID); m != nil {
			return m, nil
		}
	}
	if len(msgs) > 0 {
		return msgs[0], nil
	}
	return nil, nil
}

// responseMessageList maps a result to one message per requested
// random id (forwards), preserving order with nil holes for missing
// mappings (messageparse.py:212-231); reqID drives the edit/pin
// id-matching form (messageparse.py:155-171).
func (c *Client) responseMessageList(ref Peer, randomIDs []int64, reqID int64, result tl.Object) ([]*Message, error) {
	var updates []gen.UpdateClass
	switch r := result.(type) {
	case *gen.UpdateShort:
		updates = []gen.UpdateClass{r.Update}
	case *gen.Updates:
		updates = r.Updates
		c.entities.seedUpdateEntities(r.Users, r.Chats)
	case *gen.UpdatesCombined:
		updates = r.Updates
		c.entities.seedUpdateEntities(r.Users, r.Chats)
	case *gen.UpdateShortSentMessage:
		// messages.py:930-944: synthesize the sent message from the short
		// form + the request's known peer/reply (server-authoritative
		// fields; the peer comes from the resolved entity).
		m := c.shortSentMessage(ref, r)
		return []*Message{m}, nil
	default:
		return nil, fmt.Errorf("messaging: mutation answered %T", result)
	}

	// Entity envelope → domain cache (messageparse.py:127-129 uses the
	// same envelope for conversion).
	_ = ref

	randomToID := make(map[int64]int32)
	idToMsg := make(map[int32]*gen.Message)
	var editsByID []*gen.Message
	for _, u := range updates {
		switch t := u.(type) {
		case *gen.UpdateMessageID:
			randomToID[t.RandomID] = t.ID
		case *gen.UpdateNewMessage:
			if m, ok := t.Message.(*gen.Message); ok {
				idToMsg[m.ID] = m
			}
		case *gen.UpdateNewChannelMessage:
			if m, ok := t.Message.(*gen.Message); ok {
				idToMsg[m.ID] = m
			}
		case *gen.UpdateEditMessage:
			if m, ok := t.Message.(*gen.Message); ok {
				editsByID = append(editsByID, m)
			}
		case *gen.UpdateEditChannelMessage:
			if m, ok := t.Message.(*gen.Message); ok {
				editsByID = append(editsByID, m)
			}
		}
	}

	if len(randomIDs) > 0 {
		out := make([]*Message, len(randomIDs))
		for i, rnd := range randomIDs {
			id, ok := randomToID[rnd]
			if !ok {
				continue // messageparse.py:212-231: nil for unmapped
			}
			if m, ok := idToMsg[id]; ok {
				out[i] = c.messageFromTL(m)
			}
		}
		return out, nil
	}

	// Edit/pin form: the edit update's message id equals the request id
	// (messageparse.py:163, 169); reqID ≤ 0 means the caller maps by
	// random id only.
	if reqID > 0 {
		for _, m := range editsByID {
			if m.ID == int32(reqID) {
				return []*Message{c.messageFromTL(m)}, nil
			}
		}
		// Some servers answer edits with the new message under
		// UpdateNewMessage-shaped families; fall back to id-keyed lookup.
		if m, ok := idToMsg[int32(reqID)]; ok {
			return []*Message{c.messageFromTL(m)}, nil
		}
	}
	return make([]*Message, len(randomIDs)), nil
}

// messageFromUpdatesByID is the edit-result fallback: walks the result
// (any shape) looking for a message with the given id.
func (c *Client) messageFromUpdatesByID(result tl.Object, id int64) *Message {
	var updates []gen.UpdateClass
	switch r := result.(type) {
	case *gen.UpdateShort:
		updates = []gen.UpdateClass{r.Update}
	case *gen.Updates:
		updates = r.Updates
	case *gen.UpdatesCombined:
		updates = r.Updates
	}
	for _, u := range updates {
		var m *gen.Message
		switch t := u.(type) {
		case *gen.UpdateEditMessage:
			m, _ = t.Message.(*gen.Message)
		case *gen.UpdateEditChannelMessage:
			m, _ = t.Message.(*gen.Message)
		case *gen.UpdateNewMessage:
			m, _ = t.Message.(*gen.Message)
		case *gen.UpdateNewChannelMessage:
			m, _ = t.Message.(*gen.Message)
		}
		if m != nil && int64(m.ID) == id {
			return c.messageFromTL(m)
		}
	}
	return nil
}

// shortSentMessage mirrors messages.py:930-944: UpdateShortSentMessage
// carries the sent message's authoritative fields minus the peer (known
// from the resolved request entity) and the reply header (known from the
// request).
func (c *Client) shortSentMessage(ref Peer, r *gen.UpdateShortSentMessage) *Message {
	m := &gen.Message{
		ID:       r.ID,
		Out:      r.Out,
		Date:     r.Date,
		Entities: r.Entities,
	}
	switch p := ref.(type) {
	case *User:
		m.PeerID = &gen.PeerUser{UserID: p.ID}
	case *Chat:
		m.PeerID = &gen.PeerChat{ChatID: p.ID}
	case *Channel:
		m.PeerID = &gen.PeerChannel{ChannelID: p.ID}
	}
	// Layer 182's UpdateShortSentMessage carries no reply header; the
	// reply relationship lives on the request and the server's subsequent
	// UpdateNewMessage — nothing is fabricated here.
	return c.messageFromTL(m)
}
