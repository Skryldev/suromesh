package rpc

import "sync/atomic"

// Metrics is the request-engine counter block. Always allocated, one atomic
// add per event; no locks, no framework dependency, negligible when unused.
type Metrics struct {
	RequestsStarted    atomic.Uint64
	RequestsCompleted  atomic.Uint64
	RequestsRPCError   atomic.Uint64
	RequestsTimedOut   atomic.Uint64
	RequestsCancelled  atomic.Uint64
	RequestsConnLost   atomic.Uint64
	UnknownResponses   atomic.Uint64 // late or never-registered msg IDs
	DuplicateResponses atomic.Uint64 // second completion attempt lost the race
	ProtocolErrors     atomic.Uint64 // malformed envelopes/objects from the wire
	AcksSent           atomic.Uint64
	Resends            atomic.Uint64 // bad-server-salt recoveries
	SaltUpdates        atomic.Uint64
	UpdatesDropped     atomic.Uint64 // updates channel saturation drops
	EnvelopeErrors     atomic.Uint64 // decrypt/auth failures on inbound envelopes
	PendingLimitHits   atomic.Uint64 // invokes rejected at the pending bound
}

// Snapshot is a stable copy for exposition.
type Snapshot struct {
	RequestsStarted, RequestsCompleted   uint64
	RequestsRPCError, RequestsTimedOut   uint64
	RequestsCancelled, RequestsConnLost  uint64
	UnknownResponses, DuplicateResponses uint64
	ProtocolErrors, AcksSent             uint64
	Resends, SaltUpdates                 uint64
	UpdatesDropped, EnvelopeErrors       uint64
	PendingLimitHits                     uint64
}

// Snapshot copies the counters.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		RequestsStarted:    m.RequestsStarted.Load(),
		RequestsCompleted:  m.RequestsCompleted.Load(),
		RequestsRPCError:   m.RequestsRPCError.Load(),
		RequestsTimedOut:   m.RequestsTimedOut.Load(),
		RequestsCancelled:  m.RequestsCancelled.Load(),
		RequestsConnLost:   m.RequestsConnLost.Load(),
		UnknownResponses:   m.UnknownResponses.Load(),
		DuplicateResponses: m.DuplicateResponses.Load(),
		ProtocolErrors:     m.ProtocolErrors.Load(),
		AcksSent:           m.AcksSent.Load(),
		Resends:            m.Resends.Load(),
		SaltUpdates:        m.SaltUpdates.Load(),
		UpdatesDropped:     m.UpdatesDropped.Load(),
		EnvelopeErrors:     m.EnvelopeErrors.Load(),
		PendingLimitHits:   m.PendingLimitHits.Load(),
	}
}
