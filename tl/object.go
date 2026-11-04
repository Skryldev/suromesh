package tl

import "fmt"

// Object is implemented by every generated TL constructor and by the
// hand-written core constructors in this package.
type Object interface {
	// CtorID returns the TL constructor ID of the concrete type.
	CtorID() uint32
	// Encode appends the constructor ID and all fields to w.
	Encode(w *Writer) error
}

// Request is implemented by generated RPC request types (TL functions).
type Request interface {
	Object
	// IsRequest marks the type as an RPC request; it has no behavior.
	IsRequest()
}

// ---- Core constructors -------------------------------------------------
//
// The schema comments these out ("parsed manually"): gzip_packed#3072cfa1,
// msg_container#73f1f8dc, rpc_result#f35c6d01. They are implemented here and
// wired into the generated dispatch switch.

// Constructor IDs of the manual core objects.
const (
	GzipPackedID       uint32 = 0x3072cfa1
	MessageContainerID uint32 = 0x73f1f8dc
	RpcResultID        uint32 = 0xf35c6d01
)

// GzipPacked wraps a gzip-compressed serialized object.
//
// Data holds the *decompressed* payload bytes, mirroring the reference
// implementation's GzipPacked.from_reader. Re-encoding recompresses, so
// byte-for-byte round trips through GzipPacked are not stable across
// compressors (the payload bytes after decompression are).
type GzipPacked struct {
	Data []byte
}

// CtorID implements Object.
func (*GzipPacked) CtorID() uint32 { return GzipPackedID }

// Encode implements Object.
func (g *GzipPacked) Encode(w *Writer) error {
	w.PutUint32(GzipPackedID)
	w.PutBytes(Gzip(g.Data))
	return w.Err()
}

// DecodeGzipPackedBody decodes a gzip_packed whose constructor ID has already
// been read; it returns the decompressed payload bytes.
func DecodeGzipPackedBody(r *Reader) ([]byte, error) {
	packed, err := r.ReadBytes()
	if err != nil {
		return nil, err
	}
	return Gunzip(packed)
}

// MessageContainer carries up to 100 packed messages in one envelope.
type MessageContainer struct {
	Messages []TLMessage
}

// The reference implementation's limits (tl/core/messagecontainer.py):
// the server closes the connection past these bounds.
const (
	ContainerMaxSize   = 1044456 - 8 // 1044448 bytes of inner payload
	ContainerMaxLength = 100
)

// CtorID implements Object.
func (*MessageContainer) CtorID() uint32 { return MessageContainerID }

// Encode implements Object.
func (m *MessageContainer) Encode(w *Writer) error {
	w.PutUint32(MessageContainerID)
	w.PutInt32(int32(len(m.Messages)))
	for i := range m.Messages {
		if err := m.Messages[i].encode(w); err != nil {
			return err
		}
	}
	return w.Err()
}

// DecodeMessageContainerBody decodes a msg_container whose constructor ID has
// already been read. Each inner message is decoded within its declared length
// bound, so an over-reading inner object (rpc_result) cannot corrupt the
// framing of the following messages.
func DecodeMessageContainerBody(r *Reader) (*MessageContainer, error) {
	n, err := r.ReadVectorBareHeader()
	if err != nil {
		return nil, err
	}
	msgs := make([]TLMessage, 0, min(n, 1024))
	for i := 0; i < n; i++ {
		msgID, err := r.ReadInt64()
		if err != nil {
			return nil, err
		}
		seqNo, err := r.ReadInt32()
		if err != nil {
			return nil, err
		}
		length, err := r.ReadInt32()
		if err != nil {
			return nil, err
		}
		if length < 0 || int(length) > r.Remaining() {
			return nil, fmt.Errorf("%w: inner message length %d exceeds %d remaining bytes",
				ErrInvalidLength, length, r.Remaining())
		}
		body := r.buf[r.pos : r.pos+int(length)]
		sub := NewReader(body)
		obj, err := DecodeObject(sub)
		if err != nil {
			return nil, err
		}
		// The inner object may over-read (rpc_result reads its body as a
		// single slice); resynchronize on the declared length like the
		// reference implementation does with set_position(before+length).
		r.pos += int(length)
		msgs = append(msgs, TLMessage{MsgID: msgID, SeqNo: seqNo, Obj: obj})
	}
	return &MessageContainer{Messages: msgs}, nil
}

// TLMessage is one entry of a message container (or a bare encrypted message).
type TLMessage struct {
	MsgID int64
	SeqNo int32
	Obj   Object
}

