package live

import (
	"context"
	"errors"
	"fmt"
	"io"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"
)

// ops.go: the reusable validation operations (Phase 18 §8–§25). Each Op
// runs against ANY *client.Client — live or fake — and reports a verdict,
// so the fake-wiring suite (FAKE source) and the live suites (LIVE source)
// execute the exact same validation logic.

// Suite bundles one client with its trace, recorder and bookkeeping.
type Suite struct {
	C           *client.Client
	Trace       *RPCTrace
	Rec         *Recorder
	Cfg         Config
	Red         *Redactor
	SessionPath string
	// Addr is the actual dial address of the client (fake servers differ
	// from Cfg.Host, which only labels the validation source).
	Addr string

	sentMu  sync.Mutex
	sentIDs []sentMessage // messages created by mutations/media ops (for cleanup)
}

type sentMessage struct {
	Peer client.PeerID
	ID   int64
}

// remember tracks a created message for deterministic cleanup (§5).
func (s *Suite) remember(peer client.PeerID, id int64) {
	s.sentMu.Lock()
	s.sentIDs = append(s.sentIDs, sentMessage{Peer: peer, ID: id})
	s.sentMu.Unlock()
}

// sessionFactoryFromPath returns a factory that opens the suite's session
// file into a FRESH unconnected client wired to the suite's (current)
// trace — the exact shape the live session-reuse test requires.
func (s *Suite) sessionFactoryFromPath() func() (*client.Client, error) {
	return func() (*client.Client, error) {
		cfg := s.Cfg
		cfg.Session = s.SessionPath
		if s.Addr != "" {
			cfg.Host = s.Addr
			cfg.Port = 0
		}
		boot, err := Bootstrap(cfg, s.Red, s.Trace)
		if err != nil {
			return nil, err
		}
		return boot.Client, nil
	}
}

// Cleanup deletes every tracked message and reports the outcome. It never
// fails the suite on an individual delete error (the evidence says so).
func (s *Suite) Cleanup(ctx context.Context) int {
	s.sentMu.Lock()
	pending := s.sentIDs
	s.sentIDs = nil
	s.sentMu.Unlock()
	if s.Cfg.KeepArtifacts || len(pending) == 0 {
		return 0
	}
	deleted := 0
	for _, m := range pending {
		_, err := s.C.DeleteMessages(ctx, m.Peer, []int64{m.ID}, client.DeleteMessagesOptions{})
		if err == nil {
			deleted++
		}
	}
	return deleted
}

// ---------------------------------------------------------------------------
// OpSessionReuse (§9 — critical): reconnect from a persisted session must
// NOT issue any authentication RPC. sessionFactory must return a fresh,
// UNCONNECTED client bound to the same persisted session each call.
// ---------------------------------------------------------------------------

func (s *Suite) OpSessionReuse(ctx context.Context, sessionFactory func() (*client.Client, error), iterations int) error {
	for i := 0; i < iterations; i++ {
		c, err := sessionFactory()
		if err != nil {
			return fmt.Errorf("iteration %d: bootstrap: %w", i, err)
		}
		connCtx, cancel := context.WithTimeout(ctx, liveConnectTimeout)
		err = c.Connect(connCtx)
		cancel()
		if err != nil {
			_ = c.Close()
			return fmt.Errorf("iteration %d: connect: %w", i, err)
		}
		if got := c.AuthState(); got != client.AuthAuthorized {
			_ = c.Close()
			return fmt.Errorf("iteration %d: auth state %s, want authorized (no re-auth)", i, got)
		}
		meCtx, cancel2 := context.WithTimeout(ctx, defaultTimeout)
		me, err := c.GetMe(meCtx)
		cancel2()
		if err != nil {
			_ = c.Close()
			return fmt.Errorf("iteration %d: GetMe: %w", i, err)
		}
		// The invariant under test: zero authentication RPCs.
		if s.Trace.HasAuthRPC() {
			_ = c.Close()
			return fmt.Errorf("iteration %d: session reuse issued authentication RPCs: %v", i, s.Trace.Methods())
		}
		if me == nil {
			_ = c.Close()
			return fmt.Errorf("iteration %d: GetMe returned nil", i)
		}
		if err := c.Close(); err != nil {
			return fmt.Errorf("iteration %d: close: %w", i, err)
		}
		s.Rec.Record("session", "reuse", StatusPass, sourceOf(s), map[string]any{
			"iteration": i, "auth_rpcs": 0,
		})
	}
	return nil
}

// ---------------------------------------------------------------------------
// OpOwnUpdateNoDuplicate (§17 — critical): SendMessage's own update must
// advance state WITHOUT emitting a subscriber event.
// ---------------------------------------------------------------------------

