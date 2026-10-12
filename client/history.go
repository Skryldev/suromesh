package client

// Message history (Phase 11): bounded pagination over
// messages.getHistory, ported from the reference:
//
//	_MessagesIter       reference/SPlusthon/splusthon/client/messages.py:19-242
//	_message_in_range   messages.py:244-258
//	_update_offset      messages.py:260-285
//	_MAX_CHUNK_SIZE     messages.py:13 (100)
//
// Semantics (reference-exact):
//   - request: messages.getHistory{peer, limit, offset_id, offset_date,
//     min_id=0, max_id=0, add_offset, hash=0}; the caller's MaxID/MinID
//     are emulated LOCALLY (Soroush ignores small min/max windows —
//     messages.py:36-53: offset starts at max(offset_id, max_id), the
//     iterator stops past min_id)
//   - ordering contract: NEWEST → OLDEST (the server's native order;
//     descending IDs), preserved across pages
//   - MessageEmpty entries are skipped; message IDs duplicate-checked
//     against the previously emitted ID (messages.py:196-209)
//   - termination: messages.Messages (non-slice), empty page, a page
//     whose first ID ≤ requested limit (lowest ID is 1 — messages.py:215-229),
//     or an all-empty page (messages.py:231-242)
//   - offset update: offset_id = last emitted id, offset_date = its date
//     (messages.py:260-285)
//   - NotModified: no progress possible → the iterator terminates
//   - stall guard: 3 consecutive pages with no new message →
//     ErrPaginationStalled (§24 pagination safety)

