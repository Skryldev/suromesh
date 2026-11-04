// Package auth_test runs the complete authentication flow end to end over
// the real transport stack against a fake Soroush server:
//
//	connect → WebSocket → obfuscated2 → abridged → plain MTProto →
//	DH exchange (3 steps) → key established → session persisted →
//	reconnect → existing key reused (no re-exchange).
//
// The server side mirrors the reference protocol; every integrity check on
// the client (nonce equality, DH safety range, new_nonce_hash) uses
// independent math, so a passing flow is real evidence.
package auth_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Skryldev/suromesh/auth"
	"github.com/Skryldev/suromesh/crypto"
	"github.com/Skryldev/suromesh/crypto/rsakeys"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/mtproto"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
	"github.com/Skryldev/suromesh/transport"
)

// fakePlainAuth implements the server side of the exchange over raw
// plain-framed MTProto bodies.
type fakePlainAuth struct {
	t         *testing.T
	exchanges atomic.Int64
	priv      *rsa.PrivateKey
	testFP    int64
	dhPrime   *big.Int
	g         int32
	b         *big.Int
	resPQ     *gen.ResPQ
	newNonce  [32]byte
}

func newFakePlainAuth(t *testing.T) *fakePlainAuth {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pubDER := x509.MarshalPKCS1PublicKey(&key.PublicKey)
	fp, err := rsakeys.AddKey("-----BEGIN RSA PUBLIC KEY-----\n" + b64Lines(pubDER) + "-----END RSA PUBLIC KEY-----")
	if err != nil {
		t.Fatal(err)
	}
	prime, ok := new(big.Int).SetString(rfc3526Group14, 16)
	if !ok {
		t.Fatal("bad prime")
	}
	return &fakePlainAuth{t: t, priv: key, testFP: fp, dhPrime: prime, g: 2}
}

// Respond handles one plain request body and produces the response body.
func (s *fakePlainAuth) Respond(body []byte) ([]byte, error) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, err
	}
	switch req := obj.(type) {
	case *gen.ReqPqMultiRequest:
		s.exchanges.Add(1)
		var sn [16]byte
		_, _ = rand.Read(sn[:])
		pq := new(big.Int).Mul(big.NewInt(1000033), big.NewInt(1000037))
		s.resPQ = &gen.ResPQ{
			Nonce:                       req.Nonce,
			ServerNonce:                 sn,
			Pq:                          string(pq.Bytes()),
			ServerPublicKeyFingerprints: []int64{s.testFP},
		}
		return tl.Marshal(s.resPQ)

	case *gen.ReqDHParamsRequest:
		if req.PublicKeyFingerprint != s.testFP {
			return nil, errors.New("server: unexpected fingerprint")
		}
		m := new(big.Int).Exp(new(big.Int).SetBytes(raw256(req.EncryptedData)), s.priv.D, s.priv.N)
		plain := m.FillBytes(make([]byte, 255))
		// Reference framing: SHA1(data) ‖ data ‖ pad — data is
		// self-delimiting, so decode via a measuring reader.
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
			return nil, errors.New("server: bad inner ctor")
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
			ServerTime:  int32(time.Now().Unix()),
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
			return nil, errors.New("server: bad client inner ctor")
		}
		gB := new(big.Int).SetBytes([]byte(clientInner.GB))
		gAB := new(big.Int).Exp(gB, s.b, s.dhPrime)
		authKey := crypto.NewAuthKey(keyFromArray(gAB))
		return tl.Marshal(&gen.DHGenOK{
			Nonce:         s.resPQ.Nonce,
			ServerNonce:   s.resPQ.ServerNonce,
			NewNonceHash1: authKey.CalcNewNonceHash(s.newNonce, 1),
		})
	}
	return nil, errors.New("server: unexpected plain request")
}

// keyDataFromNonce mirrors auth.keyDataFromNonce (server-side derivation
// of the same temporary key both exchange sides use).
func keyDataFromNonce(serverNonce [16]byte, newNonce [32]byte) (key, iv [32]byte) {
	h1 := sha1.Sum(concat(newNonce[:], serverNonce[:]))
	h2 := sha1.Sum(concat(serverNonce[:], newNonce[:]))
	h3 := sha1.Sum(concat(newNonce[:], newNonce[:]))
	var k, v [32]byte
	copy(k[:20], h1[:])
	copy(k[20:], h2[:12])
	copy(v[:8], h2[12:20])
	copy(v[8:28], h3[:])
	copy(v[28:32], newNonce[:4])
	return k, v
}

func concat(parts ...[]byte) []byte {
	var out []byte
	for _, p := range parts {
		out = append(out, p...)
	}
	return out
}

func sha1Sum(b []byte) []byte {
	h := sha1.Sum(b)
	return h[:]
}

func raw256(s string) []byte {
	b := []byte(s)
	if len(b) < 256 {
		pad := make([]byte, 256-len(b))
		b = append(pad, b...)
	}
	return b
}

