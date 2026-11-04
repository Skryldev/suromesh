package client

// Message mutations (Phase 12): reply, edit, delete, forward, pin —
// ported from the reference:
//
//	send_message      reference/SPlusthon/splusthon/client/messages.py:631-946
//	forward_messages  messages.py:948-1085
//	edit_message      messages.py:1087-1263
//	delete_messages   messages.py:1265-1336
//	pin/unpin (_pin)  messages.py:1429-1524
//	_get_response_message  messageparse.py:113-231
//
// Update consistency (§22/§23): every mutation's result flows through the
// EXISTING pipeline — the rpc dispatcher wraps update-family results as
// OwnUpdate (state-only), the caller consumes the same result for its
// return value, and no mutation dispatches events directly. Duplicate
// logical events are structurally impossible: the MessageBox is the only
// path to subscribers.

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// mutationChunkSize matches the reference's delete chunking
// (messages.py:1332-1336 — utils.chunks default 100).
const mutationChunkSize = 100

// SendMessageOptions carries the optional send parameters the reference
// supports for plain-text sends (messages.py:631-654); only fields with
// real protocol effect are exposed.
type SendMessageOptions struct {
	// ReplyTo is the message ID to reply to (messages.py:684-686). It is
	// placed in InputReplyToMessage (messages.py:897, 920).
	ReplyTo int64
	// Silent suppresses notifications (messages.py:739-742).
	Silent bool
	// NoWebpage disables link preview (messages.py:701-702, 919).
	NoWebpage bool
	// ClearDraft clears the chat draft (messages.py:722-723).
	ClearDraft bool
}

// Reply sends a text message replying to the given message ID
// (reference send_message's reply_to path, messages.py:864-865, 920).
func (c *Client) Reply(ctx context.Context, peer PeerRef, replyTo int64, text string) (*Message, error) {
	return c.SendMessageOpts(ctx, peer, text, SendMessageOptions{ReplyTo: replyTo})
}

