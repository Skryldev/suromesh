# ADR-0001: Repository and module strategy

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

SuroMesh SDK is a Go module (`github.com/Skryldev/suromesh`) with
zero external dependencies, a frozen public API, and its own Docusaurus
site. A new gRPC service product needs its own module, protobuf
sources, generated code, and dependencies (google.golang.org/grpc,
protobuf).

## Problem

Where should the service live: inside the existing repository as a
nested module, or in a separate repository?

## Considered alternatives

1. **Same module as the SDK** - rejected: would pull gRPC
   dependencies into the SDK (forbidden by the phase constraints) and
   couple release cycles.
2. **Separate repository** - cleanest isolation; but early-stage
   products benefit from atomic cross-cutting changes (docs, shared
   CI patterns, version bumps travel together), and the service is
   tightly versioned against a not-yet-published SDK.
3. **Nested module `service/` in the same repository** (chosen).

## Decision

A nested module: `service/go.mod` declaring
`module github.com/Skryldev/suromesh/service`, requiring
`github.com/Skryldev/suromesh` at a **published version**. During
pre-publication development a `replace` directive pointing at the
relative path (`../`) is permitted; release builds MUST NOT carry a
machine-specific replace, and the release checklist enforces this.
Both modules build and release independently; the SDK never imports
the service; no cycles.

## Rationale

One repository gives atomic history and shared review culture; the
module boundary gives the dependency isolation. Go tooling fully
supports nested modules (the service module has its own go.mod/go.sum).

## Trade-offs

Two module dependency graphs to maintain; documented trade-off.
The nested layout is a known, reversible choice - extraction to a
separate repository later is mechanical.

## Security implications

CI gains per-module jobs with credential-free gates per module.
No security impact beyond that.

## Operational implications

Releases are independently taggable (SDK v0.x.y, service v0.x.y);
CI builds both modules independently. Risk of accidental SDK coupling
is mitigated by the boundary rule: the service's domain layer never
imports SDK packages (only the adapter does).
