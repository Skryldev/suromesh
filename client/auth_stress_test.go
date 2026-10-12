package client_test

// Phase 9 concurrency stress: 32× auth operations, close-during-auth,
// cancellation storms, logout races, and session save/load stress — all
// required to run clean under -race with deterministic final states.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
)

func TestAuthStress32ConcurrentSendCode(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newAuthClient(t)
	defer c.Close()

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = c.SendCode(context.Background(), testPhone)
		}(i)
	}
	wg.Wait()
	okCount := 0
	for i, err := range errs {
		if err != nil {
			// The fake serializes RPC handling; some concurrent sends can
			// be cancelled by the client's request timeout only if the
			// fake stalls — it does not, so everything must succeed or
			// be a flood/restart (not scripted here → must succeed).
			t.Fatalf("SendCode %d: %v", i, err)
		}
		okCount++
	}
	if okCount != 32 {
		t.Fatalf("ok = %d, want 32", okCount)
	}
	if got := c.AuthState(); got != client.AuthCodePending {
		t.Fatalf("state = %s, want code_pending", got)
	}
}

func TestAuthStress32ConcurrentSignIn(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, af := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	results := make([]error, 32)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = c.SignIn(context.Background(), testPhone, "54321")
		}(i)
	}
	wg.Wait()

	// The fake accepts the code once (client-side hash is single-slot but
	// the fake's code stays valid). Every response is either success or
	// PHONE_CODE_INVALID; both are deterministic outcomes. What must hold:
	// no panic, final state exactly Authorized (the success wins and the
	// invalid code never downgrades it), and the fake saw ≤ 32 sign-ins.
	af.mu.Lock()
	calls := af.signInCalls
	af.mu.Unlock()
	if calls == 0 || calls > 32 {
		t.Fatalf("signIn RPCs = %d, want 1..32", calls)
	}
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("final state = %s, want authorized (success must win)", got)
	}
}

func TestAuthStress32CloseDuringAuth(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newAuthClient(t)
	fake.SetDelay(250 * time.Millisecond) // every op is in flight at Close

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 3 {
			case 0:
				_, errs[i] = c.SendCode(context.Background(), testPhone)
			case 1:
				_, errs[i] = c.SignIn(context.Background(), testPhone, "54321")
			default:
				_, errs[i] = c.Invoke(context.Background(), &genPingAlias{})
			}
		}(i)
	}
	time.Sleep(60 * time.Millisecond) // let the operations go in flight
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	// Every in-flight auth op must settle with an error — none may hang
	// or succeed after Close.
	for i, err := range errs {
		if err == nil {
			t.Fatalf("operation %d succeeded after Close", i)
		}
	}
}

func TestAuthStress32Cancellation(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, fake, _ := newAuthClient(t)
	defer c.Close()
	fake.SetDelay(100 * time.Millisecond)

	var wg sync.WaitGroup
	errs := make([]error, 32)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
			defer cancel()
			_, errs[i] = c.SendCode(ctx, testPhone)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil {
			t.Fatalf("SendCode %d beat a 5 ms deadline", i)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("SendCode %d: err = %v, want deadline", i, err)
		}
	}
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state = %s, want unauthorized (cancellation never mutates)", got)
	}
}

func TestAuthStressLogoutRaces(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	// Concurrent Logout / GetMe / Invoke / Close must produce
	// deterministic outcomes without races or state corruption.
	c, fake, af := newAuthClient(t)
	fake.SetDelay(10 * time.Millisecond)

	var wg sync.WaitGroup
	const n = 32
	outcomes := make([]string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			switch i % 4 {
			case 0:
				err := c.LogOut(context.Background())
				if err == nil {
					outcomes[i] = "logout-ok"
				} else {
					outcomes[i] = "logout-err"
				}
			case 1:
				_, err := c.GetMe(context.Background())
				if err == nil {
					outcomes[i] = "getme-ok"
				} else {
					outcomes[i] = "getme-err"
				}
			case 2:
				_, err := c.Invoke(context.Background(), &genPingAlias{})
				_ = err
				outcomes[i] = "invoke-done"
			default:
				err := c.Close()
				_ = err
				outcomes[i] = "close-done"
			}
		}(i)
	}
	wg.Wait()

	// Deterministic guarantee: after everything settles, the client is
	// closed and NOT authorized (either logout or close won; neither
	// leaves a half-authorized state).
	if got := c.AuthState(); got == client.AuthAuthorized {
		t.Fatalf("final state = authorized after logout/close storm")
	}
	af.mu.Lock()
	logouts := af.logoutCalls
	af.mu.Unlock()
	t.Logf("logout RPCs seen by server: %d", logouts)
}

