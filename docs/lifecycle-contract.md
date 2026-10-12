# Lifecycle Contract (Phase 17)

The complete client lifecycle, its legal transitions, and the guarantees
attached to each. No new state machine was introduced in Phase 17 — this
document formalizes the one that exists (client/client.go, `stateMachine`).

## Two orthogonal axes

1. **Transport lifecycle** — `client.State`:
   `New → Disconnected → Connecting → Connected → Ready → (Disconnect →
   Disconnected | Close → Closed)`. `Closed` is terminal; every other
   failure path returns to `Disconnected` and is reconnectable.
2. **Authorization state** — `client.AuthState`:
   `Unauthorized → CodePending → (PasswordRequired) → Authorized |
   Revoked`. Connected ≠ logged in.

Both are observable at any time via `Client.State()` / `Client.AuthState()`.

## Legal transitions and their API

| Transition | API | Notes |
|---|---|---|
| New → Connected/Ready | `Connect(ctx)` | Idempotent per state rules; concurrent Connect serialized by `connectMu` |
| Ready → Disconnected | `Disconnect(ctx)` | Graceful: pending RPCs are allowed to drain; reconnectable afterwards |
| any → Closed | `Close()` | **Terminal barrier**: interrupts in-flight RPCs (`ErrClosed`), cancels transfers, drains and stops workers and subscribers, flushes and closes persistence; the client can never serve again |
| — | `LogOut(ctx)` | Invalidates authorization server-side; auth state leaves `Authorized`; transport lifecycle unchanged |

## Illegal transitions fail deterministically

- `Connect` from `Ready`/`Connecting`/`Closed` → `ErrBadState`
  (client/client.go:242) — wrapped with the current state name; no
  goroutine is started.
- Operations on a closed client → `ErrClosed` (Invoke, OnUpdate, SendX,
  GetX, transfers). `OnUpdate` after Close returns `ErrClosed`.
- Auth calls from the wrong auth state (e.g. `SignIn` without a pending
  code, `CheckPassword` when no password is required) fail with the auth
  sentinel errors, never re-send network traffic speculatively
  (docs/auth.md).

## Guarantees

1. **Reconnect does not re-authenticate.** Connection loss drops the
   client to `Disconnected` (or auto-recovers per retry policy) and the
   stored session replays: auth key, DC, and authorization survive
   (Phase 15/16 reconnect workflows, scenario F).
2. **Logout invalidates authorization** and clears the durable authorized
   flag; a persisted session after logout cannot resume as authorized.
3. **Close is terminal.** `closed` is an atomic set once; every subsequent
   network call fails fast with `ErrClosed` before any I/O. A second
   `Close` is a no-op returning nil.
4. **Pending RPCs drain on Close.** Every pending request is finalized
   (with `ErrClosed`) before Close returns; the RPC registry is empty
   afterwards (Phase 15 shutdown tests; `Engine.Pending() == 0`).
5. **Iterators terminate.** Active iterators observe Close/cancellation
   and end with an error instead of blocking (docs/cancellation.md rule 7).
6. **Transfers terminate.** Close cancels all active uploads/downloads;
   terminal states are CAS'd exactly once, even when a part fails during
   teardown (TestCancelAllTransfersOnClose).
7. **Subscribers terminate.** Update workers stop; queues are drained;
   handler panics never block shutdown (TestCloseDuringSubscriptionChurn).
8. **Persistence remains coherent.** The final update-state and session
   save happens inside the Close barrier (rate limiter bypassed via
   persistNow(force)); a file written after Close reflects the keyless or
   logged-out state consistently (runtime_persistence_test.go).
9. **No goroutine survives Close.** The client returns to the
   pre-creation goroutine baseline (100-cycle leak suite, runtime_test.go).

## Session persistence vs lifecycle

- `FileSession`/`StringSession` capture **durable** state only (auth key,
  DC, address, authorization, update state). Transport-internal state
  (session ID, msg-id/seq counters) is deliberately never persisted — a
  restored client starts a fresh MTProto session over the same
  authorization, matching the reference's separation of long-lived vs
  transient state.
- Restoring a session into a new `Client` requires no re-authentication:
  workflow A (docs/application-workflows.md) proves this end-to-end.

## Test index

| Guarantee | Tests |
|---|---|
| Illegal transitions | TestConfigValidation, lifecycle_test.go state-matrix tests |
| Close semantics | hardening_test.go (Invoke/retry/update/subscription), runtime tests |
| Reconnect without re-auth | updateengine_e2e_test.go, phase16 workflows (F) |
| Drain + leak freedom | runtime_test.go (100 cycles), Phase 15 soak |
| Persistence coherence | runtime_persistence_test.go |
