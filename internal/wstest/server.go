// Package wstest provides a minimal in-process WebSocket server for
// transport integration and failure-injection tests. It speaks the same
// RFC 6455 subset as the client in package ws over plain TCP (ws://),
// plus knobs to inject failures: dropping connections, truncating frames,
// delaying reads, and sending malformed data.
package wstest

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Server is one fake endpoint. Serve runs in its own goroutine per accepted
// connection; the handler receives a ServerConn.
type Server struct {
	ln      net.Listener
	Handler func(c *ServerConn)

	mu       sync.Mutex
	conns    map[*ServerConn]struct{}
	closed   bool
	Addr     string
	Requests []RequestInfo
}

// RequestInfo records handshake details for assertions (Origin, path, key…).
type RequestInfo struct {
	Path        string
	Origin      string
	Subprotocol string
	Key         string
	Host        string
	UserAgent   string
	Raw         string
}

// New starts listening on 127.0.0.1:0.
func New() (*Server, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	s := &Server{ln: ln, conns: make(map[*ServerConn]struct{})}
	s.Addr = ln.Addr().String()
	go s.acceptLoop()
	return s, nil
}

func (s *Server) acceptLoop() {
	for {
		nc, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(nc)
	}
}

func (s *Server) handle(nc net.Conn) {
	sc := &ServerConn{conn: nc, br: bufio.NewReader(nc)}
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		nc.Close()
		return
	}
	s.conns[sc] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.conns, sc)
		s.mu.Unlock()
		nc.Close()
	}()

	req, err := httpReadRequest(sc.br)
	if err != nil {
		return
	}
	s.mu.Lock()
	s.Requests = append(s.Requests, *req)
	s.mu.Unlock()

	key := strings.TrimSpace(req.Key)
	sum := sha1.Sum([]byte(key + wsGUID))
	accept := base64.StdEncoding.EncodeToString(sum[:])
	fmt.Fprintf(nc, "HTTP/1.1 101 Switching Protocols\r\n"+
		"Upgrade: websocket\r\n"+
		"Connection: Upgrade\r\n"+
		"Sec-WebSocket-Accept: %s\r\n\r\n", accept)

	if s.Handler != nil {
		s.Handler(sc)
	}
}

// Close shuts the listener and all live connections down.
func (s *Server) Close() {
	s.mu.Lock()
	s.closed = true
	conns := make([]*ServerConn, 0, len(s.conns))
	for c := range s.conns {
		conns = append(conns, c)
	}
	s.mu.Unlock()
	s.ln.Close()
	for _, c := range conns {
		c.Close()
	}
}

// httpReadRequest reads one HTTP request using stdlib parsing over the raw
// bufio reader (so the remaining buffered bytes are frame data).
func httpReadRequest(br *bufio.Reader) (*RequestInfo, error) {
	var head []byte
	b := make([]byte, 1)
	for {
		if len(head) > 16*1024 {
			return nil, errors.New("request too large")
		}
		if _, err := br.Read(b); err != nil {
			return nil, err
		}
		head = append(head, b[0])
		if len(head) >= 4 && string(head[len(head)-4:]) == "\r\n\r\n" {
			break
		}
	}
	lines := strings.Split(string(head), "\r\n")
	if len(lines) == 0 || !strings.HasPrefix(lines[0], "GET ") {
		return nil, fmt.Errorf("bad request line %q", lines[0])
	}
	parts := strings.Fields(lines[0])
	info := &RequestInfo{Path: parts[1]}
	for _, line := range lines[1:] {
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			continue
		}
		k := strings.ToLower(strings.TrimSpace(line[:colon]))
		v := strings.TrimSpace(line[colon+1:])
		switch k {
		case "origin":
			info.Origin = v
		case "sec-websocket-protocol":
			info.Subprotocol = v
		case "sec-websocket-key":
			info.Key = v
		case "host":
			info.Host = v
		case "user-agent":
			info.UserAgent = v
		}
	}
	info.Raw = string(head)
	return info, nil
}

// ServerConn is the server side of one connection.
type ServerConn struct {
	conn   net.Conn
	br     *bufio.Reader
	wmu    sync.Mutex
	closed bool
}

