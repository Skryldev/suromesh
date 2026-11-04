package session_test

import (
	"strings"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
)

func keyOf(seed byte) *crypto.AuthKey {
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(int(i)*int(seed) + 1)
	}
	return crypto.NewAuthKey(kb)
}

func hexString(b []byte) string {
	const d = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, v := range b {
		out[i*2] = d[v>>4]
		out[i*2+1] = d[v&15]
	}
	return string(out)
}

// TestRedactedNeverContainsKeyMaterial guards the logging surface: any
// human-readable rendering of session state must never contain raw key
// bytes or a long hex run of key material. Key IDs (short, derivable from
// public traffic) are the intended identifier.
func TestRedactedNeverContainsKeyMaterial(t *testing.T) {
	k := keyOf(1)
	s := session.NewWithKey(k)
	_ = s.SetDC(1, "im-server.splus.ir", 443)

	rendered := s.Redacted()
	raw := k.Key[:]
	fullHex := hexString(raw)

	if strings.Contains(rendered, fullHex) {
		t.Fatal("Redacted leaks the full hex key")
	}
	// No 32-byte hex run of actual key material may appear anywhere.
	for off := 0; off+32 <= len(raw); off += 32 {
		if strings.Contains(rendered, hexString(raw[off:off+32])) {
			t.Fatalf("Redacted leaks key bytes at offset %d: %s", off, rendered)
		}
	}
	if !strings.Contains(rendered, "key(id=") {
		t.Fatalf("Redacted lost the non-secret key id: %s", rendered)
	}
}

// TestErrorMessagesNeverContainKeys pins error-path hygiene.
func TestErrorMessagesNeverContainKeys(t *testing.T) {
	k := keyOf(2)
	fullHex := hexString(k.Key[:])

	s := session.NewWithKey(k)
	_ = s.SetDC(1, "x", 1)
	_ = s.Close()

	check := func(err error) {
		t.Helper()
		if err == nil {
			return
		}
		msg := err.Error()
		if strings.Contains(msg, fullHex) {
			t.Fatalf("error leaks full key hex: %s", msg)
		}
	}
	check(s.SetDC(1, "x", 1))
	check(s.SetAuthKey(k))
	check(s.SetAuthorized())
	check(s.ClearAuthKey())
	check(s.Close())
}
