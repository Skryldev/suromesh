package client

// Dialogs (Phase 11): domain model and bounded pagination, ported from
// the reference:
//
//	_DialogsIter          reference/SPlusthon/splusthon/client/dialogs.py:30-117
//	iter_dialogs          dialogs.py:150-237
//	get_dialogs           dialogs.py:239-265
//	_MAX_CHUNK_SIZE       dialogs.py:13 (100)
//
// Pagination semantics (reference-exact):
//   - the first request probes with limit=1 to learn the total count
//   - chunks fetch min(remaining, 100) dialogs
//   - dialogs whose peer has no entity in the response are skipped
//   - per-dialog dedup by peer ID (self.seen)
//   - offset_date filters dialogs whose top message is newer
//   - ignore_migrated skips basic chats that migrated to a channel
//   - dialog.pts seeds the MessageBox channel state (Phase 10 integration,
//     dialogs.py:89-91 try_set_channel_state)
//   - termination: empty page after filtering, fewer dialogs than
//     requested, or a non-slice answer (messages.Dialogs)
//   - continuation: exclude_pinned=true, offset_id/offset_date from the
//     last dialog WITH a top message, offset_peer from the last buffered
//     dialog's input entity (pinned order makes the raw list unreliable —
//     dialogs.py:104-116)

