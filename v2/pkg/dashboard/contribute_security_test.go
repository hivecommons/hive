package dashboard

import (
	"context"
	"testing"
	"time"
)

// TestMintVerifyInviteTokenRoundTrip ensures a freshly minted token passes
// verification, returning the original inviter.
func TestMintVerifyInviteTokenRoundTrip(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecret()

	inviter := "alice-test"
	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)

	token := mintInviteToken(inviter, now)
	if token == "" {
		t.Fatal("mintInviteToken returned empty token")
	}

	got := verifyInviteToken(token, now)
	if got != inviter {
		t.Errorf("verifyInviteToken = %q, want %q", got, inviter)
	}
}

// TestVerifyInviteTokenExpired ensures a token issued in the past is rejected
// after the TTL has elapsed.
func TestVerifyInviteTokenExpired(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecret()

	inviter := "bob"
	mintTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	token := mintInviteToken(inviter, mintTime)

	// Verify well after the 14-day TTL
	verifyTime := mintTime.Add(15 * 24 * time.Hour)
	got := verifyInviteToken(token, verifyTime)
	if got != "" {
		t.Errorf("expected empty (expired), got %q", got)
	}
}

// TestVerifyInviteTokenTampered ensures a modified token is rejected.
func TestVerifyInviteTokenTampered(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecret()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	token := mintInviteToken("carol", now)

	// Flip a character in the signature portion
	tampered := token[:len(token)-2] + "XX"
	got := verifyInviteToken(tampered, now)
	if got != "" {
		t.Errorf("expected empty for tampered token, got %q", got)
	}
}

// TestVerifyInviteTokenMalformed checks various malformed inputs.
func TestVerifyInviteTokenMalformed(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecret()

	now := time.Now()
	cases := []string{
		"",
		"onlyone",
		"two.parts",
		"four.parts.here.extra",
		"....",
		" ",
	}
	for _, tc := range cases {
		got := verifyInviteToken(tc, now)
		if got != "" {
			t.Errorf("verifyInviteToken(%q) = %q, want empty", tc, got)
		}
	}
}

// TestVerifyInviteTokenReservedUsername ensures tokens minted for reserved
// usernames are rejected on verify (defense in depth).
func TestVerifyInviteTokenReservedUsername(t *testing.T) {
	t.Setenv("HIVE_HUB_SECRET", "test-secret-for-unit-tests")
	resetInviteSecret()

	now := time.Date(2026, 6, 15, 12, 0, 0, 0, time.UTC)
	// "admin" is in reservedUsernames
	token := mintInviteToken("admin", now)
	got := verifyInviteToken(token, now)
	if got != "" {
		t.Errorf("expected empty for reserved username, got %q", got)
	}
}

// TestIsPrivateURL checks the SSRF guard against various URLs.
func TestIsPrivateURL(t *testing.T) {
	// Mock DNS resolver to avoid real network calls
	origResolver := privateURLResolver
	t.Cleanup(func() { privateURLResolver = origResolver })
	privateURLResolver = func(_ context.Context, host string) ([]string, error) {
		// Simulate public DNS for non-blocked hosts
		switch host {
		case "example.com":
			return []string{"93.184.216.34"}, nil
		case "evil.com":
			return []string{"10.0.0.1"}, nil // resolves to private
		default:
			return []string{"1.2.3.4"}, nil
		}
	}

	ctx := context.Background()
	cases := []struct {
		url  string
		want bool
	}{
		// Blocked by prefix matching
		{"http://localhost/path", true},
		{"https://127.0.0.1:8080/foo", true},
		{"http://10.0.0.5/internal", true},
		{"https://172.16.0.1/admin", true},
		{"http://192.168.1.1/", true},
		{"wss://169.254.169.254/metadata", true},
		{"http://[::1]/path", true},
		{"http://0.0.0.0/", true},
		{"http://0.1.2.3/path", true},

		// Public IPs pass prefix check, DNS resolves public
		{"https://example.com/page", false},
		{"http://safe.org/api", false},

		// DNS resolves to private IP (SSRF rebind)
		{"https://evil.com/steal", true},

		// No scheme still works
		{"localhost:8080", true},
		{"10.0.0.1", true},
		{"example.com/path", false},
	}

	for _, tc := range cases {
		got := isPrivateURL(ctx, tc.url)
		if got != tc.want {
			t.Errorf("isPrivateURL(%q) = %v, want %v", tc.url, got, tc.want)
		}
	}
}

// TestIsPrivateURLDNSFailure checks that DNS failure is treated as private
// (fail-closed behavior).
func TestIsPrivateURLDNSFailure(t *testing.T) {
	origResolver := privateURLResolver
	t.Cleanup(func() { privateURLResolver = origResolver })
	privateURLResolver = func(_ context.Context, _ string) ([]string, error) {
		return nil, context.DeadlineExceeded
	}

	ctx := context.Background()
	// unknown.host doesn't match prefix blocklist, but DNS fails → private
	if !isPrivateURL(ctx, "https://unknown.host/path") {
		t.Error("expected isPrivateURL to return true on DNS failure (fail-closed)")
	}
}

// TestPruneQueueHoldReasons checks filtering logic.
func TestPruneQueueHoldReasons(t *testing.T) {
	src := map[string]string{
		"budget": "over limit",
		"manual": "user requested",
		"stale":  "old reason",
		"empty":  "  ",
	}

	keep := []string{"budget", "manual", "empty", "missing"}
	got := pruneQueueHoldReasons(src, keep)

	if _, ok := got["stale"]; ok {
		t.Error("expected 'stale' to be pruned")
	}
	if _, ok := got["empty"]; ok {
		t.Error("expected 'empty' (whitespace-only) to be pruned")
	}
	if got["budget"] != "over limit" {
		t.Errorf("budget = %q, want %q", got["budget"], "over limit")
	}
	if got["manual"] != "user requested" {
		t.Errorf("manual = %q, want %q", got["manual"], "user requested")
	}
	if len(got) != 2 {
		t.Errorf("len(got) = %d, want 2", len(got))
	}
}

// TestIsValidUsername covers edge cases of the username validator.
func TestIsValidUsername(t *testing.T) {
	cases := []struct {
		input string
		want  bool
	}{
		{"alice", true},
		{"Bob-123", true},
		{"user_name.dot", true},
		{"", false},                                              // empty
		{"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", false},    // >39 chars
		{"admin", false},                                         // reserved
		{"ROOT", false},                                          // reserved case-insensitive
		{"user name", false},                                     // space
		{"user@name", false},                                     // special char
		{"valid-user", true},
		{"a", true},                                              // single char
		{"123", true},                                            // all digits
		{"null", false},                                          // reserved
	}
	for _, tc := range cases {
		got := isValidUsername(tc.input)
		if got != tc.want {
			t.Errorf("isValidUsername(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

// resetInviteSecret resets the sync.Once so each test can set its own secret.
func resetInviteSecret() {
	inviteSecretOnce = sync.Once{}
	inviteSecretCache = nil
}
