# SDK Capability Audit (Phase 24)

Authoritative input for every Protobuf contract in this design. The SDK
public API and tests are the source of truth; nothing is invented.
Audit method: `go doc -all ./client` (73 `*Client` methods, re-verified),
the SDK's own contract documents (`docs/error-contract.md`,
`docs/cancellation.md`, `docs/lifecycle-contract.md`), and the test
suite evidence.

Classification vocabulary:

| Class | Meaning |
|---|---|
| SUPPORTED_AND_TESTED | Implemented with unit/fake/reference-differential evidence |
| IMPLEMENTED_NOT_LIVE_VALIDATED | Implemented and offline-tested; real Soroush Plus behavior pending (project tracks this openly) |
| PARTIALLY_SUPPORTED | Works for a documented subset |
| UNSUPPORTED | No SDK API exists |
| NEEDS_INVESTIGATION | Uncertain; excluded from v1 service API |

## Connection & lifecycle

| Capability | SDK API | Class | Service exposure (v1) | Limitations |
|---|---|---|---|---|
| Establish connection (WS + obfuscated2 + DH + init) | `Connect(ctx)` | SUPPORTED_AND_TESTED; handshake+DH live-confirmed | `ConnectionService.Connect` (async job) | Handshake can take tens of seconds on slow links; budget ≥180 s |
| Graceful disconnect | `Disconnect(ctx)` | SUPPORTED_AND_TESTED | `ConnectionService.Disconnect` | Reconnectable, not terminal |
| Terminal shutdown | `Close()` | SUPPORTED_AND_TESTED | internal (account stop) | Terminal per process; service manages it |
| State observation | `State()`, `IsConnected()`, `AuthState()`, `LastError()` | SUPPORTED_AND_TESTED | `GetConnectionState`, streaming state events | Distinct axes: transport vs authorization |
| Bounded reconnect | transport-internal | SUPPORTED_AND_TESTED | documented behavior | Not configurable to unbounded — by design |

## Authentication

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| Request code | `SendCode(ctx, phone)` | IMPLEMENTED_NOT_LIVE_VALIDATED | `AuthenticationService.SendCode` | Server rate limits; one call per operator action |
| Submit code | `SignIn(ctx, phone, code)` | IMPLEMENTED_NOT_LIVE_VALIDATED | `SubmitCode` | `PASSWORD_REQUIRED` branches to 2FA |
| Sign-in with hash | `SignInWithHash` | IMPLEMENTED_NOT_LIVE_VALIDATED | exposed (operator-supplied hash) | rare path |
| 2FA / SRP | `CheckPassword(ctx, pw)` | IMPLEMENTED_NOT_LIVE_VALIDATED (golden-vector SRP) | `SubmitPassword` | OTP/password never logged or persisted plaintext |
| Logout | `LogOut(ctx)` | IMPLEMENTED_NOT_LIVE_VALIDATED | `LogOut` | Revokes server-side; session file useless afterwards |
| Auth state | `AuthState()` | SUPPORTED_AND_TESTED | `GetAuthorizationState` + stream events | |

## Account / peers

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| Self identity | `GetMe(ctx)` | IMPLEMENTED_NOT_LIVE_VALIDATED | `UserService.GetMe` | Requires authorized session |
| Peer resolution | `ResolvePeer`, `ResolvePeerString`, `ResolveUsername` | IMPLEMENTED_NOT_LIVE_VALIDATED | `UserService.ResolvePeer` | Never fabricates hashes; bare-ID failures typed |
| Metadata refresh | `GetUser/GetChannel/GetChat` | IMPLEMENTED_NOT_LIVE_VALIDATED | `UserService.GetUser/...` | Basic chats: no lightweight refresh RPC in Layer 182 |
| Full info | `GetUserFull/GetChannelFull/GetChatFull` | IMPLEMENTED_NOT_LIVE_VALIDATED | same RPCs with `View=FULL` | |
| InputPeer bridge | `InputPeerFor` | SUPPORTED_AND_TESTED | internal (adapter) | not a service RPC |

