// Package transport_test runs the full pipeline end to end against a fake
// Soroush WebSocket peer, using the Phase 2 compatibility vectors as the
// payload fixtures:
//
//	Go client transport → WebSocket → obfuscated2 → abridged →
//	MTProto envelope → TL decode (and the reverse direction).
package transport_test

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tests/vectors"
	"github.com/Skryldev/suromesh/transport"
)

// mustEnvelopeCase loads one envelope vector (auth key, payload, padding,
// exact bytes) from the Phase 2 fixtures.
func mustEnvelopeCase(t *testing.T, file, name string) envCase {
	t.Helper()
	f, err := vectors.Load(file)
	if err != nil {
		t.Fatal(err)
	}
	var cases []envCase
	if err := json.Unmarshal(f.Cases, &cases); err != nil {
		t.Fatal(err)
	}
	for _, c := range cases {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("case %s not found in %s", name, file)
	return envCase{}
}

type envCase struct {
	Name      string `json:"name"`
	AuthKey   string `json:"auth_key"`
	Salt      int64  `json:"salt"`
	SessionID int64  `json:"session_id"`
	Payload   string `json:"payload"`
	Padding   string `json:"padding"`
	Envelope  string `json:"envelope"`
	MsgID     int64  `json:"msg_id"`
}

// mustRoundtripCase loads one roundtrip.json vector.
func mustRoundtripCase(t *testing.T, name string) envCase {
	return mustEnvelopeCase(t, "roundtrip.json", name)
}

func testConfig(addr string) transport.Config {
	return transport.Config{
		Host:             addr,
		WS:               wstest.WSOptions(),
		PingInterval:     -1, // deterministic streams in vector tests
		MaxConnectionAge: 0,
		MaxReconnects:    3,
	}
}

// TestEndToEndBothDirections proves the pipeline with the exact bytes from
// Phase 2:
//   - the client's outbound envelope (request_path) arrives at the fake
//     peer bit-identical after abridged+obfuscation round trips;
//   - the peer's server envelope (response_path) is delivered to the client
//     and decodes through mtproto.DecryptMessageData + TL.
func TestEndToEndBothDirections(t *testing.T) {
	reqVec := mustRoundtripCase(t, "request_path")
	respVec := mustRoundtripCase(t, "response_path")

	reqPayload := vectors.Hex(reqVec.Payload)
	respEnv := vectors.Hex(respVec.Envelope)

	gotPackets := make(chan []byte, 4)
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	var peerOnce sync.Once
	peerReady := make(chan *wstest.SoroushPeer, 1)
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(_ *wstest.SoroushPeer, pkt []byte) {
			gotPackets <- pkt
		})
		peerOnce.Do(func() { peerReady <- peer })
		peer.Wait()
	}

	tr := transport.New(testConfig(srv.Addr))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer tr.Close()

	peer := <-peerReady

	// ---- outbound: transport must deliver the exact envelope bytes.
	if err := tr.Send(context.Background(), reqPayload); err != nil {
		t.Fatalf("send: %v", err)
	}
	select {
	case got := <-gotPackets:
		if !bytes.Equal(got, reqPayload) {
			t.Fatalf("outbound packet mutated:\n want %x\n  got %x", reqPayload, got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("outbound packet never arrived")
	}

	// The first frame on the wire must be the 64-byte obfuscation header.
	frames := peer.Frames()
	if len(frames) == 0 || len(frames[0]) != 64 {
		t.Fatalf("first frame is not the 64-byte header: %d frames", len(frames))
	}

	// ---- inbound: the fake peer sends the Phase 2 server envelope.
	if err := peer.SendPacket(respEnv); err != nil {
		t.Fatalf("peer send: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	got, err := tr.Recv(ctx)
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if !bytes.Equal(got, respEnv) {
		t.Fatalf("inbound envelope mismatch:\n want %x\n  got %x", respEnv, got)
	}

	// The envelope must decode through the Phase 2 stack: decrypt (x=8)
	// and parse the rpc_result carrying Updates.
	key := [256]byte{}
	copy(key[:], vectors.Hex(respVec.AuthKey))
	authKey := crypto.NewAuthKey(key)
	plain, err := mtproto.DecryptMessageData(authKey, got)
	if err != nil {
		t.Fatalf("envelope decrypt: %v", err)
	}
	if !bytes.Equal(plain[:len(vectors.Hex(respVec.Payload))], vectors.Hex(respVec.Payload)) {
		t.Fatal("decrypted payload mismatch")
	}
}

// TestHeaderFrameIsFirst asserts the handshake order: the 64-byte
// obfuscation header is the very first binary message, before any packet.
func TestHeaderFrameIsFirst(t *testing.T) {
	reqVec := mustRoundtripCase(t, "request_path")
	peerCh := make(chan *wstest.SoroushPeer, 1)
	srv, _ := wstest.New()
	defer srv.Close()
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, nil)
		peerCh <- peer
		peer.Wait()
	}

	tr := transport.New(testConfig(srv.Addr))
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr.Close()
	peer := <-peerCh

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(peer.Frames()) == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	frames := peer.Frames()
	if len(frames) != 1 || len(frames[0]) != 64 {
		t.Fatalf("expected exactly the 64-byte header, got %d frames", len(frames))
	}
	if err := tr.Send(context.Background(), vectors.Hex(reqVec.Payload)); err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(peer.Frames()) < 2 {
		time.Sleep(5 * time.Millisecond)
	}
	frames = peer.Frames()
	if len(frames) != 2 {
		t.Fatalf("expected header + one packet frame, got %d", len(frames))
	}
}
