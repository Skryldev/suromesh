# Message History — RPCs, Pagination & Ordering (Phase 11)

Reference: `reference/SPlusthon/splusthon/client/messages.py:19-285`
(`_MessagesIter`, `_message_in_range`, `_update_offset`), chunk size 100.

## RPCs used

| RPC | Constructor | When |
|---|---|---|
| `messages.getHistory#4423e6c5` | peer, offset_id, offset_date, add_offset, limit, max_id=0, min_id=0, hash=0 | the only history RPC in this phase (search/replies are later-phase features) |
| `channels.readHistory#cc104937` / `messages.readHistory#…` | read acknowledgements (docs/conversations.md) |

## Query model

```go
type MessageQuery struct {
    Limit      int32
    OffsetID   int64
    OffsetDate int32
    MaxID      int64
    MinID      int64
    AddOffset  int32 // raw protocol add_offset (advanced)
}
```

Only fields the reference justifies. MaxID/MinID are **emulated locally**
(messages.py:36-53: Soroush ignores small min/max windows) — the offset
starts at `max(offset_id, max_id)` and iteration stops past `min_id`
(exclusive at both ends).

## Ordering contract

**Newest → oldest** (the server's native, descending-ID order), preserved
across pages. The implementation never silently reverses; reverse
iteration (the reference's `reverse` mode via negative add_offset) is not
exposed in this phase — callers can express windows with OffsetID/AddOffset.
Message IDs are duplicate-checked against the last emitted ID
(messages.py:203-207 — protection for servers that replay IDs on bad
connections).

## Pagination (reference-exact)

```
page = getHistory(limit = min(remaining, 100))
     ├─ entity seeding BEFORE conversion (messages.py:191-192)
     ├─ MessageEmpty & non-message entries skipped (messages.py:196-198)
     ├─ range + dedup checks (messages.py:244-258)
     └─ offset update: offset_id = last emitted id (messages.py:260-264)
termination (messages.py:211-242):
     - messages.Messages (non-slice) → done
     - empty page → done
     - all-filtered page → done ("give up" — messages.py:231-242)
     - channel edge: highest ID ≤ chunk limit → done (IDs start at 1,
       so one chunk covered the rest — messages.py:222-229)
     - MinID bound crossed → done
     - NotModified → done (no progress possible)
```

`MessagesMessages`, `MessagesMessagesSlice`, `MessagesChannelMessages`
and `MessagesMessagesNotModified` are all handled; an unexpected
constructor fails explicitly (`errUnexpectedHistory`) — no silent drops.

## Message conversion (§15)

`messageFromTL` populates only server-provided fields: ID, Peer, From,
Date, Text, Out, EditDate (nil = never edited), ReplyToMessageID (when
the reply header exists), Entities (server-provided formatting entities),
Media (via the Phase 8 media extraction path), Raw (the TL object). No
fabricated defaults beyond zero values.

## Deviations

| Reference behavior | Go behavior | Reason | Risk | Tests |
|---|---|---|---|---|
| `reverse` iteration (add_offset −100 walk) | not exposed this phase | scope discipline; window expressible via OffsetID/AddOffset | callers wanting oldest→newest must page backwards themselves | ordering tests |
| `wait_time` flood pacing (limit > 3000) | not needed: the engine's FloodWait policy (Phase 6) sleeps below the configured threshold | single retry authority | none | engine retry tests |
| from_user search filtering | not implemented (search is out of scope) | scope | none | — |
| MessageEmpty pages → give up | same (clean termination) | parity | none | `TestHistoryStalledPagination` |
