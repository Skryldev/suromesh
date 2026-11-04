# Message Mutations (Phase 12)

Reply, edit, delete, forward, pin — the mutation layer. Every behavioral
rule is traced to the reference; the update-consistency architecture is
the load-bearing design.

Reference: `reference/SPlusthon/splusthon/client/messages.py`
(send_message 631-946, forward_messages 948-1085, edit_message
1087-1263, delete_messages 1265-1336, pin/unpin/_pin 1429-1524) and
`messageparse.py:113-231` (`_get_response_message`).

## Update consistency — the load-bearing invariant

```
RPC result ──► rpc dispatcher ──► OwnUpdate (update-family results)
                       │                    │
                       ▼                    ▼
                caller's return value   MessageBox (state only, no emit)
                                             │
                                             ▼
                                  syncConversation (own updates included)
                                             │
                                             ▼
                                  subscriber fan-out (server updates only)
```

- The caller consumes the RPC result directly (converted via the
  `messageparse.py` port) — no dispatch round-trip.
- The dispatcher wraps update-family results as `OwnUpdate`: the
  MessageBox advances pts/seq but never emits them to subscribers
  (Phase 10 semantics). A mutation therefore CANNOT produce a duplicate
  event for the caller.
- Own updates now also feed `syncConversation` (Phase 12): your own send
  becomes the conversation's top message and advances the outbox maximum
  — matching official-app behavior — without any subscriber event.
- Mutations never call subscribers directly and never mutate the store
  outside the sync loop's ordering point.

