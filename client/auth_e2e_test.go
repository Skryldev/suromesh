package client_test

// Phase 9 end-to-end authentication flows over the real stack
// (transport → crypto → RPC → client) against the fake Soroush server.
//
// Every scenario maps to the reference flows documented in docs/auth.md;
// error sentinels carry provenance comments to the Python reference lines.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

const testPhone = "+34 (123) 123-1234" // normalization exercised on every use

// newWSTestServerForAuth creates a wstest server closed via t.Cleanup.
func newWSTestServerForAuth(t testing.TB) *wstest.Server {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	return srv
}

// newAuthClientWithSession wires a ready client around an explicit
// FileSession (fresh or reloaded), reusing the persisted key when present
// and wiring the file as the session Saver.
func newAuthClientWithSession(t testing.TB, srv *wstest.Server, fs *session.FileSession) (*client.Client, *appFake) {
	t.Helper()
	key := fs.AuthKey()
	if key == nil || !key.HasValue {
		key = testAuthKeyOf(9)
		if err := fs.SetAuthKey(key); err != nil {
			t.Fatal(err)
		}
	}
	c, err := client.New(client.Config{
		Session:    fs.Session,
		Saver:      fs,
		Host:       srv.Addr,
		TLSEnabled: boolPtr(false),
	})
	if err != nil {
		t.Fatal(err)
	}
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	return c, fake
}

func TestAuthSendCodeSignInFlow(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("initial auth state = %s, want unauthorized", got)
	}

	sent, err := c.SendCode(context.Background(), testPhone)
	if err != nil {
		t.Fatal(err)
	}
	if sent.PhoneCodeHash == "" || sent.Type != "sms" || sent.TimeoutSecs != 90 {
		t.Fatalf("CodeSent = %+v", sent)
	}
	if got := c.AuthState(); got != client.AuthCodePending {
		t.Fatalf("after SendCode: %s, want code_pending", got)
	}
	// Normalization: the fake saw the parsed form (reference utils.py:916).
	af.mu.Lock()
	saw := af.lastSignedPhone
	af.mu.Unlock()
	if saw != "341231231234" {
		t.Fatalf("fake saw phone %q, want normalized 341231231234", saw)
	}

	// Wrong code: PHONE_CODE_INVALID is retryable; state stays CodePending
	// (reference sign_in raises, state untouched).
	if _, err := c.SignIn(context.Background(), testPhone, "00000"); !errors.Is(err, client.ErrCodeInvalid) {
		t.Fatalf("wrong code err = %v, want ErrCodeInvalid", err)
	}
	if got := c.AuthState(); got != client.AuthCodePending {
		t.Fatalf("after invalid code: %s, want code_pending", got)
	}

	// Correct code: authorization, session authorized, entity seeded.
	authz, err := c.SignIn(context.Background(), testPhone, "54321")
	if err != nil {
		t.Fatal(err)
	}
	if authz.User == nil || authz.User.ID != 7001 {
		t.Fatalf("Authorization.User = %+v", authz.User)
	}
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("after SignIn: %s, want authorized", got)
	}
	m := c.Metrics()
	if m.AuthSuccessTotal != 1 || m.AuthSignInTotal != 2 {
		t.Fatalf("metrics: success=%d signin=%d, want 1/2", m.AuthSuccessTotal, m.AuthSignInTotal)
	}

	// The wire input peer resolves from the seeded self user (access hash
	// came from the server's Authorization.User — never fabricated).
	if _, err := c.InputPeerFor(context.Background(), client.PeerID{Kind: client.PeerUser, ID: 7001}); err != nil {
		t.Fatalf("seeded self user unresolvable: %v", err)
	}
}

func TestAuthSignInWithoutSendCode(t *testing.T) {
	c, _, _ := newAuthClient(t)
	defer c.Close()

	// auth.py:280-281 — no hash stored → ValueError equivalent.
	_, err := c.SignIn(context.Background(), testPhone, "54321")
	if !errors.Is(err, client.ErrPhoneCodeHashMissing) {
		t.Fatalf("err = %v, want ErrPhoneCodeHashMissing", err)
	}
	// Invalid phone shapes (utils.py:916-923).
	for _, bad := range []string{"", "  ", "12a34", "abc", "+34_9"} {
		if _, err := c.SendCode(context.Background(), bad); !errors.Is(err, client.ErrPhoneInvalid) {
			t.Fatalf("SendCode(%q) err = %v, want ErrPhoneInvalid", bad, err)
		}
	}
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state = %s, want unauthorized", got)
	}
}

