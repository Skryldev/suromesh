package client

// Update engine (Phase 10): a full MTProto update state machine on top of
// the Phase 7 delivery mechanism.
//
// Architecture (docs/updates.md):
//
//	engine.Updates() ─► rawQueue(bounded) ─► syncLoop (single goroutine,
//	                     owns the MessageBox) ─► eventQueue(bounded) ─►
//	                     N delivery workers ─► subscriber fan-out
//
// Protocol ordering lives in the syncLoop: the MessageBox mutates and
// emits strictly in stream order, difference recovery runs inline (so at
// most one difference mutates state at a time), and slow subscribers can
// never block synchronization (bounded event queue + drop policy between
// the two stages, Phase 7 semantics preserved).
//
// Reference: reference/SPlusthon/splusthon/client/updates.py (_update_loop
// 267-519) and _updates/messagebox.py; deviations are listed in
// docs/updates.md and flagged inline.

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// UpdateHandler receives protocol updates. Handlers run on the update
// worker pool (never the network reader, never the state machine); each
// protocol-consistent update is delivered to every registered handler
// exactly once, in MessageBox-accepted order. A panicking handler is
// recovered and reported via Hooks.OnSubscriberPanic — never fatal.
type UpdateHandler func(ctx context.Context, upd tl.Object)

// Subscription is an update registration; Unsubscribe is idempotent.
type Subscription struct {
	mu     sync.Mutex
	active bool
	remove func()
}

// Unsubscribe removes the handler. Safe concurrently and after stop.
func (s *Subscription) Unsubscribe() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		s.active = false
		s.remove()
	}
}

// Update engine error surface (§21).
var (
	// ErrUpdateQueueFull reports the bounded raw queue rejecting an update
	// under extreme load (drop-oldest policy keeps the pipeline live).
	ErrUpdateQueueFull = errors.New("updates: queue full")
	// ErrDifferenceFailed reports a difference request that could not
	// complete; recovery retries on the next trigger (reference ends the
	// difference and waits for the next gap or timeout).
	ErrDifferenceFailed = errors.New("updates: difference sync failed")
)

// updateManager implements the update subsystem: state synchronization
// (syncLoop + MessageBox) independent from subscriber delivery (workers).
type updateManager struct {
	workers int
	queue   int
	hooks   Hooks
	eng     *rpc.Engine // difference RPCs (set at start)
	client  *Client     // entity cache + auth-state effects (set at start)

	// Subscriber fan-out (Phase 7 machinery, unchanged guarantees).
	regMu    sync.RWMutex
	handlers map[int64]UpdateHandler
	nextID   int64

	wg       sync.WaitGroup
	events   chan tl.Object
	stopCh   chan struct{}
	stopOnce sync.Once

	// Synchronization state (owned by syncLoop).
	box   *messageBox
	rawQ  chan tl.Object
	rawWg sync.WaitGroup

	// Persistence callback: receives the MessageBox state to store
	// (coalesced by the loop's periodic flush; never blocks the stream).
	persist          func(ss mbSessionState, cs []mbChannelState)
	persistDirty     atomic.Bool
	lastPersistCheck int64

	// Counters (aggregate only; atomic).
	received     int64 // raw updates seen from the engine
	accepted     int64 // dispatched to subscribers
	dropped      int64 // event-queue overflow (Phase 7 policy)
	queueLen     int64 // event queue depth
	duplicates   int64 // pts/seq duplicates ignored
	stale        int64 // updates skipped as already-handled (seq gate)
	gaps         int64 // gaps detected
	diffRequests int64 // getDifference calls
	chanDiffReq  int64 // getChannelDifference calls
	diffFailures int64 // difference RPC failures
	unsupported  int64 // non-Updates-family objects seen (fail-explicit)
}

