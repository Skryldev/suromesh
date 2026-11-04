package client

// MessageBox: the update-state machine, ported from the Python reference
//
//	reference/SPlusthon/splusthon/_updates/messagebox.py (830 lines)
//
// The port preserves the reference's semantics exactly; every deviation is
// commented and listed in docs/updates.md. Structure:
//
//	entries:  mbAccount (account-wide pts), mbSecret (account-wide qts),
//	          and one entry per channel (positive int64 channel IDs)
//	state:    pts per entry + per-entry no-update deadline; account-wide
//	          date/seq
//	gaps:     possibleGap per entry — held updates that may fill the gap
//	          on their own within possibleGapTimeout, else get difference
//	diffing:  entries currently being reconciled via getDifference /
//	          getChannelDifference
//
// All methods are mutex-guarded; the engine loop is the only caller, so
// the lock protects only Metrics()/State() readers.

import (
	"errors"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// Reference constants (messagebox.py:31-48).
const (
	// mbNoSeq mirrors NO_SEQ: seq 0 means "it doesn't matter".
	mbNoSeq = int32(0)
	// mbUserChannelDiffLimit mirrors USER_CHANNEL_DIFF_LIMIT (we are not a bot).
	mbUserChannelDiffLimit = int32(100)
	// mbPossibleGapTimeout mirrors POSSIBLE_GAP_TIMEOUT (Soroush: 100 ms).
	mbPossibleGapTimeout = 100 * time.Millisecond
	// mbNoUpdatesTimeout mirrors NO_UPDATES_TIMEOUT: how long without
	// updates before the client fetches the difference by itself.
	mbNoUpdatesTimeout = 15 * time.Minute
	// mbMaxGapUpdates bounds a single entry's pending list (DEVIATION: the
	// reference accumulates unboundedly; SuroMesh stops accumulating and
	// forces the difference instead — bounded memory beats buffering).
	mbMaxGapUpdates = 256
)

// mbEntry identifies a MessageBox entry: the account pts, the secret-chat
// qts, or a channel (positive channel ID). Negative values are the two
// sentinels; channel IDs are positive in the TL schema.
type mbEntry int64

const (
	mbAccount mbEntry = -1
	mbSecret  mbEntry = -2
)

// mbState is one entry's state: the current pts plus the deadline after
// which a quiet entry triggers a difference fetch (messagebox.py:127-141).
type mbState struct {
	pts      int32
	deadline time.Time
}

// mbGap mirrors PossibleGap (messagebox.py:152-165): held updates that
// may resolve the gap on their own before the deadline.
type mbGap struct {
	deadline time.Time
	updates  []tl.Object
}

// mbSessionState mirrors SessionState (_updates/session.py:6-45) — the
// persisted account-level update state.
type mbSessionState struct {
	Pts  int32
	Qts  int32
	Date int32
	Seq  int32
}

// mbChannelState mirrors ChannelState (_updates/session.py:48-66).
type mbChannelState struct {
	ChannelID int64
	Pts       int32
}

// GapError mirrors _updates.GapError: processUpdates detected a
// missing range and the caller must run difference recovery.
var ErrUpdateGap = errors.New("updates: gap detected")

// MessageBox is the ported update-state machine.
type messageBox struct {
	mu   sync.Mutex
	now  func() time.Time // injectable clock (tests); production: time.Now
	date int32            // account-wide date (unix seconds)
	seq  int32            // account-wide seq

	entries   map[mbEntry]*mbState
	gaps      map[mbEntry]*mbGap
	diffing   map[mbEntry]bool
	nextEntry mbEntry // entry with the closest deadline (optimization)
	hasNext   bool
}

func newMessageBox() *messageBox {
	return &messageBox{
		now:       time.Now,
		entries:   make(map[mbEntry]*mbState),
		gaps:      make(map[mbEntry]*mbGap),
		diffing:   make(map[mbEntry]bool),
		nextEntry: mbAccount,
	}
}

// mbEpoch returns the unix time of datetime epoch (messagebox.py:76-78):
// the reference compares against epoch() to avoid storing synthetic dates.
func mbEpoch() int32 { return 0 }

func (m *messageBox) deadline() time.Time { return m.now().Add(mbNoUpdatesTimeout) }

// load mirrors MessageBox.load (messagebox.py:221-239).
func (m *messageBox) load(ss mbSessionState, cs []mbChannelState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deadline()
	m.entries = make(map[mbEntry]*mbState)
	if ss.Pts != mbNoSeq {
		m.entries[mbAccount] = &mbState{pts: ss.Pts, deadline: d}
	}
	if ss.Qts != mbNoSeq {
		m.entries[mbSecret] = &mbState{pts: ss.Qts, deadline: d}
	}
	for _, s := range cs {
		m.entries[mbEntry(s.ChannelID)] = &mbState{pts: s.Pts, deadline: d}
	}
	m.date = ss.Date
	m.seq = ss.Seq
	m.nextEntry = mbAccount
	m.hasNext = m.entries[mbAccount] != nil
}

// sessionState mirrors MessageBox.session_state (messagebox.py:241-252).
func (m *messageBox) sessionState() (mbSessionState, []mbChannelState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	ss := mbSessionState{Date: m.date, Seq: m.seq}
	if st := m.entries[mbAccount]; st != nil {
		ss.Pts = st.pts
	}
	if st := m.entries[mbSecret]; st != nil {
		ss.Qts = st.pts
	}
	var cs []mbChannelState
	for e, st := range m.entries {
		if e > 0 {
			cs = append(cs, mbChannelState{ChannelID: int64(e), Pts: st.pts})
		}
	}
	return ss, cs
}

// isEmpty mirrors MessageBox.is_empty (messagebox.py:254-258).
func (m *messageBox) isEmpty() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.entries[mbAccount] == nil
}

