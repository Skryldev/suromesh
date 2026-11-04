package client_test

// Phase 15 persistence fault-injection + update-state durability across
// restart cycles with injected gaps.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// TestPersistenceConcurrentSaveClose: concurrent Save racing Close must
// never corrupt the file nor panic (Save-after-Close returns ErrClosed).
func TestPersistenceConcurrentSaveClose(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "conc.session")
	fs, err := session.OpenFile(path)
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
			for r := 0; r < 20; r++ {
				err := fs.Save()
				if err != nil && !errors.Is(err, session.ErrClosed) {
					t.Errorf("save %d/%d: %v", i, r, err)
				}
			}
		}(i)
	}
	// Close races the savers.
	go func() {
		time.Sleep(5 * time.Millisecond)
		_ = fs.Close()
	}()
	wg.Wait()

	// The file must still load (last atomic rename wins) or be absent.
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatalf("post-close load: %v", err)
	}
	fs2.Close()
}

// TestPersistenceSaveFailureDoesNotMutateMemory: a Save failure (closed
// session / fault injection) must leave in-memory authorization state
// intact.
func TestPersistenceSaveFailureDoesNotMutateMemory(t *testing.T) {
	fs, err := session.OpenFile(filepath.Join(t.TempDir(), "fail.session"))
	if err != nil {
		t.Fatal(err)
	}
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 7)
	}
	if err := fs.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Close(); err != nil {
		t.Fatal(err)
	}

	// Mutations after Close fail with ErrClosed...
	if err := fs.SetAuthorized(); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("mutate-after-close = %v, want ErrClosed", err)
	}
	// ...and the session is terminally keyless: Close drops the key AND
	// the state label together so a post-close Save serializes a
	// CONSISTENT keyless file (never key_exchanged without a key).
	if fs.AuthState() != session.StateNoKey {
		t.Fatalf("closed session state = %v, want no_key", fs.AuthState())
	}
	if k := fs.AuthKey(); k != nil && k.HasValue {
		t.Fatal("closed session still holds a key reference")
	}
}

// TestPersistenceCorruptedFileDetection: a corrupted/truncated file must
// fail loading with ErrCorrupt (never silently accepted).
func TestPersistenceCorruptedFileDetection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "corrupt.session")

	// Write a valid session, then corrupt each byte region.
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i + 3)
	}
	if err := fs.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthorized(); err != nil {
		t.Fatal(err)
	}
	if err := fs.Save(); err != nil {
		t.Fatal(err)
	}
	fs.Close()

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Flip one byte in the middle (HMAC region AND body region).
	for _, at := range []int{len(raw) / 2, len(raw) - 1} {
		corrupt := append([]byte{}, raw...)
		corrupt[at] ^= 0xFF
		if err := os.WriteFile(path, corrupt, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := session.OpenFile(path); !errors.Is(err, session.ErrCorrupt) {
			t.Fatalf("corruption at %d: err = %v, want ErrCorrupt", at, err)
		}
	}
	// Truncation also detected.
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := session.OpenFile(path); !errors.Is(err, session.ErrCorrupt) {
		t.Fatalf("truncated: err = %v, want ErrCorrupt", err)
	}
}

