// Package domain holds the SuroMesh Service's pure domain model.
//
// BOUNDARY (enforced by boundary_test.go): this package must not import
// the SuroMesh SDK, protobuf, gRPC, or any infrastructure package. It is
// the vocabulary every other layer shares.
package domain

import "time"

// AccountID identifies one bound Soroush Plus account. It is an opaque
// UUID string; the mapping to upstream identities (phone numbers) never
// enters the domain model.
type AccountID string

func (a AccountID) String() string { return string(a) }

// AccountStatus is the operator-facing binding status (design:
// domain-model.md).
type AccountStatus int32

const (
	AccountStatusUnspecified AccountStatus = 0
	AccountStatusPendingAuth AccountStatus = 1
	AccountStatusActive      AccountStatus = 2
	AccountStatusSuspended   AccountStatus = 3
	AccountStatusRemoved     AccountStatus = 4
)

func (s AccountStatus) String() string {
	switch s {
	case AccountStatusPendingAuth:
		return "PENDING_AUTH"
	case AccountStatusActive:
		return "ACTIVE"
	case AccountStatusSuspended:
		return "SUSPENDED"
	case AccountStatusRemoved:
		return "REMOVED"
	default:
		return "UNSPECIFIED"
	}
}

// DesiredState is what the operator wants; ObservedState is what the
// upstream actually is. The AccountRunner reconciles the two (S5).
type DesiredState int32

const (
	DesiredStopped DesiredState = 0
	DesiredRunning DesiredState = 1
)

func (s DesiredState) String() string {
	if s == DesiredRunning {
		return "RUNNING"
	}
	return "STOPPED"
}

// ObservedState describes the live upstream connection per account.
type ObservedState int32

const (
	ObservedDisconnected ObservedState = 0
	ObservedConnecting   ObservedState = 1
	ObservedConnected    ObservedState = 2
	ObservedReady        ObservedState = 3
)

func (s ObservedState) String() string {
	switch s {
	case ObservedConnecting:
		return "CONNECTING"
	case ObservedConnected:
		return "CONNECTED"
	case ObservedReady:
		return "READY"
	default:
		return "DISCONNECTED"
	}
}

// ConnectionObservation pairs desired and observed state with a
// sanitized error class (never a payload).
type ConnectionObservation struct {
	Desired        DesiredState
	Observed       ObservedState
	LastErrorClass string
	ChangedAt      time.Time
}

// AuthState mirrors the SDK authorization axis (distinct from
// connectivity).
type AuthState int32

const (
	AuthUnauthorized     AuthState = 0
	AuthCodePending      AuthState = 1
	AuthPasswordRequired AuthState = 2
	AuthAuthorized       AuthState = 3
	AuthRevoked          AuthState = 4
)

func (s AuthState) String() string {
	switch s {
	case AuthCodePending:
		return "CODE_PENDING"
	case AuthPasswordRequired:
		return "PASSWORD_REQUIRED"
	case AuthAuthorized:
		return "AUTHORIZED"
	case AuthRevoked:
		return "REVOKED"
	default:
		return "UNAUTHORIZED"
	}
}
