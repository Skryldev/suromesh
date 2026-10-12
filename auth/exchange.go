package auth

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"math/big"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/crypto/rsakeys"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// PlainSender exchanges single unencrypted MTProto messages (the Phase 2
// mtproto.PlainMessage framing over a raw connection).
type PlainSender interface {
	// SendPlain writes one plain request body and returns the response body
	// (post-framing: the caller decodes TL from it).
	SendPlain(ctx context.Context, body []byte) ([]byte, error)
}

// Config tunes the exchange. Rand defaults to crypto/rand and must stay
// cryptographically random in production; tests may inject determinism.
type Config struct {
	Rand io.Reader
	// Now returns unix seconds (tests pin the clock).
	Now func() time.Time
}

// Errors.
var (
	// ErrSecurity covers every server-reply integrity failure: wrong
	// nonces, bad DH params, key-range violations, nonce-hash mismatch.
	ErrSecurity = errors.New("auth: security check failed")
	// ErrNoServerKey is returned when no embedded RSA key matches the
	// fingerprints offered by the server.
	ErrNoServerKey = errors.New("auth: no matching server RSA key")
	// ErrStepMismatch is returned when a step answers with an unexpected
	// constructor.
	ErrStepMismatch = errors.New("auth: unexpected response constructor")
)

func (c *Config) fill() {
	if c.Rand == nil {
		c.Rand = rand.Reader
	}
	if c.Now == nil {
		c.Now = time.Now
	}
}

// Result of a successful exchange.
type Result struct {
	AuthKey    *crypto.AuthKey
	TimeOffset int64 // server_time - local_time at exchange time
}

