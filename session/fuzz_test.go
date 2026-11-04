package session_test

import (
	"os"
	"testing"

	"github.com/Skryldev/suromesh/session"
)

// FuzzStringSessionDecode feeds hostile strings through the compact-form
// decoder. Malformed inputs are errors, never panics, never state.
func FuzzStringSessionDecode(f *testing.F) {
	f.Add("")
	f.Add("1")
	f.Add("2AAAA")
	f.Add("1AAAA0000")
	f.Add("1AgABQQBQAAAAAAAAAAAAAAAAAAAA")
	f.Fuzz(func(t *testing.T, s string) {
		if len(s) > 4096 {
			return
		}
		ss, err := session.ParseStringSession(s)
		if err != nil {
			return
		}
		// Parse-serialize idempotence where a key exists; keyless
		// sessions serialize to "" (reference save() semantics).
		if !ss.HasAuthKey {
			if ss.String() != "" {
				t.Fatal("keyless session must serialize to empty")
			}
			return
		}
		re, err := session.ParseStringSession(ss.String())
		if err != nil {
			t.Fatalf("re-parse failed: %v", err)
		}
		if re.String() != ss.String() {
			t.Fatal("re-serialization drift")
		}
	})
}

// FuzzFileSessionDecode feeds hostile bytes through the versioned file
// decoder. Corruption is ErrCorrupt, never a panic.
func FuzzFileSessionDecode(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte("SUROMESH\x0a\x00\x00\x00{}\x00\x00\x00\x00\x00\x00\x00\x00"))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 8192 {
			return
		}
		dir := t.TempDir()
		path := dir + "/fuzz.bin"
		if err := writeFile(path, b); err != nil {
			t.Skip()
		}
		fs, err := session.OpenFile(path)
		if err != nil {
			return // corruption detected: fine
		}
		// Whatever loaded must re-save deterministically and reload.
		if err := fs.Save(); err != nil {
			t.Fatalf("save after load: %v", err)
		}
		if _, err := session.OpenFile(path); err != nil {
			t.Fatalf("reload after save: %v", err)
		}
	})
}

func writeFile(path string, b []byte) error {
	return osWriteFile(path, b)
}

func osWriteFile(path string, b []byte) error {
	return os.WriteFile(path, b, 0o600)
}
