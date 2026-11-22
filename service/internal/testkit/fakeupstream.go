// Package testkit provides deterministic test doubles for the service's
// ports. The fake upstream implements ports.UpstreamFactory with
// SCRIPTED behavior — it never silently reports success: unscripted
// operations return explicit errors, and scripted failures simulate the
// real failure modes (disconnect, auth failure, session invalidation,
// timeout, cancellation, malformed response, ambiguous mutation).
//
// This is test infrastructure. It is never wired into production
// configuration (production builds use the real SDK adapter).
package testkit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// ErrUnscripted is returned when a test performs an operation the test
// did not script — a deterministic failure that surfaces missing test
// setup instead of fabricating behavior.
var ErrUnscripted = errors.New("testkit: operation not scripted")

// FakeClient is the scriptable client session.
type FakeClient struct {
	mu sync.Mutex

	OnConnect        func() error
	OnSendCode       func() error
	OnSubmitCode     func() error
	OnSubmitPassword func() error
	OnSendMessage    func(text string) (domain.Message, error)
	OnGetMe          func() (domain.Peer, error)

	authState    domain.AuthState
	connected    bool
	closed       bool
	sentUpdates  chan domain.UpdateEvent
	persistCalls int
	lastPersist  []byte
}

var _ ports.ClientSession = (*FakeClient)(nil)

func (f *FakeClient) AuthState() domain.AuthState {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authState
}

func (f *FakeClient) Connected() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.connected
}

func (f *FakeClient) Connect(ctx context.Context) error {
	f.mu.Lock()
	fn, connected := f.OnConnect, f.connected
	f.mu.Unlock()
	if connected {
		return nil
	}
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	f.mu.Lock()
	f.connected = true
	f.mu.Unlock()
	return nil
}

func (f *FakeClient) Disconnect(ctx context.Context) error {
	f.mu.Lock()
	f.connected = false
	f.mu.Unlock()
	return nil
}

func (f *FakeClient) Close(ctx context.Context) error {
	f.mu.Lock()
	f.closed = true
	f.connected = false
	f.mu.Unlock()
	return nil
}

func (f *FakeClient) SendCode(ctx context.Context, phone string) error {
	f.mu.Lock()
	fn, st := f.OnSendCode, f.authState
	f.mu.Unlock()
	if st == domain.AuthUnauthorized {
		f.SetAuthState(domain.AuthCodePending)
	}
	if fn != nil {
		return fn()
	}
	return ErrUnscripted
}

func (f *FakeClient) SubmitCode(ctx context.Context, phone, code string) error {
	f.mu.Lock()
	fn := f.OnSubmitCode
	f.mu.Unlock()
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	f.SetAuthState(domain.AuthAuthorized)
	return nil
}

func (f *FakeClient) SubmitPassword(ctx context.Context, password string) error {
	f.mu.Lock()
	fn := f.OnSubmitPassword
	f.mu.Unlock()
	if fn != nil {
		if err := fn(); err != nil {
			return err
		}
	}
	f.SetAuthState(domain.AuthAuthorized)
	return nil
}

func (f *FakeClient) LogOut(ctx context.Context) error {
	f.SetAuthState(domain.AuthUnauthorized)
	return nil
}

func (f *FakeClient) GetMe(ctx context.Context) (domain.Peer, error) {
	f.mu.Lock()
	fn := f.OnGetMe
	f.mu.Unlock()
	if fn != nil {
		return fn()
	}
	return domain.Peer{}, ErrUnscripted
}

func (f *FakeClient) SendMessage(ctx context.Context, ref domain.PeerRef, text string, replyTo *int64) (domain.Message, error) {
	f.mu.Lock()
	fn := f.OnSendMessage
	f.mu.Unlock()
	if fn != nil {
		return fn(text)
	}
	return domain.Message{}, ErrUnscripted
}

func (f *FakeClient) SetAuthState(s domain.AuthState) {
	f.mu.Lock()
	f.authState = s
	f.mu.Unlock()
}

