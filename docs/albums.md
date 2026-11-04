# Albums (Phase 13)

Grouped media (photo/video albums) and their lifecycle.

Reference: `reference/SPlusthon/splusthon/events/album.py` (delay-based
aggregation) and `tl/custom/message.py:152-154, 267` (`grouped_id`).

## Wire semantics

- The server marks album members with a shared `grouped_id`
  (`Message.GroupedID`, `InAlbum` flag in the domain model).
- Album members arrive as separate `updateNewMessage`/
  `updateNewChannelMessage` updates, back to back in the stream.
- Message IDs inside an album are consecutive per peer; the ORDER the
  server sends is preserved.

## Reference behavior

The reference's `events/album.py` aggregates album events with a
**delay-based** mechanism:

- `_HACK_DELAY = 0.2` seconds (album.py:19);
- every arriving member extends the album's due time (album.py:39-43);
- when the deadline passes, the accumulated album is dispatched.

## Go model

```go
type Album struct {
    GroupedID int64
    Peer      PeerID
    Messages  []Message
}

ac := client.NewAlbumCollector()   // Delay 200ms, MaxPending 64, MaxPerAlbum 10
defer ac.Close()

sub := client.OnUpdate(func(ctx, upd) { ac.Feed(msg) })   // caller feeds
for {
    alb, ok := ac.Next(ctx)      // blocks until ONE album completes
    if !ok { break }
    // alb.Messages in arrival order
}
```

## Ownership & bounds (§12/§20)

- **Caller-owned**: the collector is not wired into the client; no
  goroutine is spawned by the client or the collector. Deadlines are
  evaluated inside `Next` against an injectable clock.
- **Bounded**: at most `MaxPending` open albums and `MaxPerAlbum` items
  per album (defaults 64/10). Capacity overflow flushes the OLDEST /
  FULL album — items are never silently dropped and memory is
  O(MaxPending × MaxPerAlbum).
- **Deterministic ordering**: arrival order is preserved within an
  album; album completion order follows deadlines (earliest first).
- **Cancellation**: `Next(ctx)` returns false when the context ends;
  `Close()` force-flushes every open album for draining via `Next`.
- **Dropped counter**: `Dropped()` reports albums abandoned by a caller
  that stopped calling Next (the bounded channel's overflow path).

## Sending albums

`SendAlbum(ctx, peer, []MediaInput, SendAlbumOptions)` — one
`messages.sendMultiMedia` request, per-item captions, per-item crypto
random ids, results ordered per input with nil holes for unmapped
entries (docs/mutations.md mapping rules). Bounded at 10 items per
request (defensive; official-client norm).
