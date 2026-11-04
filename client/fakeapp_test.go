package client_test

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// ---- shared lifecycle aliases ----------------------------------------------

const (
	clientStateNew          = client.StateNew
	clientStateDisconnected = client.StateDisconnected
	clientStateConnecting   = client.StateConnecting
	clientStateConnected    = client.StateConnected
	clientStateReady        = client.StateReady
	clientStateClosing      = client.StateClosing
	clientStateClosed       = client.StateClosed
)

func runtime_NumGoroutine() int { return runtime.NumGoroutine() }

// ---- fake app server --------------------------------------------------------

// appFake is the Phase 6 fake application server: it shares the client's
// auth key (pre-seeded session), decrypts client envelopes with x=0 keys,
// correlates msg_id, and answers with server-style envelopes built with
// independent framing math. It supports scripted per-request behavior:
// single results, RPC errors, containers, and delays.
type appFake struct {
	t   testing.TB
	key *crypto.AuthKey

	peerMu sync.Mutex
	peer   *wstest.SoroushPeer

	mu        sync.Mutex
	responder func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError)
	received  []uint32
	delay     time.Duration
	sent      atomicInt
	frameLog  bool // peer frame retention (off for high-volume media tests)
	lastEnv   wstest.ClientEnvelope
	lastMsgID int64

	attachedMedia *mediaFake // set by installMediaHooks (download routing)
}

type atomicInt struct {
	mu sync.Mutex
	v  int
}

func (a *atomicInt) add(n int) int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.v += n
	return a.v
}

type responderFunc func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError)

func wireFakeAppServer(t testing.TB, srv *wstest.Server, key *crypto.AuthKey) *appFake {
	t.Helper()
	fake := &appFake{t: t, key: key, frameLog: true}
	// Default responder: ping → pong; initConnection(getConfig) → Config
	// (the reference's connect-time initialization).
	fake.responder = func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case 0x7abe77ec: // ping
			req, err := decodePing(body)
			if err != nil {
				return nil, &rpc.RPCError{Code: 400, Message: "BAD_PING"}
			}
			return &gen.Pong{MsgID: 1, PingID: req.PingID}, nil
		case 0xda9b0d0d, 0xc1cd5ea9: // invokeWithLayer / initConnection
			if inner := nestedGetConfig(body); inner {
				return minimalConfig(), nil
			}
			return nil, &rpc.RPCError{Code: 400, Message: "BAD_INIT_QUERY"}
		}
		return nil, &rpc.RPCError{Code: 400, Message: "METHOD_NOT_IMPLEMENTED_IN_FAKE"}
	}
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(p *wstest.SoroushPeer, pkt []byte) {
			fake.onPacket(p, pkt)
		})
		fake.peerMu.Lock()
		fake.peer = peer
		fake.peerMu.Unlock()
		fake.mu.Lock()
		retain := fake.frameLog
		fake.mu.Unlock()
		if !retain {
			peer.SetRetainFrames(false)
		}
		peer.Wait()
	}
	return fake
}

// SetFrameLog toggles peer-side frame retention (default on). Media memory
// audits disable it so retained 128 KiB upload frames don't skew results.
func (f *appFake) SetFrameLog(on bool) {
	f.mu.Lock()
	f.frameLog = on
	f.mu.Unlock()
}

func (f *appFake) SetResponder(fn responderFunc) {
	f.mu.Lock()
	f.responder = fn
	f.mu.Unlock()
}

func (f *appFake) SetDelay(d time.Duration) {
	f.mu.Lock()
	f.delay = d
	f.mu.Unlock()
}

func (f *appFake) Received() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]uint32, len(f.received))
	copy(out, f.received)
	return out
}

func decodePing(body []byte) (*gen.PingRequest, error) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, err
	}
	req, ok := obj.(*gen.PingRequest)
	if !ok {
		return nil, fmt.Errorf("not a ping: %T", obj)
	}
	return req, nil
}

