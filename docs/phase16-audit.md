# Phase 16 Audit

Architecture decisions, implemented/deferred capabilities, public API
changes, compatibility, performance, security, concurrency, test
strategy, and the bug register.

## Architecture decisions

1. **Peer metadata refresh lives in the peer layer** (`client/peer_info.go`)
   — the same resolution cascade (cache-first, hash-required) as Phase 11,
   with network refresh via users.getUsers / channels.getChannels. No new
   state owner: responses seed the existing entity cache.
2. **Full-info wrappers are read-only projections** — UserFullInfo /
   ChannelFullInfo / ChatFullInfo carry server-provided fields only;
   nothing is cached beyond the existing entity cache; no second source
   of truth.
3. **Service messages convert through the canonical path** —
   `messageFromTL` now accepts `gen.MessageClass` (Message and
   MessageService); MessageService produces a domain Message with
   `IsService`/`Service` set. History/search digests now KEEP service
   messages (reference messages.py:196-198 skips only MessageEmpty).
4. **Typing indicator** rides the standard mutation path (stateless RPC,
   no state impact).

## Implemented capabilities

| Capability | RPCs | Public API |
|---|---|---|
| User metadata refresh | users.getUsers | `GetUser(ctx, PeerRef) (*User, error)` |
| Channel metadata refresh | channels.getChannels | `GetChannel(ctx, PeerRef) (*Channel, error)` |
| Basic chat lookup | cache-only (no refresh RPC for chats) | `GetChat(ctx, PeerRef) (*Chat, error)` |
| User full info | users.getFullUser | `GetUserFull` → `UserFullInfo` |
| Channel full info | channels.getFullChannel | `GetChannelFull` → `ChannelFullInfo` |
| Chat full info | messages.getFullChat | `GetChatFull` → `ChatFullInfo` |
| Typing indicator | messages.setTyping | `SetTyping(ctx, PeerRef) error` |
| Service messages | — (conversion) | `Message.IsService`, `Message.Service *ServiceInfo` |

## Deferred capabilities (with reasons)

| Capability | Class | Reason |
|---|---|---|
| Reactions | INTENTIONALLY_DEFERRED | Phase 13 scope rule carried forward; ctors exist but reaction state needs its own derived model |
| Participants enumeration | P2 | channels.getParticipants present; large pagination + participant-domain surface |
| Admin/moderation ops | P2 | ctors present; permission semantics untestable without a server |
| Folders | P2 | ctor present; UI-level concept |
| Invite links | P2 | ctors present; join-flow semantics |
| Web documents | INTENTIONALLY_DEFERRED | HTTP dependency (Phase 13 rule) |
| Drafts | BLOCKED_BY_SCHEMA (Phase 14) | no draft RPCs in layer 182 |

## Public API changes

All additive:
- `GetUser`, `GetChannel`, `GetChat`, `GetUserFull`, `GetChannelFull`,
  `GetChatFull`, `SetTyping`
- `UserFullInfo`, `ChannelFullInfo`, `ChatFullInfo` (typed projections)
- `Message.IsService`, `Message.Service *ServiceInfo`, `ServiceInfo`
- `messageFromTL` widened to `gen.MessageClass` (internal signature)

No Phase 1–15 signature changed. `docs/api-stability.md` updated.

## Security

- Access hashes never fabricated: GetUser/GetChannel require the cached
  hash; bare IDs fail with ErrAccessHashUnavailable.
- Full-info extraction carries only server-provided fields.
- About/text/username values never enter metrics or hooks.
- Metadata responses seed the entity cache before conversion (single
  conversion path preserved).

## Concurrency

All new operations are stateless request/response (no new goroutines,
no new locks). Entity-cache seeding uses the existing RWMutex path.
Metadata refresh is safe for concurrent use.

## Performance

Metadata retrieval costs one RPC round trip (~25-36 µs against the fake
transport) plus cache seeding; no hot-path regression (Phase 15
benchmarks unchanged; verified in the Phase 16 gate run).

## Bug register (Phase 16)

1. **History silently dropped service messages** — symptom: pin/edit-photo
   service payloads invisible in history iteration. Root cause: digest
   type-asserted `*gen.Message` only. Reference keeps service messages
   (messages.py:196-198 skips only MessageEmpty). Fix: digest converts
   MessageService via the class-level path. Regression:
   `TestServiceMessageRepresentation`.
2. **Missing metadata-refresh APIs** — symptom: bare IDs from updates
   could not be refreshed without a raw RPC. Fix: GetUser/GetChannel
   with cache-required hashes. Regression: `TestGetUserMetadataRefresh`,
   `TestGetChannelMetadataRefresh`.

## Known limitations

- Chat metadata has no lightweight refresh RPC in layer 182 (basic chats
  carry no access hash; full info via messages.getFullChat).
- Service-message actions beyond the six mapped kinds degrade to
  KindName (wire type) — still representable, never dropped.
- Participant enumeration beyond count deferred (P2).