func TestAuthSendCodeResendPath(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	// First send stores a hash; second send must take the resendCode path
	// (reference auth.py:527-549).
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	sent2, err := c.SendCode(context.Background(), testPhone)
	if err != nil {
		t.Fatal(err)
	}
	if sent2.Type != "app" { // the fake's resendCode answers sentCodeTypeApp
		t.Fatalf("second send type = %s, want the resend path (app)", sent2.Type)
	}
	af.mu.Lock()
	sends, signIns := af.sendCodeCalls, af.signInCalls
	af.mu.Unlock()
	if sends != 1 || signIns != 0 {
		t.Fatalf("sendCodeCalls=%d signInCalls=%d, want 1/0 (resend used)", sends, signIns)
	}
}

func TestAuthSendCodeRestartRetry(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	// auth.py:510-514: AUTH_RESTART is retried (≤3 attempts total).
	af.mu.Lock()
	af.sendCodeRestarts = 2
	af.mu.Unlock()

	sent, err := c.SendCode(context.Background(), testPhone)
	if err != nil {
		t.Fatal(err)
	}
	if sent.PhoneCodeHash == "" {
		t.Fatal("no hash after restart retries")
	}
	af.mu.Lock()
	calls := af.sendCodeCalls
	af.mu.Unlock()
	if calls != 3 {
		t.Fatalf("sendCodeCalls = %d, want 3 (2 restarts + success)", calls)
	}
}

func TestAuthSendCodeFloodWait(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	af.mu.Lock()
	af.sendCodeErr = &rpc.RPCError{Code: 420, Message: "FLOOD_WAIT_30"}
	af.mu.Unlock()

	_, err := c.SendCode(context.Background(), testPhone)
	fw, ok := client.AsFloodWait(err)
	if !ok || fw.Seconds != 30 {
		t.Fatalf("err = %v, want FloodWaitError(30)", err)
	}
	m := c.Metrics()
	if m.AuthFloodWaitTotal != 1 {
		t.Fatalf("AuthFloodWaitTotal = %d, want 1", m.AuthFloodWaitTotal)
	}
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state after flood = %s, want unauthorized", got)
	}
}

func TestAuthExpiredCode(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	// Fake: the stored hash no longer matches → PHONE_CODE_EXPIRED.
	af.mu.Lock()
	af.codeHash = "rotated_hash"
	af.mu.Unlock()

	_, err := c.SignIn(context.Background(), testPhone, "54321")
	if !errors.Is(err, client.ErrCodeExpired) {
		t.Fatalf("err = %v, want ErrCodeExpired", err)
	}
	// auth.py:390-392: the stored hash must be dropped → the flow resets.
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state after expiry = %s, want unauthorized (hash dropped)", got)
	}
	// A subsequent SendCode issues a fresh hash and restores the flow.
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if got := c.AuthState(); got != client.AuthCodePending {
		t.Fatalf("state after re-send = %s, want code_pending", got)
	}
}

func TestAuthSignUpRequired(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	af.mu.Lock()
	af.signUpRequired = true
	af.mu.Unlock()

	// auth.py:428-431: AuthorizationSignUpRequired → PhoneNumberUnoccupied.
	_, err := c.SignIn(context.Background(), testPhone, "54321")
	if !errors.Is(err, client.ErrPhoneUnoccupied) {
		t.Fatalf("err = %v, want ErrPhoneUnoccupied", err)
	}
	if got := c.AuthState(); got != client.AuthCodePending {
		t.Fatalf("state after sign-up-required = %s, want code_pending", got)
	}
}

func TestAuthPhoneNumberBanned(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	af.mu.Lock()
	af.signInErr = &rpc.RPCError{Code: 400, Message: "PHONE_NUMBER_BANNED"}
	af.mu.Unlock()

	_, err := c.SignIn(context.Background(), testPhone, "54321")
	if !errors.Is(err, client.ErrPhoneBanned) {
		t.Fatalf("err = %v, want ErrPhoneBanned", err)
	}
}