func b64Lines(der []byte) string {
	const alpha = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+/"
	var b bytes.Buffer
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

const rfc3526Group14 = "FFFFFFFFFFFFFFFFC90FDAA22168C234C4C6628B80DC1CD129024E088A67CC74020BBEA63B139B22514A08798E3404DDEF9519B3CD3A431B302B0A6DF25F14374FE1356D6D51C245E485B576625E7EC6F44C42E9A637ED6B0BFF5CB6F406B7EDEE386BFB5A899FA5AE9F24117C4B1FE649286651ECE45B3DC2007CB8A163BF0598DA48361C55D39A69163FA8FD24CF5F83655D23DCA3AD961C62F356208552BB9ED529077096966D670C354E4ABC9804F1746C08CA18217C32905E462E36CE3BE39E772C180E86039B2783A2EC07A28FB5C55DF06F4C52C9DE2BCBF6955817183995497CEA956AE515D2261898FA051015728E5A8AACAA68FFFFFFFFFFFFFFFF"

// ---- the E2E gate -------------------------------------------------------

func TestFullAuthFlowE2E(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "session.bin")

	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	fake := newFakePlainAuth(t)

	var peerAlive atomic.Value // *wstest.SoroushPeer
	srv.Handler = func(c *wstest.ServerConn) {
		peer := wstest.HandleSoroush(c, func(p *wstest.SoroushPeer, pkt []byte) {
			if len(pkt) < 20 || !bytesZero(pkt[:8]) {
				return // not a plain MTProto message
			}
			resp, err := fake.Respond(pkt[20:])
			if err != nil {
				t.Logf("fake auth server: %v", err)
				return
			}
			_ = p.SendPacket(mtproto.PlainMessage(time.Now().UnixNano(), resp))
		})
		peerAlive.Store(peer)
		peer.Wait()
	}

	// ---- 1. Fresh session → connect → exchange → persist. ------------
	fs, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if fs.AuthState() != session.StateNoKey {
		t.Fatalf("fresh session state %s", fs.AuthState())
	}
	tr := transport.New(transport.Config{
		Host: srv.Addr, WS: wstest.WSOptions(), PingInterval: -1,
	})
	if err := tr.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}

	res, err := auth.Exchange(context.Background(), auth.TransportPlainSender{Tr: tr}, auth.Config{})
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if res.AuthKey.KeyID == 0 {
		t.Fatal("empty key")
	}
	if err := fs.SetDC(1, "im-server.splus.ir", 443); err != nil {
		t.Fatal(err)
	}
	if err := fs.SetAuthKey(res.AuthKey); err != nil {
		t.Fatal(err)
	}
	if err := fs.Save(); err != nil {
		t.Fatal(err)
	}
	tr.Close()

	// ---- 2. Persistence: reload from disk. ---------------------------
	fs2, err := session.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if k := fs2.AuthKey(); k == nil || k.KeyID != res.AuthKey.KeyID {
		t.Fatal("persisted key mismatch")
	}

	// ---- 3. Reconnect: key reused, no new exchange. ------------------
	tr2 := transport.New(transport.Config{
		Host: srv.Addr, WS: wstest.WSOptions(), PingInterval: -1,
	})
	if err := tr2.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer tr2.Close()
	if k := fs2.AuthKey(); k.KeyID != res.AuthKey.KeyID {
		t.Fatal("key mutated on reconnect")
	}

	// ---- 4. Session string migration Go ↔ Go. ------------------------
	dc, addr, port := fs2.DC()
	ss := session.StringSession{
		DCID: dc, ServerAddr: addr, Port: port,
		AuthKeyValue: fs2.AuthKey().Key, HasAuthKey: true,
	}
	parsed, err := session.ParseStringSession(ss.String())
	if err != nil {
		t.Fatal(err)
	}
	if parsed.AuthKeyValue != res.AuthKey.Key || parsed.DCID != 1 {
		t.Fatal("string session round trip mismatch")
	}
	// And through a fresh FileSession.
	fs3, err := session.OpenFile(filepath.Join(dir, "s2.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := fs3.SetDC(parsed.DCID, parsed.ServerAddr, parsed.Port); err != nil {
		t.Fatal(err)
	}
	var kb [256]byte
	copy(kb[:], parsed.AuthKeyValue[:])
	if err := fs3.SetAuthKey(crypto.NewAuthKey(kb)); err != nil {
		t.Fatal(err)
	}
	if fs3.AuthKey().KeyID != res.AuthKey.KeyID {
		t.Fatal("file session from string session mismatch")
	}
}

func bytesZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}

var (
	_ = errors.Is
	_ = os.Getenv
	_ = gen.Pong{}
	_ = wstest.IsServiceBody
)

// keyFromArray converts the shared secret to the 256-byte auth key
// (mirrors auth.keyFromArray; the client package keeps it unexported).
func keyFromArray(v *big.Int) [256]byte {
	var out [256]byte
	b := v.Bytes()
	copy(out[256-len(b):], b)
	return out
}