// checkDeadlines mirrors check_deadlines (messagebox.py:260-296): entries
// whose deadline expired (quiet streams, pending gaps) start needing
// difference. Returns the next wakeup instant.
func (m *messageBox) checkDeadlines() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()

	if len(m.diffing) > 0 {
		return now // difference in flight: the loop acts immediately
	}

	deadline := m.deadline()
	if len(m.gaps) > 0 {
		for _, g := range m.gaps {
			if g.deadline.Before(deadline) {
				deadline = g.deadline
			}
		}
	} else if m.hasNext && m.entries[m.nextEntry] != nil {
		if m.entries[m.nextEntry].deadline.Before(deadline) {
			deadline = m.entries[m.nextEntry].deadline
		}
	}

	if !now.Before(deadline) {
		// Expired entries start getting difference; their gaps are cleared
		// (messagebox.py:283-294).
		for e, g := range m.gaps {
			if !now.Before(g.deadline) {
				m.diffing[e] = true
				delete(m.gaps, e)
			}
		}
		for e, st := range m.entries {
			if !now.Before(st.deadline) {
				m.diffing[e] = true
			}
		}
	}
	return deadline
}

// setState mirrors set_state (messagebox.py:326-350).
func (m *messageBox) setState(st mbSessionState, reset bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d := m.deadline()
	if st.Pts != mbNoSeq || !reset {
		m.entries[mbAccount] = &mbState{pts: st.Pts, deadline: d}
	} else {
		delete(m.entries, mbAccount)
	}
	// Reference note (messagebox.py:337-347): qts 0 means unknown at login
	// (drop the entry) but a real state while applying difference.
	if st.Qts != mbNoSeq || !reset {
		m.entries[mbSecret] = &mbState{pts: st.Qts, deadline: d}
	} else {
		delete(m.entries, mbSecret)
	}
	m.date = st.Date
	m.seq = st.Seq
	m.nextEntry, m.hasNext = mbAccount, m.entries[mbAccount] != nil
}

