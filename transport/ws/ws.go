// Package ws implements the RFC 6455 WebSocket client used by the Soroush
// Plus transport.
//
// Compatibility with the reference implementation
// (network/connection/websocket.py, which uses aiohttp):
//
//	URL        wss://{host}/apiws   (ws:// when TLS is disabled)
//	Origin     https://web.splus.ir (required by the server)
//	Subprotocol "binary"
//	Binary frames carry the obfuscated2 byte stream; message boundaries
//	have no packet meaning (the reference re-chunks with readexactly).
//	Heartbeat  application-level WS pings every 30 s (configurable here;
//	          the transport layer owns the timer, this package transports).
//	Max message 16 MiB (aiohttp max_msg_size=2**24); larger inbound messages
//	          are protocol violations.
//
// The implementation is stdlib-only (crypto/tls + net) and client-side only.
package ws

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha1"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// WebSocket GUID from RFC 6455 §1.3.
const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

// Opcodes.
const (
	OpContinuation byte = 0x0
	OpText         byte = 0x1
	OpBinary       byte = 0x2
	OpClose        byte = 0x8
	OpPing         byte = 0x9
	OpPong         byte = 0xA
)

// DefaultMaxMessageSize mirrors aiohttp max_msg_size=2**24.
const DefaultMaxMessageSize = 16 << 20

// Close codes.
const (
	CloseNormal        = 1000
	CloseGoingAway     = 1001
	CloseProtocolError = 1002
	CloseTooBig        = 1009
	CloseAbnormal      = 1006
)

// Errors.
var (
	// ErrClosed is returned after the connection (or its peer) closed.
	ErrClosed = errors.New("ws: connection closed")
	// ErrHandshakeBadStatus / bad headers are handshake failures.
	ErrHandshakeBadStatus = errors.New("ws: handshake: unexpected status")
	// ErrProtocol is returned for RFC 6455 violations from the peer.
	ErrProtocol = errors.New("ws: protocol violation")
	// ErrTooBig is returned when an inbound message exceeds the limit.
	ErrTooBig = errors.New("ws: message too large")
)

// CloseError reports a close frame from the peer.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("ws: closed by peer: code %d %q", e.Code, e.Reason)
}

func (e *CloseError) Is(target error) bool { return target == ErrClosed }

// Options configures the client.
type Options struct {
	Path           string        // default "/apiws"
	Origin         string        // default "https://web.splus.ir"
	Subprotocol    string        // default "binary"
	TLSEnabled     bool          // default true (wss)
	TLSConfig      *tls.Config   // optional
	HandshakeTimeo time.Duration // default 10 s
	MaxMessageSize int           // default 16 MiB
	// ExtraHeaders are appended to the handshake request (rarely needed;
	// Origin and subprotocol are first-class).
	ExtraHeaders [][2]string
}

func (o *Options) fill() {
	if o.Path == "" {
		o.Path = "/apiws"
	}
	if o.Origin == "" {
		o.Origin = "https://web.splus.ir"
	}
	if o.Subprotocol == "" {
		o.Subprotocol = "binary"
	}
	if o.HandshakeTimeo <= 0 {
		o.HandshakeTimeo = 10 * time.Second
	}
	if o.MaxMessageSize <= 0 {
		o.MaxMessageSize = DefaultMaxMessageSize
	}
}

// Conn is an established client connection. ReadMessage must be called from
// a single goroutine (the transport's read loop); writes are safe from
// multiple goroutines but the transport serializes them through its writer
// loop — the internal mutex is a safety net, not a design feature.
type Conn struct {
	raw        net.Conn
	br         *bufio.Reader
	opts       Options
	writeMu    sync.Mutex
	closed     atomic.Bool
	closeOnce  sync.Once
	lastRead   atomic.Int64 // unix nanos
	lastPong   atomic.Int64 // unix nanos
	readerBusy atomic.Bool
}