## Dialogs / history / messaging

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| List dialogs | `GetDialogs`, `IterDialogs` | IMPLEMENTED_NOT_LIVE_VALIDATED | `DialogService.ListDialogs` (paged) | iterator contract: `Next/Err`, sticky errors |
| Message page | `GetMessages(ctx, Peer, MessageQuery)` | IMPLEMENTED_NOT_LIVE_VALIDATED | `MessagingService.GetMessages` | needs resolved `Peer` |
| History iteration | `IterMessages` | IMPLEMENTED_NOT_LIVE_VALIDATED | server-streaming `StreamHistory` | bounded; cancellable |
| Send text | `SendMessage` / `SendMessageOpts` | IMPLEMENTED_NOT_LIVE_VALIDATED | `SendMessage` (idempotency key) | connection-loss ambiguity never auto-retried (SDK policy) |
| Reply | `Reply` | IMPLEMENTED_NOT_LIVE_VALIDATED | `SendMessage(reply_to)` | |
| Edit / delete | `EditMessage`, `DeleteMessages` | IMPLEMENTED_NOT_LIVE_VALIDATED | `EditMessage`, `DeleteMessages` | mutation ambiguity rules apply |
| Forward / pin | `ForwardMessages`, `PinMessage`/`UnpinMessage` | IMPLEMENTED_NOT_LIVE_VALIDATED | `ForwardMessages`, `Pin/Unpin` | |
| Read state | `MarkAsRead`, `IsMessageRead`, `IsOutgoingRead` | IMPLEMENTED_NOT_LIVE_VALIDATED | `MarkAsRead`, conversation views | |
| Search | `SearchMessages`, `IterSearchMessages`, `SearchGlobal`, `IterGlobalSearch` | IMPLEMENTED_NOT_LIVE_VALIDATED | `SearchMessages` (paged) | |
| Conversation view | `Conversation`, `TrackedConversations` | SUPPORTED_AND_TESTED | folded into dialogs/account views | derived state, not authority |

## Updates

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| Subscription | `OnUpdate(h) *Subscription` | SUPPORTED_AND_TESTED (exactly-once, ordered, own-updates silent) | server-streaming `StreamUpdates` | per-stream queue bounded; slow consumer = stream dropped (documented) |
| Catch-up | `CatchUp()` | SUPPORTED_AND_TESTED | `CatchUp` (unary, long deadline) | no caller deadline by SDK design |
| Persistent update state | `Saver` integration | SUPPORTED_AND_TESTED | session persistence (service-owned) | enables resume after restart; NOT durable replay of payloads |
| Durable replay / resume tokens | — | UNSUPPORTED | not offered in v1 | honest: no durable event log; ADR-010 |

## Media

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| Upload | `Upload(ctx, r, opts)`, `SendMedia`, `SendMediaInput` | IMPLEMENTED_NOT_LIVE_VALIDATED | client-streaming `UploadMedia` (size in first frame) | O(part) memory server-side; size limits enforced |
| Albums | `SendAlbum` | IMPLEMENTED_NOT_LIVE_VALIDATED | `SendAlbum` (unary, item list) | 200 ms grouping semantics |
| Download | `DownloadMedia`, `Download` | IMPLEMENTED_NOT_LIVE_VALIDATED | server-streaming `DownloadMedia` | streamed to client; hash verification recommended |
| File references | typed expiry errors | SUPPORTED_AND_TESTED | documented error + re-fetch guidance | no silent refresh |
| Existing media reuse | `ExistingPhotoInput/ExistingDocumentInput` | IMPLEMENTED_NOT_LIVE_VALIDATED | `UploadMedia` input variants | |

## Sessions / persistence

| Capability | SDK API | Class | Service exposure | Limitations |
|---|---|---|---|---|
| Memory session | `session.New/NewWithKey` | SUPPORTED_AND_TESTED | internal | |
| File session | `OpenFile`, `Save`, `Delete` | SUPPORTED_AND_TESTED | internal (service persists to its own store) | service stores encrypted blobs, not SDK files |
| String session | `ParseStringSession` (reference-compatible) | SUPPORTED_AND_TESTED | `ImportSession` (privileged; encrypted at rest) | sensitive — never returned to callers |
| Session security | HMAC + atomic rename | SUPPORTED_AND_TESTED | envelope encryption at rest (ADR-008) | |

## Cross-cutting

| Capability | Class | Notes |
|---|---|---|
| Context cancellation | SUPPORTED_AND_TESTED | propagated from gRPC deadlines |
| Concurrency (client safe for concurrent use) | SUPPORTED_AND_TESTED | per-account single-owner model still enforced (ADR-007) |
| Error taxonomy | SUPPORTED_AND_TESTED | mapped to gRPC status codes + error details (see api-design) |
| Raw MTProto invoke (`Invoke`) | SUPPORTED_AND_TESTED | **not** in v1 public API (ADR-003); possible future controlled extension |
| Reactions/participants/folders/invite links/payments/stories/bots | UNSUPPORTED | scope decision |
| Drafts | UNSUPPORTED (schema-blocked) | Layer 182 lacks draft RPCs |
| Multi-DC migration | UNSUPPORTED | single endpoint routing today |

## Live-validation disclaimer (carried into every contract)

Capabilities classified IMPLEMENTED_NOT_LIVE_VALIDATED carry strong
offline evidence (unit + fake-server E2E + executed-reference
differential vectors). The credential-free live path (transport, DH
exchange, first-message byte parity) is confirmed; authenticated flows
against the real service are pending. The service inherits this status
honestly: the API surface is designed and documented now, with the
same open tracking.