// trySetChannelState mirrors try_set_channel_state (messagebox.py:355-360):
// only fills in a channel's pts when nothing is known yet.
func (m *messageBox) trySetChannelState(id int64, pts int32) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e := mbEntry(id)
	if m.entries[e] == nil {
		m.entries[e] = &mbState{pts: pts, deadline: m.deadline()}
	}
}

// only possible when the entry has known state to diff from.

// endGetDiff mirrors end_get_diff (messagebox.py:384-391).
func (m *messageBox) endGetDiff(e mbEntry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.diffing, e)
	if st := m.entries[e]; st != nil {
		st.deadline = m.deadline()
	}
}

// isDiffing reports whether the entry's difference is in flight.
func (m *messageBox) isDiffing(e mbEntry) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.diffing[e]
}

// ptsUpdate is the normalization of one protocol update for the state
// machine (mirrors PtsInfo, messagebox.py:89-123).
type ptsUpdate struct {
	obj      tl.Object
	pts      int32
	ptsCount int32
	entry    mbEntry
	channel  bool // entry is a channel (affects difference selection)
}

// processUpdatesResult is the outcome of one processUpdates call.
type processUpdatesResult struct {
	Emit  []tl.Object // dispatchable updates (self-outgoing excluded)
	Users []gen.UserClass
	Chats []gen.ChatClass
}

// updatesBatch normalizes one incoming Updates-family object: the
// constructor's update list, its users/chats, date, and seq/seq_start.
type updatesBatch struct {
	Self     bool // _self_outgoing: advance state, never dispatch
	Bare     bool // a lone Update* over the stream (no seq/date semantics)
	Date     *int32
	Seq      *int32
	SeqStart *int32
	Updates  []tl.Object
	Users    []gen.UserClass
	Chats    []gen.ChatClass
}

// normalizeUpdates converts a wire Updates-family object into a batch
// (reference process_updates attribute extraction, messagebox.py:419-443).
// OwnUpdate results are unwrapped and marked self-outgoing; a bare
// Update* on the stream (the reference socket only ever delivers the
// Updates-family wrappers) is normalized as a one-update batch without
// seq/date semantics rather than dropped.
func normalizeUpdates(obj tl.Object, self bool) (updatesBatch, bool) {
	b := updatesBatch{Self: self}
	if ou, ok := obj.(rpc.OwnUpdate); ok {
		b.Self, obj = true, ou.Obj
	}
	switch u := obj.(type) {
	case *gen.Updates:
		date, seq := u.Date, u.Seq
		b.Date, b.Seq, b.SeqStart = &date, &seq, &seq
		for i := range u.Updates {
			b.Updates = append(b.Updates, u.Updates[i])
		}
		b.Users, b.Chats = u.Users, u.Chats
	case *gen.UpdatesCombined:
		date, seq, seqStart := u.Date, u.Seq, u.SeqStart
		b.Date, b.Seq, b.SeqStart = &date, &seq, &seqStart
		for i := range u.Updates {
			b.Updates = append(b.Updates, u.Updates[i])
		}
		b.Users, b.Chats = u.Users, u.Chats
	case *gen.UpdateShort:
		date := u.Date
		b.Date = &date
		b.Updates = []tl.Object{u.Update}
	case *gen.UpdateShortMessage:
		b.Updates = []tl.Object{u}
	case *gen.UpdateShortChatMessage:
		b.Updates = []tl.Object{u}
	case *gen.UpdateShortSentMessage:
		b.Updates = []tl.Object{u}
	case *gen.UpdatesTooLong:
		// date == nil in the reference → treated as a gap.
		return b, true
	default:
		// A lone Update* on the stream (the reference socket delivers the
		// Updates-family wrappers) — accepted only when its constructor
		// really belongs to the Update family (gen.UpdateClass is an empty
		// interface, so the check is by constructor ID).
		if isUpdateFamilyCtor(obj.CtorID()) {
			b.Bare = true
			b.Updates = []tl.Object{obj}
			return b, true
		}
		return b, false
	}
	return b, true
}

