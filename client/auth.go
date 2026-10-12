package client

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/Skryldev/suromesh/rpc"
	"github.com/Skryldev/suromesh/session"
	"github.com/Skryldev/suromesh/tl"
	"github.com/Skryldev/suromesh/tl/gen"
)

// Authorization state is a distinct axis from the transport lifecycle
// (StateNew…StateClosed). Connected ≠ logged in: the lifecycle says the
// MTProto session works; the auth state says which account — if any — the
// session belongs to. The model below follows the reference:
//
//	reference/SPlusthon/splusthon/client/auth.py     (send/sign/password flows)
//	reference/SPlusthon/splusthon/client/users.py:214 (_authorized probing)
//	reference/SPlusthon/splusthon/client/auth.py:448 (_on_login)
//
// States:
//
//	Unauthorized — no pending flow, session not logged in
//	CodePending  — SendCode succeeded; phone_code_hash held in memory
//	PasswordRequired — SignIn answered SESSION_PASSWORD_NEEDED
//	Authorized   — the session's account login is valid (persisted)
//	AuthorizationRevoked — the server revoked this authorization
//	(SESSION_REVOKED / USER_DEACTIVATED / AUTH_KEY_DUPLICATED)
type AuthState int32

const (
	AuthUnauthorized AuthState = iota
	AuthCodePending
	AuthPasswordRequired
	AuthAuthorized
	AuthAuthorizationRevoked
)

func (s AuthState) String() string {
	switch s {
	case AuthUnauthorized:
		return "unauthorized"
	case AuthCodePending:
		return "code_pending"
	case AuthPasswordRequired:
		return "password_required"
	case AuthAuthorized:
		return "authorized"
	case AuthAuthorizationRevoked:
		return "authorization_revoked"
	}
	return "unknown"
}

// authFlow is the client-local (transient) part of the state — the
// reference keeps exactly this in memory: _phone, _phone_code_hash and
// the SESSION_PASSWORD_NEEDED branch of _start. It is never persisted.
type authFlow int32

const (
	authFlowNone authFlow = iota
	authFlowCode
	authFlowPassword
)

// Authorization is the domain-level login result (auth.Authorization).
// Everything originates from the server's response; nothing is fabricated.
type Authorization struct {
	User *User
}

// CodeSent mirrors auth.SentCode (schema: auth.sendCode = auth.SentCode).
// PhoneCodeHash is sensitive authentication state: the public API exposes
// it because the reference does (sign_in accepts phone_code_hash), but it
// never appears in errors, hooks, metrics, or logs.
type CodeSent struct {
	PhoneCodeHash string
	Type          string // "app", "sms", "call", "flash_call", "missed_call", "fragment_sms", "unknown"
	NextType      string // "" when the server did not specify one
	TimeoutSecs   int32  // 0 when the server did not specify one
}

