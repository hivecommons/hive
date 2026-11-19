package claude

import (
	"encoding/json"
	"errors"
	"io/fs"
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

// TestReadTokens_KeepsTheReadError: the credential bridge must tell "this
// process may not open another uid's 0600 file" apart from "no token here";
// LoadTokens flattens both to nil, ReadTokens does not.
func TestReadTokens_KeepsTheReadError(t *testing.T) {
	if _, err := ReadTokens(filepath.Join(t.TempDir(), "missing")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("missing file: err = %v, want ErrNotExist", err)
	}
	bad := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(bad, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := ReadTokens(bad); err != nil || tok != nil {
		t.Errorf("malformed: (%v, %v), want nil, nil (readable, nothing usable)", tok, err)
	}
	if tok := LoadTokens(bad); tok != nil {
		t.Errorf("LoadTokens malformed = %v, want nil", tok)
	}
	empty := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(empty, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if tok, err := ReadTokens(empty); err != nil || tok != nil {
		t.Errorf("no token block: (%v, %v), want nil, nil", tok, err)
	}
	if os.Getuid() != 0 {
		locked := writeCredFile(t, &OAuthTokens{AccessToken: "a", ExpiresAt: ms(time.Hour)})
		if err := os.Chmod(locked, 0); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadTokens(locked); !errors.Is(err, fs.ErrPermission) {
			t.Errorf("unreadable: err = %v, want ErrPermission", err)
		}
	}
}

func TestParseTokens(t *testing.T) {
	if tok := ParseTokens([]byte("nope")); tok != nil {
		t.Errorf("malformed = %v", tok)
	}
	if tok := ParseTokens([]byte(`{}`)); tok != nil {
		t.Errorf("no block = %v", tok)
	}
	tok := ParseTokens([]byte(`{"claudeAiOauth":{"accessToken":"a","refreshToken":"r","expiresAt":42}}`))
	if tok == nil || tok.AccessToken != "a" || tok.RefreshToken != "r" || tok.ExpiresAt != 42 {
		t.Errorf("parsed = %+v", tok)
	}
}

func TestTokensUsable_AtAnInstant(t *testing.T) {
	now := time.Now()
	later := now.Add(2 * time.Hour)
	tok := &OAuthTokens{AccessToken: "a", ExpiresAt: now.Add(time.Hour).UnixMilli()}
	if !TokensUsable(tok, now) {
		t.Error("live access token at now should be usable")
	}
	if TokensUsable(tok, later) {
		t.Error("the same token judged after its expiry should be unusable")
	}
	if TokensUsable(nil, now) {
		t.Error("nil is never usable")
	}
}
