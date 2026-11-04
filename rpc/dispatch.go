package rpc

import (
	"fmt"
	"sync/atomic"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
	"github.com/Skryldev/suromesh/transport"
)

// recvLoop is the single response dispatcher: it owns inbound correlation.
// Malformed server data never reaches pending state — every decode failure
// is contained and counted.
func (e *Engine) recvLoop() {
	defer e.once.Do(func() { close(e.done) })
	acks := make([]int64, 0, 16)

	for {
		// Flush batched acknowledgements before blocking on the next read
		// (one MsgsAck per read iteration at most; content-unrelated).
		if len(acks) > 0 {
			e.sendAcks(acks)
			acks = acks[:0]
		}
		// Replay staged requests (RetryOnReconnect policy) — the dispatcher
		// owns this so Send backpressure can never wedge the lifecycle.
		select {
		case <-e.replaySignal:
			e.drainReplay()
		default:
		}

		var pkt []byte
		select {
		case p, ok := <-e.tr.Inbound():
			if !ok {
				// Channel closed with the transport: leave.
				return
			}
			pkt = p
		case <-e.replaySignal:
			e.drainReplay()
			continue
		case <-e.ctx.Done():
			return
		}
		plain, err := mtprotoDecrypt(e.cfg.AuthKey, pkt)
		if err != nil {
			e.metrics.EnvelopeErrors.Add(1)
			continue // hostile envelope: drop, never corrupt state
		}
		acks = e.handlePayload(plain, acks)
	}
}

// handlePayload walks the TLMessages in a decrypted payload. It is the
// fuzz seam: arbitrary hostile bytes must never panic or corrupt registry
// state.
// handlePayload processes the TLMessages in a decrypted payload. It is the
// fuzz seam: arbitrary hostile bytes must never panic or corrupt registry
// state.
//
// A payload carries exactly ONE TLMessage whose body may be a
// msg_container holding several messages (the reference's batching form);
// any bytes after that first message are the envelope's IGE padding and
// must NOT be parsed as messages (Phase 7 soak found the original loop
// misreading padding as garbage messages).
func (e *Engine) handlePayload(plain []byte, acks []int64) []int64 {
	if len(plain) < 16 {
		e.metrics.EnvelopeErrors.Add(1)
		return acks
	}
	r := tl.NewReader(plain[16:])
	msgID, seqNo, body, err := readTLMessage(r)
	if err != nil {
		e.metrics.ProtocolErrors.Add(1)
		return acks
	}
	acks = append(acks, msgID)
	obj, err := tl.Unmarshal(body)
	if err != nil {
		e.metrics.ProtocolErrors.Add(1)
		return acks
	}
	if err := e.handleMessage(msgID, seqNo, obj, 0); err != nil {
		e.metrics.ProtocolErrors.Add(1)
	}
	return acks
}

// readTLMessage parses msg_id|seq_no|length|body from the payload stream.
func readTLMessage(r *tl.Reader) (int64, int32, []byte, error) {
	msgID, err := r.ReadInt64()
	if err != nil {
		return 0, 0, nil, err
	}
	seqNo, err := r.ReadInt32()
	if err != nil {
		return 0, 0, nil, err
	}
	length, err := r.ReadInt32()
	if err != nil {
		return 0, 0, nil, err
	}
	if length < 0 || int(length) > r.Remaining() {
		return 0, 0, nil, fmt.Errorf("%w: message length %d", tl.ErrInvalidLength, length)
	}
	body, err := r.ReadRaw(int(length))
	return msgID, seqNo, body, err
}

// depth guards nested container/gzip recursion.
const maxNesting = 4