// processUpdates mirrors process_updates (messagebox.py:409-511). The
// caller feeds one batch; accepted updates are appended to out (state
// already advanced), entities are returned for cache seeding.
func (m *messageBox) processUpdates(b updatesBatch, out *processUpdatesResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if b.Date == nil && !b.Bare {
		// updatesTooLong is the only one with no date (messagebox.py:433-436).
		m.tryBeginGetDiffLocked(mbAccount)
		return ErrUpdateGap
	}
	seq := mbNoSeq
	if b.Seq != nil {
		seq = *b.Seq
	}
	seqStart := seq
	if b.SeqStart != nil {
		seqStart = *b.SeqStart
	}

	// Sequence gate (messagebox.py:450-460). For everything except
	// `updates`/`updatesCombined`, seq_start is 0 = "doesn't matter".
	// Bare updates carry no seq semantics at all.
	if seqStart != mbNoSeq && !b.Bare {
		if m.seq+1 > seqStart {
			// Already handled: skip the whole batch.
			return nil
		} else if m.seq+1 < seqStart {
			m.tryBeginGetDiffLocked(mbAccount)
			return ErrUpdateGap
		}
		// else: contiguous — apply.
	}

	// Sort by pts - pts_count (messagebox.py:462-473): Soroush sends
	// ReadChannelInbox (count 0) and NewChannelMessage (count 1) with the
	// same pts, out of order relative to each other.
	sorted := make([]tl.Object, len(b.Updates))
	copy(sorted, b.Updates)
	sortBySortKey(sorted)

	// Entries whose no-update deadline the batch resets (deduplicated;
	// messagebox.py:539-543 builds a set for the same reason).
	var resetSet []mbEntry
	seenReset := make(map[mbEntry]bool)

	var applied []tl.Object
	for _, u := range sorted {
		if a := m.applyPtsInfoLocked(u, &resetSet, seenReset); a != nil {
			applied = append(applied, a)
		}
	}
	m.resetDeadlinesLocked(resetSet, m.deadline())

	// Try to resolve pending gaps with the newly-applied state
	// (messagebox.py:477-497): each held update is retried; failures are
	// re-inserted (order preserved by cycling once).
	for key := range m.gaps {
		gap := m.gaps[key]
		sortBySortKey(gap.updates)
		var still []tl.Object
		for _, u := range gap.updates {
			if a := m.applyPtsInfoLocked(u, nil, nil); a != nil {
				applied = append(applied, a)
			} else {
				still = append(still, u)
			}
		}
		if len(still) == 0 {
			delete(m.gaps, key)
		} else {
			gap.updates = still
		}
	}

	// Self-outgoing updates advance state but are never dispatched
	// (messagebox.py:416-419, 499).
	for _, u := range applied {
		if !b.Self {
			out.Emit = append(out.Emit, u)
		}
	}
	out.Users, out.Chats = b.Users, b.Chats

	// Commit seq/date only when everything applied cleanly
	// (messagebox.py:501-509). Bare updates carry neither.
	if len(applied) > 0 && len(m.gaps) == 0 && !b.Bare && b.Date != nil {
		if *b.Date != mbEpoch() {
			m.date = *b.Date
		}
		if seq != mbNoSeq {
			m.seq = seq
		}
	}
	return nil
}

