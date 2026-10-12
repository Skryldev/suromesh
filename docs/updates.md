# Update Engine, State Synchronization & Event Consistency (Phase 10)

The authoritative document for SuroMesh's update subsystem: architecture,
state machine, pts/seq/qts semantics, gap handling, difference
synchronization, ordering, entity synchronization, reconnect behavior,
and the reference trace to `reference/SPlusthon` (read-only oracle).

**Real Soroush Plus validation: NOT PERFORMED** (no real account /
credentials; deferred by standing policy). All evidence comes from the
in-process fake transport exercising the real protocol stack.

## Architecture

```
                         ┌─────────────────────┐
                         │     RPC Engine      │  socket updates +
                         └──────────┬──────────┘  own-update results
                                    │
                                    ▼
                         ┌─────────────────────┐
                         │  Raw Queue (4096,   │  drop-oldest overflow
                         │      bounded)       │
                         └──────────┬──────────┘
                                    ▼
                         ┌─────────────────────┐
                         │    Sync Loop        │  ONE goroutine owns the
                         │  (normalize →       │  MessageBox: strictly
                         │  state → entities)  │  ordered state mutations,
                         └──────────┬──────────┘  at most one difference
                              │          │        at a time
             GapError ────────┘          ▼
             (recovery next     ┌─────────────────────┐
             loop pass)         │ Ordered Event Queue │  bounded; drop-newest
                                │      (Phase 7)      │  + slow-consumer hook
                                └──────────┬──────────┘
                                           ▼
                                ┌─────────────────────┐
                                │  N delivery workers │  subscriber fan-out,
                                └──────────┬──────────┘  panic-isolated
                                           ▼
                                     User subscribers
```

Files: `client/messagebox.go` (state machine), `client/ptsinfo.go`
(ordering-key extraction), `client/updates.go` (sync loop + pipeline),
`client/updatepersist.go` (session wiring), `rpc/dispatch.go`
(own-update flagging).

## State model

Ported from `_updates/messagebox.py` + `_updates/session.py`:

```go
// account-level (persisted; reference sqlite update_state table)
type mbSessionState struct{ Pts, Qts, Date, Seq int32 }
// per-channel (persisted; reference ChannelState)
type mbChannelState struct{ ChannelID int64; Pts int32 }
```

Three entry kinds mirror the reference's `ENTRY_ACCOUNT`, `ENTRY_SECRET`,
and per-channel entries (`messagebox.py:60-65`):

| Entry | Driven by | Persisted |
|---|---|---|
| `mbAccount` | updates with `pts` (private chats + small groups) | pts, date, seq |
| `mbSecret` | updates with `qts` (secret chats, count always 1) | qts |
| `mbEntry(channelID)` | channel updates (megagroup/broadcast/supergroup) | pts |

## State semantics (reference-exact)

**pts gate** (`apply_pts_info`, messagebox.py:552-575):

| Condition | Meaning | Action |
|---|---|---|
| `local + pts_count > pts` | duplicate / stale | ignore silently (counted) |
| `local + pts_count < pts` | possible gap | hold update, 100 ms deadline |
| `local + pts_count == pts` | contiguous | apply, advance pts to `pts` |

Python truthiness rule (messagebox.py:106 `if pts:`): **a zero pts/pts_count
means "no ordering info"** — such updates apply unconditionally. The
difference applier relies on this to surface `new_messages` as synthetic
`UpdateNewMessage{pts: 0, pts_count: 0}` (messagebox.py:694-698). Found
during the port and pinned by `TestMBQtsSecretEntry` /
`TestUpdateNormalizationRejectsForeign`.

**New-entry initialization** (messagebox.py:591-603): first update for an
entry initializes `pts = remote_pts - (0 if pts_count else 1)`, floored at
1 — so a count-0 `ReadChannelInbox` cannot bury a following count-1
`NewChannelMessage` with the same pts.

**Batch ordering** (messagebox.py:462-473): each incoming batch is sorted
by `pts - pts_count` before applying — Soroush sends
`ReadChannelInbox(count 0)` and `NewChannelMessage(count 1)` with the same
pts out of order.

