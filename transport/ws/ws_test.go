package ws_test

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/transport/ws"
)

func dialTest(t *testing.T, srv *wstest.Server) *ws.Conn {
	t.Helper()
	conn, err := ws.Dial(context.Background(), srv.Addr, ws.Options{TLSEnabled: false})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	return conn
}

func TestHandshakeRequirements(t *testing.T) {
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	conn := dialTest(t, connCapture(t, srv))
	defer conn.Close(0, "")
	time.Sleep(100 * time.Millisecond)

	srv.Mu(func() {
		if len(srv.Requests) != 1 {
			t.Fatalf("requests %d", len(srv.Requests))
		}
		req := srv.Requests[0]
		if req.Path != "/apiws" {
			t.Fatalf("path %q", req.Path)
		}
		if req.Origin != "https://web.splus.ir" {
			t.Fatalf("origin %q", req.Origin)
		}
		if req.Subprotocol != "binary" {
			t.Fatalf("subprotocol %q", req.Subprotocol)
		}
		if req.Key == "" {
			t.Fatal("missing Sec-WebSocket-Key")
		}
	})
}

// connHold keeps the server-side handler alive until the client goes away.
func connHold(c *wstest.ServerConn) {
	for {
		if _, _, err := c.ReadMessage(); err != nil {
			return
		}
	}
}

// connCapture installs a hold-open handler while keeping the server usable.
func connCapture(t *testing.T, srv *wstest.Server) *wstest.Server {
	t.Helper()
	srv.Handler = connHold
	return srv
}

