# Peers — Entities, References & Resolution (Phase 11)

The three concepts stay distinct at every layer:

```
Entity      server-returned domain info     User / Chat / Channel
PeerRef     what the caller addresses with  *User, *Chat, *Channel, PeerID,
            (implements client.PeerRef)     UsernameRef, SelfRef, nil-check
InputPeer   the minimal wire identity       InputPeerUser/Chat/Channel/Self
```

Reference: `reference/SPlusthon/splusthon/client/users.py:364-596`
(`get_input_entity`, `_get_entity_from_string`, `get_peer_id`),
`utils.py:136-231` (`get_input_peer`), `utils.py:1004-1066`
(`get_peer_id`/`resolve_id`).

## Access hashes — the absolute rule

An access hash originates ONLY from a server response that carried it
(auth envelope, dialog listing, resolve answer). It is cached in the
entity cache and rotates in place when the server sends a newer value.
A user/channel peer without a cached hash resolves to:

```go
&ErrAccessHashUnavailable{Peer: id}   // never a synthetic hash
```

Basic chats carry NO access hash in the protocol (reference
`get_input_entity` PeerChat branch, users.py:475-476): `InputPeerChat{id}`
is always constructible.

**Documented deviation (hash=0 last resort)**: the reference, as a final
fallback for bots and contacts, sends `users.getUsers(hash=0)` /
`channels.getChannels(hash=0)` (users.py:458-483). SuroMesh does NOT
attempt hash=0: this client is user-facing, the trick only works in
special cases, and a silent zero-hash request contradicts the
never-fabricate rule. Risk: a caller with a bare user/channel ID and no
cached hash must resolve the username first (or receive the entity via
updates/dialogs). Covered by `TestPeerResolveMissingHash`.

## Marked IDs

Bot-API style identifiers (reference `get_peer_id`, utils.py:1004-1053):

| Kind | Marked form | Example |
|---|---|---|
| User | `+id` | `123` |
| Chat | `-id` | `-456` |
| Channel | `-(1_000_000_000_000 + id)` | `-1000000000042` |

`client.MarkedID(PeerID)` and `client.ResolveMarkedID(int64)` round-trip
exactly (fuzz-pinned by `FuzzPeerResolution`).

## Resolution cascade (`Client.ResolvePeer`)

```
PeerRef ──already an entity─────────────────► return as-is
        ──PeerID: cache hit─────────────────► return entity
                chat (no hash needed)───────► &Chat{ID}
                user/channel, no hash──────► ErrAccessHashUnavailable
        ──SelfRef / "me" / "self"───────────► GetMe (authoritative, cached)
        ──UsernameRef / "@name"─────────────► contacts.resolveUsername
                                               (server answer decides type)
```

Self is addressed as **InputPeerSelf** on the wire once `GetMe` has run
(reference users.py:442-444) — never `InputPeerUser` for the
authenticated account (`TestPeerSelfUsesInputPeerSelf`).

Username resolution reuses the Phase 6 `ResolveUsername` (no
duplication): normalize `@`, validate non-empty, `contacts.resolveUsername`,
convert the response's `Peer` + envelope entities, seed the cache, return
the domain object. The server response is authoritative — the username's
shape never implies the peer type.

## Cache interaction

The entity cache (Phase 6, bounded, mutex-guarded) is the single store of
hashes. Every response envelope carrying `users`/`chats` — RPC results,
dialog pages, history pages, update envelopes — seeds it BEFORE domain
conversion (§16). Cache updates replace stored hashes with newer
server-provided ones ("hash rotation"); `min`-entities without usable
hashes are skipped, never guessed.

## Concurrency

`ResolvePeer` is safe for concurrent use: the cascade reads the cache
under its RWMutex and performs no unsynchronized writes. 32 concurrent
resolvers with mixed hits/misses are race-clean
(`TestStress32ConcurrentResolvers`).
