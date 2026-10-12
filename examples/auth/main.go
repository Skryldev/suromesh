// Example: the full authorization flow against the demo fake server.
//
// The flow shape is exactly what a real deployment does: sendCode → signIn
// → authorized. The demo fake accepts the fixed code "12345" — a real
// server delivers the code by SMS/app, and the real-server flow is
// documented in docs/auth.md. Real Soroush Plus validation has NOT been
// performed (no credentials available); this example proves API shape, not
// live compatibility.
package main

import (
	"context"
	"fmt"

	"github.com/Skryldev/suromesh/examples/internal/demo"
)

func main() {
	srv, err := demo.Start()
	if err != nil {
		demo.Fail("start", err)
	}
	defer srv.Close()
	demo.InstallAuth(srv, "12345")

	c, err := srv.NewClient()
	if err != nil {
		demo.Fail("connect", err)
	}
	defer c.Close()

	ctx := context.Background()

	sent, err := c.SendCode(ctx, "+15551230000")
	if err != nil {
		demo.Fail("sendCode", err)
	}
	fmt.Printf("state after sendCode: state=%s auth=%s codeHash=%s\n",
		c.State(), c.AuthState(), sent.PhoneCodeHash)

	if _, err := c.SignIn(ctx, "+15551230000", "12345"); err != nil {
		demo.Fail("signIn", err)
	}
	me, err := c.GetMe(ctx)
	if err != nil {
		demo.Fail("getMe", err)
	}
	fmt.Printf("authorized: state=%s auth=%s user=%s\n",
		c.State(), c.AuthState(), me.FirstName)
}
