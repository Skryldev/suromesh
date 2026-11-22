// Package handlers implements the gRPC service surface. Handlers are
// thin: validate → resolve tenant scope → call application use cases →
// map errors. Business logic lives in the application layer.
package handlers

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/domain"
	domerrors "github.com/Skryldev/suromesh/service/internal/grpcapi/errors"
	"github.com/Skryldev/suromesh/service/internal/grpcapi/interceptors"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// principal resolves the authenticated caller (transport-guaranteed
// non-nil for intercepted RPCs; defensive fallback returns an error).
func principal(ctx context.Context) (*interceptors.Principal, error) {
	p := interceptors.PrincipalFromContext(ctx)
	if p == nil {
		return nil, status.Error(codes.Unauthenticated, "missing principal")
	}
	return p, nil
}

// mapError is THE handler-boundary conversion (status + ErrorInfo).
func mapError(err error) error {
	if err == nil {
		return nil
	}
	return domerrors.Map(err)
}

// fail builds a mapped error directly (handlers with local failures).
func fail(code codes.Code, msg string) error {
	return status.Error(code, msg)
}

// errMappedWithText overrides the client-visible message (caller-actionable
// validation text) while keeping the mapped status + reasons.
func errMappedWithText(err error, msg string) error {
	mapped := domerrors.Map(err)
	if msg != "" {
		if st, ok := status.FromError(mapped); ok {
			return status.New(st.Code(), msg).Err()
		}
	}
	return mapped
}

func timestampOf(t time.Time) *timestamppb.Timestamp {
	if t.IsZero() {
		return nil
	}
	return timestamppb.New(t)
}

// requireAccountID validates the account id shape (UUID-ish string).
func requireAccountID(accountID string) error {
	if len(accountID) != 36 || strings.Count(accountID, "-") != 4 {
		return fmt.Errorf("%w: account_id must be a UUID", domain.ErrInvalidInput)
	}
	return nil
}

// ---- domain → contract conversions ----------------------------------------

func convertDomainPeer(p domain.Peer) *v1.Peer {
	out := &v1.Peer{
		PeerId:      p.ID.Canonical(),
		Kind:        convertPeerKind(p.ID.Kind),
		DisplayName: p.DisplayName,
	}
	if p.Username != "" {
		u := p.Username
		out.Username = &u
	}
	return out
}

func convertPeerKind(k domain.PeerKind) v1.PeerKind {
	switch k {
	case domain.PeerKindUser:
		return v1.PeerKind_PEER_KIND_USER
	case domain.PeerKindChat:
		return v1.PeerKind_PEER_KIND_CHAT
	case domain.PeerKindChannel:
		return v1.PeerKind_PEER_KIND_CHANNEL
	default:
		return v1.PeerKind_PEER_KIND_UNSPECIFIED
	}
}

func parsePeerRef(r *v1.PeerRef) (domain.PeerRef, error) {
	if r == nil {
		return domain.PeerRef{}, fmt.Errorf("%w: peer reference required", domain.ErrInvalidInput)
	}
	switch ref := r.Ref.(type) {
	case *v1.PeerRef_PeerId:
		id, err := domain.ParsePeerID(ref.PeerId)
		if err != nil {
			return domain.PeerRef{}, err
		}
		return domain.RefPeerID(id), nil
	case *v1.PeerRef_Username:
		u := strings.TrimPrefix(ref.Username, "@")
		if u == "" {
			return domain.PeerRef{}, fmt.Errorf("%w: empty username", domain.ErrInvalidInput)
		}
		return domain.RefUsername(u), nil
	case *v1.PeerRef_Self:
		if !ref.Self {
			return domain.PeerRef{}, fmt.Errorf("%w: empty peer reference", domain.ErrInvalidInput)
		}
		return domain.RefSelf(), nil
	default:
		return domain.PeerRef{}, fmt.Errorf("%w: empty peer reference", domain.ErrInvalidInput)
	}
}

func parseMessageID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("%w: message id %q must be a positive decimal", domain.ErrInvalidInput, s)
	}
	return id, nil
}

