package handlers

// AccountService + SessionService + AuthenticationService handlers.
// Every handler: principal → tenant-scoped use case → contract mapping.

import (
	"context"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/protobuf/types/known/emptypb"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/application"
	"github.com/Skryldev/suromesh/service/internal/domain"
)

// AccountHandlers serves AccountService, SessionService and
// AuthenticationService.
type AccountHandlers struct {
	v1.UnimplementedAccountServiceServer
	v1.UnimplementedSessionServiceServer
	v1.UnimplementedAuthenticationServiceServer

	accounts *application.AccountService
	auth     *application.AuthService
	sessions SessionOps
}

// SessionOps is the session-management seam (persistence-backed; the
// handler never touches session material).
type SessionOps interface {
	Status(ctx context.Context, tenantID, accountID string) (present bool, version string, lastPersisted time.Time, err error)
	Revoke(ctx context.Context, tenantID, actor, accountID string) error
	Import(ctx context.Context, tenantID, actor, phone, alias, stringSession string) (BoundResult, error)
}

// BoundResult is the import result (existing binding + account id).
type BoundResult struct {
	AccountID string
	Existing  bool
}

// NewAccountHandlers wires the handlers.
func NewAccountHandlers(accounts *application.AccountService,
	auth *application.AuthService, sessions SessionOps) *AccountHandlers {
	return &AccountHandlers{accounts: accounts, auth: auth, sessions: sessions}
}

// ---- AccountService ----

func (h *AccountHandlers) BindAccount(ctx context.Context, req *v1.BindAccountRequest) (*v1.BindAccountResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetDisplayAlias() == "" {
		return nil, errMappedWithText(domain.ErrInvalidInput, "display_alias is required")
	}
	bound, err := h.accounts.Bind(ctx, p.TenantID, p.CredentialID, req.GetPhone(), req.GetDisplayAlias())
	if err != nil {
		return nil, mapError(err)
	}
	acc := convertAccount(bound.Record, domain.ConnectionObservation{})
	return &v1.BindAccountResponse{Account: acc, Existing: bound.Existing}, nil
}

