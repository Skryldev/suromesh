# Resource Budgets (Phase 15)

Every structure classified BOUND / STREAMED / CALLER-OWNED /
PROVEN-CONSTANT, with defaults and rationale. No accidental unbounded
growth is permitted.

| Structure | Class | Default bound | Rationale / overflow policy |
|---|---|---|---|
| RPC pending registry | BOUND | MaxPending 8192 | FAIL policy: PendingLimitHits metric, error before wire |
| engine.updates channel | BOUND | 1024 | DROP_NEWEST + UpdatesDropped metric |
| update rawQ | BOUND | 4096 | DROP_OLDEST (protects the pts frontier) |
| update event queue | BOUND | UpdateQueue 1024 | 250 ms grace → DROP_NEWEST + OnSlowConsumer |
| update workers | BOUND | UpdateWorkers 4 (1..N) | caller-configured pool |
| MessageBox gap buffer | BOUND | 256/entry | overflow forces difference (bounded-memory deviation, documented) |
| MessageBox entries | BOUND | per-channel map, chanStateLimit 4096 persisted | arbitrary eviction beyond bound (derived/pts state; difference recovers) |
| subscriber registry | BOUND | caller churn; no hard cap | UNLIMITED by design — each entry is one callback ref; churn tested |
| subscriber event queue | BOUND | see update event queue | shared |
| entity cache | BOUND | EntityCacheSize 4096 | refuses new keys at capacity (retention policy) |
| conversation store | BOUND | ConversationCacheSize 1024 | arbitrary eviction (derived state only) |
| album collector | BOUND | MaxPending 64 × MaxPerAlbum 10 | oldest flush at capacity; Dropped() counter |
| transfer semaphore | BOUND | MaxConcurrentTransfers 16 | BLOCK on caller ctx |
| transfer buffers | BOUND | part size ≤ 512 KiB / chunk ≤ 512 KiB | O(part) streaming, never O(file) |
| history/search/global iterators | STREAMED | O(page) | one chunk + dedup/offset state |
| dialog iterator | STREAMED | O(page + seen) | dedup set is O(dialogs seen), inherent to dedup |
| persisted session file | BOUND | key + state + ≤4096 channel pts entries | bounded by chanStateLimit |
| auth hashes map | CALLER-OWNED | — | per-SRP call, freed on return |
| response mapping maps | CALLER-OWNED | — | per-request, freed on return |
| MessageBox state struct | PROVEN CONSTANT | O(entries) | per-entry pts only; no history retained |

## Verified invariants (tests)

- 10k pending storm → pending == 0 at end (TestPendingInvariantStorms)
- 100 concurrent transfers with failures/cancel/Close → active == 0
  (TestTransferManagerHardening)
- 1000 media messages → ~0 MiB conversion growth (Phase 13)
- 64 MiB / 256 MiB logical transfers → O(part) heap (Phase 8/13 audits)
- slow subscriber → drop policy, no growth (Phase 10/15)
