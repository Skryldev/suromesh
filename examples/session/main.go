// Example: session persistence — authorize, persist, close, reload.
//
// A restored session never re-authenticates: the saved auth key and
// authorization state are replayed as-is. The demo fake backs the server;
// with a real server the flow is identical (docs/session.md).
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Skryldev/suromesh/examples/internal/demo"
	"github.com/Skryldev/suromesh/session"
)

func main() {
	dir, err := os.MkdirTemp("", "suromesh-demo-session")
	if err != nil {
		demo.Fail("tempdir", err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "demo.session")

	srv, err := demo.Start()
	if err != nil {
		demo.Fail("start", err)
	}
	defer srv.Close()
	demo.InstallAuth(srv, "12345")

	ctx := context.Background()

	// First run: authorize; the FileSession persists automatically via
	// the Saver hook. (The key is seeded for the demo fake; a real client
	// obtains it through the DH exchange during Connect.)
	fs, err := session.OpenFile(path)
	if err != nil {
		demo.Fail("open session", err)
	}
	if fs.AuthKey() == nil {
		if err := fs.SetAuthKey(srv.Key()); err != nil {
			demo.Fail("seed key", err)
		}
	}
	c1, err := srv.NewClientWithSession(fs.Session, fs)
	if err != nil {
		demo.Fail("connect", err)
	}
	if _, err := c1.SendCode(ctx, "+15551230000"); err != nil {
		demo.Fail("sendCode", err)
	}
	if _, err := c1.SignIn(ctx, "+15551230000", "12345"); err != nil {
		demo.Fail("signIn", err)
	}
	fmt.Printf("first run: auth=%s file=%s\n", c1.AuthState(), filepath.Base(path))
	if err := c1.Close(); err != nil {
		demo.Fail("close", err)
	}

	// Second run: reload from disk — no SendCode/SignIn anywhere.
	fs2, err := session.OpenFile(path)
	if err != nil {
		demo.Fail("reopen", err)
	}
	srv2, err := demo.Start()
	if err != nil {
		demo.Fail("restart", err)
	}
	defer srv2.Close()
	demo.InstallAuth(srv2, "12345")
	c2, err := srv2.NewClientWithSession(fs2.Session, fs2)
	if err != nil {
		demo.Fail("connect restored", err)
	}
	defer c2.Close()
	me, err := c2.GetMe(ctx)
	if err != nil {
		demo.Fail("getMe after restore", err)
	}
	fmt.Printf("restored run: auth=%s user=%s (no re-authentication)\n",
		c2.AuthState(), me.FirstName)
}
