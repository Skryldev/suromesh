# API Stability Audit (Phase 14)

The classification of every public API surface after Phase 14. Rules:

- **STABLE** — covered by compatibility guarantees; breaking changes
  require a documented migration.
- **PROVISIONAL** — correct today, may gain additive fields/options;
  removal or signature change is not planned but not yet frozen.
- **MUST NOT CHANGE** — wire-visible or persistence-visible semantics.
- **DEPRECATED** — retained for compatibility; migration documented.
- **INTERNAL** — not part of the public contract despite being exported
  for test/diagnostic access.

## Client lifecycle & config

| API | Class | Notes |
|---|---|---|
| `New`, `Connect`, `Close`, `Disconnect` | STABLE | Phase 6/7 semantics |
| `State()`, `IsConnected`, `LastError` | STABLE | |
| `Config` (all fields incl. `Transfer`, `ConversationCacheSize`) | STABLE | additive fields only |
| `Hooks` (all fields) | STABLE | additive fields only |
| `Metrics`, `TransferMetrics`, `DroppedUpdates` | STABLE | aggregate shape |

## Auth & session

| API | Class | Notes |
|---|---|---|
| `AuthState`, `SendCode`, `SignIn`, `SignInWithHash`, `CheckPassword`, `LogOut` | STABLE | Phase 9 |
| `session.Session/FileSession` public methods | MUST NOT CHANGE | file format V1 + additive Phase 10 update-state fields (HMAC envelope) |
| `ParsePhone`, `MarkedID`, `ResolveMarkedID` | STABLE | reference-parity |

## Peers & entities

| API | Class | Notes |
|---|---|---|
| `Peer`, `PeerID`, `User`, `Chat`, `Channel` | STABLE | additive fields only (Chat/Channel.MigratedTo added Phase 11) |
| `PeerRef`, `UsernameRef`, `SelfRef`, `ResolvePeer`, `ResolvePeerString` | STABLE | Phase 11 |
| `ResolveUsername`, `InputPeerFor` | STABLE | `InputPeerFor` renamed from Phase 6 `ResolvePeer` (documented migration) |
| `SeedEntities`, `SeedRaw`, `InvalidateEntity`, `ClearEntities`, `CachedEntities` | STABLE | |

## Messaging

| API | Class | Notes |
|---|---|---|
| `SendMessage(ctx, peer, text)` | STABLE | Phase 6 signature |
| `SendMessageOpts`, `Reply`, `EditMessage`, `DeleteMessages`, `ForwardMessages`, `PinMessage`, `UnpinMessage` | STABLE | Phase 12 |
| `GetMessages`, `MessageQuery` | STABLE | `MessageQuery` additive fields only |
| `OnUpdate`, `Subscription`, `UpdateHandler` | STABLE | delivery semantics documented (Phase 10) |
| `CatchUp` | STABLE | Phase 10 |
| `SendMedia(ctx, peer, io.Reader, opts)` | STABLE | Phase 8 signature (implemented on SendMediaInput) |
| `SendMediaInput`, `SendAlbum`, `MediaInput` family | PROVISIONAL | Phase 13; additive options only |
| `DownloadMedia`, `DownloadOptions` | STABLE | Phase 8/13 semantics |

## Message domain

| API | Class | Notes |
|---|---|---|
| `Message` core fields (ID, PeerID, FromID, Text, Date, Out, Raw) | STABLE | Phase 6 |
| `Message.Entities []gen.MessageEntityClass` | DEPRECATED (reading) | retained; prefer `TextEntities` (Phase 14) |
| `Message.TextEntities`, `UnknownTextEntities`, `Reply` | PROVISIONAL | Phase 14; additive fields only |
| `Message.Media`, `GroupedID`, `InAlbum`, `Views`, `Forward` | PROVISIONAL | Phase 13; additive fields only |
| `ReplyToMessageID` | STABLE | Phase 11 compat field |
| `Media` interface, `Photo`, `Document`, `TextEntity`, `ReplyInfo`, `MessageKey` | PROVISIONAL | additive fields only; no method changes |

