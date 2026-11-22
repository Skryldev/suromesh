package upstream

// translate: SDK errors → the service domain error taxonomy. This is
// the single mapping point; the transport layer maps domain errors to
// gRPC statuses (never the SDK's own shapes).

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	sdkclient "github.com/Skryldev/suromesh/client"
	sdkrpc "github.com/Skryldev/suromesh/rpc"
	sdktransport "github.com/Skryldev/suromesh/transport"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// translate maps one SDK error into the domain taxonomy. The returned
// error wraps the original for internal logging while exposing only
// domain sentinels to callers.
func translate(err error) error {
	if err == nil {
		return nil
	}
	// Contextual cancellations pass through verbatim (callers match on
	// the stdlib sentinels).
	if errors.Is(err, context.Canceled) {
		return fmt.Errorf("%w", context.Canceled)
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("%w", context.DeadlineExceeded)
	}

	// Upstream RPC errors (typed).
	var rpcErr *sdkrpc.RPCError
	if errors.As(err, &rpcErr) {
		return translateRPC(rpcErr, err)
	}

	// Client-level wrappers.
	var connLost *sdkclient.ConnectionLostError
	if errors.As(err, &connLost) {
		return fmt.Errorf("%w: connection lost during upstream operation",
			domain.ErrAmbiguousMutation)
	}
	if sdkclient.AsConnectionLost(err) {
		return fmt.Errorf("%w: connection lost during upstream operation",
			domain.ErrAmbiguousMutation)
	}
	if errors.Is(err, sdkclient.ErrClosed) {
		return fmt.Errorf("%w: client closed", domain.ErrAccountStopped)
	}
	if errors.Is(err, sdkclient.ErrNotReady) {
		return fmt.Errorf("%w: upstream not ready", domain.ErrAccountNotConnected)
	}
	if errors.Is(err, sdkclient.ErrFileReferenceExpired) {
		return fmt.Errorf("%w", domain.ErrFileReferenceExpired)
	}

	// Transport failures (classified).
	var tErr *sdktransport.Error
	if errors.As(err, &tErr) {
		switch tErr.Class {
		case sdktransport.ClassTemporary:
			return fmt.Errorf("%w: transport temporary failure: %v",
				domain.ErrUpstreamUnavailable, tErr.Error())
		default:
			return fmt.Errorf("%w: transport failure: %v",
				domain.ErrUpstreamProtocol, tErr.Error())
		}
	}

	// Network-level failures (dial etc.).
	var netErr net.Error
	if errors.As(err, &netErr) {
		if netErr.Timeout() {
			return fmt.Errorf("%w: %v", domain.ErrUpstreamTimeout, err)
		}
		return fmt.Errorf("%w: %v", domain.ErrUpstreamUnavailable, err)
	}
	if isConnRefused(err) {
		return fmt.Errorf("%w: %v", domain.ErrUpstreamUnavailable, err)
	}

	// Fallback: internal (unexpected SDK error shape — logged upstream of
	// this seam; never surfaced raw to clients).
	return fmt.Errorf("%w: %v", domain.ErrUpstreamProtocol, err)
}

// translateRPC maps a typed upstream RPC error.
func translateRPC(rpcErr *sdkrpc.RPCError, cause error) error {
	msg := rpcErr.Type
	switch {
	case rpcErr.IsFloodWait():
		d, _ := rpcErr.RetryAfter()
		return fmt.Errorf("%w: %s", &domain.FloodWaitError{Seconds: int(d / time.Second)}, cause)
	case strings.HasPrefix(msg, "AUTH_KEY_UNREGISTERED"),
		strings.HasPrefix(msg, "AUTH_KEY_INVALID"),
		strings.HasPrefix(msg, "SESSION_REVOKED"),
		strings.HasPrefix(msg, "SESSION_EXPIRED"),
		strings.HasPrefix(msg, "USER_DEACTIVATED"):
		return fmt.Errorf("%w: %s", domain.ErrSessionRevoked, msg)
	case msg == "FILE_REFERENCE_EXPIRED":
		return fmt.Errorf("%w: %s", domain.ErrFileReferenceExpired, msg)
	case strings.HasPrefix(msg, "PEER_ID_INVALID"),
		strings.HasPrefix(msg, "PEER_HASH_INVALID"):
		return fmt.Errorf("%w: %s (resolve the peer first)", domain.ErrInvalidInput, msg)
	case strings.HasPrefix(msg, "CHANNEL_PRIVATE"),
		strings.HasPrefix(msg, "CHAT_WRITE_FORBIDDEN"),
		strings.HasPrefix(msg, "CHANNEL_WRITE_FORBIDDEN"),
		strings.HasPrefix(msg, "USER_IS_BLOCKED"):
		return fmt.Errorf("%w: %s", domain.ErrNotFound, msg)
	case strings.HasPrefix(msg, "MESSAGE_ID_INVALID"):
		return fmt.Errorf("%w: %s", domain.ErrNotFound, msg)
	default:
		return fmt.Errorf("%w: %s (%d)", domain.ErrUpstreamProtocol, msg, rpcErr.Code)
	}
}

// isConnRefused detects connection-refused shaped errors without
// importing syscall families per platform.
func isConnRefused(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "connection refused") ||
		strings.Contains(err.Error(), "no such host")
}
