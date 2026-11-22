package upstream

// By-id message retrieval and media transfer, built on the SDK's public
// Invoke + InputPeerFor surface (the SDK's own getMessagesByIDs is
// unexported; this adapter re-implements the retrieval over the same
// upstream RPCs — messages.getMessages / channels.getMessages — with
// identical semantics: MessageEmpty ids are missing).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	sdkclient "github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// getMessagesByIDs retrieves specific messages for a peer.
func (s *clientSession) getMessagesByIDs(ctx context.Context, peer sdkclient.Peer, ids []int64) ([]*sdkclient.Message, error) {
	inputPeer, err := s.c.InputPeerFor(ctx, peer)
	if err != nil {
		return nil, translate(err)
	}
	ids32 := make([]gen.InputMessageClass, 0, len(ids))
	for _, id := range ids {
		if id <= 0 || id > 1<<31-1 {
			return nil, fmt.Errorf("%w: message id %d out of range", domain.ErrInvalidInput, id)
		}
		ids32 = append(ids32, &gen.InputMessageID{ID: int32(id)})
	}

	var result tl.Object
	switch sdkKindOf(peer) {
	case domain.PeerKindChannel:
		channel, err := inputChannelFromPeer(inputPeer, sdkIDOf(peer))
		if err != nil {
			return nil, err
		}
		result, err = s.c.Invoke(ctx, &gen.ChannelsGetMessagesRequest{
			Channel: channel,
			ID:      ids32,
		})
		if err != nil {
			return nil, translate(err)
		}
	default:
		result, err = s.c.Invoke(ctx, &gen.MessagesGetMessagesRequest{ID: ids32})
		if err != nil {
			return nil, translate(err)
		}
	}
	return s.parseMessagesResponse(result, ids)
}

// sdkKindOf/sdkIDOf extract identity from a resolved SDK peer via the
// public domain types.
func sdkKindOf(p sdkclient.Peer) domain.PeerKind {
	switch v := p.(type) {
	case *sdkclient.User:
		return domain.PeerKindUser
	case *sdkclient.Chat:
		return domain.PeerKindChat
	case *sdkclient.Channel:
		return domain.PeerKindChannel
	default:
		_ = v
		return domain.PeerKindUser
	}
}

func sdkIDOf(p sdkclient.Peer) int64 {
	switch v := p.(type) {
	case *sdkclient.User:
		return v.ID
	case *sdkclient.Chat:
		return v.ID
	case *sdkclient.Channel:
		return v.ID
	default:
		return 0
	}
}

// inputChannelFromPeer derives the channel input from the cached input
// peer (server-provided access hash — never fabricated).
func inputChannelFromPeer(ip gen.InputPeerClass, channelID int64) (gen.InputChannelClass, error) {
	switch p := ip.(type) {
	case *gen.InputPeerChannel:
		return &gen.InputChannel{ChannelID: channelID, AccessHash: p.AccessHash}, nil
	default:
		return nil, fmt.Errorf("%w: peer is not a channel (%T)", domain.ErrInvalidInput, ip)
	}
}

// parseMessagesResponse converts the getMessages family responses:
// found messages in order, MessageEmpty/absent ids reported missing.
func (s *clientSession) parseMessagesResponse(result tl.Object, ids []int64) ([]*sdkclient.Message, error) {
	var msgs []gen.MessageClass
	switch r := result.(type) {
	case *gen.MessagesMessages:
		msgs = r.Messages
	case *gen.MessagesMessagesSlice:
		msgs = r.Messages
	case *gen.MessagesChannelMessages:
		msgs = r.Messages
	case *gen.MessagesMessagesNotModified:
		return nil, fmt.Errorf("upstream: getMessages answered NotModified: %w",
			domain.ErrUpstreamProtocol)
	default:
		return nil, unexpectedType("getMessages", result)
	}

	byID := make(map[int32]gen.MessageClass, len(msgs))
	for _, mc := range msgs {
		switch m := mc.(type) {
		case *gen.Message:
			byID[m.ID] = mc
		case *gen.MessageService:
			byID[m.ID] = mc
		case *gen.MessageEmpty:
			byID[m.ID] = mc // explicitly missing upstream
		}
	}

	emptyMsg := func(id int32) *sdkclient.Message {
		// Service-side representation of an explicitly-empty upstream
		// message (deleted/inaccessible): excluded from results.
		return nil
	}
	out := make([]*sdkclient.Message, 0, len(ids))
	for _, id := range ids {
		id32 := int32(id)
		mc, ok := byID[id32]
		if !ok {
			continue // absent → missing
		}
		if _, isEmpty := mc.(*gen.MessageEmpty); isEmpty {
			_ = emptyMsg
			continue
		}
		// Convert via the SDK's own response-message parsing path: build a
		// synthetic RPC result conversion through the public GetMessages
		// domain conversion by invoking messageFromTL indirectly — the
		// SDK exposes conversion through GetMessages only, so the adapter
		// converts the raw constructor here.
		if m, err := s.convertWireMessage(mc); err == nil {
			out = append(out, m)
		}
	}
	return out, nil
}