func newUpdateManager(workers, queue int, hooks Hooks) *updateManager {
	if workers <= 0 {
		workers = 1
	}
	if queue <= 0 {
		queue = 1024
	}
	return &updateManager{
		workers:  workers,
		queue:    queue,
		hooks:    hooks,
		handlers: make(map[int64]UpdateHandler),
		stopCh:   make(chan struct{}),
		box:      newMessageBox(),
	}
}

// slowConsumerGrace bounds how long the event feeder waits when the
// subscriber queue is full before applying the drop-newest policy
// (Phase 7 semantics, kept unchanged).
const slowConsumerGrace = 250 * time.Millisecond

// rawQueueCapacity bounds the raw input queue between the engine channel
// and the syncLoop (§23: no unbounded structures).
const rawQueueCapacity = 4096

// deadlinePoll is the deadline-check granularity; the reference computes
// exact per-deadline waits, the Go loop polls — POSSIBLE_GAP_TIMEOUT is
// 100 ms, so a 25 ms poll resolves gaps promptly without timer churn.
const deadlinePoll = 25 * time.Millisecond

func (m *updateManager) start(src <-chan tl.Object, eng *rpc.Engine) {
	m.eng = eng
	m.events = make(chan tl.Object, m.queue)
	m.rawQ = make(chan tl.Object, rawQueueCapacity)

	m.wg.Add(m.workers)
	for i := 0; i < m.workers; i++ {
		go m.worker()
	}
	// The sync loop (and its raw feeder) wait on rawWg; delivery workers
	// on wg. stop() waits for both groups in order.
	m.rawWg.Add(2)
	go m.syncLoop(src)
}

// rawFeeder moves updates from the engine channel into the bounded raw
// queue. Overflow applies drop-oldest (the newest update is the most
// likely to carry the pts that triggers recovery; dropping the oldest
// preserves the pts frontier — the inverse of the subscriber-side
// drop-newest policy, which guards delivery ordering instead).
func (m *updateManager) rawFeeder(src <-chan tl.Object) {
	defer m.rawWg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case upd, ok := <-src:
			if !ok {
				return
			}
			atomic.AddInt64(&m.received, 1)
			if m.hooks.OnUpdateReceived != nil {
				m.hooks.OnUpdateReceived(updCtor(upd))
			}
			select {
			case m.rawQ <- upd:
			default:
				// Drop oldest: evict one queued update to admit the new one.
				select {
				case <-m.rawQ:
				default:
				}
				select {
				case m.rawQ <- upd:
				default:
				}
			}
		}
	}
}

func updCtor(o tl.Object) uint32 {
	if ou, ok := o.(rpc.OwnUpdate); ok {
		return ou.Obj.CtorID()
	}
	return o.CtorID()
}

// syncLoop owns the MessageBox and the difference-recovery flow — the
// port of _update_loop (updates.py:267-519). Single-threaded by design:
// state mutations are strictly ordered and at most one difference runs
// at a time.
func (m *updateManager) syncLoop(src <-chan tl.Object) {
	defer m.rawWg.Done()
	defer m.flushPersist()
	ticker := time.NewTicker(deadlinePoll)
	defer ticker.Stop()

	go m.rawFeeder(src)

	for {
		// 1. Difference recovery has absolute priority while needed
		// (updates.py:300-471: difference is checked before draining the
		// queue, so gaps are repaired before fresh updates are applied).
		if m.runDifferenceIfNeeded() {
			continue
		}

		// 2. Drain the raw queue (bounded per pass so deadlines and Close
		// stay responsive; updates.py:473-492 drains in one pass — the
		// bounded slice is a Go-side adaptation for cancellation).
		drained := 0
		for drained < rawQueueCapacity {
			select {
			case <-m.stopCh:
				return
			case obj := <-m.rawQ:
				m.ingest(obj)
				drained++
				continue
			default:
			}
			break
		}
		if drained > 0 {
			continue
		}

		// 3. Wait for work, a deadline tick, or shutdown.
		select {
		case <-m.stopCh:
			return
		case obj := <-m.rawQ:
			m.ingest(obj)
		case <-ticker.C:
			m.box.checkDeadlines()
			m.maybePersist()
		}
	}
}

