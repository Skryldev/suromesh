// Package errors maps domain errors to gRPC statuses with stable,
// machine-readable reasons (google.rpc.ErrorInfo). Callers classify via
// ErrorInfo.reason — never by parsing message strings.
package errors

import (
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// Machine-readable reasons (stable contract — additive only).
const (
	ReasonInvalidInput         = "INVALID_INPUT"
	ReasonNotFound             = "NOT_FOUND"
	ReasonAlreadyExists        = "ALREADY_EXISTS"
	ReasonAccountStopped       = "ACCOUNT_STOPPED"
	ReasonAccountNotConnected  = "ACCOUNT_NOT_CONNECTED"
	ReasonUpstreamUnavailable  = "UPSTREAM_UNAVAILABLE"
	ReasonUpstreamTimeout      = "UPSTREAM_TIMEOUT"
	ReasonUpstreamProtocol     = "UPSTREAM_PROTOCOL_FAILURE"
	ReasonAmbiguousMutation    = "OPERATION_AMBIGUOUS"
	ReasonSessionRevoked       = "SESSION_REVOKED"
	ReasonFloodWait            = "UPSTREAM_FLOOD_WAIT"
	ReasonFileReferenceExpired = "FILE_REFERENCE_EXPIRED"
	ReasonUnsupported          = "UNSUPPORTED_CAPABILITY"
	ReasonPersistence          = "PERSISTENCE_FAILURE"
	ReasonCrypto               = "CRYPTO_FAILURE"
	ReasonTooManyRequests      = "TENANT_QUOTA"
	ReasonUnauthenticated      = "UNAUTHENTICATED"
	ReasonPermissionDenied     = "PERMISSION_DENIED"
	ReasonTenantMismatch       = "TENANT_MISMATCH"
	ReasonInternal             = "INTERNAL"
)

// Map converts a domain error to a gRPC status with ErrorInfo details.
// Internal causes are NEVER embedded in client-visible messages (they go
// to logs/traces via the recovery/telemetry interceptors).
func Map(err error) error {
	if err == nil {
		return nil
	}
	class := domain.Classify(err)
	var reason string
	var code codes.Code

	switch class {
	case domain.ClassNone:
		return nil
	case domain.ClassCanceled:
		code, reason = codes.Canceled, "REQUEST_CANCELED"
	case domain.ClassTimeout:
		code, reason = codes.DeadlineExceeded, "REQUEST_TIMEOUT"
	case domain.ClassInvalidInput:
		code, reason = codes.InvalidArgument, ReasonInvalidInput
	case domain.ClassNotFound:
		code, reason = codes.NotFound, ReasonNotFound
	case domain.ClassAlreadyExists:
		code, reason = codes.AlreadyExists, ReasonAlreadyExists
	case domain.ClassAccountStopped:
		code, reason = codes.Unavailable, ReasonAccountStopped
	case domain.ClassNotConnected:
		code, reason = codes.FailedPrecondition, ReasonAccountNotConnected
	case domain.ClassAmbiguous:
		code, reason = codes.Unavailable, ReasonAmbiguousMutation
	case domain.ClassSessionRevoked:
		code, reason = codes.FailedPrecondition, ReasonSessionRevoked
	case domain.ClassFloodWait:
		code, reason = codes.ResourceExhausted, ReasonFloodWait
	case domain.ClassFileRefExpired:
		code, reason = codes.FailedPrecondition, ReasonFileReferenceExpired
	case domain.ClassUnsupported:
		code, reason = codes.Unimplemented, ReasonUnsupported
	case domain.ClassCrypto:
		code, reason = codes.Internal, ReasonCrypto
	case domain.ClassPersistence:
		code, reason = codes.Internal, ReasonPersistence
	case domain.ClassTooManyRequests:
		code, reason = codes.ResourceExhausted, ReasonTooManyRequests
	case domain.ClassUpstreamUnavail:
		code, reason = codes.Unavailable, ReasonUpstreamUnavailable
	case domain.ClassUpstreamTimeout:
		code, reason = codes.DeadlineExceeded, ReasonUpstreamTimeout
	case domain.ClassUpstreamProtocol:
		code, reason = codes.Internal, ReasonUpstreamProtocol
	default:
		code, reason = codes.Internal, ReasonInternal
	}

	st := status.New(code, sanitizedMessage(class, err))
	// ErrorInfo: reason (stable) + domain + sanitized metadata (flood-wait
	// seconds are the one numeric detail callers legitimately need).
	info := &errdetails.ErrorInfo{
		Reason: reason,
		Domain: "suromesh.service",
	}
	if secs, ok := domain.WaitSeconds(err); ok {
		info.Metadata = map[string]string{"wait_seconds": itoa(secs)}
	}
	withDetails, derr := st.WithDetails(info)
	if derr == nil {
		return withDetails.Err()
	}
	return st.Err()
}

// sanitizedMessage renders a class-level message; raw error text stays
// in logs (it may contain upstream shapes we do not promise to sanitize).
func sanitizedMessage(class domain.Class, err error) string {
	if secs, ok := domain.WaitSeconds(err); ok {
		return "upstream flood wait: retry after " + itoa(secs) + "s"
	}
	switch class {
	case domain.ClassAmbiguous:
		return "the operation may or may not have completed upstream; " +
			"reconcile via reads — do not blindly retry"
	case domain.ClassNotFound:
		return "resource not found"
	case domain.ClassSessionRevoked:
		return "the account session was revoked upstream; re-authentication required"
	default:
		return "request failed (" + string(class) + ")"
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// ReasonOf extracts the machine-readable reason from a gRPC error
// (helper for tests and gateway clients).
func ReasonOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Reason
		}
	}
	return ""
}
