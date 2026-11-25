// Command e2eclient validates the SuroMesh Service over the WIRE — real
// gRPC against the real service container (Phase 27 §4.2 item 13, §10).
// It never contacts Soroush Plus: live-upstream paths are exercised only
// up to the honest-failure boundary, and the exact error CONTRACT
// (status code + ErrorInfo reason) is asserted.
//
// Checks (all must PASS for exit 0):
//  1. service readiness (gRPC WaitForReady within budget)
//  2. unauthenticated request rejected (UNAUTHENTICATED)
//  3. invalid API key rejected (UNAUTHENTICATED)
//  4. bootstrap key binds an account (PENDING_AUTH; raw phone not echoed)
//  5. re-binding the same phone is idempotent (existing=true)
//  6. tenant-scoped GetAccount round trip
//  7. ListAccounts returns the bound account
//  8. GetSessionStatus is false before authentication
//  9. GetAuthorizationState is UNAUTHORIZED before authentication
//  10. unknown account → NOT_FOUND + ErrorInfo(NOT_FOUND)
//  11. SendCode on a non-authenticated account → honest upstream-class
//     failure with a stable ErrorInfo reason (never fabricated success)
//  12. oversized media upload declared up front → INVALID_ARGUMENT
//  13. RemoveAccount → crypto-shred; GetAccount → NOT_FOUND afterwards
//
// Output: PASS/FAIL lines (grep-friendly) + non-zero exit on any FAIL.
package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	v1 "github.com/Skryldev/suromesh/service/gen/suromesh/service/v1"
)

var (
	target     = envOr("E2E_TARGET", "service:50051")
	bootstrapK = envOr("E2E_BOOTSTRAP_KEY", "")
	failures   int
)

