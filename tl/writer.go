package tl

import (
	"encoding/binary"
	"math"
)

// Writer accumulates TL wire bytes. It grows its backing array as needed;
// reuse a Writer across encodes via Reset to amortize allocations.
//
// The zero value is ready to use.
type Writer struct {
	buf []byte
	err error
}

// NewWriter returns a Writer with an initial capacity hint of n bytes.
func NewWriter(n int) *Writer {
	if n < 0 {
		n = 0
	}
	return &Writer{buf: make([]byte, 0, n)}
}

// Wrap returns a Writer that appends to dst. Cap for size, len for position.
func Wrap(dst []byte) *Writer { return &Writer{buf: dst} }

// Reset empties the buffer, retaining capacity for reuse.
func (w *Writer) Reset() { w.buf = w.buf[:0] }

// Bytes returns the accumulated bytes. The slice is owned by the Writer and
// is only valid until the next mutation.
func (w *Writer) Bytes() []byte { return w.buf }

// Len returns the number of bytes written so far.
func (w *Writer) Len() int { return len(w.buf) }

// PutRaw appends b verbatim (no length prefix, no padding).
func (w *Writer) PutRaw(b []byte) { w.buf = append(w.buf, b...) }

// PutUint8 appends a single byte.
func (w *Writer) PutUint8(v uint8) { w.buf = append(w.buf, v) }

// PutUint32 appends a little-endian uint32.
func (w *Writer) PutUint32(v uint32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, v) }

// PutInt32 appends a little-endian int32.
func (w *Writer) PutInt32(v int32) { w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v)) }

// PutUint64 appends a little-endian uint64.
func (w *Writer) PutUint64(v uint64) { w.buf = binary.LittleEndian.AppendUint64(w.buf, v) }

// PutInt64 appends a little-endian int64.
func (w *Writer) PutInt64(v int64) { w.buf = binary.LittleEndian.AppendUint64(w.buf, uint64(v)) }

// PutDouble appends an IEEE-754 float64 in little-endian byte order.
func (w *Writer) PutDouble(v float64) { w.PutUint64(math.Float64bits(v)) }

// PutFloat appends an IEEE-754 float32 in little-endian byte order.
func (w *Writer) PutFloat(v float32) { w.PutUint32(math.Float32bits(v)) }

// maxSerializedLength bounds the length-prefixed payloads this codec can
// express. The 3-byte long form tops out at 0xFFFFFF; larger inputs are a
// caller bug and are rejected instead of silently wrapping.
const maxSerializedLength = 0xFFFFFF

// PutBytes appends b with the Soroush Plus / MTProto length prefix and zero
// padding to a 4-byte boundary:
//
//	len < 254:  [len][data][pad to 4]
//	else:       [0xFE][len:3 LE][data][pad to 4]
//
// A 0xFF first byte is never produced (the reference implementation does not
// support it either).
func (w *Writer) PutBytes(b []byte) {
	l := len(b)
	switch {
	case l < 254:
		w.buf = append(w.buf, uint8(l))
		w.buf = append(w.buf, b...)
		pad := (l + 1) % 4
		if pad != 0 {
			w.buf = append(w.buf, make([]byte, 4-pad)...)
		}
	case l <= maxSerializedLength:
		w.buf = append(w.buf, 0xFE,
			uint8(l), uint8(l>>8), uint8(l>>16))
		w.buf = append(w.buf, b...)
		pad := l % 4
		if pad != 0 {
			w.buf = append(w.buf, make([]byte, 4-pad)...)
		}
	default:
		// Length cannot be framed; truncate the write and record the error
		// state. Encode methods surface it via Writer.Err.
		w.err = ErrPayloadTooLarge
	}
}

// PutString appends s as UTF-8 bytes with the same framing as PutBytes.
func (w *Writer) PutString(s string) {
	w.PutBytes([]byte(s)) // TODO(perf): avoid the copy for long strings
}

// PutBoolBoxed writes a boxed Bool constructor.
func (w *Writer) PutBoolBoxed(v bool) {
	if v {
		w.PutUint32(0x997275b5) // boolTrue
	} else {
		w.PutUint32(0xbc799737) // boolFalse
	}
}

// VectorID is the constructor of a boxed Vector<T>.
const VectorID uint32 = 0x1cb5c415

// PutVectorBoxedHeader writes the boxed vector constructor plus element count.
func (w *Writer) PutVectorBoxedHeader(n int) {
	w.PutUint32(VectorID)
	w.PutInt32(int32(n))
}

// PutVectorBareHeader writes only the element count (bare vector form).
func (w *Writer) PutVectorBareHeader(n int) {
	w.PutInt32(int32(n))
}

// Err reports the first deferred error (e.g. from PutBytes overflow).
func (w *Writer) Err() error { return w.err }
