# Conversations — Derived State, Update Integration & Eviction (Phase 11)

The conversation layer is **derived application state**, deliberately
separate from the protocol state machines:

```
MessageBox (Phase 10)         = protocol synchronization authority
                                 (pts / qts / seq — never duplicated)
EntityCache (Phase 6)         = authoritative access hashes
ConversationStore (Phase 11)  = derived per-peer view:
                                 top message, unread counts, read maxima,
                                 last activity
```

No second pts/qts/seq machine exists (§17).

## Ownership (§19)

| Structure | Owner | Synchronization |
|---|---|---|
| MessageBox | the update sync loop (single goroutine) | internal mutex for Metrics readers |
| EntityCache | shared; all writers go through seed paths | RWMutex |
| ConversationStore | the sync loop writes (inside the ordered stream walk); any goroutine reads | RWMutex + value-copy reads |
| Dialog/History iterators | caller-owned local state | none needed; not shared |
| Raw queue / event queue | engine channels | bounded channel semantics |

## Update integration (§18)

The synchronizer runs INSIDE the sync loop — the single authoritative
ordering point — while walking the MessageBox-accepted stream:

```
MessageBox accepts update (ordered)
        ↓
syncConversation(update)          ← conversation state mutates here
        ↓
emit to subscribers
```

Classification:

| Update | Store effect |
|---|---|
| `updateNewMessage` / `updateNewChannelMessage` | top message advances; inbox → unread++, outbox → read-outbox max (never unread) |
| own mutations (Phase 12) | the caller's own sends/edits advance top message + outbox max through the same ordered stream (syncConversation runs over OwnUpdate batches too) — no subscriber events |
| `updateEditMessage` | last activity |
| `updateDeleteChannelMessages` | last activity (attributable peer); plain `updateDeleteMessages` carries no peer → skipped (next dialog fetch re-syncs) |
| `updateReadHistoryInbox` / `updateReadChannelInbox` | read-inbox max advances; unread reduced by the covered range (bounded ≥ 0) |
| `updateReadHistoryOutbox` / `updateReadChannelOutbox` | read-outbox max |

Outgoing messages never create duplicate events: the send's own update
flows through the MessageBox once (Phase 10 `OwnUpdate`), and the
conversation store sees exactly the ordered stream — including the
caller's own mutations (Phase 12), which advance top/outbox state without
producing subscriber events.

## Read state (§21)

`MarkAsRead(ctx, peer, maxID)` uses the reference's per-kind split:
`channels.readHistory#cc104937` for channels (hash required — never
fabricated), `messages.readHistory` for everything else. `maxID ≤ 0`
means the currently known top message (reference
`send_read_acknowledge` permissiveness). **No optimistic local state**:
the store updates only when the server's read event arrives through the
ordered stream (`TestMarkAsReadE2E`).

## Seeding & eviction (§20)

`IterDialogs` seeds each dialog from server-reported numbers (top,
unread, mentions, read maxima) — the same numbers official apps display.
The store is bounded by `Config.ConversationCacheSize` (default 1024):
at capacity the oldest-by-insertion entry is dropped. Eviction is safe
by construction — the store holds derived state only, so MessageBox,
entity cache, auth, and transport are unaffected; evicted conversations
reappear on the next dialog fetch or qualifying update
(`TestConversationCacheEviction`).

## Concurrency (§26)

Reads are value copies under RLock; writes are single-threaded (sync
loop). 8 reader goroutines racing 2000 ordered updates produce coherent
final numbers with zero races
(`TestConversationConcurrentReadersAndUpdates`); the fuzz target
`FuzzConversationUpdate` pins impossible-state freedom (no negative
unread, bounded capacity).