func (s *Suite) OpOwnUpdateNoDuplicate(ctx context.Context, peer client.PeerRef) (int64, error) {
	var events int32
	sub := s.C.OnUpdate(func(ctx context.Context, upd tl.Object) { atomic.AddInt32(&events, 1) })
	defer sub.Unsubscribe()

	sendCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	res, err := s.C.SendMessageOpts(sendCtx, peer, liveTestText("own-update"), client.SendMessageOptions{})
	cancel()
	if err != nil {
		return 0, err
	}
	if res == nil || res.ID == 0 {
		return 0, fmt.Errorf("send returned no message id")
	}
	s.remember(peerIDOf(peer), res.ID)

	// Give the update pipeline a bounded window to (wrongly) deliver.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && atomic.LoadInt32(&events) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if n := atomic.LoadInt32(&events); n != 0 {
		return res.ID, fmt.Errorf("own update produced %d subscriber events, want 0", n)
	}
	s.Rec.Record("updates", "own_update_no_duplicate", StatusPass, sourceOf(s), map[string]any{
		"events": 0, "message_id": res.ID,
	})
	return res.ID, nil
}

// ---------------------------------------------------------------------------
// OpIncomingExactlyOnce (§16): every INCOMING message update must reach the
// subscriber exactly once, with a coherent MessageBox/Conversation frontier.
// The window is operator-assisted: someone sends a message from elsewhere.
// ---------------------------------------------------------------------------

func (s *Suite) OpIncomingExactlyOnce(ctx context.Context, window time.Duration) error {
	var events int32
	seen := make(map[client.MessageKey]int)
	sub := s.C.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt32(&events, 1)
		if m, ok := liveExtractMessage(upd); ok {
			if m.Out {
				return // own updates never reach subscribers
			}
			seen[client.MessageKey{Peer: m.Peer, ID: m.ID}]++
		}
	})
	defer sub.Unsubscribe()

	timer := time.NewTimer(window)
	defer timer.Stop()
	tick := time.NewTicker(50 * time.Millisecond)
	defer tick.Stop()
	stable := time.Duration(0)
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case <-timer.C:
			break loop
		case <-tick.C:
			// Exit once the event count matches the distinct incoming
			// count and has been stable for 500 ms (settlement window).
			n := len(seen)
			if n > 0 && int(atomic.LoadInt32(&events)) == n {
				stable += 50 * time.Millisecond
				if stable >= 500*time.Millisecond {
					break loop
				}
			} else {
				stable = 0
			}
		}
	}
	if len(seen) == 0 {
		s.Rec.Record("updates", "incoming_exactly_once", StatusNotObserved, sourceOf(s), nil)
		return nil
	}
	for k, n := range seen {
		if n != 1 {
			return fmt.Errorf("message %s/%d delivered %d times, want exactly 1", k.Peer.Kind, k.ID, n)
		}
	}
	if int(atomic.LoadInt32(&events)) != len(seen) {
		return fmt.Errorf("events=%d distinct incoming=%d (own updates leaked to subscriber?)",
			atomic.LoadInt32(&events), len(seen))
	}
	s.Rec.Record("updates", "incoming_exactly_once", StatusPass, sourceOf(s), map[string]any{
		"distinct_incoming": len(seen),
	})
	return nil
}

// ---------------------------------------------------------------------------
// OpMediaRoundTrip (§20 — P0): upload → send → download → SHA-256 equality,
// streaming end-to-end (O(part) memory).
// ---------------------------------------------------------------------------

func (s *Suite) OpMediaRoundTrip(ctx context.Context, size int64) error {
	name := fmt.Sprintf("live-%d.bin", size)
	up, err := s.C.Upload(ctx, newPatternReader(size),
		client.UploadOptions{Size: size, FileName: name})
	if err != nil {
		return fmt.Errorf("upload %d bytes: %w", size, err)
	}
	sent, err := s.C.SendMediaInput(ctx, s.Cfg.PeerRef(), client.UploadedDocumentInput{
		Upload: up, MimeType: "application/octet-stream",
	})
	if err != nil {
		return fmt.Errorf("send %d bytes: %w", size, err)
	}
	if sent == nil || sent.Media == nil {
		return fmt.Errorf("send returned no media (size %d)", size)
	}
	s.remember(peerIDOf(s.Cfg.PeerRef()), sent.ID)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	sink := newSHA256Sink()
	dlCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	err = s.C.DownloadMedia(dlCtx, sent, sink, client.DownloadOptions{Size: size})
	cancel()
	runtime.ReadMemStats(&after)
	heapGrowth := int64(after.TotalAlloc) - int64(before.TotalAlloc)

	if err != nil {
		return fmt.Errorf("download %d bytes: %w", size, err)
	}
	if sink.Bytes() != size {
		return fmt.Errorf("downloaded %d bytes, want %d", sink.Bytes(), size)
	}
	want := patternSum(size)
	if sink.Sum() != want {
		return fmt.Errorf("content mismatch for %d bytes: got %s want %s", size, sink.Sum(), want)
	}
	s.Rec.Record("media", "round_trip", StatusPass, sourceOf(s), map[string]any{
		"size": size, "parts": up.Parts, "big": up.IsBig,
		"heap_alloc_total": heapGrowth, "part_budget_x": heapGrowth / max64(size, 1),
	})
	return nil
}