func TestAuth2FAFlow(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	af.arm2FA("correct horse battery staple") // test double, not a secret

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}

	// SESSION_PASSWORD_NEEDED → ErrPasswordRequired + state transition
	// (reference _start two_step_detected, auth.py:205-207).
	_, err := c.SignIn(context.Background(), testPhone, "54321")
	if !errors.Is(err, client.ErrPasswordRequired) {
		t.Fatalf("err = %v, want ErrPasswordRequired", err)
	}
	if got := c.AuthState(); got != client.AuthPasswordRequired {
		t.Fatalf("state = %s, want password_required", got)
	}

	// Wrong password: PASSWORD_HASH_INVALID is retryable, state unchanged.
	if _, err := c.CheckPassword(context.Background(), "wrong-password"); !errors.Is(err, client.ErrPasswordInvalid) {
		t.Fatalf("err = %v, want ErrPasswordInvalid", err)
	}
	if got := c.AuthState(); got != client.AuthPasswordRequired {
		t.Fatalf("state after bad password = %s, want password_required", got)
	}
	// The fake's verifier must have rejected something well-formed (the
	// client produced a full SRP proof against the armed parameters).
	af.mu.Lock()
	pwCalls := af.checkPWCalls
	af.mu.Unlock()
	if pwCalls != 1 {
		t.Fatalf("checkPassword RPCs = %d, want 1", pwCalls)
	}

	// Correct password: the SRP proof is validated by the independent
	// server-side verifier, then authorization completes.
	authz, err := c.CheckPassword(context.Background(), "correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if authz.User == nil || authz.User.ID != 7001 {
		t.Fatalf("Authorization.User = %+v", authz.User)
	}
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("state = %s, want authorized", got)
	}
}

func TestAuth2FAProofActuallyValidated(t *testing.T) {
	// Guard against a fake that would accept anything: corrupt the proof
	// by arming the server with a DIFFERENT password than the client uses.
	c, _, af := newAuthClient(t)
	defer c.Close()

	af.arm2FA("server-side password")
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); !errors.Is(err, client.ErrPasswordRequired) {
		t.Fatalf("err = %v, want ErrPasswordRequired", err)
	}
	if _, err := c.CheckPassword(context.Background(), "client-side password"); !errors.Is(err, client.ErrPasswordInvalid) {
		t.Fatalf("mismatched passwords accepted: %v", err)
	}
	// The client's (A, M1) reached the verifier.
	af.mu.Lock()
	gotA, gotM1 := len(af.lastA) > 0, len(af.lastM1) > 0
	af.mu.Unlock()
	if !gotA || !gotM1 {
		t.Fatal("verifier never received the SRP proof")
	}
}

func TestAuthSessionRestartLive(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.session")
	srv := newWSTestServerForAuth(t)

	// Process 1: connect, authorize, close.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c1, fake1 := newAuthClientWithSession(t, srv, fs)
	af1 := newAuthFake("54321")
	installAuthHooks(t, fake1, af1)
	if _, err := c1.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	// Process 2: fresh client, same session file — must already be
	// authorized WITHOUT repeating authentication (no signIn RPCs).
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer fs2.Close()
	c2, fake2 := newAuthClientWithSession(t, srv, fs2)
	defer c2.Close()
	af2 := newAuthFake("54321") // would reject any auth attempt
	installAuthHooks(t, fake2, af2)

	if got := c2.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("restarted state = %s, want authorized (session reuse)", got)
	}
	af2.mu.Lock()
	signIns := af2.signInCalls
	af2.mu.Unlock()
	if signIns != 0 {
		t.Fatalf("restart performed %d signIn RPCs, want 0", signIns)
	}
}

func TestAuthLogoutPersistence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "logout.session")
	srv := newWSTestServerForAuth(t)

	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	c1, fake1 := newAuthClientWithSession(t, srv, fs)
	af := newAuthFake("54321")
	installAuthHooks(t, fake1, af)
	if _, err := c1.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c1.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	if got := c1.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("state = %s, want authorized", got)
	}

	// Logout (reference auth.py:593-620): after it AuthState != Authorized
	// and nothing reusable persists.
	if err := c1.LogOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := c1.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state after logout = %s, want unauthorized", got)
	}
	if c1.CachedEntities() != 0 {
		t.Fatalf("entity cache not cleared on logout (%d entries)", c1.CachedEntities())
	}
	if err := c1.Close(); err != nil {
		t.Fatal(err)
	}

	// The persisted file must not carry the authorization (or the key).
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatalf("reload after logout: %v", err)
	}
	defer fs2.Close()
	if k := fs2.AuthKey(); k != nil && k.HasValue {
		t.Fatal("session file still holds an auth key after logout")
	}
	if fs2.AuthState() == session.StateAuthorized {
		t.Fatal("session file still marked authorized after logout")
	}
}

func TestAuthLogoutServerErrorLeavesState(t *testing.T) {
	c, _, af := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	// auth.py:610-612: RPCError → return False, NO local mutation.
	af.mu.Lock()
	af.logoutErr = &rpc.RPCError{Code: 500, Message: "INTERNAL"}
	af.mu.Unlock()

	if err := c.LogOut(context.Background()); err == nil {
		t.Fatal("logout succeeded despite server error")
	}
	if got := c.AuthState(); got != client.AuthAuthorized {
		t.Fatalf("state after failed logout = %s, want authorized (untouched)", got)
	}
}

