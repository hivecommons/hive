package claude

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// #18732: a revoked grant leaves a credentials file that every on-disk
// predicate accepts. The verifier is the one check that asks the provider.
func TestVerifierRejectsLiveTokenOn401(t *testing.T) {
	var sawToken string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawToken = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()

	path := writeCredFile(t, &OAuthTokens{
		AccessToken:           "live-but-revoked",
		RefreshToken:          "revoked-refresh",
		ExpiresAt:             ms(4 * time.Hour),
		RefreshTokenExpiresAt: ms(28 * 24 * time.Hour),
	})
	if !HasUsableToken(path) {
		t.Fatal("precondition: the file must look valid to the on-disk predicate")
	}

	verdict, detail := Verifier{BaseURL: srv.URL}.Verify(context.Background(), path)
	if verdict != CredentialRejected {
		t.Fatalf("verdict = %v, want rejected", verdict)
	}
	if detail != "claude usage HTTP 401" {
		t.Fatalf("detail = %q", detail)
	}
	if sawToken != "Bearer live-but-revoked" {
		t.Fatalf("the live access token must be what is verified; got %q", sawToken)
	}
	if strings.Contains(detail, "live-but-revoked") || strings.Contains(detail, "revoked-refresh") {
		t.Fatalf("detail leaks token material: %q", detail)
	}
}

func TestVerifierAcceptsOn200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	path := writeCredFile(t, &OAuthTokens{AccessToken: "live", ExpiresAt: ms(time.Hour)})
	if verdict, _ := (Verifier{BaseURL: srv.URL}).Verify(context.Background(), path); verdict != CredentialAccepted {
		t.Fatalf("verdict = %v, want accepted", verdict)
	}
}

// An aged-out access token must not be sent: the 401 it would earn says
// nothing about the refresh grant (#5165). 403/429/5xx and transport errors
// are equally inconclusive.
func TestVerifierInconclusiveCases(t *testing.T) {
	calls := 0
	status := http.StatusForbidden
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(status)
	}))
	defer srv.Close()

	expiredPath := writeCredFile(t, &OAuthTokens{
		AccessToken: "stale", ExpiresAt: ms(-time.Hour),
		RefreshToken: "live-refresh", RefreshTokenExpiresAt: ms(24 * time.Hour),
	})
	if verdict, _ := (Verifier{BaseURL: srv.URL}).Verify(context.Background(), expiredPath); verdict != CredentialInconclusive {
		t.Fatalf("expired access token: verdict = %v, want inconclusive", verdict)
	}
	if calls != 0 {
		t.Fatalf("an aged-out token must never be sent to the provider; %d call(s) made", calls)
	}

	livePath := writeCredFile(t, &OAuthTokens{AccessToken: "live", ExpiresAt: ms(time.Hour)})
	for _, code := range []int{http.StatusForbidden, http.StatusTooManyRequests, http.StatusInternalServerError} {
		status = code
		if verdict, _ := (Verifier{BaseURL: srv.URL}).Verify(context.Background(), livePath); verdict != CredentialInconclusive {
			t.Fatalf("HTTP %d: verdict = %v, want inconclusive", code, verdict)
		}
	}

	if verdict, detail := (Verifier{BaseURL: "http://127.0.0.1:1"}).Verify(context.Background(), livePath); verdict != CredentialInconclusive || detail != "provider unreachable" {
		t.Fatalf("unreachable: verdict = %v detail = %q", verdict, detail)
	}

	if verdict, _ := (Verifier{BaseURL: srv.URL}).Verify(context.Background(), "/nonexistent/.credentials.json"); verdict != CredentialInconclusive {
		t.Fatalf("missing file: verdict = %v, want inconclusive", verdict)
	}
}