// applyPtsInfoLocked mirrors apply_pts_info (messagebox.py:518-605).
// resetSet == nil means "gap re-application" (no deadline bookkeeping).
func (m *messageBox) applyPtsInfoLocked(u tl.Object, resetSet *[]mbEntry, seenReset map[mbEntry]bool) tl.Object {
	// UpdateChannelTooLong → the channel's difference is needed
	// (messagebox.py:525-527).
	if ctl, ok := u.(*gen.UpdateChannelTooLong); ok {
		m.tryBeginGetDiffLocked(mbEntry(ctl.ChannelID))
		return nil
	}

	pi, ok := ptsInfo(u)
	if !ok {
		// No pts: order-independent, always applies (messagebox.py:530-534).
		return u
	}

	if resetSet != nil && !seenReset[pi.entry] {
		seenReset[pi.entry] = true
		*resetSet = append(*resetSet, pi.entry)
	}

	if m.diffing[pi.entry] {
		// Skip while the difference is being fetched (messagebox.py:545-550).
		return nil
	}

	st := m.entries[pi.entry]
	if st != nil {
		switch {
		case st.pts+pi.ptsCount > pi.pts:
			// Duplicate/stale: ignore (messagebox.py:554-558).
			return nil
		case st.pts+pi.ptsCount < pi.pts:
			// Possible gap: hold the update (messagebox.py:559-571).
			gap := m.gaps[pi.entry]
			if gap == nil {
				gap = &mbGap{deadline: m.now().Add(mbPossibleGapTimeout)}
				m.gaps[pi.entry] = gap
			}
			if len(gap.updates) < mbMaxGapUpdates {
				gap.updates = append(gap.updates, u)
			} else {
				// Bounded-memory deviation: stop accumulating and force
				// the difference now.
				m.tryBeginGetDiffLocked(pi.entry)
			}
			return nil
		}
		// Contiguous: apply (messagebox.py:572-575).
	} else if m.diffing[pi.entry] {
		return nil
	}

	if st != nil {
		st.pts = pi.pts
	} else {
		// New entry. The count-0 rule (messagebox.py:577-603): a pts_count
		// of 0 must not advance past a following count-1 update with the
		// same pts, so initialize one lower ("or 1" keeps pts positive).
		init := pi.pts
		if pi.ptsCount == 0 {
			init--
		}
		if init <= 0 {
			init = 1
		}
		m.entries[pi.entry] = &mbState{pts: init, deadline: m.deadline()}
	}
	return u
}

// getDifference mirrors get_difference (messagebox.py:612-628).
func (m *messageBox) getDifference() (*gen.UpdatesGetDifferenceRequest, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range []mbEntry{mbAccount, mbSecret} {
		if !m.diffing[e] {
			continue
		}
		acc := m.entries[mbAccount]
		sec := m.entries[mbSecret]
		if acc == nil {
			return nil, false // no known state: cannot diff (reference raises)
		}
		qts := mbNoSeq
		if sec != nil {
			qts = sec.pts
		}
		return &gen.UpdatesGetDifferenceRequest{
			PTS:  acc.pts,
			Date: m.date,
			QTS:  qts,
		}, true
	}
	return nil, false
}

// applyDifference mirrors apply_difference (messagebox.py:631-673).
func (m *messageBox) applyDifference(diff tl.Object, out *processUpdatesResult) error {
	finish := false
	switch d := diff.(type) {
	case *gen.UpdatesDifferenceEmpty:
		finish = true
		m.mu.Lock()
		m.date = d.Date
		m.seq = d.Seq
		m.mu.Unlock()
		return nil
	case *gen.UpdatesDifference:
		finish = true
		out.Emit = m.applyDifferenceType(d.State, toObjects(d.OtherUpdates), d.NewMessages, d.NewEncryptedMessages, out)
		out.Users, out.Chats = d.Users, d.Chats
	case *gen.UpdatesDifferenceSlice:
		finish = false
		_ = m.applyDifferenceType(d.IntermediateState, toObjects(d.OtherUpdates), d.NewMessages, d.NewEncryptedMessages, out)
		out.Users, out.Chats = d.Users, d.Chats
		return nil
	case *gen.UpdatesDifferenceTooLong:
		finish = true
		m.mu.Lock()
		if st := m.entries[mbAccount]; st != nil {
			st.pts = d.PTS
		}
		m.mu.Unlock()
	default:
		return errUnexpectedDifference
	}
	if finish {
		m.endDiffPair()
	}
	return nil
}