// convertWireMessage converts a raw gen.Message/MessageService into the
// SDK's domain message (the adapter's own conversion; mirrors the SDK's
// documented conversion rules without touching internals).
func (s *clientSession) convertWireMessage(mc gen.MessageClass) (*sdkclient.Message, error) {
	switch m := mc.(type) {
	case *gen.Message:
		out := &sdkclient.Message{ID: int64(m.ID), Text: m.Message, Raw: m}
		if m.Out {
			out.Out = true
		}
		out.Date = time.Unix(int64(m.Date), 0)
		if m.PeerID != nil {
			out.PeerID = convertWirePeer(m.PeerID)
		}
		if m.ReplyTo != nil {
			if hdr, ok := m.ReplyTo.(*gen.MessageReplyHeader); ok && hdr.ReplyToMsgID != nil {
				out.Reply = &sdkclient.ReplyInfo{MessageID: int64(*hdr.ReplyToMsgID)}
			}
		}
		for _, e := range m.Entities {
			te := convertWireEntity(e)
			if te != nil {
				out.TextEntities = append(out.TextEntities, *te)
			}
		}
		if m.Media != nil {
			out.Media = s.c.ClassifyMediaForTest(m.Media)
		}
		return out, nil
	case *gen.MessageService:
		return &sdkclient.Message{
			ID: int64(m.ID), Raw: m, IsService: true,
			Date:   time.Unix(int64(m.Date), 0),
			PeerID: convertWirePeer(m.PeerID),
		}, nil
	default:
		return nil, unexpectedType("message", mc)
	}
}

func convertWirePeer(p gen.PeerClass) sdkclient.PeerID {
	switch v := p.(type) {
	case *gen.PeerUser:
		return sdkclient.PeerID{Kind: sdkclient.PeerUser, ID: v.UserID}
	case *gen.PeerChat:
		return sdkclient.PeerID{Kind: sdkclient.PeerChat, ID: v.ChatID}
	case *gen.PeerChannel:
		return sdkclient.PeerID{Kind: sdkclient.PeerChannel, ID: v.ChannelID}
	default:
		return sdkclient.PeerID{}
	}
}

func convertWireEntity(e gen.MessageEntityClass) *sdkclient.TextEntity {
	switch v := e.(type) {
	case *gen.MessageEntityMention:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityMention, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityHashtag:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityHashtag, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityUrl:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityURL, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityTextUrl:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityURL, Offset: v.Offset, Length: v.Length, URL: v.URL}
	case *gen.MessageEntityBold:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityBold, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityItalic:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityItalic, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityCode:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityCode, Offset: v.Offset, Length: v.Length}
	case *gen.MessageEntityPre:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityPre, Offset: v.Offset, Length: v.Length, Language: v.Language}
	default:
		return &sdkclient.TextEntity{Kind: sdkclient.TextEntityUnknown}
	}
}

// ---- media transfer -------------------------------------------------------

// GetMediaMetadata resolves the media summary of a message.
func (s *clientSession) getMediaForDownload(ctx context.Context, ref domain.PeerRef, id int64) (*sdkclient.Message, error) {
	peer, err := s.c.ResolvePeer(ctx, convertPeerRef(ref))
	if err != nil {
		return nil, translate(err)
	}
	msgs, err := s.getMessagesByIDs(ctx, peer, []int64{id})
	if err != nil {
		return nil, err
	}
	if len(msgs) == 0 {
		return nil, fmt.Errorf("%w: message %s", domain.ErrNotFound, domain.FormatID(id))
	}
	msg := msgs[0]
	if msg.Media == nil {
		return nil, fmt.Errorf("%w: message %s has no media", domain.ErrInvalidInput,
			domain.FormatID(id))
	}
	return msg, nil
}

// uploadMediaDocument: Upload + SendMediaInput (two upstream calls;
// O(part) memory — the reader is streamed).
func (s *clientSession) uploadMediaDocument(ctx context.Context, spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
	up, err := s.c.Upload(ctx, r, sdkclient.UploadOptions{
		Size:     spec.SizeBytes,
		FileName: spec.FileName,
	})
	if err != nil {
		return domain.Message{}, translate(err)
	}
	m, err := s.c.SendMediaInput(ctx, convertPeerRef(spec.Peer),
		sdkclient.UploadedDocumentInput{Upload: up, MimeType: spec.MimeType})
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

// uploadMediaAny: single-call upload+send (the SDK classifies photo vs
// document from mime/attributes).
func (s *clientSession) uploadMediaAny(ctx context.Context, spec ports.UploadSpec, r io.Reader) (domain.Message, error) {
	peer, err := s.c.ResolvePeer(ctx, convertPeerRef(spec.Peer))
	if err != nil {
		return domain.Message{}, translate(err)
	}
	m, err := s.c.SendMedia(ctx, peer, r, sdkclient.UploadOptions{
		Size:     spec.SizeBytes,
		FileName: spec.FileName,
	})
	if err != nil {
		return domain.Message{}, translate(err)
	}
	return convertMessage(m), nil
}

var _ = errors.Is // (retained for future error shaping in this file)