// onPacket: decrypt (x=0), correlate, respond (x=8).
func (f *appFake) onPacket(peer *wstest.SoroushPeer, pkt []byte) {
	env, err := wstest.DecryptClientEnvelope(pkt, f.key)
	if err != nil {
		return
	}
	if len(env.Body) < 4 {
		return
	}
	if wstest.IsServiceBody(env.Body) {
		return // msgs_ack/state info: never answered
	}
	f.mu.Lock()
	f.lastEnv = env
	f.mu.Unlock()
	ctor := leU32Test(env.Body[:4])

	f.mu.Lock()
	fn := f.responder
	f.received = append(f.received, ctor)
	delay := f.delay
	f.mu.Unlock()
	n := f.sent.add(1)
	_ = n

	if delay > 0 {
		time.Sleep(delay)
	}

	result, rpcErr := fn(ctor, env.Body)

	rr := &tl.RpcResult{ReqMsgID: env.MsgID}
	if rpcErr != nil {
		rr.Error = &tl.RpcError{ErrorCode: rpcErr.Code, ErrorMessage: rpcErr.Message}
	} else {
		b, err := tl.Marshal(result)
		if err != nil {
			return // test double: drop unmarshalable results silently
		}
		rr.Body = b
	}
	f.reply(peer, env, rr)
}

// reply frames the rpc_result into a TLMessage with the client's
// salt/session and sends it with server keys.
func (f *appFake) reply(peer *wstest.SoroushPeer, env wstest.ClientEnvelope, rr *tl.RpcResult) {
	body, err := tl.Marshal(rr)
	if err != nil {
		return
	}
	f.mu.Lock()
	if env.MsgID > f.lastMsgID {
		f.lastMsgID = env.MsgID
	}
	f.lastMsgID += 2
	msgID := f.lastMsgID
	if msgID%4 != 0 {
		msgID += 2
	}
	f.mu.Unlock()
	w := tl.NewWriter(16 + len(body) + 16)
	w.PutInt64(env.Salt)
	w.PutInt64(env.SessionID)
	w.PutInt64(msgID)
	w.PutInt32(1) // server seq (odd content side is the reference's inbound rule)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	payload := w.Bytes()

	envelope, err := mtproto.EncryptServerMessageData(f.key, payload, rand.Reader)
	if err != nil {
		return
	}
	// Send errors are expected during teardown (peer closed); the fake
	// stays silent so logs never fire after a test completes.
	_ = peer.SendPacket(envelope)
}

func boolPtr(v bool) *bool { return &v }

// leU32Test reads a little-endian uint32.
func leU32Test(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// pingLike is a real protocol request the fake server understands.
type pingLike = gen.PingRequest

var _ = context.Background
var _ = mtproto.PlainMessage
var _ = rpc.ErrConnectionLost
var _ = session.StateNoKey
var _ = wstest.IsServiceBody

// testAuthKeyOf derives a deterministic test auth key.
func testAuthKeyOf(seed byte) *crypto.AuthKey {
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(int(i)*int(seed) + 1)
	}
	return crypto.NewAuthKey(kb)
}

// nestedGetConfig reports whether the request body (invokeWithLayer or
// initConnection framing) ultimately carries help.getConfig.
func nestedGetConfig(body []byte) bool {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return false
	}
	var q tl.Object
	switch r := obj.(type) {
	case *gen.InvokeWithLayerRequest:
		q = r.Query
	case *gen.InitConnectionRequest:
		q = r.Query
	default:
		return false
	}
	switch r := q.(type) {
	case *gen.InitConnectionRequest:
		_, ok := r.Query.(*gen.HelpGetConfigRequest)
		return ok
	case *gen.HelpGetConfigRequest:
		return true
	}
	return false
}

// minimalConfig answers help.getConfig with the smallest valid Config.
func minimalConfig() tl.Object {
	return &gen.Config{
		Date:                    1700000000,
		Expires:                 1800000000,
		TestMode:                false,
		ThisDC:                  1,
		DCTxtDomainName:         "splus.ir",
		ChatSizeMax:             200,
		MegagroupSizeMax:        200000,
		ForwardedCountMax:       100,
		OnlineUpdatePeriodMs:    120000,
		OfflineBlurTimeoutMs:    5000,
		OfflineIdleTimeoutMs:    30000,
		OnlineCloudTimeoutMs:    300000,
		NotifyCloudDelayMs:      30000,
		NotifyDefaultDelayMs:    1500,
		PushChatPeriodMs:        60000,
		PushChatLimit:           20,
		EditTimeLimit:           172800,
		RevokeTimeLimit:         172800,
		RevokePmTimeLimit:       604800,
		RatingEDecay:            2419200,
		StickersRecentLimit:     30,
		ChannelsReadMediaPeriod: 43200,
		MeURLPrefix:             "https://splus.ir/",
		MessageLengthMax:        4096,
		WebfileDCID:             2,
	}
}

// testKey is the lifecycle tests' auth key alias.
func testKey(seed byte) *crypto.AuthKey { return testAuthKeyOf(seed) }

// ---- media fake server ------------------------------------------------------

