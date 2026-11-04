package rpc

import (
	"sync"

	"github.com/Skryldev/suromesh/mtproto"
)

// idAssigner hands out (msg_id, seq_no) pairs atomically: MTProto requires
// monotonic unique message IDs and per-session sequence numbers whose
// parity carries content-relatedness; assigning them under one lock keeps
// both invariants under arbitrary concurrency.
//
// The underlying generators are the Phase 2 implementations (monotonic
// guard last+4, timestamp-based IDs congruent 0 mod 4; seq parity rules).
type idAssigner struct {
	mu  sync.Mutex
	gen *mtproto.MessageIDGenerator
	seq *mtproto.SeqNoGenerator
}

func newIDAssigner(offset int64) *idAssigner {
	return &idAssigner{
		gen: &mtproto.MessageIDGenerator{Offset: offset},
		seq: &mtproto.SeqNoGenerator{},
	}
}

// assign returns the next message ID and its sequence number for a
// content-related (RPC) or control message.
func (a *idAssigner) assign(contentRelated bool) (int64, int32) {
	a.mu.Lock()
	id := a.gen.NewMessageID()
	seq := a.seq.Next(contentRelated)
	a.mu.Unlock()
	return id, seq
}

// setTimeOffset corrects the local clock offset (bad-msg codes 16/17).
func (a *idAssigner) setTimeOffset(serverMsgID int64, nowSec int64) {
	correct := serverMsgID >> 32
	a.mu.Lock()
	a.gen.Offset = correct - nowSec
	a.gen.Last = 0 // reference: reset so the next ID derives from the new offset
	a.mu.Unlock()
}

// advanceSeq implements bad-msg codes 32/33 (seqno too low/high).
func (a *idAssigner) advanceSeq(delta int32) {
	a.mu.Lock()
	a.seq.Advance(delta)
	a.mu.Unlock()
}

// resetSession mirrors the reference reconnect behavior: a fresh session ID
// and a zeroed sequence counter (sequence numbers are per-session).
func (a *idAssigner) resetSession() (int64, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq = &mtproto.SeqNoGenerator{}
	return mtproto.NewSessionID()
}
