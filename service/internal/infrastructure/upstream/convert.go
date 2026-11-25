package upstream

// Conversion layer: SDK domain types → service domain types. This is the
// anti-corruption translation — no SDK type escapes the adapter.

import (
	"fmt"

	sdkclient "github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl/gen"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// convertPeerID maps an SDK peer id.
func convertPeerID(p sdkclient.PeerID) domain.PeerID {
	var k domain.PeerKind
	switch p.Kind {
	case sdkclient.PeerUser:
		k = domain.PeerKindUser
	case sdkclient.PeerChat:
		k = domain.PeerKindChat
	case sdkclient.PeerChannel:
		k = domain.PeerKindChannel
	default:
		k = domain.PeerKindUser
	}
	return domain.PeerID{Kind: k, ID: p.ID}
}

func sdkPeerID(p domain.PeerID) sdkclient.PeerID {
	var k sdkclient.PeerKind
	switch p.Kind {
	case domain.PeerKindUser:
		k = sdkclient.PeerUser
	case domain.PeerKindChat:
		k = sdkclient.PeerChat
	case domain.PeerKindChannel:
		k = sdkclient.PeerChannel
	}
	return sdkclient.PeerID{Kind: k, ID: p.ID}
}

// convertPeerRef maps a domain reference to the SDK reference.
func convertPeerRef(r domain.PeerRef) sdkclient.PeerRef {
	switch r.Kind {
	case domain.RefKindPeerID:
		return sdkclient.PeerID(sdkPeerID(r.ID))
	case domain.RefKindUsername:
		return sdkclient.UsernameRef(r.Username)
	default:
		return sdkclient.SelfRef{}
	}
}

// convertUser maps a resolved user.
// convertWirePeerDomain maps a raw wire peer to the domain peer id.
func convertWirePeerDomain(p gen.PeerClass) domain.PeerID {
	return convertPeerID(convertWirePeer(p))
}

func convertUser(u *sdkclient.User) domain.Peer {
	name := strings_join_nonempty(u.FirstName, u.LastName)
	return domain.Peer{
		ID:          domain.PeerID{Kind: domain.PeerKindUser, ID: u.ID},
		DisplayName: name,
		Username:    u.Username,
	}
}

func strings_join_nonempty(parts ...string) string {
	out := ""
	for _, p := range parts {
		if p == "" {
			continue
		}
		if out != "" {
			out += " "
		}
		out += p
	}
	return out
}

// convertMessage maps an SDK message to the domain message.
func convertMessage(m *sdkclient.Message) domain.Message {
	out := domain.Message{
		ID:        m.ID,
		PeerID:    convertPeerID(m.PeerID),
		Text:      m.Text,
		ReplyTo:   replyToID(m),
		Date:      m.Date,
		IsService: m.IsService,
	}
	if m.Out {
		out.Direction = domain.DirectionOutgoing
	} else {
		out.Direction = domain.DirectionIncoming
	}
	for _, e := range m.TextEntities {
		out.Entities = append(out.Entities, convertEntity(e))
	}
	if m.Media != nil {
		out.Media = convertMedia(m.Media)
	}
	if m.IsService {
		out.ServiceKind = serviceKind(m)
	}
	return out
}

func replyToID(m *sdkclient.Message) *int64 {
	if m.Reply == nil || m.Reply.MessageID == 0 {
		return nil
	}
	id := m.Reply.MessageID
	return &id
}

func convertEntity(e sdkclient.TextEntity) domain.Entity {
	var k domain.EntityKind
	switch e.Kind {
	case sdkclient.TextEntityMention:
		k = domain.EntityMention
	case sdkclient.TextEntityHashtag:
		k = domain.EntityHashtag
	case sdkclient.TextEntityURL:
		k = domain.EntityURL
	case sdkclient.TextEntityBold:
		k = domain.EntityBold
	case sdkclient.TextEntityItalic:
		k = domain.EntityItalic
	case sdkclient.TextEntityCode:
		k = domain.EntityCode
	case sdkclient.TextEntityPre:
		k = domain.EntityPre
	default:
		k = domain.EntityOther
	}
	return domain.Entity{Kind: k, Offset: e.Offset, Length: e.Length, URL: e.URL}
}

func convertMedia(m sdkclient.Media) *domain.MediaSummary {
	switch v := m.(type) {
	case *sdkclient.Document:
		name := ""
		if fn := v.Filename(); fn != "" {
			name = fn
		}
		return &domain.MediaSummary{
			Kind:         domain.MediaKindDocument,
			SizeBytes:    v.Size,
			MimeType:     v.MimeType,
			FileName:     name,
			Downloadable: v.AccessHash != 0 && len(v.FileReference) > 0,
		}
	case *sdkclient.Photo:
		size, ok := v.Largest()
		var b int64
		if ok {
			b = size.ByteCount
		}
		return &domain.MediaSummary{
			Kind:         domain.MediaKindPhoto,
			SizeBytes:    b,
			Downloadable: ok,
		}
	default:
		return &domain.MediaSummary{Kind: domain.MediaKindUnsupported}
	}
}

// serviceKind extracts the service-action label from the raw constructor
// (shape only; unknown actions degrade to the constructor family name).
func serviceKind(m *sdkclient.Message) string {
	if m.Service == nil {
		return ""
	}
	return m.Service.KindName
}

// convertDialog maps an SDK dialog.
func convertDialog(d sdkclient.Dialog) domain.Dialog {
	out := domain.Dialog{
		Peer:            convertResolvedPeer(d.Peer),
		TopMessageID:    int64(d.TopMessageID),
		UnreadCount:     d.UnreadCount,
		UnreadMentions:  d.UnreadMentions,
		ReadInboxMaxID:  int64(d.ReadInboxMaxID),
		ReadOutboxMaxID: int64(d.ReadOutboxMaxID),
		Pinned:          d.Pinned,
	}
	if d.TopMessage != nil {
		out.LastActivity = d.TopMessage.Date
	}
	return out
}

// convertResolvedPeer maps a resolved SDK peer (domain User/Chat/Channel).
func convertResolvedPeer(p sdkclient.Peer) domain.Peer {
	switch v := p.(type) {
	case *sdkclient.User:
		return convertUser(v)
	case *sdkclient.Channel:
		return domain.Peer{
			ID:          domain.PeerID{Kind: domain.PeerKindChannel, ID: v.ID},
			DisplayName: v.Title,
			Username:    v.Username,
		}
	case *sdkclient.Chat:
		return domain.Peer{
			ID:          domain.PeerID{Kind: domain.PeerKindChat, ID: v.ID},
			DisplayName: v.Title,
		}
	default:
		return domain.Peer{}
	}
}

// unexpectedType builds a protocol-failure error for upstream responses
// outside the contract (classified, never a payload dump).
func unexpectedType(op string, v any) error {
	return fmt.Errorf("upstream: %s answered %T: %w", op, v, domain.ErrUpstreamProtocol)
}

// ensure gen import is used even when conversion compiles without it in
// some configurations.
var _ = gen.PeerUser{}
