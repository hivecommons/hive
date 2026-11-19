package claude

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/sharedfile"
)

// tokenEndpoint must fall back to the real Claude TokenURL when no test
// override is installed — the production branch every other test bypasses.
func TestTokenEndpoint_DefaultsToTokenURL(t *testing.T) {
	prev := tokenEndpointOverride
	tokenEndpointOverride = ""
	t.Cleanup(func() { tokenEndpointOverride = prev })

	if got := tokenEndpoint(); got != TokenURL {
		t.Fatalf("tokenEndpoint() = %q, want %q", got, TokenURL)
	}
}

func TestWriteCredentials_TempWriteError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("read-only dir is not read-only for root")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatalf("chmod: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })

	// MkdirAll succeeds on the existing dir, so the failure surfaces at the
	// temp-file write, not at directory creation.
	err := WriteCredentials(&OAuthTokens{AccessToken: "at"}, filepath.Join(dir, "creds.json"))
	if err == nil {
		t.Fatalf("WriteCredentials into read-only dir: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "write credentials") {
		t.Fatalf("error = %q, want it to mention write credentials", err)
	}
	if left, _ := filepath.Glob(filepath.Join(dir, "creds.json.*")); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

func TestWriteCredentials_RenameError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "creds.json")
	// A non-empty directory at the destination lets the temp write succeed but
	// makes the final rename fail.
	if err := os.MkdirAll(filepath.Join(path, "occupied"), 0o755); err != nil {
		t.Fatalf("mkdir blocker: %v", err)
	}

	err := WriteCredentials(&OAuthTokens{AccessToken: "at"}, path)
	if err == nil {
		t.Fatalf("WriteCredentials over a directory: err = nil, want error")
	}
	if !errors.Is(err, sharedfile.ErrUnsafeLocation) {
		t.Fatalf("error = %q, want sharedfile.ErrUnsafeLocation", err)
	}
	if left, _ := filepath.Glob(path + ".*"); len(left) != 0 {
		t.Fatalf("temporary files left behind: %v", left)
	}
}

// A symlink at the credentials path is refused, and the file it points at is
// left exactly as it was.
func TestWriteCredentials_SymlinkAtPathIsRefused(t *testing.T) {
	dir := t.TempDir()
	victim := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(victim, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".credentials.json")
	if err := os.Symlink(victim, path); err != nil {
		t.Fatal(err)
	}
	err := WriteCredentials(&OAuthTokens{AccessToken: "at"}, path)
	if !errors.Is(err, sharedfile.ErrUnsafeLocation) {
		t.Fatalf("error = %v, want sharedfile.ErrUnsafeLocation", err)
	}
	if got, _ := os.ReadFile(victim); string(got) != "keep" {
		t.Fatalf("victim rewritten: %q", got)
	}
	if info, _ := os.Lstat(victim); info.Mode().Perm() != 0o600 {
		t.Fatalf("victim mode changed: %v", info.Mode())
	}
}

// TestWriteCredentialsRefusesSignedOutTokens: no hive writer may put a
// credential with no access token over the shared login.
func TestWriteCredentialsRefusesSignedOutTokens(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := WriteCredentials(&OAuthTokens{AccessToken: "live", ExpiresAt: time.Now().Add(time.Hour).UnixMilli()}, path); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(path)
	for _, tok := range []*OAuthTokens{nil, {}, {RefreshToken: "r", ExpiresAt: 1}} {
		if err := WriteCredentials(tok, path); !errors.Is(err, ErrNoAccessToken) {
			t.Errorf("WriteCredentials(%+v) = %v, want ErrNoAccessToken", tok, err)
		}
	}
	if after, _ := os.ReadFile(path); string(after) != string(before) {
		t.Error("the shared login was replaced by a signed-out credential")
	}
}