**seq gate** (messagebox.py:450-460), only for `updates`/`updatesCombined`
(`seq_start != 0`; everything else uses seq 0 = "doesn't matter"):

| Condition | Meaning | Action |
|---|---|---|
| `local_seq + 1 > seq_start` | already handled | skip the whole batch |
| `local_seq + 1 < seq_start` | seq gap | start account difference, `GapError` |
| `local_seq + 1 == seq_start` | contiguous | apply |

seq/date commit only when **everything applied and no gaps remain**
(messagebox.py:501-509); a zero seq is never committed.

**qts** (messagebox.py:116-118): every qts-carrying update acts as
`(qts, count=1, ENTRY_SECRET)` regardless of channel_id.

## Classification (§7 — nothing silently dropped)

| Wire constructor | Handling |
|---|---|
| `updates#74ae4240` | batch with seq gate |
| `updatesCombined#725b04c3` | batch with seq_start/seq gate |
| `updateShort#78d4dec1` | single update, date only, no seq |
| `updateShortMessage#313bc7f8` | single update, carries pts |
| `updateShortChatMessage#4d6deea5` | single update, carries pts |
| `updateShortSentMessage#9015e101` | single update, carries pts |
| `updatesTooLong#e317af7e` | no date → account difference (gap) |
| bare `update*` over the stream | normalized as a one-update batch, no seq/date semantics (the reference socket only delivers the wrappers; accepted by constructor-ID family check) |
| non-Updates-family object | **rejected** by `normalizeUpdates` (fail-explicit; counted via `UpdatesUnsupported` when seen) |

Family membership is checked by constructor ID
(`client/updatefamily.go`, generated from the schema): the generated
`gen.UpdateClass` interfaces are empty, so type assertions cannot
classify — mirroring the reference's `SUBCLASS_OF_ID == crc32(b'Updates')`
assertion (mtprotosender.py:736).

## Gap handling

1. Incoming update beyond the frontier → `possibleGap{deadline: now+100ms,
   updates[]}` (`POSSIBLE_GAP_TIMEOUT`, Soroush-reduced from 500 ms,
   messagebox.py:37-39).
2. Each subsequent batch re-tries held updates (sorted); resolved ones
   are emitted, unresolved stay (messagebox.py:477-497).
3. If the deadline expires → the entry starts getting difference, the
   held list is dropped — **the difference supplies the authoritative
   range** (messagebox.py:283-294).
4. **Bounded memory (deviation)**: the reference's gap list is unbounded;
   SuroMesh caps it at 256 held updates per entry — at the cap the
   difference is forced immediately (`TestMBGapBoundForcesDifference`).
5. No infinite recovery loops: each recovery ends the difference (success
   or error); the next attempt only triggers on a NEW gap or the 15-minute
   no-update deadline (`NO_UPDATES_TIMEOUT`, messagebox.py:41-48).

## Difference synchronization

Ported from messagebox.py:609-828 and updates.py:267-471. The sync loop
runs at most one difference at a time (single-threaded state owner), in
the reference's priority order: account → channel → queued updates.

**Account difference** (`get_difference`, messagebox.py:612-628):
`updates.getDifference{pts, date, qts}` from the box state.

| Result | Reference behavior |
|---|---|
| `differenceEmpty` | adopt date/seq, finish |
| `difference` | adopt state, process `other_updates` through the normal pipeline, surface `new_messages`/`new_encrypted_messages` as synthetic pts-0 updates, finish |
| `differenceSlice` | adopt `intermediate_state`, process, **not** finished (loop continues) |
| `differenceTooLong` | adopt pts, finish, nothing dispatched |

**Channel difference** (`get_channel_difference`, messagebox.py:727-758):
`updates.getChannelDifference{force=false, channel, filter=empty, pts,
limit=100}` — the channel's access hash comes from the entity cache; if
unknown, the entry is **forgotten** (never fabricated) so the next update
rebuilds it. `ChannelDifferenceTooLong` adopts `dialog.pts` and dispatches
nothing (resync-via-dialogs semantics). `end_channel_difference` has two
premature reasons: temporary issues (pts kept, retried later) and banned
(state forgotten).

