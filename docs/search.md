# Server-Side Search (Phase 12)

`messages.search` pagination, ported from the reference's `_MessagesIter`
search branch (`reference/SPlusthon/splusthon/client/messages.py:108-133,
181-285`).

## API

```go
SearchMessages(ctx, peer, SearchQuery) ([]*Message, error)  // materialize
IterSearchMessages(ctx, peer, SearchQuery) (*SearchIterator, error)
```

```go
type SearchQuery struct {
    Query    string  // server-side text query
    From     PeerRef // non-user peers only (messages.py:108-118)
    MinID    int64   // local window (exclusive)
    MaxID    int64   // local window (offset starts here)
    OffsetID int64   // pagination start
    Limit    int     // total cap (0 = server-unbounded)
}
```

## Request construction

`messages.search{peer, q, filter=InputMessagesFilterEmpty, from_id?,
min_date=0, max_date=0, offset_id, add_offset=0, limit, max_id=0,
min_id=0, hash=0}` — per messages.py:120-133:

- **limit=0 probe**: search returns 0 items when asked; the reference
  probes with the request as built and reads `count` (messages.py:159-166).
  SuroMesh probes implicitly via the first chunk (Limit accounting) and
  exposes the server total through `Iterator.Total()`.
- **from_id**: only sent for non-user peers — Soroush ignores it in
  private chats and error-prone on some shapes (messages.py:108-118).
  When a User is given as `From`, the filter is dropped (kept local
  per the reference's comment; SuroMesh drops it entirely — the server
  would answer unfiltered, so a User `From` narrows nothing and callers
  filter client-side if needed). Documented deviation.
- **No filter constructors exposed**: `InputMessagesFilterEmpty` is the
  only filter (media filters are future-phase scope).

## Pagination (identical discipline to Phase 11 history)

- chunks of min(remaining, 100) (messages.py:183)
- newest → oldest ordering, last-ID duplicate protection (messages.py:203-207)
- MessageEmpty skipped
- termination: `messages.Messages` (non-slice), empty page, all-filtered
  page, channel edge (top ID ≤ chunk limit), MinID crossed, NotModified
- offset update: `offset_id = last emitted id` (messages.py:260-264;
  the SearchRequest max_date clearing at messages.py:269-274 is moot —
  SuroMesh never sets max_date)
- pagination safety: bounded pages, context cancellation, Close
  interruption, no-progress termination, `ErrPaginationStalled` backstop

## Result families

`messages.Messages`, `messages.MessagesSlice`,
`messages.ChannelMessages`, `messages.MessagesNotModified` — all
handled with the same digest as history; unexpected constructors fail
explicitly.

## Entity synchronization (§19)

Every page's `users`/`chats` envelope seeds the entity cache BEFORE
conversion — a search result's sender is resolvable without further
network (`TestSearchEntitySynchronization`).

## Memory

O(page) per iterator + dedup state; 100k search results stream with no
accumulation (same audit as Phase 11 history).

## Deviations

| Reference behavior | Go behavior | Reason | Risk | Tests |
|---|---|---|---|---|
| `from_id` sent for users (ignored by server) | dropped for User peers | the server ignores it; sending it is noise | none — semantics identical | `TestSearchSinglePage` |
| `max_date` offset parameter | not exposed | messages.py:269-274 clears it after the first page anyway; date-window search is future scope | none | — |
| media/message filters | `InputMessagesFilterEmpty` only | Phase 12 scope (§2) | filters slot into SearchQuery later | — |
| global search (`searchGlobal`, no peer) | peer required | global results span chats — message identity (§24) and conversation attribution need design first | future phase | — |
