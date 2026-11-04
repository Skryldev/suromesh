// Package tl implements the Soroush Plus Type-Language wire runtime:
// binary encoding/decoding primitives, the Object model, and the
// hand-parsed core constructors (gzip_packed, msg_container, rpc_result)
// that the schema marks as "parsed manually".
//
// All multi-byte integers are little-endian, matching MTProto.
package tl

import (
	"errors"
	"fmt"
)

// Sentinel errors. Wrap with %w where context is added.
var (
	// ErrUnexpectedEOF is returned when a read runs past the end of the buffer.
	ErrUnexpectedEOF = errors.New("tl: unexpected end of buffer")
	// ErrUnknownConstructor is returned when a constructor ID has no registered
	// decoder. It corresponds to the reference implementation's TypeNotFoundError.
	ErrUnknownConstructor = errors.New("tl: unknown constructor")
	// ErrInvalidBool is returned when a boxed Bool field carries neither
	// boolTrue nor boolFalse.
	ErrInvalidBool = errors.New("tl: invalid boxed bool")
	// ErrInvalidVector is returned when a boxed vector carries a wrong
	// constructor ID or a negative/implausible element count.
	ErrInvalidVector = errors.New("tl: invalid vector")
	// ErrInvalidLength is returned when a length prefix is inconsistent with
	// the remaining buffer or violates the TL framing rules.
	ErrInvalidLength = errors.New("tl: invalid length prefix")
	// ErrConstructorMismatch is returned when a typed decoder encounters a
	// different constructor ID than expected.
	ErrConstructorMismatch = errors.New("tl: constructor mismatch")
	// ErrPayloadTooLarge is returned when an encoded element would exceed the
	// framing limits of the protocol (e.g. a string longer than 16 MiB).
	ErrPayloadTooLarge = errors.New("tl: payload too large")
)

// ConstructorMismatchError reports a typed decode that found a different
// constructor than expected, keeping both IDs for diagnostics.
type ConstructorMismatchError struct {
	Got  uint32
	Want uint32
}

func (e *ConstructorMismatchError) Error() string {
	return fmt.Sprintf("tl: constructor mismatch: got %#08x, want %#08x", e.Got, e.Want)
}

func (e *ConstructorMismatchError) Unwrap() error { return ErrConstructorMismatch }

// UnknownConstructorError reports an unregistered constructor ID together with
// the bytes that followed it, mirroring TypeNotFoundError(constructor_id,
// remaining) in the reference implementation.
type UnknownConstructorError struct {
	ID        uint32
	Remaining []byte
}

func (e *UnknownConstructorError) Error() string {
	return fmt.Sprintf("tl: unknown constructor %#08x (%d remaining bytes)", e.ID, len(e.Remaining))
}

func (e *UnknownConstructorError) Unwrap() error { return ErrUnknownConstructor }