## Updates & conversations

| API | Class | Notes |
|---|---|---|
| `AuthState()` (update-state) | STABLE | Phase 10 |
| `Conversation(peer)`, `TrackedConversations` | STABLE | Phase 11 |
| `IsMessageRead`, `IsOutgoingRead` | PROVISIONAL | Phase 14 |
| `SearchMessages`, `IterMessages`, `SearchIterator` methods | STABLE | Phase 11/12 |
| `SearchGlobal`, `IterGlobalSearch`, `GlobalSearchQuery/Result` | PROVISIONAL | Phase 14 (new) |
| `ResolveReply` | PROVISIONAL | Phase 14 (new) |
| `IterDialogs`, `GetDialogs`, `DialogIterator` | STABLE | Phase 11 |

## Transport/session internals

| API | Class | Notes |
|---|---|---|
| `rpc.Engine`, `rpc.RPCError` (Code/Type/Message/Argument) | MUST NOT CHANGE | error classification contract |
| `rpc.OwnUpdate` | INTERNAL | test/infrastructure seam |
| `wstest.*` | INTERNAL | test doubles only; moved to `github.com/Skryldev/suromesh/internal/wstest` in Phase 17 (invisible outside the module) |
| `crypto.*`, `transport.*` | MUST NOT CHANGE | frozen layers |

## Phase 16 additions

| API | Class | Notes |
|---|---|---|
| `GetUser`, `GetChannel`, `GetChat` | PROVISIONAL | metadata refresh; hash-required rule |
| `GetUserFull`, `GetChannelFull`, `GetChatFull` | PROVISIONAL | typed full-info projections |
| `UserFullInfo`, `ChannelFullInfo`, `ChatFullInfo` | PROVISIONAL | additive fields only |
| `SetTyping` | STABLE | stateless mutation |
| `Message.IsService`, `Message.Service`, `ServiceInfo` | PROVISIONAL | additive Message fields |
| `messageFromTL` widened to `gen.MessageClass` | INTERNAL | history/search now keep service messages |

## Deprecated (with migration)

| API | Since | Migration |
|---|---|---|
| `Message.Entities` (reading) | Phase 14 | use `Message.TextEntities` (typed) or `Message.Raw` (raw TL) |
| history dropping service messages | Phase 16 (fixed) | service messages now appear in history with `IsService`/`Service` |
| `Client.ResolvePeer` (wire form, Phase 6) | Phase 11 | `InputPeerFor` for wire conversion; `ResolvePeer` now resolves to domain entities |

## Phase 17 additions

| API | Class | Notes |
|---|---|---|
| Public API surface contract test (`tests/compatibility/api_surface_test.go`) | STABLE | compile-time pin of consumer signatures + INTERNAL seam behavior |
| `client.MessageBox` | REMOVED from public surface | was INTERNAL-BY-MISTAKE (no public API returned it; zero external references). Now `messageBox` (unexported). Justified in docs/public-api-audit.md. |
| `github.com/Skryldev/suromesh/transport/wstest` | MOVED to `github.com/Skryldev/suromesh/internal/wstest` | test-support package must not occupy a public import path. Import-rewrite only; no behavior change. |
| `ClassifyError`, `AsFloodWait`, `AsConnectionLost` | STABLE | now contractual: docs/error-contract.md |
| `Close/Connect/Disconnect/Invoke` cancellation + Close semantics | STABLE | contractual: docs/cancellation.md, docs/lifecycle-contract.md |
| Dead unexported code (~30 symbols across client/rpc/auth/wstest) | REMOVED | staticcheck U1000; no API impact |

Consumer-facing method signatures: unchanged in Phase 17 (verified by the
surface contract test). The only public-surface removals are the two
INTERNAL-BY-MISTAKE items above, both documented with compatibility
analysis in docs/public-api-audit.md.