// applyDifferenceType mirrors apply_difference_type (messagebox.py:675-704):
// adopt the new state, re-process other_updates through the normal
// pipeline (seq 0, epoch date), then surface new messages as synthetic
// UpdateNewMessage / UpdateNewEncryptedMessage with pts 0.
func (m *messageBox) applyDifferenceType(state gen.UpdatesStateClass, other []tl.Object,
	newMsgs []gen.MessageClass, newEnc []gen.EncryptedMessageClass, out *processUpdatesResult) []tl.Object {
	var st mbSessionState
	if s, ok := state.(*gen.UpdatesState); ok {
		st = mbSessionState{Pts: s.PTS, Qts: s.QTS, Date: s.Date, Seq: s.Seq}
	}
	m.setState(st, false) // reset=false: zero qts is a real state here

	epoch := mbEpoch()
	b := updatesBatch{Date: &epoch, Seq: &mbNoSeqShared, SeqStart: &mbNoSeqShared, Updates: other}
	var res processUpdatesResult
	_ = m.processUpdates(b, &res)
	out.Emit = append(out.Emit, res.Emit...)

	for i := range newMsgs {
		out.Emit = append(out.Emit, &gen.UpdateNewMessage{
			Message:  newMsgs[i],
			PTS:      mbNoSeq,
			PTSCount: mbNoSeq,
		})
	}
	for i := range newEnc {
		out.Emit = append(out.Emit, &gen.UpdateNewEncryptedMessage{
			Message: newEnc[i],
			QTS:     mbNoSeq,
		})
	}
	return out.Emit
}

// endDiffPair ends both account and secret difference (messagebox.py:706-720).
func (m *messageBox) endDiffPair() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range []mbEntry{mbAccount, mbSecret} {
		if m.diffing[e] {
			delete(m.diffing, e)
			if st := m.entries[e]; st != nil {
				st.deadline = m.deadline()
			}
		}
	}
}

// getChannelDifference mirrors get_channel_difference (messagebox.py:727-758).
// The channel's access hash must come from the entity cache — never
// fabricated; without it the entry is forgotten (messagebox.py:735-743).
func (m *messageBox) getChannelDifference(resolveChannel func(int64) (*gen.InputChannel, bool)) (*gen.UpdatesGetChannelDifferenceRequest, mbEntry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for e := range m.diffing {
		if e <= 0 {
			continue // account/secret handled by getDifference
		}
		ch, ok := resolveChannel(int64(e))
		if !ok {
			// Missing access hash: end the diff and drop the stale entry so
			// the next update can rebuild it (messagebox.py:737-743).
			delete(m.diffing, e)
			delete(m.entries, e)
			delete(m.gaps, e)
			return nil, 0, false
		}
		st := m.entries[e]
		if st == nil {
			return nil, 0, false
		}
		return &gen.UpdatesGetChannelDifferenceRequest{
			Force:   false,
			Channel: ch,
			Filter:  &gen.ChannelMessagesFilterEmpty{},
			PTS:     st.pts,
			Limit:   mbUserChannelDiffLimit,
		}, e, true
	}
	return nil, 0, false
}