// ingest normalizes and processes one incoming object through the
// MessageBox, then feeds accepted updates to the subscriber stage and
// seeds the entity cache (updates.py:482-513 + _preprocess_updates).
func (m *updateManager) ingest(obj tl.Object) {
	batch, ok := normalizeUpdates(obj, false)
	if !ok {
		// §7: never silently discard an update family. Non-Updates objects
		// on the updates channel are counted and surfaced via the hook.
		atomic.AddInt64(&m.unsupported, 1)
		m.fireHook(func() {
			if m.hooks.OnUpdateGap != nil {
				m.hooks.OnUpdateGap("unsupported family")
			}
		})
		return
	}

	beforeGaps := m.box.gapsCount()
	var res processUpdatesResult
	err := m.box.processUpdates(batch, &res)
	if after := m.box.gapsCount(); after > beforeGaps {
		atomic.AddInt64(&m.gaps, int64(after-beforeGaps))
	}
	if err != nil {
		// GapError: the loop's next pass runs difference recovery
		// (updates.py:487-488 / 509-510: `continue`).
		atomic.AddInt64(&m.gaps, 1)
		m.fireHook(func() {
			if m.hooks.OnUpdateGap != nil {
				m.hooks.OnUpdateGap("gap")
			}
		})
		return
	}

	m.seedEntities(res.Users, res.Chats)
	m.markPersistDirty()
	for _, u := range res.Emit {
		m.syncConversation(u)
		m.emit(u)
	}
	// Own mutations (Phase 12): their updates advance protocol state only
	// (no subscriber emission — Phase 10 semantics) but the DERIVED
	// conversation store still reflects the caller's own sends/edits
	// (official-app behavior: your own message becomes the top message).
	if batch.Self {
		for _, u := range batch.Updates {
			m.syncConversation(u)
		}
	}
}

// syncConversation applies one ORDERED update to the derived
// conversation store. It runs inside the sync loop — the single
// authoritative ordering point — so conversation state never races with
// the MessageBox's view of the stream (§18/§19).
func (m *updateManager) syncConversation(u tl.Object) {
	if m.client == nil || m.client.conversations == nil {
		return
	}
	cs := m.client.conversations
	switch t := u.(type) {
	case *gen.UpdateNewMessage:
		if msg, ok := t.Message.(*gen.Message); ok && msg.PeerID != nil {
			cs.onNewMessage(peerIDFromWire(msg.PeerID), int64(msg.ID), msg.Out)
		}
	case *gen.UpdateNewChannelMessage:
		if msg, ok := t.Message.(*gen.Message); ok && msg.PeerID != nil {
			cs.onNewMessage(peerIDFromWire(msg.PeerID), int64(msg.ID), msg.Out)
		}
	case *gen.UpdateEditMessage:
		if msg, ok := t.Message.(*gen.Message); ok && msg.PeerID != nil {
			cs.onEditMessage(peerIDFromWire(msg.PeerID), int64(msg.ID))
		}
	case *gen.UpdateDeleteChannelMessages:
		// Channel deletes are attributable; plain UpdateDeleteMessages
		// carries no peer, so it cannot move per-conversation state (the
		// next dialog fetch re-syncs the server numbers).
		cs.onDeleteMessages(PeerID{PeerChannel, t.ChannelID})
	case *gen.UpdateReadHistoryInbox:
		cs.onRead(entryPeerID(t.Peer), t.MaxID, true)
	case *gen.UpdateReadHistoryOutbox:
		cs.onRead(entryPeerID(t.Peer), t.MaxID, false)
	case *gen.UpdateReadChannelInbox:
		cs.onRead(PeerID{PeerChannel, t.ChannelID}, t.MaxID, true)
	case *gen.UpdateReadChannelOutbox:
		cs.onRead(PeerID{PeerChannel, t.ChannelID}, t.MaxID, false)
	}
}