**Error matrix** (updates.py:305-347 / 361-463): ServerError/TimedOut/
FloodWait/ValueError → end difference, retry on next trigger; 401-class →
auth state effects (Phase 9); ChannelPrivate/ChannelInvalid → banned
(state forgotten); transport failures → end difference, deadline
machinery retries. No hidden request retries.

## Own updates

Reference `_store_own_updates` (mtprotosender.py:744-790), implemented in
`rpc/dispatch.go`:

- Update-family RPC results (`updates`, `updatesCombined`, `updateShort`,
  `updateShortMessage`, `updateShortChatMessage`,
  `updateShortSentMessage`) are wrapped as `rpc.OwnUpdate` and pushed to
  the updates stream: **the state machine advances but subscribers never
  see them** (no duplicate events for a successful `SendMessage`) —
  `TestUpdateEngineOwnUpdateNotDispatched`.
- `messages.AffectedHistory` / `AffectedMessages` /
  `AffectedFoundMessages` results convert into a self-outgoing
  `UpdateShort(UpdateDeleteMessages([], pts, pts_count))` with epoch date
  (the reference's anti-gap hack for bots) — pts advance, no events.
- Own updates participate fully in difference synchronization (their pts
  are state).

## Ordering vs subscriber concurrency (§12)

- **Protocol ordering**: the sync loop is the single state owner; updates
  are classified, gated, applied, and emitted strictly in stream order.
- **Subscriber concurrency**: `UpdateWorkers` (default 4) workers consume
  the ordered event queue — delivery may interleave across workers by
  design (documented since Phase 6). `UpdateWorkers=1` gives strict
  delivery order (`TestUpdateEngineOrderAndReconciliation` uses it).
- Slow subscribers never block synchronization: the event queue is
  bounded with the Phase 7 drop-newest policy and slow-consumer hook
  (`TestUpdateStressSlowSubscriberDoesNotBlock`).

## Reconnect synchronization (§17)

At connect the client loads the persisted update state into the MessageBox
(`seedUpdateStateFromSession`; the reference's catch-up seeding path,
telegrambaseclient.py:578-591). Missed updates then surface as ordinary
pts gaps on the first new update and recover through the standard
difference flow — never a blind pts reset:

- `TestUpdateEngineReconnectExactlyOnce`: persisted pts 3 → restart →
  next update pts 4 applies contiguously, delivered exactly once.
- `TestUpdateEngineReconnectWithGapRecovers`: server moved 10→20 while
  disconnected → gap → difference → consistent final state, no duplicates.
- `CatchUp()` (reference `catch_up()`, updates.py:248-262) queues a
  synthetic `updatesTooLong` to force a full difference on demand.

## Persistence decision (§18)

The reference **does** persist update state: the SQLite session keeps an
`update_state` table with pts/qts/date/seq per entity
(sessions/sqlite.py:107-149, 224-245). SuroMesh therefore persists it in
the Phase 5 session file additively: optional `upd` (account state) and
`chans` (channel pts) JSON fields, HMAC-integrity and atomic-save
behavior unchanged, V1 files load identically (fields absent → empty
state). Persistence is rate-limited to at most once per second from the
update loop (the reference defers SQLite persistence to its keepalive
loop, updates.py:298, 521-531) with a forced flush at shutdown.
Verification codes, entity payloads, or message content are never
persisted by the update engine.

## Entity synchronization (§14)

Update envelopes' `users`/`chats` feed the Phase 6 entity cache
(`_preprocess_updates`, updates.py:521-531). Only server-provided
entities are stored — access hashes rotate in place when the server sends
newer values and are never fabricated. `UserEmpty` / `Chat` /
`ChannelForbidden` are recorded where meaningful; `min`-entities (no
usable hash) are skipped. `TestUpdateEngineEntityExtractionAndRotation`
covers extraction + hash rotation.

## Metrics & hooks

Aggregate counters only (no labels, no identifiers): `UpdatesReceived`,
`UpdatesAccepted`, `UpdatesDropped`, `UpdatesDuplicate`, `UpdatesStale`,
`UpdatesUnsupported`, `UpdateGaps`, `DifferenceRequests`,
`ChannelDifferenceRequests`, `DifferenceFailures`. New hooks:
`OnUpdateGap(kind)` and `OnDifferenceSync(scope)` — no payload, no user
content, panic-isolated in the state machine.

## Reference trace

| Behavior | Reference file | Reference location | Go implementation | Tests |
| -------- | -------------- | ------------------ | ----------------- | ----- |
| entry kinds (account/secret/channel) | `_updates/messagebox.py` | 60-65 | `mbEntry`, `ptsinfo.go` | `TestMBQtsSecretEntry` |
| persisted state shape | `_updates/session.py` | 6-66 | `mbSessionState`, `mbChannelState`, session fileDoc | `TestAuthSessionSaveLoadStress` |
| pts gate (dup/stale/contiguous/gap) | `_updates/messagebox.py` | 552-575 | `applyPtsInfoLocked` | `TestMBContiguousAndDuplicate`, `TestMBGapHoldsThenResolves` |
| zero-pts truthiness | `_updates/messagebox.py` | 106, 116 | `ptsInfo` zero rule | `TestMBQtsSecretEntry` |
| new-entry init (count-0 rule) | `_updates/messagebox.py` | 577-603 | `applyPtsInfoLocked` | `TestMBFirstUpdateInitializesEntry`, `TestMBOutOfOrderSamePts` |
| batch sort by pts−count | `_updates/messagebox.py` | 462-473 | `sortBySortKey` | `TestMBOutOfOrderSamePts`, `TestUpdateEngineOrderAndReconciliation` |
| seq gate | `_updates/messagebox.py` | 450-460 | `processUpdates` | `TestMBSeqGate` |
| possible-gap timeout | `_updates/messagebox.py` | 37-39, 283-294 | `checkDeadlines` | `TestMBGapTimeoutForcesDifference` |
| no-updates timeout (15 min) | `_updates/messagebox.py` | 41-48, 286 | `checkDeadlines` | `TestMBNoUpdatesTimeout` |
| updatesTooLong → gap | `_updates/messagebox.py` | 433-436 | `processUpdates` | `TestMBUpdatesTooLongIsGap`, `TestUpdateEngineTooLongTriggersDifference` |
| updateChannelTooLong | `_updates/messagebox.py` | 525-527 | `applyPtsInfoLocked` | `TestMBChannelTooLong` |
| seq/date commit rule | `_updates/messagebox.py` | 501-509 | `processUpdates` | `TestMBAccountOwnUpdatesAdvanceWithoutEmit` |
| self-outgoing suppression | `_updates/messagebox.py` + `network/mtprotosender.py` | 416-419, 499 / 744-790 | `rpc.OwnUpdate` | `TestUpdateEngineOwnUpdateNotDispatched` |
| affected-history conversion | `network/mtprotosender.py` | 771-783 | `ownUpdateFromAffected` | pts-advance coverage via `TestMB*` |
| account difference request | `_updates/messagebox.py` | 612-628 | `getDifference` | `TestMBGetDifferenceRequestShape` |
| difference variants | `_updates/messagebox.py` | 631-704 | `applyDifference`, `applyDifferenceType` | `TestMBApplyDifferenceVariants` |
| channel difference + missing hash | `_updates/messagebox.py` | 727-758 | `getChannelDifference` | `TestMBChannelDifferenceFlow`, `TestUpdateEngineChannelDifferenceE2E` |
| channel difference variants | `_updates/messagebox.py` | 761-811 | `applyChannelDifference` | `TestMBChannelDifferenceFlow` |
| premature end (temp/banned) | `_updates/messagebox.py` | 813-828 | `endChannelDifferencePremature` | error-matrix handling |
| difference error matrix | `client/updates.py` | 305-347, 361-463 | `handleDifferenceError`, `handleChannelDifferenceError` | `TestUpdateEngineCloseDuringDifference` |
| update loop priority | `client/updates.py` | 267-519 | `syncLoop` | E2E suite |
| catch_up | `client/updates.py` | 248-262 | `Client.CatchUp` | reconnect tests |
| state persistence | `sessions/sqlite.py` | 107-149, 224-245 | session fileDoc `upd`/`chans` | reconnect persistence tests |
| update-state load at start | `client/telegrambaseclient.py` | 578-591 | `seedUpdateStateFromSession` | `TestUpdateEngineReconnectExactlyOnce` |

Documented deviations: bounded gap buffer (256 vs unbounded); deadline
polling (25 ms ticker vs exact per-deadline waits); persisted-state load
at connect (always, vs the reference's opt-in `_catch_up`); rate-limited
session persistence (1 s) mirroring the reference's keepalive deferral;
per-worker delivery interleaving preserved from Phase 6 (documented);
raw queue drop-oldest at saturation (the reference's queue is unbounded).

## Test matrix (§24)

| Area | Tests |
|---|---|
| Classification | `TestUpdateNormalizationRejectsForeign` (all 8 wrapper families + bare + foreign rejection) |
| State | `TestMBFirstUpdateInitializesEntry`, `TestMBContiguousAndDuplicate`, `TestMBSeqGate`, `TestMBOutOfOrderSamePts`, `TestMBQtsSecretEntry`, `TestMBGapBoundForcesDifference` |
| Difference | `TestMBApplyDifferenceVariants`, `TestMBChannelDifferenceFlow`, `TestUpdateEngineGapRecoveryE2E`, `TestUpdateEngineTooLongTriggersDifference`, `TestUpdateEngineChannelDifferenceE2E`, `TestUpdateEngineCloseDuringDifference` |
| Ordering | `TestUpdateEngineOrderAndReconciliation` (strict mode), `TestUpdateStress100kUpdates`, `TestUpdateStressSlowSubscriberDoesNotBlock` |
| Entity | `TestUpdateEngineEntityExtractionAndRotation` (extraction + rotation) |
| Own updates | `TestUpdateEngineOwnUpdateNotDispatched`, `TestMBAccountOwnUpdatesAdvanceWithoutEmit` |
| Reconnect | `TestUpdateEngineReconnectExactlyOnce`, `TestUpdateEngineReconnectWithGapRecovers`, `TestUpdateStress10ReconnectCycles` |
| Stress | 100k updates (0.0 MiB heap growth), 100 subscribers × 1000 updates, 32 producers (per-channel), 10 reconnect cycles |
| Fuzz | `FuzzMessageBoxProcess`, `FuzzNormalizeUpdates`, `FuzzApplyDifference`, `FuzzPendingGapInsertion` |

## Bugs found during Phase 10 (all fixed with regression tests)

1. **Zero-pts truthiness**: the port initially treated `pts=0` as real
   ordering info; Python's `if pts:` means zero = "no info" — synthetic
   difference updates (pts 0) would have been gated instead of applied.
   Fixed in `ptsInfo`; covered by the difference tests.
2. **`UpdateClass` is an empty interface**: `normalizeUpdates` accepted
   ANY object as a bare update (so would the engine's dispatcher
   classification). Family membership now checks constructor IDs
   (`updatefamily.go`, generated from the schema) — mirroring the
   reference's SUBCLASS_OF_ID assertion. Covered by
   `TestUpdateNormalizationRejectsForeign`.
3. **`SoroushPeer.SendPacket` raced its own keystream** (test
   infrastructure): concurrent sends encrypted outside the lock,
   interleaving the rolling CTR stream. Fixed: encrypt+write atomic.
   Found by `TestUpdateStress32Producers`.
4. **Raw-wrapper delivery contract superseded**: Phase 6 delivered raw
   `Updates` wrappers to subscribers; Phase 10 delivers
   state-reconciled individual updates (the phase's purpose). The two
   Phase 6 tests were updated to the new, stronger contract.

## Limitations

- Channel-state eviction beyond 4096 channels is arbitrary (bounded map;
  the reference SQLite table is unbounded).
- The 25 ms deadline poll adds up to 25 ms of latency to gap-timeout
  recovery (the reference waits exact per-deadline durations).
- `CatchUp` is on-demand only; there is no automatic catch-up on connect
  beyond gap detection (matches the reference's default `_catch_up=false`
  while the persistence makes gaps self-healing).
- Real-server behavior of pts progressions on Soroush remains unverified
  (deferred; the reference semantics are implemented exactly as traced).