// handleMessage routes one decoded inbound message object. A failure in
// one message never affects its siblings (bounded per-message decode).
func (e *Engine) handleMessage(msgID int64, seqNo int32, obj tl.Object, depth int) error {
	if obj == nil {
		return fmt.Errorf("rpc: nil message body")
	}
	if depth > maxNesting {
		return fmt.Errorf("rpc: message nesting exceeds %d", maxNesting)
	}

	switch o := obj.(type) {
	case *tl.MessageContainer:
		// Each contained message correlates independently; bounded by the
		// Phase 2 decoder limits (100 messages, 1 MiB payload).
		for _, m := range o.Messages {
			if err := e.handleMessage(m.MsgID, m.SeqNo, m.Obj, depth+1); err != nil {
				e.metrics.ProtocolErrors.Add(1)
			}
		}
		return nil
	case *tl.GzipPacked:
		inner, err := tl.Unmarshal(o.Data)
		if err != nil {
			e.metrics.ProtocolErrors.Add(1)
			return fmt.Errorf("rpc: decode gzip body: %w", err)
		}
		return e.handleMessage(msgID, seqNo, inner, depth+1)
	case *tl.RpcResult:
		e.handleRPCResult(o)
		return nil
	case *gen.BadServerSalt:
		e.handleBadServerSalt(o)
		return nil
	case *gen.BadMsgNotification:
		e.handleBadMsgNotification(msgID, o)
		return nil
	case *gen.NewSessionCreated:
		e.salt.Store(o.ServerSalt)
		e.metrics.SaltUpdates.Add(1)
		return nil
	case *gen.Pong:
		return nil // transport-level keepalive is WS-based; MTProto pings unused
	case *gen.MsgsAck:
		return nil
	case *gen.FutureSalts:
		return nil
	case *gen.MsgsStateReq:
		e.replyStateInfo(msgID, len(o.MsgIds))
		return nil
	case *gen.MsgResendReq:
		// Server asks us to resend messages we sent — we do not implement
		// outbound message history in the engine; acknowledge via state info.
		e.replyStateInfo(msgID, len(o.MsgIds))
		return nil
	case *gen.MsgsStateInfo, *gen.MsgsAllInfo, *gen.MsgDetailedInfo, *gen.MsgNewDetailedInfo:
		return nil
	}

	// Updates (Phase 7 will consume): never satisfy pending RPC requests.
	if isUpdateObject(obj) {
		select {
		case e.updates <- obj:
		default:
			e.metrics.UpdatesDropped.Add(1)
		}
		return nil
	}

	e.metrics.UnknownResponses.Add(1)
	return nil
}

// VectorResult wraps a top-level bare vector result (ctor 0x1cb5c415).
// The reference decodes boxed vectors in tgread_object as a list of
// generically decoded objects; Go represents that as this wrapper.
// Schema functions declared `= Vector<T>` (users.getUsers,
// messages.getSuggestedDialogFilters, photos.deletePhotos, …) answer with
// this shape.
type VectorResult struct {
	Items []tl.Object
}

// CtorID implements tl.Object (the vector constructor).
func (*VectorResult) CtorID() uint32 { return tl.VectorID }

// Encode re-serializes items (boxed form). Vector results are never sent
// by the client; provided for round-trip completeness.
func (v *VectorResult) Encode(w *tl.Writer) error {
	w.PutVectorBoxedHeader(len(v.Items))
	for i := range v.Items {
		if v.Items[i] == nil {
			return fmt.Errorf("rpc: nil element in vector result")
		}
		if err := v.Items[i].Encode(w); err != nil {
			return err
		}
	}
	return w.Err()
}

// decodeVectorResult decodes a boxed vector whose ctor has already been
// consumed by the caller (only the element count remains).
func decodeVectorResult(r *tl.Reader) (*VectorResult, error) {
	n, err := r.ReadVectorBareHeader()
	if err != nil {
		return nil, err
	}
	items := make([]tl.Object, 0, min(n, 1024))
	for i := 0; i < n; i++ {
		id, err := r.ReadUint32()
		if err != nil {
			return nil, err
		}
		obj, err := gen.DecodeObjectAfterID(id, r)
		if err != nil {
			return nil, err
		}
		items = append(items, obj)
	}
	return &VectorResult{Items: items}, nil
}

var debugProtoErr atomic.Value // temporary diagnostics (Phase 7)

