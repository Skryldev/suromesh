package transport

import (
	"sync/atomic"
	"time"
)

// Metrics is a fixed-size, always-present counter block. Reading it is a
// snapshot; recording costs one atomic add per event — negligible when
// unused (no allocations, no locks).
//
// A Hooks function may be installed to observe events as they happen; nil
// hooks mean zero observable overhead beyond the atomic counters.
type Metrics struct {
	ConnOpened        atomic.Uint64
	ConnClosed        atomic.Uint64
	Reconnects        atomic.Uint64
	ReconnectFailures atomic.Uint64
	BytesSent         atomic.Uint64
	BytesReceived     atomic.Uint64
	PacketsSent       atomic.Uint64
	PacketsReceived   atomic.Uint64
	PacketsDelivered  atomic.Uint64
	QueueFullEvents   atomic.Uint64 // outbound rejections/backpressure stalls
	ProtocolErrors    atomic.Uint64
	PingsSent         atomic.Uint64
	PongsReceived     atomic.Uint64
}

// Hooks observes transport events. Every hook is optional; set only what
// you need. Hooks must not block — they are called inline on I/O paths.
type Hooks struct {
	OnStateChange func(from, to State)
	// OnReadLatency / OnWriteLatency report per-packet durations.
	OnReadLatency  func(d time.Duration)
	OnWriteLatency func(d time.Duration)
	// OnQueueDepth samples queue depths periodically (heartbeat tick).
	OnQueueDepth func(outbound, inbound int)
	// OnError observes every classified error before policy applies.
	OnError func(err error, class Classification)
}

// Snapshot is a stable copy for exposition.
type Snapshot struct {
	ConnOpened, ConnClosed        uint64
	Reconnects, ReconnectFailures uint64
	BytesSent, BytesReceived      uint64
	PacketsSent, PacketsReceived  uint64
	PacketsDelivered              uint64
	QueueFullEvents               uint64
	ProtocolErrors                uint64
	PingsSent, PongsReceived      uint64
}

// Snapshot copies the counters.
func (m *Metrics) Snapshot() Snapshot {
	return Snapshot{
		ConnOpened:        m.ConnOpened.Load(),
		ConnClosed:        m.ConnClosed.Load(),
		Reconnects:        m.Reconnects.Load(),
		ReconnectFailures: m.ReconnectFailures.Load(),
		BytesSent:         m.BytesSent.Load(),
		BytesReceived:     m.BytesReceived.Load(),
		PacketsReceived:   m.PacketsReceived.Load(),
		PacketsSent:       m.PacketsSent.Load(),
		PacketsDelivered:  m.PacketsDelivered.Load(),
		QueueFullEvents:   m.QueueFullEvents.Load(),
		ProtocolErrors:    m.ProtocolErrors.Load(),
		PingsSent:         m.PingsSent.Load(),
		PongsReceived:     m.PongsReceived.Load(),
	}
}
