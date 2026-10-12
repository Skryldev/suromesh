package client

// Server-side search (Phase 12): messages.search pagination ported from
// the reference's _MessagesIter search branch:
//
//	request construction  messages.py:108-133 (messages.SearchRequest,
//	                      limit=0 probe — search returns 0 items if asked)
//	from_id filtering     messages.py:108-118 (users ignore from_id —
//	                      Soroush filters it server-side only for non-users)
//	chunk loading         messages.py:181-242 (min(left, 100))
//	offset update         messages.py:260-285 (SearchRequest clears
//	                      max_date; offset_id = last message)
//	result families       Messages / MessagesSlice / ChannelMessages /
//	                      MessagesNotModified (same digest as history)
//
// Pagination safety (§17): identical discipline to Phase 11 history —
// bounded pages, last-ID dedup, context cancellation, Close interruption,
// no-progress termination, ErrPaginationStalled backstop.

import (
	"context"

	"github.com/Skryldev/suromesh/tl/gen"
)

// SearchQuery is the bounded search model (only fields the reference's
// search path actually uses).
type SearchQuery struct {
	// Query is the server-side text query ("" = filter-only search).
	Query string
	// From restricts results to one sender (non-user peers only —
	// reference messages.py:108-118: private chats ignore from_id).
	From PeerRef
	// MinID / MaxID are the local window bounds (messages.py:36-53).
	MinID int64
	MaxID int64
	// OffsetID starts the listing strictly before this ID.
	OffsetID int64
	// Limit bounds the total results (0 = server-unbounded, still
	// terminating on the standard conditions).
	Limit int
}

// SearchIterator streams search results page by page (newest → oldest).
// Memory: O(page) + dedup state; no goroutines owned.
type SearchIterator struct {
	c     *Client
	peer  Peer
	query SearchQuery
	req   *gen.MessagesSearchRequest

	lastID    int64
	handOut   int64
	total     int32
	buffer    []*Message
	err       error
	exhausted bool

	stalled int
	started bool
}

// IterSearchMessages streams server-side search results
// (reference iter_messages(search=...), messages.py:351+ / 108-133).
func (c *Client) IterSearchMessages(ctx context.Context, peer PeerRef, query SearchQuery) (*SearchIterator, error) {
	ref, err := c.ResolvePeer(ctx, peer)
	if err != nil {
		return nil, err
	}
	input, err := c.inputPeer(ref)
	if err != nil {
		return nil, err
	}
	// Reference messages.py:49-53: offset starts at max(offset_id, max_id).
	offsetID := query.OffsetID
	if query.MaxID > 0 && query.MaxID > offsetID {
		offsetID = query.MaxID
	}
	if query.MinID > 0 && offsetID > 0 && offsetID-query.MinID <= 1 {
		return &SearchIterator{c: c, peer: ref, query: query, exhausted: true}, nil
	}
	req := &gen.MessagesSearchRequest{
		Peer:     input,
		Q:        query.Query,
		Filter:   &gen.InputMessagesFilterEmpty{},
		OffsetID: int32(offsetID),
	}
	// from_id: only for non-user peers (messages.py:108-118 — Soroush
	// ignores from_id in private chats and errors on some shapes).
	if query.From != nil {
		fromRef, err := c.ResolvePeer(ctx, query.From)
		if err != nil {
			return nil, err
		}
		if _, isUser := fromRef.(*User); !isUser {
			fromInput, err := c.inputPeer(fromRef)
			if err != nil {
				return nil, err
			}
			req.FromID = fromInput
		}
	}
	return &SearchIterator{c: c, peer: ref, query: query, req: req}, nil
}

// Total reports the server-provided total once known.
func (it *SearchIterator) Total() int32 { return it.total }

// Err returns the terminal error (nil = clean exhaustion).
func (it *SearchIterator) Err() error { return it.err }

// Next returns the next matching message (newest → oldest). Buffered
// messages drain before the terminal state is honored (Phase 11 rule).
func (it *SearchIterator) Next(ctx context.Context) (*Message, bool) {
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

// loadPage fetches and digests one search chunk (messages.py:181-242).
func (it *SearchIterator) loadPage(ctx context.Context) bool {
	if it.exhausted {
		return false
	}
	limit := historyMaxChunk
	if it.query.Limit > 0 {
		remaining := int64(it.query.Limit) - it.handOut
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
		slice = false
	case *gen.MessagesMessagesSlice:
		msgs, users, chats = r.Messages, r.Users, r.Chats
		it.total = r.Count
	case *gen.MessagesChannelMessages:
		msgs, users, chats = r.Messages, r.Users, r.Chats
		it.total = r.Count
	case *gen.MessagesMessagesNotModified:
		it.exhausted = true
		return false
	default:
		it.err = errUnexpectedHistory{res}
		return false
	}

	// Entity synchronization BEFORE conversion (§19 — same pipeline as
	// history/dialogs).
	it.c.entities.seedUpdateEntities(users, chats)

	newCount := 0
	for _, mc := range msgs {
		m, ok := mc.(*gen.Message)
		if !ok {
			continue
		}
		if it.lastID != 0 && int64(m.ID) >= it.lastID {
			continue
		}
		if it.query.MinID > 0 && int64(m.ID) <= it.query.MinID {
			it.exhausted = true
			return newCount > 0
		}
		it.lastID = int64(m.ID)
		it.buffer = append(it.buffer, it.c.messageFromTL(m))
		newCount++
	}

	// Offset update (messages.py:260-277): SearchRequest clears its max
	// date (we never set one) and advances offset_id.
	if it.lastID != 0 {
		it.req.OffsetID = int32(it.lastID)
	}

	// Termination (messages.py:211-242) — shared rules with history.
	if !slice || len(msgs) == 0 || (len(it.buffer) == 0 && newCount == 0) {
		it.exhausted = true
		return newCount > 0
	}
	if len(it.buffer) > 0 && it.buffer[0].ID <= int64(limit) {
		it.exhausted = true
		return newCount > 0
	}
	if newCount == 0 {
		it.stalled++
		if it.stalled >= 3 {
			it.err = ErrPaginationStalled
			return false
		}
	} else {
		it.stalled = 0
	}
	return true
}

// SearchMessages materializes search results (bounded by Query.Limit;
// prefer IterSearchMessages for large result sets).
func (c *Client) SearchMessages(ctx context.Context, peer PeerRef, query SearchQuery) ([]*Message, error) {
	it, err := c.IterSearchMessages(ctx, peer, query)
	if err != nil {
		return nil, err
	}
	var out []*Message
	for {
		m, ok := it.Next(ctx)
		if !ok {
			if it.Err() != nil && !isContextErr(it.Err()) {
				return out, it.Err()
			}
			return out, nil
		}
		out = append(out, m)
	}
}

func isContextErr(err error) bool {
	return err == context.Canceled || err == context.DeadlineExceeded
}
