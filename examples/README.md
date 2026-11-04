# SuroMesh Examples

Runnable, credential-free examples. Each program executes end-to-end
against an in-process demo fake server (`internal/demo`) that speaks the
real protocol stack — obfuscated transport, MTProto encryption, TL Layer
182 — but answers only the RPCs the example uses.

**These examples are not live-server tests.** Real Soroush Plus validation
has NOT been performed (no credentials available); the fake backs the
server side. The API shapes shown are exactly what a real deployment uses.

## Run

```sh
go run ./examples/auth      # sendCode → signIn → authorized
go run ./examples/session   # FileSession persist → close → reload (no re-auth)
go run ./examples/messages  # subscribe, dialogs, history, send, edit, close
go run ./examples/media     # upload → send → download, byte-verified
go run ./examples/updates   # update stream: exactly-once delivery, shutdown
```

## What each shows

| Example | API surface |
|---|---|
| auth | `client.New`, `Connect`, `SendCode`, `SignIn`, `GetMe`, `AuthState`, `Close` |
| session | `session.OpenFile`, `Config.Saver`, session restore without re-auth |
| messages | `OnUpdate`/`Subscription`, `GetDialogs`, `GetMessages`, `SendMessageOpts`, `EditMessage`, terminal `Close` |
| media | `Upload`, `SendMediaInput`, `DownloadMedia` (bounded memory, real part RPCs) |
| updates | update subscription, exactly-once semantics, clean teardown |

## Honest limitations

- The demo fake (`internal/demo`) seeds a local auth key instead of the DH
  exchange; `examples/auth` documents this in code. The real flow is in
  docs/auth.md.
- The fake's user/peer data is synthetic; the client's own rules
  (never fabricate an access hash, never invent message identity) are
  visible in the code paths the examples exercise.
- Examples print deterministic output and terminate on their own; none of
  them contact the network.