// Authentication error surface. Sentinels are matched with errors.Is;
// the wrapping authRPCError keeps the raw *rpc.RPCError in the chain
// (errors.As) for callers that need code/type, and its Error() renders
// only the sentinel text — no phone numbers, codes, or hashes.
var (
	// ErrPhoneInvalid: ParsePhone rejected the input (reference raises
	// ValueError for non-digit residue, utils.py:916-923).
	ErrPhoneInvalid = errors.New("auth: invalid phone number")
	// ErrPhoneNumberInvalid maps RPC PHONE_NUMBER_INVALID.
	ErrPhoneNumberInvalid = errors.New("auth: phone number invalid (PHONE_NUMBER_INVALID)")
	// ErrPhoneCodeHashMissing: sign-in without a hash and none stored
	// (auth.py:280-281).
	ErrPhoneCodeHashMissing = errors.New("auth: missing phone_code_hash (call SendCode first)")
	// ErrAPIConfigMissing: SendCode requires APIID/APIHash (auth.py:508-509
	// uses the client's api_id/api_hash).
	ErrAPIConfigMissing = errors.New("auth: APIID/APIHash not configured")
	// ErrPasswordRequired maps RPC SESSION_PASSWORD_NEEDED; state moves to
	// AuthPasswordRequired (auth.py:205 / _start two_step_detected).
	ErrPasswordRequired = errors.New("auth: two-step verification password required")
	// ErrPhoneUnoccupied maps the auth.AuthorizationSignUpRequired response
	// (auth.py:428-431 raises PhoneNumberUnoccupiedError).
	ErrPhoneUnoccupied = errors.New("auth: phone number not registered (sign-up required)")
	// ErrPasswordInvalid maps RPC PASSWORD_HASH_INVALID (retryable).
	ErrPasswordInvalid = errors.New("auth: invalid password")
	// ErrCodeInvalid maps RPC PHONE_CODE_INVALID (retryable).
	ErrCodeInvalid = errors.New("auth: invalid confirmation code")
	// ErrCodeExpired maps RPC PHONE_CODE_EXPIRED; the stored hash is
	// dropped (auth.py:390-392) and SendCode must be called again.
	ErrCodeExpired = errors.New("auth: confirmation code expired")
	// ErrPhoneBanned maps RPC PHONE_NUMBER_BANNED.
	ErrPhoneBanned = errors.New("auth: phone number banned")
	// ErrSendCodeSuccess maps the auth.SentCodeSuccess response
	// (auth.py:517-518 raises RuntimeError).
	ErrSendCodeSuccess = errors.New("auth: logged in right after sending the code")
	// ErrAuthRestart maps RPC AUTH_RESTART (auth.py:510-514 retries).
	ErrAuthRestart = errors.New("auth: server requested restart (AUTH_RESTART)")
	// ErrAuthorizationRevoked maps the permanent 401 set: SESSION_REVOKED,
	// USER_DEACTIVATED, AUTH_KEY_DUPLICATED.
	ErrAuthorizationRevoked = errors.New("auth: authorization revoked")
	// ErrUnauthorized maps the recoverable 401 set: AUTH_KEY_UNREGISTERED,
	// AUTH_KEY_INVALID (the key exists but no account is bound to it).
	ErrUnauthorized = errors.New("auth: not authorized")
)

// authRPCError binds a sentinel to its originating RPC failure. errors.Is
// matches the sentinel; errors.As reaches the *rpc.RPCError.
type authRPCError struct {
	sentinel error
	rpc      *rpc.RPCError
}

func (e *authRPCError) Error() string { return e.sentinel.Error() }
func (e *authRPCError) Unwrap() error { return e.rpc }
func (e *authRPCError) Is(target error) bool {
	return target == e.sentinel
}

// permanent401 marks authorizations the server terminated.
var permanent401 = map[string]bool{
	"SESSION_REVOKED":     true,
	"USER_DEACTIVATED":    true,
	"AUTH_KEY_DUPLICATED": true,
}

// mapAuthError classifies an RPC failure from the auth flows into the
// sentinel surface above. Non-RPC errors pass through untouched.
func mapAuthError(err error) error {
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) {
		return err
	}
	sentinel := authSentinelFor(rpcErr.Type)
	if sentinel == nil {
		return err
	}
	return &authRPCError{sentinel: sentinel, rpc: rpcErr}
}

// authSentinelFor maps an RPC error type string to the auth sentinel
// (nil = no specific mapping). Fuzz seam: must never panic.
func authSentinelFor(rpcType string) error {
	switch rpcType {
	case "SESSION_PASSWORD_NEEDED":
		return ErrPasswordRequired
	case "PHONE_CODE_INVALID":
		return ErrCodeInvalid
	case "PHONE_CODE_EXPIRED":
		return ErrCodeExpired
	case "PHONE_NUMBER_INVALID":
		return ErrPhoneNumberInvalid
	case "PHONE_NUMBER_BANNED":
		return ErrPhoneBanned
	case "PASSWORD_HASH_INVALID":
		return ErrPasswordInvalid
	case "AUTH_RESTART":
		return ErrAuthRestart
	case "SESSION_REVOKED", "USER_DEACTIVATED", "AUTH_KEY_DUPLICATED":
		return ErrAuthorizationRevoked
	case "AUTH_KEY_UNREGISTERED", "AUTH_KEY_INVALID":
		return ErrUnauthorized
	}
	return nil
}

