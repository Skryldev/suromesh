package client

// Album grouping (Phase 13): caller-owned, bounded aggregation of
// grouped media messages by the server-provided grouped_id.
//
// Reference: the SPlusthon tree groups albums via events/album.py — a
// delay-based aggregation (_HACK_DELAY = 0.2s, extended per arriving
// message; events/album.py:19, 40-56). SuroMesh ports that semantics into
// a caller-owned collector instead of a client-level event hook:
//
//   - Feed(msg) adds a message carrying GroupedID to its pending album
//     and extends the album's deadline (album.py:39-43).
//   - Next(ctx) blocks until ONE album is complete (deadline hit) and
//     returns its messages in arrival order.
//   - Bounded: at most MaxPending albums and MaxPerAlbum items; overflow
//     forces an immediate flush of the oldest album (never a silent
//     drop, never unbounded memory).
//   - No goroutine is owned by the collector: deadlines are evaluated
//     inside Next against the injected clock. Close/Cancellation is
//     entirely the caller's context.
//
// The MessageBox stays the sole ordering authority; the collector only
// re-buffers already-ordered messages.

import (
	"context"
	"time"
)

// Album is one completed grouped-media set.
type Album struct {
	GroupedID int64
	Peer      PeerID
	Messages  []Message
}

// AlbumCollector aggregates InAlbum messages by GroupedID.
type AlbumCollector struct {
	// MaxPending bounds concurrently open albums (default 64).
	MaxPending int
	// MaxPerAlbum bounds one album's item count (default 10).
	MaxPerAlbum int
	// Delay is the aggregation window per album (default 200 ms — the
	// reference's _HACK_DELAY, events/album.py:19).
	Delay time.Duration
	// Now is the injectable clock (tests); production time.Now.
	Now func() time.Time

	pending map[int64]*pendingAlbum
	order   []int64 // arrival order of open albums
	next    chan Album
	dropped int64
}

type pendingAlbum struct {
	peer PeerID
	msgs []Message
	due  time.Time
}

// NewAlbumCollector creates a collector with reference defaults.
func NewAlbumCollector() *AlbumCollector {
	return &AlbumCollector{
		MaxPending:  64,
		MaxPerAlbum: 10,
		Delay:       200 * time.Millisecond,
		Now:         time.Now,
		pending:     make(map[int64]*pendingAlbum),
	}
}

// Dropped reports how many albums were force-flushed by bounds.
func (ac *AlbumCollector) Dropped() int64 { return ac.dropped }

// Feed adds one message. Messages without album membership are ignored
// (the caller can still use them directly).
func (ac *AlbumCollector) Feed(msg Message) {
	if !msg.InAlbum {
		return
	}
	if ac.pending == nil {
		ac.init()
	}
	pa, ok := ac.pending[msg.GroupedID]
	if !ok {
		// Bound: flush the OLDEST album when at capacity (never drop).
		if len(ac.pending) >= ac.maxPendingOrDefault() {
			ac.flushOldest()
		}
		pa = &pendingAlbum{peer: msg.PeerID}
		ac.pending[msg.GroupedID] = pa
		ac.order = append(ac.order, msg.GroupedID)
	}
	pa.msgs = append(pa.msgs, msg)
	if len(pa.msgs) >= ac.maxPerAlbumOrDefault() {
		// Full album: flush immediately (no reason to wait).
		ac.flushLocked(msg.GroupedID)
		return
	}
	// Extend the deadline (album.py:42-43).
	pa.due = ac.Now().Add(ac.delayOrDefault())
}

// Next blocks until one album completes (deadline or capacity), returning
// it with its grouped id. Returns false when the context ends first.
func (ac *AlbumCollector) Next(ctx context.Context) (Album, bool) {
	ac.init()
	for {
		// Flush any album whose deadline has passed (earliest first).
		if gid, ok := ac.dueAlbum(); ok {
			ac.flushLocked(gid)
		}
		select {
		case a := <-ac.next:
			return a, true
		default:
		}
		if len(ac.pending) == 0 {
			select {
			case a := <-ac.next:
				return a, true
			case <-ctx.Done():
				return Album{}, false
			}
		}
		// Wait until the earliest deadline or the caller's context.
		wait := time.NewTimer(ac.earliestDue())
		select {
		case <-ctx.Done():
			wait.Stop()
			return Album{}, false
		case <-wait.C:
			wait.Stop()
		}
	}
}

// Close flushes ALL open albums through Next (drain with Next until
// false is... no: Close itself buffers the flushes into the ready
// channel; the caller keeps calling Next to drain them).
func (ac *AlbumCollector) Close() {
	ac.init()
	for _, gid := range append([]int64{}, ac.order...) {
		ac.flushLocked(gid)
	}
}

func (ac *AlbumCollector) init() {
	if ac.pending == nil {
		ac.pending = make(map[int64]*pendingAlbum)
	}
	if ac.MaxPending <= 0 {
		ac.MaxPending = 64
	}
	if ac.MaxPerAlbum <= 0 {
		ac.MaxPerAlbum = 10
	}
	if ac.Delay <= 0 {
		ac.Delay = 200 * time.Millisecond
	}
	if ac.Now == nil {
		ac.Now = time.Now
	}
	if ac.next == nil {
		ac.next = make(chan Album, 128)
	}
}

func (ac *AlbumCollector) maxPendingOrDefault() int      { ac.init(); return ac.MaxPending }
func (ac *AlbumCollector) maxPerAlbumOrDefault() int     { ac.init(); return ac.MaxPerAlbum }
func (ac *AlbumCollector) delayOrDefault() time.Duration { ac.init(); return ac.Delay }

// dueAlbum finds the earliest-deadline album that has expired.
func (ac *AlbumCollector) dueAlbum() (int64, bool) {
	now := ac.Now()
	for _, gid := range ac.order {
		if pa, ok := ac.pending[gid]; ok && !now.Before(pa.due) {
			return gid, true
		}
	}
	return 0, false
}

// earliestDue returns the wait until the earliest open deadline.
func (ac *AlbumCollector) earliestDue() time.Duration {
	now := ac.Now()
	earliest := now.Add(time.Hour)
	for _, pa := range ac.pending {
		if pa.due.Before(earliest) {
			earliest = pa.due
		}
	}
	d := earliest.Sub(now)
	if d < 0 {
		return 0
	}
	return d
}

// flushLocked completes the album, delivering it on the channel.
func (ac *AlbumCollector) flushLocked(gid int64) {
	pa, ok := ac.pending[gid]
	if !ok {
		return
	}
	delete(ac.pending, gid)
	for i, g := range ac.order {
		if g == gid {
			ac.order = append(ac.order[:i], ac.order[i+1:]...)
			break
		}
	}
	if len(pa.msgs) == 0 {
		return
	}
	a := Album{GroupedID: gid, Peer: pa.peer, Messages: pa.msgs}
	select {
	case ac.next <- a:
	default:
		// The channel only backs up if the caller abandoned Next; drop
		// the album with a counter (bounded memory over silent growth).
		ac.dropped++
	}
}

// flushOldest completes the longest-open album (capacity bound).
func (ac *AlbumCollector) flushOldest() {
	if len(ac.order) == 0 {
		return
	}
	ac.flushLocked(ac.order[0])
}
