# Dialogs — Model, Pagination & State (Phase 11)

Reference: `reference/SPlusthon/splusthon/client/dialogs.py:30-265`
(`_DialogsIter`, `iter_dialogs`, `get_dialogs`), chunk size 100
(`_MAX_CHUNK_SIZE`, dialogs.py:13).

## Model

```go
type Dialog struct {
    Peer            Peer     // resolved entity (User/Chat/Channel)
    TopMessageID    int32
    UnreadCount     int32
    UnreadMentions  int32
    ReadInboxMaxID  int32
    ReadOutboxMaxID int32
    Pinned          bool
    FolderID        *int32   // nil = main, 1 = archived
    TopMessage      *Message // server-provided top message, when present
    ChannelPTS      *int32   // feeds the MessageBox (Phase 10 integration)
    Raw             tl.Object
}
```

Only concepts the reference exposes; raw TL stays reachable via `Raw`.

## Pagination (reference-exact)

```
probe (limit=1) ─► learn total (dialogs.py:44-49)
     ▼
chunks of min(remaining, 100) ─► messages.getDialogs
     │
     ├─ entities seeded from users/chats FIRST (dialogs.py:64-64)
     ├─ top-message index keyed by (channel_id, message_id) (dialogs.py:19-27)
     ├─ per-dialog: dedup by peer ID (self.seen), skip dialogs whose
     │  entity is missing, apply offset_date / IgnorePinned /
     │  IgnoreMigrated filters
     ├─ dialog.pts → MessageBox.try_set_channel_state (dialogs.py:89-91)
     │  + conversation store seeding (server numbers are authoritative)
     ▼
termination (dialogs.py:97-102):
     - non-slice answer (messages.Dialogs) → everything returned
     - fewer dialogs than requested → end reached
     - empty buffer after filters → ended
continuation (dialogs.py:104-116):
     exclude_pinned = true
     offset_id / offset_date from the last dialog WITH a top message
     (pinned order makes the raw list unreliable)
     offset_peer  = last buffered dialog's input entity
```

**Ordering contract**: the server's native order — pinned first, then
most-recent-first — is preserved; the implementation never re-sorts.

## Pagination safety (§24)

Every iterator terminates on: context cancellation, client Close, RPC
error, empty terminal page, fewer-than-requested pages, non-slice
answers, `NotModified`, and no-progress pages. The reference terminates
cleanly when a page yields nothing new (`not self.buffer`,
dialogs.py:97-102) — SuroMesh matches; `ErrPaginationStalled` remains
exported as a defensive backstop for pathological future iterators
(never firing for reference-compatible servers — asserted by
`TestDialogsStalledPagination`, which proves termination with a
maliciously repeating server and a hard 10-call bound).

## Memory

`IterDialogs` holds one page (≤100 dialogs) + the dedup set
(O(dialogs seen) — inherent to dedup) + the continuation offset. No
full-account accumulation. `GetDialogs` materializes exactly the
requested limit.

## Deviations

| Reference behavior | Go behavior | Reason | Risk | Tests |
|---|---|---|---|---|
| `limit=None` iterates everything | `Limit: 0` does the same, bounded by the server count | parity | none | `TestDialogsPaginationTermination` |
| buffered `TotalList` with total count | `Iterator.Total()` once known | streaming keeps memory O(page) | callers wanting a count must read `Total()` after the first page | `TestDialogsSinglePageMixed` |
| offset_peer from `buffer[-1].input_entity` | same, from the resolved entity | parity | none | pagination tests |
| drafts (`iter_drafts`) not implemented | out of scope (Phase 11 scope note) | no redesign pressure | future phase | — |
