# Iterator Lifetime Model (Phase 15)

All iterators: lifetime contract, resource ownership, termination
guarantees.

| Iterator | File | Owns goroutines? | Owns transport refs? | Buffers | Termination |
|---|---|---|---|---|---|
| DialogIterator | client/dialogs.go | NO | NO (client ref only, RPC-per-page) | O(page) + seen set | ctx cancel / Close (next Invoke fails) / end conditions / stall |
| MessageIterator (history) | client/history.go | NO | NO | O(page) + lastID/offset | same set |
| SearchIterator | client/message_search.go | NO | NO | O(page) + state | same set |
| GlobalSearchIterator | client/global_search.go | NO | NO | O(page) + state | same set |
| AlbumCollector | client/albums.go | NO (deadline eval inside Next) | NO | O(MaxPending × MaxPerAlbum) | ctx cancel / Close (force flush) |

## Contract

1. **No goroutines**: an iterator is inert between Next calls. Forgetting
   to drain one leaks nothing — the only retained resource is local
   state (GC'd with the iterator).
2. **No transport references**: iterators hold `*Client` and issue one
   Invoke per page through the standard lifecycle-merged path; Close
   makes the next Invoke fail (iterator terminates promptly).
3. **Bounded buffers**: one page plus dedup/offset state. Response trees
   are converted and released per page; nothing is retained across
   pages except the documented state fields.
4. **Termination set** (every iterator): context cancellation, client
   Close, RPC error, empty/skipped page, non-slice terminal answer,
   NotModified, bottom-reached rule, no-progress stall guard.
5. **Caller-owned state**: safe to drop at any point; safe to share
   across goroutines only by external synchronization (documented per
   iterator — none is safe for concurrent Next).
