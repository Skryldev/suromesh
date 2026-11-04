# Phase 17 Audit & Bug Register

SDK-readiness audit: public API, package boundaries, static analysis,
error/cancellation/lifecycle contracts, consumer workflows, examples,
module hygiene, performance/memory/concurrency evidence.

## Bug register

Every defect discovered in Phase 17, with the mandated fields.

### BUG-17-1 — dead engine parameter masked intended retry semantics review

- **ID**: BUG-17-1
- **Symptom**: `staticcheck` SA4009 on `client/invoke.go`: the `eng
  *rpc.Engine` argument of `invokeWithRetries` was overwritten before its
  first use.
- **Root cause**: `invokeWithRetries(ctx, eng, req, attempt)` immediately
  re-fetched the engine via `c.engine()`; the caller-passed engine was
  dead. `Invoke` also kept a now-useless early engine fetch alive only to
  pass it down.
- **Severity**: low (latent smell — behavior was correct because
  `c.engine()` is deterministic; but the dead parameter made the retry
  path *look* snapshot-based, inviting a future wrong refactor).
- **Affected subsystem**: client/invoke.go (RPC retry path).
- **Fix**: removed the parameter and the dead local; `Invoke` keeps its
  fast-fail engine check for error precedence, now as `if _, err :=
  c.engine(); err != nil`.
- **Regression test**: covered by the existing retry/close tests
  (`TestCloseDuringRetryBackoff`, `TestRetryOnReconnectReplays`) — full
  suite green after the change.
- **Compatibility impact**: none (unexported function).

### BUG-17-2 — dead-no-op finalize branch in RPC result error path

- **ID**: BUG-17-2
- **Symptom**: `staticcheck` SA4006 on `rpc/dispatch.go`: `if _, won :=
  e.reg.finalizeByMsgID(...); won {}` — an empty branch.
- **Root cause**: leftover from a refactor; the finalize call (side
  effect) is required, the branch was not.
- **Severity**: cosmetic.
- **Affected subsystem**: rpc/dispatch.go (result correlation).
- **Fix**: plain call discarding both returns. Also removed the unused
  `debugProtoErrCnt` counter declared beside it.
- **Regression test**: tests/rpc suite (malformed-response and
  correlation tests) green.
- **Compatibility impact**: none.

### BUG-17-3 — replies reader dead-initialized slices

- **ID**: BUG-17-3
- **Symptom**: `staticcheck` SA4006 ×2 on `client/replies.go`.
- **Root cause**: `users := []gen.UserClass{}` / `chats :=
  []gen.ChatClass{}` initialized then unconditionally overwritten by all
  switch branches before use.
- **Severity**: cosmetic.
- **Affected subsystem**: client/replies.go.
- **Fix**: `var` declarations.
- **Regression test**: replies/read-state tests green.
- **Compatibility impact**: none.

### BUG-17-4 — `client.MessageBox` exported without any public producer

- **ID**: BUG-17-4
- **Symptom**: `client.MessageBox` (the ported update state machine, 23
  methods) appeared in godoc as public API although no public function or
  method returns it and zero code outside package `client` referenced it
  (external test references were comments only).
- **Root cause**: exported during the Phase 10 port and never revisited.
- **Severity**: medium (API-surface misrepresentation: suggested consumers
  could own update state, contradicting the architecture's sole-authority
  rule).
- **Affected subsystem**: client public surface.
- **Fix**: unexported to `messageBox` (type rename across 8 files,
  compile-verified; concept name kept in prose comments and docs).
- **Regression test**: full suite + surface contract test
  (`tests/compatibility/api_surface_test.go`).
- **Compatibility impact**: `client.MessageBox` removed from the public
  surface. No external consumer could hold a `*MessageBox` (nothing
  returned one), so the break is theoretical; documented in
  docs/public-api-audit.md and the Phase 17 report.

### BUG-17-5 — test-double package at a public import path

- **ID**: BUG-17-5
- **Symptom**: `github.com/Skryldev/suromesh/transport/wstest` (in-memory MTProto server
  double) occupied a public import path, indistinguishable from supported
  API.
- **Root cause**: created beside the transport in Phase 3 for the fakes
  and never relocated once `internal/` usage was possible.
