package client_test

// Fake Soroush auth server (Phase 9). Exercises the real transport →
// crypto → RPC → client stack for the auth constructor family:
//
//	auth.sendCode#3e72ba19 family (0xa677244f), auth.resendCode (0x3ef1a9bf)
//	auth.signIn (0x8d52a951), auth.checkPassword (0xd18b4d16)
//	auth.logOut (0x3e72ba19), account.getPassword (0x548a30f5)
//
// The 2FA path implements the SERVER side of MTProto cloud-password SRP
// (independent big-int math, per the protocol relation
// S = (A · v^u)^b mod p, B = (g^b + k·v) mod p): the fake derives x from
// its configured password, builds B, and validates the client's (A, M1)
// with a constant-time comparison. Cross-implementation correctness of the
// client math is separately pinned by the Python golden vectors
// (TestSRPGolden in the client package).

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/binary"
	"math/big"
	"sync"
	"testing"

	"github.com/Skryldev/suromesh/client"
	"github.com/Skryldev/suromesh/internal/wstest"
	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// srpTestPrime is the 2048-bit MTProto modulus — the reference's
// pre-validated good_prime (reference/SPlusthon/splusthon/password.py:46-62),
// transcribed here for the fake SERVER (the client's copy lives in
// client/srp.go; the golden vectors bind the two together).
var srpTestPrime = mustBigFromHex(
	"C71CAEB9C6B1C9048E6C522F70F13F73980D40238E3E21C14934D037563D930F" +
		"48198A0AA7C14058229493D22530F4DBFA336F6E0AC925139543AED44CCE7C37" +
		"20FD51F69458705AC68CD4FE6B6B13ABDC9746512969328454F18FAF8C595F64" +
		"2477FE96BB2A941D5BCD1D4AC8CC49880708FA9B378E3C4F3A9060BEE67CF9A4" +
		"A4A695811051907E162753B56B0F6B410DBA74D8A84B2A14B3144E0EF1284754" +
		"FD17ED950D5965B4B9DD46582DB1178D169C6BC465B0D6FF9CA3928FEF5B9AE4" +
		"E418FC15E83EBEA0F87FA9FF5EED70050DED2849F47BF959D956850CE929851F" +
		"0D8115F635B105EE2E4E15D04B2454BF6F4FADF034B10403119CD8E3B92FCC5B")

func mustBigFromHex(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 16)
	if !ok || v.BitLen() != 2048 {
		panic("fakeauth: bad prime literal")
	}
	return v
}

const (
	authCtorSendCode      = 0xa677244f
	authCtorResendCode    = 0x3ef1a9bf
	authCtorSignIn        = 0x8d52a951
	authCtorCheckPassword = 0xd18b4d16
	authCtorLogOut        = 0x3e72ba19
	authCtorGetPassword   = 0x548a30f5
)

func isAuthCtor(ctor uint32) bool {
	switch ctor {
	case authCtorSendCode, authCtorResendCode, authCtorSignIn,
		authCtorCheckPassword, authCtorLogOut, authCtorGetPassword:
		return true
	}
	return false
}

// authFake is the scripted auth server.
type authFake struct {
	mu sync.Mutex

	// Phone/code flow state.
	code           string // the only accepted confirmation code
	codeHash       string // issued by sendCode
	codeExpired    bool   // next signIn with a hash → PHONE_CODE_EXPIRED
	signUpRequired bool   // signIn → auth.AuthorizationSignUpRequired

	// Scripted failures (one-shot where noted).
	sendCodeErr      *rpc.RPCError // returned by every sendCode
	signInErr        *rpc.RPCError // returned by every signIn (before other checks)
	logoutErr        *rpc.RPCError // returned by logOut (reference auth.py:610-612)
	sendCodeRestarts int           // AUTH_RESTART this many times, then succeed
	sendCodeCalls    int
	signInCalls      int
	checkPWCalls     int
	logoutCalls      int
	floodSignInAt    int // 1-based signIn call that answers FLOOD_WAIT_X
	lastSignedPhone  string

	// 2FA server state (armed by setPassword).
	passwordOn bool
	password   string
	badPrime   bool // serve a corrupted P (client must reject via ErrSRPParams)
	salt1      []byte
	salt2      []byte
	g          int32
	p          *big.Int
	b          *big.Int // server private exponent
	srpB       []byte
	srpID      int64
	v          *big.Int // g^x mod p
	k          *big.Int
	lastA      []byte
	lastM1     []byte

	loggedOut bool
}

func newAuthFake(code string) *authFake {
	return &authFake{code: code, codeHash: "fake_phone_code_hash_0001", g: 3, srpID: 0x2fa2fa2fa2fa2fa}
}

