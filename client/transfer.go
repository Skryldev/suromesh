package client

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// TransferID is an immutable, monotonically increasing identifier for one
// transfer. Safe to use as a map key. Contains no secret material.
type TransferID uint64

// TransferState is the transfer lifecycle:
//
//	Pending → Running → Completed | Failed | Cancelled
//
// Terminal states are immutable; exactly one transition to a terminal
// state occurs per transfer (enforced by CAS).
type TransferState int32

const (
	TransferPending TransferState = iota
	TransferRunning
	TransferCompleted
	TransferFailed
	TransferCancelled
)

func (s TransferState) String() string {
	switch s {
	case TransferPending:
		return "pending"
	case TransferRunning:
		return "running"
	case TransferCompleted:
		return "completed"
	case TransferFailed:
		return "failed"
	case TransferCancelled:
		return "cancelled"
	}
	return "unknown"
}

// TransferKind discriminates upload from download.
type TransferKind int32

const (
	TransferUpload TransferKind = iota
	TransferDownload
)

func (k TransferKind) String() string {
	if k == TransferUpload {
		return "upload"
	}
	return "download"
}

// TransferProgress is a point-in-time progress snapshot.
type TransferProgress struct {
	ID      TransferID
	Kind    TransferKind
	Current int64 // bytes transferred so far
	Total   int64 // total bytes (0 = unknown)
}

// transfer is the internal state of one active transfer. Fields guarded by
// the manager mutex or atomics as noted.
type transfer struct {
	id     TransferID
	kind   TransferKind
	state  atomic.Int32
	ctx    context.Context    // derived from manager ctx; cancelled on transfer cancel
	cancel context.CancelFunc // cancels ctx

	written int64 // atomic: bytes transferred so far
	total   int64 // immutable: total expected bytes (-1 = unknown)
}

// transition moves from → to; terminal states are immutable.
func (t *transfer) transition(from, to TransferState) bool {
	return t.state.CompareAndSwap(int32(from), int32(to))
}

// terminal reports whether the transfer is in a terminal state.

// TransferConfig bounds the transfer subsystem.
type TransferConfig struct {
	// UploadWorkers bounds parallel part uploads per file (default 1,
	// reference-sequential). Values > 1 are protocol-safe because
	// saveFilePart is idempotent (same file_id+part_index), but the
	// reference uploads sequentially — keep 1 unless throughput demands
	// otherwise.
	UploadWorkers int
	// MaxConcurrentTransfers bounds total active transfers (default 16).
	MaxConcurrentTransfers int
	// ProgressInterval: progress is delivered at most once per this
	// duration per transfer (default 250 ms; 0 = every part).
	ProgressInterval time.Duration
}

const (
	defaultUploadWorkers    = 1
	defaultMaxConcurrent    = 16
	defaultProgressInterval = 250 * time.Millisecond
)

func (c *TransferConfig) fill() {
	if c.UploadWorkers == 0 {
		c.UploadWorkers = defaultUploadWorkers
	}
	if c.MaxConcurrentTransfers == 0 {
		c.MaxConcurrentTransfers = defaultMaxConcurrent
	}
	if c.ProgressInterval == 0 {
		c.ProgressInterval = defaultProgressInterval
	}
}

func (c *TransferConfig) validate() error {
	if c.UploadWorkers < 0 {
		return fmt.Errorf("%w: UploadWorkers must be >= 0", ErrInvalidConfig)
	}
	if c.MaxConcurrentTransfers < 0 {
		return fmt.Errorf("%w: MaxConcurrentTransfers must be >= 0", ErrInvalidConfig)
	}
	if c.ProgressInterval < 0 {
		return fmt.Errorf("%w: ProgressInterval must be >= 0", ErrInvalidConfig)
	}
	return nil
}

// transferManager owns the transfer subsystem: bounded concurrency,
// active-transfer registry, progress dispatch, and shutdown.
type transferManager struct {
	cfg TransferConfig

	sem    chan struct{} // bounded active transfers
	mu     sync.Mutex
	active map[TransferID]*transfer
	nextID atomic.Uint64

	metrics transferMetrics

	stopCh   chan struct{}
	stopOnce sync.Once
	closed   bool
}

// transferMetrics tracks aggregate transfer counters (all atomic).
type transferMetrics struct {
	started   atomic.Uint64
	completed atomic.Uint64
	failed    atomic.Uint64
	cancelled atomic.Uint64
	bytesUp   atomic.Uint64
	bytesDown atomic.Uint64
	retriesUp atomic.Uint64
	retriesDn atomic.Uint64
}

func newTransferManager(cfg TransferConfig) (*transferManager, error) {
	cfg.fill()
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	tm := &transferManager{
		cfg:    cfg,
		sem:    make(chan struct{}, cfg.MaxConcurrentTransfers),
		active: make(map[TransferID]*transfer),
		stopCh: make(chan struct{}),
	}
	return tm, nil
}

