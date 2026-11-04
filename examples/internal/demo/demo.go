// Package demo hosts the in-process fake Soroush Plus server used by the
// runnable examples. DEMO-ONLY: it exists so the examples can execute
// end-to-end without real credentials or a real network. It speaks the real
// protocol stack (obfuscated transport, MTProto encryption, TL) but answers
// only the handful of RPCs the examples use.
//
// This package lives under internal/ on purpose: it is not part of the SDK.
package demo

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"sync"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// responder answers one decrypted client request; returning an *rpc.RPCError
// turns into a server-side RPC error reply.
type responder func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError)

// Server is the demo fake. Create with Start; close with Close.
type Server struct {
	srv *wstest.Server
	key *crypto.AuthKey

	mu        sync.Mutex
	peer      *wstest.SoroushPeer
	responder responder
	lastEnv   wstest.ClientEnvelope
	lastMsgID int64
	msgCount  int64
}

// Start boots the fake server. The client side always connects with a
// locally generated auth key (no DH exchange against the fake, no network).
func Start() (*Server, error) {
	s := &Server{key: demoKey()}
	srv, err := wstest.New()
	if err != nil {
		return nil, err
	}
	s.srv = srv
	// Default responder: ping + the connect-time help.getConfig.
	s.responder = func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		switch ctor {
		case 0x7abe77ec: // ping
			obj, err := tl.Unmarshal(body)
			if err != nil {
				return nil, &rpc.RPCError{Code: 400, Message: "BAD_PING"}
			}
			req, ok := obj.(*gen.PingRequest)
			if !ok {
				return nil, &rpc.RPCError{Code: 400, Message: "BAD_PING"}
			}
			return &gen.Pong{MsgID: 1, PingID: req.PingID}, nil
		case 0xda9b0d0d, 0xc1cd5ea9: // invokeWithLayer / initConnection
			if nestedGetConfig(body) {
				return minimalConfig(), nil
			}
			return nil, &rpc.RPCError{Code: 400, Message: "BAD_INIT_QUERY"}
		}
		return nil, &rpc.RPCError{Code: 400, Message: "METHOD_NOT_IMPLEMENTED_IN_FAKE"}
	}
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, s.onPacket)
		s.mu.Lock()
		s.peer = peer
		s.mu.Unlock()
		peer.Wait()
	}
	return s, nil
}

// SetResponder replaces the RPC dispatch (the previous default is lost;
// chain manually if needed).
func (s *Server) SetResponder(fn responder) {
	s.mu.Lock()
	s.responder = fn
	s.mu.Unlock()
}

// Addr returns the dial address of the fake server.
func (s *Server) Addr() string { return s.srv.Addr }

// NewClient returns a connected client speaking to the fake. The session
// carries a local key and no server-side account state.
func (s *Server) NewClient() (*client.Client, error) {
	return s.NewClientWithSession(session.NewWithKey(s.key), nil)
}

// Key returns the demo auth key. DEMO ONLY: a real client obtains its key
// through the DH exchange during Connect (docs/auth.md); the fake has no
// DH handler, so keyless sessions are seeded with this key instead.
func (s *Server) Key() *crypto.AuthKey { return s.key }

// NewClientWithSession connects a caller-supplied session (e.g. a
// session.FileSession) to the fake. The fake adopts the session's key if
// one is present.
func (s *Server) NewClientWithSession(sess *session.Session, saver interface{ Save() error }) (*client.Client, error) {
	key := sess.AuthKey()
	if key != nil && key.HasValue {
		s.mu.Lock()
		s.key = key
		s.mu.Unlock()
	}
	c, err := client.New(client.Config{
		Session:    sess,
		Saver:      saver,
		Host:       s.srv.Addr,
		TLSEnabled: boolPtr(false),
	})
	if err != nil {
		return nil, err
	}
	if err := c.Connect(context.Background()); err != nil {
		_ = c.Close()
		return nil, err
	}
	return c, nil
}