// ParsePhone mirrors utils.parse_phone (utils.py:916-923): strip '+', '(',
// ')', '-' and whitespace; the remainder must be non-empty and all digits.
// Python's str.isdigit accepts unicode digit forms; Go accepts ASCII
// digits only — documented, irrelevant for E.164 phone numbers.
func ParsePhone(phone string) (string, error) {
	var b strings.Builder
	for _, r := range phone {
		switch {
		case r == '+' || r == '(' || r == ')' || r == '-' ||
			r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f':
			// stripped
		case r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			return "", fmt.Errorf("%w: non-digit residue", ErrPhoneInvalid)
		}
	}
	if b.Len() == 0 {
		return "", fmt.Errorf("%w: empty", ErrPhoneInvalid)
	}
	return b.String(), nil
}

// AuthState reports the current authorization state. It derives from two
// sources, each the single owner of its truth:
//
//   - the durable session flag (session.AuthState == Authorized), and
//   - the client-local transient flow (code pending / password required),
//     which the reference also keeps only in memory (_phone_code_hash).
//
// Priority: Revoked > PasswordRequired > Authorized > CodePending >
// Unauthorized. An authorized session stays Authorized even if a re-login
// SendCode is issued (the reference likewise keeps _authorized true).
func (c *Client) AuthState() AuthState {
	if c.authRevoked.Load() {
		return AuthAuthorizationRevoked
	}
	if authFlow(c.authFlow.Load()) == authFlowPassword {
		return AuthPasswordRequired
	}
	if c.sess.AuthState() == session.StateAuthorized {
		return AuthAuthorized
	}
	if authFlow(c.authFlow.Load()) == authFlowCode {
		return AuthCodePending
	}
	return AuthUnauthorized
}

// SendCode implements the reference send_code_request (auth.py:472-551):
//
//	parse phone → stored hash? resendCode : sendCode(phone, api_id,
//	api_hash, CodeSettings{}) → AUTH_RESTART retries (≤3 attempts) →
//	SentCodeSuccess → error → store non-empty hash → return SentCode.
//
// When a hash already exists for the phone the reference resends via
// auth.resendCode (auth.py:527-549); PHONE_CODE_EXPIRED there drops the
// hash and re-sends fresh (auth.py:536-544).
func (c *Client) SendCode(ctx context.Context, phone string) (*CodeSent, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	if c.cfg.APIID == 0 || c.cfg.APIHash == "" {
		return nil, ErrAPIConfigMissing
	}
	norm, err := ParsePhone(phone)
	if err != nil {
		return nil, err
	}
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	c.authMu.Lock()
	c.authPhone = norm // reference self._phone (auth.py:530)
	c.authMu.Unlock()

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if opCtx.Err() != nil {
			return nil, fmt.Errorf("auth: sendCode cancelled: %w", opCtx.Err())
		}
		c.authMu.Lock()
		existing := c.authHashes[norm]
		c.authMu.Unlock()

		if existing == "" {
			sent, err := c.sendCodeOnce(opCtx, eng, norm)
			if err != nil {
				if errors.Is(err, ErrAuthRestart) {
					lastErr = err
					continue // reference _retry_count+1 (auth.py:513-514)
				}
				c.authMetrics.failure.Add(1)
				return nil, err
			}
			// Store only if non-empty and not already present
			// (auth.py:524-526: "phone_code_hash may be empty").
			if sent.PhoneCodeHash != "" {
				c.authMu.Lock()
				if _, ok := c.authHashes[norm]; !ok {
					c.authHashes[norm] = sent.PhoneCodeHash
				}
				c.authMu.Unlock()
			}
			c.setFlow(authFlowCode)
			c.authMetrics.sendCode.Add(1)
			return sent, nil
		}

		// Resend path (auth.py:527-549).
		res, err := c.invokeAuth(opCtx, eng, &gen.AuthResendCodeRequest{
			PhoneNumber:   norm,
			PhoneCodeHash: existing,
		})
		if err != nil {
			if errors.Is(err, ErrCodeExpired) {
				// auth.py:536-544: drop the hash, request a new code.
				c.authMu.Lock()
				delete(c.authHashes, norm)
				c.authMu.Unlock()
				lastErr = err
				continue
			}
			c.authMetrics.failure.Add(1)
			return nil, err
		}
		sent, ok := res.(*gen.AuthSentCode)
		if !ok {
			if _, isSuccess := res.(*gen.AuthSentCodeSuccess); isSuccess {
				c.authMetrics.failure.Add(1)
				return nil, ErrSendCodeSuccess
			}
			c.authMetrics.failure.Add(1)
			return nil, fmt.Errorf("auth: resendCode answered %T", res)
		}
		c.authMu.Lock()
		c.authHashes[norm] = sent.PhoneCodeHash
		c.authMu.Unlock()
		c.setFlow(authFlowCode)
		c.authMetrics.sendCode.Add(1)
		return sentCodeFrom(sent), nil
	}
	c.authMetrics.failure.Add(1)
	return nil, lastErr
}

