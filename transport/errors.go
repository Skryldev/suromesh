package transport

import (
	"context"
	"errors"
	"fmt"
	"net"
	"syscall"

	"github.com/Skryldev/suromesh/transport/abridged"
	"github.com/Skryldev/suromesh/transport/ws"
)

// Classification drives the reconnect policy:
//
//	Temporary network failure → reconnect (with backoff)
//	Timeout                   → reconnect
//	Connection closed         → reconnect if not user-initiated
//	Protocol violation        → fail fast, do not blindly reconnect
//	Resource exhaustion       → surface to caller, no reconnect
//	Context cancellation      → stop immediately
type Classification int

const (
	// ClassTemporary covers transient network failures worth retrying.
	ClassTemporary Classification = iota
	// ClassTimeout covers read/write/connect deadlines.
	ClassTimeout
	// ClassClosed covers the peer going away (EOF, reset, close frames).
	ClassClosed
	// ClassProtocol covers wire-format violations from the peer. A peer
	// that speaks garbage will keep speaking garbage; reconnecting forever
	// is wrong, so this classification stops the transport.
	ClassProtocol
	// ClassResource covers local resource exhaustion (queue full, packet
	// too large). Not a connection health signal.
	ClassResource
	// ClassCancelled covers context cancellation and user shutdown.
	ClassCancelled
)

func (c Classification) String() string {
	switch c {
	case ClassTemporary:
		return "temporary"
	case ClassTimeout:
		return "timeout"
	case ClassClosed:
		return "closed"
	case ClassProtocol:
		return "protocol"
	case ClassResource:
		return "resource"
	case ClassCancelled:
		return "cancelled"
	}
	return "unknown"
}

// Sentinel errors. All transport errors wrap one of these (errors.Is).
var (
	// ErrClosed is returned by operations on a closed transport.
	ErrClosed = errors.New("transport: closed")
	// ErrQueueFull is returned when the outbound queue is full and the
	// configured policy is Reject.
	ErrQueueFull = errors.New("transport: outbound queue full")
	// ErrNotConnected is returned for sends while no connection exists and
	// queueing is impossible (queue full) or disallowed.
	ErrNotConnected = errors.New("transport: not connected")
	// ErrProtocolFatal wraps peer protocol violations; the transport stops
	// rather than reconnect-looping against a broken peer.
	ErrProtocolFatal = errors.New("transport: fatal protocol violation")
	// ErrReconnectExhausted is the terminal error after the reconnect
	// budget runs out.
	ErrReconnectExhausted = errors.New("transport: reconnect attempts exhausted")
	// ErrBadState reports an invalid lifecycle transition request.
	ErrBadState = errors.New("transport: invalid state transition")
)

// Error is the structured transport error carrying its classification.
type Error struct {
	Class Classification
	Op    string
	Err   error
}

func (e *Error) Error() string {
	return fmt.Sprintf("transport: %s: %s: %v", e.Op, e.Class, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// Is maps onto the sentinel errors so errors.Is works through the wrapper.
func (e *Error) Is(target error) bool {
	switch target {
	case ErrClosed:
		return errors.Is(e.Err, context.Canceled) || e.Class == ClassCancelled && e.Err == ErrClosed
	case ErrQueueFull:
		return e.Err == ErrQueueFull
	case ErrProtocolFatal:
		return e.Class == ClassProtocol
	}
	return false
}

// Classify maps a raw error to its transport classification.
func Classify(err error) Classification {
	if err == nil {
		return ClassTemporary
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) && errors.Is(err, ErrClosed) {
		return ClassCancelled
	}
	// Transport-level sentinels.
	var te *Error
	if errors.As(err, &te) {
		return te.Class
	}
	// WebSocket-layer classification: peer protocol violations and
	// hostile sizes are fatal; anything else from the socket is temporary
	// or a closure.
	if errors.Is(err, ws.ErrProtocol) || errors.Is(err, ws.ErrTooBig) ||
		errors.Is(err, ws.ErrHandshakeBadStatus) {
		return ClassProtocol
	}
	if errors.Is(err, abridged.ErrTooLarge) || errors.Is(err, abridged.ErrBadLength) {
		return ClassProtocol
	}
	if errors.Is(err, ErrQueueFull) {
		return ClassResource
	}
	// Network layer.
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return ClassTimeout
		}
		return ClassTemporary
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNABORTED) ||
		errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENETUNREACH) ||
		errors.Is(err, syscall.EHOSTUNREACH) || errors.Is(err, syscall.ECONNREFUSED) {
		return ClassTemporary
	}
	if errors.Is(err, ErrClosed) {
		return ClassClosed
	}
	// io.EOF and everything else from a live socket reads as closure.
	return ClassClosed
}

// reconnectable reports whether a classification should trigger a reconnect
// attempt (as opposed to stopping or surfacing locally).
func (c Classification) reconnectable() bool {
	switch c {
	case ClassTemporary, ClassTimeout, ClassClosed:
		return true
	}
	return false
}