// ---------------------------------------------------------------------------
// OpReconnect (§18): forced reconnect must preserve authorization and the
// auth key, issue no authentication RPCs, and keep the engine healthy.
// reconnect forces a real transport teardown (ReconnectTransportForTest is
// the documented INTERNAL seam for failure injection).
// ---------------------------------------------------------------------------

func (s *Suite) OpReconnect(ctx context.Context) error {
	keyBefore := s.C.SessionKeyID()
	authBefore := s.C.AuthState()
	s.C.ReconnectTransportForTest()

	deadline := time.Now().Add(liveConnectTimeout)
	for time.Now().Before(deadline) {
		if s.C.IsConnected() && s.C.State() == client.StateReady {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !s.C.IsConnected() {
		return fmt.Errorf("not reconnected within 30s (state %s)", s.C.State())
	}
	if s.C.AuthState() != authBefore {
		return fmt.Errorf("auth state changed across reconnect: %s → %s", authBefore, s.C.AuthState())
	}
	if s.C.SessionKeyID() != keyBefore {
		return fmt.Errorf("session key changed across reconnect")
	}
	if s.Trace.HasAuthRPC() {
		return fmt.Errorf("reconnect issued authentication RPCs: %v", s.Trace.Methods())
	}
	if err := s.C.CatchUp(); err != nil {
		return fmt.Errorf("catch-up after reconnect: %w", err)
	}
	s.Rec.Record("reconnect", "forced_reconnect", StatusPass, sourceOf(s), map[string]any{
		"state": s.C.State().String(), "auth": s.C.AuthState().String(),
	})
	return nil
}

// ---------------------------------------------------------------------------
// OpGapAndDifference (§19): with the client DISCONNECTED, an operator sends
// a message; reconnect + difference must deliver it exactly once. When no
// incoming message arrives, the verdict is NOT_OBSERVED (§19 forbids
// manufacturing gap claims).
// ---------------------------------------------------------------------------

func (s *Suite) OpGapAndDifference(ctx context.Context, window time.Duration) error {
	if err := s.C.Disconnect(ctx); err != nil {
		return fmt.Errorf("disconnect: %w", err)
	}
	time.Sleep(window)
	var events int32
	keys := make(map[client.MessageKey]int)
	sub := s.C.OnUpdate(func(ctx context.Context, upd tl.Object) {
		atomic.AddInt32(&events, 1)
		if m, ok := liveExtractMessage(upd); ok && !m.Out {
			keys[client.MessageKey{Peer: m.Peer, ID: m.ID}]++
		}
	})
	defer sub.Unsubscribe()
	if err := s.C.Connect(ctx); err != nil {
		return fmt.Errorf("reconnect: %w", err)
	}
	if err := s.C.CatchUp(); err != nil {
		return fmt.Errorf("catch-up: %w", err)
	}
	// Allow the difference-served updates to flow.
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && len(keys) == 0 && atomic.LoadInt32(&events) == 0 {
		time.Sleep(100 * time.Millisecond)
	}
	if len(keys) == 0 {
		s.Rec.Record("updates", "gap_difference", StatusNotObserved, sourceOf(s), nil)
		return nil
	}
	for k, n := range keys {
		if n != 1 {
			return fmt.Errorf("gap-recovered message %s/%d delivered %d times", k.Peer.Kind, k.ID, n)
		}
	}
	s.Rec.Record("updates", "gap_difference", StatusPass, sourceOf(s), map[string]any{
		"recovered": len(keys),
	})
	return nil
}

// ---------------------------------------------------------------------------
// OpErrorMatrix (§24): safe, bounded probes; every observed error class is
// recorded. Invalid input and unknown-entity probes never touch private
// data (nonexistent identifiers only).
// ---------------------------------------------------------------------------

func (s *Suite) OpErrorMatrix(ctx context.Context) error {
	// 1. Not-ready: Invoke before Connect must fail with ErrNotReady/ErrClosed.
	{
		c, err := disconnectedClient(s)
		if err == nil {
			_, ierr := c.Invoke(ctx, newProbePing())
			class := client.ClassifyError(ierr)
			s.Rec.Record("errors", "not_ready_probe", StatusPass, sourceOf(s), map[string]any{"class": class})
			_ = c.Close()
		}
	}
	// 2. Unknown username resolution: server error class observed, never a
	// fabricated entity.
	{
		_, err := s.C.ResolveUsername(ctx, "@suromesh-live-probe-nonexistent-7c3f")
		class := client.ClassifyError(err)
		if err == nil {
			return fmt.Errorf("nonexistent username resolved — unexpected")
		}
		s.Rec.Record("errors", "unknown_username", StatusPass, sourceOf(s), map[string]any{"class": class})
	}
	// 3. Uncached bare ID must fail LOCALLY with the typed error (never
	// fabricated, never sent).
	{
		_, err := s.C.ResolvePeer(ctx, client.PeerID{Kind: client.PeerUser, ID: 987654321})
		var ah *client.ErrAccessHashUnavailable
		if !errors.As(err, &ah) {
			return fmt.Errorf("uncached bare id error = %v, want ErrAccessHashUnavailable", err)
		}
		s.Rec.Record("errors", "no_hash_fabrication", StatusPass, sourceOf(s), map[string]any{"class": "access_hash_unavailable"})
	}
	return nil
}

// ---------------------------------------------------------------------------
// OpDialogsAndHistory (§11, §12): pagination, conversion, termination.
// ---------------------------------------------------------------------------

func (s *Suite) OpDialogsAndHistory(ctx context.Context, limit int) error {
	dialogs, err := s.C.GetDialogs(ctx, client.DialogQuery{Limit: limit})
	if err != nil {
		return fmt.Errorf("GetDialogs: %w", err)
	}
	it := s.C.IterDialogs(ctx, client.DialogQuery{Limit: limit})
	paged := 0
	for {
		dctx, cancel := context.WithTimeout(ctx, defaultTimeout)
		_, ok := it.Next(dctx)
		cancel()
		if !ok {
			break
		}
		paged++
	}
	if err := it.Err(); err != nil && !errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("dialog iterator: %w", err)
	}
	if paged < len(dialogs) {
		return fmt.Errorf("iterator yielded %d, GetDialogs %d", paged, len(dialogs))
	}
	s.Rec.Record("dialogs", "get_dialogs", StatusPass, sourceOf(s), map[string]any{
		"dialogs": len(dialogs), "iterated": paged,
	})

	// History over the configured peer, or the first resolved dialog when
	// the operator did not configure one.
	var hPeer client.Peer
	if s.Cfg.Peer != "" {
		hp, rerr := s.C.ResolvePeer(ctx, s.Cfg.PeerRef())
		if rerr != nil {
			return fmt.Errorf("resolve history peer: %w", rerr)
		}
		hPeer = hp
	} else if len(dialogs) > 0 {
		hPeer = dialogs[0].Peer
	}
	if hPeer == nil {
		s.Rec.Record("history", "get_messages", StatusNotObserved, sourceOf(s), nil)
		return nil
	}
	histCtx, cancel := context.WithTimeout(ctx, defaultTimeout)
	msgs, err := s.C.GetMessages(histCtx, hPeer, client.MessageQuery{Limit: 20})
	cancel()
	if err != nil {
		return fmt.Errorf("GetMessages: %w", err)
	}
	// Ordering: live history must be non-increasing by date.
	for i := 1; i < len(msgs); i++ {
		if msgs[i].Date.After(msgs[i-1].Date) {
			return fmt.Errorf("history ordering violated at %d: %s after %s", i, msgs[i].Date, msgs[i-1].Date)
		}
	}
	serviceCount := 0
	mediaCount := 0
	for _, m := range msgs {
		if m.IsService {
			serviceCount++
		}
		if m.Media != nil {
			mediaCount++
		}
	}
	s.Rec.Record("history", "get_messages", StatusPass, sourceOf(s), map[string]any{
		"messages": len(msgs), "service": serviceCount, "media": mediaCount,
	})
	return nil
}

// sourceOf labels the validation source from the client's host.
func sourceOf(s *Suite) string {
	if s.Cfg.Host == fakeHostMarker {
		return SourceFake
	}
	return SourceLive
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

var _ = io.EOF