// entryPeerID converts a wire Peer to the domain PeerID (zero when nil).
func entryPeerID(p gen.PeerClass) PeerID {
	if p == nil {
		return PeerID{}
	}
	return peerIDFromWire(p)
}

// fireHook runs a hook body with panic isolation: a broken hook must
// never kill the state machine (§20).
func (m *updateManager) fireHook(body func()) {
	defer func() { _ = recover() }()
	body()
}

// slowConsumerTimer is the sync loop's reusable grace timer (timer
// audit, Phase 15: the overflow path may fire per update under sustained
// slow-consumer pressure; allocating a one-shot timer per emit both
// churns the heap and risks timer-pileup. A single Stop/Reset pair owned
// by the sync loop is safe: emit is only called from the sync loop).
var _ = slowConsumerGrace

// emit pushes one accepted update into the subscriber stage (bounded;
// Phase 7 overflow policy). Called only from the sync loop.
func (m *updateManager) emit(u tl.Object) {
	atomic.AddInt64(&m.queueLen, 1)
	timer := time.NewTimer(slowConsumerGrace)
	defer timer.Stop()
	select {
	case m.events <- u:
	case <-timer.C:
		n := atomic.AddInt64(&m.dropped, 1)
		if m.hooks.OnSlowConsumer != nil {
			m.hooks.OnSlowConsumer(int(atomic.LoadInt64(&m.queueLen)), int(n))
		}
	}
}

// runDifferenceIfNeeded executes pending difference requests, applying
// results through the same pipeline (updates.py:300-471). Returns true
// when recovery progressed and the caller should loop immediately.
func (m *updateManager) runDifferenceIfNeeded() bool {
	if m.eng == nil {
		return false
	}
	// Account difference first (the reference checks account before
	// channel; updates.py:300 / 356).
	if req, ok := m.box.getDifference(); ok {
		atomic.AddInt64(&m.diffRequests, 1)
		ctx, cancel := m.opCtx()
		res, err := m.eng.Invoke(ctx, req)
		cancel()
		if err != nil {
			atomic.AddInt64(&m.diffFailures, 1)
			m.handleDifferenceError(err, func() { m.box.endDiffPair() })
			return true
		}
		m.fireHook(func() {
			if m.hooks.OnDifferenceSync != nil {
				m.hooks.OnDifferenceSync("account")
			}
		})
		var out processUpdatesResult
		if err := m.box.applyDifference(res, &out); err != nil {
			atomic.AddInt64(&m.diffFailures, 1)
			m.box.endDiffPair()
			return true
		}
		m.seedEntities(out.Users, out.Chats)
		m.markPersistDirty()
		for _, u := range out.Emit {
			m.emit(u)
		}
		return true
	}

	// Channel difference: one channel at a time (the reference picks the
	// first int entry; updates.py:356-471).
	req, entry, ok := m.box.getChannelDifference(m.resolveInputChannel)
	if !ok {
		return false
	}
	atomic.AddInt64(&m.chanDiffReq, 1)
	ctx, cancel := m.opCtx()
	res, err := m.eng.Invoke(ctx, req)
	cancel()
	if err != nil {
		atomic.AddInt64(&m.diffFailures, 1)
		m.handleChannelDifferenceError(entry, err)
		return true
	}
	m.fireHook(func() {
		if m.hooks.OnDifferenceSync != nil {
			m.hooks.OnDifferenceSync("channel")
		}
	})
	var out processUpdatesResult
	if err := m.box.applyChannelDifference(entry, res, &out); err != nil {
		atomic.AddInt64(&m.diffFailures, 1)
		m.box.endChannelDifferencePremature(entry, mbEndTemporary)
		return true
	}
	m.seedEntities(out.Users, out.Chats)
	m.markPersistDirty()
	for _, u := range out.Emit {
		m.emit(u)
	}
	return true
}