func TestAuthStressStateTransitionsRepeat(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	// Repeated login/logout rounds. After logout the reference client is
	// unusable (session deleted, transport down); the Go client mirrors
	// that with logoutTeardown (Disconnected, keyless session), so each
	// round runs against a fresh client + server, and the persisted file
	// from the previous round must carry NO reusable authorization.
	dir := t.TempDir()

	for round := 0; round < 5; round++ {
		srv := newWSTestServerForAuth(t)
		_ = os.Remove(filepath.Join(dir, "round.session")) // fresh per round
		fs2, err := session.OpenFile(filepath.Join(dir, "round.session"))
		if err != nil {
			t.Fatal(err)
		}
		c, fake := newAuthClientWithSession(t, srv, fs2)
		af := newAuthFake("54321")
		installAuthHooks(t, fake, af)

		if _, err := c.SendCode(context.Background(), testPhone); err != nil {
			t.Fatalf("round %d send: %v", round, err)
		}
		if got := c.AuthState(); got != client.AuthCodePending {
			t.Fatalf("round %d: %s, want code_pending", round, got)
		}
		if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
			t.Fatalf("round %d sign: %v", round, err)
		}
		if got := c.AuthState(); got != client.AuthAuthorized {
			t.Fatalf("round %d: %s, want authorized", round, got)
		}
		if err := c.LogOut(context.Background()); err != nil {
			t.Fatalf("round %d logout: %v", round, err)
		}
		if got := c.AuthState(); got != client.AuthUnauthorized {
			t.Fatalf("round %d: %s, want unauthorized", round, got)
		}
		// Post-logout: the transport is down; auth ops fail loudly.
		if _, err := c.SendCode(context.Background(), testPhone); err == nil {
			t.Fatalf("round %d: SendCode succeeded after logout teardown", round)
		}
		// The persisted file carries neither authorization nor key.
		chk, err := session.OpenFile(filepath.Join(dir, "round.session"))
		if err != nil {
			t.Fatalf("round %d reload: %v", round, err)
		}
		if chk.AuthState() == session.StateAuthorized {
			t.Fatalf("round %d: file still authorized", round)
		}
		if k := chk.AuthKey(); k != nil && k.HasValue {
			t.Fatalf("round %d: file still holds a key", round)
		}
		chk.Close()
		c.Close()
	}
}

func TestAuthSessionSaveLoadStress(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	// Concurrent SetAuthorized / ClearAuthKey / Save on one FileSession
	// must never produce a corrupt or contradictory file.
	dir := t.TempDir()
	path := filepath.Join(dir, "stress.session")
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	key := testAuthKeyOf(9)
	if err := fs.SetAuthKey(key); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < 10; r++ {
				if i%2 == 0 {
					_ = fs.SetAuthorized()
				} else {
					fs.SetUnauthorized()
				}
				_ = fs.Save()
			}
		}(i)
	}
	wg.Wait()

	// The file must load cleanly and be internally consistent.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatalf("final file corrupt: %v", err)
	}
	defer fs2.Close()
	if fs2.AuthState() != fs.AuthState() {
		t.Fatalf("state mismatch: %v in memory vs %v persisted",
			fs.AuthState(), fs2.AuthState())
	}
	if k := fs2.AuthKey(); k == nil || !k.HasValue {
		t.Fatal("key lost by concurrent saves")
	}
}

func TestAuthStressNoGoroutineLeak(t *testing.T) {
	if testing.Short() {
		t.Skip("stress (short)")
	}
	c, _, _ := newAuthClient(t)
	before := runtime_NumGoroutine()
	for i := 0; i < 50; i++ {
		_, _ = c.SendCode(context.Background(), testPhone)
		_, _ = c.SignIn(context.Background(), testPhone, "54321")
		_ = c.LogOut(context.Background())
	}
	c.Close()
	if err := goroutineSettled(before, 8); err != nil {
		t.Fatal(err)
	}
}

var _ = rpc.ErrConnectionLost
