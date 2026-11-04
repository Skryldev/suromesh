package session_test

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
)

func testKey(seed byte) *crypto.AuthKey {
	var b [256]byte
	for i := range b {
		b[i] = byte(int(i)*int(seed) + 1)
	}
	return crypto.NewAuthKey(b)
}

func TestFileSessionRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.bin")

	// Missing file → fresh session.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fs.AuthState() != session.StateNoKey {
		t.Fatalf("fresh state %s", fs.AuthState())
	}

	// Populate and save.
	if err := fs.SetDC(2, "im-server.splus.ir", 443); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthKey(testKey(3)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Save(); err != nil {
		t.Fatal(err)
	}

	// Reload → identical state.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	dc, addr, port := fs2.DC()
	if dc != 2 || addr != "im-server.splus.ir" || port != 443 {
		t.Fatalf("dc mismatch: %+v", fs2)
	}
	if fs2.AuthState() != session.StateAuthorized {
		t.Fatalf("state %s", fs2.AuthState())
	}
	k := fs2.AuthKey()
	if k == nil || k.KeyID != testKey(3).KeyID {
		t.Fatal("key mismatch after reload")
	}

	// Determinism: identical state produces identical bytes.
	b1, _ := os.ReadFile(path)
	if err := fs2.Save(); err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path)
	if string(b1) != string(b2) {
		t.Fatal("persistence is not deterministic")
	}
}

func TestFileSessionCorruption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.bin")
	fs, _ := session.OpenFile(path)
	_ = fs.SetDC(1, "a", 443)
	_ = fs.SetAuthKey(testKey(5))
	_ = fs.Save()
	good, _ := os.ReadFile(path)

	corrupt := func(mut func(b []byte) []byte) error {
		bad := mut(append([]byte{}, good...))
		if err := os.WriteFile(path, bad, 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := session.OpenFile(path)
		return err
	}

	// Truncation, byte flip, bad magic, bad version, bad key length.
	if err := corrupt(func(b []byte) []byte { return b[:len(b)/2] }); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("truncated: %v", err)
	}
	if err := corrupt(func(b []byte) []byte { b[30] ^= 0xFF; return b }); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("corrupted body: %v", err)
	}
	if err := corrupt(func(b []byte) []byte { b[0] = 'X'; return b }); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("bad magic: %v", err)
	}
	if err := corrupt(func(b []byte) []byte { b[12+9] = '9'; return b }); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("bad version: %v", err)
	}
	if err := corrupt(func(b []byte) []byte { b[13] = 0xEE; return b }); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("json length clobber: %v", err)
	}

	// After all corruption experiments, saving a valid session repairs the
	// file — corruption never poisons future writes.
	if err := fs.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := session.OpenFile(path); err != nil {
		t.Fatalf("valid save after corruption: %v", err)
	}
}

// TestFileSessionAtomicReplace simulates a crash mid-write: a stale temp
// file must never be picked up, and the real file must stay valid.
func TestFileSessionAtomicReplace(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.bin")
	fs, _ := session.OpenFile(path)
	_ = fs.SetAuthKey(testKey(7))
	_ = fs.Save()

	// Partial write artifacts.
	junk := filepath.Join(dir, ".suromesh-session-crashed")
	_ = os.WriteFile(junk, []byte("half written"), 0o600)

	// Reload still sees the old valid session; the temp artifact is ignored.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatalf("crash artifact poisoned load: %v", err)
	}
	if k := fs2.AuthKey(); k == nil || k.KeyID != testKey(7).KeyID {
		t.Fatal("valid session damaged")
	}
	_ = os.Remove(junk)
}

func TestFileSessionConcurrentSaveLoad(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "s.bin")
	fs, _ := session.OpenFile(path)
	_ = fs.SetAuthKey(testKey(9))

	var wg sync.WaitGroup
	errs := make(chan error, 200)
	for i := 0; i < 100; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); errs <- fs.Save() }()
		go func() {
			defer wg.Done()
			_, err := session.OpenFile(path)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent access error: %v", err)
		}
	}
}

func TestSessionLifecycleGuards(t *testing.T) {
	s := session.New()
	// Authorized without a key is illegal.
	if err := s.SetAuthorized(); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("authorized without key: %v", err)
	}
	// Key install moves NoKey → KeyExchanged.
	if err := s.SetAuthKey(testKey(2)); err != nil {
		t.Fatal(err)
	}
	if s.AuthState() != session.StateKeyExchanged {
		t.Fatalf("state %s", s.AuthState())
	}
	if err := s.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	// Key rotation while authorized is rejected.
	if err := s.SetAuthKey(testKey(3)); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("rotation while authorized: %v", err)
	}
	// Clear resets everything.
	if err := s.ClearAuthKey(); err != nil {
		t.Fatal(err)
	}
	if s.AuthState() != session.StateNoKey {
		t.Fatalf("post-clear state %s", s.AuthState())
	}
	// Close zeroes the key material and rejects further use.
	_ = s.SetAuthKey(testKey(4))
	_ = s.Close()
	if k := s.AuthKey(); k != nil && k.HasValue && k.Key[0] != 0 {
		t.Fatal("key not zeroed on close")
	}
	if err := s.SetDC(1, "x", 1); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("use after close: %v", err)
	}
}

func TestStringSessionInterop(t *testing.T) {
	// Go session ↔ StringSession (the Python-compatible format from Phase 2).
	dir := t.TempDir()
	fs, _ := session.OpenFile(filepath.Join(dir, "s.bin"))
	_ = fs.SetDC(3, "im-server.splus.ir", 443)
	_ = fs.SetAuthKey(testKey(6))

	// Round trip through the compact form.
	ss, err := session.ParseStringSession(exportStringSession(t, fs))
	if err != nil {
		t.Fatal(err)
	}
	if ss.DCID != 3 || ss.ServerAddr != "im-server.splus.ir" || ss.Port != 443 {
		t.Fatalf("fields: %+v", ss)
	}
	if ss.AuthKeyValue != fs.AuthKey().Key {
		t.Fatal("key mismatch through string session")
	}
}

// exportStringSession adapts a Session to the compact format (production
// wiring will live in the client; here we verify the pieces compose).
func exportStringSession(t *testing.T, fs *session.FileSession) string {
	t.Helper()
	dc, addr, port := fs.DC()
	k := fs.AuthKey()
	if k == nil {
		return ""
	}
	ss := session.StringSession{
		DCID:         dc,
		ServerAddr:   addr,
		Port:         port,
		AuthKeyValue: k.Key,
		HasAuthKey:   true,
	}
	return ss.String()
}
