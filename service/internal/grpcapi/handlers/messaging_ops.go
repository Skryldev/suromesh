package handlers

// Messaging handlers (mutations + retrieval + search + streaming) and
// the runtime helper functions shared with messaging.go.

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// runtimeSession aliases the port (readability in handler closures).
type runtimeSession = ports.ClientSession

// accountRecordT is the ports record alias.
type accountRecordT = ports.AccountRecord

// setDesired records desired state and reconciles the runner: connect =
// ensure the runner exists + Connect; disconnect = Disconnect (session
// preserved). The observation snapshot returned reflects post-action
// state.
func (h *RuntimeHandlers) setDesired(ctx context.Context, tenantID, accountID string, desired domain.DesiredState) (domain.ConnectionObservation, error) {
	if err := requireAccountID(accountID); err != nil {
		return domain.ConnectionObservation{}, mapError(err)
	}
	rec, err := h.accounts.Get(ctx, tenantID, accountID)
	if err != nil {
		return domain.ConnectionObservation{}, mapError(err)
	}
	if rec.Status == domain.AccountStatusSuspended {
		return domain.ConnectionObservation{}, fmt.Errorf(
			"%w: account %s is suspended", domain.ErrAccountStopped, accountID)
	}
	runner, err := h.manager.Get(tenantID, domain.AccountID(rec.AccountID))
	if err != nil {
		return domain.ConnectionObservation{}, err
	}
	if err := runner.SetDesired(ctx, desired); err != nil {
		return domain.ConnectionObservation{}, mapError(err)
	}
	return runner.Observation(), nil
}

// observation returns the current desired/observed snapshot.
func (h *RuntimeHandlers) observation(ctx context.Context, tenantID, accountID string) (domain.ConnectionObservation, error) {
	if err := requireAccountID(accountID); err != nil {
		return domain.ConnectionObservation{}, mapError(err)
	}
	runner, err := h.manager.Get(tenantID, domain.AccountID(accountID))
	if err != nil {
		return domain.ConnectionObservation{}, err
	}
	return runner.Observation(), nil
}