// mediaFake records uploaded parts and serves deterministic download
// chunks. It handles the media RPC family on top of the shared app server
// infrastructure.
type mediaFake struct {
	mu          sync.Mutex
	parts       map[int64]map[int32][]byte
	partLens    map[int64]map[int32]int
	discardMode bool
	bigParts    map[int64]bool
	fileNames   map[int64]string
	failParts   map[int64]map[int32]bool
	failFrom    map[int64]int32
	chunkFailAt map[int64]int
	getFileN    map[int64]int
	fileData    map[int64][]byte
	synth       map[int64]int64 // locID → logical size served by pattern
}

func newMediaFake() *mediaFake {
	return &mediaFake{
		parts:       make(map[int64]map[int32][]byte),
		partLens:    make(map[int64]map[int32]int),
		bigParts:    make(map[int64]bool),
		fileNames:   make(map[int64]string),
		failParts:   make(map[int64]map[int32]bool),
		failFrom:    make(map[int64]int32),
		chunkFailAt: make(map[int64]int),
		getFileN:    make(map[int64]int),
		fileData:    make(map[int64][]byte),
		synth:       make(map[int64]int64),
	}
}

// enableDiscard makes the fake record part lengths only — no content is
// retained server-side, so client-side memory audits stay meaningful for
// very large uploads.
func (f *mediaFake) enableDiscard() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.discardMode = true
}

// setSynth serves `size` bytes of the deterministic pattern for locID
// without ever materializing them.
func (f *mediaFake) setSynth(locID int64, size int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.synth[locID] = size
}

// uploadedLen returns the total bytes accepted for a file (discard mode).
func (f *mediaFake) uploadedLen(fileID int64) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	pl := f.partLens[fileID]
	if pl == nil {
		return 0
	}
	var total int64
	for _, n := range pl {
		total += int64(n)
	}
	return total
}

// uploadedParts reports how many distinct parts were accepted for a file.
func (f *mediaFake) uploadedParts(fileID int64) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.partLens[fileID])
}

func (f *mediaFake) uploadedFile(fileID int64, parts int32) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	pm := f.parts[fileID]
	if pm == nil {
		return nil
	}
	buf := make([]byte, 0, int(parts)*512*1024)
	for i := int32(0); i < parts; i++ {
		buf = append(buf, pm[i]...)
	}
	return buf
}

func (f *mediaFake) handleMediaRPC(t testing.TB, body []byte) (tl.Object, *rpc.RPCError) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
	}
	switch req := obj.(type) {
	case *gen.UploadSaveFilePartRequest:
		return f.handleSavePart(t, req.FileID, req.FilePart, 0, req.Bytes)
	case *gen.UploadSaveBigFilePartRequest:
		return f.handleSavePart(t, req.FileID, req.FilePart, req.FileTotalParts, req.Bytes)
	case *gen.UploadGetFileRequest:
		return f.handleGetFile(t, req)
	default:
		return nil, &rpc.RPCError{Code: 400, Message: "UNKNOWN_MEDIA_METHOD"}
	}
}

func (f *mediaFake) handleSavePart(t testing.TB, fileID int64, partIdx, totalParts int32, data []byte) (tl.Object, *rpc.RPCError) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if fp := f.failParts[fileID]; fp != nil && fp[partIdx] {
		return nil, &rpc.RPCError{Code: 400, Message: "FILE_PART_INVALID"}
	}
	if from, ok := f.failFrom[fileID]; ok && partIdx >= from {
		return nil, &rpc.RPCError{Code: 500, Message: "INTERNAL"}
	}
	if f.partLens[fileID] == nil {
		f.partLens[fileID] = make(map[int32]int)
	}
	f.partLens[fileID][partIdx] = len(data)
	if f.discardMode {
		// Length accounting only: no server-side copy for memory audits.
		if totalParts > 0 {
			f.bigParts[fileID] = true
		}
		return &gen.BoolTrue{}, nil
	}
	if f.parts[fileID] == nil {
		f.parts[fileID] = make(map[int32][]byte)
	}
	cp := make([]byte, len(data))
	copy(cp, data)
	f.parts[fileID][partIdx] = cp
	if totalParts > 0 {
		f.bigParts[fileID] = true
	}
	return &gen.BoolTrue{}, nil
}

