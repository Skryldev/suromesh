package upstream

// clientSession: the ports.ClientSession implementation over one SDK
// client. Every method maps a service command to SDK calls, converts
// results into domain values, and translates SDK errors into the domain
// error taxonomy. Error translation lives in translate.go.

import (
	"context"
	"errors"
	"fmt"
	"io"

	sdkclient "github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

var _ ports.ClientSession = (*clientSession)(nil)

// Connect blocks until the upstream session is connected (or failed).
// The SDK's own retry/bounded-reconnect policy applies.
func (s *clientSession) Connect(ctx context.Context) error {
	if err := s.c.Connect(ctx); err != nil {
		return translate(err)
	}
	return s.persist() // key may have been exchanged; persist best-effort
}

func (s *clientSession) Disconnect(ctx context.Context) error {
	return translate(s.c.Disconnect(ctx))
}

func (s *clientSession) Close(ctx context.Context) error {
	return translate(s.c.Close())
}

func (s *clientSession) Connected() bool {
	return s.c.IsConnected()
}

func (s *clientSession) AuthState() domain.AuthState {
	switch s.c.AuthState() {
	case sdkclient.AuthAuthorized:
		return domain.AuthAuthorized
	case sdkclient.AuthCodePending:
		return domain.AuthCodePending
	case sdkclient.AuthPasswordRequired:
		return domain.AuthPasswordRequired
	default:
		// Note: upstream revocation clears the SDK's durable authorized
		// flag (AuthState → unauthorized); revocation events surface
		// through 401-class errors, not a distinct state constant.
		return domain.AuthUnauthorized
	}
}

func (s *clientSession) SendCode(ctx context.Context, phone string) error {
	if _, err := s.c.SendCode(ctx, phone); err != nil {
		return translate(err)
	}
	return nil
}

func (s *clientSession) SubmitCode(ctx context.Context, phone, code string) error {
	if _, err := s.c.SignIn(ctx, phone, code); err != nil {
		return translate(err)
	}
	return s.persist()
}

func (s *clientSession) SubmitPassword(ctx context.Context, password string) error {
	if _, err := s.c.CheckPassword(ctx, password); err != nil {
		return translate(err)
	}
	return s.persist()
}

func (s *clientSession) LogOut(ctx context.Context) error {
	if err := s.c.LogOut(ctx); err != nil {
		return translate(err)
	}
	return s.persist()
}

func (s *clientSession) GetMe(ctx context.Context) (domain.Peer, error) {
	me, err := s.c.GetMe(ctx)
	if err != nil {
		return domain.Peer{}, translate(err)
	}
	return convertUser(me), nil
}

func (s *clientSession) ResolvePeer(ctx context.Context, ref domain.PeerRef) (domain.Peer, error) {
	p, err := s.c.ResolvePeer(ctx, convertPeerRef(ref))
	if err != nil {
		return domain.Peer{}, translate(err)
	}
	return convertResolvedPeer(p), nil
}

func (s *clientSession) PeerDetails(ctx context.Context, ref domain.PeerRef, view domain.PeerView) (ports.PeerDetailsResult, error) {
	resolved, err := s.ResolvePeer(ctx, ref)
	if err != nil {
		return ports.PeerDetailsResult{}, err
	}
	switch resolved.ID.Kind {
	case domain.PeerKindUser:
		u, err := s.c.GetUser(ctx, convertPeerRef(ref))
		if err != nil {
			return ports.PeerDetailsResult{}, translate(err)
		}
		return ports.PeerDetailsResult{User: &domain.UserDetails{
			Peer: convertUser(u), IsBot: u.IsBot,
		}}, nil
	case domain.PeerKindChannel:
		if view == domain.PeerViewFull {
			ch, err := s.c.GetChannelFull(ctx, convertPeerRef(ref))
			if err != nil {
				return ports.PeerDetailsResult{}, translate(err)
			}
			return ports.PeerDetailsResult{Channel: &domain.ChannelDetails{
				Peer: resolved, ParticipantsCount: int32(ch.ParticipantsCount),
			}}, nil
		}
		ch, err := s.c.GetChannel(ctx, convertPeerRef(ref))
		if err != nil {
			return ports.PeerDetailsResult{}, translate(err)
		}
		return ports.PeerDetailsResult{Channel: &domain.ChannelDetails{Peer: convertResolvedPeer(ch)}}, nil
	default:
		if view == domain.PeerViewFull {
			ch, err := s.c.GetChatFull(ctx, convertPeerRef(ref))
			if err != nil {
				return ports.PeerDetailsResult{}, translate(err)
			}
			return ports.PeerDetailsResult{Chat: &domain.ChatDetails{
				Peer: resolved, ParticipantsCount: int32(ch.ParticipantsCount),
			}}, nil
		}
		ch, err := s.c.GetChat(ctx, convertPeerRef(ref))
		if err != nil {
			return ports.PeerDetailsResult{}, translate(err)
		}
		return ports.PeerDetailsResult{Chat: &domain.ChatDetails{Peer: convertResolvedPeer(ch)}}, nil
	}
}

func (s *clientSession) ListDialogs(ctx context.Context, pageSize int, token string) (domain.DialogPage, error) {
	query := sdkclient.DialogQuery{Limit: pageSize}
	// Opaque continuation token: base64 JSON {"o": <top_message_id>}.
	if token != "" {
		offset, err := decodeDialogToken(token)
		if err != nil {
			return domain.DialogPage{}, fmt.Errorf("%w: dialog page token: %v",
				domain.ErrInvalidInput, err)
		}
		query.OffsetID = offset
	}
	dialogs, err := s.c.GetDialogs(ctx, query)
	if err != nil {
		return domain.DialogPage{}, translate(err)
	}
	page := domain.DialogPage{}
	for _, d := range dialogs {
		page.Dialogs = append(page.Dialogs, convertDialog(d))
	}
	if pageSize > 0 && len(page.Dialogs) == pageSize {
		// Possible continuation: encode the last dialog's top message as
		// the offset for the next page.
		last := page.Dialogs[len(page.Dialogs)-1]
		page.NextPageToken = encodeDialogToken(last.TopMessageID)
	}
	return page, nil
}

func (s *clientSession) SendMessage(ctx context.Context, ref domain.PeerRef, text string, replyTo *int64) (domain.Message, error) {
	opts := sdkclient.SendMessageOptions{}
	if replyTo != nil {
		opts.ReplyTo = *replyTo
	}
	m, err := s.c.SendMessageOpts(ctx, convertPeerRef(ref), text, opts)
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

func (s *clientSession) GetMessages(ctx context.Context, ref domain.PeerRef, ids []int64) ([]domain.Message, []int64, error) {
	peer, err := s.c.ResolvePeer(ctx, convertPeerRef(ref))
	if err != nil {
		return nil, nil, translate(err)
	}
	found, err := s.getMessagesByIDs(ctx, peer, ids)
	if err != nil {
		return nil, nil, err
	}
	foundSet := map[int64]bool{}
	var out []domain.Message
	for _, m := range found {
		if m == nil {
			continue
		}
		out = append(out, convertMessage(m))
		foundSet[m.ID] = true
	}
	var missing []int64
	for _, id := range ids {
		if !foundSet[id] {
			missing = append(missing, id)
		}
	}
	return out, missing, nil
}

func (s *clientSession) EditMessage(ctx context.Context, ref domain.PeerRef, id int64, text string) (domain.Message, error) {
	m, err := s.c.EditMessage(ctx, convertPeerRef(ref), id, text)
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

func (s *clientSession) DeleteMessages(ctx context.Context, ref domain.PeerRef, ids []int64) ([]int64, error) {
	if _, err := s.c.DeleteMessages(ctx, convertPeerRef(ref), ids, sdkclient.DeleteMessagesOptions{}); err != nil {
		return nil, translate(err)
	}
	// The upstream acknowledges the batch (affected-messages ctor); the
	// requested ids are the affected set (standard semantics).
	return ids, nil
}

func (s *clientSession) ForwardMessages(ctx context.Context, from, to domain.PeerRef, ids []int64) ([]domain.Message, error) {
	msgs, err := s.c.ForwardMessages(ctx, convertPeerRef(from), convertPeerRef(to), ids, sdkclient.ForwardOptions{})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, convertMessage(m))
	}
	return out, nil
}

func (s *clientSession) PinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error) {
	m, err := s.c.PinMessage(ctx, convertPeerRef(ref), id, sdkclient.PinOptions{})
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

func (s *clientSession) UnpinMessage(ctx context.Context, ref domain.PeerRef, id int64) (domain.Message, error) {
	m, err := s.c.UnpinMessage(ctx, convertPeerRef(ref), id)
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

func (s *clientSession) MarkAsRead(ctx context.Context, ref domain.PeerRef, maxID int64) error {
	return translate(s.c.MarkAsRead(ctx, convertPeerRef(ref), maxID))
}

func (s *clientSession) SearchMessages(ctx context.Context, ref domain.PeerRef, query string, limit int) ([]domain.Message, error) {
	msgs, err := s.c.SearchMessages(ctx, convertPeerRef(ref), sdkclient.SearchQuery{Query: query, Limit: limit})
	if err != nil {
		return nil, translate(err)
	}
	out := make([]domain.Message, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, convertMessage(m))
	}
	return out, nil
}

// historyStream adapts the SDK's MessageIterator to the HistoryStream
// port with sticky-error semantics.
type historyStream struct {
	it  *sdkclient.MessageIterator
	err error
}

func (s *clientSession) StreamHistory(ctx context.Context, ref domain.PeerRef, limit int) (ports.HistoryStream, error) {
	it, err := s.c.IterMessages(ctx, convertPeerRef(ref), sdkclient.MessageQuery{Limit: int32(limit)})
	if err != nil {
		return nil, translate(err)
	}
	return &historyStream{it: it}, nil
}

func (h *historyStream) Next(ctx context.Context) (domain.Message, bool) {
	m, ok := h.it.Next(ctx)
	if !ok {
		h.err = h.it.Err()
		return domain.Message{}, false
	}
	return convertMessage(m), true
}

func (h *historyStream) Err() error { return h.err }

func (s *clientSession) SubscribeUpdates(ctx context.Context, h ports.UpdateHandler) (func(), error) {
	sub := s.c.OnUpdate(func(ctx context.Context, upd tl.Object) {
		ev, ok := convertUpdate(upd)
		if !ok {
			return // unsupported update families are silently state-tracked
		}
		h(ctx, ev)
	})
	return sub.Unsubscribe, nil
}

func (s *clientSession) CatchUp(ctx context.Context) error {
	return translate(s.c.CatchUp())
}

func (s *clientSession) GetMediaMetadata(ctx context.Context, ref domain.PeerRef, id int64) (domain.MediaSummary, error) {
	msg, err := s.getMediaForDownload(ctx, ref, id)
	if err != nil {
		return domain.MediaSummary{}, err
	}
	if msg.Media == nil {
		return domain.MediaSummary{}, fmt.Errorf("%w: message %s has no media",
			domain.ErrInvalidInput, domain.FormatID(id))
	}
	return *convertMedia(msg.Media), nil
}

// UploadMedia streams the reader into the SDK upload path (O(part)
// memory on both sides; the reader is consumed incrementally — nothing
// buffers the whole file). as_document uses the two-call
// upload+SendMediaInput path; otherwise the SDK classifies media itself.
func (s *clientSession) UploadMedia(ctx context.Context, spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
	if spec.AsDocument {
		return s.uploadMediaDocument(ctx, spec, r)
	}
	return s.uploadMediaAny(ctx, spec, r)
}

// DownloadMedia streams the message's media into w (O(chunk) memory;
// hash-writers compose).
func (s *clientSession) DownloadMedia(ctx context.Context, ref domain.PeerRef, id int64, w io.Writer) error {
	msg, err := s.getMediaForDownload(ctx, ref, id)
	if err != nil {
		return err
	}
	if err := s.c.DownloadMedia(ctx, msg, w, sdkclient.DownloadOptions{}); err != nil {
		return translate(err)
	}
	return nil
}

// ExportState serializes the live session into the service blob format.
func (s *clientSession) ExportState() ([]byte, error) {
	return marshalSession(s.sess)
}

var ErrNoState = errors.New("upstream: session state unavailable")
