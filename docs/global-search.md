# Global Search (Phase 14)

Cross-chat server-side search: `messages.searchGlobal`.

Reference: `_MessagesIter` global-search branch — request construction
messages.py:80-90; offset update messages.py:279-285; `MessageEmpty`
skipping messages.py:196-198.

## Identity attribution (the Phase 12 prerequisite)

Every returned message carries its own `peer_id`, so attribution is
per-message: `GlobalSearchResult{Message, Key MessageKey}` — the peer+id
pairing is exact. A message ID alone is NEVER treated as globally unique;
duplicate IDs across peers produce distinct keys
(`TestGlobalSearchE2E` pins user-vs-channel duplicates).

## Request/continuation

```
messages.searchGlobal{q, filter=InputMessagesFilterEmpty, min_date=0,
                      max_date=0, offset_rate, offset_peer, offset_id, limit}
```

Continuation (messages.py:279-285):
- `offset_id` = last emitted message id;
- `offset_rate` = `messagesMessagesSlice.next_rate`;
- `offset_peer` = the last message's chat, resolved through the entity
  cache — `InputPeerEmpty` fallback when unresolvable
  (messageparse.py:279-283).

## Termination

Shared pagination-safety discipline (Phase 11): non-slice answer, empty
page, all-skipped page, NotModified, MinID bound, bottom-reached rule
(top ID ≤ chunk limit), no-progress stall guard, context cancellation,
Close. Per-ID dedup is intentionally ABSENT — IDs are per-peer and the
reference skips the range check when no entity is set
(messages.py:249-250).

## API

```go
type GlobalSearchQuery struct {
    Query    string
    MinID    int64
    MaxID    int64
    OffsetID int64
    Limit    int
}
IterGlobalSearch(ctx, query) (*GlobalSearchIterator, error)
SearchGlobal(ctx, query) ([]GlobalSearchResult, error)
```

Results carry the full domain `Message` (media/entities/reply metadata
included — the canonical conversion path) plus `Key`.