func (c *Client) sendCodeOnce(ctx context.Context, eng *rpc.Engine, phone string) (*CodeSent, error) {
	res, err := c.invokeAuth(ctx, eng, &gen.AuthSendCodeRequest{
		PhoneNumber: phone,
		APIID:       c.cfg.APIID,
		APIHash:     c.cfg.APIHash,
		Settings:    &gen.CodeSettings{},
	})
	if err != nil {
		return nil, err
	}
	sent, ok := res.(*gen.AuthSentCode)
	if !ok {
		if _, isSuccess := res.(*gen.AuthSentCodeSuccess); isSuccess {
			return nil, ErrSendCodeSuccess // auth.py:517-518
		}
		return nil, fmt.Errorf("auth: sendCode answered %T", res)
	}
	return sentCodeFrom(sent), nil
}

// SignIn implements the phone+code branch of reference sign_in
// (auth.py:342-433): parse phone, take the stored phone_code_hash,
// invoke auth.signIn, handle the response/error surface, and on success
// run _on_login (session authorized + entity seeding).
func (c *Client) SignIn(ctx context.Context, phone, code string) (*Authorization, error) {
	return c.signIn(ctx, phone, code, "")
}

// SignInWithHash is SignIn with an explicit phone_code_hash (the
// reference's sign_in(phone_code_hash=...) parameter, auth.py:325-327).
func (c *Client) SignInWithHash(ctx context.Context, phone, code, phoneCodeHash string) (*Authorization, error) {
	return c.signIn(ctx, phone, code, phoneCodeHash)
}

func (c *Client) signIn(ctx context.Context, phone, code, phoneCodeHash string) (*Authorization, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	norm, err := ParsePhone(phone)
	if err != nil {
		return nil, err
	}
	// auth.py:269-283 (_parse_phone_and_hash): explicit hash wins, else
	// the stored one; neither → ValueError.
	if phoneCodeHash == "" {
		c.authMu.Lock()
		phoneCodeHash = c.authHashes[norm]
		c.authMu.Unlock()
		if phoneCodeHash == "" {
			return nil, ErrPhoneCodeHashMissing
		}
	}
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	c.authMetrics.signIn.Add(1)
	res, err := c.invokeAuth(opCtx, eng, &gen.AuthSignInRequest{
		PhoneNumber:   norm,
		PhoneCodeHash: phoneCodeHash,
		PhoneCode:     &code,
	})
	if err != nil {
		merr := err
		switch {
		case errors.Is(merr, ErrPasswordRequired):
			// auth.py:205 (_start): two-step verification engaged; the
			// reference re-enters sign_in with the password.
			c.setFlow(authFlowPassword)
			c.authMetrics.passwordRequired.Add(1)
			return nil, merr
		case errors.Is(merr, ErrCodeExpired):
			// auth.py:390-392: drop the stored hash; SendCode again. The
			// flow state follows the hash: none left for the current
			// phone → back to Unauthorized.
			c.authMu.Lock()
			delete(c.authHashes, norm)
			if c.authHashes[c.authPhone] == "" {
				c.authFlow.Store(int32(authFlowNone))
			}
			c.authMu.Unlock()
			c.authMetrics.failure.Add(1)
			return nil, merr
		default:
			c.authMetrics.failure.Add(1)
			return nil, merr
		}
	}
	switch r := res.(type) {
	case *gen.AuthAuthorization:
		return c.onAuthorization(r)
	case *gen.AuthAuthorizationSignUpRequired:
		// auth.py:428-431: emulate pre-layer-104 behaviour.
		return nil, ErrPhoneUnoccupied
	default:
		c.authMetrics.failure.Add(1)
		return nil, fmt.Errorf("auth: signIn answered %T", res)
	}
}