func (h *AccountHandlers) GetAccount(ctx context.Context, req *v1.GetAccountRequest) (*v1.GetAccountResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := requireAccountID(req.GetAccountId()); err != nil {
		return nil, mapError(err)
	}
	rec, err := h.accounts.Get(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetAccountResponse{Account: convertAccount(rec, domain.ConnectionObservation{})}, nil
}

func (h *AccountHandlers) ListAccounts(ctx context.Context, req *v1.ListAccountsRequest) (*v1.ListAccountsResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	pageSize := int(req.GetPageSize())
	recs, err := h.accounts.List(ctx, p.TenantID, pageSize, 0)
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.ListAccountsResponse{}
	for _, rec := range recs {
		resp.Accounts = append(resp.Accounts, convertAccount(rec, domain.ConnectionObservation{}))
	}
	return resp, nil
}

func (h *AccountHandlers) SuspendAccount(ctx context.Context, req *v1.SuspendAccountRequest) (*v1.SuspendAccountResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := h.accounts.Suspend(ctx, p.TenantID, p.CredentialID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.SuspendAccountResponse{Account: convertAccount(rec, domain.ConnectionObservation{})}, nil
}

func (h *AccountHandlers) ResumeAccount(ctx context.Context, req *v1.ResumeAccountRequest) (*v1.ResumeAccountResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	rec, err := h.accounts.Resume(ctx, p.TenantID, p.CredentialID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.ResumeAccountResponse{Account: convertAccount(rec, domain.ConnectionObservation{})}, nil
}

func (h *AccountHandlers) RemoveAccount(ctx context.Context, req *v1.RemoveAccountRequest) (*v1.RemoveAccountResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.accounts.Remove(ctx, p.TenantID, p.CredentialID, req.GetAccountId()); err != nil {
		return nil, mapError(err)
	}
	return &v1.RemoveAccountResponse{}, nil
}

// ---- SessionService ----

func (h *AccountHandlers) ImportSession(ctx context.Context, req *v1.ImportSessionRequest) (*v1.ImportSessionResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if h.sessions == nil {
		return nil, fail(codes.Unimplemented, "session import not configured")
	}
	res, err := h.sessions.Import(ctx, p.TenantID, p.CredentialID,
		req.GetPhone(), req.GetDisplayAlias(), req.GetStringSession())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.ImportSessionResponse{
		Account: &v1.Account{
			AccountId: res.AccountID,
			Status:    v1.AccountStatus_ACCOUNT_STATUS_PENDING_AUTH,
			AuthState: v1.AuthorizationState_AUTHORIZATION_STATE_AUTHORIZED,
		},
		Existing: res.Existing,
	}, nil
}

func (h *AccountHandlers) GetSessionStatus(ctx context.Context, req *v1.GetSessionStatusRequest) (*v1.GetSessionStatusResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if h.sessions == nil {
		return nil, fail(codes.Unimplemented, "session status not configured")
	}
	present, version, last, err := h.sessions.Status(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	resp := &v1.GetSessionStatusResponse{Present: present, FormatVersion: version}
	if !last.IsZero() {
		resp.LastPersisted = timestampOf(last)
	}
	return resp, nil
}

func (h *AccountHandlers) RevokeSession(ctx context.Context, req *v1.RevokeSessionRequest) (*v1.RevokeSessionResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if h.sessions == nil {
		return nil, fail(codes.Unimplemented, "session revoke not configured")
	}
	if err := h.sessions.Revoke(ctx, p.TenantID, p.CredentialID, req.GetAccountId()); err != nil {
		return nil, mapError(err)
	}
	return &v1.RevokeSessionResponse{Shredded: true}, nil
}

// ---- AuthenticationService ----

func (h *AccountHandlers) SendCode(ctx context.Context, req *v1.SendCodeRequest) (*v1.SendCodeResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	// The phone travels with the request (the service never stores raw
	// phones); the auth use case verifies it against the binding hash.
	state, err := h.auth.SendCode(ctx, p.TenantID, p.CredentialID,
		req.GetAccountId(), req.GetPhone())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.SendCodeResponse{State: convertAuthState(state)}, nil
}

func (h *AccountHandlers) SubmitCode(ctx context.Context, req *v1.SubmitCodeRequest) (*v1.SubmitCodeResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetCode() == "" {
		return nil, errMappedWithText(domain.ErrInvalidInput, "code is required")
	}
	state, err := h.auth.SubmitCode(ctx, p.TenantID, p.CredentialID,
		req.GetAccountId(), req.GetPhone(), req.GetCode())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.SubmitCodeResponse{State: convertAuthState(state)}, nil
}

func (h *AccountHandlers) SubmitPassword(ctx context.Context, req *v1.SubmitPasswordRequest) (*v1.SubmitPasswordResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if req.GetPassword() == "" {
		return nil, errMappedWithText(domain.ErrInvalidInput, "password is required")
	}
	state, err := h.auth.SubmitPassword(ctx, p.TenantID, p.CredentialID, req.GetAccountId(), req.GetPassword())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.SubmitPasswordResponse{State: convertAuthState(state)}, nil
}

func (h *AccountHandlers) GetAuthorizationState(ctx context.Context, req *v1.GetAuthorizationStateRequest) (*v1.GetAuthorizationStateResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	state, err := h.auth.AuthorizationState(ctx, p.TenantID, req.GetAccountId())
	if err != nil {
		return nil, mapError(err)
	}
	return &v1.GetAuthorizationStateResponse{State: convertAuthState(state)}, nil
}

func (h *AccountHandlers) LogOut(ctx context.Context, req *v1.LogOutRequest) (*v1.LogOutResponse, error) {
	p, err := principal(ctx)
	if err != nil {
		return nil, err
	}
	if err := h.auth.LogOut(ctx, p.TenantID, p.CredentialID, req.GetAccountId()); err != nil {
		return nil, mapError(err)
	}
	return &v1.LogOutResponse{State: v1.AuthorizationState_AUTHORIZATION_STATE_UNAUTHORIZED}, nil
}

func convertAuthState(s domain.AuthState) v1.AuthorizationState {
	switch s {
	case domain.AuthCodePending:
		return v1.AuthorizationState_AUTHORIZATION_STATE_CODE_PENDING
	case domain.AuthPasswordRequired:
		return v1.AuthorizationState_AUTHORIZATION_STATE_PASSWORD_REQUIRED
	case domain.AuthAuthorized:
		return v1.AuthorizationState_AUTHORIZATION_STATE_AUTHORIZED
	case domain.AuthRevoked:
		return v1.AuthorizationState_AUTHORIZATION_STATE_REVOKED
	default:
		return v1.AuthorizationState_AUTHORIZATION_STATE_UNAUTHORIZED
	}
}

var _ = emptypb.Empty{}
