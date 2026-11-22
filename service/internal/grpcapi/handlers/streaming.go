package handlers

// Streaming handlers (S7): server-streaming updates, account events, and
// history. Semantics (ADR-0010): at-most-once per stream; bounded
// per-stream buffers; slow consumers are disconnected (their stream ends
// with RESOURCE_EXHAUSTED / STREAM_OVERFLOW); cancellation propagates to
// the subscription.

import (
	"time"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// StreamUpdates implements UpdateService.StreamUpdates.
func (h *RuntimeHandlers) StreamUpdates(req *v1.StreamUpdatesRequest, stream v1.UpdateService_StreamUpdatesServer) error {
	p, err := principal(stream.Context())
	if err != nil {
		return err
	}
	runner, err := h.runner(stream.Context(), p.TenantID, req.GetAccountId())
	if err != nil {
		return mapError(err)
	}
	events, cancel, err := runner.Subscribe(stream.Context())
	if err != nil {
		return mapError(err)
	}
	defer cancel()

	seq := int64(0)
	for {
		select {
		case <-stream.Context().Done():
			return nil // client cancellation is a clean end
		case ev, ok := <-events:
			if !ok {
				return nil // runner stopped or subscriber dropped
			}
			seq++
			out := &v1.UpdateEvent{StreamSequence: seq}
			switch ev.Kind {
			case domain.UpdateNewMessage:
				out.Event = &v1.UpdateEvent_NewMessage{NewMessage: convertDomainMessage(*ev.NewMessage)}
			case domain.UpdateEditedMessage:
				out.Event = &v1.UpdateEvent_EditedMessage{EditedMessage: convertDomainMessage(*ev.Edited)}
			case domain.UpdateDeletedMessages:
				ids := make([]string, 0, len(ev.Deleted.MessageIDs))
				for _, id := range ev.Deleted.MessageIDs {
					ids = append(ids, domain.FormatID(id))
				}
				out.Event = &v1.UpdateEvent_DeletedMessages{DeletedMessages: &v1.DeletedMessages{
					Peer:       &v1.Peer{PeerId: ev.Deleted.Peer.Canonical(), Kind: convertPeerKind(ev.Deleted.Peer.Kind)},
					MessageIds: ids,
				}}
			case domain.UpdateReadState:
				out.Event = &v1.UpdateEvent_ReadState{ReadState: &v1.ReadState{
					Peer:     &v1.Peer{PeerId: ev.Read.Peer.Canonical(), Kind: convertPeerKind(ev.Read.Peer.Kind)},
					MaxId:    ev.Read.MaxID,
					Outgoing: ev.Read.Outgoing,
				}}
			}
			if err := stream.Send(out); err != nil {
				return nil // transport closed under us — clean end
			}
		}
	}
}

// StreamAccountEvents implements UpdateService.StreamAccountEvents: one
// event per connection/authorization transition.
func (h *RuntimeHandlers) StreamAccountEvents(req *v1.StreamAccountEventsRequest, stream v1.UpdateService_StreamAccountEventsServer) error {
	p, err := principal(stream.Context())
	if err != nil {
		return err
	}
	if err := requireAccountID(req.GetAccountId()); err != nil {
		return mapError(err)
	}
	// v1 polls the runner's observation on a tick (the runner exposes no
	// transition callback yet — S5's runner records transitions; the tick
	// is the v1 delivery mechanism and is documented as such).
	runner, err := h.manager.Get(p.TenantID, domain.AccountID(req.GetAccountId()))
	if err != nil {
		return mapError(err)
	}
	last := domain.ConnectionObservation{Desired: -1, Observed: -1}
	tick := accountEventTick
	seq := int64(0)
	for {
		select {
		case <-stream.Context().Done():
			return nil
		case <-time.After(tick):
			obs := runner.Observation()
			if obs != last {
				seq++
				auth := runner.SessionAuthState()
				if err := stream.Send(&v1.AccountEvent{
					Event:          &v1.AccountEvent_Connection{Connection: convertObservation(obs)},
					StreamSequence: seq,
				}); err != nil {
					return nil
				}
				if err := stream.Send(&v1.AccountEvent{
					Event:          &v1.AccountEvent_Auth{Auth: convertAuthState(auth)},
					StreamSequence: seq,
				}); err != nil {
					return nil
				}
				last = obs
			}
		}
	}
}

func (h *RuntimeHandlers) StreamHistory(req *v1.StreamHistoryRequest, stream v1.MessagingService_StreamHistoryServer) error {
	p, err := principal(stream.Context())
	if err != nil {
		return err
	}
	ref, err := parsePeerRef(req.GetPeer())
	if err != nil {
		return mapError(err)
	}
	limit := int(req.GetLimit())
	r, err := h.runner(stream.Context(), p.TenantID, req.GetAccountId())
	if err != nil {
		return mapError(err)
	}
	var hs historyStreamIface
	err = r.Read(stream.Context(), func(s runtimeSession) (readErr error) {
		hs, readErr = s.StreamHistory(stream.Context(), ref, limit)
		return readErr
	})
	if err != nil {
		return mapError(err)
	}
	for {
		m, ok := hs.Next(stream.Context())
		if !ok {
			return mapError(hs.Err())
		}
		if err := stream.Send(&v1.MessageEvent{Message: convertDomainMessage(m)}); err != nil {
			return nil // transport closed
		}
	}
}

// historyStreamIface aliases the port stream for the closure above.
type historyStreamIface = ports.HistoryStream

// accountEventTick is the StreamAccountEvents poll interval.
const accountEventTick = 2 * time.Second