Retry safety (§27): the Phase 6/7 engine retry policy is the only retry
authority (server-internal 500-class and tolerated flood waits — the
reference's own `_call` classification). No mutation-specific retries
were added; a connection-loss mid-mutation fails the call (the request's
execution state is unknown — the reference does not replay it either).

## Reply & Send

`SendMessageOpts(ctx, peer, text, SendMessageOptions{ReplyTo, Silent,
NoWebpage, ClearDraft})` / `Reply(ctx, peer, replyTo, text)`.

- Reply semantics: `reply_to` → `InputReplyToMessage{ReplyToMsgID}`
  (messages.py:897/920). Thread/top-id distinctions exist in the schema
  (`InputReplyToMessage.TopMsgID`) but Soroush's send path sets only the
  target id — the Go API exposes exactly that.
- random_id: crypto-random int64 per request (the reference fills it at
  the TL layer; uniqueness is the only requirement).
- Result: `UpdateShortSentMessage` → the message is synthesized from the
  short form + the resolved peer (messages.py:930-944; layer 182's short
  form carries no reply header — nothing is fabricated); `Updates`/
  `UpdatesCombined`/`UpdateShort` → the random-id mapping below.
- Empty text fails locally (`messages.py:910-913` — "cannot be empty
  unless a file is provided"; file sends are Phase 8's SendMedia).

## The response mapping (`_get_response_message` port)

`client/message_response.go` — the heart of mutation-result semantics:

| Result update | Mapping |
|---|---|
| `updateMessageID` | random_id → message id |
| `updateNewMessage` / `updateNewChannelMessage` | id-keyed message |
| `updateEditMessage` (non-channel peer) | edit result, matched by request id (messageparse.py:155-164) |
| `updateEditChannelMessage` (peer match) | edit result, matched by request id (messageparse.py:166-171) |
| `updateShortSentMessage` | short-form synthesis (messages.py:930-944) |

Sends/forwards map each requested random_id → id → message; unmapped
entries surface as **nil** (messageparse.py:212-231 — partial failures
are visible, never fabricated). Edits/pins match by request id.

## Edit

`EditMessage(ctx, peer, messageID, text)`. The result is the
authoritative server message — never a local mutation. `EditDate` is
nil unless the server set it. Validation: positive IDs only.

## Delete

`DeleteMessages(ctx, peer, ids, DeleteMessagesOptions{Revoke})`.

- Channels (resolved entity type) route to `channels.deleteMessages`
  (revoke is unconditional); everything else to
  `messages.deleteMessages{revoke}` (messages.py:1331-1336).
- **Chunking**: 100 IDs per request (the reference's `utils.chunks`
  default). A 100k-ID call becomes 1000 bounded requests; memory is
  O(batch input + one chunk).
- Returns one `AffectedMessages` per chunk, in order; a failing chunk
  aborts with its error — partial results are returned alongside the
  error, never silently swallowed.
- Reference caveat preserved verbatim (messages.py:1276-1281): the
  server does NOT validate that IDs belong to the passed chat.

## Forward

`ForwardMessages(ctx, from, to, ids, ForwardOptions{Silent, DropAuthor,
DropMediaCaptions})`.

- The protocol's forwarding mechanism is used — never a local text copy.
  `DropAuthor` IS the protocol's "copy" form (messages.py:964-967,
  1004-1005); the forwarded/copied distinction stays on the wire flags.
- Per-ID crypto-random ids; chunked at 100 (documented deviation: the
  reference sends one request per source group regardless of size —
  chunking bounds the request; per-message semantics are unchanged).
- Results preserve request order with nil holes for unmapped IDs.

## Pin / Unpin

`PinMessage(ctx, peer, id, PinOptions{Notify, PmOneside})` /
`UnpinMessage(ctx, peer, id)`.

- `messages.updatePinnedMessage{peer, id, silent=!notify, unpin,
  pm_oneside}` (messages.py:1507-1513).
- Unpin with `id ≤ 0` → `messages.unpinAllMessages` (messages.py:1503-1505).
- Return semantics (messages.py:1516-1524): unpinning and no-op pins
  return nil — a nil result is a valid, successful outcome; the service
  message arrives (for others) through the update stream.
- No second pin state: the pin update flows through the MessageBox like
  any other; conversation state derives from the stream.

## Cancellation & lifecycle

Every mutation merges the caller context with the lifecycle context
(`mergeContext`): caller cancellation AND `Close` interrupt the RPC and
any retry backoff. Verified: deadline-beating mutations, Close-during-
mutation settling with an error.

## Test inventory

| Area | Tests |
|---|---|
| Reply construction + result mapping | `TestReplyRequestConstructionAndResult` |
| Own-update duplicate prevention | `TestSendMessageOwnUpdateNoDuplicateEvents`, `TestStress32ConcurrentMutations` |
| Edit authoritative result + validation | `TestEditMessage*` |
| Delete routing/chunking/validation | `TestDeleteMessages*` |
| Forward mapping + partial failure | `TestForwardMessages*` |
| Pin lifecycle | `TestPinUnpinLifecycle` |
| Error surfacing | `TestMutationServerErrorsSurface` |
| Concurrency | `TestStress32ConcurrentMutations`, `TestStressUpdatesRaceMutations` |
| Lifecycle | `TestCloseDuringMutation`, `TestMutationCancellation` |
| Bounds | `TestStress100kMutationIDs` |
| Fuzz | `FuzzMessageMutationResponse`, `FuzzMessageIDs`, `FuzzForwardResponse`, `FuzzMutationUpdates` |

## Deviations

| Reference behavior | Go behavior | Reason | Risk | Tests |
|---|---|---|---|---|
| Forwards unchunked per source group | chunked at 100 | §25 bounded requests | none per-message | `TestForwardMessagesMapping` |
| `_get_response_message` logs missing mappings | nil entries, no log (no-logging policy) | Phase 1 security rule | callers must handle nils | `TestForwardMessagesPartialFailure` |
| `comment_to` / `send_as` / `schedule` send params | not exposed | scope discipline (no linked-group machinery yet) | future phases extend SendMessageOptions | — |
| `editInlineBotMessage` DC-routed edits | not exposed (bots out of scope) | scope | none | — |
