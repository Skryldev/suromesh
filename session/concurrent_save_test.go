package session

// Regression test for the Phase 9 finding: FileSession.Save read the
// embedded Session's authState/authKey/DC under fs.mu while mutators
// wrote them under the Session's own mutex — a data race. The fix routes
// Save through Session.snapshot (one RLock hold). Run under -race.

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/crypto"
)

func TestFileSessionConcurrentMutateAndSave(t *testing.T) {
	dir := t.TempDir()
	fs, err := OpenFile(filepath.Join(dir, "race.session"))
	if err != nil {
		t.Fatal(err)
	}
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 1)
	}
	if err := fs.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for r := 0; r < 50; r++ {
				switch i % 4 {
				case 0:
					_ = fs.SetAuthorized()
				case 1:
					fs.SetUnauthorized()
				case 2:
					_ = fs.SetDC(1, "h", 443)
				default:
					_ = fs.Save()
				}
			}
		}(i)
	}
	wg.Wait()

	// The file must always be loadable and consistent with the final state.
	final := fs.AuthState()
	rl, err := OpenFile(filepath.Join(dir, "race.session"))
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	defer rl.Close()
	if rl.AuthState() != final {
		t.Fatalf("persisted state %v != in-memory %v", rl.AuthState(), final)
	}
}
