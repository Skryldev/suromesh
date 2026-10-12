# SuroMesh

Module: `github.com/Skryldev/suromesh` · Repository: `github.com/Skryldev/suromesh`

A production-oriented Go client for the Soroush Plus (SPlusthon) MTProto
API — a full rewrite of the Python client under
[`reference/SPlusthon`](reference/SPlusthon), proven against it by
executed differential tests rather than by assertion.

**Real Soroush Plus validation has NOT been performed — no real
credentials are available.** All correctness evidence comes from
differential tests that execute the Python reference, deterministic fake
servers over the real protocol stack, fuzzing, the race detector, and
stress/soak runs. See [docs/reference-compatibility.md](docs/reference-compatibility.md).

## What this is

- **Protocol**: MTProto 2.0 over the Soroush Plus WebSocket transport
  (obfuscated2 + abridged framing), **Layer 182** — 1,684 generated
  TL constructors, reflection-free, byte-compatible with the reference.
- **Shape**: a single consumer package (`github.com/Skryldev/suromesh/client`) plus session
  persistence (`github.com/Skryldev/suromesh/session`). Lower layers (`tl`, `mtproto`, `rpc`,
  `transport`, `crypto`, `auth`) are exported composition layers —
  documented in [docs/package-boundaries.md](docs/package-boundaries.md).

## Supported

- **Auth**: code request → sign-in, 2FA via SRP (line-faithful port with
  golden vectors from the executed reference), session reuse without
  re-authentication, logout, revocation detection.
- **Updates**: a ported update state machine (MessageBox) — pts/qts/seq
  ordering, gap detection with bounded buffering, difference recovery,
  exactly-once subscriber delivery, own-update semantics (your sends never
  double-fire), persistent update state.
- **Peers**: resolution cascade (self/username/cache/marked-ID) with a
  server-authoritative entity cache — access hashes are never fabricated.
- **Messaging**: dialogs, history, conversations with unread/read state,
  send/edit/delete/forward/pin/mark-read, replies (incl. cross-peer),
  search, global search — all through bounded, cancellable iterators.
- **Media**: streaming uploads/downloads with O(part) memory, big-file
  path, progress, file-reference expiry handling, albums, a canonical
  media domain model (photos, documents, stickers, voice, video...).
- **Runtime**: bounded queues with explicit overflow policies, retry
  governance (server errors and small flood-waits auto-retried; ambiguous
  mutations never), terminal `Close` barrier with full drain, session and
  update-state persistence hardened against crashes and clock regression.

## Not supported (documented decisions)

- Drafts (schema-blocked: Layer 182 lacks the draft RPCs).
- Reactions, participants enumeration/admin ops, folders, invite links,
  payments, stories, bot API — intentionally out of scope
  ([docs/phase16-capability-matrix.md](docs/phase16-capability-matrix.md)).

## Quick start

```go
cfg := client.Config{Session: session.New()}
c, err := client.New(cfg)
if err != nil { return err }
defer c.Close()

if err := c.Connect(ctx); err != nil { return err }
sent, err := c.SendCode(ctx, "+15550001111") // real flow: code arrives by SMS
// ... c.SignIn(ctx, phone, code) ... 2FA: c.CheckPassword(ctx, pw)

sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) { /* ... */ })
defer sub.Unsubscribe()

res, err := c.SendMessageOpts(ctx, client.PeerID{Kind: client.PeerUser, ID: 7},
    "hello", client.SendMessageOptions{})
```

Runnable, credential-free examples (in-process fake server):
[examples/](examples/README.md) — `go run ./examples/auth` etc.

## Sessions

`session.FileSession` persists auth key + authorization + update state
(crash-safe, versioned, atomic rename). `session.StringSession` is
byte-compatible with the reference's compact form. Transport-internal
state is deliberately never persisted — restoring a session never
re-authenticates ([docs/session.md](docs/session.md),
[docs/lifecycle-contract.md](docs/lifecycle-contract.md)).

## Concurrency guarantees

A `Client` is safe for concurrent use. One fixed goroutine set per client
(transport reader/writer/heartbeat per connection + sync loop + bounded
worker pools) — no goroutine per message, no unbounded queues. `Close` is
a global terminal barrier: in-flight RPCs fail with `ErrClosed`, pending
requests drain, transfers and subscribers terminate, persistence flushes.
Guarantees and their tests: [docs/cancellation.md](docs/cancellation.md),
[docs/error-contract.md](docs/error-contract.md).

## Reliability & observability

Retry/flood-wait policy ([docs/retry-policy.md](docs/retry-policy.md)),
resource budgets ([docs/resource-budgets.md](docs/resource-budgets.md)),
hooks + metrics ([docs/observability.md](docs/observability.md)), failure
matrix ([docs/failure-matrix.md](docs/failure-matrix.md)). No secrets
(keys, hashes, message bodies, phone numbers, query text) ever enter
errors, metrics, hooks, or logs.

## Compatibility evidence

`tests/vectors/` holds 16+ JSON fixture sets **executed from the original
Python implementation** (seeded randomness, fixed clock). `tests/compatibility`
asserts `Python bytes == Go bytes` and pins the public API surface
(`api_surface_test.go`). Ten consumer workflows, ten architectural
invariants, fuzz campaigns over every parsing boundary, `-race` clean,
deterministic soak (10k RPC / 10k updates / 1k mutations / 500 media /
100 reconnects).

The reference implementation is frozen under `reference/SPlusthon/`
(read-only; 0 modified files throughout the project).

## Development

```bash
go build ./...                      # incl. examples
go test ./...                       # unit + compatibility + protocol + workflows
go test -race ./...                 # race detector
go test -bench=. -benchmem ./client # benchmarks (docs/performance.md)
go run ./cmd/tlgen -verify          # generated code is current & deterministic
go test -fuzz=FuzzDecodeObject ./tl/  # fuzzing (campaigns in phase reports)
staticcheck ./...                   # if installed (clean as of Phase 17)
```

## Documentation

Start here: [docs/architecture.md](docs/architecture.md) ·
[docs/client.md](docs/client.md) · [docs/updates.md](docs/updates.md) ·
[docs/error-contract.md](docs/error-contract.md) ·
[docs/lifecycle-contract.md](docs/lifecycle-contract.md) ·
[docs/api-stability.md](docs/api-stability.md) ·
[docs/versioning.md](docs/versioning.md). Full index in
[docs/](docs/).

## Known limitations

See [docs/versioning.md](docs/versioning.md) for the honest v0.x verdict:
production-shape SDK, complete contracts and evidence, **not yet validated
against the live Soroush Plus service**.

## License

MIT — see [LICENSE](LICENSE) and [docs/licensing.md](docs/licensing.md).
The upstream SPlusthon reference (`reference/SPlusthon`, included for
differential testing only) is third-party GPL-3.0 material and is
excluded from release artifacts; the bundled TL schema files are verbatim
copies of the reference's schema data (provenance recorded). The SuroMesh
implementation is an original behavioral rewrite (no copied source).