func convertDomainMessage(m domain.Message) *v1.Message {
	out := &v1.Message{
		MessageId:   domain.FormatID(m.ID),
		Peer:        &v1.Peer{PeerId: m.PeerID.Canonical(), Kind: convertPeerKind(m.PeerID.Kind)},
		Direction:   convertDirection(m.Direction),
		Text:        m.Text,
		Date:        timestampOf(m.Date),
		IsService:   m.IsService,
		ServiceKind: m.ServiceKind,
	}
	if m.ReplyTo != nil {
		r := domain.FormatID(*m.ReplyTo)
		out.ReplyToMessageId = &r
	}
	for _, e := range m.Entities {
		out.Entities = append(out.Entities, &v1.MessageEntity{
			Kind: convertEntityKind(e.Kind), Offset: e.Offset, Length: e.Length, Url: e.URL,
		})
	}
	if m.Media != nil {
		out.Media = convertMediaSummary(m.Media)
	}
	return out
}

func convertDirection(d domain.Direction) v1.Direction {
	if d == domain.DirectionOutgoing {
		return v1.Direction_DIRECTION_OUTGOING
	}
	return v1.Direction_DIRECTION_INCOMING
}

func convertEntityKind(k domain.EntityKind) v1.MessageEntityKind {
	switch k {
	case domain.EntityMention:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_MENTION
	case domain.EntityHashtag:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_HASHTAG
	case domain.EntityURL:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_URL
	case domain.EntityBold:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_BOLD
	case domain.EntityItalic:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_ITALIC
	case domain.EntityCode:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_CODE
	case domain.EntityPre:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_PRE
	default:
		return v1.MessageEntityKind_MESSAGE_ENTITY_KIND_OTHER
	}
}

func convertMediaSummary(m *domain.MediaSummary) *v1.MediaSummary {
	out := &v1.MediaSummary{
		Kind:         convertMediaKind(m.Kind),
		SizeBytes:    m.SizeBytes,
		MimeType:     m.MimeType,
		Downloadable: m.Downloadable,
	}
	if m.FileName != "" {
		f := m.FileName
		out.FileName = &f
	}
	return out
}

func convertMediaKind(k domain.MediaKind) v1.MediaKind {
	switch k {
	case domain.MediaKindPhoto:
		return v1.MediaKind_MEDIA_KIND_PHOTO
	case domain.MediaKindDocument:
		return v1.MediaKind_MEDIA_KIND_DOCUMENT
	default:
		return v1.MediaKind_MEDIA_KIND_UNSUPPORTED
	}
}

func convertAccount(rec ports.AccountRecord, obs domain.ConnectionObservation) *v1.Account {
	return &v1.Account{
		AccountId:    rec.AccountID,
		DisplayAlias: rec.DisplayAlias,
		MaskedPhone:  rec.PhoneMask,
		Status:       convertAccountStatus(rec.Status),
		CreatedAt:    timestampOf(rec.CreatedAt),
		Connection:   convertObservation(obs),
	}
}

func convertAccountStatus(s domain.AccountStatus) v1.AccountStatus {
	switch s {
	case domain.AccountStatusPendingAuth:
		return v1.AccountStatus_ACCOUNT_STATUS_PENDING_AUTH
	case domain.AccountStatusActive:
		return v1.AccountStatus_ACCOUNT_STATUS_ACTIVE
	case domain.AccountStatusSuspended:
		return v1.AccountStatus_ACCOUNT_STATUS_SUSPENDED
	case domain.AccountStatusRemoved:
		return v1.AccountStatus_ACCOUNT_STATUS_REMOVED
	default:
		return v1.AccountStatus_ACCOUNT_STATUS_UNSPECIFIED
	}
}

func convertObservation(o domain.ConnectionObservation) *v1.ConnectionObservation {
	var desired v1.ConnectionDesired
	if o.Desired == domain.DesiredRunning {
		desired = v1.ConnectionDesired_CONNECTION_DESIRED_RUNNING
	} else {
		desired = v1.ConnectionDesired_CONNECTION_DESIRED_STOPPED
	}
	var observed v1.ConnectionObserved
	switch o.Observed {
	case domain.ObservedConnecting:
		observed = v1.ConnectionObserved_CONNECTION_OBSERVED_CONNECTING
	case domain.ObservedConnected:
		observed = v1.ConnectionObserved_CONNECTION_OBSERVED_CONNECTED
	case domain.ObservedReady:
		observed = v1.ConnectionObserved_CONNECTION_OBSERVED_READY
	default:
		observed = v1.ConnectionObserved_CONNECTION_OBSERVED_DISCONNECTED
	}
	return &v1.ConnectionObservation{
		Desired:        desired,
		Observed:       observed,
		LastErrorClass: o.LastErrorClass,
		ChangedAt:      timestampOf(o.ChangedAt),
	}
}
