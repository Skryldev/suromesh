package client

// Phase 10 MessageBox unit tests: the state machine exercised directly
// with an injected clock. Every test cites its reference behavior in
// docs/updates.md's trace table.

import (
	"errors"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// mbClock is a controllable clock for deadline-sensitive tests.
type mbClock struct{ t time.Time }

func (c *mbClock) Now() time.Time { return c.t }
func (c *mbClock) Advance(d time.Duration) {
	c.t = c.t.Add(d)
}

func newTestBox() (*messageBox, *mbClock) {
	c := &mbClock{t: time.Unix(1700000000, 0)}
	m := newMessageBox()
	m.now = c.Now
	return m, c
}

func updNewMsg(pts, count int32) *gen.UpdateNewMessage {
	return &gen.UpdateNewMessage{
		Message:  &gen.Message{ID: int32(pts), Message: "m", PeerID: &gen.PeerUser{UserID: 1}},
		PTS:      pts,
		PTSCount: count,
	}
}

func updChannelMsg(ch int64, pts, count int32) *gen.UpdateNewChannelMessage {
	return &gen.UpdateNewChannelMessage{
		Message:  &gen.Message{ID: int32(pts), Message: "cm", PeerID: &gen.PeerChannel{ChannelID: ch}},
		PTS:      pts,
		PTSCount: count,
	}
}

func batchOf(date, seq int32, ups ...tl.Object) updatesBatch {
	d, s := date, seq
	return updatesBatch{Date: &d, Seq: &s, SeqStart: &s, Updates: ups}
}

// TestMBFirstUpdateInitializesEntry: an unknown entry is created with
// pts = pts - (0 if count else 1), or 1 if that would be non-positive
// (messagebox.py:591-603).
func TestMBFirstUpdateInitializesEntry(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 1, updNewMsg(5, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 1 {
		t.Fatalf("emitted %d, want 1", len(out.Emit))
	}
	ss, _ := m.sessionState()
	if ss.Pts != 5 {
		t.Fatalf("pts = %d, want 5 (applied pts)", ss.Pts)
	}

	// Zero-count first update initializes one lower.
	m2, _ := newTestBox()
	var out2 processUpdatesResult
	if err := m2.processUpdates(batchOf(100, 1, updChannelMsg(9, 7, 0)), &out2); err != nil {
		t.Fatal(err)
	}
	ss2, _ := m2.sessionState()
	_ = ss2 // account state unchanged; the channel entry holds pts-1
	m2.mu.Lock()
	entry := m2.entries[mbEntry(9)]
	m2.mu.Unlock()
	if entry == nil || entry.pts != 6 {
		t.Fatalf("channel entry = %+v, want pts 6 (pts-1 for count-0)", entry)
	}
}

// TestMBContiguousAndDuplicate: local+count == pts applies; local+count >
// pts is a duplicate and is ignored (messagebox.py:552-558).
func TestMBContiguousAndDuplicate(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 1, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 1 {
		t.Fatalf("first emit = %d", len(out.Emit))
	}
	// Contiguous: pts 2 (seq advances with it).
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 2, updNewMsg(2, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 1 {
		t.Fatalf("contiguous emit = %d", len(out.Emit))
	}
	// Duplicate replay of pts 2 with a REPLAYED seq range: skipped.
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 2, updNewMsg(2, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 0 {
		t.Fatalf("duplicate emitted %d updates", len(out.Emit))
	}
	// Fully stale (pts 1): ignored.
	out = processUpdatesResult{}
	_ = m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out)
	if len(out.Emit) != 0 {
		t.Fatalf("stale emitted %d updates", len(out.Emit))
	}
}

// TestMBSeqGate: seq_start beyond seq+1 is a gap; a replayed seq range is
// skipped wholesale (messagebox.py:450-460).
func TestMBSeqGate(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 1, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	// Contiguous next: seq_start 2.
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 2, updNewMsg(2, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 1 {
		t.Fatal("contiguous seq rejected")
	}
	// Replayed range (seq_start 2 again): skipped, no error, no emit.
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 2, updNewMsg(3, 1)), &out); err != nil {
		t.Fatalf("replayed seq range returned %v", err)
	}
	if len(out.Emit) != 0 {
		t.Fatal("replayed seq range emitted")
	}
	// Gap: seq_start 9 (expected 7).
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 9, updNewMsg(4, 1)), &out); !errors.Is(err, ErrUpdateGap) {
		t.Fatalf("gap seq err = %v", err)
	}
	if !m.isDiffing(mbAccount) {
		t.Fatal("seq gap did not start difference")
	}
}