func (f *mediaFake) handleGetFile(t testing.TB, req *gen.UploadGetFileRequest) (tl.Object, *rpc.RPCError) {
	loc := req.Location
	var locID int64
	switch l := loc.(type) {
	case *gen.InputDocumentFileLocation:
		locID = l.ID
	case *gen.InputFileLocation:
		locID = l.VolumeID
	default:
		return nil, &rpc.RPCError{Code: 400, Message: "LOCATION_INVALID"}
	}
	f.mu.Lock()
	f.getFileN[locID]++
	n := f.getFileN[locID]
	failN, hasFail := f.chunkFailAt[locID]
	data := f.fileData[locID]
	synthSize := f.synth[locID]
	f.mu.Unlock()
	if hasFail && n == failN {
		return nil, &rpc.RPCError{Code: 500, Message: "INTERNAL"}
	}
	offset := req.Offset
	limit := int(req.Limit)
	if limit <= 0 {
		return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
	}
	// Synthetic mode: generate pattern bytes without a stored buffer.
	if synthSize > 0 {
		if offset < 0 || offset >= synthSize {
			return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
		}
		end := offset + int64(limit)
		if end > synthSize {
			end = synthSize
		}
		chunk := make([]byte, end-offset)
		for i := range chunk {
			chunk[i] = patternByte(offset + int64(i)) //nolint:staticcheck — test double
		}
		return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Mtime: 1700000000, Bytes: chunk}, nil
	}
	if offset < 0 || offset > int64(len(data)) {
		return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
	}
	end := int(offset) + limit
	if end > len(data) {
		end = len(data)
	}
	chunk := data[offset:end]
	if len(chunk) == 0 {
		return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Bytes: []byte{}}, nil
	}
	return &gen.UploadFile{Type_: &gen.StorageFileJpeg{}, Mtime: 1700000000, Bytes: chunk}, nil
}

func (f *mediaFake) setDownloadData(locID int64, data []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fileData[locID] = data
}

func (f *mediaFake) failChunkAt(locID int64, n int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.chunkFailAt[locID] = n
}

// installMediaHooks extends the fake app server's default responder.
func installMediaHooks(t testing.TB, fake *appFake, mf *mediaFake) {
	fake.mu.Lock()
	fake.attachedMedia = mf
	fake.mu.Unlock()
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case 0xb304a621, 0xde7b673d, 0xbe5335be:
			return mf.handleMediaRPC(t, body)
		default:
			if prev != nil {
				return prev(ctor, body)
			}
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
		}
	})
}

func mediaDocLocation(docID, accessHash int64) *gen.InputDocumentFileLocation {
	return &gen.InputDocumentFileLocation{ID: docID, AccessHash: accessHash, ThumbSize: ""}
}

// deterministicReader produces n bytes of a deterministic pattern without
// allocating n bytes of memory.
type deterministicReader struct {
	remaining int64
	pos       int64
}

func newDeterministicReader(n int64) *deterministicReader {
	return &deterministicReader{remaining: n}
}

func (r *deterministicReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := int64(0); i < n; i++ {
		p[i] = byte((r.pos + i) % 251)
	}
	r.remaining -= n
	r.pos += n
	return int(n), nil
}

// resetCounter clears the getFile call counter for a location so failure
// injection can target a fresh download.
func (f *mediaFake) resetCounter(locID int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.getFileN, locID)
}

// PushUpdate sends an unsolicited update to the client over the live
// connection (server-side update push — the normal MTProto flow). Fails
// silently when no connection is live (test double).
func (f *appFake) PushUpdateErr(obj tl.Object) error {
	f.peerMu.Lock()
	peer := f.peer
	f.peerMu.Unlock()
	if peer == nil {
		return errors.New("no live peer")
	}
	body, err := tl.Marshal(obj)
	if err != nil {
		return err
	}
	w := tl.NewWriter(16 + len(body) + 16)
	msgID := f.nextServerMsgID()
	salt, sess := f.keySalt(), f.keySession()
	w.PutInt64(salt)
	w.PutInt64(sess)
	w.PutInt64(msgID)
	w.PutInt32(1)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	envelope, err := mtproto.EncryptServerMessageData(f.key, w.Bytes(), rand.Reader)
	if err != nil {
		return err
	}
	return peer.SendPacket(envelope)
}

// PushUpdate sends an unsolicited update, ignoring errors.
func (f *appFake) PushUpdate(obj tl.Object) { _ = f.PushUpdateErr(obj) }

// keySalt/keySession return the last observed client salt/session so the
// pushed message uses the client's current values (locked: onPacket
// writes lastEnv concurrently).
func (f *appFake) keySalt() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnv.Salt
}

func (f *appFake) keySession() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastEnv.SessionID
}

func (f *appFake) nextServerMsgID() int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.lastMsgID += 2
	if f.lastMsgID%4 != 0 {
		f.lastMsgID += 2
	}
	return f.lastMsgID
}