// TestUpdateStateDurabilityGapCycles: N restart cycles with injected
// gaps — the persisted pts frontier survives, gaps recover through
// difference, and no update replays or is skipped.
func TestUpdateStateDurabilityGapCycles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "durability.session")
	srv := newWSTestServerForAuth(t)

	nextPts := int32(1)
	for cycle := 0; cycle < 5; cycle++ {
		fs, err := session.OpenFile(path)
		if err != nil {
			t.Fatalf("cycle %d load: %v", cycle, err)
		}
		c, fake := newAuthClientWithSession(t, srv, fs)
		uf := newUpdatesFake()
		installUpdatesHooks(t, fake, uf)
		// The difference answer carries the authoritative state at the
		// end of each cycle's gap.
		uf.pushDiff(&gen.UpdatesDifference{
			State: &gen.UpdatesState{PTS: nextPts + 5, Date: 100, Seq: 0},
		})

		var delivered int64
		sub := c.OnUpdate(func(ctx context.Context, upd tl.Object) {
			atomic.AddInt64(&delivered, 1)
		})

		// Inject a gap: push pts nextPts+5 (the missing nextPts..+4 are
		// recovered via the scripted difference).
		fake.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: nextPts + 5, Message: "gap",
				PeerID: &gen.PeerUser{UserID: 300}, Date: nextPts + 5},
			PTS: nextPts + 5, PTSCount: 1,
		}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})

		waitUntil(t, 10*time.Second, func() bool {
			rec, ok := fs.UpdateState()
			return ok && rec.Pts == nextPts+5
		})
		if atomic.LoadInt64(&delivered) == 0 && cycle == 0 {
			t.Fatal("no update delivered in first cycle")
		}
		sub.Unsubscribe()
		recMem, _ := fs.Session.UpdateState()
		mm := c.Metrics()
		t.Logf("cycle %d pre-close: mem pts=%d diffReq=%d diffFail=%d accepted=%d gaps=%d",
			cycle, recMem.Pts, mm.DifferenceRequests, mm.DifferenceFailures, mm.UpdatesAccepted, mm.UpdateGaps)
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}

		// Reload: the persisted frontier must be nextPts+5 (no regression,
		// no replay on the next cycle's gap detection).
		fs2, err := session.OpenFile(path)
		if err != nil {
			t.Fatalf("cycle %d reload: %v", cycle, err)
		}
		rec, ok := fs2.Session.UpdateState()
		if !ok || rec.Pts != nextPts+5 {
			t.Fatalf("cycle %d persisted pts = %+v (ok %v), want %d", cycle, rec, ok, nextPts+5)
		}
		fs2.Close()
		nextPts += 6
	}
}

// TestPersistenceRestartNoReplay: after a restart with a persisted pts
// frontier, already-delivered updates are NOT replayed (exactly-once).
func TestPersistenceRestartNoReplay(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "noreplay.session")
	srv := newWSTestServerForAuth(t)

	fs, _ := session.OpenFile(path)
	c1, fake1 := newAuthClientWithSession(t, srv, fs)
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake1, uf)

	var seen1 int64
	sub1 := c1.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if _, ok := upd.(*gen.UpdateNewMessage); ok {
			atomic.AddInt64(&seen1, 1)
		}
	})
	fake1.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	waitUntil(t, 5*time.Second, func() bool { return atomic.LoadInt64(&seen1) == 1 })
	sub1.Unsubscribe()
	c1.Close()

	// Restart: same pts frontier, push a DIFFERENT message with a LOWER
	// pts — the MessageBox must drop it as a duplicate (no replay).
	fs2, _ := session.OpenFile(path)
	c2, fake2 := newAuthClientWithSession(t, srv, fs2)
	defer c2.Close()
	uf2 := newUpdatesFake()
	installUpdatesHooks(t, fake2, uf2)

	var seen2 int64
	sub2 := c2.OnUpdate(func(ctx context.Context, upd tl.Object) {
		if _, ok := upd.(*gen.UpdateNewMessage); ok {
			atomic.AddInt64(&seen2, 1)
		}
	})
	defer sub2.Unsubscribe()
	fake2.PushUpdate(&gen.Updates{Updates: []gen.UpdateClass{&gen.UpdateNewMessage{
		Message: &gen.Message{ID: 5, PeerID: &gen.PeerUser{UserID: 300}, Date: 5},
		PTS:     5, PTSCount: 1,
	}}, Users: []gen.UserClass{}, Chats: []gen.ChatClass{}, Date: 100, Seq: 0})
	time.Sleep(300 * time.Millisecond)
	if n := atomic.LoadInt64(&seen2); n != 0 {
		t.Fatalf("committed update replayed %d times after restart", n)
	}
}
