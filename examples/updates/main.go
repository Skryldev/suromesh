// Example: consuming the update stream.
//
// The demo fake pushes three updates; the subscriber receives each exactly
// once, in MessageBox order. Cancellation and shutdown follow
// docs/cancellation.md and docs/lifecycle-contract.md.
package main

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/examples/internal/demo"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
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

	var seen atomic.Int32
	sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		seen.Add(1)
	})
	// The push happens on a goroutine after a short delay so the
	// subscription is live first.
	go func() {
		time.Sleep(100 * time.Millisecond)
		for i := int32(1); i <= 3; i++ {
			_ = srv.PushUpdate(&gen.Updates{
				Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
					Message: &gen.Message{ID: 100 + i, Message: "pushed",
						PeerID: &gen.PeerUser{UserID: 42}, Date: int32(100 + i)},
					PTS: 100 + i, PTSCount: 1,
				}},
				Users: []gen.UserClass{}, Chats: []gen.ChatClass{},
				Date: 100, Seq: 0,
			})
			time.Sleep(20 * time.Millisecond)
		}
	}()

	// A real application runs until ctx is cancelled; here we wait for the
	// three pushes, then shut down cleanly.
	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for seen.Load() < 3 {
		select {
		case <-deadline:
			demo.Fail("updates", fmt.Errorf("only %d of 3 updates arrived", seen.Load()))
		case <-tick.C:
		}
	}
	fmt.Printf("received %d updates, exactly once each\n", seen.Load())

	if err := c.Close(); err != nil {
		demo.Fail("close", err)
	}
	_ = sub // Unsubscribe is optional: Close tears down all subscribers.
	fmt.Printf("closed: state=%s auth=%s\n", c.State(), c.AuthState())
}
