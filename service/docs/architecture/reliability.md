# Reliability Design (Phase 24)

Distributed-systems behavior for the service, grounded in the SDK's
documented semantics (retry governance, mutation ambiguity, terminal
Close, bounded queues).

## Availability axes (never conflated)

| Axis | Measured by | Independently reported via |
|---|---|---|
| gRPC process health | liveness/readiness probes, health service | `grpc.health.v1.Health` per service |
| Storage availability | repository health check | readiness failure when down |
| Account connection state | observed ConnectionState per account | `ConnectionService.GetState` + stream events |
| Soroush Plus availability | upstream error rates, handshake failures | metrics + account last_error_class |

A healthy process with zero connected accounts is **healthy** (it can
serve management APIs) — connection state is data, not liveness.

## Failure matrix

| # | Event | Expected behavior | Risks accepted |
|---|---|---|---|
| F1 | Upstream WebSocket disconnects | SDK bounded reconnect; ConnectionState observed → DISCONNECTED; streams stay open and emit state events; pending SDK ops fail per SDK policy | brief event gaps during reconnect |
| F2 | Account session expires/revoked (401-class) | observed `AUTH_REVOKED` event; account status → `PENDING_AUTH` (re-bind required); session marked revoked; no auto re-auth (would need OTP) | downtime until operator re-binds |
| F3 | Process crash mid-operation | on restart: sessions restored from encrypted store; operations stuck in RUNNING/PENDING → reclassified **AMBIGUOUS** (outcome unknown) | caller must reconcile via reads; no fabricated success |
| F4 | Streaming consumer disconnects | server cancels the stream ctx; subscription freed; no buffering for the dead consumer | events during the gap are lost on that stream (at-most-once per stream; ADR-010) |
| F5 | Two instances claim one account | v1 single-process: impossible by deployment; future sharding: PostgreSQL lease with fencing token — loser detects fenced writes and stops its client | split-brain window bounded by lease TTL; ADR-007 |
| F6 | Storage unavailable | management RPCs fail `UNAVAILABLE`; account runners keep last-known desired state (do not silently stop clients); readiness probe fails | cannot bind/auth/persist while down |
| F7 | Request times out after upstream may have succeeded | operation → AMBIGUOUS (if mutation); caller gets timeout status + detail; idempotency key retained so a retry with the same key returns the recorded (ambiguous) outcome, never a silent duplicate | caller reconciliation burden (reads) |
| F8 | Slow update consumer | per-stream bounded buffer; overflow policy **disconnects that stream** (documented); consumer reconnects; catch-up via SDK persisted state | lost events for that stream only |
| F9 | Media transfer interrupted | client-streaming: partial upload discarded (upload id invalid); server-streaming: consumer resumes by re-issuing download; no server-side resume in v1 | re-upload/re-download cost |
| F10 | OTP RPC interrupted | stateless: re-request a code (rate-limited); OTP never stored | SMS cost |
| F11 | Storage corruption of a session blob | typed load failure → account PENDING_AUTH + alert; no fallback to plaintext | re-bind required |

## Deadlines, cancellation, backpressure

- Every RPC carries a gRPC deadline. Defaults: management 10 s;
  auth flows 60 s; connect orchestration 300 s (handshake can take
  tens of seconds); catch-up 120 s; media streams 30 min.
- Cancellation propagates: gRPC ctx → use case → SDK calls (SDK
  guarantees exact ctx error propagation).
- Bounded resources: per-account mutation queue (default 64),
  per-tenant concurrent operations, per-stream event buffer (default
  1024), global in-flight media bytes.

## Idempotency & retry

- Mutating RPCs accept an `Idempotency-Key` (UUID). The service records
  (tenant, key) → OperationStatus for a retention window (default 24 h)
  and replays the recorded outcome (including AMBIGUOUS) on retry.
- Retries of `SUCCEEDED` return the original result reference.
- The service never retries mutations upstream; the SDK's own policy
  (500-class, small flood-waits) is preserved and surfaced through
  events.

## Graceful shutdown ordering

1. Stop admission (health → NOT_SERVING; new RPCs rejected).
2. Cancel server streams (consumers see cancellation; they reconnect).
3. Drain/abort in-flight operations (record outcomes; ambiguous stays
   ambiguous).
4. Stop account runners: SDK `Close()` per account (drains, saves).
5. Flush persistence; close storage; exit 0.