// SendMessageOpts is SendMessage with the reference's optional parameters.
func (c *Client) SendMessageOpts(ctx context.Context, peer PeerRef, text string, opts SendMessageOptions) (*Message, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	if text == "" {
		return nil, errors.New("messaging: the message cannot be empty")
	}
	var replyTo gen.InputReplyToClass
	if opts.ReplyTo > 0 {
		replyTo = &gen.InputReplyToMessage{ReplyToMsgID: int32(opts.ReplyTo)}
	}
	var rid [8]byte
	if _, err := randRead(rid[:]); err != nil {
		return nil, fmt.Errorf("messaging: random_id: %w", err)
	}
	req := &gen.MessagesSendMessageRequest{
		Peer:       input,
		ReplyTo:    replyTo,
		Message:    text,
		RandomID:   int64BE(rid),
		NoWebpage:  opts.NoWebpage,
		Silent:     opts.Silent,
		ClearDraft: opts.ClearDraft,
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.responseMessage(ref, []int64{req.RandomID}, int64(0), res)
}

// EditMessage edits a text message (reference edit_message,
// messages.py:1087-1263). Returns the authoritative edited message
// derived from the RPC result — never a locally mutated copy.
func (c *Client) EditMessage(ctx context.Context, peer PeerRef, messageID int64, text string) (*Message, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	if messageID <= 0 {
		return nil, fmt.Errorf("%w: invalid message id %d", ErrInvalidPeer, messageID)
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	txt := text
	req := &gen.MessagesEditMessageRequest{
		Peer:    input,
		ID:      int32(messageID),
		Message: &txt,
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	return c.responseMessage(ref, nil, messageID, res)
}

// DeleteMessagesOptions carries the delete flags (messages.py:1292-1304).
type DeleteMessagesOptions struct {
	// Revoke deletes for everyone (reference default true,
	// messages.py:1294-1304; ignored for channels — channels always
	// revoke).
	Revoke bool
}

// DeleteMessages deletes messages (reference delete_messages,
// messages.py:1265-1336). Channels route to channels.deleteMessages;
// everything else to messages.deleteMessages with the revoke flag.
// IDs are chunked at 100 per request (reference utils.chunks); the
// returned slice holds one AffectedMessages per chunk, in order. A
// failing chunk aborts with its error — partial results are NOT
// swallowed (§25).
func (c *Client) DeleteMessages(ctx context.Context, peer PeerRef, messageIDs []int64, opts DeleteMessagesOptions) ([]tl.Object, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return nil, nil // reference: empty input → empty result
	}
	isChannel := false
	var channel *gen.InputChannel
	if ch, ok := ref.(*Channel); ok {
		isChannel = true
		channel = &gen.InputChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
	}
	ids := make([]int32, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			return nil, fmt.Errorf("%w: invalid message id %d", ErrInvalidPeer, id)
		}
		ids = append(ids, int32(id))
	}

	var out []tl.Object
	for start := 0; start < len(ids); start += mutationChunkSize {
		end := start + mutationChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		var req tl.Request
		if isChannel {
			req = &gen.ChannelsDeleteMessagesRequest{Channel: channel, ID: chunk}
		} else {
			req = &gen.MessagesDeleteMessagesRequest{ID: chunk, Revoke: opts.Revoke}
		}
		res, err := c.Invoke(ctx, req)
		if err != nil {
			return out, err
		}
		out = append(out, res)
	}
	return out, nil
}

// ForwardOptions carries the forward flags (messages.py:984-1008).
type ForwardOptions struct {
	// Silent suppresses notifications.
	Silent bool
	// DropAuthor forwards without the forward header (a "copy" of the
	// content; the protocol distinction — NOT a local SendMessage copy,
	// messages.py:1004-1005, 964-967).
	DropAuthor bool
	// DropMediaCaptions strips media captions (requires DropAuthor
	// server-side; messages.py:1007-1008).
	DropMediaCaptions bool
}

// ForwardMessages forwards messages from one peer to another
// (reference forward_messages, messages.py:948-1085). The protocol's
// forwarding mechanism is used — never a local text copy. IDs are
// chunked at 100 per request (documented deviation: the reference sends
// one request per source chat regardless of size; chunking bounds the
// request). The result holds one entry per requested ID, in order;
// entries for messages the server could not map are nil (reference
// messageparse.py:212-231 — partial failures surface as nils, never
// fabricated).
func (c *Client) ForwardMessages(ctx context.Context, from PeerRef, to PeerRef, messageIDs []int64, opts ForwardOptions) ([]*Message, error) {
	fromRef, err := c.ResolvePeer(ctx, from)
	if err != nil {
		return nil, err
	}
	toRef, err := c.ResolvePeer(ctx, to)
	if err != nil {
		return nil, err
	}
	fromInput, err := c.inputPeer(fromRef)
	if err != nil {
		return nil, err
	}
	toInput, err := c.inputPeer(toRef)
	if err != nil {
		return nil, err
	}
	if len(messageIDs) == 0 {
		return nil, nil
	}
	ids := make([]int32, 0, len(messageIDs))
	for _, id := range messageIDs {
		if id <= 0 {
			return nil, fmt.Errorf("%w: invalid message id %d", ErrInvalidPeer, id)
		}
		ids = append(ids, int32(id))
	}

	var out []*Message
	for start := 0; start < len(ids); start += mutationChunkSize {
		end := start + mutationChunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		randomIDs := make([]int64, len(chunk))
		for i := range randomIDs {
			var rid [8]byte
			if _, err := rand.Read(rid[:]); err != nil {
				return out, fmt.Errorf("messaging: random_id: %w", err)
			}
			randomIDs[i] = int64LE(rid)
		}
		req := &gen.MessagesForwardMessagesRequest{
			FromPeer:          fromInput,
			ID:                chunk,
			RandomID:          randomIDs,
			ToPeer:            toInput,
			Silent:            opts.Silent,
			DropAuthor:        opts.DropAuthor,
			DropMediaCaptions: opts.DropMediaCaptions,
		}
		res, err := c.Invoke(ctx, req)
		if err != nil {
			return out, err
		}
		msgs, err := c.responseMessageList(toRef, randomIDs, 0, res)
		if err != nil {
			return out, err
		}
		out = append(out, msgs...)
	}
	return out, nil
}

// PinOptions carries the pin flags (messages.py:1433-1459).
type PinOptions struct {
	// Notify pins with a notification (reference default: silent).
	Notify bool
	// PmOneside pins one-sided in private chats.
	PmOneside bool
}

// PinMessage pins a message (reference pin_message → _pin,
// messages.py:1429-1468). Returns the resulting service message when the
// server produces one (messageparse.py:1516-1524: unpinning and
// already-pinned pins produce none — nil is a valid outcome).
func (c *Client) PinMessage(ctx context.Context, peer PeerRef, messageID int64, opts PinOptions) (*Message, error) {
	return c.pin(ctx, peer, messageID, false, opts)
}

// UnpinMessage unpins a message; messageID ≤ 0 unpins ALL messages
// via messages.unpinAllMessages (reference _pin, messages.py:1500-1505).
func (c *Client) UnpinMessage(ctx context.Context, peer PeerRef, messageID int64) (*Message, error) {
	if messageID <= 0 {
		ref, err := c.ResolvePeer(ctx, peer)
		if err != nil {
			return nil, err
		}
		input, err := c.inputPeer(ref)
		if err != nil {
			return nil, err
		}
		_, err = c.Invoke(ctx, &gen.MessagesUnpinAllMessagesRequest{Peer: input})
		return nil, err
	}
	return c.pin(ctx, peer, messageID, true, PinOptions{})
}

func (c *Client) pin(ctx context.Context, peer PeerRef, messageID int64, unpin bool, opts PinOptions) (*Message, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	if messageID <= 0 {
		return nil, fmt.Errorf("%w: invalid message id %d", ErrInvalidPeer, messageID)
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	req := &gen.MessagesUpdatePinnedMessageRequest{
		Peer:      input,
		ID:        int32(messageID),
		Silent:    !opts.Notify, // reference: silent = not notify (messages.py:1510)
		Unpin:     unpin,
		PmOneside: opts.PmOneside,
	}
	res, err := c.Invoke(ctx, req)
	if err != nil {
		return nil, err
	}
	// Reference messages.py:1516-1524: unpinning (or a pin that produced
	// no service update) returns nothing.
	if unpin {
		return nil, nil
	}
	if ups, ok := res.(*gen.Updates); ok && len(ups.Updates) == 0 {
		return nil, nil
	}
	return c.responseMessage(ref, nil, messageID, res)
}

// int64LE builds a little-endian random_id (forward random IDs have no
// endianness contract beyond uniqueness; the reference uses
// os.urandom(8) interpreted by the TL layer as little-endian int64).
func int64LE(b [8]byte) int64 {
	return int64(uint64(b[0]) | uint64(b[1])<<8 | uint64(b[2])<<16 | uint64(b[3])<<24 |
		uint64(b[4])<<32 | uint64(b[5])<<40 | uint64(b[6])<<48 | uint64(b[7])<<56)
}