// Publish delivers one update to the registered handler (test-driven
// upstream event injection).
func (f *FakeClient) Publish(ev domain.UpdateEvent) {
	f.mu.Lock()
	ch := f.sentUpdates
	f.mu.Unlock()
	if ch != nil {
		ch <- ev
	}
}

// PersistCalls reports how many times session persistence fired.
func (f *FakeClient) PersistCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.persistCalls
}

// ---- required-but-unscripted port methods (explicit failures) ----

func (f *FakeClient) Prepare(ctx context.Context, account domain.AccountID) error { return nil }
func (f *FakeClient) ResolvePeer(ctx context.Context, ref domain.PeerRef) (domain.Peer, error) {
	return domain.Peer{}, ErrUnscripted
}
func (f *FakeClient) PeerDetails(ctx context.Context, ref domain.PeerRef, view domain.PeerView) (ports.PeerDetailsResult, error) {
	return ports.PeerDetailsResult{}, ErrUnscripted
}
func (f *FakeClient) ListDialogs(ctx context.Context, pageSize int, token string) (domain.DialogPage, error) {
	return domain.DialogPage{}, ErrUnscripted
}
func (f *FakeClient) GetMessages(ctx context.Context, ref domain.PeerRef, ids []int64) ([]domain.Message, []int64, error) {
	return nil, nil, ErrUnscripted
}
func (f *FakeClient) EditMessage(ctx context.Context, ref domain.PeerRef, id int64, text string) (domain.Message, error) {
	return domain.Message{}, ErrUnscripted
}
func (f *FakeClient) DeleteMessages(ctx context.Context, ref domain.PeerRef, ids []int64) ([]int64, error) {
	return nil, ErrUnscripted
}
func (f *FakeClient) ForwardMessages(ctx context.Context, from, to domain.PeerRef, ids []int64) ([]domain.Message, error) {
	return nil, ErrUnscripted
}
func (f *FakeClient) PinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error) {
	return domain.Message{}, ErrUnscripted
}
func (f *FakeClient) UnpinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error) {
	return domain.Message{}, ErrUnscripted
}
func (f *FakeClient) MarkAsRead(ctx context.Context, ref domain.PeerRef, maxID int64) error {
	return ErrUnscripted
}
func (f *FakeClient) SearchMessages(ctx context.Context, ref domain.PeerRef, query string, limit int) ([]domain.Message, error) {
	return nil, ErrUnscripted
}
func (f *FakeClient) StreamHistory(ctx context.Context, ref domain.PeerRef, limit int) (ports.HistoryStream, error) {
	return nil, ErrUnscripted
}
func (f *FakeClient) CatchUp(ctx context.Context) error { return ErrUnscripted }
func (f *FakeClient) GetMediaMetadata(ctx context.Context, ref domain.PeerRef, id int64) (domain.MediaSummary, error) {
	return domain.MediaSummary{}, ErrUnscripted
}
func (f *FakeClient) UploadMedia(ctx context.Context, spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
	return domain.Message{}, ErrUnscripted
}
func (f *FakeClient) DownloadMedia(ctx context.Context, ref domain.PeerRef, id int64, w io.Writer) error {
	return ErrUnscripted
}

// SubscribeUpdates registers the update channel used by Publish.
func (f *FakeClient) SubscribeUpdates(ctx context.Context, h ports.UpdateHandler) (func(), error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sentUpdates != nil {
		close(f.sentUpdates)
	}
	f.sentUpdates = make(chan domain.UpdateEvent, 256)
	go func() {
		for ev := range f.sentUpdates {
			h(ctx, ev)
		}
	}()
	return func() {}, nil
}

// ExportState records persistence.
func (f *FakeClient) ExportState() ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.persistCalls++
	f.lastPersist = []byte(fmt.Sprintf("state-%d", f.persistCalls))
	return f.lastPersist, nil
}

// LastPersist returns the last persisted blob (test assertion aid).
func (f *FakeClient) LastPersist() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastPersist
}
