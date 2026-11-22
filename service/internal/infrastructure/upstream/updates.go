package upstream

// Update conversion: raw upstream constructors → domain events.
// Unsupported families are dropped from *subscriber* delivery here (the
// SDK's MessageBox still tracks their state) — shape-safe, never
// fabricated.

import (
	"time"

	sdkclient "github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

// convertUpdate maps one raw update to a domain event; ok=false when the
// family has no domain representation (state-only families).
func convertUpdate(upd any) (domain.UpdateEvent, bool) {
	switch u := upd.(type) {
	case *gen.Updates:
		// Batched updates: convert the first convertible member. The SDK
		// delivers batched ctor content to handlers as-is; the runner's
		// subscription walks each member through this same function.
		for _, one := range u.Updates {
			if ev, ok := convertUpdateOne(one); ok {
				return ev, true
			}
		}
		return domain.UpdateEvent{}, false
	case gen.UpdateClass:
		return convertUpdateOne(u)
	default:
		return domain.UpdateEvent{}, false
	}
}

func convertUpdateOne(u gen.UpdateClass) (domain.UpdateEvent, bool) {
	switch v := u.(type) {
	case *gen.UpdateNewMessage:
		if m, ok := convertUpdateMessage(v.Message); ok {
			return domain.UpdateEvent{Kind: domain.UpdateNewMessage, NewMessage: &m}, true
		}
		return domain.UpdateEvent{}, false
	case *gen.UpdateNewChannelMessage:
		if m, ok := convertUpdateMessage(v.Message); ok {
			return domain.UpdateEvent{Kind: domain.UpdateNewMessage, NewMessage: &m}, true
		}
		return domain.UpdateEvent{}, false
	case *gen.UpdateEditMessage:
		if m, ok := convertUpdateMessage(v.Message); ok {
			return domain.UpdateEvent{Kind: domain.UpdateEditedMessage, Edited: &m}, true
		}
		return domain.UpdateEvent{}, false
	case *gen.UpdateEditChannelMessage:
		if m, ok := convertUpdateMessage(v.Message); ok {
			return domain.UpdateEvent{Kind: domain.UpdateEditedMessage, Edited: &m}, true
		}
		return domain.UpdateEvent{}, false
	case *gen.UpdateDeleteMessages:
		return domain.UpdateEvent{Kind: domain.UpdateDeletedMessages, Deleted: &domain.DeletedBatch{
			Peer:       domain.PeerID{}, // deletion events carry no peer in this family
			MessageIDs: convertIDs(v.Messages),
		}}, true
	case *gen.UpdateReadHistoryInbox:
		return domain.UpdateEvent{Kind: domain.UpdateReadState, Read: &domain.ReadState{
			Peer: convertWirePeerDomain(v.Peer), MaxID: int64(v.MaxID), Outgoing: false,
		}}, true
	case *gen.UpdateReadHistoryOutbox:
		return domain.UpdateEvent{Kind: domain.UpdateReadState, Read: &domain.ReadState{
			Peer: convertWirePeerDomain(v.Peer), MaxID: int64(v.MaxID), Outgoing: true,
		}}, true
	default:
		return domain.UpdateEvent{}, false
	}
}

func convertIDs(ids []int32) []int64 {
	out := make([]int64, 0, len(ids))
	for _, id := range ids {
		out = append(out, int64(id))
	}
	return out
}

// convertUpdateMessage maps gen.MessageClass to a domain message.
func convertUpdateMessage(mc gen.MessageClass) (domain.Message, bool) {
	switch m := mc.(type) {
	case *gen.Message:
		out := domain.Message{
			ID:        int64(m.ID),
			Text:      m.Message,
			Date:      time.Unix(int64(m.Date), 0),
			Direction: dirOf(m.Out),
		}
		if m.PeerID != nil {
			out.PeerID = convertWirePeerDomain(m.PeerID)
		}
		if m.ReplyTo != nil {
			if hdr, ok := m.ReplyTo.(*gen.MessageReplyHeader); ok && hdr.ReplyToMsgID != nil {
				reply := int64(*hdr.ReplyToMsgID)
				out.ReplyTo = &reply
			}
		}
		for _, e := range m.Entities {
			if te := convertWireEntity(e); te != nil && te.Kind != sdkclient.TextEntityUnknown {
				out.Entities = append(out.Entities, domain.Entity{
					Kind: sdkEntityToDomain(te.Kind), Offset: te.Offset,
					Length: te.Length, URL: te.URL,
				})
			}
		}
		if m.Media != nil {
			ms := classifyMedia(m.Media)
			out.Media = ms
		}
		return out, true
	case *gen.MessageService:
		out := domain.Message{
			ID:          int64(m.ID),
			Date:        time.Unix(int64(m.Date), 0),
			IsService:   true,
			ServiceKind: serviceActionName(m.Action),
		}
		if m.PeerID != nil {
			out.PeerID = convertWirePeerDomain(m.PeerID)
		}
		if m.Out {
			out.Direction = domain.DirectionOutgoing
		} else {
			out.Direction = domain.DirectionIncoming
		}
		return out, true
	default:
		return domain.Message{}, false // MessageEmpty: never fabricated
	}
}

func dirOf(out bool) domain.Direction {
	if out {
		return domain.DirectionOutgoing
	}
	return domain.DirectionIncoming
}

func sdkEntityToDomain(k sdkclient.TextEntityKind) domain.EntityKind {
	switch k {
	case sdkclient.TextEntityMention:
		return domain.EntityMention
	case sdkclient.TextEntityHashtag:
		return domain.EntityHashtag
	case sdkclient.TextEntityURL:
		return domain.EntityURL
	case sdkclient.TextEntityBold:
		return domain.EntityBold
	case sdkclient.TextEntityItalic:
		return domain.EntityItalic
	case sdkclient.TextEntityCode:
		return domain.EntityCode
	case sdkclient.TextEntityPre:
		return domain.EntityPre
	default:
		return domain.EntityOther
	}
}

// classifyMedia: shape-safe media summary from the raw constructor
// (photos/documents/unsupported), mirroring the canonical classification
// without importing SDK internals.
func classifyMedia(m gen.MessageMediaClass) *domain.MediaSummary {
	switch v := m.(type) {
	case *gen.MessageMediaPhoto:
		if p, ok := v.Photo.(*gen.Photo); ok {
			return &domain.MediaSummary{Kind: domain.MediaKindPhoto,
				SizeBytes: largestPhotoSize(p), Downloadable: true}
		}
		return &domain.MediaSummary{Kind: domain.MediaKindPhoto}
	case *gen.MessageMediaDocument:
		if d, ok := v.Document.(*gen.Document); ok {
			name := documentName(d)
			return &domain.MediaSummary{Kind: domain.MediaKindDocument,
				SizeBytes: d.Size, MimeType: d.MimeType, FileName: name,
				Downloadable: d.AccessHash != 0 && len(d.FileReference) > 0}
		}
		return &domain.MediaSummary{Kind: domain.MediaKindDocument}
	default:
		return &domain.MediaSummary{Kind: domain.MediaKindUnsupported}
	}
}

func largestPhotoSize(p *gen.Photo) int64 {
	var max int64
	for _, s := range p.Sizes {
		switch sz := s.(type) {
		case *gen.PhotoSize:
			if int64(sz.Size) > max {
				max = int64(sz.Size)
			}
		case *gen.PhotoSizeProgressive:
			for _, b := range sz.Sizes {
				if int64(b) > max {
					max = int64(b)
				}
			}
		}
	}
	return max
}

func documentName(d *gen.Document) string {
	for _, a := range d.Attributes {
		if fn, ok := a.(*gen.DocumentAttributeFilename); ok {
			return fn.FileName
		}
	}
	return ""
}

// serviceActionName maps service-message actions to stable labels.
func serviceActionName(a gen.MessageActionClass) string {
	switch a.(type) {
	case *gen.MessageActionPinMessage:
		return "pinned"
	case *gen.MessageActionChatCreate:
		return "chat_created"
	case *gen.MessageActionChatEditPhoto:
		return "photo_changed"
	case *gen.MessageActionChatAddUser:
		return "member_added"
	case *gen.MessageActionChatDeleteUser:
		return "member_removed"
	case *gen.MessageActionChatJoinedByLink:
		return "member_joined_by_link"
	case *gen.MessageActionChatEditTitle:
		return "title_changed"
	case nil:
		return ""
	default:
		return "other"
	}
}

var _ = tl.Object(nil) // (raw family retained for future conversions)