func (h *RuntimeHandlers) SendMessage(ctx context.Context, req *v1.SendMessageRequest) (*v1.SendMessageResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetText() == "" {
		return nil, fail(codes.InvalidArgument, "text is required")
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	var replyTo *int64
	if rid := req.GetReplyToMessageId(); rid != "" {
		rt, perr := parseMessageID(rid)
		if perr != nil {
			return nil, mapError(perr)
		}
		replyTo = &rt
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msg domain.Message
	err = r.Mutate(ctx, func(s runtimeSession) error {
		msg, err = s.SendMessage(ctx, ref, req.GetText(), replyTo)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.SendMessageResponse{Message: convertDomainMessage(msg)}, nil
}

func (h *RuntimeHandlers) GetMessages(ctx context.Context, req *v1.GetMessagesRequest) (*v1.GetMessagesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	ids := make([]int64, 0, len(req.GetMessageIds()))
	for _, raw := range req.GetMessageIds() {
		id, perr := parseMessageID(raw)
		if perr != nil {
			return nil, mapError(perr)
		}
		ids = append(ids, id)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var found []domain.Message
	var missing []int64
	err = r.Read(ctx, func(s runtimeSession) error {
		found, missing, err = s.GetMessages(ctx, ref, ids)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.GetMessagesResponse{}
	for _, m := range found {
		resp.Messages = append(resp.Messages, convertDomainMessage(m))
	}
	for _, id := range missing {
		resp.MissingIds = append(resp.MissingIds, domain.FormatID(id))
	}
	return resp, nil
}

func (h *RuntimeHandlers) EditMessage(ctx context.Context, req *v1.EditMessageRequest) (*v1.EditMessageResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	id, err := parseMessageID(req.GetMessageId())
	if err != nil {
		return nil, mapError(err)
	}
	if req.GetText() == "" {
		return nil, fail(codes.InvalidArgument, "text is required")
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msg domain.Message
	err = r.Mutate(ctx, func(s runtimeSession) error {
		msg, err = s.EditMessage(ctx, ref, id, req.GetText())
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.EditMessageResponse{Message: convertDomainMessage(msg)}, nil
}

func (h *RuntimeHandlers) DeleteMessages(ctx context.Context, req *v1.DeleteMessagesRequest) (*v1.DeleteMessagesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	if len(req.GetMessageIds()) == 0 || len(req.GetMessageIds()) > 100 {
		return nil, fail(codes.InvalidArgument, "message_ids: 1..100 items")
	}
	ids := make([]int64, 0, len(req.GetMessageIds()))
	for _, raw := range req.GetMessageIds() {
		id, perr := parseMessageID(raw)
		if perr != nil {
			return nil, mapError(perr)
		}
		ids = append(ids, id)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var deleted []int64
	err = r.Mutate(ctx, func(s runtimeSession) error {
		deleted, err = s.DeleteMessages(ctx, ref, ids)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.DeleteMessagesResponse{}
	for _, id := range deleted {
		resp.DeletedIds = append(resp.DeletedIds, domain.FormatID(id))
	}
	return resp, nil
}

func (h *RuntimeHandlers) ForwardMessages(ctx context.Context, req *v1.ForwardMessagesRequest) (*v1.ForwardMessagesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	from, err := parsePeerRef(req.GetFromPeer())
	if err != nil {
		return nil, mapError(err)
	}
	to, err := parsePeerRef(req.GetToPeer())
	if err != nil {
		return nil, mapError(err)
	}
	if len(req.GetMessageIds()) == 0 || len(req.GetMessageIds()) > 100 {
		return nil, fail(codes.InvalidArgument, "message_ids: 1..100 items")
	}
	ids := make([]int64, 0, len(req.GetMessageIds()))
	for _, raw := range req.GetMessageIds() {
		id, perr := parseMessageID(raw)
		if perr != nil {
			return nil, mapError(perr)
		}
		ids = append(ids, id)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msgs []domain.Message
	err = r.Mutate(ctx, func(s runtimeSession) error {
		msgs, err = s.ForwardMessages(ctx, from, to, ids)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.ForwardMessagesResponse{}
	for _, m := range msgs {
		resp.Messages = append(resp.Messages, convertDomainMessage(m))
	}
	return resp, nil
}

func (h *RuntimeHandlers) PinMessage(ctx context.Context, req *v1.PinMessageRequest) (*v1.PinMessageResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	id, err := parseMessageID(req.GetMessageId())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msg domain.Message
	err = r.Mutate(ctx, func(s runtimeSession) error {
		msg, err = s.PinMessage(ctx, ref, id)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.PinMessageResponse{Message: convertDomainMessage(msg)}, nil
}

func (h *RuntimeHandlers) UnpinMessage(ctx context.Context, req *v1.UnpinMessageRequest) (*v1.UnpinMessageResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	id, err := parseMessageID(req.GetMessageId())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msg domain.Message
	err = r.Mutate(ctx, func(s runtimeSession) error {
		msg, err = s.UnpinMessage(ctx, ref, id)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.UnpinMessageResponse{Message: convertDomainMessage(msg)}, nil
}

func (h *RuntimeHandlers) MarkAsRead(ctx context.Context, req *v1.MarkAsReadRequest) (*v1.MarkAsReadResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	maxID, err := parseMessageID(req.GetMaxId())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	err = r.Mutate(ctx, func(s runtimeSession) error {
		return s.MarkAsRead(ctx, ref, maxID)
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.MarkAsReadResponse{Peer: convertDomainPeer(domain.Peer{ID: refToID(ref)}), MaxId: maxID}, nil
}

// refToID resolves an id-form reference to a PeerID (username refs need
// upstream resolution, so MarkAsRead requires the id form — documented in
// the API reference).
func refToID(ref domain.PeerRef) domain.PeerID {
	if ref.Kind == domain.RefKindPeerID {
		return ref.ID
	}
	return domain.PeerID{}
}

func (h *RuntimeHandlers) SearchMessages(ctx context.Context, req *v1.SearchMessagesRequest) (*v1.SearchMessagesResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	limit := int(req.GetLimit())
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var msgs []domain.Message
	err = r.Read(ctx, func(s runtimeSession) error {
		msgs, err = s.SearchMessages(ctx, ref, req.GetQuery(), limit)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.SearchMessagesResponse{}
	for _, m := range msgs {
		resp.Messages = append(resp.Messages, convertDomainMessage(m))
	}
	return resp, nil
}
