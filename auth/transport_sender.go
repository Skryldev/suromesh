package auth

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/transport"
)

// TransportPlainSender adapts the Phase 3 transport to the PlainSender
// interface used during the key exchange (production path).
type TransportPlainSender struct {
	Tr *transport.Transport
}

// SendPlain implements PlainSender over the transport's raw packet lane:
// plain envelope out, packet in, plain response body back.
func (t TransportPlainSender) SendPlain(ctx context.Context, body []byte) ([]byte, error) {
	pkt := mtproto.PlainMessage(nextPlainMsgID(), body)
	if err := t.Tr.Send(ctx, pkt); err != nil {
		return nil, fmt.Errorf("auth: plain send: %w", err)
	}
	select {
	case resp, ok := <-t.Tr.Inbound():
		if !ok {
			return nil, fmt.Errorf("auth: transport closed during exchange")
		}
		return plainBody(resp)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func nextPlainMsgID() int64 {
	plainMsgIDMu.Lock()
	defer plainMsgIDMu.Unlock()
	plainMsgIDCounter += 4
	return plainMsgIDCounter
}

// plainBody strips the plain response framing: 0:int64 ‖ msg_id:int64 ‖ len:int32.
func plainBody(pkt []byte) ([]byte, error) {
	if len(pkt) < 20 {
		return nil, fmt.Errorf("auth: plain response too short")
	}
	return pkt[20:], nil
}

var (
	plainMsgIDMu      sync.Mutex
	plainMsgIDCounter int64 = time.Now().UnixNano() &^ 3 // even-aligned base, grows by 4
)