import (
	"context"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// historyMaxChunk mirrors _MAX_CHUNK_SIZE (messages.py:13).
const historyMaxChunk = 100

// MessageQuery selects a history window. Ordering is newest → oldest
// (reference behavior; see docs/history.md).
type MessageQuery struct {
	// Limit bounds the total messages returned (0 = server default).
	Limit int32
	// OffsetID starts strictly before this ID (0 = newest).
	OffsetID int64
	// OffsetDate starts before this unix timestamp (advanced; the
	// reference passes it through to the RPC).
	OffsetDate int32
	// MaxID / MinID emulate the reference's local window: iteration stops
	// once IDs pass the bound (messages.py:36-53).
	MaxID int64
	MinID int64
	// AddOffset is the raw protocol add_offset (advanced; negative values
	// page from before the offset).
	AddOffset int32
}

// MessageIterator streams message history page by page. Memory: O(page)
// plus the offset state. Not safe for concurrent use; safe to drop
// without cleanup (no goroutines owned).
type MessageIterator struct {
	c     *Client
	peer  Peer
	query MessageQuery
	req   *gen.MessagesGetHistoryRequest

	lastID    int64 // previously emitted id (dedup + range)
	handOut   int64 // messages handed to the caller (Limit accounting)
	total     int32
	buffer    []*Message
	err       error
	exhausted bool

	stalled int // consecutive pages with no new message
	started bool
}

// IterMessages streams the history for a peer (reference iter_messages
// limited to the getHistory family; search/replies belong to later phases
// and are rejected explicitly rather than approximated).
func (c *Client) IterMessages(ctx context.Context, peer PeerRef, query MessageQuery) (*MessageIterator, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	// Reference local-window emulation (messages.py:49-53): the offset
	// starts at max(offset_id, max_id); a window narrower than one ID
	// terminates immediately.
	offsetID := query.OffsetID
	if query.MaxID > 0 && query.MaxID > offsetID {
		offsetID = query.MaxID
	}
	if query.MinID > 0 && offsetID > 0 && offsetID-query.MinID <= 1 {
		return &MessageIterator{c: c, peer: ref, query: query, exhausted: true}, nil
	}
	return &MessageIterator{
		c:     c,
		peer:  ref,
		query: query,
		req: &gen.MessagesGetHistoryRequest{
			Peer:       input,
			OffsetID:   int32(offsetID),
			OffsetDate: query.OffsetDate,
			AddOffset:  query.AddOffset,
		},
		lastID: 0, // forward iteration: last_id starts at +inf conceptually
	}, nil
}

// Total reports the server-provided total count once known.
func (it *MessageIterator) Total() int32 { return it.total }

// Err returns the terminal error (nil = clean exhaustion).
func (it *MessageIterator) Err() error { return it.err }

// Next returns the next message (newest → oldest). A terminating page
// may still hold buffered messages (the reference appends to its buffer
// before returning True, messages.py:209) — the buffer drains BEFORE the
// terminal state is honored.
func (it *MessageIterator) Next(ctx context.Context) (*Message, bool) {
	if len(it.buffer) == 0 {
		if it.err != nil || it.exhausted {
			return nil, false
		}
		for len(it.buffer) == 0 {
			if !it.loadPage(ctx) && len(it.buffer) == 0 {
				return nil, false
			}
		}
	}
	m := it.buffer[0]
	it.buffer = it.buffer[1:]
	it.handOut++
	return m, true
}

// loadPage fetches and digests one chunk (reference _load_next_chunk).
func (it *MessageIterator) loadPage(ctx context.Context) bool {
	if it.exhausted {
		return false
	}
	limit := historyMaxChunk
	if it.query.Limit > 0 {
		// The window emulation also caps the wire limit at the remaining
		// range so a huge MinID..MaxID span cannot over-fetch.
		remaining := int64(it.query.Limit) - it.delivered()
		if remaining < int64(limit) {
			limit = int(remaining)
		}
	}
	if limit <= 0 {
		it.exhausted = true
		return false
	}
	it.req.Limit = int32(limit)

	res, err := it.c.Invoke(ctx, it.req)
	if err != nil {
		it.err = err
		return false
	}
	it.started = true

	var (
		msgs  []gen.MessageClass
		users []gen.UserClass
		chats []gen.ChatClass
		slice = true
	)
	switch r := res.(type) {
	case *gen.MessagesMessages:
		msgs, users, chats = r.Messages, r.Users, r.Chats
		slice = false // not a slice: nothing more to fetch (messages.py:212-213)
	case *gen.MessagesMessagesSlice:
		msgs, users, chats = r.Messages, r.Users, r.Chats
		it.total = r.Count
	case *gen.MessagesChannelMessages:
		msgs, users, chats = r.Messages, r.Users, r.Chats
		it.total = r.Count
	case *gen.MessagesMessagesNotModified:
		// No progress possible (§24): terminate without spinning.
		it.exhausted = true
		return false
	default:
		it.err = errUnexpectedHistory{res}
		return false
	}

	// Entity synchronization BEFORE conversion (§16).
	it.c.entities.seedUpdateEntities(users, chats)

	newCount := 0
	for _, mc := range msgs {
		if _, empty := mc.(*gen.MessageEmpty); empty {
			continue // MessageEmpty skipped (messages.py:196-198); MessageService KEPT
		}
		m, ok := mc.(*gen.Message)
		if !ok {
			// MessageService and other non-plain payloads convert via the
			// class-level path (service representation, Phase 16).
			if _, isSvc := mc.(*gen.MessageService); isSvc {
				it.lastID = int64(classMessageID(mc))
				it.buffer = append(it.buffer, it.c.messageFromTL(mc))
				newCount++
			}
			continue
		}
		// Range + duplicate check (messages.py:244-258, 203-207): IDs
		// arrive descending; anything at or below the last emitted ID is
		// a replay, anything at/below MinID leaves the window.
		if it.lastID != 0 && int64(m.ID) >= it.lastID {
			continue
		}
		if it.query.MinID > 0 && int64(m.ID) <= it.query.MinID {
			it.exhausted = true
			return newCount > 0
		}
		it.lastID = int64(m.ID)
		dm := it.c.messageFromTL(m)
		it.buffer = append(it.buffer, dm)
		newCount++
	}

	// Offset update (messages.py:260-277): from the last NON-EMPTY
	// message — the buffer tail after filtering.
	if it.lastID != 0 {
		it.req.OffsetID = int32(it.lastID)
	}

	// Termination conditions (messages.py:211-242).
	if !slice || len(msgs) == 0 || len(it.buffer) == 0 && it.started && newCount == 0 {
		it.exhausted = true
		return newCount > 0
	}
	// Channel edge: the highest ID ≤ the requested limit means the
	// history bottom is reached (messages.py:222-229). The highest ID is
	// the FIRST non-empty message (descending order).
	if len(it.buffer) > 0 && it.buffer[0].ID <= int64(limit) {
		it.exhausted = true
		return newCount > 0
	}
	if newCount == 0 {
		// All-empty page or pure duplicates: stall guard.
		it.stalled++
		if it.stalled >= 3 {
			it.err = ErrPaginationStalled
			return false
		}
	}
	if newCount > 0 {
		it.stalled = 0
	}
	return true
}

// delivered reports how many messages have been handed out so far
// (local accounting for the Limit cap).
func (it *MessageIterator) delivered() int64 { return it.handOut }

type errUnexpectedHistory struct{ got tl.Object }

func (e errUnexpectedHistory) Error() string {
	return "history: getHistory answered unexpected constructor"
}
