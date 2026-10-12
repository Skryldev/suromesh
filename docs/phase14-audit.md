# Phase 14 Architectural Audit

The mandatory pre-implementation audit (§3): current state, reference
mapping, gaps, ownership decisions, and explicitly rejected features.

## 1. Current architecture (Phases 1–13)

| Layer | Owner | State |
|---|---|---|
| Transport/crypto/TL (frozen) | generator output | 47 files, 1684 ctors, LAYER 182, current |
| RPC engine | `rpc/` | bounded pending registry, retry classification |
| MessageBox | `client/updates.go` + `messagebox.go` | sole ordered protocol state (pts/qts/seq, gaps, difference) |
| Entity cache | `client/entities.go` | authoritative hashes, RWMutex |
| Peer resolution | `client/peerref.go` | cascade, no fabrication |
| Dialogs/history/search | `client/dialogs.go`, `history.go`, `message_search.go` | caller-owned iterators, O(page) |
| ConversationStore | `client/conversations.go` | derived state, sync-loop writer, RWMutex readers |
| Mutations | `client/message_mutation.go` + `message_response.go` | stateless; messageparse.py port |
| Media | `media_domain.go`, `media_location.go`, `media_send.go` | canonical classifier + resolver |
| Albums | `client/albums.go` | caller-owned, bounded |

## 2. Message identity audit (§11)

All `map` keyed structures inspected:

| Structure | Key | Verdict |
|---|---|---|
| ConversationStore.m | `PeerID` | correct (peer-scoped) |
| MessageBox.entries | `mbEntry` (account/secret/channel) | correct (pts sequences are per-entry) |
| MessageBox last-ID dedup | per-entry `lastID` | correct — message IDs are only unique within a peer sequence |
| mutation response `randomToID` | per-request random_id | correct (request-scoped) |
| AlbumCollector.pending | `grouped_id` | correct (server-assigned, album-scoped) |
| subscriber registry | registration id | correct |

**No `map[messageID]Message` misuse found.** Phase 14 formalizes the
identity as `MessageKey{Peer PeerID, ID int64}` (+ `Message.Key()`) so
future consumers cannot regress; reply resolution and global search use it.

## 3. Rich text / entities (§5)

All 18 layer-182 `MessageEntity*` constructors are generated. The
reference computes entity offsets in **UTF-16 code units**
(extensions/markdown.py:62 — "work on byte level with the utf-16le
encoding to get the offsets right"; helpers.py:39-45 `add_surrogate`
converts SMP → surrogate pairs because "SoroushPlus offsets are
calculated with these").

Go consequence: Go strings are UTF-8, so offsets must NOT be used as
byte or rune indices. Phase 14 adds:

- `utf16Len(s)` — the wire length of a Go string
- `TextEntity{Kind, Offset, Length (UTF-16 units), payload fields}` —
  typed conversion of every generated entity constructor, with
  `UnknownTextEntity{WireCtor}` preserving future/unknown ctors
- `Message.TextEntities` (typed) added alongside the existing raw
  `Message.Entities` (Phase 11 compat, retained)

## 4. Replies (§6)

`MessageReplyHeader` (layer 182) carries: `reply_to_msg_id`,
`reply_to_top_id` (forum/thread root), `reply_to_peer_id` (cross-chat
reply), `quote_text/quote_entities/quote_offset`, `reply_to_scheduled`,
`forum_topic`. Phase 11 exposed only `ReplyToMessageID`.

Phase 14 adds `Message.Reply *ReplyInfo` distinguishing:
direct reply (no top id), thread reply (top id present → the reply
targets a forum topic/thread root), cross-peer reply (reply_to_peer_id),
and quote metadata. **Metadata only** — resolution of the replied-to
message is an explicit operation (`ResolveReply`) requiring a
GetMessages round trip; field accessors never hide network calls.

## 5. Read state (§7)

Phase 11 wired `updateReadHistoryInbox/Outbox`,
`updateReadChannelInbox/Outbox` into the ConversationStore with the
reference's max-id semantics. Audit found the surface complete for
inbox/outbox maxima and unread counts. Phase 14 adds the public
`Conversation.IsRead(messageID, out)` predicate (id ≤ max_id of the
matching direction — the reference's read rule) and mention counts
already flow. `updateReadMessagesContents` (media-contents read) maps
to activity-only derived state (no per-message cache exists to flip).

## 6. Drafts (§8) — DEFERRED

`updateDraftMessage#1b49ec6d` exists in the schema (updates flow to
subscribers raw), but the layer-182 schema contains **no**
`messages.getAllDrafts` / `messages.setDraftMessage` /
`messages.getPeerDrafts` / `messages.clearAllDrafts` RPC constructors —
verified against `tl/schema/api.tl`. Implementing draft get/set would
require modifying the frozen schema/generator for RPCs the layer does
not declare. **Deferred with this documented reason**; revisit if the
schema gains them. (The reference's `iter_drafts` at dialogs.py:267-324
traces against those absent RPCs.)

## 7. Global search (§10) — IMPLEMENTED

Phase 12 deferred `messages.searchGlobal` pending identity attribution.
The audit resolves the concern: every returned message carries its own
`peer_id`, so attribution is per-message and `MessageKey` makes the
peer+id pairing explicit — a message ID alone is never treated as
globally unique. The reference iterator (messages.py:80-90, 279-285)
provides the continuation semantics: `offset_rate = response.next_rate`,
`offset_peer = last message's input chat` (entity-cache resolved;
`InputPeerEmpty` fallback when unresolvable — messageparse.py:279-283),
`offset_id = last message id`. `MessagesMessagesSlice.NextRate` carries
the rate. Implemented as `IterGlobalSearch`/`SearchGlobal` with the same
pagination-safety discipline as history/search. `MessageEmpty` entries
(inaccessible peers) are skipped like the reference (messages.py:196-198).

## 8. Pins (§9) — no second subsystem

Phase 12's Pin/Unpin remain the only mutation path; pin state
propagation (`updatePinnedMessage`-family updates and dialog
`Pinned` flags) already flows through the MessageBox → ConversationStore
in ordered order. No new pin state added; `Pinned` remains a dialog-level
flag served by `GetDialogs`.

## 9. Saved messages — DEFERRED

Saved Messages = the user's own "me" peer, which already works through
the existing peer resolution (`SelfRef` → `InputPeerSelf`) and ordinary
messaging APIs. There is no distinct saved-message protocol state in the
layer-182 subset the reference exercises; nothing to build without
speculation.

## 10. Conversion unification (§12)

Single canonical path confirmed: `messageFromTL` is the only
TL-message → domain-Message converter; history/search/dialogs/mutations/
updates all reach it (verified across the test matrix). Phase 14 keeps
that invariant and adds cross-path equivalence tests for the new fields.

## 11. Compatibility risks

- `Message.Entities []gen.MessageEntityClass` stays (Phase 11 compat);
  the typed `Message.TextEntities` is additive.
- `Message.ReplyToMessageID` stays; `Message.Reply *ReplyInfo` is
  additive.
- No signature changes to any Phase 1–13 public API.

## 12. Explicitly rejected from this phase

- Draft RPCs (absent from frozen schema — see §6).
- Reactions (out of scope per Phase 13 §32, unchanged).
- Media/grouped filters in search (future phase).
- Bots/inline/takeout/DC migration (never in scope).
- A message-content cache (speculative; server remains authoritative).