// applyChannelDifference mirrors apply_channel_difference
// (messagebox.py:761-811).
func (m *messageBox) applyChannelDifference(entry mbEntry, diff tl.Object, out *processUpdatesResult) error {
	m.mu.Lock()
	delete(m.gaps, entry)
	m.mu.Unlock()

	switch d := diff.(type) {
	case *gen.UpdatesChannelDifferenceEmpty:
		m.mu.Lock()
		if st := m.entries[entry]; st != nil {
			st.pts = d.PTS
		}
		m.mu.Unlock()
		m.endGetDiff(entry)
		return nil
	case *gen.UpdatesChannelDifferenceTooLong:
		m.mu.Lock()
		if dlg, ok := d.Dialog.(*gen.Dialog); ok && dlg.PTS != nil {
			if st := m.entries[entry]; st != nil {
				st.pts = *dlg.PTS
			}
		}
		m.mu.Unlock()
		// Entities are still extracted (callers see Users/Chats) but no
		// events are dispatched: too-long means resync via dialogs
		// (messagebox.py:778-787).
		if d.Final {
			m.endGetDiff(entry)
		}
		return nil
	case *gen.UpdatesChannelDifference:
		if d.Final {
			m.endGetDiff(entry)
		}
		m.mu.Lock()
		if st := m.entries[entry]; st != nil {
			st.pts = d.PTS
		}
		m.mu.Unlock()
		epoch := mbEpoch()
		b := updatesBatch{
			Date: &epoch, Seq: &mbNoSeqShared, SeqStart: &mbNoSeqShared,
			Updates: toObjects(d.OtherUpdates), Users: d.Users, Chats: d.Chats,
		}
		var res processUpdatesResult
		_ = m.processUpdates(b, &res)
		out.Emit = append(out.Emit, res.Emit...)
		for i := range d.NewMessages {
			out.Emit = append(out.Emit, &gen.UpdateNewChannelMessage{
				Message:  d.NewMessages[i],
				PTS:      mbNoSeq,
				PTSCount: mbNoSeq,
			})
		}
		out.Users, out.Chats = d.Users, d.Chats
		return nil
	default:
		return errUnexpectedDifference
	}
}

// endChannelDifferencePremature mirrors end_channel_difference
// (messagebox.py:813-828): temporary issues keep the pts for a later
// retry; a ban forgets the channel's state entirely.
type mbPrematureEnd int32

const (
	mbEndTemporary mbPrematureEnd = iota
	mbEndBanned
)

func (m *messageBox) endChannelDifferencePremature(entry mbEntry, reason mbPrematureEnd) {
	m.mu.Lock()
	delete(m.gaps, entry)
	delete(m.diffing, entry)
	if reason == mbEndBanned {
		delete(m.entries, entry)
	} else if st := m.entries[entry]; st != nil {
		st.deadline = m.deadline()
	}
	m.mu.Unlock()
}

func (m *messageBox) tryBeginGetDiffLocked(e mbEntry) {
	if m.entries[e] == nil {
		return
	}
	m.diffing[e] = true
	delete(m.gaps, e)
}

func (m *messageBox) resetDeadlinesLocked(entries []mbEntry, d time.Time) {
	for _, e := range entries {
		if st := m.entries[e]; st != nil {
			st.deadline = d
		}
	}
	// Recompute the min-deadline entry lazily (cheap: few entries).
	var minD time.Time
	var minE mbEntry
	found := false
	for e, st := range m.entries {
		if !found || st.deadline.Before(minD) {
			minD, minE, found = st.deadline, e, true
		}
	}
	m.nextEntry, m.hasNext = minE, found
}

// mbNoSeqShared is the shared seq=0 marker for difference batches
// (messagebox.py:691: "this way date is not used").
var mbNoSeqShared = mbNoSeq

// toObjects widens a typed update slice.
func toObjects(us []gen.UpdateClass) []tl.Object {
	out := make([]tl.Object, len(us))
	for i := range us {
		out[i] = us[i]
	}
	return out
}

var errUnexpectedDifference = errors.New("updates: unexpected difference constructor")

// sortKey mirrors _sort_gaps (messagebox.py:462-464).
func sortKey(u tl.Object) int32 {
	if pi, ok := ptsInfo(u); ok {
		return pi.pts - pi.ptsCount
	}
	return 0
}

// sortBySortKey applies the reference's stable sort by (pts - pts_count).
func sortBySortKey(us []tl.Object) {
	// Insertion sort: batches are small (protocol batches ≈ few updates)
	// and stability matters for equal keys.
	for i := 1; i < len(us); i++ {
		for j := i; j > 0 && sortKey(us[j]) < sortKey(us[j-1]); j-- {
			us[j], us[j-1] = us[j-1], us[j]
		}
	}
}

// gapsCount reports the number of entries with pending possible gaps
// (metrics helper; the reference tracks this in possible_gaps).
func (m *messageBox) gapsCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.gaps)
}