// arm2FA configures the server-side password verifier.
func (f *authFake) arm2FA(password string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.passwordOn = true
	f.password = password
	f.salt1 = bytes.Repeat([]byte{0x5a}, 16)
	f.salt2 = bytes.Repeat([]byte{0xc3}, 16)
	f.p = srpTestPrime

	x := authFakeX(f.salt1, f.salt2, password)
	f.k = new(big.Int).SetBytes(authFakeSHA256(pad256(f.p.Bytes()), pad256(big.NewInt(int64(f.g)).Bytes())))
	f.v = new(big.Int).Exp(big.NewInt(int64(f.g)), x, f.p)

	bBytes := make([]byte, 256)
	if _, err := rand.Read(bBytes); err != nil {
		panic(err) // test double: entropy is a hard requirement
	}
	f.b = new(big.Int).SetBytes(bBytes)
	// B = (g^b + k·v) mod p
	gb := new(big.Int).Exp(big.NewInt(int64(f.g)), f.b, f.p)
	kv := new(big.Int).Mod(new(big.Int).Mul(f.k, f.v), f.p)
	B := new(big.Int).Mod(new(big.Int).Add(gb, kv), f.p)
	f.srpB = pad256(B.Bytes())
}

// authFakeSHA256 is the test-side multi-part SHA-256.
func authFakeSHA256(parts ...[]byte) []byte {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func pad256(b []byte) []byte {
	out := make([]byte, 256)
	copy(out[256-len(b):], b)
	return out
}

// authFakeX mirrors the reference KDF chain server-side
// (password.py:113-118) with its own PBKDF2 implementation.
func authFakeX(salt1, salt2 []byte, password string) *big.Int {
	pw := []byte(password)
	h1 := authFakeSHA256(salt1, pw, salt1)
	h2 := authFakeSHA256(salt2, h1, salt2)
	h3 := fakePBKDF2SHA512(h2, salt1, 100000, 64)
	return new(big.Int).SetBytes(authFakeSHA256(salt2, h3, salt2))
}

// fakePBKDF2SHA512 is PBKDF2 (RFC 8018) with HMAC-SHA512. Independent of
// the client implementation by construction (separate package, separate
// code); cross-implementation equivalence is pinned by the Python vectors.
func fakePBKDF2SHA512(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha512.New, password)
	hLen := sha512.Size
	blocks := (keyLen + hLen - 1) / hLen
	var out []byte
	buf := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		prf.Reset()
		prf.Write(salt)
		binary.BigEndian.PutUint32(buf, uint32(block))
		prf.Write(buf)
		u := prf.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for i := 1; i < iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(u[:0])
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

// verifySRP validates a client (A, M1) pair server-side:
//
//	u = SHA256(pad(A) ‖ pad(B))
//	S = (A · v^u)^b mod p
//	M1 = SHA256(xor(H(pad p), H(pad g)), H(salt1), H(salt2), pad(A), pad(B), H(pad(S)))
func (f *authFake) verifySRP(a, m1 []byte) bool {
	if f.v == nil || len(a) == 0 || len(m1) == 0 {
		return false
	}
	Av := new(big.Int).Mod(new(big.Int).SetBytes(a), f.p)
	Av = Av.Mul(Av, new(big.Int).Exp(f.v,
		new(big.Int).SetBytes(authFakeSHA256(pad256(a), pad256(f.srpB))), f.p))
	Av = Av.Mod(Av, f.p)
	S := new(big.Int).Exp(Av, f.b, f.p)
	gForHash := pad256(big.NewInt(int64(f.g)).Bytes())
	xored := make([]byte, 32)
	hp := authFakeSHA256(pad256(f.p.Bytes()))
	hg := authFakeSHA256(gForHash)
	for i := range xored {
		xored[i] = hp[i] ^ hg[i]
	}
	expect := authFakeSHA256(
		xored,
		authFakeSHA256(f.salt1),
		authFakeSHA256(f.salt2),
		pad256(a),
		pad256(f.srpB),
		authFakeSHA256(pad256(S.Bytes())),
	)
	return subtle.ConstantTimeCompare(expect, m1) == 1
}

// handle answers one auth RPC.
func (f *authFake) handle(t testing.TB, ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
	obj, err := tl.Unmarshal(body)
	if err != nil {
		return nil, &rpc.RPCError{Code: 400, Message: "BAD_REQUEST"}
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	switch ctor {
	case authCtorSendCode:
		req, ok := obj.(*gen.AuthSendCodeRequest)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_SEND_CODE"}
		}
		f.sendCodeCalls++
		if f.sendCodeErr != nil {
			e := f.sendCodeErr
			return nil, e
		}
		if f.sendCodeRestarts > 0 {
			f.sendCodeRestarts--
			return nil, &rpc.RPCError{Code: 420, Message: "AUTH_RESTART"}
		}
		f.lastSignedPhone = req.PhoneNumber
		f.codeExpired = false
		return &gen.AuthSentCode{
			Type_:         &gen.AuthSentCodeTypeSms{Length: 5},
			PhoneCodeHash: f.codeHash,
			Timeout:       ptrInt32(90),
		}, nil

	case authCtorResendCode:
		req, ok := obj.(*gen.AuthResendCodeRequest)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_RESEND_CODE"}
		}
		if req.PhoneCodeHash != f.codeHash {
			return nil, &rpc.RPCError{Code: 400, Message: "PHONE_CODE_EXPIRED"}
		}
		f.lastSignedPhone = req.PhoneNumber
		return &gen.AuthSentCode{
			Type_:         &gen.AuthSentCodeTypeApp{Length: 5},
			PhoneCodeHash: f.codeHash + "-resend",
		}, nil

	case authCtorSignIn:
		req, ok := obj.(*gen.AuthSignInRequest)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_SIGN_IN"}
		}
		f.signInCalls++
		f.lastSignedPhone = req.PhoneNumber
		if f.signInErr != nil {
			e := f.signInErr
			return nil, e
		}
		if f.floodSignInAt > 0 && f.signInCalls == f.floodSignInAt {
			return nil, &rpc.RPCError{Code: 420, Message: "FLOOD_WAIT_2"}
		}
		if f.codeExpired || req.PhoneCodeHash != f.codeHash {
			return nil, &rpc.RPCError{Code: 400, Message: "PHONE_CODE_EXPIRED"}
		}
		if req.PhoneCode == nil || *req.PhoneCode != f.code {
			return nil, &rpc.RPCError{Code: 400, Message: "PHONE_CODE_INVALID"}
		}
		if f.passwordOn {
			return nil, &rpc.RPCError{Code: 401, Message: "SESSION_PASSWORD_NEEDED"}
		}
		if f.signUpRequired {
			f.signUpRequired = false
			return &gen.AuthAuthorizationSignUpRequired{TermsOfService: nil}, nil
		}
		f.loggedOut = false
		return f.authorization(), nil

	case authCtorGetPassword:
		if !f.passwordOn {
			return nil, &rpc.RPCError{Code: 400, Message: "PASSWORD_NOT_SET"}
		}
		servedP := f.p.Bytes()
		if f.badPrime {
			servedP = append([]byte(nil), servedP...)
			servedP[0] ^= 0xFF
		}
		return &gen.AccountPassword{
			HasPassword: true,
			CurrentAlgo: &gen.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
				Salt1: f.salt1, Salt2: f.salt2, G: f.g, P: servedP,
			},
			SRPID:         &f.srpID,
			SRPB:          f.srpB,
			NewAlgo:       &gen.PasswordKdfAlgoUnknown{},
			NewSecureAlgo: &gen.SecurePasswordKdfAlgoUnknown{},
		}, nil

	case authCtorCheckPassword:
		req, ok := obj.(*gen.AuthCheckPasswordRequest)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_CHECK_PASSWORD"}
		}
		f.checkPWCalls++
		srp, ok := req.Password.(*gen.InputCheckPasswordSRP)
		if !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "PASSWORD_HASH_INVALID"}
		}
		f.lastA = append([]byte(nil), srp.A...)
		f.lastM1 = append([]byte(nil), srp.M1...)
		if srp.SRPID != f.srpID || !f.verifySRP(srp.A, srp.M1) {
			return nil, &rpc.RPCError{Code: 400, Message: "PASSWORD_HASH_INVALID"}
		}
		f.loggedOut = false
		return f.authorization(), nil

	case authCtorLogOut:
		if _, ok := obj.(*gen.AuthLogOutRequest); !ok {
			return nil, &rpc.RPCError{Code: 400, Message: "NOT_LOGOUT"}
		}
		f.logoutCalls++
		if f.logoutErr != nil {
			e := f.logoutErr
			return nil, e
		}
		f.loggedOut = true
		return &gen.AuthLoggedOut{FutureAuthToken: []byte{}}, nil
	}
	return nil, &rpc.RPCError{Code: 400, Message: "UNKNOWN_AUTH_METHOD"}
}