// Dial establishes the WebSocket connection. The context bounds dial and
// handshake together.
func Dial(ctx context.Context, host string, opts Options) (*Conn, error) {
	opts.fill()

	var (
		raw net.Conn
		err error
	)
	dialer := &net.Dialer{Timeout: opts.HandshakeTimeo}
	if opts.TLSEnabled {
		raw, err = dialer.DialContext(ctx, "tcp", host)
		if err == nil {
			cfg := opts.TLSConfig
			if cfg == nil {
				cfg = &tls.Config{}
			}
			hostname, _, splitErr := net.SplitHostPort(host)
			if splitErr != nil {
				hostname = host
			}
			cfg = cfg.Clone()
			if cfg.ServerName == "" {
				cfg.ServerName = hostname
			}
			tlsConn := tls.Client(raw, cfg)
			tlsConn.SetDeadline(time.Now().Add(opts.HandshakeTimeo))
			if err = tlsConn.HandshakeContext(ctx); err != nil {
				raw.Close()
				return nil, fmt.Errorf("ws: tls handshake: %w", err)
			}
			raw = tlsConn
		}
	} else {
		raw, err = dialer.DialContext(ctx, "tcp", host)
	}
	if err != nil {
		return nil, fmt.Errorf("ws: dial %s: %w", host, err)
	}

	c := &Conn{raw: raw, br: bufio.NewReaderSize(raw, 4096), opts: opts}
	if err := c.handshake(host); err != nil {
		raw.Close()
		return nil, err
	}
	c.lastRead.Store(time.Now().UnixNano())
	c.lastPong.Store(time.Now().UnixNano())
	return c, nil
}

