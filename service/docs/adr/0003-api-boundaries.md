# ADR-0003: gRPC API boundaries

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

The SDK exposes 73 methods including raw `Invoke` (full MTProto
surface). The service must present a stable, business-oriented
contract to heterogeneous clients.

## Problem

Should the service mirror the SDK method-by-method, expose raw
MTProto, or define business-oriented services?

## Considered alternatives

1. **Mirror all 73 methods** - rejected: couples the public contract
   to SDK internals (test seams, metrics probes), bloats the API, and
   breaks the stable-contract goal.
2. **Raw MTProto passthrough service** - rejected for v1: exposes
   protocol complexity to every client and makes security review
   impossible (any request, any time).
3. **Business-oriented domain services** (chosen): Authentication,
   Account, Session, Connection, User, Dialog, Messaging, Update,
   Media (+ standard Health).

## Decision

Ten domain services served by one deployable. RPCs are named for
use cases (SendMessage, ListDialogs, StreamUpdates), map to the
capability audit, and deliberately exclude: test seams, metrics
probes, raw `Invoke`, and unsupported SDK operations. Raw passthrough
is a possible future `DebugService` with separate authorization and
compatibility documentation - explicitly out of v1.

## Rationale

Stable contract decoupled from SDK evolution; smaller security
surface; protobuf remains reviewable (one .proto per domain).

## Trade-offs

Some SDK capability is not exposed initially (by design); new SDK
capability reaches the contract through additive RPCs, not automatic
mirroring.

## Security implications

Smaller surface = smaller attack surface; every RPC has an
explicit authorization requirement.

## Operational implications

Documented per-RPC; unsupported operations return typed
UNIMPLEMENTED-family errors rather than pretending.
