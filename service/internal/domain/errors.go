package domain

import (
	"context"
	"errors"
	"fmt"
	"strconv"
)

// Domain sentinel errors — the vocabulary of the application and domain
// layers. The transport adapter maps these (plus classes) to gRPC
// statuses with machine-readable reasons; nothing parses strings.
var (
	// ErrInvalidInput: caller-supplied input failed validation.
	ErrInvalidInput = errors.New("domain: invalid input")

	// ErrNotFound: the entity does not exist within the caller's scope.
	// Cross-tenant references are deliberately indistinguishable from
	// missing ones (no existence oracle).
	ErrNotFound = errors.New("domain: not found")

	// ErrAlreadyExists: a genuine conflicting entity exists.
	ErrAlreadyExists = errors.New("domain: already exists")

	// ErrAccountStopped: the account runner is not running.
	ErrAccountStopped = errors.New("domain: account stopped")

	// ErrAccountNotConnected: runner running but upstream not READY.
	ErrAccountNotConnected = errors.New("domain: account not connected")

	// ErrUpstreamUnavailable: upstream unreachable; reads may retry per
	// the documented policy.
	ErrUpstreamUnavailable = errors.New("domain: upstream unavailable")

	// ErrUpstreamTimeout: the upstream operation exceeded its deadline.
	ErrUpstreamTimeout = errors.New("domain: upstream timeout")

	// ErrUpstreamProtocol: upstream behaved outside the protocol contract.
	ErrUpstreamProtocol = errors.New("domain: upstream protocol failure")

	// ErrAmbiguousMutation: a mutation whose upstream outcome is unknown
	// (connection lost mid-operation). NEVER auto-retried; callers
	// reconcile via reads.
	ErrAmbiguousMutation = errors.New("domain: ambiguous mutation")

	// ErrSessionRevoked: upstream rejected the session (401-class).
	ErrSessionRevoked = errors.New("domain: session revoked")

	// ErrFloodWait: upstream rate limit; wait duration attached via
	// *FloodWaitError.
	ErrFloodWait = errors.New("domain: upstream flood wait")

	// ErrFileReferenceExpired: media reference expired; re-fetch the
	// message and retry once.
	ErrFileReferenceExpired = errors.New("domain: file reference expired")

	// ErrUnsupported: not supported by the upstream schema/SDK scope
	// (stable refusal — never fabricated data).
	ErrUnsupported = errors.New("domain: unsupported operation")

	// ErrDisabled: deterministic stub seam (explicit, never silent
	// success).
	ErrDisabled = errors.New("domain: disabled in this phase")

	// ErrPersistence: storage-layer failure.
	ErrPersistence = errors.New("domain: persistence failure")

	// ErrCrypto: encryption/key-provider failure.
	ErrCrypto = errors.New("domain: crypto failure")

	// ErrTooManyRequests: service-side quota/rate limit (distinct from
	// upstream flood waits).
	ErrTooManyRequests = errors.New("domain: too many requests")

	// ErrClientClosed: the account client was closed mid-operation
	// (shutdown racing an operation) — classified with the account-stopped
	// family at the adapter boundary.
	ErrClientClosed = errors.New("domain: client closed")
)

// FloodWaitError attaches the upstream wait duration to ErrFloodWait.
type FloodWaitError struct {
	Seconds int
}

func (e *FloodWaitError) Error() string {
	return fmt.Sprintf("domain: upstream flood wait %ds", e.Seconds)
}

func (e *FloodWaitError) Unwrap() error { return ErrFloodWait }

// Class is the sanitized error class for observability and transport
// mapping (bounded cardinality; never a payload).
type Class string

const (
	ClassNone             Class = ""
	ClassCanceled         Class = "canceled"
	ClassTimeout          Class = "timeout"
	ClassInvalidInput     Class = "invalid_input"
	ClassNotFound         Class = "not_found"
	ClassAlreadyExists    Class = "already_exists"
	ClassAccountStopped   Class = "account_stopped"
	ClassNotConnected     Class = "not_connected"
	ClassUpstreamUnavail  Class = "upstream_unavailable"
	ClassUpstreamTimeout  Class = "upstream_timeout"
	ClassUpstreamProtocol Class = "upstream_protocol"
	ClassAmbiguous        Class = "ambiguous_mutation"
	ClassSessionRevoked   Class = "session_revoked"
	ClassFloodWait        Class = "flood_wait"
	ClassFileRefExpired   Class = "file_reference_expired"
	ClassUnsupported      Class = "unsupported"
	ClassDisabled         Class = "disabled"
	ClassPersistence      Class = "persistence"
	ClassCrypto           Class = "crypto"
	ClassTooManyRequests  Class = "too_many_requests"
	ClassInternal         Class = "internal"
)

// classOrder maps errors to classes, most specific first.
var classMap = []struct {
	target error
	class  Class
}{
	{ErrAmbiguousMutation, ClassAmbiguous},
	{ErrSessionRevoked, ClassSessionRevoked},
	{ErrFileReferenceExpired, ClassFileRefExpired},
	{ErrFloodWait, ClassFloodWait},
	{ErrTooManyRequests, ClassTooManyRequests},
	{ErrInvalidInput, ClassInvalidInput},
	{ErrNotFound, ClassNotFound},
	{ErrAlreadyExists, ClassAlreadyExists},
	{ErrAccountStopped, ClassAccountStopped},
	{ErrAccountNotConnected, ClassNotConnected},
	{ErrCrypto, ClassCrypto},
	{ErrPersistence, ClassPersistence},
	{ErrUnsupported, ClassUnsupported},
	{ErrDisabled, ClassUnsupported},
	{ErrUpstreamTimeout, ClassUpstreamTimeout},
	{ErrUpstreamProtocol, ClassUpstreamProtocol},
	{ErrUpstreamUnavailable, ClassUpstreamUnavail},
	{ErrClientClosed, ClassAccountStopped},
}

// Classify maps an error to its sanitized class. Context cancellation
// and deadlines are honored wherever they appear in the chain.
func Classify(err error) Class {
	if err == nil {
		return ClassNone
	}
	if errors.Is(err, context.Canceled) {
		return ClassCanceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ClassTimeout
	}
	for _, m := range classMap {
		if errors.Is(err, m.target) {
			return m.class
		}
	}
	return ClassInternal
}

// WaitSeconds extracts the upstream flood-wait duration when present.
func WaitSeconds(err error) (int, bool) {
	var fw *FloodWaitError
	if errors.As(err, &fw) {
		return fw.Seconds, true
	}
	return 0, false
}

// FormatID renders an upstream int64 id as the decimal string used on
// the wire contract (safe for JS consumers).
func FormatID(id int64) string { return strconv.FormatInt(id, 10) }
