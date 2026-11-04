package wstest

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/transport/abridged"
	"github.com/Skryldev/suromesh/transport/ws"
)

// WSOptions returns client options for connecting to a wstest server
// (plain TCP, no TLS).
func WSOptions() ws.Options {
	return ws.Options{TLSEnabled: false}
}

// SoroushPeer is a test double of the server side of the Soroush transport:
// it consumes the client's 64-byte obfuscation header, then exchanges
// packet-level data over the same framing the real server uses
// (obfuscated2 AES-CTR + abridged), answering WebSocket pings with pongs.
//
// It exists so transport tests can assert packet equality (and raw WS-frame
// equality, via Frames) without any network dependency.
type SoroushPeer struct {
	conn    *ServerConn
	streams *crypto.ObfuscatedStream
	dec     *abridged.Decoder

	mu           sync.Mutex
	frames       [][]byte // every binary frame received, header first
	retainFrames bool     // frame log for assertions; off for high-volume media tests
	done         chan struct{}
	once         sync.Once
}

// ErrNoHeader is returned when the first binary message is not a valid
// 64-byte obfuscation header.
var ErrNoHeader = errors.New("wstest: first message was not a 64-byte header")

// HandleSoroush runs the peer loop for one accepted connection in a
// background goroutine. Packets decoded from the client are delivered to
// onPacket (called on the peer's loop goroutine, with the loop's own peer
// so responses always route to the live connection). The returned peer is
// usable from other goroutines for SendPacket/Frames/Wait; the Handler
// should call Wait so the server-side connection stays open.
func HandleSoroush(conn *ServerConn, onPacket func(p *SoroushPeer, pkt []byte)) *SoroushPeer {
	p := &SoroushPeer{conn: conn, dec: abridged.NewDecoder(0), done: make(chan struct{}), retainFrames: true}
	go p.loop(onPacket)
	return p
}

// HandleSoroushNoFrames is HandleSoroush without the per-connection frame
// log. High-volume media tests use it so retaining every ~128 KiB upload
// frame doesn't masquerade as a client-side memory leak.
func HandleSoroushNoFrames(conn *ServerConn, onPacket func(p *SoroushPeer, pkt []byte)) *SoroushPeer {
	p := &SoroushPeer{conn: conn, dec: abridged.NewDecoder(0), done: make(chan struct{})}
	go p.loop(onPacket)
	return p
}

// SetRetainFrames toggles the frame log at runtime. Safe to call before
// the first frame arrives; a peer created by HandleSoroushNoFrames cannot
// re-enable retention.
func (p *SoroushPeer) SetRetainFrames(on bool) {
	p.mu.Lock()
	p.retainFrames = on
	p.mu.Unlock()
}

// Wait blocks until the peer loop ends (connection closed or dropped).
func (p *SoroushPeer) Wait() { <-p.done }

func (p *SoroushPeer) loop(onPacket func(p *SoroushPeer, pkt []byte)) {
	defer p.once.Do(func() { close(p.done) })
	for {
		op, msg, err := p.conn.ReadMessage()
		if err != nil {
			return
		}
		switch op {
		case 0x9: // ping → pong
			if err := p.conn.WriteMessage(0xA, msg); err != nil {
				return
			}
		case 0x1, 0x2: // data
			p.mu.Lock()
			if p.streams == nil {
				if p.retainFrames {
					p.frames = append(p.frames, msg)
				}
				p.mu.Unlock()
				streams, err := crypto.StreamsFromHeader(msg)
				if err != nil {
					return
				}
				p.mu.Lock()
				p.streams = streams
				p.mu.Unlock()
				continue
			}
			if p.retainFrames {
				p.frames = append(p.frames, msg)
			}
			p.mu.Unlock()

			clearBytes := p.streams.Decrypt(msg)
			pkts, err := p.dec.Feed(clearBytes, nil)
			if err != nil {
				return // client sent garbage framing; drop connection
			}
			for _, pkt := range pkts {
				if onPacket != nil {
					owned := append(make([]byte, 0, len(pkt)), pkt...)
					onPacket(p, owned)
				}
			}
		case 0x8: // client close
			return
		}
	}
}

