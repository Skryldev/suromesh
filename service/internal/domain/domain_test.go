package domain_test

// Layer D (§4.1): domain tests — state transitions, error taxonomy,
// peer-id parsing, boundary values. Pure, no I/O.

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/Skryldev/suromesh/service/internal/domain"
)

func TestPeerID_ParseRoundTrip(t *testing.T) {
	for _, s := range []string{"user:1", "chat:999999", "channel:42"} {
		id, err := domain.ParsePeerID(s)
		if err != nil {
			t.Fatalf("%s: %v", s, err)
		}
		if id.Canonical() != s {
			t.Fatalf("round trip: %s → %s", s, id.Canonical())
		}
	}
}

func TestPeerID_InvalidInputs(t *testing.T) {
	bad := []string{
		"", "user", "user:", "user:0", "user:-5", "user:abc",
		"unknown:1", "user:1:2", "USER:1", "channel:999999999999999999999",
	}
	for _, s := range bad {
		if _, err := domain.ParsePeerID(s); !errors.Is(err, domain.ErrInvalidInput) {
			t.Errorf("ParsePeerID(%q) err = %v, want ErrInvalidInput", s, err)
		}
	}
}

func TestPeerRef_StringForms(t *testing.T) {
	if got := domain.RefUsername("dupe").String(); got != "@dupe" {
		t.Fatalf("username ref = %q", got)
	}
	if got := domain.RefSelf().String(); got != "self" {
		t.Fatalf("self ref = %q", got)
	}
	if got := domain.RefPeerID(domain.PeerID{Kind: domain.PeerKindChannel, ID: 7}).String(); got != "channel:7" {
		t.Fatalf("id ref = %q", got)
	}
}

func TestErrorClassification_FullTaxonomy(t *testing.T) {
	cases := []struct {
		err  error
		want domain.Class
	}{
		{nil, domain.ClassNone},
		{context.Canceled, domain.ClassCanceled},
		{context.DeadlineExceeded, domain.ClassTimeout},
		{fmt.Errorf("wrap: %w", context.Canceled), domain.ClassCanceled},
		{fmt.Errorf("wrap: %w", context.DeadlineExceeded), domain.ClassTimeout},
		{domain.ErrInvalidInput, domain.ClassInvalidInput},
		{fmt.Errorf("x: %w", domain.ErrNotFound), domain.ClassNotFound},
		{domain.ErrAlreadyExists, domain.ClassAlreadyExists},
		{domain.ErrAccountStopped, domain.ClassAccountStopped},
		{domain.ErrAccountNotConnected, domain.ClassNotConnected},
		{domain.ErrAmbiguousMutation, domain.ClassAmbiguous},
		{domain.ErrSessionRevoked, domain.ClassSessionRevoked},
		{domain.ErrFileReferenceExpired, domain.ClassFileRefExpired},
		{domain.ErrUnsupported, domain.ClassUnsupported},
		{domain.ErrDisabled, domain.ClassUnsupported},
		{domain.ErrCrypto, domain.ClassCrypto},
		{domain.ErrPersistence, domain.ClassPersistence},
		{domain.ErrTooManyRequests, domain.ClassTooManyRequests},
		{domain.ErrUpstreamTimeout, domain.ClassUpstreamTimeout},
		{domain.ErrUpstreamProtocol, domain.ClassUpstreamProtocol},
		{domain.ErrUpstreamUnavailable, domain.ClassUpstreamUnavail},
		{domain.ErrClientClosed, domain.ClassAccountStopped},
		{&domain.FloodWaitError{Seconds: 5}, domain.ClassFloodWait},
		{errors.New("something unknown"), domain.ClassInternal},
	}
	for _, tc := range cases {
		if got := domain.Classify(tc.err); got != tc.want {
			t.Errorf("Classify(%v) = %q, want %q", tc.err, got, tc.want)
		}
	}
}

func TestErrorClassification_SentinelsAreDistinct(t *testing.T) {
	sentinels := []error{
		domain.ErrInvalidInput, domain.ErrNotFound, domain.ErrAlreadyExists,
		domain.ErrAccountStopped, domain.ErrAccountNotConnected,
		domain.ErrUpstreamUnavailable, domain.ErrUpstreamTimeout,
		domain.ErrUpstreamProtocol, domain.ErrAmbiguousMutation,
		domain.ErrSessionRevoked, domain.ErrFloodWait, domain.ErrFileReferenceExpired,
		domain.ErrUnsupported, domain.ErrDisabled, domain.ErrPersistence,
		domain.ErrCrypto, domain.ErrTooManyRequests, domain.ErrClientClosed,
	}
	for i, a := range sentinels {
		for j := i + 1; j < len(sentinels); j++ {
			if errors.Is(a, sentinels[j]) || errors.Is(sentinels[j], a) {
				t.Fatalf("sentinels alias: %v ↔ %v", a, sentinels[j])
			}
		}
	}
}

func TestFloodWait_UnwrapAndSeconds(t *testing.T) {
	err := &domain.FloodWaitError{Seconds: 42}
	if !errors.Is(err, domain.ErrFloodWait) {
		t.Fatal("FloodWaitError must unwrap to ErrFloodWait")
	}
	if s, ok := domain.WaitSeconds(err); !ok || s != 42 {
		t.Fatalf("WaitSeconds = %d %v", s, ok)
	}
	if _, ok := domain.WaitSeconds(errors.New("other")); ok {
		t.Fatal("non-flood error must not yield seconds")
	}
}

func TestFormatID_DecimalStrings(t *testing.T) {
	cases := map[int64]string{0: "0", 1: "1", -5: "-5",
		9007199254740993: "9007199254740993"} // 2^53+1: beyond JS safe range
	for id, want := range cases {
		if got := domain.FormatID(id); got != want {
			t.Errorf("FormatID(%d) = %q, want %q", id, got, want)
		}
	}
}

func TestStateStrings_Stable(t *testing.T) {
	// Contract strings consumed by clients (do not change silently).
	if got := domain.ObservedReady.String(); got != "READY" {
		t.Fatalf("ObservedReady = %q", got)
	}
	if got := domain.DesiredRunning.String(); got != "RUNNING" {
		t.Fatalf("DesiredRunning = %q", got)
	}
	if got := domain.AuthRevoked.String(); got != "REVOKED" {
		t.Fatalf("AuthRevoked = %q", got)
	}
	if got := domain.AccountStatusPendingAuth.String(); got != "PENDING_AUTH" {
		t.Fatalf("PendingAuth = %q", got)
	}
}