// encode writes the on-wire message framing: msg_id:int64, seq_no:int32,
// byte length:int32 (16-byte header), body.
func (m *TLMessage) encode(w *Writer) error {
	start := w.Len()
	w.PutInt64(m.MsgID)
	w.PutInt32(m.SeqNo)
	w.PutInt32(0) // length placeholder
	if m.Obj == nil {
		return fmt.Errorf("tl: message %d has nil body", m.MsgID)
	}
	if err := m.Obj.Encode(w); err != nil {
		return err
	}
	length := w.Len() - start - 16
	if length < 0 || length > ContainerMaxSize {
		return fmt.Errorf("%w: message body is %d bytes", ErrPayloadTooLarge, length)
	}
	// Patch the length placeholder.
	raw := w.Bytes()
	putInt32At(raw, start+12, int32(length))
	return nil
}

// RpcResult is the response envelope for RPC calls.
//
// When Error is nil, Body holds the raw serialized result object; when Error
// is non-nil, Body is nil. Mirrors tl/core/rpcresult.py.
type RpcResult struct {
	ReqMsgID int64
	Body     []byte
	Error    *RpcError
}

// CtorID implements Object.
func (*RpcResult) CtorID() uint32 { return RpcResultID }

// Encode implements Object. Encoding is only used by test doubles; the client
// never sends rpc_result.
func (x *RpcResult) Encode(w *Writer) error {
	w.PutUint32(RpcResultID)
	w.PutInt64(x.ReqMsgID)
	switch {
	case x.Error != nil:
		if err := x.Error.Encode(w); err != nil {
			return err
		}
	case x.Body != nil:
		w.PutRaw(x.Body)
	default:
		return fmt.Errorf("tl: rpc_result needs Body or Error")
	}
	return w.Err()
}

// DecodeRpcResultBody decodes an rpc_result whose constructor ID has already
// been read.
func DecodeRpcResultBody(r *Reader) (*RpcResult, error) {
	reqMsgID, err := r.ReadInt64()
	if err != nil {
		return nil, err
	}
	inner, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	switch inner {
	case RpcErrorID:
		e, err := DecodeRpcErrorBody(r)
		if err != nil {
			return nil, err
		}
		return &RpcResult{ReqMsgID: reqMsgID, Error: e}, nil
	case GzipPackedID:
		data, err := DecodeGzipPackedBody(r)
		if err != nil {
			return nil, err
		}
		return &RpcResult{ReqMsgID: reqMsgID, Body: data}, nil
	default:
		// Raw result object bytes, unread (lazy decode), like the reference
		// implementation which keeps the body and decodes later.
		r.Seek(-4)
		body := r.buf[r.pos:]
		r.pos = len(r.buf)
		return &RpcResult{ReqMsgID: reqMsgID, Body: body}, nil
	}
}

// DecodeObject decodes any registered constructor (manual core objects plus
// the generated schema constructors) from r.
func DecodeObject(r *Reader) (Object, error) {
	id, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	return DecodeObjectAfterID(id, r)
}

// DecodeObjectAfterID dispatches on an already-read constructor ID.
// It is replaced at init time by the generated package, which registers the
// full schema switch; until then only the manual core objects decode.
func DecodeObjectAfterID(id uint32, r *Reader) (Object, error) {
	if dispatchHook != nil {
		obj, err := dispatchHook(id, r)
		if err == nil || !IsUnknownConstructor(err) {
			return obj, err
		}
		// Fall through to the manual core objects below so that a generated
		// switch missing a core ID still decodes; unknown errors propagate.
	}
	switch id {
	case GzipPackedID:
		data, err := DecodeGzipPackedBody(r)
		if err != nil {
			return nil, err
		}
		return &GzipPacked{Data: data}, nil
	case MessageContainerID:
		return DecodeMessageContainerBody(r)
	case RpcResultID:
		return DecodeRpcResultBody(r)
	case 0x2144ca19: // rpc_error (also generated; kept for standalone use)
		return DecodeRpcErrorBody(r)
	}
	return nil, &UnknownConstructorError{ID: id, Remaining: r.buf[r.pos:]}
}

var dispatchHook func(id uint32, r *Reader) (Object, error)

// SetDispatchHook registers the generated schema dispatcher. It panics if
// called twice: there is exactly one schema per binary.
func SetDispatchHook(fn func(id uint32, r *Reader) (Object, error)) {
	if dispatchHook != nil {
		panic("tl: dispatch hook already registered")
	}
	dispatchHook = fn
}

// IsUnknownConstructor reports whether err (or its chain) is
// ErrUnknownConstructor.
func IsUnknownConstructor(err error) bool {
	var u *UnknownConstructorError
	if asErr(err, &u) {
		return true
	}
	return err == ErrUnknownConstructor
}

// asErr wraps errors.As to keep object.go import-light.
func asErr(err error, target **UnknownConstructorError) bool {
	for err != nil {
		if u, ok := err.(*UnknownConstructorError); ok {
			*target = u
			return true
		}
		u2, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u2.Unwrap()
	}
	return false
}

func putInt32At(b []byte, off int, v int32) {
	b[off] = byte(v)
	b[off+1] = byte(v >> 8)
	b[off+2] = byte(v >> 16)
	b[off+3] = byte(v >> 24)
}
