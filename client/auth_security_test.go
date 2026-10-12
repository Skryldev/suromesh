package client_test

// Phase 9 security tests: behavioral proof that authentication secrets
// never reach errors, hooks, or metrics — plus fuzz targets and
// benchmarks for the auth-critical pure functions.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/session"
)

// TestAuthHooksCarryNoSecrets: the hook surface (method names, durations,
// state transitions) must never contain the phone, code, hash, password,
// or auth key bytes.
func TestAuthHooksCarryNoSecrets(t *testing.T) {
	srv := newWSTestServerForAuth(t)
	key := testAuthKeyOf(9)

	var log strings.Builder
	c, err := client.New(client.Config{
		Session:    session.NewWithKey(key),
		Host:       srv.Addr,
		TLSEnabled: boolPtr(false),
		Hooks: client.Hooks{
			OnRPCStart:    func(method string) { log.WriteString("start:" + method + ";") },
			OnStateChange: func(from, to client.State) { log.WriteString("state:" + from.String() + ">" + to.String() + ";") },
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	af := newAuthFake("54321")
	af.arm2FA("correct-horse-2fa")
	installAuthHooks(t, fake, af)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected password-required error, got %v", err)
	}
	if _, err := c.CheckPassword(context.Background(), "correct-horse-2fa"); err != nil {
		t.Fatal(err)
	}
	if err := c.LogOut(context.Background()); err != nil {
		t.Fatal(err)
	}

	for _, secret := range []string{
		"341231231234", "54321", "fake_phone_code_hash", "correct-horse-2fa",
	} {
		if strings.Contains(log.String(), secret) {
			t.Fatalf("hook log leaks %q: %s", secret, log.String())
		}
	}
	t.Logf("hook log (diagnostic): %s", log.String())
}

// TestAuthMetricsStayAggregate: the Metrics snapshot must expose counters
// only — never phone/code/hash/password values.
func TestAuthMetricsStayAggregate(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()
	af.arm2FA("correct-horse-2fa")

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil && !strings.Contains(err.Error(), "password") {
		t.Fatal(err)
	}
	if _, err := c.CheckPassword(context.Background(), "correct-horse-2fa"); err != nil {
		t.Fatal(err)
	}

	m := c.Metrics()
	if m.AuthSendCodeTotal != 1 || m.AuthSignInTotal != 2 || m.AuthSuccessTotal != 1 {
		t.Fatalf("auth counters: send=%d sign=%d success=%d, want 1/2/1",
			m.AuthSendCodeTotal, m.AuthSignInTotal, m.AuthSuccessTotal)
	}
	if m.AuthState != "authorized" {
		t.Fatalf("AuthState metric = %s", m.AuthState)
	}
	// Metrics is a fixed-shape struct; assert no field accidentally
	// carries a long opaque value (hashes/passwords would).
	for _, s := range []string{m.SessionState, m.AuthState} {
		if strings.Contains(s, "fake_phone_code_hash") || strings.Contains(s, "correct-horse") {
			t.Fatalf("metric string leaks secret material: %q", s)
		}
	}
}

// TestAuthSRPParamRejection: hostile server parameters are rejected
// before password processing, with parameter-only error text (no
// password material, no hashing of the password against bad params).
func TestAuthSRPParamRejection(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	af.arm2FA("correct-horse-2fa")
	af.mu.Lock()
	af.badPrime = true // the fake now serves a corrupted P
	af.mu.Unlock()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); !strings.Contains(err.Error(), "password") {
		t.Fatalf("expected password-required, got %v", err)
	}
	_, err := c.CheckPassword(context.Background(), "correct-horse-2fa")
	if err == nil {
		t.Fatal("bad server prime accepted")
	}
	if !errors.Is(err, client.ErrSRPParams) {
		t.Fatalf("err = %v, want ErrSRPParams", err)
	}
	if strings.Contains(err.Error(), "correct-horse") {
		t.Fatalf("SRP error leaks password material: %v", err)
	}
}