// make401Responder answers every request with a 401-class failure.
func make401Responder(msg string) responderFunc {
	return func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		return nil, &rpc.RPCError{Code: 401, Message: msg}
	}
}

func TestAuthRevoked(t *testing.T) {
	c, fake, _ := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	// The server terminates the session permanently; ANY request now
	// answers 401 SESSION_REVOKED (reference UnauthorizedError semantics).
	fake.SetResponder(make401Responder("SESSION_REVOKED"))

	_, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: 1})
	if err == nil {
		t.Fatal("revoked session answered a request")
	}
	if got := c.AuthState(); got != client.AuthAuthorizationRevoked {
		t.Fatalf("state = %s, want authorization_revoked", got)
	}
}

func TestAuthKeyUnregisteredIsNotRevocation(t *testing.T) {
	c, fake, _ := newAuthClient(t)
	defer c.Close()

	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); err != nil {
		t.Fatal(err)
	}
	// AUTH_KEY_UNREGISTERED: recoverable — re-authentication with the same
	// key is possible; the client is unauthorized, not revoked.
	fake.SetResponder(make401Responder("AUTH_KEY_UNREGISTERED"))
	if _, err := c.Invoke(context.Background(), &gen.PingRequest{PingID: 2}); err == nil {
		t.Fatal("unregistered key answered a request")
	}
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state = %s, want unauthorized", got)
	}
}

func TestAuthCancellation(t *testing.T) {
	c, fake, _ := newAuthClient(t)
	defer c.Close()

	// Slow the fake so cancellation lands mid-RPC.
	fake.SetDelay(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.SendCode(ctx, testPhone)
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
	if got := c.AuthState(); got != client.AuthUnauthorized {
		t.Fatalf("state after cancel = %s, want unauthorized", got)
	}

	// Close during SendCode: same deterministic outcome via lifecycle ctx.
	fake.SetDelay(200 * time.Millisecond)
	done := make(chan error, 1)
	go func() {
		_, err := c.SendCode(context.Background(), testPhone)
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("SendCode succeeded after Close")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("SendCode did not settle after Close")
	}
}

func TestAuthStateTransitionTable(t *testing.T) {
	// The legality table from docs/auth.md, verified through the fake
	// server (no direct state pokes).
	c, _, af := newAuthClient(t)
	defer c.Close()

	expect := func(want client.AuthState, phase string) {
		t.Helper()
		if got := c.AuthState(); got != want {
			t.Fatalf("%s: state = %s, want %s", phase, got, want)
		}
	}
	expect(client.AuthUnauthorized, "fresh")
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	expect(client.AuthCodePending, "after sendCode")
	if _, err := c.SignIn(context.Background(), testPhone, "99999"); !errors.Is(err, client.ErrCodeInvalid) {
		t.Fatalf("invalid code: %v", err)
	}
	expect(client.AuthCodePending, "invalid code keeps pending")
	af.arm2FA("pw")
	if _, err := c.SignIn(context.Background(), testPhone, "54321"); !errors.Is(err, client.ErrPasswordRequired) {
		t.Fatalf("2fa: %v", err)
	}
	expect(client.AuthPasswordRequired, "password required")
	if _, err := c.CheckPassword(context.Background(), "pw"); err != nil {
		t.Fatal(err)
	}
	expect(client.AuthAuthorized, "authorized")
	if err := c.LogOut(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(client.AuthUnauthorized, "logged out")
}

func TestAuthErrorStringsCarryNoSecrets(t *testing.T) {
	// Security requirement: errors must not leak phone, code, or hash.
	c, _, af := newAuthClient(t)
	defer c.Close()
	af.arm2FA("correct-horse")

	secrets := []string{"341231231234", "54321", "fake_phone_code_hash", "correct-horse"}

	check := func(err error, what string) {
		t.Helper()
		if err == nil {
			t.Fatalf("%s: expected error", what)
		}
		for _, s := range secrets {
			if strings.Contains(err.Error(), s) {
				t.Fatalf("%s error leaks %q: %s", what, s, err.Error())
			}
		}
	}

	_, err := c.SignIn(context.Background(), testPhone, "wrong")
	check(err, "wrong code")
	_, err = c.SendCode(context.Background(), "not a phone!")
	check(err, "bad phone")
	if _, err := c.SendCode(context.Background(), testPhone); err != nil {
		t.Fatal(err)
	}
	_, err = c.SignIn(context.Background(), testPhone, "54321") // password needed
	check(err, "password required")
	_, err = c.CheckPassword(context.Background(), "correct-horse-typo")
	check(err, "wrong password")
}
