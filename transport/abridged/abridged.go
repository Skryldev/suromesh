// Package abridged implements the MTProto "abridged" packet codec — the
// length-framing layer that sits directly above the obfuscated2 byte stream
// on the Soroush WebSocket transport.
//
// Wire format (verified against network/connection/tcpabridged.py):
//
//	encode: l = len(payload)/4; l < 127  → [l byte] + payload
//	        l >= 127        → [0x7F][l:3 LE] + payload
//	decode: read one byte; if >= 127 read three more (LE u24); payload = n*4
//
// Payloads must be a multiple of 4 bytes (MTProto envelopes are padded to
// 16, so this always holds). Zero-length packets are legal on the wire and
// skipped, mirroring the reference's recv loop (`if result:`).
//
// The decoder is streaming: WebSocket message boundaries carry no
// relationship to packet boundaries (the reference de-chunks across frames
// with readexactly), so arbitrary fragmentation must reconstruct packets.
package abridged

import (
	"errors"
	"fmt"
)

// DefaultMaxPacketSize mirrors the reference's 16 MiB WebSocket message
// bound (aiohttp max_msg_size=2**24).
const DefaultMaxPacketSize = 16 << 20

// Errors.
var (
	// ErrTooLarge is returned when a packet length prefix would exceed the
	// configured maximum. Hostile prefixes are rejected before allocation.
	ErrTooLarge = errors.New("abridged: packet exceeds maximum size")
	// ErrBadLength is returned when a length prefix is structurally
	// impossible (payload not a multiple of 4 after decoding).
	ErrBadLength = errors.New("abridged: invalid packet length")
	// ErrTruncated is returned when the stream ends mid-packet.
	ErrTruncated = errors.New("abridged: stream ended mid-packet")
)

// Encode frames one payload packet. The payload length must be a multiple
// of 4 and within MaxPacketSize; violations return an error rather than
// producing a corrupt frame.
func Encode(dst []byte, payload []byte) ([]byte, error) {
	if len(payload)%4 != 0 {
		return dst, fmt.Errorf("%w: payload %d not a multiple of 4", ErrBadLength, len(payload))
	}
	l := len(payload) >> 2
	if l < 127 {
		dst = append(dst, byte(l))
	} else if l <= 0xFFFFFF {
		dst = append(dst, 0x7F, byte(l), byte(l>>8), byte(l>>16))
	} else {
		return dst, fmt.Errorf("%w: %d bytes", ErrTooLarge, len(payload))
	}
	return append(dst, payload...), nil
}

// Decoder reassembles packets from an arbitrary fragment stream.
//
// Feed consumes network bytes and appends complete packets to dst (reusing
// backing capacity across calls). Internal buffering is bounded by
// MaxPacketSize: a hostile length prefix is rejected before allocation and
// never grows memory beyond one maximum packet.
type Decoder struct {
	buf          []byte // pending bytes (header and/or partial payload)
	maxPacket    int
	maxPending   int
	packetsSeen  uint64
	bytesDropped uint64 // zero-length packets skipped
}

// NewDecoder returns a decoder with the given packet size bound (0 selects
// DefaultMaxPacketSize).
func NewDecoder(maxPacket int) *Decoder {
	if maxPacket <= 0 {
		maxPacket = DefaultMaxPacketSize
	}
	return &Decoder{maxPacket: maxPacket, maxPending: maxPacket + 4}
}

// MaxPacket reports the configured packet size bound.
func (d *Decoder) MaxPacket() int { return d.maxPacket }

// PacketsSeen reports how many packets have been decoded (zero-length
// packets included in bytesDropped instead).
func (d *Decoder) PacketsSeen() uint64 { return d.packetsSeen }

// Feed appends network bytes and returns the newly completed packets
// appended to dst. The returned slice aliases Decoder-internal buffers only
// until the next Feed call; consumers must copy if retaining.
func (d *Decoder) Feed(chunk []byte, dst [][]byte) ([][]byte, error) {
	if len(d.buf) == 0 {
		d.buf = append(d.buf, chunk...)
	} else {
		d.buf = append(d.buf, chunk...)
	}
	if len(d.buf) > d.maxPending {
		return dst, fmt.Errorf("%w: %d bytes buffered", ErrTooLarge, len(d.buf))
	}
	for {
		packet, err := d.next()
		if err != nil {
			return dst, err
		}
		if packet == nil {
			return dst, nil // need more bytes
		}
		if len(packet) == 0 {
			// Zero-length packet: legal, skipped like the reference recv.
			d.bytesDropped++
			continue
		}
		d.packetsSeen++
		dst = append(dst, packet)
	}
}

// next decodes one packet from the head of buf, or returns nil when more
// bytes are needed.
func (d *Decoder) next() ([]byte, error) {
	if len(d.buf) == 0 {
		return nil, nil
	}
	first := d.buf[0]
	var words int
	hdr := 1
	if first < 127 {
		words = int(first)
	} else {
		if len(d.buf) < 4 {
			return nil, nil
		}
		words = int(d.buf[1]) | int(d.buf[2])<<8 | int(d.buf[3])<<16
		hdr = 4
	}
	size := words << 2
	if words != 0 && size>>2 != words { // u32 overflow guard (unreachable below max, kept for safety)
		return nil, fmt.Errorf("%w: length overflow", ErrBadLength)
	}
	if size > d.maxPacket {
		return nil, fmt.Errorf("%w: %d bytes", ErrTooLarge, size)
	}
	if len(d.buf) < hdr+size {
		return nil, nil
	}
	packet := d.buf[hdr : hdr+size]
	d.buf = d.buf[hdr+size:]
	// Compact occasionally to keep buf bounded without per-packet copies.
	if len(d.buf) == 0 {
		d.buf = d.buf[:0]
	} else if len(d.buf) < cap(d.buf)/4 && cap(d.buf) > 4096 {
		n := copy(d.buf, d.buf)
		d.buf = d.buf[:n]
	}
	return packet, nil
}

// Reset drops all pending state (used when a connection is replaced).
func (d *Decoder) Reset() { d.buf = d.buf[:0] }
