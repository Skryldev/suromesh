// Example: dialogs, history, sending, and consuming updates.
//
// The demo fake echoes every sent message back as a server update, so the
// subscriber observes the own-update semantics live (state advances, the
// send itself emits no duplicate event).
package main

import (
	"context"
	"fmt"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/examples/internal/demo"
	"github.com/Skryldev/suromesh/tl"
)

func main() {
	srv, err := demo.Start()
	if err != nil {
		demo.Fail("start", err)
	}
	defer srv.Close()
	demo.InstallAuth(srv, "12345")
	demo.InstallMessaging(srv)

	c, err := srv.NewClient()
	if err != nil {
		demo.Fail("connect", err)
	}
	defer c.Close()
	ctx := context.Background()

	// Subscribe to server updates BEFORE doing anything else.
	got := make(chan tl.Object, 16)
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		select {
		case got <- upd:
		default: // demo: never block the delivery worker
		}
	})
	defer sub.Unsubscribe()

	// Dialogs (page 1).
	dialogs, err := c.GetDialogs(ctx, client.DialogQuery{Limit: 10})
	if err != nil {
		demo.Fail("dialogs", err)
	}
	fmt.Printf("dialogs: %d\n", len(dialogs))

	// History of the demo peer.
	msgs, err := c.GetMessages(ctx, client.PeerID{Kind: client.PeerChat, ID: 55}, client.MessageQuery{Limit: 5})
	if err != nil {
		demo.Fail("history", err)
	}
	fmt.Printf("history: %d messages\n", len(msgs))

	// Send — the fake echoes the message as an update.
	if _, err := c.SendMessageOpts(ctx, client.PeerID{Kind: client.PeerChat, ID: 55}, "hello from the example",
		client.SendMessageOptions{}); err != nil {
		demo.Fail("send", err)
	}

	// Edit the message we just sent.
	if _, err := c.EditMessage(ctx, client.PeerID{Kind: client.PeerChat, ID: 55}, 10, "edited by the example"); err != nil {
		demo.Fail("edit", err)
	}

	// Consume one update (the echo). Bounded wait keeps the demo
	// deterministic even if the delivery worker is busy.
	select {
	case upd := <-got:
		fmt.Printf("update received: %T\n", upd)
	case <-time.After(5 * time.Second):
		demo.Fail("update wait", fmt.Errorf("no update within 5s"))
	}

	// Clean shutdown: Close drains everything (docs/lifecycle-contract.md).
	if err := c.Close(); err != nil {
		demo.Fail("close", err)
	}
	fmt.Printf("closed: state=%s\n", c.State())
}
