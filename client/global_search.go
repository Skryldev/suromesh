package client

// Global search (Phase 14): messages.searchGlobal with explicit
// per-message peer attribution, ported from the reference's
// _MessagesIter global-search branch:
//
//	request        messages.py:80-90 (searchGlobal: q, filter, min_date,
//	               max_date=offset_date, offset_rate, offset_peer,
//	               offset_id, limit=1 probe)
//	offset update  messages.py:279-285 (offset_peer = last message's
//	               input chat — entity-cache resolved, InputPeerEmpty
//	               fallback; offset_rate = response next_rate;
//	               offset_id = last message id)
//	digest         shared with history/search (messages.py:181-242)
//
// Identity attribution (§10/§24): every returned message carries its own
// peer_id, so attribution is per-message and MessageKey pairs are exact.
// A message ID alone is never treated as globally unique. MessageEmpty
// entries (inaccessible peers) are skipped like the reference
// (messages.py:196-198).

import (
	"context"

	"github.com/Skryldev/suromesh/tl/gen"
)

// GlobalSearchQuery is the bounded global-search model (only fields the
// reference's searchGlobal path uses).
type GlobalSearchQuery struct {
	// Query is the server-side text query.
	Query string
	// MinID/MaxID are the local window bounds (messages.py:36-53).
	MinID int64
	MaxID int64
	// OffsetID continues a previous listing (advanced).
	OffsetID int64
	// Limit bounds the total results (0 = server-unbounded, still
	// terminating on the standard conditions).
	Limit int
}

// GlobalSearchResult is one attributed search hit: the message plus its
// conversation identity (MessageKey pairing is exact — never an
// ID-only attribution).
type GlobalSearchResult struct {
	Message *Message
	Key     MessageKey
}

// GlobalSearchIterator streams attributed results page by page
// (newest → oldest). Memory: O(page) + dedup state; no goroutines owned.
type GlobalSearchIterator struct {
	c     *Client
	query GlobalSearchQuery
	req   *gen.MessagesSearchGlobalRequest

	lastID    int64
	handOut   int64
	total     int32
	buffer    []*Message
	err       error
	exhausted bool

	stalled int
	started bool
}

// IterGlobalSearch streams cross-chat search results (reference
// iter_messages without an entity → searchGlobal, messages.py:80-90).
func (c *Client) IterGlobalSearch(ctx context.Context, query GlobalSearchQuery) (*GlobalSearchIterator, error) {
	// Reference messages.py:49-53 window emulation.
	offsetID := query.OffsetID
	if query.MaxID > 0 && query.MaxID > offsetID {
		offsetID = query.MaxID
	}
	if query.MinID > 0 && offsetID > 0 && offsetID-query.MinID <= 1 {
		return &GlobalSearchIterator{c: c, query: query, exhausted: true}, nil
	}
	return &GlobalSearchIterator{
		c:     c,
		query: query,
		req: &gen.MessagesSearchGlobalRequest{
			Q:          query.Query,
			Filter:     &gen.InputMessagesFilterEmpty{},
			OffsetPeer: &gen.InputPeerEmpty{},
			OffsetID:   int32(offsetID),
		},
	}, nil
}

// Total reports the server-provided total once known.
func (it *GlobalSearchIterator) Total() int32 { return it.total }

// Err returns the terminal error (nil = clean exhaustion).
func (it *GlobalSearchIterator) Err() error { return it.err }

// Next returns the next attributed result (newest → oldest).
func (it *GlobalSearchIterator) Next(ctx context.Context) (GlobalSearchResult, bool) {
	if len(it.buffer) == 0 {
		if it.err != nil || it.exhausted {
			return GlobalSearchResult{}, false
		}
		for len(it.buffer) == 0 {
			if !it.loadPage(ctx) && len(it.buffer) == 0 {
				return GlobalSearchResult{}, false
			}
		}
	}
	m := it.buffer[0]
	it.buffer = it.buffer[1:]
	it.handOut++
	return GlobalSearchResult{Message: m, Key: m.Key()}, true
}

// loadPage fetches and digests one global-search chunk.
func (it *GlobalSearchIterator) loadPage(ctx context.Context) bool {
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
		// offset_rate for the next request (messages.py:285).
		if r.NextRate != nil {
			it.req.OffsetRate = *r.NextRate
		}
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

	// Entity synchronization BEFORE conversion/attribution (§19).
	it.c.entities.seedUpdateEntities(users, chats)

	newCount := 0
	for _, mc := range msgs {
		m, ok := mc.(*gen.Message)
		if !ok {
			continue // MessageEmpty (inaccessible peers) skipped
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

	// Offset update (messages.py:260-285): offset_id from the last
	// message; offset_peer from its chat resolved via the entity cache —
	// InputPeerEmpty fallback when unresolvable (messageparse.py:279-283).
	if it.lastID != 0 {
		it.req.OffsetID = int32(it.lastID)
		if len(it.buffer) > 0 {
			last := it.buffer[len(it.buffer)-1]
			if ip, ipErr := it.c.inputPeer(last.PeerID); ipErr == nil {
				it.req.OffsetPeer = ip
			} else {
				it.req.OffsetPeer = &gen.InputPeerEmpty{}
			}
		}
	}

	// Termination — shared history rules (messages.py:211-242). The
	// per-ID dedup is intentionally ABSENT for global search: IDs are
	// per-peer, and the reference skips the range check when no entity
	// is set (messages.py:249-250 "message IDs between chats may vary").
	if !slice || len(msgs) == 0 || (len(it.buffer) == 0 && newCount == 0) {
		it.exhausted = true
		return newCount > 0
	}
	// Bottom-reached rule (messages.py:222-229): top ID ≤ chunk limit.
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

// SearchGlobal materializes global search results (bounded by
// Query.Limit; prefer IterGlobalSearch for large result sets).
func (c *Client) SearchGlobal(ctx context.Context, query GlobalSearchQuery) ([]GlobalSearchResult, error) {
	it, err := c.IterGlobalSearch(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []GlobalSearchResult
	for {
		r, ok := it.Next(ctx)
		if !ok {
			if it.Err() != nil && !isContextErr(it.Err()) {
				return out, it.Err()
			}
			return out, nil
		}
		out = append(out, r)
	}
}
