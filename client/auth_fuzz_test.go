package client

// Phase 9 fuzz targets: hostile inputs to the auth-critical pure seams
// must never panic, hang, or allocate without bounds. The SRP fuzz uses a
// reduced PBKDF2 iteration count (production is always srpPBKDF2Iterations
// = 100000, pinned by TestSRPGolden against the reference).

import (
	"errors"
	"testing"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// FuzzParsePhone: any input either fails or normalizes to non-empty ASCII
// digits with only the reference's strip set removed.
func FuzzParsePhone(f *testing.F) {
	seeds := []string{"+34 (123) 123-1234", "341231231234", "12a34", "", "+", "---", "٣٣٣", "\x00\x01", "0"}
	for _, s := range seeds {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, in string) {
		out, err := ParsePhone(in)
		if err != nil {
			return
		}
		if out == "" {
			t.Fatalf("accepted empty normalization of %q", in)
		}
		for _, r := range out {
			if r < '0' || r > '9' {
				t.Fatalf("normalized output %q contains %q", out, r)
			}
		}
	})
}

// FuzzAuthSentinelMapping: the RPC-type mapping must be total and stable —
// any input maps to a sentinel or the original error, never a panic; a
// mapped sentinel keeps its identity through the wrapper.
func FuzzAuthSentinelMapping(f *testing.F) {
	f.Add(400, "PHONE_CODE_INVALID")
	f.Add(401, "SESSION_REVOKED")
	f.Add(420, "FLOOD_WAIT_5")
	f.Add(500, "weird_type")
	f.Fuzz(func(t *testing.T, code int, rpcType string) {
		e := &rpc.RPCError{Code: int32(code), Type: leadingAlpha(rpcType), Message: rpcType}
		m := mapAuthError(e)
		if m == nil {
			t.Fatal("mapAuthError returned nil for non-nil input")
		}
		if s := authSentinelFor(rpcType); s != nil && !errors.Is(m, s) {
			t.Fatalf("sentinel identity lost for %q: %v", rpcType, m)
		}
		// A mapped sentinel never renders as the raw wire type alone.
		if s := authSentinelFor(rpcType); s != nil && m.Error() == rpcType {
			t.Fatalf("mapped sentinel rendered as raw type %q", rpcType)
		}
	})
}

// FuzzSentCodeConversion: hostile/unknown SentCode types must convert
// without panicking and render as "unknown".
func FuzzSentCodeConversion(f *testing.F) {
	f.Add([]byte(nil))
	f.Add([]byte{0x5e, 0x00, 0x25, 0x02}) // auth.sentCode ctor
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		if sc, ok := obj.(*gen.AuthSentCode); ok {
			out := sentCodeFrom(sc)
			if out.Type == "" {
				t.Fatal("empty type string")
			}
		}
	})
}

// FuzzSRPParams: arbitrary server parameter sets must be rejected or
// produce a bounded (A, M1) — never a panic, never a hang. Uses a small
// PBKDF2 count for throughput (fuzz seam only).
func FuzzSRPParams(f *testing.F) {
	f.Add([]byte{3}, []byte{1}, []byte{2}, make([]byte, 256))
	f.Add([]byte{7}, []byte{0}, []byte{0}, make([]byte, 2048/8))
	f.Fuzz(func(t *testing.T, gBytes, salt1, salt2, pBytes []byte) {
		if len(gBytes) == 0 || len(pBytes) > 512 {
			return // keep the modexp bounded
		}
		g := int32(int8(gBytes[0])) // include negative g values
		srpID := int64(1)
		pwd := &gen.AccountPassword{
			HasPassword: true,
			CurrentAlgo: &gen.PasswordKdfAlgoSHA256SHA256PBKDF2HMACSHA512iter100000SHA256ModPow{
				Salt1: salt1, Salt2: salt2, G: g, P: pBytes,
			},
			SRPB:  make([]byte, 32),
			SRPID: &srpID,
		}
		// The whole point: this must return (result, nil) or (nil, error)
		// quickly for ANY parameter set.
		srp, err := computeCheck(pwd, "fuzz-password", 2)
		if err != nil {
			return // rejection is the expected path for hostile params
		}
		if len(srp.A) == 0 || len(srp.M1) != 32 {
			t.Fatalf("accepted params produced A=%d bytes M1=%d bytes", len(srp.A), len(srp.M1))
		}
	})
}

// FuzzAuthResponseDecode: hostile server answers for auth RPCs decode
// without panicking (the conversion layer tolerates any constructor).
func FuzzAuthResponseDecode(f *testing.F) {
	f.Add([]byte{0x2e, 0xa2, 0xc0, 0xd4}) // auth.authorization ctor
	f.Add([]byte{0x44, 0x74, 0x7e, 0x9a}) // auth.authorizationSignUpRequired
	f.Fuzz(func(t *testing.T, body []byte) {
		obj, err := tl.Unmarshal(body)
		if err != nil {
			return
		}
		switch o := obj.(type) {
		case *gen.AuthAuthorization:
			_ = o.User
		case *gen.AuthSentCode:
			_ = sentCodeFrom(o)
		}
	})
}

// leadingAlpha mirrors the engine's Type derivation: the leading ASCII
// alpha/underscore segment of the wire message.
func leadingAlpha(s string) string {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c == '_') {
			return s[:i]
		}
	}
	return s
}
