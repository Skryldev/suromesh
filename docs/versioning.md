# Versioning Readiness (Phase 17)

## Verdict

**v0.x. A v1.0 tag is NOT justified today.** The reason is not feature
count, API shape, or engineering quality — it is the one thing no amount
of local evidence can substitute: the library has never spoken to the real
Soroush Plus service.

## What is v1-ready (and pinned)

- **Public API stability**: full inventory + classification
  (docs/public-api-audit.md); additive-evolution policy
  (docs/api-stability.md); compile-time surface contract
  (`tests/compatibility/api_surface_test.go`).
- **Error contract** (docs/error-contract.md): every class distinguishable
  without string parsing; precedence rules tested.
- **Cancellation contract** (docs/cancellation.md): ten named rules, each
  enforced by a named test.
- **Lifecycle contract** (docs/lifecycle-contract.md): legal/illegal
  transitions, terminal Close, drain guarantees — all tested.
- **Reference fidelity** (docs/reference-compatibility.md): byte-level TL
  parity, line-faithful algorithm ports (SRP, MessageBox, message
  parsing), executed-Python golden vectors.
- **Engineering evidence**: `-race` clean (×3 per phase gate), fuzzing on
  every parsing boundary, deterministic soak, goroutine-leak suite,
  staticcheck/vet clean, stdlib-only module (zero dependencies — no
  supply-chain surface).

## Why not v1.0 — the honest list

1. **Live-server uncertainty (the blocker).** Real salt rotation cadence,
   real FLOOD_WAIT distributions, real file-reference lifetimes, DC
   migration behavior, server-side quirks of auth/SRP flows: all UNVERIFIED
   (docs/reference-compatibility.md). A v1.0 promise is a compatibility
   promise to consumers; this SDK cannot yet make it truthfully.
2. **API-surface aging.** The surface has never been exercised by external
   consumers. v0.x exists precisely to let real usage inform the last
   round of shape corrections *cheaply*.
3. **Known schema gaps** (drafts) and deferred P2 capabilities are
   documented but will evolve.

## The path to v1.0

1. Live-server validation against real credentials (the single
   hard requirement).
2. Fix whatever the live server teaches, without breaking the documented
   contracts (they are designed to absorb this).
3. One deprecation pass over PROVISIONAL items
   (docs/api-stability.md), then freeze.

## Rules until then

- Every breaking change: STOP protocol (document behavior, problem,
  alternatives, migration path, compatibility impact) in
  docs/api-stability.md.
- Version tags: `v0.y.z`; minor = additive API + behavior fixes; patch =
  fixes with no API change.
- The module path (`suromesh`) is placeholder-grade; before any public  release it must move to its final import path (a breaking event to
  schedule deliberately, not discover accidentally).

## Module-path migration procedure (Phase 19, exact steps)

Perform ONCE, before any publish, in a single commit:

1. Final import path: `github.com/Skryldev/suromesh` (owner-specified).
2. `go.mod` rewritten (`module github.com/Skryldev/suromesh`).
3. Rewrite every import: all in-module packages import `github.com/Skryldev/suromesh/...`.
   Mechanical, compile-verified:
   ```sh
   grep -rl '"github.com/Skryldev/suromesh/' --include="*.go" . | xargs sed -i 's|"github.com/Skryldev/suromesh/|"<final-path>/|'
   go build ./... && go vet ./... && go test -count=1 ./...
   ```
4. Update docs references to the module path (README, package-boundaries,
   api-stability, examples/README, consumer-simulation snippets).
5. Re-run the external consumer simulation with the new path.
6. Tag the migration as its own release (it is the one intentional
   breaking event in the v0.x policy — every import path changes).
7. Do NOT publish anything under the placeholder path beforehand (a
   squatted `suromesh` would make the claim unfixable).