func TestDataRoundTrip(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	ready := make(chan *wstest.ServerConn, 1)
	srv.Handler = func(c *wstest.ServerConn) {
		ready <- c
		for {
			op, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if op == 0x2 {
				_ = c.WriteMessage(0x2, msg) // echo
			}
		}
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")
	sconn := <-ready
	_ = sconn

	payload := make([]byte, 200_000) // exercises the 64-bit length path
	for i := range payload {
		payload[i] = byte(i)
	}
	if err := conn.WriteMessage(ws.OpBinary, payload); err != nil {
		t.Fatal(err)
	}
	op, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if op != ws.OpBinary || string(got) != string(payload) {
		t.Fatalf("echo mismatch: op=%d len=%d", op, len(got))
	}
}

func TestFragmentedMessageReassembly(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	ready := make(chan *wstest.ServerConn, 1)
	srv.Handler = func(c *wstest.ServerConn) {
		ready <- c
		connHold(c)
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")
	sconn := <-ready
	_ = sconn

	// Server sends one logical message as three fragments.
	parts := [][]byte{[]byte("hello "), []byte("fragmented "), []byte("world")}
	for i, p := range parts {
		fin := byte(0)
		if i == len(parts)-1 {
			fin = 0x80
		}
		opcode := byte(0x0)
		if i == 0 {
			opcode = 0x2
		}
		// Write raw frame via WriteMessage? WriteMessage always sets FIN;
		// use raw frame building through the server's low-level write.
		if err := sconn.WriteFragment(fin|opcode, p); err != nil {
			t.Fatal(err)
		}
	}
	op, got, err := conn.ReadMessage()
	if err != nil {
		t.Fatal(err)
	}
	if op != ws.OpBinary || string(got) != "hello fragmented world" {
		t.Fatalf("reassembly got %q", got)
	}
}

func TestPingAnsweredWithPong(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	gotPong := make(chan []byte, 1)
	srv.Handler = func(c *wstest.ServerConn) {
		for {
			op, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if op == 0xA {
				gotPong <- msg
				return
			}
		}
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")

	// Pongs are auto-sent from the read path; run it.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	if err := srv.WriteToFirstConn(func(c *wstest.ServerConn) error {
		return c.WriteMessage(ws.OpPing, []byte("hb"))
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case msg := <-gotPong:
		if string(msg) != "hb" {
			t.Fatalf("pong payload %q", msg)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no pong within 2s")
	}
}

func TestCloseByPeer(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	srv.Handler = func(c *wstest.ServerConn) {
		time.Sleep(50 * time.Millisecond)
		c.SendGracefulClose(1000)
		connHold(c)
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")

	_, _, err := conn.ReadMessage()
	if err == nil {
		t.Fatal("expected close error")
	}
	var ce *ws.CloseError
	if !errors.As(err, &ce) || ce.Code != 1000 {
		t.Fatalf("expected CloseError 1000, got %v", err)
	}
	if !errors.Is(err, ws.ErrClosed) {
		t.Fatal("CloseError must satisfy Is(ErrClosed)")
	}
}

func TestOversizedInboundMessageRejected(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	srv.Handler = func(c *wstest.ServerConn) {
		big := make([]byte, 17<<20) // > 16 MiB default bound
		_ = c.WriteMessage(0x2, big)
		connHold(c)
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")

	_, _, err := conn.ReadMessage()
	if !errors.Is(err, ws.ErrTooBig) {
		t.Fatalf("expected ErrTooBig, got %v", err)
	}
}

func TestMalformedHeaderRejected(t *testing.T) {
	// Raw TCP: respond 101 then send a frame with reserved bits set.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, _ := ln.Accept()
		if nc == nil {
			return
		}
		defer nc.Close()
		// Minimal valid handshake echo.
		buf := make([]byte, 4096)
		n, _ := nc.Read(buf)
		req := string(buf[:n])
		key := keyFromRequest(req)
		nc.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + acceptKey(key) + "\r\n\r\n"))
		// 0x82 would be binary; 0x92 sets an RSV bit.
		nc.Write([]byte{0x92, 0x00})
		time.Sleep(500 * time.Millisecond)
	}()

	conn, err := ws.Dial(context.Background(), ln.Addr().String(), ws.Options{TLSEnabled: false})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(0, "")
	_, _, err = conn.ReadMessage()
	if !errors.Is(err, ws.ErrProtocol) {
		t.Fatalf("expected protocol error, got %v", err)
	}
}

func TestHandshakeBadStatus(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		nc, _ := ln.Accept()
		if nc == nil {
			return
		}
		defer nc.Close()
		nc.Write([]byte("HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n"))
		time.Sleep(200 * time.Millisecond)
	}()

	_, err = ws.Dial(context.Background(), ln.Addr().String(), ws.Options{TLSEnabled: false})
	if !errors.Is(err, ws.ErrHandshakeBadStatus) {
		t.Fatalf("expected bad status, got %v", err)
	}
	if !strings.Contains(err.Error(), "403") {
		t.Fatalf("status detail missing: %v", err)
	}
}

func TestConcurrentWritesAreSafe(t *testing.T) {
	srv, _ := wstest.New()
	defer srv.Close()

	srv.Handler = func(c *wstest.ServerConn) {
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}
	conn := dialTest(t, srv)
	defer conn.Close(0, "")

	done := make(chan struct{}, 8)
	for i := 0; i < 8; i++ {
		go func() {
			defer func() { done <- struct{}{} }()
			for j := 0; j < 50; j++ {
				if err := conn.WriteMessage(ws.OpBinary, []byte("concurrent")); err != nil {
					return
				}
			}
		}()
	}
	for i := 0; i < 8; i++ {
		<-done
	}
}

// --- helpers -------------------------------------------------------------

func keyFromRequest(req string) string {
	for _, line := range strings.Split(req, "\r\n") {
		if strings.HasPrefix(strings.ToLower(line), "sec-websocket-key:") {
			return strings.TrimSpace(line[len("sec-websocket-key:"):])
		}
	}
	return ""
}

func acceptKey(key string) string {
	h := sha1.Sum([]byte(key + "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"))
	return base64.StdEncoding.EncodeToString(h[:])
}
