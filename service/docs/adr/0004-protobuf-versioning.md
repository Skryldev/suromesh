# ADR-0004: Protobuf versioning and evolution

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

gRPC contracts outlive server implementations; clients in many
languages will pin generated code.

## Problem

How to evolve the contract without breaking clients?

## Considered alternatives

1. **v1 forever with breaking edits** - rejected: corrupts client
   trust.
2. **Buf-based breaking-change detection** - adopted as a *tool*, not
   the strategy itself.
3. **`suromesh.service.v1` package + additive evolution + reserved
   fields + v2 alongside v1 for true breaks** (chosen).

## Decision

Package `suromesh.service.v1`. Rules: new fields are appended with
explicit presence; removed fields/values are `reserved` (numbers +
names); enums append; ids are strings (UUIDs) except protocol int64
ids; timestamps are `google.protobuf.Timestamp`; pagination is opaque
tokens. CI runs buf breaking-change checks against the last release.
True breaking changes create `suromesh.service.v2` served alongside
v1 for a documented deprecation window.

## Rationale

Standard, tool-enforced, familiar to gRPC consumers; matches how
the SDK itself evolved (additive-only).

## Trade-offs

Reserving numbers costs field-number space (negligible); dual
v1/v2 serving costs an interim maintenance window.

## Security implications

None beyond normal API review.

## Operational implications

Deprecation windows must be communicated in release notes;
buf runs in CI from phase S2 onward.
