# SDK Integration Boundary (Phase 24 design)

The single rule: **the service speaks business commands; only the
adapter speaks SDK.**

## Port definition (application layer owns the interface)

```go
// UpstreamGateway is the ONLY seam to the SDK. Implemented by
// internal/adapter/suromesh. Application/domain code never imports
// github.com/Skryldev/suromesh.
type UpstreamGateway interface {
    // Lifecycle
    NewClient(ctx, sessionBlob) (ClientHandle, error)
    Close(ClientHandle) error
    Connect(ctx, ClientHandle) error
    Disconnect(ctx, ClientHandle) error
    Observe(handle) (ConnectionObservation, <-chan StateEvent)

    // Auth
    SendCode(ctx, h, phone) error
    SubmitCode(ctx, h, phone, code) error
    SubmitPassword(ctx, h, password) error
    AuthorizationState(h) AuthState
    LogOut(ctx, h) error

    // Account/peers
    GetMe(ctx, h) (Peer, error)
    ResolvePeer(ctx, h, PeerRef) (Peer, error)
    PeerDetails(ctx, h, PeerRef, View) (PeerDetails, error)

    // Dialogs/messages
    ListDialogs(ctx, h, pageSize, token) (DialogPage, error)
    GetMessages(ctx, h, peer, ids) ([]Message, []missingID, error)
    StreamHistory(ctx, h, peer, limit) (<-chan MessageOrError, error)
    SendMessage / EditMessage / DeleteMessages / ForwardMessages /
    PinMessage / UnpinMessage / MarkAsRead / SearchMessages ...
    Subscribe(h) (UpdateSubscription, error)   // wraps OnUpdate
    CatchUp(ctx, h) error

    // Media
    Upload(ctx, h, spec, chunkReader) (Message, error)
    Download(ctx, h, mediaRef, chunkWriter) error
}
```

## Adapter responsibilities (implementation phase S3)

1. **Client construction**: from the decrypted session blob →
   `session.NewWithKey` + persisted update state restoration →
   `client.New`. The service never touches raw keys outside this seam.
2. **Lifecycle ownership**: the adapter is the only caller of
   `Connect/Disconnect/Close`; the AccountRunner drives it.
3. **Context propagation**: gRPC ctx → SDK calls verbatim (SDK ctx
   guarantees: exact cancellation/deadline errors).
4. **Error translation**: SDK sentinels/typed errors → domain error
   classes → (transport adapter) gRPC status + ErrorInfo reasons.
   Translation tables are exhaustive per `capability-audit.md` and
   pinned by table tests.
5. **Session persistence**: after `Connect` and on auth changes the
   adapter exports session state (via the SDK's `Saver` hook bound to
   an in-memory sink) and hands *encrypted blobs* to the persistence
   port — the adapter never sees the encryption layer.
6. **Event fan-in**: SDK `Hooks` (already sanitized: method names,
   durations, error classes) feed the observability pipeline;
   `OnUpdate` feeds the per-account fan-out hub.
7. **No mechanical 1:1 wrapping**: RPCs are business operations; SDK
   test seams and probes are unreachable by design.

## SDK upgrade policy

- The service pins the SDK version in its `go.mod` (no floating).
- SDK releases are additive (v0.x policy); the adapter's translation
  tables and the contract tests fail loudly on any behavioral drift.
- Breaking SDK changes cannot reach the service silently (SDK v0
  policy + adapter test suite + gRPC contract tests are independent
  gates).