// handleDifferenceError applies the reference's account-difference error
// matrix (updates.py:305-347): transient → end diff and retry on the
// next trigger; 401-class → auth state effects; everything else → end
// diff and continue (fail-explicit metrics, never a silent drop).
func (m *updateManager) handleDifferenceError(err error, end func()) {
	var rpcErr *rpcErrorAlias
	if errors.As(err, &rpcErr) {
		switch {
		case rpcErr.Code == 401:
			end()
			if m.client != nil {
				m.client.handleAuthReject(err)
			}
			return
		case rpcErr.Code == 420:
			// FloodWait: the engine's retry policy may have already slept;
			// treat like transient server issues (updates.py:305-314).
			end()
			return
		case rpcErr.Code >= 500:
			end()
			return
		}
	}
	// Transport/context failures (network down): the reference sleeps 5 s
	// and continues; the Go loop ends the difference and relies on the
	// deadline machinery (retry at the next gap or 15-min timeout —
	// bounded, loop-free).
	end()
}

// handleChannelDifferenceError applies the channel-difference error matrix
// (updates.py:361-463): PersistentTimestampOutdated/Invalid, ServerError,
// TimedOut, FloodWait → temporary end (pts unchanged, retried later);
// ChannelPrivate/ChannelInvalid → banned (forget the channel); 401 →
// auth effects; fallback → temporary.
func (m *updateManager) handleChannelDifferenceError(entry mbEntry, err error) {
	var rpcErr *rpcErrorAlias
	if errors.As(err, &rpcErr) {
		switch rpcErr.Type {
		case "CHANNEL_PRIVATE", "CHANNEL_INVALID":
			m.box.endChannelDifferencePremature(entry, mbEndBanned)
			return
		}
		if rpcErr.Code == 401 {
			m.box.endChannelDifferencePremature(entry, mbEndTemporary)
			if m.client != nil {
				m.client.handleAuthReject(err)
			}
			return
		}
	}
	m.box.endChannelDifferencePremature(entry, mbEndTemporary)
}

// resolveInputChannel adapts the entity cache to the MessageBox's channel
// difference needs: the access hash MUST come from the cache — never
// fabricated (messagebox.py:735-743 forgets the entry when unknown).
func (m *updateManager) resolveInputChannel(channelID int64) (*gen.InputChannel, bool) {
	if m.client == nil {
		return nil, false
	}
	ent, ok := m.client.entities.get(PeerID{PeerChannel, channelID})
	if !ok {
		return nil, false
	}
	if ch, ok := ent.(*Channel); ok {
		return &gen.InputChannel{ChannelID: channelID, AccessHash: ch.AccessHash}, true
	}
	return nil, false
}

// opCtx derives a difference-RPC context: cancelled on Close (bounded
// recovery; §22 cancellation requirements).
func (m *updateManager) opCtx() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

// seedEntities feeds users/chats from update envelopes into the entity
// cache (reference _preprocess_updates:22-23 + Phase 6 rules). Never
// fabricates: only server-provided entities are stored.
func (m *updateManager) seedEntities(users []gen.UserClass, chats []gen.ChatClass) {
	if m.client == nil || (len(users) == 0 && len(chats) == 0) {
		return
	}
	m.client.entities.seedUpdateEntities(users, chats)
}

func (m *updateManager) markPersistDirty() {
	m.persistDirty.Store(true)
}

// maybePersist flushes coalesced update state to the session (the
// reference defers SQLite persistence to the keepalive loop;
// updates.py:298 and 521-531 — never persist per update).
func (m *updateManager) maybePersist() { m.persistNow(false) }

// flushPersist forces the persistence of any dirty update state
// (shutdown path -- bypasses the rate limiter so the persisted frontier
// never trails the applied state).
func (m *updateManager) flushPersist() { m.persistNow(true) }

