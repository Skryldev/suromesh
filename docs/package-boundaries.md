# Package Boundaries (Phase 17)

Authoritative statement of the intended package hierarchy, who may import
what, and which packages are part of the recommended consumer API.

## Hierarchy (dependency direction top → down)

```text
                consumer code
                     │
        ┌────────────▼────────────┐
        │  client                 │  consumer surface (STABLE)
        └─┬──────┬──────┬─────────┘
          │      │      └──────────────┐
   ┌──────▼─┐ ┌──▼─────┐        ┌──────▼──────┐
   │ session│ │ tl     │◄───────┤ tl/gen      │  schema-derived (STABLE)
   └──────┬─┘ └──┬─────┘        └─────────────┘
          │      │
   ┌──────▼──────▼──────────────────────────┐
   │ rpc · auth · mtproto · transport(+ws,  │  composition surface
   │ abridged) · crypto(+rsakeys)           │  (REQUIRED; usable, not primary)
   └────────────────────────────────────────┘

   internal/wstest      test-support server double (never for consumers)
   internal/tlgen       generator library (never for consumers)
   cmd/tlgen            codegen CLI (tooling)
   tests/...            cross-phase integration/stress/fuzz suites (tooling)
   examples/...         runnable, credential-free demos (documentation)
   tools/vectors        golden-vector generator executed against the Python
                        reference (dev tooling)
```

## Import rules

1. `client` is the only package consumers are expected to program against.
   It re-exports everything a consumer needs; where generated types are part
   of signatures (`tl.Object`, `gen.*Class`), importing `tl` / `tl/gen` is
   expected and supported.
2. `session` is the persistence surface: `Memory`, `FileSession`/`OpenFile`,
   `StringSession`/`ParseStringSession`. The reference-compatible compact
   form is byte-identical to `sessions/string.py` (both v1 layouts).
3. `rpc`, `auth`, `mtproto`, `crypto`, `crypto/rsakeys`, `transport`,
   `transport/ws`, `transport/abridged` are the **composition surface**:
   exported because `client` composes them. They may be read for
   observability (`rpc.Snapshot`, `transport.Snapshot`) and extended by
   advanced users, but no consumer workflow requires them.
4. `internal/*` is invisible outside the module by construction. Phase 17
   moved the in-memory MTProto server double there (`github.com/Skryldev/suromesh/internal/wstest`,
   formerly `transport/wstest`) so that a test fake cannot be mistaken for
   public API.
5. `tests/**` and `cmd/tlgen` are tooling: they are not import targets and
   make no compatibility promises.
6. Frozen layers (do not modify without a proven defect, see
   docs/api-stability.md and the phase reports): `tl` (wire runtime), `tl/gen`
   (generated output), `crypto`, `mtproto` (serialization), `transport`
   (protocol: obfuscated2/abridged framing), `internal/tlgen`.

## Exported-but-not-consumer-facing

Full per-symbol register in docs/public-api-audit.md. Summary:

- Global test seams: `mtproto.Rand`, `crypto/rsakeys.ProductionRand`,
  `tl.SetDispatchHook` — production code never swaps them.
- `*Client` test seams (`SeedRaw`, `SeedEntities`, `ClassifyMediaForTest`,
  `ResolveMediaLocationForTest`, `ClosedForTest`,
  `ReconnectTransportForTest`) — used by the module's external test
  package; not part of the recommended API; pinned by
  `tests/compatibility/api_surface_test.go`.

## Rationale: no further splits

A deeper split (e.g., extracting iterators or the media engine into their
own packages) was evaluated and rejected:

- The `client` package is cohesive around one state-owning type (`Client`);
  the internal ownership rules (MessageBox sole ordering authority,
  ConversationStore derived, entity cache server-authoritative, RPC registry
  sole pending authority, TransferManager sole transfer authority) all
  assume single-package visibility of the loop and its workers.
- Extra packages would force exported accessors for internals that are
  currently unexported, *growing* the public surface — the opposite of the
  Phase 17 goal.
- Consumers already get a clean two-package programming model
  (`client` + `session`), with `tl`/`tl/gen` only for typed access to wire
  objects.