import (
	"context"
	"errors"
	"fmt"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// dialogsMaxChunk mirrors _MAX_CHUNK_SIZE (dialogs.py:13).
const dialogsMaxChunk = 100

// Dialog is the application-level conversation entry (the reference's
// custom.Dialog distilled to stable concepts; the raw TL constructor
// stays reachable via Raw).
type Dialog struct {
	Peer            Peer     // resolved domain entity (User/Chat/Channel)
	TopMessageID    int32    // dialog.top_message
	UnreadCount     int32    // dialog.unread_count
	UnreadMentions  int32    // dialog.unread_mentions_count
	ReadInboxMaxID  int32    // dialog.read_inbox_max_id
	ReadOutboxMaxID int32    // dialog.read_outbox_max_id
	Pinned          bool     // dialog.pinned
	FolderID        *int32   // dialog.folder_id (nil = main list)
	TopMessage      *Message // the server-provided top message, when present
	ChannelPTS      *int32   // dialog.pts (channels only; feeds MessageBox)
	Raw             tl.Object
}

// DialogQuery selects a dialog listing window (reference iter_dialogs
// arguments; only fields the reference actually uses are exposed).
type DialogQuery struct {
	// Limit bounds the total number of dialogs; 0 means "no explicit
	// limit" (the iterator still terminates on server conditions).
	Limit int
	// OffsetID / OffsetDate continue a previous listing (advanced use).
	OffsetID   int32
	OffsetDate int32
	// OffsetPeer continues after the given peer (advanced use).
	OffsetPeer Peer
	// IgnorePinned skips pinned dialogs entirely (dialogs.py:186-189).
	IgnorePinned bool
	// IgnoreMigrated skips basic chats that migrated to a supergroup
	// (dialogs.py:190-194).
	IgnoreMigrated bool
	// FolderID: nil = all folders; 0 = main list; 1 = archived
	// (dialogs.py:196-214).
	FolderID *int32
}

// DialogIterator streams dialogs page by page. Next returns false when
// the listing is exhausted, the context is cancelled, the client closes,
// or pagination stalls (same page served repeatedly). Err exposes the
// terminal cause. Memory: O(page) plus the dedup set.
type DialogIterator struct {
	c      *Client
	query  DialogQuery
	req    *gen.MessagesGetDialogsRequest
	seen   map[PeerID]bool
	buffer []Dialog
	total  int32
	err    error

	stalled        int                // consecutive pages with no new dialogs
	started        bool               // probe issued
	lastTopMessage *Message           // continuation offset source
	lastInputPeer  gen.InputPeerClass // continuation offset peer
}

// IterDialogs streams the dialog list (reference iter_dialogs).
func (c *Client) IterDialogs(ctx context.Context, query DialogQuery) *DialogIterator {
	it := &DialogIterator{
		c:     c,
		query: query,
		seen:  make(map[PeerID]bool),
		req: &gen.MessagesGetDialogsRequest{
			OffsetDate:    query.OffsetDate,
			OffsetID:      query.OffsetID,
			OffsetPeer:    &gen.InputPeerEmpty{},
			ExcludePinned: query.IgnorePinned,
			FolderID:      query.FolderID,
		},
	}
	if query.OffsetPeer != nil {
		if ip, err := c.inputPeer(query.OffsetPeer); err == nil {
			it.req.OffsetPeer = ip
		}
	}
	return it
}

// Total reports the server-provided total count once known (0 before the
// first page; -1 when the server did not send a count).
func (it *DialogIterator) Total() int32 { return it.total }

// Err returns the terminal error, if any (context cancellation, RPC
// failure, ErrPaginationStalled). nil when the listing simply ended.
func (it *DialogIterator) Err() error { return it.err }

// Next fetches the next dialog, buffering whole pages internally.
func (it *DialogIterator) Next(ctx context.Context) (Dialog, bool) {
	if it.err != nil {
		return Dialog{}, false
	}
	if len(it.buffer) == 0 {
		// A terminating page may still have buffered dialogs (the
		// reference appends to the buffer before returning True,
		// dialogs.py:95-102) — drain before stopping.
		if !it.loadPage(ctx) && len(it.buffer) == 0 {
			return Dialog{}, false
		}
	}
	d := it.buffer[0]
	it.buffer = it.buffer[1:]
	return d, true
}

// loadPage fetches one chunk (reference _load_next_chunk, dialogs.py:54-117).
func (it *DialogIterator) loadPage(ctx context.Context) bool {
	if it.query.Limit > 0 && it.total > 0 && len(it.seen) >= it.query.Limit {
		return false // limit reached
	}
	if !it.started {
		it.started = true
		// Reference probe: limit=1 learns the total (dialogs.py:34-49).
		it.req.Limit = 1
		res, err := it.c.Invoke(ctx, it.req)
		if err != nil {
			it.err = err
			return false
		}
		it.adoptPage(ctx, res, 1)
		if len(it.buffer) > 0 || it.doneByResponse(res, 1) || it.query.Limit == 0 && it.total <= 1 {
			return len(it.buffer) > 0
		}
		return it.loadNextChunk(ctx)
	}
	return it.loadNextChunk(ctx)
}

func (it *DialogIterator) loadNextChunk(ctx context.Context) bool {
	if it.err != nil {
		return false
	}
	left := it.query.Limit - len(it.seen)
	if it.query.Limit <= 0 || left > dialogsMaxChunk {
		left = dialogsMaxChunk
	}
	if left <= 0 {
		return false
	}
	it.req.Limit = int32(left)
	res, err := it.c.Invoke(ctx, it.req)
	if err != nil {
		it.err = err
		return false
	}
	it.adoptPage(ctx, res, int(it.req.Limit))
	if it.doneByResponse(res, int(it.req.Limit)) {
		return false
	}
	if len(it.buffer) == 0 {
		// Everything in the page was skipped by filters: do not spin —
		// count consecutive unproductive pages (§24 pagination safety).
		it.stalled++
		if it.stalled >= 3 {
			it.err = ErrPaginationStalled
			return false
		}
		return it.loadNextChunk(ctx)
	}
	it.stalled = 0
	return true
}

// adoptPage converts one response page: entity seeding first (§16), then
// dialog conversion (dialogs.py:56-96).
func (it *DialogIterator) adoptPage(ctx context.Context, res tl.Object, requested int) {
	var (
		dialogs  []gen.DialogClass
		messages []gen.MessageClass
		users    []gen.UserClass
		chats    []gen.ChatClass
	)
	switch r := res.(type) {
	case *gen.MessagesDialogs:
		dialogs, messages, users, chats = r.Dialogs, r.Messages, r.Users, r.Chats
	case *gen.MessagesDialogsSlice:
		it.total = r.Count
		dialogs, messages, users, chats = r.Dialogs, r.Messages, r.Users, r.Chats
	case *gen.MessagesDialogsNotModified:
		// No progress possible: the listing ends (stall-safe).
		it.stalled++
		return
	default:
		it.err = fmt.Errorf("dialogs: getDialogs answered %T", res)
		return
	}

	// Entity synchronization BEFORE conversion (§16).
	it.c.entities.seedUpdateEntities(users, chats)

	// Top-message index keyed like the reference
	// (_dialog_message_key: channels share ID space, so channel ID joins
	// the key; dialogs.py:19-27).
	msgs := make(map[PeerID]*Message, len(messages))
	for _, mc := range messages {
		if m, ok := mc.(*gen.Message); ok {
			msgs[peerIDFromWire(m.PeerID)] = it.c.messageFromTL(m)
		}
	}

	for _, dc := range dialogs {
		d, ok := dc.(*gen.Dialog)
		if !ok {
			continue
		}
		peerID := peerIDFromWire(d.Peer)
		if it.seen[peerID] {
			continue // reference self.seen dedup (dialogs.py:80-81)
		}
		ent, found := it.c.entities.get(peerID)
		if !found {
			// Dialogs without an entity are skipped (dialogs.py:82-86).
			continue
		}
		if it.query.IgnorePinned && d.Pinned {
			continue
		}
		top := msgs[peerID]
		if it.query.OffsetDate > 0 {
			if top == nil || int64(top.Date.Unix()) > int64(it.query.OffsetDate) {
				continue // dialogs.py:74-77
			}
		}
		if it.query.IgnoreMigrated {
			switch e := ent.(type) {
			case *Channel:
				if e.MigratedTo != nil {
					continue
				}
			case *Chat:
				if e.MigratedTo != nil {
					continue
				}
			}
		}
		it.seen[peerID] = true

		dlg := Dialog{
			Peer:            ent.(Peer),
			TopMessageID:    d.TopMessage,
			UnreadCount:     d.UnreadCount,
			UnreadMentions:  d.UnreadMentionsCount,
			ReadInboxMaxID:  d.ReadInboxMaxID,
			ReadOutboxMaxID: d.ReadOutboxMaxID,
			Pinned:          d.Pinned,
			FolderID:        d.FolderID,
			TopMessage:      top,
			ChannelPTS:      d.PTS,
			Raw:             d,
		}
		// Phase 10 integration: dialog.pts seeds the MessageBox channel
		// state (dialogs.py:89-91).
		if d.PTS != nil && peerID.Kind == PeerChannel {
			if box := it.c.updatesBox(); box != nil {
				box.trySetChannelState(peerID.ID, *d.PTS)
			}
		}
		// Conversation state seeding: server-reported dialog numbers.
		if c := it.c.conversations; c != nil {
			c.seed(peerID, int64(d.TopMessage), d.UnreadCount,
				d.UnreadMentionsCount, d.ReadInboxMaxID, d.ReadOutboxMaxID)
		}
		// Continuation offset: the last dialog WITH a top message
		// (dialogs.py:104-116; pinned order makes the raw list unreliable).
		if top != nil {
			it.lastTopMessage = top
			it.lastInputPeer, _ = it.c.inputPeer(ent.(Peer))
		}
		it.buffer = append(it.buffer, dlg)
	}

	// Prepare the continuation request when the page warrants another.
	if !it.doneByResponse(res, requested) && it.lastTopMessage != nil {
		it.req.ExcludePinned = true
		it.req.OffsetID = int32(it.lastTopMessage.ID)
		it.req.OffsetDate = int32(it.lastTopMessage.Date.Unix())
		if it.lastInputPeer != nil {
			it.req.OffsetPeer = it.lastInputPeer
		}
	}
}

// doneByResponse mirrors the reference's termination conditions
// (dialogs.py:97-102).
func (it *DialogIterator) doneByResponse(res tl.Object, requested int) bool {
	switch r := res.(type) {
	case *gen.MessagesDialogs:
		return true // not a slice: everything was returned
	case *gen.MessagesDialogsSlice:
		return len(r.Dialogs) < requested
	case *gen.MessagesDialogsNotModified:
		return true
	}
	return true
}

// GetDialogs materializes the listing (reference get_dialogs). With
// Limit=0 it enumerates everything the server reports — bounded by the
// server's dialog count, not by memory games; prefer IterDialogs for
// large accounts.
func (c *Client) GetDialogs(ctx context.Context, query DialogQuery) ([]Dialog, error) {
	it := c.IterDialogs(ctx, query)
	var out []Dialog
	for {
		d, ok := it.Next(ctx)
		if !ok {
			if err := it.Err(); err != nil && !errors.Is(err, context.Canceled) &&
				!errors.Is(err, context.DeadlineExceeded) {
				return out, err
			}
			return out, nil
		}
		out = append(out, d)
	}
}
