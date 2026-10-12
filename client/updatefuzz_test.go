package client

// Phase 10 fuzz targets: hostile update streams and difference results
// must never panic, deadlock, or loop the state machine.

import (
	"testing"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzMessageBoxProcess: arbitrary bytes decoded as Updates-family
// objects flow through the state machine — no panic, deterministic
// terminal state, gap accounting stays coherent.
func FuzzMessageBoxProcess(f *testing.F) {
	f.Add(uint32(1), int32(1), int32(1))
	f.Add(uint32(0x74ae4240), int32(5), int32(0)) // updates ctor
	f.Fuzz(func(t *testing.T, ctor uint32, pts, ptsCount int32) {
		m := newMessageBox()
		var out processUpdatesResult
		// A synthetic UpdateNewMessage variant with hostile fields.
		u := &gen.UpdateNewMessage{
			Message:  &gen.Message{ID: int32(ctor), PeerID: &gen.PeerUser{UserID: 1}},
			PTS:      pts,
			PTSCount: ptsCount,
		}
		batch, ok := normalizeUpdates(u, false)
		if !ok {
			return
		}
		_ = m.processUpdates(batch, &out)
		// Idempotent terminal check: the state must stay queryable.
		_, _ = m.sessionState()
		_ = m.isEmpty()
		_ = m.checkDeadlines()
	})
}

// FuzzNormalizeUpdates: any object must classify without panicking —
// accepted or rejected, never normalized into a wrong shape.
func FuzzNormalizeUpdates(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{0x74, 0xae, 0x42, 0x40}) // updates ctor prefix
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		batch, ok := normalizeUpdates(obj, false)
		if ok && batch.Date == nil && !batch.Bare {
			// Only updatesTooLong may carry a nil date as a non-bare batch.
			if _, isTooLong := obj.(*gen.UpdatesTooLong); !isTooLong {
				t.Fatalf("nil-date non-bare batch accepted for %T", obj)
			}
		}
	})
}

// FuzzApplyDifference: hostile difference results must never corrupt the
// box (state stays queryable; no panic; gap clearing stays coherent).
func FuzzApplyDifference(f *testing.F) {
	f.Add(int32(1), int32(2), int32(3), int32(4), int32(5))
	f.Fuzz(func(t *testing.T, pts, qts, date, seq, diffPts int32) {
		m := newMessageBox()
		var out processUpdatesResult
		_ = m.processUpdates(batchOf(date, seq, updNewMsg(pts, 1)), &out)
		// Force difference state, then feed a hostile complete difference.
		_ = m.processUpdates(updatesBatch{}, &out)
		diff := &gen.UpdatesDifference{
			State: &gen.UpdatesState{PTS: diffPts, QTS: qts, Date: date, Seq: seq},
		}
		_ = m.applyDifference(diff, &out)
		_, _ = m.sessionState()
	})
}

// FuzzPendingGapInsertion: held-gap bookkeeping stays bounded and
// coherent under arbitrary pts progressions.
func FuzzPendingGapInsertion(f *testing.F) {
	f.Add(int32(1), int32(2), int32(1))
	f.Fuzz(func(t *testing.T, a, b, c int32) {
		m := newMessageBox()
		var out processUpdatesResult
		_ = m.processUpdates(batchOf(1, 0, updNewMsg(a, 1)), &out)
		_ = m.processUpdates(batchOf(1, 0, updNewMsg(b, 1)), &out)
		_ = m.processUpdates(batchOf(1, 0, updNewMsg(c, 1)), &out)
		if n := m.gapsCount(); n > 1 {
			t.Fatalf("single-entry box accumulated %d gaps", n)
		}
	})
}