// SendPacket frames one packet back to the client (abridged + obfuscated).
// Atomic across encrypt+write: the obfuscation keystream is rolling, so
// concurrent sends must serialize or the client decrypts garbage.
func (p *SoroushPeer) SendPacket(pkt []byte) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	streams := p.streams
	if streams == nil {
		return errors.New("wstest: header not yet received")
	}
	frame, err := abridged.Encode(nil, pkt)
	if err != nil {
		return err
	}
	return p.conn.WriteMessage(0x2, streams.Encrypt(frame))
}

// Frames returns a copy of every binary frame received so far (the 64-byte
// header first).
func (p *SoroushPeer) Frames() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([][]byte, len(p.frames))
	copy(out, p.frames)
	return out
}

// HasHeader reports whether the obfuscation header arrived.
func (p *SoroushPeer) HasHeader() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.streams != nil
}

// Close drops the TCP connection.
func (p *SoroushPeer) Close() { p.conn.Close() }

// Raw exposes the server-side connection for failure injection.
func (p *SoroushPeer) Raw() *ServerConn { return p.conn }

// ClientEnvelope is a decrypted client packet with its MTProto framing.
type ClientEnvelope struct {
	Salt      int64
	SessionID int64
	MsgID     int64
	SeqNo     int32
	Body      []byte
}

// DecryptClientEnvelope opens a client->server envelope with the client key
// derivation (x=0), exactly as the reference server would, and parses the
// first TLMessage framing.
func DecryptClientEnvelope(pkt []byte, authKey *crypto.AuthKey) (ClientEnvelope, error) {
	var mk [16]byte
	copy(mk[:], pkt[8:24])
	aesKey, aesIV := mtproto.CalcAESKeyIV(&authKey.Key, mk, true)
	plain, err := crypto.DecryptIGE(pkt[24:], aesKey[:], aesIV[:])
	if err != nil {
		return ClientEnvelope{}, err
	}
	if len(plain) < 16 {
		return ClientEnvelope{}, errors.New("wstest: payload too short")
	}
	salt := int64(binary.LittleEndian.Uint64(plain[0:8]))
	session := int64(binary.LittleEndian.Uint64(plain[8:16]))
	r := tl.NewReader(plain[16:])
	msgID, err := r.ReadInt64()
	if err != nil {
		return ClientEnvelope{}, err
	}
	seqNo, err := r.ReadInt32()
	if err != nil {
		return ClientEnvelope{}, err
	}
	length, err := r.ReadInt32()
	if err != nil {
		return ClientEnvelope{}, err
	}
	body, err := r.ReadRaw(int(length))
	if err != nil {
		return ClientEnvelope{}, err
	}
	return ClientEnvelope{
		Salt: salt, SessionID: session, MsgID: msgID, SeqNo: seqNo, Body: body,
	}, nil
}

// SendServerEnvelope encrypts a plaintext payload (salt|session|messages)
// with server keys and frames it to the client.
func (p *SoroushPeer) SendServerEnvelope(authKey *crypto.AuthKey, payload []byte, rnd io.Reader) error {
	envelope, err := mtproto.EncryptServerMessageData(authKey, payload, rnd)
	if err != nil {
		return err
	}
	return p.SendPacket(envelope)
}

// IsServiceBody reports whether a decrypted TL message body is a
// service/acknowledgement message rather than an RPC request. Test doubles
// use it to avoid replying to acknowledgements (which would create
// ACK→reply→ACK loops).
func IsServiceBody(body []byte) bool {
	if len(body) < 4 {
		return false
	}
	switch binary.LittleEndian.Uint32(body[:4]) {
	case msgsAckCtor, msgsStateInfoCtor, msgsAllInfoCtor, msgDetailedInfoCtor,
		msgNewDetailedInfoCtor:
		return true
	}
	return false
}

// Service-message constructor IDs (mtproto.tl).
const (
	msgsAckCtor            uint32 = 0x62d6b459
	msgsStateInfoCtor      uint32 = 0x04deb57d
	msgsAllInfoCtor        uint32 = 0x8cc0d131
	msgDetailedInfoCtor    uint32 = 0x276d3ec6
	msgNewDetailedInfoCtor uint32 = 0x809db6df
)
