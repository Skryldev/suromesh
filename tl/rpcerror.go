package tl

import "fmt"

// RpcErrorID is rpc_error#2144ca19 (mtproto.tl). The type is hand-written and
// excluded from generation because the manual rpc_result decoder in this
// package depends on it.
const RpcErrorID uint32 = 0x2144ca19

// RpcError is an RPC failure reported by the server:
//
//	rpc_error#2144ca19 error_code:int error_message:string = RpcError;
type RpcError struct {
	ErrorCode    int32
	ErrorMessage string
}

// CtorID implements Object.
func (*RpcError) CtorID() uint32 { return RpcErrorID }

// Encode implements Object.
func (e *RpcError) Encode(w *Writer) error {
	w.PutUint32(RpcErrorID)
	w.PutInt32(e.ErrorCode)
	w.PutString(e.ErrorMessage)
	return w.Err()
}

// DecodeRpcErrorBody decodes rpc_error fields after the constructor ID.
func DecodeRpcErrorBody(r *Reader) (*RpcError, error) {
	code, err := r.ReadInt32()
	if err != nil {
		return nil, err
	}
	msg, err := r.ReadString()
	if err != nil {
		return nil, err
	}
	return &RpcError{ErrorCode: code, ErrorMessage: msg}, nil
}

// DecodeRpcError decodes a full rpc_error including its constructor ID and
// validates the ID.
func DecodeRpcError(r *Reader) (*RpcError, error) {
	id, err := r.ReadUint32()
	if err != nil {
		return nil, err
	}
	if id != RpcErrorID {
		return nil, &ConstructorMismatchError{Got: id, Want: RpcErrorID}
	}
	return DecodeRpcErrorBody(r)
}

func (e *RpcError) String() string {
	return fmt.Sprintf("rpc_error %d %s", e.ErrorCode, e.ErrorMessage)
}
