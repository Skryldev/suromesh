# Application Workflows (Phase 16)

End-to-end scenarios over the real stack (transport → crypto → RPC →
client → domain), driven by the deterministic fake server. These are
application-level integration tests, NOT real-server tests.

Source: `client/phase16_workflows_test.go`, `client/phase16_invariants_test.go`.

## Scenario results

| Scenario | Flow | Test | Result |
|---|---|---|---|
| A — session restore | connect on authorized session → no auth RPCs → GetMe → dialogs | `TestWorkflowSessionRestore` | PASS |
| B — send/receive | resolve → send → own update (0 events) → incoming (exactly 1 event) → unread=1 | `TestWorkflowSendReceive` | PASS |
| C — edit/delete coherence | send → edit (authoritative text) → delete (AffectedMessages) | `TestWorkflowEditDeleteCoherence` | PASS |
| D — reply | metadata → ResolveReply (real getMessages) → MessageKey exact | `TestWorkflowReply` | PASS |
| E — media | upload (real part RPCs) → send → classify → download → byte equality | `TestWorkflowMedia` | PASS |
| F — reconnect | updates → forced conn loss → reconnect → authorized preserved → contiguous update applies | `TestWorkflowReconnect` | PASS |
| G — concurrent load | dialogs ∥ history ∥ search ∥ sends ∥ media ∥ edits ∥ updates ∥ reconnect | `TestWorkflowConcurrentLoad` | PASS |

## Invariants (§10)

| # | Invariant | Test |
|---|---|---|
| 1 | Message identity = Peer + MessageID (never ID alone) | TestInvariant1MessageIdentity |
| 2 | Own updates: zero subscriber events | TestInvariant2and3UpdateEventCounts |
| 3 | External updates: exactly one logical event | TestInvariant2and3UpdateEventCounts |
| 4 | ConversationStore is derived, never protocol authority | TestInvariant4ConversationStoreNotProtocolAuthority |
| 5 | Entity hashes originate only from server responses | TestInvariant5EntityHashesFromServer |
| 6 | Reconnect never requires re-auth with valid authorization | TestInvariant6ReconnectNoReauth |
| 7 | Close terminates every client-owned operation | TestInvariant7and8CloseBarrier |
| 8 | No pending RPCs survive terminal Close | TestInvariant7and8CloseBarrier |
| 9 | Iterators leak no goroutines | TestInvariant9IteratorNoLeaks |
| 10 | Media file references are never fabricated | TestInvariant10NoFabricatedFileReferences |

## Usage example (realistic application)

```go
c, _ := client.New(client.Config{
    Session: fs, Saver: fs,
    Host: "im-server.splus.ir:443",
})

// Restore: authorized session connects without re-auth.
if err := c.Connect(ctx); err != nil { ... }
if c.AuthState() != client.AuthAuthorized { /* run Phase 9 login flow */ }

// Conversations.
dialogs, _ := c.GetDialogs(ctx, client.DialogQuery{Limit: 20})
for _, d := range dialogs { /* render d.Peer, d.TopMessage, d.UnreadCount */ }

// History.
it, _ := c.IterMessages(ctx, dialogs[0].Peer, client.MessageQuery{Limit: 50})
for { m, ok := it.Next(ctx); if !ok { break }; /* render m */ }

// Mutations.
sent, _ := c.SendMessageOpts(ctx, dialogs[0].Peer, "hello",
    client.SendMessageOptions{ReplyTo: 42})
c.MarkAsRead(ctx, dialogs[0].Peer, 0)
c.SetTyping(ctx, dialogs[0].Peer)  // Phase 16

// Rich text: UTF-16 offsets on m.Text (docs/rich-text.md).
for _, e := range sent.TextEntities { /* render span */ }

// Media.
doc, _ := c.GetChannelFull(ctx, chPeer)  // About, ParticipantsCount…
err = c.DownloadMedia(ctx, sent, w, client.DownloadOptions{})
```