// PushUpdate delivers an unsolicited update over the real wire path.
func (s *Server) PushUpdate(o tl.Object) error {
	s.mu.Lock()
	peer := s.peer
	s.mu.Unlock()
	if peer == nil {
		return errors.New("demo: no live peer")
	}
	body, err := tl.Marshal(o)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.msgCount += 2
	msgID := s.msgCount
	salt, sess := s.lastEnv.Salt, s.lastEnv.SessionID
	s.mu.Unlock()

	w := tl.NewWriter(32 + len(body))
	w.PutInt64(salt)
	w.PutInt64(sess)
	w.PutInt64(0x40000000 + msgID)
	w.PutInt32(1)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	env, err := mtproto.EncryptServerMessageData(s.key, w.Bytes(), rand.Reader)
	if err != nil {
		return err
	}
	return peer.SendPacket(env)
}

// Close stops the fake server.
func (s *Server) Close() { s.srv.Close() }

// ---- wire plumbing (compact port of the test harness) ---------------------

func (s *Server) onPacket(peer *wstest.SoroushPeer, pkt []byte) {
	env, err := wstest.DecryptClientEnvelope(pkt, s.key)
	if err != nil {
		return
	}
	if len(env.Body) < 4 || wstest.IsServiceBody(env.Body) {
		return
	}
	s.mu.Lock()
	s.lastEnv = env
	if env.MsgID > s.lastMsgID {
		s.lastMsgID = env.MsgID
	}
	s.lastMsgID += 2
	replyMsgID := s.lastMsgID
	fn := s.responder
	s.mu.Unlock()

	result, rpcErr := fn(leU32(env.Body[:4]), env.Body)
	rr := &tl.RpcResult{ReqMsgID: env.MsgID}
	if rpcErr != nil {
		rr.Error = &tl.RpcError{ErrorCode: rpcErr.Code, ErrorMessage: rpcErr.Message}
	} else {
		b, err := tl.Marshal(result)
		if err != nil {
			return
		}
		rr.Body = b
	}
	body, err := tl.Marshal(rr)
	if err != nil {
		return
	}
	w := tl.NewWriter(32 + len(body))
	w.PutInt64(env.Salt)
	w.PutInt64(env.SessionID)
	w.PutInt64(0x40000000 + replyMsgID)
	w.PutInt32(1)
	w.PutInt32(int32(len(body)))
	w.PutRaw(body)
	envelope, err := mtproto.EncryptServerMessageData(s.key, w.Bytes(), rand.Reader)
	if err != nil {
		return
	}
	_ = peer.SendPacket(envelope)
}

func leU32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}

// demoKey builds a deterministic local auth key. DEMO ONLY — a real client
// obtains its key through the DH exchange (docs/auth.md).
func demoKey() *crypto.AuthKey {
	var kb [256]byte
	for i := range kb {
		kb[i] = byte(i*7 + 3)
	}
	return crypto.NewAuthKey(kb)
}

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

func minimalConfig() tl.Object {
	return &gen.Config{
		Date: 1700000000, Expires: 1800000000, ThisDC: 1,
		DCTxtDomainName: "splus.ir", ChatSizeMax: 200,
		MegagroupSizeMax: 200000, ForwardedCountMax: 100,
		OnlineUpdatePeriodMs: 120000, OfflineBlurTimeoutMs: 5000,
		OfflineIdleTimeoutMs: 30000, OnlineCloudTimeoutMs: 300000,
		NotifyCloudDelayMs: 30000, NotifyDefaultDelayMs: 1500,
		PushChatPeriodMs: 60000, PushChatLimit: 20,
		EditTimeLimit: 172800, RevokeTimeLimit: 172800,
		RevokePmTimeLimit: 604800, RatingEDecay: 2419200,
		StickersRecentLimit: 30, ChannelsReadMediaPeriod: 43200,
		MeURLPrefix: "https://splus.ir/", MessageLengthMax: 4096,
		WebfileDCID: 2,
	}
}

func boolPtr(v bool) *bool { return &v }

// Fail prints the error and terminates the example with a non-zero status.
func Fail(stage string, err error) {
	fmt.Printf("demo failed at %s: %v\n", stage, err)
	panic(err)
}
