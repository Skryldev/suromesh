package handlers

// User/Dialog/Connection handlers: thin tenant-scoped delegations to the
// account runtime (the Manager resolves the runner; handlers stay thin).

import (
	"context"

	"google.golang.org/grpc/codes"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// RuntimeHandlers serves UserService, DialogService, ConnectionService —
// all backed by the account runner.
type RuntimeHandlers struct {
	v1.UnimplementedUserServiceServer
	v1.UnimplementedDialogServiceServer
	v1.UnimplementedConnectionServiceServer

	manager  *application.Manager
	accounts AccountReader
}

// AccountReader is the minimal account-lookup seam for handlers.
type AccountReader interface {
	Get(ctx context.Context, tenantID, accountID string) (rec accountRecord, err error)
}

// accountRecord aliases the ports record shape for this file.
type accountRecord = accountRecordT

func NewRuntimeHandlers(manager *application.Manager, accounts AccountReader) *RuntimeHandlers {
	return &RuntimeHandlers{manager: manager, accounts: accounts}
}

// runner resolves the tenant-scoped runner for an account id.
func (h *RuntimeHandlers) runner(ctx context.Context, tenantID, accountID string) (*application.Runner, error) {
	if err := requireAccountID(accountID); err != nil {
		return nil, mapError(err)
	}
	return h.manager.Get(tenantID, domain.AccountID(accountID))
}

func (h *RuntimeHandlers) GetMe(ctx context.Context, req *v1.GetMeRequest) (*v1.GetMeResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var peer domain.Peer
	err = r.Read(ctx, func(s runtimeSession) (readErr error) {
		peer, readErr = s.GetMe(ctx)
		return readErr
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetMeResponse{Peer: convertDomainPeer(peer)}, nil
}

func (h *RuntimeHandlers) ResolvePeer(ctx context.Context, req *v1.ResolvePeerRequest) (*v1.ResolvePeerResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var peer domain.Peer
	err = r.Read(ctx, func(s runtimeSession) error {
		peer, err = s.ResolvePeer(ctx, ref)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.ResolvePeerResponse{Peer: convertDomainPeer(peer)}, nil
}

func (h *RuntimeHandlers) GetUser(ctx context.Context, req *v1.GetUserRequest) (*v1.GetUserResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	view := domain.PeerViewBasic
	if req.GetView() == v1.PeerView_PEER_VIEW_FULL {
		view = domain.PeerViewFull
	}
	var details ports.PeerDetailsResult
	err = r.Read(ctx, func(s runtimeSession) error {
		details, err = s.PeerDetails(ctx, ref, view)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	if details.User == nil {
		return nil, fail(codes.NotFound, "not a user peer")
	}
	u := &v1.User{PeerId: details.User.Peer.ID.Canonical(), DisplayName: details.User.Peer.DisplayName, IsBot: details.User.IsBot}
	if details.User.Peer.Username != "" {
		un := details.User.Peer.Username
		u.Username = &un
	}
	return &v1.GetUserResponse{User: u}, nil
}

func (h *RuntimeHandlers) GetChannel(ctx context.Context, req *v1.GetChannelRequest) (*v1.GetChannelResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	view := domain.PeerViewBasic
	if req.GetView() == v1.PeerView_PEER_VIEW_FULL {
		view = domain.PeerViewFull
	}
	var details ports.PeerDetailsResult
	err = r.Read(ctx, func(s runtimeSession) error {
		details, err = s.PeerDetails(ctx, ref, view)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	if details.Channel == nil {
		return nil, fail(codes.NotFound, "not a channel peer")
	}
	c := &v1.Channel{PeerId: details.Channel.Peer.ID.Canonical(), Title: details.Channel.Peer.DisplayName, ParticipantsCount: details.Channel.ParticipantsCount}
	if details.Channel.Peer.Username != "" {
		un := details.Channel.Peer.Username
		c.Username = &un
	}
	return &v1.GetChannelResponse{Channel: c}, nil
}

func (h *RuntimeHandlers) GetChat(ctx context.Context, req *v1.GetChatRequest) (*v1.GetChatResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return nil, mapError(err)
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	view := domain.PeerViewBasic
	if req.GetView() == v1.PeerView_PEER_VIEW_FULL {
		view = domain.PeerViewFull
	}
	var details ports.PeerDetailsResult
	err = r.Read(ctx, func(s runtimeSession) error {
		details, err = s.PeerDetails(ctx, ref, view)
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	if details.Chat == nil {
		return nil, fail(codes.NotFound, "not a chat peer")
	}
	return &v1.GetChatResponse{Chat: &v1.Chat{
		PeerId: details.Chat.Peer.ID.Canonical(), Title: details.Chat.Peer.DisplayName,
		ParticipantsCount: details.Chat.ParticipantsCount,
	}}, nil
}

func (h *RuntimeHandlers) ListDialogs(ctx context.Context, req *v1.ListDialogsRequest) (*v1.ListDialogsResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	pageSize := int(req.GetPageSize())
	if pageSize <= 0 {
		pageSize = 50
	}
	if pageSize > 100 {
		pageSize = 100
	}
	r, err := h.runner(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	var page domain.DialogPage
	err = r.Read(ctx, func(s runtimeSession) error {
		page, err = s.ListDialogs(ctx, pageSize, req.GetPageToken())
		return err
	})
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.ListDialogsResponse{}
	for _, d := range page.Dialogs {
		dialog := &v1.Dialog{
			Peer:            convertDomainPeer(d.Peer),
			TopMessageId:    domain.FormatID(d.TopMessageID),
			UnreadCount:     d.UnreadCount,
			ReadInboxMaxId:  d.ReadInboxMaxID,
			ReadOutboxMaxId: d.ReadOutboxMaxID,
			Pinned:          d.Pinned,
			LastActivity:    timestampOf(d.LastActivity),
		}
		resp.Dialogs = append(resp.Dialogs, dialog)
	}
	if page.NextPageToken != "" {
		t := page.NextPageToken
		resp.NextPageToken = &t
	}
	if page.TotalEstimate > 0 {
		te := page.TotalEstimate
		resp.TotalEstimate = &te
	}
	return resp, nil
}

func (h *RuntimeHandlers) Connect(ctx context.Context, req *v1.ConnectRequest) (*v1.ConnectResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	obs, err := h.setDesired(ctx, p.TenantID, req.GetAccountId(), domain.DesiredRunning)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.ConnectResponse{Observed: convertObservation(obs)}, nil
}

func (h *RuntimeHandlers) Disconnect(ctx context.Context, req *v1.DisconnectRequest) (*v1.DisconnectResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	obs, err := h.setDesired(ctx, p.TenantID, req.GetAccountId(), domain.DesiredStopped)
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.DisconnectResponse{Observed: convertObservation(obs)}, nil
}

func (h *RuntimeHandlers) GetConnectionState(ctx context.Context, req *v1.GetConnectionStateRequest) (*v1.GetConnectionStateResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	obs, err := h.observation(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetConnectionStateResponse{Observed: convertObservation(obs)}, nil
}