// CheckPassword implements the password branch of reference sign_in
// (auth.py:372-376): account.getPassword → password.compute_check →
// auth.checkPassword. The password exists only inside computeCheck's
// frame and is never retained, logged, or included in any error.
func (c *Client) CheckPassword(ctx context.Context, password string) (*Authorization, error) {
	eng, err := c.engine()
	if err != nil {
		return nil, err
	}
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	c.authMetrics.signIn.Add(1)
	res, err := c.invokeAuth(opCtx, eng, &gen.AccountGetPasswordRequest{})
	if err != nil {
		c.authMetrics.failure.Add(1)
		return nil, err
	}
	pwd, ok := res.(*gen.AccountPassword)
	if !ok {
		return nil, fmt.Errorf("auth: getPassword answered %T", res)
	}
	srp, err := computeCheck(pwd, password, srpPBKDF2Iterations)
	if err != nil {
		// ErrSRPParams / ErrSRPAlgo: parameter text only, no password.
		c.authMetrics.failure.Add(1)
		return nil, err
	}
	authRes, err := c.invokeAuth(opCtx, eng, &gen.AuthCheckPasswordRequest{Password: srp})
	if err != nil {
		c.authMetrics.failure.Add(1)
		return nil, err // PASSWORD_HASH_INVALID → ErrPasswordInvalid; state unchanged (retryable)
	}
	authz, ok := authRes.(*gen.AuthAuthorization)
	if !ok {
		c.authMetrics.failure.Add(1)
		return nil, fmt.Errorf("auth: checkPassword answered %T", authRes)
	}
	return c.onAuthorization(authz)
}

// LogOut implements reference log_out (auth.py:593-620): auth.logOut →
// on RPCError the state is untouched (reference returns False) → on
// success clear the self user, drop the durable authorization, clear the
// transient flow, and persist the reset session.
//
// Documented deviation: the reference disconnects and nulls the session
// ("the client is unusable after logging out"); the Go client stays
// usable as an object — a fresh SendCode/SignIn can log in again — but no
// authorization state survives: AuthState() == AuthUnauthorized is
// guaranteed and the persisted session carries no key.
func (c *Client) LogOut(ctx context.Context) error {
	eng, err := c.engine()
	if err != nil {
		return err
	}
	opCtx, cancelOp := mergeContext(ctx, c.lifecycleCtx)
	defer cancelOp()

	if _, err := c.invokeAuth(opCtx, eng, &gen.AuthLogOutRequest{}); err != nil {
		c.authMetrics.failure.Add(1)
		return err // auth.py:610-612: RPCError → no local mutation
	}

	c.ClearEntities() // superset of _mb_entity_cache.set_self_user(None) (auth.py:614)
	c.authMu.Lock()
	c.authHashes = make(map[string]string)
	c.authPhone = ""
	c.authMu.Unlock()
	c.authFlow.Store(int32(authFlowNone))
	c.authRevoked.Store(false)
	if err := c.sess.ClearAuthKey(); err != nil {
		return fmt.Errorf("auth: logout session reset: %w", err)
	}
	if c.cfg.Saver != nil {
		if err := c.cfg.Saver.Save(); err != nil {
			return fmt.Errorf("auth: logout session save: %w", err)
		}
	}
	// Reference auth.py:617-619: disconnect and drop the session. The Go
	// client tears the transport down (Disconnected); auth operations fail
	// with ErrNotReady until a fresh Connect (which re-exchanges the key
	// for the now-keyless session).
	c.logoutTeardown()
	c.authMetrics.logout.Add(1)
	return nil
}

