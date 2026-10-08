package github

import (
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

func TestValidateTokenCachedHitAvoidsSecondHTTPCall(t *testing.T) {
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	restoreCache := resetTokenIdentityCacheForTest(func() time.Time { return now }, time.Hour, 5*time.Minute)
	defer restoreCache()

	var calls int32
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		if got := restCallerFromContext(req.Context()); got != "hive:validate_token" {
			t.Fatalf("REST caller = %q, want hive:validate_token", got)
		}
		return jsonBodyResponse(http.StatusOK, GitHubUser{Login: "owner", AvatarURL: "https://example.test/a.png"}), nil
	}, func() {
		first, err := ValidateTokenCached("gho_owner", "")
		if err != nil {
			t.Fatalf("first validate: %v", err)
		}
		first.Login = "mutated"
		second, err := ValidateTokenCached("gho_owner", "")
		if err != nil {
			t.Fatalf("second validate: %v", err)
		}
		if second.Login != "owner" {
			t.Fatalf("cached login = %q, want owner", second.Login)
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("HTTP calls = %d, want 1", got)
		}
	})
}

func TestValidateTokenCachedTTLExpiryRefetches(t *testing.T) {
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	restoreCache := resetTokenIdentityCacheForTest(func() time.Time { return now }, time.Minute, 5*time.Minute)
	defer restoreCache()

	var calls int32
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		call := atomic.AddInt32(&calls, 1)
		return jsonBodyResponse(http.StatusOK, GitHubUser{Login: fmt.Sprintf("owner%d", call)}), nil
	}, func() {
		first, err := ValidateTokenCached("gho_expiring", "")
		if err != nil {
			t.Fatalf("first validate: %v", err)
		}
		now = now.Add(time.Minute + time.Nanosecond)
		second, err := ValidateTokenCached("gho_expiring", "")
		if err != nil {
			t.Fatalf("second validate: %v", err)
		}
		if first.Login == second.Login {
			t.Fatalf("login did not refetch after TTL expiry: %q", second.Login)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("HTTP calls = %d, want 2", got)
		}
	})
}

func TestValidateTokenCachedNegativeTTL(t *testing.T) {
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	restoreCache := resetTokenIdentityCacheForTest(func() time.Time { return now }, time.Hour, time.Minute)
	defer restoreCache()

	var calls int32
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		call := atomic.AddInt32(&calls, 1)
		if call == 1 {
			return jsonBodyResponse(http.StatusUnauthorized, map[string]string{"message": "bad credentials"}), nil
		}
		return jsonBodyResponse(http.StatusOK, GitHubUser{Login: "owner"}), nil
	}, func() {
		if _, err := ValidateTokenCached("gho_revoked", ""); err == nil {
			t.Fatal("first validate should fail")
		}
		if _, err := ValidateTokenCached("gho_revoked", ""); err == nil {
			t.Fatal("cached negative validate should fail")
		}
		if got := atomic.LoadInt32(&calls); got != 1 {
			t.Fatalf("HTTP calls before negative TTL expiry = %d, want 1", got)
		}
		now = now.Add(time.Minute + time.Nanosecond)
		user, err := ValidateTokenCached("gho_revoked", "")
		if err != nil {
			t.Fatalf("validate after negative TTL expiry: %v", err)
		}
		if user.Login != "owner" {
			t.Fatalf("login = %q, want owner", user.Login)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("HTTP calls after negative TTL expiry = %d, want 2", got)
		}
	})
}

func TestInvalidateTokenIdentity(t *testing.T) {
	now := time.Date(2026, 10, 8, 16, 0, 0, 0, time.UTC)
	restoreCache := resetTokenIdentityCacheForTest(func() time.Time { return now }, time.Hour, 5*time.Minute)
	defer restoreCache()

	var calls int32
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		call := atomic.AddInt32(&calls, 1)
		return jsonBodyResponse(http.StatusOK, GitHubUser{Login: fmt.Sprintf("owner%d", call)}), nil
	}, func() {
		first, err := ValidateTokenCached("gho_logout", "")
		if err != nil {
			t.Fatalf("first validate: %v", err)
		}
		InvalidateTokenIdentity("gho_logout")
		second, err := ValidateTokenCached("gho_logout", "")
		if err != nil {
			t.Fatalf("second validate: %v", err)
		}
		if first.Login == second.Login {
			t.Fatalf("login did not refetch after invalidate: %q", second.Login)
		}
		if got := atomic.LoadInt32(&calls); got != 2 {
			t.Fatalf("HTTP calls = %d, want 2", got)
		}
	})
}