// Exchange runs the full three-step DH handshake against sender
// (network/authenticator.py, verified step by step).
func Exchange(ctx context.Context, sender PlainSender, cfg Config) (*Result, error) {
	cfg.fill()

	// ---- Step 1: req_pq_multi → resPQ --------------------------------
	var nonce [16]byte
	if _, err := io.ReadFull(cfg.Rand, nonce[:]); err != nil {
		return nil, fmt.Errorf("auth: nonce entropy: %w", err)
	}
	resPQ, err := sendDecode[*gen.ResPQ](ctx, sender, &gen.ReqPqMultiRequest{Nonce: nonce})
	if err != nil {
		return nil, err
	}
	if resPQ.Nonce != nonce {
		return nil, fmt.Errorf("%w: step 1 nonce mismatch", ErrSecurity)
	}

	// ---- Step 2: factorize pq, RSA-encrypt inner data -----------------
	pq := new(big.Int).SetBytes([]byte(resPQ.Pq))
	p, q, err := Factorize(pq)
	if err != nil {
		return nil, fmt.Errorf("auth: factorize: %w", err)
	}

	var newNonce [32]byte
	if _, err := io.ReadFull(cfg.Rand, newNonce[:]); err != nil {
		return nil, fmt.Errorf("auth: new_nonce entropy: %w", err)
	}

	inner := &gen.PQInnerData{
		Pq:          resPQ.Pq,
		P:           tlBytesOf(p),
		Q:           tlBytesOf(q),
		Nonce:       resPQ.Nonce,
		ServerNonce: resPQ.ServerNonce,
		NewNonce:    newNonce,
	}
	innerBytes, err := tl.Marshal(inner)
	if err != nil {
		return nil, err
	}

	var serverDH tl.Object
	encrypted := false
	for _, fp := range resPQ.ServerPublicKeyFingerprints {
		block, err := rsakeys.Encrypt(fp, innerBytes, cfg.Rand)
		if err != nil {
			continue // fingerprint unknown to us: try the next
		}
		req := &gen.ReqDHParamsRequest{
			Nonce:                resPQ.Nonce,
			ServerNonce:          resPQ.ServerNonce,
			P:                    inner.P,
			Q:                    inner.Q,
			PublicKeyFingerprint: fp,
			EncryptedData:        string(block),
		}
		serverDH, err = sendDecodeClass(ctx, sender, req)
		if err != nil {
			return nil, err
		}
		encrypted = true
		break
	}
	if !encrypted {
		return nil, fmt.Errorf("%w: offered %x", ErrNoServerKey,
			resPQ.ServerPublicKeyFingerprints)
	}

	dhOk, ok := serverDH.(*gen.ServerDHParamsOK)
	if !ok {
		return nil, fmt.Errorf("%w: step 2 answered %#x", ErrStepMismatch, serverDH.CtorID())
	}
	if dhOk.Nonce != resPQ.Nonce || dhOk.ServerNonce != resPQ.ServerNonce {
		return nil, fmt.Errorf("%w: step 2 nonce mismatch", ErrSecurity)
	}

	// ---- Step 3: decrypt server DH params, complete the exchange -------
	key, iv := keyDataFromNonce(resPQ.ServerNonce, newNonce)
	if len(dhOk.EncryptedAnswer)%16 != 0 {
		return nil, fmt.Errorf("%w: step 3 AES block size mismatch", ErrSecurity)
	}
	plain, err := crypto.DecryptIGE([]byte(dhOk.EncryptedAnswer), key[:], iv[:])
	if err != nil {
		return nil, fmt.Errorf("auth: decrypt DH params: %w", err)
	}
	if len(plain) < 20 {
		return nil, fmt.Errorf("%w: step 3 answer too short", ErrSecurity)
	}
	dhInnerObj, err := tl.Unmarshal(plain[20:]) // skip SHA1 prefix
	if err != nil {
		return nil, fmt.Errorf("auth: decode server_DH_inner_data: %w", err)
	}
	dhInner, ok := dhInnerObj.(*gen.ServerDHInnerData)
	if !ok {
		return nil, fmt.Errorf("%w: step 3 answered %#x", ErrStepMismatch, dhInnerObj.CtorID())
	}
	if dhInner.Nonce != resPQ.Nonce || dhInner.ServerNonce != resPQ.ServerNonce {
		return nil, fmt.Errorf("%w: step 3 nonce mismatch", ErrSecurity)
	}

	dhPrime := new(big.Int).SetBytes([]byte(dhInner.DHPrime))
	g := big.NewInt(int64(dhInner.G))
	gA := new(big.Int).SetBytes([]byte(dhInner.GA))
	timeOffset := int64(dhInner.ServerTime) - cfg.Now().Unix()

	var bBytes [256]byte
	if _, err := io.ReadFull(cfg.Rand, bBytes[:]); err != nil {
		return nil, fmt.Errorf("auth: b entropy: %w", err)
	}
	b := new(big.Int).SetBytes(bBytes[:])
	gB := new(big.Int).Exp(g, b, dhPrime)
	gAB := new(big.Int).Exp(gA, b, dhPrime)

	if err := validateDH(dhPrime, g, gA, gB); err != nil {
		return nil, err
	}

	clientInner := &gen.ClientDHInnerData{
		Nonce:       resPQ.Nonce,
		ServerNonce: resPQ.ServerNonce,
		RetryID:     0,
		GB:          tlBytesOf(gB),
	}
	clientBytes, err := tl.Marshal(clientInner)
	if err != nil {
		return nil, err
	}
	// SHA1(body) ‖ body, encrypted with IGE (no extra padding: the TL
	// framing of client_DH_inner_data already yields a multiple of 16 with
	// the hash prefix, mirroring the reference's construction).
	hashed := append(sha1Sum(clientBytes), clientBytes...)
	if len(hashed)%16 != 0 {
		hashed = paddedTo16(hashed)
	}
	clientEnc, err := crypto.EncryptIGE(hashed, key[:], iv[:])
	if err != nil {
		return nil, err
	}

	dhGen, err := sendDecodeClass(ctx, sender, &gen.SetClientDHParamsRequest{
		Nonce:         resPQ.Nonce,
		ServerNonce:   resPQ.ServerNonce,
		EncryptedData: string(clientEnc),
	})
	if err != nil {
		return nil, err
	}

	var (
		dhGenOk  *gen.DHGenOK
		idx      byte
		nonceHsh [16]byte
	)
	switch g := dhGen.(type) {
	case *gen.DHGenOK:
		dhGenOk, idx, nonceHsh = g, 1, g.NewNonceHash1
	case *gen.DHGenRetry:
		return nil, fmt.Errorf("%w: server requested DH retry (unimplemented)", ErrSecurity)
	case *gen.DHGenFail:
		return nil, fmt.Errorf("%w: server rejected the DH exchange", ErrSecurity)
	default:
		return nil, fmt.Errorf("%w: step 3 answered %#x", ErrStepMismatch, dhGen.CtorID())
	}
	if dhGenOk.Nonce != resPQ.Nonce || dhGenOk.ServerNonce != resPQ.ServerNonce {
		return nil, fmt.Errorf("%w: step 3 completion nonce mismatch", ErrSecurity)
	}

	authKey := crypto.NewAuthKey(keyFromArray(gAB))
	if authKey.CalcNewNonceHash(newNonce, idx) != nonceHsh {
		return nil, fmt.Errorf("%w: step 3 new_nonce_hash mismatch", ErrSecurity)
	}

	return &Result{AuthKey: authKey, TimeOffset: timeOffset}, nil
}

// sendDecode sends one plain request and decodes a typed response.
func sendDecode[T tl.Object](ctx context.Context, s PlainSender, req tl.Request) (T, error) {
	var zero T
	body, err := tl.Marshal(req)
	if err != nil {
		return zero, err
	}
	resp, err := s.SendPlain(ctx, body)
	if err != nil {
		return zero, err
	}
	obj, err := tl.Unmarshal(resp)
	if err != nil {
		return zero, fmt.Errorf("auth: decode response: %w", err)
	}
	typed, ok := obj.(T)
	if !ok {
		return zero, fmt.Errorf("%w: got %#x", ErrStepMismatch, obj.CtorID())
	}
	return typed, nil
}

// sendDecodeClass is the interface-typed variant (ServerDHParams family).
func sendDecodeClass(ctx context.Context, s PlainSender, req tl.Request) (tl.Object, error) {
	body, err := tl.Marshal(req)
	if err != nil {
		return nil, err
	}
	resp, err := s.SendPlain(ctx, body)
	if err != nil {
		return nil, err
	}
	return tl.Unmarshal(resp)
}
