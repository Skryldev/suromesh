# Configuration Reference (S1)

## Sources and precedence

1. **Explicit defaults** (this document, `internal/config.Defaults`).
2. **Environment variables** (`SUROMESH_*`) override defaults.

A configuration **file** source is deliberately deferred to S2 (a
parser dependency is only justified by real configuration volume);
when it lands, precedence becomes: defaults < file < environment.
Secrets never go into files — environment/secret-store only.

Unknown `SUROMESH_*` keys are ignored (the environment is shared);
**reserved keys naming future features of this service are rejected**
(fail closed) so a configured-but-unimplemented capability can never be
silently ignored.

## Keys

| Env var | Default | Constraints | Effect |
|---|---|---|---|
| `SUROMESH_LISTEN_ADDR` | `:50051` | host:port | Future gRPC listener address (validated now) |
| `SUROMESH_LISTEN_TLS_CERT` | empty | file path; required with key | TLS certificate for the listener |
| `SUROMESH_LISTEN_TLS_KEY` | empty | file path; required with cert | TLS private key |
| `SUROMESH_LISTEN_PLAINTEXT_DEV` | `false` | bool | Dev-only plaintext listener; required unless the TLS pair is set |
| `SUROMESH_LOG_LEVEL` | `info` | debug\|info\|warn\|error | Log verbosity |
| `SUROMESH_LOG_FORMAT` | `json` | json\|text | Output encoding |
| `SUROMESH_ACCOUNTS_AUTO_CONNECT` | `false` | bool | Connect bound accounts at startup (S5) |
| `SUROMESH_ACCOUNTS_MUTATION_QUEUE` | `64` | 1..10000 | Per-account mutation queue depth (S5) |
| `SUROMESH_STREAMS_BUFFER` | `1024` | 16..1000000 | Per-stream event buffer (S7) |
| `SUROMESH_SHUTDOWN_TIMEOUT` | `20s` | 1s..2m | Graceful-shutdown budget |

## Reserved (rejected in S1 — fail closed)

| Env var | Phase | Reason |
|---|---|---|
| `SUROMESH_CRYPTO_KEK` | S4 | Session envelope-encryption key source; owner decision open |
| `SUROMESH_STORAGE_POSTGRES_DSN` | S4 | Persistence DSN; storage layer not built |

## Logging configuration object

The validated `Config` implements `slog.LogValuer` and emits shape
only (flags, sizes, durations) — no secret-bearing fields exist in the
configuration model. The redaction layer additionally scrubs any
registered secret values from every record (defense in depth).

## S4 additions

| Env var | Default | Constraints | Effect |
|---|---|---|---|
| `SUROMESH_STORAGE_POSTGRES_DSN` | — (required) | postgres DSN | Metadata + encrypted-session storage; fail-closed at startup |
| `SUROMESH_CRYPTO_PROVIDER` | `file` | file\|kms | Session envelope-encryption key source; kms fails closed (owner decision pending) |
| `SUROMESH_CRYPTO_FILE_KEY` | — (required when provider=file) | 32-byte raw key file | KEK for DEK wrapping |
| `SUROMESH_AUTH_BOOTSTRAP_KEY` | empty | sk_ key (≥16 chars) | Dev-only bootstrap credential with full permissions (never committed) |
| `SUROMESH_AUTH_BOOTSTRAP_TENANT` | empty | tenant name | Tenant for the bootstrap credential |
| `SUROMESH_REQUEST_TIMEOUT` | 30s | duration | Default per-RPC deadline when the caller sets none |
