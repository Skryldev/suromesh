package auth

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"errors"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/crypto/rsakeys"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// fakePlainServer drives the server side of the exchange with a TEST
// keypair. All server math mirrors the protocol steps; the client's own
// integrity checks (nonce equality, DH range checks, new_nonce_hash) run
// with independent math, so a passing Exchange is real evidence.
type fakePlainServer struct {
	t *testing.T

	authKey        *crypto.AuthKey // derived server-side (SetClientDHParams)
	testFP         int64
	priv           *rsa.PrivateKey
	dhPrime        *big.Int
	g              int32
	b              *big.Int
	resPQ          *gen.ResPQ
	newNonce       [32]byte
	serverTimeBase int64

	// Fault injection.
	corruptStep1Nonce bool
	unknownFP         bool
	corruptStep3Hash  bool
	badTempKey        bool
}

func newFakePlainServer(t *testing.T) *fakePlainServer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	pem := "-----BEGIN RSA PUBLIC KEY-----\n" + pemLines(pubDER) + "-----END RSA PUBLIC KEY-----"
	fp, err := rsakeys.AddKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	prime, ok := new(big.Int).SetString(rfc3526Group14, 16)
	if !ok {
		t.Fatal("bad prime")
	}
	return &fakePlainServer{
		t:              t,
		testFP:         fp,
		priv:           key,
		dhPrime:        prime,
		g:              2,
		serverTimeBase: 1700000000,
	}
}

// pemLines base64-encodes DER at 64 chars/line (PEM body convention).
func pemLines(der []byte) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b strings.Builder
	for i := 0; i < len(der); i += 3 {
		n := len(der) - i
		var v uint32
		v = uint32(der[i]) << 16
		if n > 1 {
			v |= uint32(der[i+1]) << 8
		}
		if n > 2 {
			v |= uint32(der[i+2])
		}
		b.WriteByte(alpha[(v>>18)&63])
		b.WriteByte(alpha[(v>>12)&63])
		if n > 1 {
			b.WriteByte(alpha[(v>>6)&63])
		} else {
			b.WriteByte('=')
		}
		if n > 2 {
			b.WriteByte(alpha[v&63])
		} else {
			b.WriteByte('=')
		}
		if (i/3+1)%24 == 0 {
			b.WriteByte('\n')
		}
	}
	b.WriteByte('\n')
	return b.String()
}

