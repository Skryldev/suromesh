package client

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/transport"
)

// Client-level sentinel errors. Protocol-level RPC failures surface as
// *rpc.RPCError (Phase 4); transport failures as transport-classified
// errors (Phase 3). This file consolidates the client's own error surface.
var (
	// ErrClosed is returned by every operation after Close.
	ErrClosed = errors.New("client: closed")
	// ErrNotReady is returned when Invoke runs before Ready.
	ErrNotReady = errors.New("client: not ready")
	// ErrBadState reports an illegal lifecycle transition request.
	ErrBadState = errors.New("client: invalid state transition")
	// ErrConnectInProgress is returned by Connect when another Connect is
	// already running.
	ErrConnectInProgress = errors.New("client: connect already in progress")
	// ErrNilRequest is returned when Invoke receives a nil request.
	ErrNilRequest = errors.New("client: nil request")
	// ErrUnsupportedPeer is returned when a Peer implementation is not one
	// of the supported domain types.
	ErrUnsupportedPeer = errors.New("client: unsupported peer type")
	// ErrEmptyUsername is returned by ResolveUsername("").
	ErrEmptyUsername = errors.New("client: empty username")
	// ErrResolveEmptyPeer is returned when the resolver answers without a
	// usable peer.
	ErrResolveEmptyPeer = errors.New("client: resolveUsername: empty peer")
	// ErrPaginationStalled reports a listing that stopped making
	// progress (the server repeating pages, NotModified answers, or
	// filters skipping everything) — the iterator terminates instead of
	// spinning (Phase 11 pagination safety).
	ErrPaginationStalled = errors.New("client: pagination stalled")
)

// ErrAccessHashUnavailable reports that a peer is addressable by ID alone
// and no access hash is known (cache miss / never resolved). Access hashes
// are NEVER fabricated — the caller must resolve the entity first.
type ErrAccessHashUnavailable struct{ Peer PeerID }

func (e *ErrAccessHashUnavailable) Error() string {
	return fmt.Sprintf("client: no access hash for %s %d (resolve the entity first)", e.Peer.Kind, e.Peer.ID)
}

// FloodWaitError is the structured form of FLOOD_WAIT_X with the wait
// exposed for caller-side scheduling.
type FloodWaitError struct {
	Seconds int
	Request string
}

func (e *FloodWaitError) Error() string {
	return fmt.Sprintf("client: flood wait %ds on %s", e.Seconds, e.Request)
}

// AsFloodWait converts an rpc error chain into *FloodWaitError when the
// failure is a flood wait.
func AsFloodWait(err error) (*FloodWaitError, bool) {
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		return nil, false
	}
	if d, ok := rpcErr.RetryAfter(); ok {
		return &FloodWaitError{Seconds: int(d / time.Second), Request: rpcErr.Type}, true
	}
	return nil, false
}

// ConnectionLostError wraps rpc.ErrConnectionLost with the phase at which
// the connection died, without embedding any wire material.
type ConnectionLostError struct{ Err error }

func (e *ConnectionLostError) Error() string { return "client: connection lost during request" }
func (e *ConnectionLostError) Unwrap() error { return e.Err }

// AsConnectionLost reports whether err is a connection-loss failure.
func AsConnectionLost(err error) bool {
	return errors.Is(err, rpc.ErrConnectionLost)
}

// ClassifyError maps any client error to a coarse class for observability.
func ClassifyError(err error) string {
	switch {
	case err == nil:
		return "none"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	case AsConnectionLost(err):
		return "connection_lost"
	case errors.Is(err, ErrClosed):
		return "closed"
	case errors.Is(err, ErrNotReady):
		return "not_ready"
	}
	var rpcErr *rpc.RPCError
	if errors.As(err, &rpcErr) {
		return "rpc_error"
	}
	var tErr *transport.Error
	if errors.As(err, &tErr) {
		return "transport_" + tErr.Class.String()
	}
	return "unknown"
}
