# Replies (Phase 14)

Reply metadata and explicit resolution.

Reference: `MessageReplyHeader` (layer 182: reply_to_msg_id,
reply_to_top_id, reply_to_peer_id, quote fields, forum_topic,
reply_to_scheduled); `messages.getMessages` via the reference's
`_IDsIter` (messages.py:288-322).

## Metadata vs resolution

`Message.Reply *ReplyInfo` carries **metadata only** — populated from
the wire header during the canonical conversion. Field accessors never
perform network calls; resolving the target is explicit:

```go
target, err := client.ResolveReply(ctx, msg)  // messages.getMessages
```

## ReplyInfo

```go
type ReplyInfo struct {
    MessageID    int64  // replied-to message id (0 = absent)
    TopMessageID int64  // forum/thread root (0 = direct reply)
    CrossPeer    PeerID // reply_to_peer_id (cross-chat reply; zero = same chat)
    ForumTopic   bool
    Scheduled    bool
    HasQuote     bool
    QuoteText    string
    QuoteOffset  int32  // UTF-16 units on the quoted message
    QuoteLength  int32  // server-provided span (0 = not carried)
}
```

- **Direct reply**: TopMessageID == 0.
- **Thread reply**: TopMessageID != 0 (the reply targets the
  topic/thread root); `IsThreadReply()`.
- **Cross-peer reply**: CrossPeer non-zero — the target lives in a
  different chat (layer 182 reply_to_peer_id).
- **Quote**: text + offset when the server includes a quote; the length
  is derived only from server-provided quote entities (never guessed).

## Resolution

`ResolveReply(ctx, msg)` routes to `messages.getMessages`
(non-channel) or `channels.getMessages` (channel peers) with
`InputMessageID` — the reference `_IDsIter` split (messages.py:300-322).
Invalid ids (≤ 0) are skipped, never fabricated.

Outcomes:
- target exists → its domain Message (entity/media conversion included);
- target deleted/inaccessible/NotModified → `ErrReplyUnavailable`;
- no reply metadata → `ErrReplyUnavailable` (no network call);
- service-message targets surface as unavailable (documented: service
  messages are not returned as domain Messages today).

## Identity

The resolution keys on `MessageKey{Peer, ID}`: the target peer is the
message's own peer unless `ReplyInfo.CrossPeer` names another one
(`TestReplyResolutionE2E`).