// onAuthorization is _on_login (auth.py:448-470): seed the self user into
// the entity cache, mark the session authorized, persist, clear the
// transient flow. The reference additionally bootstraps update state
// (GetState/GetDifference); in SuroMesh that is the update manager's own
// responsibility (Phase 6), not the auth flow's — documented deviation.
func (c *Client) onAuthorization(a *gen.AuthAuthorization) (*Authorization, error) {
	out := &Authorization{}
	if u, ok := a.User.(*gen.User); ok {
		c.entities.seedUser(u)
		if ent, found := c.entities.get(PeerID{PeerUser, u.ID}); found {
			if eu, ok := ent.(*User); ok {
				out.User = eu
			}
		}
		if out.User == nil {
			out.User = &User{ID: u.ID}
		}
	}
	if err := c.sess.SetAuthorized(); err != nil {
		return nil, fmt.Errorf("auth: session: %w", err)
	}
	if c.cfg.Saver != nil {
		if err := c.cfg.Saver.Save(); err != nil {
			return nil, fmt.Errorf("auth: session save: %w", err)
		}
	}
	c.authFlow.Store(int32(authFlowNone))
	c.authRevoked.Store(false)
	c.authMetrics.success.Add(1)
	return out, nil
}

// setFlow moves the transient flow state. Forward movement only; a
// concurrent success never resurrects a stale pending state.
func (c *Client) setFlow(f authFlow) {
	c.authFlow.Store(int32(f))
}

// handleAuthReject applies 401-class state effects: the session's
// durable authorized flag is dropped, and the permanent set additionally
// marks the client revoked (until a future successful login).
func (c *Client) handleAuthReject(err error) {
	var rpcErr *rpc.RPCError
	if !errors.As(err, &rpcErr) || rpcErr.Code != 401 {
		return
	}
	c.sess.SetUnauthorized()
	if permanent401[rpcErr.Type] {
		c.authRevoked.Store(true)
		c.authFlow.Store(int32(authFlowNone))
	}
}

// invokeAuth wraps an auth RPC with the error mapping and 401 state
// effects shared by every flow.
func (c *Client) invokeAuth(ctx context.Context, eng *rpc.Engine, req tl.Request) (tl.Object, error) {
	res, err := eng.Invoke(ctx, req)
	if err != nil {
		var rpcErr *rpc.RPCError
		if errors.As(err, &rpcErr) && rpcErr.IsFloodWait() {
			c.authMetrics.floodWait.Add(1)
		}
		merr := mapAuthError(err)
		c.handleAuthReject(merr)
		return nil, merr
	}
	return res, nil
}

// sentCodeTypeString renders auth.SentCodeType*/auth.CodeType* variants.
// Unknown constructors render as "unknown" — never a panic (fuzz seam).
func sentCodeTypeString(v any) string {
	switch v.(type) {
	case *gen.AuthSentCodeTypeApp:
		return "app"
	case *gen.AuthSentCodeTypeSms:
		return "sms"
	case *gen.AuthSentCodeTypeCall:
		return "call"
	case *gen.AuthSentCodeTypeFlashCall:
		return "flash_call"
	case *gen.AuthSentCodeTypeMissedCall:
		return "missed_call"
	case *gen.AuthSentCodeTypeFragmentSms:
		return "fragment_sms"
	case *gen.AuthSentCodeTypeSetUpEmailRequired:
		return "email_required"
	default:
		return "unknown"
	}
}

func sentCodeNextTypeString(v any) string {
	switch v.(type) {
	case *gen.AuthCodeTypeSms:
		return "sms"
	case *gen.AuthCodeTypeCall:
		return "call"
	case *gen.AuthCodeTypeFlashCall:
		return "flash_call"
	case *gen.AuthCodeTypeMissedCall:
		return "missed_call"
	case *gen.AuthCodeTypeFragmentSms:
		return "fragment_sms"
	default:
		return "unknown"
	}
}

// sentCodeFrom converts the TL result. nil NextType renders as "".
func sentCodeFrom(s *gen.AuthSentCode) *CodeSent {
	out := &CodeSent{
		PhoneCodeHash: s.PhoneCodeHash,
		Type:          sentCodeTypeString(s.Type_),
	}
	if s.NextType != nil {
		out.NextType = sentCodeNextTypeString(s.NextType)
	}
	if s.Timeout != nil {
		out.TimeoutSecs = *s.Timeout
	}
	return out
}

// authMetrics holds the aggregate authorization counters. No high-
// cardinality labels and no identifying values — ever.
type authMetrics struct {
	sendCode         atomic.Uint64
	signIn           atomic.Uint64
	success          atomic.Uint64
	failure          atomic.Uint64
	passwordRequired atomic.Uint64
	logout           atomic.Uint64
	floodWait        atomic.Uint64
}
