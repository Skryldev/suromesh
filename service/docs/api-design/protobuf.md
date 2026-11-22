# Protobuf API Design — `suromesh.service.v1` (Phase 24 proposal)

Proposed contract sources live in
[`proto/suromesh/service/v1/`](./proto/suromesh/service/v1/service.proto)
(written as **proposal** files in this phase; generation and server
implementation follow in roadmap phase S2/S3 — no generated code is
committed in this phase).

## Versioning (ADR-004)

- Package `suromesh.service.v1`; one `.proto` per domain +
  `common.proto` (pagination, error details, timestamps).
- Additive evolution only: new optional fields, new RPCs, new enum
  values appended. Removed fields/values are `reserved` (numbers and
  names) — never reused.
- Breaking changes → `v2` package served alongside `v1` during a
  deprecation window.
- Every message that can grow uses explicit presence (`optional`) for
  optional scalars; timestamps are `google.protobuf.Timestamp`; ids are
  `string` (UUIDs) except protocol ids (`int64` for message ids —
  proto `int64`, matching the SDK).

## Services (one deployable, multiple gRPC services)

| Service | RPCs (v1) | Streaming |
|---|---|---|
| `AuthenticationService` | SendCode, SubmitCode, SubmitPassword, GetAuthorizationState, LogOut | state changes observable via `UpdateService` |
| `AccountService` | BindAccount, GetAccount, ListAccounts, SuspendAccount, ResumeAccount, RemoveAccount | — |
| `SessionService` | ImportSession, GetSessionStatus, RevokeSession | — |
| `ConnectionService` | Connect (async job), Disconnect, GetConnectionState | — |
| `UserService` | GetMe, ResolvePeer, GetUser, GetChannel, GetChat (view: BASIC/FULL) | — |
| `DialogService` | ListDialogs (paged) | — |
| `MessagingService` | SendMessage, GetMessages, EditMessage, DeleteMessages, ForwardMessages, PinMessage, UnpinMessage, MarkAsRead, SearchMessages | StreamHistory (server-streaming) |
| `UpdateService` | — | StreamUpdates (server-streaming), StreamAccountEvents (server-streaming: connection/auth state) |
| `MediaService` | GetMediaMetadata | UploadMedia (client-streaming), DownloadMedia (server-streaming) |
| `Health` | standard `grpc.health.v1` (separate listening concern, ADR-013) | Watch |

## Cross-cutting conventions

- **Authn**: every RPC except `Health` requires metadata
  `authorization: Bearer sk_...` (ADR-005).
- **Request ID**: caller may set `x-request-id`; otherwise generated;
  echoed in response metadata and error details.
- **Idempotency**: mutating RPCs carry `idempotency_key` (string, UUID
  recommended); see reliability F7.
- **Pagination**: `page_size` + `page_token` (opaque, expiring);
  responses carry `next_page_token` + `total_estimate` where the SDK
  provides it (dialogs) — history uses streaming, not pages.
- **Errors**: gRPC status codes + `google.rpc.ErrorInfo` with
  `reason` (stable machine enum: e.g. `AUTH_CODE_INVALID`,
  `SESSION_REVOKED`, `OPERATION_AMBIGUOUS`, `FILE_REFERENCE_EXPIRED`)
  + `metadata` (sanitized). String parsing of messages is never
  required (SDK contract carried over).
- **Timeouts (documented defaults)**: management 10 s; auth 60 s;
  connect 300 s; catch-up 120 s; streams 30 min idle-timeout with
  keepalives.

## Representative RPC signatures (abridged; full definitions in .proto)

```protobuf
service MessagingService {
  // Sends a text message. Idempotent via request.idempotency_key.
  rpc SendMessage(SendMessageRequest) returns (SendMessageResponse);
  // Streams history newest-first for a peer. Cancellation-safe.
  rpc StreamHistory(StreamHistoryRequest) returns (stream MessageEvent);
}

message SendMessageRequest {
  string account_id = 1;               // scoped to caller tenant
  PeerRef peer = 2;                    // peer_id | username | self
  string text = 3;                     // 1..4096 chars (server limit applies)
  optional string reply_to_message_id = 4;  // decimal string of int64
  string idempotency_key = 5;
}

message SendMessageResponse {
  Message message = 1;                 // domain message incl. stable ids
  google.rpc.Status status = 2;        // always OK here; errors via trailer
}
```

## Error mapping (SDK → gRPC)

| SDK condition | gRPC status | ErrorInfo reason |
|---|---|---|
| `ErrNotReady` / account not connected | `FAILED_PRECONDITION` | `ACCOUNT_NOT_CONNECTED` |
| `ErrClosed` (account stopped) | `UNAVAILABLE` | `ACCOUNT_STOPPED` |
| ctx deadline | `DEADLINE_EXCEEDED` | — |
| ctx cancel | `CANCELED` | — |
| `*rpc.RPCError` 400-class | `INVALID_ARGUMENT` (validation-shaped) / `FAILED_PRECONDITION` | `UPSTREAM_<TYPE>` (e.g. `UPSTREAM_PEER_ID_INVALID`) |
| FLOOD_WAIT above threshold | `RESOURCE_EXHAUSTED` + retry-info | `UPSTREAM_FLOOD_WAIT` (seconds in metadata) |
| 401-class revocation | `FAILED_PRECONDITION` | `SESSION_REVOKED` |
| `ErrConnectionLost` (mutation) | `UNAVAILABLE` | `OPERATION_AMBIGUOUS` |
| `ErrFileReferenceExpired` | `FAILED_PRECONDITION` | `FILE_REFERENCE_EXPIRED` |
| authz failure | `PERMISSION_DENIED` | `TENANT_MISMATCH` / `MISSING_PERMISSION` |
| quota exceeded | `RESOURCE_EXHAUSTED` | `TENANT_QUOTA` |
| unknown/absent account for tenant | `NOT_FOUND` | `ACCOUNT_NOT_FOUND` (never leak existence cross-tenant) |

## Explicitly NOT in v1

- Raw MTProto invoke passthrough (ADR-003) — business-oriented RPCs
  only; a future `DebugService` would be a separately authorized,
  separately documented extension.
- Durable update replay/resume tokens (ADR-010).
- Multi-DC, drafts, reactions, participants, folders, payments,
  stories, bots (SDK scope).