- **Severity**: medium (SDK clarity).
- **Affected subsystem**: transport tree / test infrastructure.
- **Fix**: moved to `github.com/Skryldev/suromesh/internal/wstest`; import paths rewritten
  across client tests and `tests/*` (same module, so `internal/` imports
  remain legal).
- **Regression test**: full suite green; examples compile
  (examples import it via the module-internal path).
- **Compatibility impact**: public import path removed; the package was
  test-support only and never part of a documented workflow.

### BUG-17-6 — deprecated randomness source in the upload path

- **ID**: BUG-17-6
- **Symptom**: `staticcheck` SA1019: `math/rand.Read` (deprecated since
  Go 1.20) used for upload file IDs.
- **Root cause**: parity with the reference's `generate_random_long`
  (Python `random`) — but Go's `math/rand.Read` is deprecated API on the
  production path.
- **Severity**: low.
- **Affected subsystem**: client/upload.go (`randomInt64`).
- **Fix**: `crypto/rand.Read` (still random, non-deterministic, standard
  library; file IDs are protocol identifiers, not secrets — no
  constant-time requirement).
- **Regression test**: media upload tests (part framing, big-file path)
  green.
- **Compatibility impact**: none (IDs remain uniformly random int64s).

### BUG-17-7 — test-only defects (static analysis)

- **ID**: BUG-17-7
- **Symptom**: `SA1032` argument-order false-positive trap in
  `rpc/pending_test.go` (sentinel-vs-sentinel `errors.Is`), `SA4006` in
  protocol/bench tests, `S1021` ×3 in tests/rpc, `SA1012` nil Context in a
  fuzz target, ~15 unused test helpers/fields (U1000).
- **Root cause**: accumulated test scaffolding.
- **Severity**: cosmetic (test hygiene).
- **Fix**: all cleaned; `staticcheck ./...` now reports exactly one
  finding (see below).
- **Compatibility impact**: none (tests only). Two `tests/rpc` closures
  required two-step `var h *harness` declarations because the response
  callbacks run before `startHarness` returns.

### BUG-17-8 — poll-based closer in TestRetryOnReconnectReplays starves under suite load

- **ID**: BUG-17-8
- **Symptom**: `tests/rpc` `TestRetryOnReconnectReplays` timed out (60 s)
  during one full-suite run; historically a recurring flake.
- **Root cause**: the helper goroutine that kills the first connection
  polled `eng.Pending()` with a 15 s budget. Under full-suite parallel
  load (the 137 s client suite, soak and stress tests running
  concurrently) the poll could starve past its budget, the connection was
  never killed, and the invoke waited out its whole 60 s context. The
  Phase 15 fix note ("deterministic closer") had been applied to the
  client-side variant; the tests/rpc copy still polled.
- **Severity**: medium (test determinism; no production code involved).
- **Affected subsystem**: tests/rpc/integration_test.go.
- **Fix**: event-driven closer — `<-firstDelivery` (closed by the fake on
  first delivery, i.e. the request is provably registered and in-flight)
  followed by `peer1.Close()`. No polling, no time budget; cannot starve.
  This *strengthens* the test (the trigger is now the exact causal event).
- **Regression test**: 5 consecutive single-test runs + 2 consecutive
  parallel runs of tests/rpc + client + tests/* (recreating the original
  load condition) — all green.
- **Compatibility impact**: none (test only).

## staticcheck disposition

`staticcheck ./...` result: **1 finding**, in a frozen file:

- `tl/gzip.go:38` SA4006 — `out := make([]byte, 0, min(len(packed)*4,
  limit))` is a capacity hint discarded when `io.ReadAll` reassigns.
  Behavior-identical either way (ReadAll allocates its own buffer); this
  is a micro-inefficiency, not a correctness defect. Per the frozen-layer
  policy (docs/api-stability.md; phase reports) the TL runtime is not
  modified without a proven correctness defect — **documented, not
  touched**.

## Audit findings recorded without code changes

- `CatchUp()` lacks a `context.Context` parameter (documented in
  docs/cancellation.md as intentional: it posts to the owning sync loop
  and inherits the client lifetime; changing the signature would break
  the API without a semantic gain).
- The six `*ForTest` seams on `*Client` remain exported because the
  external `client_test` package (37 files, the deliberate
  external-consumer test architecture) requires them; classified INTERNAL
  and pinned by the surface contract test (docs/public-api-audit.md).