// begin reserves a concurrency slot and registers the transfer.
// Returns the transfer, its per-transfer context, and a release function
// that MUST be called exactly once when the transfer settles.
func (tm *transferManager) begin(ctx context.Context, kind TransferKind, total int64) (*transfer, context.Context, func(), error) {
	select {
	case tm.sem <- struct{}{}:
	case <-ctx.Done():
		return nil, nil, nil, ctx.Err()
	case <-tm.stopCh:
		return nil, nil, nil, ErrTransferClosed
	}

	id := TransferID(tm.nextID.Add(1))
	tCtx, tCancel := context.WithCancel(ctx)
	t := &transfer{
		id:     id,
		kind:   kind,
		ctx:    tCtx,
		cancel: tCancel,
		total:  total,
	}
	t.state.Store(int32(TransferPending))

	tm.mu.Lock()
	if tm.closed {
		tm.mu.Unlock()
		tCancel()
		<-tm.sem
		return nil, nil, nil, ErrTransferClosed
	}
	tm.active[id] = t
	tm.mu.Unlock()

	tm.metrics.started.Add(1)

	release := func() {
		tCancel()
		tm.mu.Lock()
		delete(tm.active, id)
		tm.mu.Unlock()
		<-tm.sem
	}
	return t, tCtx, release, nil
}

// cancelTransfer cancels one active transfer by ID.
func (tm *transferManager) cancelTransfer(id TransferID) bool {
	tm.mu.Lock()
	t, ok := tm.active[id]
	tm.mu.Unlock()
	if !ok {
		return false
	}
	t.cancel()
	return true
}

// finish marks a transfer terminal (exactly once) and updates metrics.
func (tm *transferManager) finish(t *transfer, state TransferState) {
	if !t.transition(TransferRunning, state) &&
		!t.transition(TransferPending, state) {
		return // already terminal
	}
	switch state {
	case TransferCompleted:
		tm.metrics.completed.Add(1)
	case TransferFailed:
		tm.metrics.failed.Add(1)
	case TransferCancelled:
		tm.metrics.cancelled.Add(1)
	}
}

// reportProgress dispatches progress to the caller's callback without
// holding any manager lock. The caller's callback is invoked inline (it
// is expected to be fast); panic isolation protects the worker.
func (tm *transferManager) reportProgress(t *transfer, fn func(TransferProgress), current, total int64) {
	if fn == nil {
		return
	}
	defer func() { _ = recover() }() // panic isolation (no payload: may be user content)
	fn(TransferProgress{ID: t.id, Kind: t.kind, Current: current, Total: total})
}

// stop shuts the manager down: no new transfers begin, active transfers
// are cancelled.
func (tm *transferManager) stop() {
	tm.stopOnce.Do(func() {
		close(tm.stopCh)
		tm.mu.Lock()
		tm.closed = true
		ids := make([]TransferID, 0, len(tm.active))
		for id := range tm.active {
			ids = append(ids, id)
		}
		tm.mu.Unlock()
		for _, id := range ids {
			tm.cancelTransfer(id)
		}
	})
}

// MetricsSnapshot returns transfer counters (concurrency-safe).
func (tm *transferManager) snapshot() TransferMetricsSnapshot {
	return TransferMetricsSnapshot{
		Started:         tm.metrics.started.Load(),
		Completed:       tm.metrics.completed.Load(),
		Failed:          tm.metrics.failed.Load(),
		Cancelled:       tm.metrics.cancelled.Load(),
		BytesUp:         tm.metrics.bytesUp.Load(),
		BytesDown:       tm.metrics.bytesDown.Load(),
		UploadRetries:   tm.metrics.retriesUp.Load(),
		DownloadRetries: tm.metrics.retriesDn.Load(),
		Active:          int64(len(tm.activeList())),
	}
}

// activeList returns the active transfer IDs.
func (tm *transferManager) activeList() []TransferID {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	out := make([]TransferID, 0, len(tm.active))
	for id := range tm.active {
		out = append(out, id)
	}
	return out
}

// TransferMetricsSnapshot is a point-in-time transfer counter set.
type TransferMetricsSnapshot struct {
	Started         uint64
	Completed       uint64
	Failed          uint64
	Cancelled       uint64
	BytesUp         uint64
	BytesDown       uint64
	UploadRetries   uint64
	DownloadRetries uint64
	Active          int64
}

// TransferMetrics returns a snapshot of the transfer counters
// (concurrency-safe; cheap).
func (c *Client) TransferMetrics() TransferMetricsSnapshot {
	return c.transfers.snapshot()
}

var (
	_ = errors.New
	_ = fmt.Sprintf
)