// handleRPCResult correlates rpc_result#f35c6d01 with its pending request.
func (e *Engine) handleRPCResult(o *tl.RpcResult) {
	if o.Error != nil {
		rpcErr := parseRPCError(o.Error.ErrorCode, o.Error.ErrorMessage)
		e.metrics.RequestsRPCError.Add(1)
		if p, won := e.reg.finalizeByMsgID(o.ReqMsgID, nil, rpcErr); won && p != nil {
			_ = p
		}
		return
	}
	// Lazy-decoded body: decode now within this message's lifetime so a
	// malformed result is contained here. Bare vectors are decoded into
	// VectorResult (reference tgread_object behavior).
	var result tl.Object
	var err error
	if len(o.Body) >= 4 && leU32(o.Body[:4]) == tl.VectorID {
		vr := tl.NewReader(o.Body)
		if _, err2 := vr.ReadUint32(); err2 != nil {
			err = err2
		} else {
			result, err = decodeVectorResult(vr)
		}
	} else {
		result, err = tl.Unmarshal(o.Body)
	}
	if err != nil {
		debugProtoErr.Store(err.Error())
		e.metrics.ProtocolErrors.Add(1)
		_, _ = e.reg.finalizeByMsgID(o.ReqMsgID, nil, fmt.Errorf("rpc: decode result: %w", err))
		return
	}
	if p, won := e.reg.finalizeByMsgID(o.ReqMsgID, result, nil); won {
		_ = p
	}
	// Reference _store_own_updates (mtprotosender.py:744-790): update-family
	// results ALSO feed the updates stream, flagged as own updates — the
	// caller receives the result AND the update state machine advances,
	// but the update is never re-dispatched to subscribers.
	if isUpdateFamily(result) {
		select {
		case e.updates <- OwnUpdate{Obj: result}:
		default:
			e.metrics.UpdatesDropped.Add(1)
		}
		return
	}
	// Affected-history results carry pts state without being Updates:
	// convert them into a self-outgoing UpdateShort(UpdateDeleteMessages)
	// (reference mtprotosender.py:771-783 — bots otherwise detect gaps
	// when deleting messages).
	if own := ownUpdateFromAffected(result); own != nil {
		select {
		case e.updates <- OwnUpdate{Obj: own}:
		default:
			e.metrics.UpdatesDropped.Add(1)
		}
	}
}

// OwnUpdate wraps an update-family RPC result: the update state machine
// must process it (pts/seq advance) but subscribers must not see it
// twice (reference _self_outgoing flag, messagebox.py:416-419). It rides
// the updates channel, so it implements tl.Object nominally (it is never
// serialized — the update state machine unwraps it immediately).
type OwnUpdate struct{ Obj tl.Object }

// CtorID implements tl.Object (never encoded: panics loudly if it ever
// reaches the wire by mistake).
func (o OwnUpdate) CtorID() uint32 { panic("rpc: OwnUpdate is not a wire object") }

// Encode implements tl.Object (never encoded).
func (o OwnUpdate) Encode(*tl.Writer) error { panic("rpc: OwnUpdate is not a wire object") }

// ownUpdateFromAffected converts messages.AffectedHistory /
// AffectedMessages / AffectedFoundMessages into the reference's
// self-outgoing UpdateShort form; non-affected results return nil.
func ownUpdateFromAffected(obj tl.Object) tl.Object {
	var pts, ptsCount int32
	switch a := obj.(type) {
	case *gen.MessagesAffectedHistory:
		pts, ptsCount = a.PTS, a.PTSCount
	case *gen.MessagesAffectedMessages:
		pts, ptsCount = a.PTS, a.PTSCount
	case *gen.MessagesAffectedFoundMessages:
		pts, ptsCount = a.PTS, a.PTSCount
	default:
		return nil
	}
	return &gen.UpdateShort{
		Update: &gen.UpdateDeleteMessages{Messages: []int32{}, PTS: pts, PTSCount: ptsCount},
		Date:   0, // epoch: the date is never read for this synthetic form
	}
}

// isUpdateFamily reports whether obj belongs to the Updates* result family
// (reference _update_ids frozenset in mtprotosender._store_own_updates).
func isUpdateFamily(obj tl.Object) bool {
	switch obj.(type) {
	case *gen.Updates, *gen.UpdatesCombined, *gen.UpdateShort,
		*gen.UpdateShortMessage, *gen.UpdateShortChatMessage,
		*gen.UpdateShortSentMessage:
		return true
	}
	return false
}

// handleBadServerSalt adopts the new salt and resends the affected request
// (reference behavior: the message provably did not execute).
func (e *Engine) handleBadServerSalt(o *gen.BadServerSalt) {
	e.salt.Store(o.NewServerSalt)
	e.metrics.SaltUpdates.Add(1)
	if !e.cfg.ResendOnBadSalt {
		return
	}
	if p, ok := e.reg.lookup(o.BadMsgID); ok && !p.isDone() {
		if err := e.resend(p); err != nil {
			e.reg.finalizeByMsgID(p.msgID, nil, err)
		}
	}
}

