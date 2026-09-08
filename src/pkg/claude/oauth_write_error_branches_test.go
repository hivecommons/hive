package claude

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	if !strings.Contains(err.Error(), "write temp credentials") {
		t.Fatalf("error = %q, want it to mention write temp credentials", err)
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
		t.Fatalf("WriteCredentials over non-empty dir: err = nil, want error")
	}
	if !strings.Contains(err.Error(), "rename credentials") {
		t.Fatalf("error = %q, want it to mention rename credentials", err)
	}
}
