// Package upstream implements ports.UpstreamFactory against the SuroMesh
// SDK. This package is the ONLY service code that imports the SDK.
//
// Session state format (v1, service-owned, encrypted at rest by the
// persistence layer): JSON with the auth key, DC routing, authorization
// flag, and the update-state frontier — everything needed to restore a
// client with zero re-authentication and correct update continuation.
package upstream

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"

	sdkclient "github.com/Skryldev/suromesh/client"
	sdkcrypto "github.com/Skryldev/suromesh/crypto"
	sdksession "github.com/Skryldev/suromesh/session"

	"github.com/Skryldev/suromesh/service/internal/domain"
	"github.com/Skryldev/suromesh/service/internal/ports"
)

// sessionBlobV1 is the service-owned serialized session state.
type sessionBlobV1 struct {
	Version        int          `json:"version"` // always 1
	DCID           int          `json:"dc_id"`
	Addr           string       `json:"addr"`
	Port           int          `json:"port"`
	AuthKey        string       `json:"auth_key_b64,omitempty"` // base64 of the 256-byte key
	HasAuthKey     bool         `json:"has_auth_key"`
	Authorized     bool         `json:"authorized"`
	Pts            int32        `json:"pts"`
	Qts            int32        `json:"qts"`
	Date           int32        `json:"date"`
	Seq            int32        `json:"seq"`
	HasUpdateState bool         `json:"has_update_state"`
	Channels       []channelPts `json:"channels,omitempty"`
}

type channelPts struct {
	ID  int64 `json:"id"`
	Pts int32 `json:"pts"`
}

const sessionFormatVersion = 1

// marshalSession encodes the SDK session into the service blob format.
func marshalSession(s *sdksession.Session) ([]byte, error) {
	b := sessionBlobV1{Version: sessionFormatVersion}
	b.DCID, b.Addr, b.Port = s.DC()
	if k := s.AuthKey(); k != nil && k.HasValue {
		b.HasAuthKey = true
		b.AuthKey = base64.StdEncoding.EncodeToString(k.Key[:])
	}
	b.Authorized = s.AuthState() == sdksession.StateAuthorized
	if st, ok := s.UpdateState(); ok {
		b.HasUpdateState = true
		b.Pts, b.Qts, b.Date, b.Seq = st.Pts, st.Qts, st.Date, st.Seq
	}
	channels := s.ChannelUpdateStates()
	// Sort by channel id: the SDK stores these in a map (iteration order
	// is random), so an unsorted array makes byte-stable comparison and
	// round-trip fidelity impossible. (Conformance test finding, Phase 26.)
	for i := 1; i < len(channels); i++ {
		for j := i; j > 0 && channels[j].ChannelID < channels[j-1].ChannelID; j-- {
			channels[j], channels[j-1] = channels[j-1], channels[j]
		}
	}
	for _, c := range channels {
		b.Channels = append(b.Channels, channelPts{ID: c.ChannelID, Pts: c.Pts})
	}
	return json.Marshal(b)
}

// unmarshalSession rebuilds an SDK session from the service blob.
func unmarshalSession(blob []byte) (*sdksession.Session, error) {
	var b sessionBlobV1
	if err := json.Unmarshal(blob, &b); err != nil {
		return nil, fmt.Errorf("%w: session blob: %v", domain.ErrCrypto, err)
	}
	if b.Version != sessionFormatVersion {
		return nil, fmt.Errorf("%w: session blob version %d, want %d",
			domain.ErrCrypto, b.Version, sessionFormatVersion)
	}
	var sess *sdksession.Session
	if b.HasAuthKey {
		raw, err := base64.StdEncoding.DecodeString(b.AuthKey)
		if err != nil || len(raw) != 256 {
			return nil, fmt.Errorf("%w: session auth key", domain.ErrCrypto)
		}
		var keyArr [256]byte
		copy(keyArr[:], raw)
		sess = sdksession.NewWithKey(sdkcrypto.NewAuthKey(keyArr))
	} else {
		sess = sdksession.New()
	}
	if b.DCID != 0 || b.Addr != "" {
		if err := sess.SetDC(b.DCID, b.Addr, b.Port); err != nil {
			return nil, fmt.Errorf("%w: session dc: %v", domain.ErrCrypto, err)
		}
	}
	if b.Authorized && b.HasAuthKey {
		sess.SetAuthorized()
	}
	if b.HasUpdateState {
		if err := sess.SetUpdateState(sdksession.UpdateStateRecord{
			Pts: b.Pts, Qts: b.Qts, Date: b.Date, Seq: b.Seq,
		}); err != nil {
			return nil, fmt.Errorf("%w: session update state: %v", domain.ErrCrypto, err)
		}
	}
	for _, c := range b.Channels {
		if err := sess.SetChannelUpdateState(c.ID, c.Pts); err != nil {
			return nil, fmt.Errorf("%w: session channel state: %v", domain.ErrCrypto, err)
		}
	}
	return sess, nil
}

// Factory implements ports.UpstreamFactory with the real SDK.
type Factory struct{}

// NewFactory builds the production adapter.
func NewFactory() *Factory { return &Factory{} }

var _ ports.UpstreamFactory = (*Factory)(nil)

// CreateClient builds a live SDK client from the decrypted blob (or a
// fresh session when empty). The onPersist callback receives
// service-format session blobs whenever the SDK signals a change worth
// persisting.
func (f *Factory) CreateClient(ctx context.Context, spec ports.ClientSpec) (ports.ClientSession, error) {
	if spec.OnPersist == nil {
		return nil, fmt.Errorf("upstream: OnPersist must be provided")
	}
	var sess *sdksession.Session
	if len(spec.SessionBlob) > 0 {
		var err error
		if sess, err = unmarshalSession(spec.SessionBlob); err != nil {
			return nil, err
		}
	} else {
		sess = sdksession.New()
	}

	persist := func() error {
		blob, err := marshalSession(sess)
		if err != nil {
			return err
		}
		return spec.OnPersist(blob)
	}

	c, err := sdkclient.New(sdkclient.Config{
		Session: sess,
		Saver:   &persistSaver{fn: persist},
	})
	if err != nil {
		return nil, fmt.Errorf("upstream: client construction: %w", err)
	}
	return &clientSession{account: spec.AccountID, c: c, sess: sess, persist: persist}, nil
}

// persistSaver adapts the SDK's Saver hook to the service blob format.
type persistSaver struct{ fn func() error }

func (p *persistSaver) Save() error { return p.fn() }

// account implements ports.ClientSession over one SDK client.
type clientSession struct {
	account domain.AccountID
	c       *sdkclient.Client
	sess    *sdksession.Session
	persist func() error
}