// handleBadMsgNotification mirrors mtprotostate._handle_bad_notification:
// correctable conditions fix engine state and resend; others fail the call.
func (e *Engine) handleBadMsgNotification(ourMsgID int64, o *gen.BadMsgNotification) {
	switch o.ErrorCode {
	case 16, 17: // msg_id too low/high: correct the clock offset
		e.ids.setTimeOffset(ourMsgID, e.cfg.Now().Unix())
		if e.cfg.ResendOnBadSalt {
			if p, ok := e.reg.lookup(o.BadMsgID); ok && !p.isDone() {
				if err := e.resend(p); err != nil {
					e.reg.finalizeByMsgID(p.msgID, nil, err)
				}
			}
		}
	case 32: // seq_no too low
		e.ids.advanceSeq(64)
		if e.cfg.ResendOnBadSalt {
			if p, ok := e.reg.lookup(o.BadMsgID); ok && !p.isDone() {
				if err := e.resend(p); err != nil {
					e.reg.finalizeByMsgID(p.msgID, nil, err)
				}
			}
		}
	case 33: // seq_no too high
		e.ids.advanceSeq(-16)
		if e.cfg.ResendOnBadSalt {
			if p, ok := e.reg.lookup(o.BadMsgID); ok && !p.isDone() {
				if err := e.resend(p); err != nil {
					e.reg.finalizeByMsgID(p.msgID, nil, err)
				}
			}
		}
	default:
		if p, ok := e.reg.lookup(o.BadMsgID); ok {
			e.reg.finalizeByMsgID(o.BadMsgID, nil, fmt.Errorf("%w: code %d", ErrBadMessage, o.ErrorCode))
			_ = p
		}
	}
}

// sendAcks acknowledges received message IDs (content-unrelated MsgsAck).
func (e *Engine) sendAcks(ids []int64) {
	ack := &gen.MsgsAck{MsgIds: ids}
	w := tl.NewWriter(16 + 8*len(ids))
	if err := ack.Encode(w); err != nil {
		return
	}
	body := make([]byte, w.Len())
	copy(body, w.Bytes())
	msgID, seqNo := e.ids.assign(false)
	payload := e.frameMessage(msgID, seqNo, body, false)
	envelope, err := mtproto.EncryptMessageData(e.cfg.AuthKey, payload, e.cfg.Rand)
	if err != nil {
		return
	}
	if err := e.tr.Send(e.ctx, envelope); err == nil {
		e.metrics.AcksSent.Add(1)
	}
}

// replyStateInfo answers msgs_state_req with the reference's all-ones info
// (info=chr(1)*count, matching mtprotosender._handle_state_forgotten).
func (e *Engine) replyStateInfo(reqMsgID int64, count int) {
	info := &gen.MsgsStateInfo{ReqMsgID: reqMsgID, Info: string(repOnes(count))}
	w := tl.NewWriter(64)
	if err := info.Encode(w); err != nil {
		return
	}
	body := make([]byte, w.Len())
	copy(body, w.Bytes())
	msgID, seqNo := e.ids.assign(false)
	payload := e.frameMessage(msgID, seqNo, body, false)
	envelope, err := mtproto.EncryptMessageData(e.cfg.AuthKey, payload, e.cfg.Rand)
	if err != nil {
		return
	}
	_ = e.tr.Send(e.ctx, envelope)
}

// isUpdateObject reports whether obj belongs to the update family
// (Update* constructors or Updates* wrappers) — the boundary that keeps
// updates out of request correlation.
func isUpdateObject(obj tl.Object) bool {
	switch obj.(type) {
	case gen.UpdateClass, gen.UpdatesClass:
		return true
	}
	return false
}

// mtprotoDecrypt is indirected for fuzz-test seam clarity.
func mtprotoDecrypt(authKey *crypto.AuthKey, body []byte) ([]byte, error) {
	return mtproto.DecryptMessageData(authKey, body)
}

// repOnes builds the reference's chr(1)*n state-info payload.
func repOnes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = 1
	}
	return b
}

var _ = transport.StateConnected

// leU32 reads a little-endian uint32 (ctor peek).
func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