// TestMBGapHoldsThenResolves: an update beyond the frontier is held in a
// possible gap and applied once the missing update arrives
// (messagebox.py:559-571 + 477-497).
func TestMBGapHoldsThenResolves(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	// Missing 2; receive 3 → held, no emit, no diff yet.
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(3, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 0 {
		t.Fatal("gap update emitted prematurely")
	}
	if m.isDiffing(mbAccount) {
		t.Fatal("possible gap must not start difference immediately")
	}
	// The missing 2 arrives: held 3 applies too.
	out = processUpdatesResult{}
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(2, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 2 {
		t.Fatalf("resolved gap emitted %d, want 2 (2 then 3)", len(out.Emit))
	}
	if len(m.gapsSnapshot()) != 0 {
		t.Fatal("gap not cleared after resolution")
	}
}

// TestMBGapTimeoutForcesDifference: a gap that stays unresolved past
// POSSIBLE_GAP_TIMEOUT starts difference recovery (check_deadlines,
// messagebox.py:283-294).
func TestMBGapTimeoutForcesDifference(t *testing.T) {
	m, clock := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	_ = m.processUpdates(batchOf(100, 0, updNewMsg(3, 1)), &out)

	clock.Advance(mbPossibleGapTimeout + time.Millisecond)
	m.checkDeadlines()
	if !m.isDiffing(mbAccount) {
		t.Fatal("expired gap did not start difference")
	}
	if len(m.gapsSnapshot()) != 0 {
		t.Fatal("gap not cleared when difference began")
	}
}

// TestMBNoUpdatesTimeout: a quiet stream past NO_UPDATES_TIMEOUT also
// triggers difference (messagebox.py:41-48, 286).
func TestMBNoUpdatesTimeout(t *testing.T) {
	m, clock := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	clock.Advance(mbNoUpdatesTimeout + time.Second)
	m.checkDeadlines()
	if !m.isDiffing(mbAccount) {
		t.Fatal("quiet-stream timeout did not start difference")
	}
}

// TestMBOutOfOrderSamePts: ReadChannelInbox (count 0) and
// NewChannelMessage (count 1) with the SAME pts must both apply — the
// batch is sorted by pts - pts_count first (messagebox.py:462-473,
// 577-590).
func TestMBOutOfOrderSamePts(t *testing.T) {
	m, _ := newTestBox()
	read := &gen.UpdateReadChannelInbox{ChannelID: 42, PTS: 10, MaxID: 5}
	msg := updChannelMsg(42, 10, 1)
	var out processUpdatesResult
	// The schema order (read first) would break an unsorted apply.
	if err := m.processUpdates(batchOf(100, 1, read, msg), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 2 {
		t.Fatalf("emitted %d, want both updates", len(out.Emit))
	}
	m.mu.Lock()
	entry := m.entries[mbEntry(42)]
	m.mu.Unlock()
	if entry == nil || entry.pts != 10 {
		t.Fatalf("channel pts = %+v, want 10", entry)
	}
}

// TestMBChannelTooLong: UpdateChannelTooLong starts channel difference
// (messagebox.py:525-527).
func TestMBChannelTooLong(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	// Without known channel state, tooLong cannot start a difference
	// (messagebox.py:366-374: try_begin_get_diff needs a known pts).
	if err := m.processUpdates(batchOf(100, 0, &gen.UpdateChannelTooLong{ChannelID: 7}), &out); err != nil {
		t.Fatal(err)
	}
	if m.isDiffing(mbEntry(7)) {
		t.Fatal("difference started without known channel state")
	}
	// With known state: tooLong starts the channel difference.
	if err := m.processUpdates(batchOf(100, 0, updChannelMsg(7, 5, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if err := m.processUpdates(batchOf(100, 0, &gen.UpdateChannelTooLong{ChannelID: 7}), &out); err != nil {
		t.Fatal(err)
	}
	if !m.isDiffing(mbEntry(7)) {
		t.Fatal("channel difference not started")
	}
}

// TestMBGetDifferenceRequestShape: the account difference request carries
// pts/date/qts from the box (messagebox.py:618-623).
func TestMBGetDifferenceRequestShape(t *testing.T) {
	m, clock := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	_ = m.processUpdates(batchOf(100, 0, updNewMsg(3, 1)), &out)
	// A held gap only becomes a difference request once its deadline
	// expires (check_deadlines).
	clock.Advance(mbPossibleGapTimeout + time.Millisecond)
	m.checkDeadlines()
	req, ok := m.getDifference()
	if !ok {
		t.Fatal("no difference request")
	}
	if req.PTS != 1 {
		t.Fatalf("diff pts = %d, want 1", req.PTS)
	}
	if req.Date != 100 {
		t.Fatalf("diff date = %d, want 100", req.Date)
	}
	if req.QTS != 0 {
		t.Fatalf("diff qts = %d, want 0", req.QTS)
	}
}

// TestMBApplyDifferenceVariants: DifferenceEmpty adopts date/seq;
// TooLong adopts pts; Difference adopts the full state and surfaces
// new_messages as synthetic UpdateNewMessage with pts 0
// (messagebox.py:631-704).
func TestMBApplyDifferenceVariants(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	// Trigger the account difference via a tooLong batch.
	if err := m.processUpdates(updatesBatch{}, &out); !errors.Is(err, ErrUpdateGap) {
		t.Fatal("expected tooLong gap")
	}

	// Difference with the missed state.
	diff := &gen.UpdatesDifference{
		NewMessages:  []gen.MessageClass{&gen.Message{ID: 77, Message: "missed", PeerID: &gen.PeerUser{UserID: 3}}},
		OtherUpdates: []gen.UpdateClass{updNewMsg(2, 1)},
		State:        &gen.UpdatesState{PTS: 2, QTS: 5, Date: 200, Seq: 9},
	}
	out = processUpdatesResult{}
	if err := m.applyDifference(diff, &out); err != nil {
		t.Fatal(err)
	}
	// Emit: the account entry is still diffing, so other_updates with pts
	// are skipped (messagebox.py:545-550); the synthetic new message
	// (pts 0 = no info) applies unconditionally.
	if len(out.Emit) != 1 {
		t.Fatalf("difference emitted %d, want 1", len(out.Emit))
	}
	if _, ok := out.Emit[0].(*gen.UpdateNewMessage); !ok {
		t.Fatalf("synthetic form = %T, want UpdateNewMessage", out.Emit[0])
	}
	// A complete Difference ends the difference (messagebox.py:647-648:
	// finish=True → end_get_diff for account and secret).
	if m.isDiffing(mbAccount) {
		t.Fatal("difference still running after a complete Difference")
	}
	// State adopted from the difference.
	req, ok := m.getDifference()
	_ = req
	_ = ok
	ss, _ := m.sessionState()
	if ss.Seq != 9 || ss.Qts != 5 {
		t.Fatalf("state after difference = %+v", ss)
	}

	// TooLong variant: pts adopted, finished.
	m2, _ := newTestBox()
	var out2 processUpdatesResult
	if err := m2.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out2); err != nil {
		t.Fatal(err)
	}
	if err := m2.processUpdates(batchOf(100, 9, updNewMsg(2, 1)), &out2); !errors.Is(err, ErrUpdateGap) {
		t.Fatal("expected gap")
	}
	out2 = processUpdatesResult{}
	if err := m2.applyDifference(&gen.UpdatesDifferenceTooLong{PTS: 50}, &out2); err != nil {
		t.Fatal(err)
	}
	ss2, _ := m2.sessionState()
	if ss2.Pts != 50 {
		t.Fatalf("TooLong pts = %d, want 50", ss2.Pts)
	}
}

// TestMBAccountOwnUpdatesAdvanceWithoutEmit: _self_outgoing updates
// advance pts/seq but are never dispatched (messagebox.py:416-419, 499;
// mtprotosender.py:744-790).
func TestMBAccountOwnUpdatesAdvanceWithoutEmit(t *testing.T) {
	m, _ := newTestBox()
	b := batchOf(100, 0, updNewMsg(1, 1))
	b.Self = true
	var out processUpdatesResult
	if err := m.processUpdates(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 0 {
		t.Fatalf("own update emitted %d events", len(out.Emit))
	}
	ss, _ := m.sessionState()
	if ss.Pts != 1 {
		t.Fatalf("pts did not advance: %+v", ss)
	}
	// Seq 0 = NO_SEQ: not committed (messagebox.py:508-509).
	if ss.Seq != 0 {
		t.Fatalf("NO_SEQ batch committed seq: %+v", ss)
	}
}

// TestMBGapBoundForcesDifference: a pending list beyond mbMaxGapUpdates
// forces difference instead of growing unboundedly (SuroMesh bounded-memory
// deviation, messagebox.py:559-571).
func TestMBGapBoundForcesDifference(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	// Skip far ahead with large strides: every update lands beyond the
	// frontier and extends the held gap until the bound forces the
	// difference.
	for i := int32(1); i <= int32(mbMaxGapUpdates)+3; i++ {
		_ = m.processUpdates(batchOf(100, 1, updNewMsg(i*300, 1)), &out)
		if m.isDiffing(mbAccount) {
			return // bound reached → difference forced
		}
	}
	t.Fatal("gap grew beyond the bound without forcing difference")
}

// TestMBQtsSecretEntry: qts-carrying updates drive the secret entry with
// count 1 (messagebox.py:116-118).
func TestMBQtsSecretEntry(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	enc := &gen.UpdateNewEncryptedMessage{
		Message: &gen.EncryptedMessage{RandomID: 1, ChatID: 2, Bytes: []byte{1}},
		QTS:     3,
	}
	if err := m.processUpdates(batchOf(100, 1, enc), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 1 {
		t.Fatalf("qts update emitted %d", len(out.Emit))
	}
	// Duplicate qts: ignored.
	out = processUpdatesResult{}
	_ = m.processUpdates(batchOf(100, 1, enc), &out)
	if len(out.Emit) != 0 {
		t.Fatal("duplicate qts emitted")
	}
}

// TestMBUpdatesTooLongIsGap: updatesTooLong has no date → gap +
// difference (messagebox.py:433-436).
func TestMBUpdatesTooLongIsGap(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	// With no known state, tooLong cannot start a difference (the
	// reference's try_begin_get_diff silently returns without state).
	if err := m.processUpdates(updatesBatch{}, &out); !errors.Is(err, ErrUpdateGap) {
		t.Fatalf("err = %v, want ErrUpdateGap", err)
	}
	if m.isDiffing(mbAccount) {
		t.Fatal("difference started without known state")
	}
	// With known state: tooLong starts the difference.
	if err := m.processUpdates(batchOf(100, 0, updNewMsg(1, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if err := m.processUpdates(updatesBatch{}, &out); !errors.Is(err, ErrUpdateGap) {
		t.Fatalf("err = %v", err)
	}
	if !m.isDiffing(mbAccount) {
		t.Fatal("tooLong did not start difference")
	}
}

// TestMBChannelDifferenceFlow: request built from cached access hash;
// without a hash the entry is forgotten (messagebox.py:727-758).
func TestMBChannelDifferenceFlow(t *testing.T) {
	m, _ := newTestBox()
	var out processUpdatesResult
	if err := m.processUpdates(batchOf(100, 0, &gen.UpdateChannelTooLong{ChannelID: 7}), &out); err != nil {
		t.Fatal(err)
	}

	// Without a cached entity: entry forgotten, no request.
	_, _, ok := m.getChannelDifference(func(int64) (*gen.InputChannel, bool) { return nil, false })
	if ok {
		t.Fatal("difference requested without an access hash")
	}
	m.mu.Lock()
	_, stillThere := m.entries[mbEntry(7)]
	m.mu.Unlock()
	if stillThere {
		t.Fatal("entry not forgotten after missing hash")
	}

	// With a cached entity: request carries pts and the real hash.
	if err := m.processUpdates(batchOf(100, 0, updChannelMsg(7, 5, 1)), &out); err != nil {
		t.Fatal(err)
	}
	if err := m.processUpdates(batchOf(100, 0, &gen.UpdateChannelTooLong{ChannelID: 7}), &out); err != nil {
		t.Fatal(err)
	}
	req, entry, ok := m.getChannelDifference(func(id int64) (*gen.InputChannel, bool) {
		return &gen.InputChannel{ChannelID: id, AccessHash: 0xfeed}, true
	})
	if !ok || entry != mbEntry(7) {
		t.Fatalf("channel difference ok=%v entry=%v", ok, entry)
	}
	if req.PTS != 5 || req.Limit != mbUserChannelDiffLimit {
		t.Fatalf("channel diff req = %+v", req)
	}
	if ch, ok := req.Channel.(*gen.InputChannel); !ok || ch.AccessHash != 0xfeed {
		t.Fatal("fabricated or missing access hash")
	}

	// Apply the channel difference.
	out = processUpdatesResult{}
	cd := &gen.UpdatesChannelDifference{
		Final:       true,
		PTS:         9,
		NewMessages: []gen.MessageClass{&gen.Message{ID: 8, PeerID: &gen.PeerChannel{ChannelID: 7}}},
	}
	if err := m.applyChannelDifference(entry, cd, &out); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	st := m.entries[entry]
	m.mu.Unlock()
	if st == nil || st.pts != 9 {
		t.Fatalf("channel pts after diff = %+v, want 9", st)
	}
	if len(out.Emit) != 1 {
		t.Fatalf("channel difference emitted %d", len(out.Emit))
	}
}

// TestMBCombinedBatch: UpdatesCombined applies via seq_start/seq like
// Updates (the reference treats them identically apart from seq_start).
func TestMBCombinedBatch(t *testing.T) {
	m, _ := newTestBox()
	b := updatesBatch{
		Date: ptrInt32MB(100), Seq: ptrInt32MB(2), SeqStart: ptrInt32MB(1),
		Updates: []tl.Object{updNewMsg(1, 1), updNewMsg(2, 1)},
		Users:   []gen.UserClass{&gen.User{ID: 9, AccessHash: ptrInt64MB(0xab)}},
	}
	var out processUpdatesResult
	if err := m.processUpdates(b, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Emit) != 2 {
		t.Fatalf("combined emitted %d", len(out.Emit))
	}
	if len(out.Users) != 1 {
		t.Fatal("users not surfaced")
	}
}

func ptrInt32MB(v int32) *int32 { return &v }
func ptrInt64MB(v int64) *int64 { return &v }

// gapsSnapshot returns the count of pending gaps (test helper).
func (m *messageBox) gapsSnapshot() map[mbEntry]int {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[mbEntry]int, len(m.gaps))
	for e, g := range m.gaps {
		out[e] = len(g.updates)
	}
	return out
}

// TestUpdateNormalizationRejectsForeign: non-Updates objects must be
// rejected by the normalization seam (fail-explicit, §7) while every
// Updates-family constructor is recognized.
func TestUpdateNormalizationRejectsForeign(t *testing.T) {
	if _, ok := normalizeUpdates(&gen.Pong{MsgID: 1, PingID: 2}, false); ok {
		t.Fatal("non-update object normalized as an update batch")
	}
	for _, o := range []tl.Object{
		&gen.Updates{}, &gen.UpdatesCombined{}, &gen.UpdateShort{},
		&gen.UpdateShortMessage{}, &gen.UpdateShortChatMessage{},
		&gen.UpdateShortSentMessage{}, &gen.UpdatesTooLong{},
		&gen.UpdateNewMessage{PTS: 1}, // bare updates normalize as one-update batches
	} {
		if _, ok := normalizeUpdates(o, false); !ok {
			t.Fatalf("Updates-family object %T not recognized", o)
		}
	}
}

func BenchmarkDuplicateDetection(b *testing.B) {
	m := newMessageBox()
	var out processUpdatesResult
	d := int32(100)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Same pts repeatedly: pure duplicate-detection path.
		_ = m.processUpdates(updatesBatch{Date: &d, Updates: []tl.Object{&gen.UpdateNewMessage{
			Message: &gen.Message{ID: 1, PeerID: &gen.PeerUser{UserID: 1}}, PTS: 1, PTSCount: 1}}}, &out)
	}
}
