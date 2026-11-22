# ADR-0012: Observability stack

**Status:** Accepted (Phase 24 design — implementation pending)

## Context

Operations need logs, metrics, traces across a multi-tenant
service holding sensitive data.

## Problem

Which observability stack and what label policy?

## Considered alternatives

1. **OpenTelemetry SDK** (chosen): OTLP export to any backend;
   vendor-neutral.
2. **Backend-specific SDKs** - rejected: coupling.

## Decision

OTel for traces (gRPC interceptors), metrics (request/stream/
upstream/account gauges), logs (structured, correlated by request-id
+ trace-id). **Cardinality policy**: labels limited to tenant-id,
account-id (opaque UUID), rpc method, status code, error class -
never phone numbers, session ids, message content, or upstream raw
ids. Correlation to SDK internals via the adapter's Hooks (already
sanitized: method names, durations, error classes). Redaction
middleware scrubs configured secret values before any emit (inherits
SDK discipline).

## Rationale

Vendor-neutral, standard instrumentation; the SDK's sanitized
hooks were designed for exactly this consumption.

## Trade-offs

OTel collector becomes an operational dependency (can start with
a stdout exporter in dev).

## Security implications

Redaction tests are mandatory; the label allowlist is
review-enforced.

## Operational implications

Dashboards/SLOs are defined in the operations documentation at
implementation time.
