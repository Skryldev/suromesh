// Package rpc implements the MTProto request engine: pending-request
// correlation, response dispatch, RPC error mapping, and the service
// message handling (salts, bad-msg recovery) that sits between the Phase 3
// transport and the future client layer.
//
// Lifecycle documentation lives in docs/request-engine.md; this package
// never touches the wire directly — everything goes through the Phase 3
// transport (bounded queues, backpressure, reconnect) and the Phase 2
// crypto/TL layers.
package rpc

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Sentinel errors surfaced by Invoke. All wrap into *RPCError or plain
// errors; use errors.Is/As.
var (
	// ErrEngineClosed is returned when the engine is shut down.
	ErrEngineClosed = fmt.Errorf("rpc: engine closed")
	// ErrConnectionLost is returned when pending requests are failed
	// because the transport connection died (default reconnect policy).
	ErrConnectionLost = fmt.Errorf("rpc: connection lost")
	// ErrPendingLimit is returned when the pending-request registry is at
	// its configured capacity: the caller must back off or fail over.
	// The request was never sent (deterministic, safe to retry).
	ErrPendingLimit = fmt.Errorf("rpc: pending request limit reached")
	// ErrBadMessage is returned when the server rejects a specific message
	// (bad_msg_notification) in a way that cannot be corrected locally.
	ErrBadMessage = fmt.Errorf("rpc: bad message")
)

// RPCError is a structured server-side RPC failure:
//
//	rpc_error#2144ca19 error_code:int error_message:string
//
// The message frequently embeds a numeric argument (FLOOD_WAIT_7,
// PHONE_NUMBER_INVALID has none). Type and Argument are separated so
// callers can branch without string parsing.
type RPCError struct {
	Code     int32  // wire error_code (400, 420, 500, …)
	Type     string // leading alpha segment ("FLOOD_WAIT")
	Message  string // raw wire error_message
	Argument int    // numeric suffix when present (-1 when absent)
}

// Error implements error.
func (e *RPCError) Error() string {
	return fmt.Sprintf("rpc error %d: %s (caused by request)", e.Code, e.Message)
}

// RetryAfter reports the wait time for FLOOD_WAIT_X / SLOWMODE_WAIT_X style
// errors and whether one was present.
func (e *RPCError) RetryAfter() (time.Duration, bool) {
	switch e.Type {
	case "FLOOD_WAIT", "FLOOD_PREMIUM_WAIT", "SLOWMODE_WAIT", "FLOOD_TEST_PHONE_WAIT":
		if e.Argument > 0 {
			return time.Duration(e.Argument) * time.Second, true
		}
	}
	return 0, false
}

// parseRPCError maps a wire RpcError to the structured type.
func parseRPCError(code int32, message string) *RPCError {
	e := &RPCError{Code: code, Type: message, Message: message, Argument: -1}
	// Convention: TYPE_ARG or TYPE-ARG; the reference regexes on the raw
	// message with a trailing numeric capture.
	if i := strings.LastIndexAny(message, "_-"); i >= 0 && i+1 < len(message) {
		if n, err := strconv.Atoi(message[i+1:]); err == nil {
			e.Argument = n
			e.Type = message[:i]
			// "TYPE-X" keeps the dash out of the type name.
			e.Type = strings.TrimRight(e.Type, "_-")
		}
	}
	return e
}

// IsFloodWait reports whether the error is a flood wait carrying a retry
// delay.
func (e *RPCError) IsFloodWait() bool {
	_, ok := e.RetryAfter()
	return ok
}
