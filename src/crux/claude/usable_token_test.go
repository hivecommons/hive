package claude

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// writeCredFile stages a credentials file with the given token set.
func writeCredFile(t *testing.T, tokens *OAuthTokens) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	data, err := json.Marshal(Credentials{ClaudeAIOAuth: tokens})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

func ms(d time.Duration) int64 { return time.Now().Add(d).UnixMilli() }

// TestHasUsableToken_ExpiredButRefreshable pins the incident this function was
// added for (hivecommons/hive, 2026-09-01): a six-agent fleet went to
// "needs re-authentication" overnight because every hive predicate read the
// aged-out access token as "logged out", while the refresh grant beside it was
// four weeks from expiring and a single CLI restart recovered the fleet.
func TestHasUsableToken_ExpiredButRefreshable(t *testing.T) {
	path := writeCredFile(t, &OAuthTokens{
		AccessToken:           "expired-access",
		ExpiresAt:             ms(-2 * time.Hour),
		RefreshToken:          "live-refresh",
		RefreshTokenExpiresAt: ms(28 * 24 * time.Hour),
	})
	if HasValidToken(path) {
		t.Fatal("HasValidToken must stay false for an expired access token — callers put its result in an Authorization header")
	}
	if !HasUsableToken(path) {
		t.Fatal("HasUsableToken must be true: the refresh grant is live, so a CLI restart recovers this with no operator login")
	}
}

func TestHasUsableToken_LiveAccessToken(t *testing.T) {
	path := writeCredFile(t, &OAuthTokens{
		AccessToken: "live-access",
		ExpiresAt:   ms(4 * time.Hour),
	})
	if !HasUsableToken(path) {
		t.Fatal("a live access token is usable even with no refresh grant recorded")
	}
}

// TestHasUsableToken_NoRefreshGrantStaysUnusable is the half that must NOT
// change: with nothing on disk that can mint a token, the loud operator alert
// is the correct outcome and this function must not suppress it.
func TestHasUsableToken_NoRefreshGrantStaysUnusable(t *testing.T) {
	path := writeCredFile(t, &OAuthTokens{
		AccessToken: "expired-access",
		ExpiresAt:   ms(-time.Minute),
	})
	if HasUsableToken(path) {
		t.Fatal("an expired access token with no refresh grant is spent; only a human can fix it")
	}
}

func TestHasUsableToken_ExpiredRefreshGrantIsUnusable(t *testing.T) {
	path := writeCredFile(t, &OAuthTokens{
		AccessToken:           "expired-access",
		ExpiresAt:             ms(-48 * time.Hour),
		RefreshToken:          "stale-refresh",
		RefreshTokenExpiresAt: ms(-time.Hour),
	})
	if HasUsableToken(path) {
		t.Fatal("a refresh grant past its own expiry cannot mint anything")
	}
}

// TestHasUsableToken_MissingExpiryIsNotExpiry guards the direction the whole
// change fails safely in: an absent stamp means "not recorded", and inventing
// an expiry from it would declare a working credential dead.
func TestHasUsableToken_MissingExpiryIsNotExpiry(t *testing.T) {
	path := writeCredFile(t, &OAuthTokens{
		AccessToken:  "expired-access",
		ExpiresAt:    ms(-time.Hour),
		RefreshToken: "refresh-with-no-recorded-expiry",
	})
	if !HasUsableToken(path) {
		t.Fatal("a refresh grant with no recorded expiry must be treated as live, not as expired")
	}
}

// TestHasUsableToken_PositiveEvidenceOnly: anything short of a parseable
// credential leaves the caller exactly where it was.
func TestHasUsableToken_PositiveEvidenceOnly(t *testing.T) {
	dir := t.TempDir()
	missing := filepath.Join(dir, "absent.json")
	if HasUsableToken(missing) {
		t.Fatal("an absent file is not evidence of anything")
	}
	malformed := filepath.Join(dir, "malformed.json")
	if err := os.WriteFile(malformed, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if HasUsableToken(malformed) {
		t.Fatal("a malformed file is not evidence of anything")
	}
	empty := writeCredFile(t, nil)
	if HasUsableToken(empty) {
		t.Fatal("a credentials file with no token block is not evidence of anything")
	}
}

// TestRefreshTokenExpiresAtRoundTrips: WriteCredentials must not silently drop
// the only field that distinguishes a refreshable credential from a spent one.
func TestRefreshTokenExpiresAtRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	want := int64(1790668886454)
	if err := WriteCredentials(&OAuthTokens{
		AccessToken:           "a",
		ExpiresAt:             ms(time.Hour),
		RefreshToken:          "r",
		RefreshTokenExpiresAt: want,
	}, path); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := LoadTokens(path)
	if got == nil || got.RefreshTokenExpiresAt != want {
		t.Fatalf("refreshTokenExpiresAt lost on round trip: got %+v", got)
	}
}