func (m *updateManager) persistNow(force bool) {
	if m.persist == nil {
		return
	}
	// Rate limit BEFORE consuming the dirty flag: a skipped save must
	// leave the flag set so the next tick (or the forced shutdown flush)
	// retries. Otherwise the disk frontier trails memory. A wall-clock
	// regression (now < last, e.g. NTP correction) is treated as "save
	// now" — never as "within interval" (Phase 15 fuzz finding: a
	// regression would otherwise postpone saves indefinitely).
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&m.lastPersistCheck)
	if !force && now >= last && now-last < int64(persistInterval) {
		return
	}
	atomic.StoreInt64(&m.lastPersistCheck, now)
	if !m.persistDirty.CompareAndSwap(true, false) && !force {
		return
	}
	ss, cs := m.box.sessionState()
	m.persist(ss, cs)
}

func (m *updateManager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.stopCh:
			return
		case upd, ok := <-m.events:
			if !ok {
				return
			}
			atomic.AddInt64(&m.queueLen, -1)
			m.deliver(upd)
		}
	}
}

// deliver fans the update out to all registered handlers with panic
// isolation per handler (Phase 7 semantics unchanged).
func (m *updateManager) deliver(upd tl.Object) {
	m.regMu.RLock()
	handlers := make([]UpdateHandler, 0, len(m.handlers))
	for _, h := range m.handlers {
		handlers = append(handlers, h)
	}
	m.regMu.RUnlock()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	for _, h := range handlers {
		func() {
			defer func() {
				if r := recover(); r != nil && m.hooks.OnSubscriberPanic != nil {
					m.hooks.OnSubscriberPanic()
				}
			}()
			h(ctx, upd)
			atomic.AddInt64(&m.accepted, 1)
		}()
	}
}

// counters (concurrency-safe) for metrics exposure.
func (m *updateManager) counters() (received, accepted, delivered, dropped, subscribers, queueDepth int64) {
	return atomic.LoadInt64(&m.received), atomic.LoadInt64(&m.accepted),
		atomic.LoadInt64(&m.accepted), atomic.LoadInt64(&m.dropped),
		int64(m.subscriberCount()), atomic.LoadInt64(&m.queueLen)
}

func (m *updateManager) subscriberCount() int {
	m.regMu.RLock()
	defer m.regMu.RUnlock()
	return len(m.handlers)
}

// OnUpdate registers a handler and returns its subscription.
func (c *Client) OnUpdate(h UpdateHandler) *Subscription {
	c.mu.RLock()
	m := c.updates
	c.mu.RUnlock()
	if m == nil || c.closed.Load() {
		return &Subscription{} // inert: not connected
	}
	m.regMu.Lock()
	id := m.nextID
	m.nextID++
	m.handlers[id] = h
	m.regMu.Unlock()

	sub := &Subscription{active: true}
	sub.remove = func() {
		m.regMu.Lock()
		delete(m.handlers, id)
		m.regMu.Unlock()
	}
	return sub
}

// stop drains and terminates the subsystem; no handler runs after stop
// returns. Close interrupts: the raw queue wait, the difference RPC (via
// eng.Close), deadline ticks, and subscriber delivery (§22).
func (m *updateManager) stop() {
	m.stopOnce.Do(func() { close(m.stopCh) })
	m.rawWg.Wait()
	m.wg.Wait()
}

// updatesBox exposes the MessageBox for the dialog layer's
// try_set_channel_state integration (nil before connect).
func (c *Client) updatesBox() *messageBox {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.updates == nil {
		return nil
	}
	return c.updates.box
}

// DroppedUpdates reports how many updates the subscriber-stage overflow
// policy dropped (Phase 7 API, unchanged).
func (c *Client) DroppedUpdates() int {
	c.mu.RLock()
	m := c.updates
	c.mu.RUnlock()
	if m == nil {
		return 0
	}
	return int(atomic.LoadInt64(&m.dropped))
}

// rpcErrorAlias is a local alias for the engine's RPC error type.
type rpcErrorAlias = rpc.RPCError

// updNormalizeForTest exposes the normalization seam (test-only use in
// the external test package via this exported shim).