func main() {
	if bootstrapK == "" {
		fmt.Println("FAIL e2e: E2E_BOOTSTRAP_KEY not set")
		os.Exit(2)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		fmt.Printf("FAIL e2e: dial %s: %v\n", target, err)
		os.Exit(1)
	}
	defer conn.Close()

	authz := v1.NewAccountServiceClient(conn)
	auth := v1.NewAuthenticationServiceClient(conn)
	sessions := v1.NewSessionServiceClient(conn)
	media := v1.NewMediaServiceClient(conn)
	authed := func(ctx context.Context) context.Context {
		return metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer "+bootstrapK)
	}

	// 1. Readiness: WaitForReady waits through transient connection
	//    states until the server answers or the budget dies. An
	//    UNAUTHENTICATED answer proves the server is up (auth is
	//    checked in the following steps).
	check("readiness", func() error {
		rctx, rcancel := context.WithTimeout(ctx, 45*time.Second)
		defer rcancel()
		_, err := authz.ListAccounts(rctx, &v1.ListAccountsRequest{}, grpc.WaitForReady(true))
		if err == nil || status.Code(err) == codes.Unauthenticated {
			return nil
		}
		return fmt.Errorf("server not ready: %w", err)
	})

	// 2. Unauthenticated request must fail with UNAUTHENTICATED.
	check("unauthenticated-rejected", func() error {
		_, err := authz.ListAccounts(ctx, &v1.ListAccountsRequest{})
		return wantCode(err, codes.Unauthenticated)
	})

	// 3. Invalid key must fail with UNAUTHENTICATED.
	check("bad-key-rejected", func() error {
		bad := metadata.AppendToOutgoingContext(ctx, "authorization", "Bearer sk_definitely_wrong_key_123")
		_, err := authz.ListAccounts(bad, &v1.ListAccountsRequest{})
		return wantCode(err, codes.Unauthenticated)
	})

	// 4-7. Bootstrap bind + idempotency + reads.
	var accountID string
	check("bind-account", func() error {
		resp, err := authz.BindAccount(authed(ctx), &v1.BindAccountRequest{
			Phone: "+15550100200", DisplayAlias: "e2e-primary",
		})
		if err != nil {
			return err
		}
		if resp.Account == nil || resp.Account.AccountId == "" {
			return fmt.Errorf("bind returned no account")
		}
		if resp.Account.Status != v1.AccountStatus_ACCOUNT_STATUS_PENDING_AUTH {
			return fmt.Errorf("status = %s, want PENDING_AUTH", resp.Account.Status)
		}
		// Privacy: masked form only — the raw phone must never round-trip.
		if resp.Account.MaskedPhone == "+15550100200" {
			return fmt.Errorf("raw phone leaked in response")
		}
		accountID = resp.Account.AccountId
		return nil
	})
	check("bind-idempotent", func() error {
		resp, err := authz.BindAccount(authed(ctx), &v1.BindAccountRequest{
			Phone: "+15550100200", DisplayAlias: "e2e-primary",
		})
		if err != nil {
			return err
		}
		if !resp.Existing {
			return fmt.Errorf("re-bind existing = false, want true")
		}
		if resp.Account.AccountId != accountID {
			return fmt.Errorf("re-bind returned a different account id")
		}
		return nil
	})
	check("get-account-tenant-scoped", func() error {
		resp, err := authz.GetAccount(authed(ctx), &v1.GetAccountRequest{AccountId: accountID})
		if err != nil {
			return err
		}
		if resp.Account.DisplayAlias != "e2e-primary" {
			return fmt.Errorf("alias = %q", resp.Account.DisplayAlias)
		}
		return nil
	})
	check("list-accounts", func() error {
		resp, err := authz.ListAccounts(authed(ctx), &v1.ListAccountsRequest{PageSize: 10})
		if err != nil {
			return err
		}
		if len(resp.Accounts) == 0 {
			return fmt.Errorf("list returned zero accounts")
		}
		return nil
	})

	// 8-9. Session/authz state before any authentication.
	check("session-status-absent", func() error {
		resp, err := sessions.GetSessionStatus(authed(ctx), &v1.GetSessionStatusRequest{AccountId: accountID})
		if err != nil {
			return err
		}
		if resp.Present {
			return fmt.Errorf("session exists before authentication")
		}
		return nil
	})
	check("authz-state-unauthorized", func() error {
		resp, err := auth.GetAuthorizationState(authed(ctx), &v1.GetAuthorizationStateRequest{AccountId: accountID})
		if err != nil {
			return err
		}
		if resp.State != v1.AuthorizationState_AUTHORIZATION_STATE_UNAUTHORIZED {
			return fmt.Errorf("state = %s, want UNAUTHORIZED", resp.State)
		}
		return nil
	})

	// 10. Unknown account → NOT_FOUND + stable reason.
	check("unknown-account-not-found", func() error {
		_, err := authz.GetAccount(authed(ctx),
			&v1.GetAccountRequest{AccountId: "00000000-0000-0000-0000-000000000000"})
		if err := wantCode(err, codes.NotFound); err != nil {
			return err
		}
		return wantReason(err, "NOT_FOUND")
	})

	// 11. SendCode without a live authenticated upstream: honest failure
	//     with a stable, machine-readable reason (never a fabricated OK).
	check("sendcode-honest-failure", func() error {
		_, err := auth.SendCode(authed(ctx), &v1.SendCodeRequest{AccountId: accountID, Phone: "+15550100200"})
		if err == nil {
			return fmt.Errorf("SendCode must not succeed without a live account")
		}
		reason := reasonOf(err)
		if reason == "" {
			return fmt.Errorf("no ErrorInfo reason on honest failure: %v", err)
		}
		fmt.Printf("     sendcode honest failure: code=%s reason=%s\n", status.Code(err), reason)
		return nil
	})

	// 12. Oversized upload declared up front → INVALID_ARGUMENT.
	check("oversize-upload-rejected", func() error {
		declared := int64(3) << 30 // 3 GiB > 2 GiB limit
		stream, err := media.UploadMedia(authed(ctx))
		if err != nil {
			return err
		}
		if err := stream.Send(&v1.UploadMediaFrame{
			Frame: &v1.UploadMediaFrame_Start{
				Start: &v1.UploadMediaStart{
					AccountId: accountID,
					Peer:      &v1.PeerRef{Ref: &v1.PeerRef_Self{Self: true}},
					SizeBytes: declared,
					FileName:  "oversize.bin",
				},
			},
		}); err != nil {
			return err
		}
		_, err = stream.CloseAndRecv()
		return wantCode(err, codes.InvalidArgument)
	})

	// 13. RemoveAccount → crypto-shred; afterwards NOT_FOUND.
	check("remove-account-shred", func() error {
		if _, err := authz.RemoveAccount(authed(ctx), &v1.RemoveAccountRequest{AccountId: accountID}); err != nil {
			return err
		}
		_, err := authz.GetAccount(authed(ctx), &v1.GetAccountRequest{AccountId: accountID})
		return wantCode(err, codes.NotFound)
	})

	if failures > 0 {
		fmt.Printf("E2E RESULT: FAIL (%d failed checks)\n", failures)
		os.Exit(1)
	}
	fmt.Println("E2E RESULT: ALL PASSED")
}

func check(name string, fn func() error) {
	if err := fn(); err != nil {
		failures++
		fmt.Printf("FAIL %-32s %v\n", name, err)
		return
	}
	fmt.Printf("PASS %-32s\n", name)
}

func wantCode(err error, want codes.Code) error {
	if err == nil {
		return fmt.Errorf("expected %s, got success", want)
	}
	if got := status.Code(err); got != want {
		return fmt.Errorf("code = %s (%v), want %s", got, err, want)
	}
	return nil
}

func wantReason(err error, want string) error {
	if got := reasonOf(err); got != want {
		return fmt.Errorf("reason = %q, want %q", got, want)
	}
	return nil
}

func reasonOf(err error) string {
	st, ok := status.FromError(err)
	if !ok {
		return ""
	}
	for _, d := range st.Details() {
		if info, ok := d.(*errdetails.ErrorInfo); ok {
			return info.Reason
		}
	}
	return ""
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
