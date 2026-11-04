# Read State (Phase 14)

Read/delivery state ownership and semantics.

Reference: `updateReadHistoryInbox/Outbox`, `updateReadChannelInbox/Outbox`,
`updateReadMessagesContents`; dialog `read_inbox_max_id` /
`read_outbox_max_id` / `unread_count`; Phase 11 ConversationStore.

## Ownership

```
read/delivery updates (ordered)
        ↓
MessageBox (state authority — pts advance)
        ↓
syncConversation (ordered stream walk)
        ↓
ConversationStore (derived: UnreadCount, ReadInbox/OutboxMaxID)
        ↓
Conversation(peer) / IsMessageRead / IsOutgoingRead
```

No second read-state machine exists; the store is derived and the
MessageBox remains the sole ordering authority.

## Semantics

- **Max-id coverage**: a message is read when its ID ≤ the matching
  direction's maximum (`IsMessageRead` → inbox rule, `IsOutgoingRead` →
  outbox rule). There are no per-message flags — the reference uses
  max-id coverage (`read_inbox_max_id` semantics).
- **Maxima only advance**: `seed`/`onRead` never lower a maximum; a
  hostile negative max-id input leaves the effective maximum unchanged
  (fuzz-pinned by `FuzzReadState`).
- **Unread counts**: inbox messages increment; read updates reduce the
  count by the covered range, bounded below by zero.
- **Unknown peers** report not-read (nothing tracked).

## Channel vs private

Channel reads arrive as `updateReadChannelInbox/Outbox` (peer =
channel), private/group reads as `updateReadHistoryInbox/Outbox`
(peer = user/chat) — both map into the same peer-keyed derived state.
`MarkAsRead` (Phase 11) remains the acknowledgement writer with the
reference's per-kind RPC split.

## Tested

`TestReadStateViaUpdatesE2E` (ordered updates → max-id rule),
`TestReadStatePredicates` (unit), `FuzzReadState` (advance-only +
coherence under hostile inputs), `TestStressUpdatesRaceMutations`
(reads racing mutations, Phase 12).