// handshake performs the client opening handshake and validates the
// server's 101 response.
func (c *Conn) handshake(host string) error {
	deadline := time.Now().Add(c.opts.HandshakeTimeo)
	c.raw.SetDeadline(deadline)
	defer c.raw.SetDeadline(time.Time{})

	// 16 random bytes for Sec-WebSocket-Key.
	var keyRaw [16]byte
	if _, err := rand.Read(keyRaw[:]); err != nil {
		return fmt.Errorf("ws: key entropy: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(keyRaw[:])

	hostHeader := host
	if c.opts.TLSEnabled && !strings.Contains(hostHeader, ":") {
		hostHeader = hostHeader + ":443"
	}
	var req bytes.Buffer
	req.WriteString("GET " + c.opts.Path + " HTTP/1.1\r\n")
	req.WriteString("Host: " + hostHeader + "\r\n")
	req.WriteString("Upgrade: websocket\r\n")
	req.WriteString("Connection: Upgrade\r\n")
	req.WriteString("Sec-WebSocket-Key: " + key + "\r\n")
	req.WriteString("Sec-WebSocket-Version: 13\r\n")
	req.WriteString("Sec-WebSocket-Protocol: " + c.opts.Subprotocol + "\r\n")
	if c.opts.Origin != "" {
		req.WriteString("Origin: " + c.opts.Origin + "\r\n")
	}
	for _, kv := range c.opts.ExtraHeaders {
		req.WriteString(kv[0] + ": " + kv[1] + "\r\n")
	}
	req.WriteString("\r\n")
	if _, err := c.raw.Write(req.Bytes()); err != nil {
		return fmt.Errorf("ws: handshake write: %w", err)
	}

	// Read the response headers with a hard bound so a hostile peer cannot
	// stream unbounded data into the header parser.
	httpErr := c.raw.SetReadDeadline(time.Now().Add(c.opts.HandshakeTimeo))
	_ = httpErr
	resp, err := httpReadResponse(c.br)
	if err != nil {
		return fmt.Errorf("ws: handshake response: %w", err)
	}
	if resp.StatusCode != 101 {
		return fmt.Errorf("%w: %s", ErrHandshakeBadStatus, resp.Status)
	}
	if !strings.EqualFold(resp.headerGet("Upgrade"), "websocket") {
		return fmt.Errorf("%w: missing Upgrade header", ErrHandshakeBadStatus)
	}
	if !strings.Contains(strings.ToLower(resp.headerGet("Connection")), "upgrade") {
		return fmt.Errorf("%w: missing Connection upgrade", ErrHandshakeBadStatus)
	}
	accept := resp.headerGet("Sec-WebSocket-Accept")
	sum := sha1.Sum([]byte(key + wsGUID))
	want := base64.StdEncoding.EncodeToString(sum[:])
	if accept != want {
		return fmt.Errorf("%w: bad Sec-WebSocket-Accept", ErrHandshakeBadStatus)
	}
	return nil
}

// httpReadResponse parses a response without consuming frame bytes: it scans
// for the CRLF CRLF terminator with a hard size cap, then parses that block.
func httpReadResponse(br *bufio.Reader) (*httpResponseLite, error) {
	var head []byte
	buf := make([]byte, 1)
	for {
		if len(head) > 32*1024 {
			return nil, errors.New("ws: response header too large")
		}
		if _, err := br.Read(buf); err != nil {
			return nil, err
		}
		head = append(head, buf[0])
		if len(head) >= 4 && bytes.Equal(head[len(head)-4:], []byte("\r\n\r\n")) {
			break
		}
	}
	return parseResponseLite(head)
}

// httpResponseLite is the minimal response representation needed for the
// handshake (stdlib net/http's ReadResponse would also work but drags in
// more machinery than required; this keeps the client tiny and strict).
type httpResponseLite struct {
	Status     string
	StatusCode int
	Header     map[string][]string
}

func parseResponseLite(head []byte) (*httpResponseLite, error) {
	lines := strings.Split(string(head), "\r\n")
	if len(lines) < 1 || !strings.HasPrefix(lines[0], "HTTP/1.1 ") && !strings.HasPrefix(lines[0], "HTTP/1.0 ") {
		return nil, errors.New("ws: malformed status line")
	}
	statusLine := strings.TrimPrefix(lines[0], "HTTP/1.1 ")
	statusLine = strings.TrimPrefix(statusLine, "HTTP/1.0 ")
	code := 0
	for i := 0; i < 3 && i < len(statusLine); i++ {
		if statusLine[i] < '0' || statusLine[i] > '9' {
			break
		}
		code = code*10 + int(statusLine[i]-'0')
	}
	if code == 0 {
		return nil, errors.New("ws: malformed status code")
	}
	hdr := make(map[string][]string)
	for _, line := range lines[1:] {
		if line == "" {
			continue
		}
		colon := strings.IndexByte(line, ':')
		if colon <= 0 {
			return nil, errors.New("ws: malformed header line")
		}
		k := strings.TrimSpace(line[:colon])
		v := strings.TrimSpace(line[colon+1:])
		hdr[httpCanonicalKey(k)] = append(hdr[httpCanonicalKey(k)], v)
	}
	return &httpResponseLite{
		Status:     statusLine,
		StatusCode: code,
		Header:     hdr,
	}, nil
}

func httpCanonicalKey(k string) string {
	return strings.ToLower(k)
}

// headerGet looks up a canonical (lowercase) key.
func (r *httpResponseLite) headerGet(k string) string {
	if vs, ok := r.Header[strings.ToLower(k)]; ok && len(vs) > 0 {
		return vs[0]
	}
	return ""
}

// LastRead returns the time of the last inbound activity.
func (c *Conn) LastRead() time.Time { return time.Unix(0, c.lastRead.Load()) }

// LastPong returns the time of the last pong received.
func (c *Conn) LastPong() time.Time { return time.Unix(0, c.lastPong.Load()) }

// ReadMessage returns the next data message (opcode OpBinary or OpText).
// Control frames are handled internally: pings are answered with pongs,
// pongs update the liveness clock, and a close frame terminates the
// connection with *CloseError (which Is(ErrClosed)).
//
// Fragmented messages are reassembled with the message size enforced during
// accumulation, so a hostile peer cannot grow memory past MaxMessageSize.
func (c *Conn) ReadMessage() (byte, []byte, error) {
	if !c.readerBusy.CompareAndSwap(false, true) {
		return 0, nil, errors.New("ws: ReadMessage called concurrently")
	}
	defer c.readerBusy.Store(false)

	var (
		accum   []byte
		message = false // a data message is in progress
		op      byte
	)
	for {
		fin, fop, payload, err := c.readFrame()
		if err != nil {
			return 0, nil, err
		}
		switch fop {
		case OpPing:
			// Auto-pong (write path is mutex-protected).
			_ = c.writeFrame(OpPong, payload)
			continue
		case OpPong:
			c.lastPong.Store(time.Now().UnixNano())
			continue
		case OpClose:
			code := 0
			reason := ""
			if len(payload) >= 2 {
				code = int(binary.BigEndian.Uint16(payload[:2]))
				reason = string(payload[2:])
			}
			c.closeOnce.Do(func() {
				_ = c.writeFrame(OpClose, nil) // echo close, best effort
				_ = c.raw.Close()
			})
			c.closed.Store(true)
			return 0, nil, &CloseError{Code: code, Reason: reason}
		case OpContinuation:
			if !message {
				return 0, nil, fmt.Errorf("%w: continuation without start", ErrProtocol)
			}
			accum = append(accum, payload...)
			if len(accum) > c.opts.MaxMessageSize {
				return 0, nil, fmt.Errorf("%w: fragmented message", ErrTooBig)
			}
			if fin {
				c.lastRead.Store(time.Now().UnixNano())
				return op, accum, nil
			}
		case OpText, OpBinary:
			if message {
				return 0, nil, fmt.Errorf("%w: nested data frame", ErrProtocol)
			}
			if fin {
				c.lastRead.Store(time.Now().UnixNano())
				return fop, payload, nil
			}
			message = true
			op = fop
			accum = append(accum[:0], payload...)
			if len(accum) > c.opts.MaxMessageSize {
				return 0, nil, fmt.Errorf("%w: fragmented message", ErrTooBig)
			}
		default:
			return 0, nil, fmt.Errorf("%w: opcode %d", ErrProtocol, fop)
		}
	}
}

// readFrame reads exactly one frame.
func (c *Conn) readFrame() (fin bool, op byte, payload []byte, err error) {
	var hdr [2]byte
	if _, err = io.ReadFull(c.br, hdr[:]); err != nil {
		return false, 0, nil, err
	}
	fin = hdr[0]&0x80 != 0
	if hdr[0]&0x70 != 0 {
		return false, 0, nil, fmt.Errorf("%w: reserved bits set", ErrProtocol)
	}
	op = hdr[0] & 0x0F
	masked := hdr[1]&0x80 != 0
	if masked {
		// Server frames must be unmasked (RFC 6455 §5.1).
		return false, 0, nil, fmt.Errorf("%w: server sent masked frame", ErrProtocol)
	}
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
		if length&(1<<63) != 0 {
			return false, 0, nil, fmt.Errorf("%w: length MSB set", ErrProtocol)
		}
	}
	if op >= OpClose { // control frames: 8, 9, 10
		if length > 125 {
			return false, 0, nil, fmt.Errorf("%w: control frame too long", ErrProtocol)
		}
		if !fin {
			return false, 0, nil, fmt.Errorf("%w: fragmented control frame", ErrProtocol)
		}
	}
	if length > uint64(c.opts.MaxMessageSize) {
		return false, 0, nil, fmt.Errorf("%w: frame of %d bytes", ErrTooBig, length)
	}
	payload = make([]byte, length)
	if _, err = io.ReadFull(c.br, payload); err != nil {
		return false, 0, nil, err
	}
	return fin, op, payload, nil
}

// WriteMessage writes one complete data message (single unfragmented frame).
func (c *Conn) WriteMessage(op byte, payload []byte) error {
	if c.closed.Load() {
		return ErrClosed
	}
	return c.writeFrame(op, payload)
}

func (c *Conn) writeFrame(op byte, payload []byte) error {
	if len(payload) > c.opts.MaxMessageSize {
		return fmt.Errorf("%w: outbound message of %d bytes", ErrTooBig, len(payload))
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed.Load() {
		return ErrClosed
	}

	// Header: FIN|opcode, MASK|len(7/16/64), then 4-byte mask.
	n := len(payload)
	var hdr []byte
	switch {
	case n <= 125:
		hdr = []byte{0x80 | op, 0x80 | byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{0x80 | op, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		ext := make([]byte, 8)
		binary.BigEndian.PutUint64(ext, uint64(n))
		hdr = append([]byte{0x80 | op, 0x80 | 127}, ext...)
	}
	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("ws: mask entropy: %w", err)
	}
	buf := make([]byte, 0, len(hdr)+4+n)
	buf = append(buf, hdr...)
	buf = append(buf, mask[:]...)
	buf = append(buf, maskPayload(payload, mask[:])...)
	_, err := c.raw.Write(buf)
	return err
}

// maskPayload XORs payload with the 4-byte mask (RFC 6455 §5.3).
func maskPayload(payload, mask []byte) []byte {
	out := make([]byte, len(payload))
	for i, b := range payload {
		out[i] = b ^ mask[i&3]
	}
	return out
}

// SetReadDeadline bounds the next read (0 clears).
func (c *Conn) SetReadDeadline(t time.Time) error { return c.raw.SetReadDeadline(t) }

// SetWriteDeadline bounds pending writes.
func (c *Conn) SetWriteDeadline(t time.Time) error { return c.raw.SetWriteDeadline(t) }

// Close sends a close frame (best effort) and closes the transport.
func (c *Conn) Close(code int, reason string) error {
	var err error
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		var payload []byte
		if code != 0 {
			payload = make([]byte, 2+len(reason))
			binary.BigEndian.PutUint16(payload, uint16(code))
			copy(payload[2:], reason)
		}
		c.raw.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = c.writeFrame(OpClose, payload)
		err = c.raw.Close()
	})
	return err
}
