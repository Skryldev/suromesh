# ADR-0014: Separate service documentation website

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

The SDK Docusaurus (`website/`) targets library consumers. The
service has a different audience (service operators, RPC consumers)
and lifecycle.

## Problem

Extend `website/` or create `service-docs/`?

## Considered alternatives

1. **Extend `website/`** - rejected: mixes audiences; couples build
   cycles; complicates per-product versioning.
2. **Separate `service-docs/` Docusaurus** (chosen): independent
   package.json/build/CI, protobuf-focused content, own deployment.

## Decision

`service-docs/` with its own Docusaurus 3 config (search, mermaid,
proto highlighting), documented information architecture (introduction,
getting started, authn/authz, accounts/sessions, API reference per
service, language guides, streaming/media guides, operations,
architecture). Independently buildable; CI job separate from the SDK
docs; nothing deploys without owner action.

## Rationale

Audience separation mirrors the product separation; both sites can
link to each other without coupling builds.

## Trade-offs

Two sites to maintain; shared style conventions copied (small,
documented).

## Security implications

Service docs must repeat the security handling rules (no
credentials in examples) - enforced by the same secret-scan gate.

## Operational implications

Deployment target for service-docs is decided at implementation
(Pages path or other) - owner-gated like the SDK site.
