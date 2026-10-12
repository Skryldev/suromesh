# Phase 16 Capability Matrix

Python reference vs Layer-182 schema vs Go public API vs tests vs
architectural feasibility. Classification is exactly one of:
IMPLEMENTED / PARTIAL / MISSING / BLOCKED_BY_SCHEMA /
INTENTIONALLY_DEFERRED / NOT_APPLICABLE.

Priority: P0 essential for application usability · P1 high-value ·
P2 deferrable · P3 niche.

## Account / session

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| sendCode/signIn/2FA/logOut | auth.py | auth.* | Phase 9 | IMPLEMENTED | — |
| session persistence + integrity | sessions/* | file format | Phase 5/15 | IMPLEMENTED | — |
| session restore without re-auth | telegrambaseclient | — | Phase 9/11 | IMPLEMENTED | — |
| logout data wipe | auth.py:593 | auth.logOut | Phase 9 | IMPLEMENTED | — |

## Peer / entity metadata

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| ResolveUsername | users.py:571 | contacts.resolveUsername | Phase 6/11 | IMPLEMENTED | — |
| Cache-first resolution | users.py:364 | — | Phase 11 | IMPLEMENTED | — |
| **User metadata refresh (network)** | users.py:463-474 (users.getUsers) | users.getUsers#d91a548 | MISSING | **P0** |
| **Channel metadata refresh (network)** | users.py:477-483 (channels.getChannels) | channels.getChannels#a7f6bbb | MISSING | **P0** |
| **User full info** | users.py helpers | users.getFullUser#b60f5918 | MISSING | **P1** |
| **Channel full info** | dialogs.py full variants | channels.getFullChannel#8736a09 | MISSING | **P1** |
| **Chat full info** | messages.getFullChat#aeb00b34 | MISSING | **P1** |
| participants enumeration | channels.getParticipants | ctor present | MISSING | P2 (deferrable: large surface, pagination + participant domain) |
| admin operations | channels.* | ctors present | MISSING | P2 |

## Messaging

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| send/reply/edit/delete/forward/pin/unpin | messages.py | auth entourage | Phase 12 | IMPLEMENTED | — |
| history/search/global-search iteration | messages.py | full | Phase 11/14 | IMPLEMENTED | — |
| typing indicator (send) | messages.py | messages.setTyping#58943ee2 | MISSING | **P1** |
| service message representation | message.py (action) | MessageService | PARTIAL (raw only; history skips) | **P1** (minimal typed action) |
| reactions | messages.py | ctors present | INTENTIONALLY_DEFERRED (Phase 13 scope rule) | P2 |
| views/read counters | message.py views | fields present | IMPLEMENTED (Message.Views, Phase 13) | — |
| sponsored/payments/stories | — | ctors present | NOT_APPLICABLE (scope) | P3 |

## Media

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| photo/document/video/audio/voice/sticker/animated domain | Phase 13 trace | full | Phase 13 | IMPLEMENTED | — |
| albums send/receive | sendMultiMedia | full | Phase 13 | IMPLEMENTED | — |
| file reference lifecycle | downloads.py | — | Phase 13 | IMPLEMENTED | — |
| web documents | downloads.py:967+ | MessageMediaWebPage | PARTIAL → UnknownMedia | INTENTIONALLY_DEFERRED (HTTP dependency) | P2 |

## Drafts / read state

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| read state | read-history updates | full | Phase 11/14 | IMPLEMENTED | — |
| MarkAsRead | send_read_acknowledge | per-kind split | Phase 11 | IMPLEMENTED | — |
| drafts get/set | dialogs.py:267 | **no draft RPCs in schema** | BLOCKED_BY_SCHEMA | — |
| pinned state | Phase 12 | full | IMPLEMENTED | — |

## Updates / runtime

| Capability | Ref | L182 | Go | Class | P |
|---|---|---|---|---|---|
| MessageBox pts/qts/seq | messagebox.py | — | Phase 10 | IMPLEMENTED | — |
| difference recovery | updates.py | full | Phase 10 | IMPLEMENTED | — |
| runtime hardening | — | — | Phase 15 | IMPLEMENTED | — |

## P0/P1 scope selected for Phase 16

1. **P0 — peer metadata refresh**: `GetUser`, `GetChannel` (network
   refresh via users.getUsers / channels.getChannels, hash required from
   the cache — never fabricated; envelope seeding on response).
   Value: bare IDs from updates become usable; the reference's
   `get_entity` network path (users.py:239-320) becomes available.
2. **P1 — full info**: `GetUserFull`, `GetChannelFull`, `GetChatFull`
   typed wrappers over users.getFullUser / channels.getFullChannel /
   messages.getFullChat with About/participants-count/settings fields.
3. **P1 — typing indicator**: `SetTyping(ctx, peer, action)` with
   SendMessageTypingAction (messages.py:631-946 uses the same peer
   resolution; the reference sends typing via messages.setTyping).
4. **P1 — service message representation**: minimal typed
   `Message.Service` for MessageService payloads (action ctor + pinned
   detection), so pin/edit-photo service messages are representable
   without raw TL — history conversion classifies them safely.

Everything else: deferred with the classifications above.