func (f *authFake) authorization() tl.Object {
	id := int64(7001)
	accessHash := int64(0xabc123)
	first := "Golden"
	username := "goldenuser"
	return &gen.AuthAuthorization{
		User: &gen.User{
			ID:         id,
			UserType:   &gen.UserTypeNormal{},
			AccessHash: &accessHash,
			FirstName:  &first,
			Username:   &username,
		},
	}
}

func ptrInt32(v int32) *int32 { return &v }

// installAuthHooks routes the auth constructor family to the fake.
func installAuthHooks(t testing.TB, fake *appFake, af *authFake) {
	prev := fake.responder
	fake.SetResponder(func(ctor uint32, body []byte) (tl.Object, *rpc.RPCError) {
		if isAuthCtor(ctor) {
			return af.handle(t, ctor, body)
		}
		if prev != nil {
			return prev(ctor, body)
		}
		return nil, &rpc.RPCError{Code: 400, Message: "NOT_HANDLED"}
	})
}

// newAuthClient: ready client whose fake server answers the auth family.
// The wstest server is closed via t.Cleanup (after the test's own defers).
func newAuthClient(t testing.TB) (*client.Client, *appFake, *authFake) {
	t.Helper()
	srv, err := wstest.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { srv.Close() })
	c, key := newTestClient(t, srv.Addr)
	fake := wireFakeAppServer(t, srv, key)
	fake.SetFrameLog(false)
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	af := newAuthFake("54321")
	installAuthHooks(t, fake, af)
	return c, fake, af
}

// genPingAlias is a real, generic request used by stress tests.
type genPingAlias = gen.PingRequest