func (s *fakePlainServer) SendPlain(ctx context.Context, body []byte) ([]byte, error) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, err
	}
	switch req := obj.(type) {
	case *gen.ReqPqMultiRequest:
		var sn [16]byte
		_, _ = rand.Read(sn[:])
		fp := s.testFP
		if s.unknownFP {
			fp = 0x1234
		}
		pq := new(big.Int).Mul(big.NewInt(1000033), big.NewInt(1000037))
		s.resPQ = &gen.ResPQ{
			Nonce:                       req.Nonce,
			ServerNonce:                 sn,
			Pq:                          string(pq.Bytes()),
			ServerPublicKeyFingerprints: []int64{fp},
		}
		if s.corruptStep1Nonce {
			s.resPQ.Nonce[0] ^= 1
		}
		return tl.Marshal(s.resPQ)

	case *gen.ReqDHParamsRequest:
		if req.PublicKeyFingerprint != s.testFP {
			return nil, errors.New("unexpected fingerprint")
		}
		// Raw modexp with the test private key; verify SHA1 framing exactly
		// as the real server does.
		m := new(big.Int).Exp(new(big.Int).SetBytes(raw256(req.EncryptedData)), s.priv.D, s.priv.N)
		// The padded block is exactly 255 bytes; FillBytes restores any
		// leading zeros that m.Bytes() would drop.
		plain := m.FillBytes(make([]byte, 255))
		// Reference framing: SHA1(data) ‖ data ‖ random padding — the hash
		// covers exactly the TL body, which is self-delimiting. Decode via
		// a measuring reader to find the body length.
		r := tl.NewReader(plain[20:])
		innerObj, err := tl.DecodeObject(r)
		if err != nil {
			return nil, err
		}
		bodyLen := len(plain[20:]) - r.Remaining()
		if !bytes.Equal(plain[:20], sha1Sum(plain[20:20+bodyLen])) {
			return nil, errors.New("server: bad RSA block framing")
		}
		inner, ok := innerObj.(*gen.PQInnerData)
		if !ok {
			return nil, errors.New("bad inner ctor")
		}
		if inner.Pq != s.resPQ.Pq || inner.Nonce != s.resPQ.Nonce ||
			inner.ServerNonce != s.resPQ.ServerNonce {
			return nil, errors.New("server: inner nonce mismatch")
		}
		s.newNonce = inner.NewNonce

		var bBytes [256]byte
		_, _ = rand.Read(bBytes[:])
		s.b = new(big.Int).SetBytes(bBytes[:])
		gA := new(big.Int).Exp(big.NewInt(int64(s.g)), s.b, s.dhPrime)

		dhInner := &gen.ServerDHInnerData{
			Nonce:       s.resPQ.Nonce,
			ServerNonce: s.resPQ.ServerNonce,
			G:           s.g,
			DHPrime:     string(s.dhPrime.Bytes()),
			GA:          string(gA.Bytes()),
			ServerTime:  int32(s.serverTimeBase),
		}
		dhInnerBytes, err := tl.Marshal(dhInner)
		if err != nil {
			return nil, err
		}
		hashed := append(sha1Sum(dhInnerBytes), dhInnerBytes...)
		if pad := (16 - len(hashed)%16) % 16; pad != 0 {
			hashed = append(hashed, make([]byte, pad)...)
		}
		key, iv := keyDataFromNonce(s.resPQ.ServerNonce, s.newNonce)
		if s.badTempKey {
			key[0] ^= 1
		}
		enc, err := crypto.EncryptIGE(hashed, key[:], iv[:])
		if err != nil {
			return nil, err
		}
		return tl.Marshal(&gen.ServerDHParamsOK{
			Nonce:           s.resPQ.Nonce,
			ServerNonce:     s.resPQ.ServerNonce,
			EncryptedAnswer: string(enc),
		})

	case *gen.SetClientDHParamsRequest:
		key, iv := keyDataFromNonce(s.resPQ.ServerNonce, s.newNonce)
		plain, err := crypto.DecryptIGE([]byte(req.EncryptedData), key[:], iv[:])
		if err != nil {
			return nil, err
		}
		clientInnerObj, err := tl.Unmarshal(plain[20:])
		if err != nil {
			return nil, err
		}
		clientInner, ok := clientInnerObj.(*gen.ClientDHInnerData)
		if !ok {
			return nil, errors.New("bad client inner ctor")
		}
		gB := new(big.Int).SetBytes([]byte(clientInner.GB))
		gAB := new(big.Int).Exp(gB, s.b, s.dhPrime)

		s.authKey = crypto.NewAuthKey(keyFromArray(gAB))
		h := s.authKey.CalcNewNonceHash(s.newNonce, 1)
		if s.corruptStep3Hash {
			h[0] ^= 1
		}
		return tl.Marshal(&gen.DHGenOK{
			Nonce:         s.resPQ.Nonce,
			ServerNonce:   s.resPQ.ServerNonce,
			NewNonceHash1: h,
		})
	}
	return nil, errors.New("unexpected plain request")
}

func raw256(s string) []byte {
	b := []byte(s)
	if len(b) < 256 {
		pad := make([]byte, 256-len(b))
		b = append(pad, b...)
	}
	return b
}

// ---- Exchange tests ------------------------------------------------------

func TestExchangeAgainstFakeServer(t *testing.T) {
	srv := newFakePlainServer(t)
	res, err := Exchange(context.Background(), srv, Config{
		Now: func() time.Time { return time.Unix(1700000000, 0) },
	})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.AuthKey.KeyID == 0 {
		t.Fatal("empty auth key id")
	}
	if res.TimeOffset != 0 {
		t.Fatalf("time offset %d, want 0 (clocks pinned)", res.TimeOffset)
	}
	// The derived key must match the server's independently computed one.
	if res.AuthKey.KeyID != srv.authKey.KeyID {
		t.Fatalf("key mismatch: client %x vs server %x",
			res.AuthKey.KeyID, srv.authKey.KeyID)
	}
}

func TestExchangeRejectsCorruptStep1Nonce(t *testing.T) {
	srv := newFakePlainServer(t)
	srv.corruptStep1Nonce = true
	_, err := Exchange(context.Background(), srv, Config{})
	if !errors.Is(err, ErrSecurity) {
		t.Fatalf("error %v, want ErrSecurity", err)
	}
}

func TestExchangeRejectsUnknownFingerprint(t *testing.T) {
	srv := newFakePlainServer(t)
	srv.unknownFP = true
	_, err := Exchange(context.Background(), srv, Config{})
	if !errors.Is(err, ErrNoServerKey) {
		t.Fatalf("error %v, want ErrNoServerKey", err)
	}
}

func TestExchangeRejectsCorruptStep3Hash(t *testing.T) {
	srv := newFakePlainServer(t)
	srv.corruptStep3Hash = true
	_, err := Exchange(context.Background(), srv, Config{})
	if !errors.Is(err, ErrSecurity) {
		t.Fatalf("error %v, want ErrSecurity", err)
	}
}

func TestExchangeRejectsBadTempKey(t *testing.T) {
	srv := newFakePlainServer(t)
	srv.badTempKey = true
	_, err := Exchange(context.Background(), srv, Config{})
	if err == nil {
		t.Fatal("garbled step 2 answer accepted")
	}
}

const rfc3526Group14 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF"