// ReadMessage reads the next data message from the client (reassembling
// fragmented frames, unmasking). Control frames are surfaced to the caller.
func (c *ServerConn) ReadMessage() (op byte, payload []byte, err error) {
	var accum []byte
	inMsg := false
	for {
		fin, fop, data, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case 0x0:
			accum = append(accum, data...)
			if fin {
				return 0, accum, nil
			}
		case 0x1, 0x2:
			if fin {
				return fop, data, nil
			}
			inMsg = true
			accum = append(accum[:0], data...)
		case 0x8, 0x9, 0xA:
			return fop, data, nil
		}
		_ = inMsg
	}
}

func (c *ServerConn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	length := uint64(hdr[1] & 0x7F)
	switch length {
	case 126:
		var ext [2]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err = io.ReadFull(c.br, ext[:]); err != nil {
			return false, 0, nil, err
		}
		length = binary.BigEndian.Uint64(ext[:])
	}
	var mask [4]byte
	if masked {
		if _, err = io.ReadFull(c.br, mask[:]); err != nil {
			return false, 0, nil, err
		}
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i&3]
		}
	}
	return fin, op, payload, nil
}

// WriteMessage writes one unmasked frame to the client. A write deadline
// bounds the write so tests that intentionally stall the client (bounded
// inbound queue + no reader) observe an error instead of hanging forever.
func (c *ServerConn) WriteMessage(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("wstest: closed")
	}
	_ = c.conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
	var hdr []byte
	n := len(payload)
	switch {
	case n <= 125:
		hdr = []byte{0x80 | op, byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{0x80 | op, 126, byte(n >> 8), byte(n)}
	default:
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append([]byte{0x80 | op, 127}, ext...)
	}
	buf := append(append([]byte{}, hdr...), payload...)
	_, err := c.conn.Write(buf)
	return err
}

// TruncateHalf writes only half of the requested frame — a partial write.
func (c *ServerConn) TruncateHalf(op byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	frame, err := frameBytes(op, payload)
	if err != nil {
		return err
	}
	_, err = c.conn.Write(frame[:len(frame)/2])
	return err
}

func frameBytes(op byte, payload []byte) ([]byte, error) {
	var hdr []byte
	n := len(payload)
	switch {
	case n <= 125:
		hdr = []byte{0x80 | op, byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{0x80 | op, 126, byte(n >> 8), byte(n)}
	default:
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append([]byte{0x80 | op, 127}, ext...)
	}
	return append(append([]byte{}, hdr...), payload...), nil
}

// DelayRead stalls the connection for d before allowing further reads.
func (c *ServerConn) DelayRead(d time.Duration) { time.Sleep(d) }

// Raw exposes the underlying connection (deadline control in tests).
func (c *ServerConn) Raw() net.Conn { return c.conn }

// Close terminates the TCP connection abruptly.
func (c *ServerConn) Close() {
	c.wmu.Lock()
	c.closed = true
	c.wmu.Unlock()
	c.conn.Close()
}

// SendGracefulClose writes a proper close frame.
func (c *ServerConn) SendGracefulClose(code int) {
	payload := make([]byte, 2)
	binary.BigEndian.PutUint16(payload, uint16(code))
	_ = c.WriteMessage(0x8, payload)
}

// WriteFragment writes one raw frame with an explicit first byte (for
// FIN/opcode control in fragmentation tests).
func (c *ServerConn) WriteFragment(first byte, payload []byte) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	if c.closed {
		return errors.New("wstest: closed")
	}
	n := len(payload)
	var hdr []byte
	switch {
	case n <= 125:
		hdr = []byte{first, byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{first, 126, byte(n >> 8), byte(n)}
	default:
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append([]byte{first, 127}, ext...)
	}
	buf := append(append([]byte{}, hdr...), payload...)
	_, err := c.conn.Write(buf)
	return err
}

// Mu runs f under the server lock (safe access to Requests).
func (s *Server) Mu(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f()
}

// WriteToFirstConn waits up to 2s for a connection and runs f on it.
func (s *Server) WriteToFirstConn(f func(c *ServerConn) error) error {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		var c *ServerConn
		for sc := range s.conns {
			c = sc
			break
		}
		s.mu.Unlock()
		if c != nil {
			return f(c)
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("wstest: no connection within 2s")
}
