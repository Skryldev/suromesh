package client_test

// Fake Soroush updates server (Phase 10): scripts the updates RPC family
// (getState / getDifference / getChannelDifference) and delivers
// unsolicited updates through the REAL wire path (PushUpdate →
// encryption → transport → dispatcher → update engine).

import (
	"context"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

const (
	updatesCtorGetState    = 0xedd4882a
	updatesCtorGetDiff     = 0x19c2f763
	updatesCtorGetChanDiff = 0x03173d78
)

// updatesFake scripts the update-state reconciliation surface.
type updatesFake struct {
	mu sync.Mutex

	// Gap-correcting difference log: every pushed update is recorded by
	// pts so getDifference can serve the missed range (a real server
	// answers from its update log; DifferenceEmpty would strand a gap).
	pushLog map[int32]*gen.UpdateNewMessage
	pushMax int32
	gapMode bool // serve gap-correcting differences from pushLog

	// getState answer.
	statePTS, stateQTS, stateDate, stateSeq int32

	// getDifference: queued answers, popped per request.
	diffAnswers []tl.Object
	diffErrs    []*rpc.RPCError

	// getChannelDifference: answers per channel id.
	chanDiffAnswers map[int64][]tl.Object
	chanDiffErrs    map[int64][]*rpc.RPCError

	getStateCalls, getDiffCalls, getChanDiffCalls int
	lastDiffPts, lastDiffDate, lastDiffQTS        int32
	lastChanDiffPts                               map[int64]int32
}

func newUpdatesFake() *updatesFake {
	return &updatesFake{
		chanDiffAnswers: make(map[int64][]tl.Object),
		chanDiffErrs:    make(map[int64][]*rpc.RPCError),
		lastChanDiffPts: make(map[int64]int32),
		pushLog:         make(map[int32]*gen.UpdateNewMessage),
	}
}

// enableGapCorrection makes getDifference serve the missed update range
// from the push log (a faithful server: DifferenceEmpty only when
// nothing is missing).
func (f *updatesFake) enableGapCorrection() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gapMode = true
}

// recordPush logs a pushed update for gap correction.
func (f *updatesFake) recordPush(u *gen.UpdateNewMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u.PTS > f.pushMax {
		f.pushMax = u.PTS
	}
	f.pushLog[u.PTS] = u
}

func (f *updatesFake) pushDiff(o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffAnswers = append(f.diffAnswers, o)
}

func (f *updatesFake) pushDiffErr(e *rpc.RPCError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.diffErrs = append(f.diffErrs, e)
}

func (f *updatesFake) pushChanDiff(channelID int64, o tl.Object) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chanDiffAnswers[channelID] = append(f.chanDiffAnswers[channelID], o)
}

func (f *updatesFake) setState(pts, qts, date, seq int32) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statePTS, f.stateQTS, f.stateDate, f.stateSeq = pts, qts, date, seq
}

func (f *updatesFake) handle(t testing.TB, ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch ctor {
	case updatesCtorGetState:
		f.getStateCalls++
		return &gen.UpdatesState{
			PTS: f.statePTS, QTS: f.stateQTS, Date: f.stateDate, Seq: f.stateSeq,
		}, nil

	case updatesCtorGetDiff:
		f.getDiffCalls++
		req, _ := obj.(*gen.UpdatesGetDifferenceRequest)
		if req != nil {
			f.lastDiffPts, f.lastDiffDate, f.lastDiffQTS = req.PTS, req.Date, req.QTS
		}
		if len(f.diffErrs) > 0 {
			e := f.diffErrs[0]
			f.diffErrs = f.diffErrs[1:]
			return nil, e
		}
		// Gap-correcting mode: serve every pushed update past the
		// request's pts (a faithful server log). Requests with no missed
		// updates get DifferenceEmpty.
		if f.gapMode {
			var missed []*gen.UpdateNewMessage
			maxPts := req.PTS
			for pts := req.PTS + 1; pts <= f.pushMax; pts++ {
				if u, ok := f.pushLog[pts]; ok {
					missed = append(missed, u)
					if pts > maxPts {
						maxPts = pts
					}
				}
			}
			if len(missed) == 0 {
				return &gen.UpdatesDifferenceEmpty{Date: f.stateDate, Seq: f.stateSeq}, nil
			}
			msgs := make([]gen.MessageClass, len(missed))
			for i, u := range missed {
				msgs[i] = u
			}
			return &gen.UpdatesDifference{
				NewMessages: msgs,
				State:       &gen.UpdatesState{PTS: maxPts, Date: f.stateDate, Seq: f.stateSeq},
			}, nil
		}
		if len(f.diffAnswers) == 0 {
			return &gen.UpdatesDifferenceEmpty{Date: f.stateDate, Seq: f.stateSeq}, nil
		}
		a := f.diffAnswers[0]
		f.diffAnswers = f.diffAnswers[1:]
		return a, nil

	case updatesCtorGetChanDiff:
		f.getChanDiffCalls++
		req, ok := obj.(*gen.UpdatesGetChannelDifferenceRequest)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_CHANNEL_DIFF"}
		}
		var chID int64
		if ch, ok := req.Channel.(*gen.InputChannel); ok {
			chID = ch.ChannelID
			f.lastChanDiffPts[chID] = req.PTS
		}
		if errs := f.chanDiffErrs[chID]; len(errs) > 0 {
			f.chanDiffErrs[chID] = errs[1:]
			return nil, errs[0]
		}
		if answers := f.chanDiffAnswers[chID]; len(answers) > 0 {
			f.chanDiffAnswers[chID] = answers[1:]
			return answers[0], nil
		}
		return &gen.UpdatesChannelDifferenceEmpty{Final: true, PTS: req.PTS}, nil
	}
	return nil, &rpc.RPCError{Code: 400, Message: "UNKNOWN_UPDATES_METHOD"}
}

// installUpdatesHooks routes the updates RPC family to the fake.
func installUpdatesHooks(t testing.TB, fake *appFake, uf *updatesFake) {
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case updatesCtorGetState, updatesCtorGetDiff, updatesCtorGetChanDiff:
			return uf.handle(t, ctor, body)
		}
		if prev != nil {
			return prev(ctor, body)
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
	})
}

// newUpdatesClient: ready client whose fake serves the updates family.
// UpdateWorkers defaults (4): subscriber execution may interleave —
// protocol ordering is guaranteed by the state machine, delivery
// concurrency by the worker pool (§12).
func newUpdatesClient(t testing.TB) (*client.Client, *appFake, *updatesFake) {
	t.Helper()
	return newUpdatesClientW(t, 0)
}

// newUpdatesClientW is newUpdatesClient with an explicit UpdateWorkers
// count (1 = strict delivery order).
func newUpdatesClientW(t testing.TB, workers int) (*client.Client, *appFake, *updatesFake) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	key := testAuthKeyOf(9)
	cfg := client.Config{
		Session:    session.NewWithKey(key),
		Host:       srv.Addr,
		TLSEnabled: boolPtr(false),
	}
	if workers > 0 {
		cfg.UpdateWorkers = workers
	}
	c, err := client.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	uf := newUpdatesFake()
	installUpdatesHooks(t, fake, uf)
	return c, fake, uf
}
