package tl

import (
	"encoding/binary"
	"fmt"
	"math"
)

// Reader decodes TL wire bytes from an in-memory buffer. It never allocates
// for scalar reads; Bytes/String return slices that alias the input buffer.
//
// The zero value is not useful; construct with NewReader.
type Reader struct {
	buf []byte
	pos int
}

// NewReader returns a Reader over b. The Reader aliases b; the caller must
// not mutate b while decoding.
func NewReader(b []byte) *Reader { return &Reader{buf: b} }

// Reset points the Reader at a new buffer.
func (r *Reader) Reset(b []byte) { r.buf, r.pos = b, 0 }

// Remaining returns the number of unread bytes.
func (r *Reader) Remaining() int { return len(r.buf) - r.pos }

// Unread returns the not-yet-consumed bytes, aliasing the buffer.
func (r *Reader) Unread() []byte { return r.buf[r.pos:] }

// Pos returns the current read offset.
func (r *Reader) Pos() int { return r.pos }

// Seek moves the read offset (may be negative, clamped to the buffer).
func (r *Reader) Seek(off int) { r.pos += off }

// ReadUint8 reads a single byte.
func (r *Reader) ReadUint8() (uint8, error) {
	if r.Remaining() < 1 {
		return 0, r.eof(1)
	}
	v := r.buf[r.pos]
	r.pos++
	return v, nil
}

// ReadUint32 reads a little-endian uint32.
func (r *Reader) ReadUint32() (uint32, error) {
	if r.Remaining() < 4 {
		return 0, r.eof(4)
	}
	v := binary.LittleEndian.Uint32(r.buf[r.pos:])
	r.pos += 4
	return v, nil
}

// ReadInt32 reads a little-endian int32.
func (r *Reader) ReadInt32() (int32, error) {
	v, err := r.ReadUint32()
	return int32(v), err
}

// ReadUint64 reads a little-endian uint64.
func (r *Reader) ReadUint64() (uint64, error) {
	if r.Remaining() < 8 {
		return 0, r.eof(8)
	}
	v := binary.LittleEndian.Uint64(r.buf[r.pos:])
	r.pos += 8
	return v, nil
}

// ReadInt64 reads a little-endian int64.
func (r *Reader) ReadInt64() (int64, error) {
	v, err := r.ReadUint64()
	return int64(v), err
}

// ReadDouble reads an IEEE-754 float64.
func (r *Reader) ReadDouble() (float64, error) {
	v, err := r.ReadUint64()
	return math.Float64frombits(v), err
}

// ReadFloat reads an IEEE-754 float32.
func (r *Reader) ReadFloat() (float32, error) {
	v, err := r.ReadUint32()
	return math.Float32frombits(v), err
}

// ReadRaw reads exactly n bytes, aliasing the input buffer.
func (r *Reader) ReadRaw(n int) ([]byte, error) {
	if n < 0 || r.Remaining() < n {
		return nil, r.eof(n)
	}
	v := r.buf[r.pos : r.pos+n]
	r.pos += n
	return v, nil
}

// ReadBytes reads a length-prefixed byte string, aliasing the input buffer.
// Mirrors the reference implementation's tgread_bytes, including its
// treatment of a 0xFF first byte as a 255-byte short form.
func (r *Reader) ReadBytes() ([]byte, error) {
	first, err := r.ReadUint8()
	if err != nil {
		return nil, err
	}
	var length, pad int
	if first == 254 {
		b1, err := r.ReadUint8()
		if err != nil {
			return nil, err
		}
		b2, err := r.ReadUint8()
		if err != nil {
			return nil, err
		}
		b3, err := r.ReadUint8()
		if err != nil {
			return nil, err
		}
		length = int(b1) | int(b2)<<8 | int(b3)<<16
		pad = length % 4
	} else {
		length = int(first)
		pad = (length + 1) % 4
	}
	data, err := r.ReadRaw(length)
	if err != nil {
		return nil, err
	}
	if pad > 0 {
		if _, err := r.ReadRaw(4 - pad); err != nil {
			return nil, err
		}
	}
	return data, nil
}

// ReadString reads a length-prefixed UTF-8 string. The returned string shares
// memory with the input buffer.
func (r *Reader) ReadString() (string, error) {
	b, err := r.ReadBytes()
	return string(b), err // TODO(perf): unsafe cast once correctness is proven
}

// BoolTrue and BoolFalse are the boxed Bool constructor IDs.
const (
	BoolTrueID  uint32 = 0x997275b5
	BoolFalseID uint32 = 0xbc799737
)

// ReadBoolBoxed reads a boxed Bool (boolTrue/boolFalse constructor).
func (r *Reader) ReadBoolBoxed() (bool, error) {
	id, err := r.ReadUint32()
	if err != nil {
		return false, err
	}
	switch id {
	case BoolTrueID:
		return true, nil
	case BoolFalseID:
		return false, nil
	default:
		return false, fmt.Errorf("%w: %#08x", ErrInvalidBool, id)
	}
}

// ReadVectorBoxedHeader reads the boxed vector constructor and element count.
func (r *Reader) ReadVectorBoxedHeader() (int, error) {
	id, err := r.ReadUint32()
	if err != nil {
		return 0, err
	}
	if id != VectorID {
		return 0, fmt.Errorf("%w: expected vector ctor %#08x, got %#08x", ErrInvalidVector, VectorID, id)
	}
	return r.ReadVectorBareHeader()
}

// ReadVectorBareHeader reads only the element count (bare vector form).
func (r *Reader) ReadVectorBareHeader() (int, error) {
	n, err := r.ReadInt32()
	if err != nil {
		return 0, err
	}
	if n < 0 {
		return 0, fmt.Errorf("%w: negative count %d", ErrInvalidVector, n)
	}
	// Every element consumes at least 4 bytes (even empty objects carry a
	// constructor). Reject counts that cannot possibly fit, so a hostile
	// count cannot spin a huge preallocation.
	if n > int32(r.Remaining()/4) {
		return 0, fmt.Errorf("%w: count %d exceeds remaining %d bytes", ErrInvalidVector, n, r.Remaining())
	}
	return int(n), nil
}

func (r *Reader) eof(want int) error {
	return fmt.Errorf("%w: need %d bytes at offset %d, have %d",
		ErrUnexpectedEOF, want, r.pos, r.Remaining())
}
