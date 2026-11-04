package client

import (
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/transport"
	"sync/atomic"
)

// Metrics is a point-in-time, concurrency-safe snapshot of every
// operationally relevant counter across the client's layers. Zero-value
// fields are meaningful (nothing happened yet).
//
// Cardinality policy: these counters carry NO labels — no user IDs, chat
// IDs, message IDs, or key material. Per-method labels are intentionally
// omitted from the built-in snapshot to keep cardinality bounded; callers
// needing them can derive them from Hooks.OnRPCStart/Complete which expose
// the bounded constructor-name string.
//
// Secret protection: no field ever contains auth keys, nonces, AES key/IV
// material, or encrypted payloads. Only derived identifiers (key ID) and
// counts appear.
type Metrics struct {
	// Lifecycle / transport.
	State            State
	TransportState   transport.State
	ConnectionsTotal uint64
	Reconnects       uint64
	ConnFailures     uint64
	BytesSent        uint64
	BytesReceived    uint64

	// RPC.
	InvokesStarted   uint64
	InvokesCompleted uint64
	InvokeErrors     uint64
	InvokeRetries    uint64
	PendingRequests  int
	PendingLimit     int
	PendingLimitHits uint64

	// Updates.
	UpdatesReceived           uint64
	UpdatesAccepted           uint64
	UpdatesDelivered          uint64
	UpdatesDuplicate          uint64
	UpdatesStale              uint64
	UpdatesDropped            uint64
	UpdatesUnsupported        uint64
	UpdateGaps                uint64
	DifferenceRequests        uint64
	ChannelDifferenceRequests uint64
	DifferenceFailures        uint64
	UpdateQueueDepth          int64
	UpdateQueueCap            int
	UpdateWorkers             int
	UpdateSubscribers         int

	// Entities.
	EntityCacheEntries int

	// Session.
	SessionState string

	// Authorization (aggregate counters only — never phone numbers, codes,
	// hashes, keys, or user IDs).
	AuthSendCodeTotal    uint64
	AuthSignInTotal      uint64
	AuthSuccessTotal     uint64
	AuthFailureTotal     uint64
	AuthPasswordReqTotal uint64
	AuthLogoutTotal      uint64
	AuthFloodWaitTotal   uint64
	AuthState            string
}

// Metrics snapshots every layer. Cost: one atomic load per counter plus a
// few RLocks — safe to poll at metering intervals (e.g. 1–15 s); not
// designed for per-request hot-path polling.
func (c *Client) Metrics() Metrics {
	m := Metrics{State: c.sm.get()}

	c.mu.RLock()
	tr, eng, upd := c.tr, c.eng, c.updates
	c.mu.RUnlock()

	if tr != nil {
		m.TransportState = tr.State()
		ts := tr.Metrics().Snapshot()
		m.ConnectionsTotal = ts.ConnOpened
		m.Reconnects = ts.Reconnects
		m.ConnFailures = ts.ReconnectFailures
		m.BytesSent = ts.BytesSent
		m.BytesReceived = ts.BytesReceived
	}
	if eng != nil {
		es := eng.Metrics().Snapshot()
		m.InvokesStarted = es.RequestsStarted
		m.InvokesCompleted = es.RequestsCompleted
		m.InvokeErrors = es.RequestsRPCError + es.RequestsTimedOut +
			es.RequestsCancelled + es.RequestsConnLost + es.ProtocolErrors
		m.InvokeRetries = es.Resends
		m.PendingRequests = eng.Pending()
		m.PendingLimit = eng.MaxPending()
		m.PendingLimitHits = es.PendingLimitHits
	}
	if upd != nil {
		recv, acc, _del, drop, subs, depth := upd.counters()
		m.UpdatesReceived = uint64(recv)
		m.UpdatesAccepted = uint64(acc)
		m.UpdatesDelivered = uint64(_del)
		m.UpdatesDropped = uint64(drop)
		m.UpdateQueueDepth = depth
		m.UpdateSubscribers = int(subs)
		m.UpdateQueueCap = c.cfg.UpdateQueue
		m.UpdateWorkers = c.cfg.UpdateWorkers
		m.UpdatesDuplicate = uint64(atomic.LoadInt64(&upd.duplicates))
		m.UpdatesStale = uint64(atomic.LoadInt64(&upd.stale))
		m.UpdatesUnsupported = uint64(atomic.LoadInt64(&upd.unsupported))
		m.UpdateGaps = uint64(atomic.LoadInt64(&upd.gaps))
		m.DifferenceRequests = uint64(atomic.LoadInt64(&upd.diffRequests))
		m.ChannelDifferenceRequests = uint64(atomic.LoadInt64(&upd.chanDiffReq))
		m.DifferenceFailures = uint64(atomic.LoadInt64(&upd.diffFailures))
	}

	if c.entities != nil {
		c.entities.mu.RLock()
		m.EntityCacheEntries = len(c.entities.m)
		c.entities.mu.RUnlock()
	}
	m.SessionState = c.sess.AuthState().String()
	m.AuthSendCodeTotal = c.authMetrics.sendCode.Load()
	m.AuthSignInTotal = c.authMetrics.signIn.Load()
	m.AuthSuccessTotal = c.authMetrics.success.Load()
	m.AuthFailureTotal = c.authMetrics.failure.Load()
	m.AuthPasswordReqTotal = c.authMetrics.passwordRequired.Load()
	m.AuthLogoutTotal = c.authMetrics.logout.Load()
	m.AuthFloodWaitTotal = c.authMetrics.floodWait.Load()
	m.AuthState = c.AuthState().String()
	return m
}

// EngineMetricsDebug returns the last protocol decode error text (temp).
func (c *Client) EngineMetricsDebug() string {
	c.mu.RLock()
	eng := c.eng
	c.mu.RUnlock()
	if eng == nil {
		return "(no engine)"
	}
	return eng.DebugProtoErr()
}

// EngineMetrics exposes the rpc engine's raw snapshot (diagnostics/tests).
func (c *Client) EngineMetrics() rpc.Snapshot {
	c.mu.RLock()
	eng := c.eng
	c.mu.RUnlock()
	if eng == nil {
		return rpc.Snapshot{}
	}
	return eng.Metrics().Snapshot()
}
