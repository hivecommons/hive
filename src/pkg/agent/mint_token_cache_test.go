package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// These tests cover the per-agent mint-token cache plumbing end to end:
// AgentMintTokenCachePath (the cache location contract every consumer — the
// agent's WIF exchange and the refresher — relies on), writeAgentCredFile (the
// atomic owner-only write), and issueAgentMintToken (the fail-safe issuance
// wrapper). They were previously exercised only via the deadlock regression
// test, which used a DISABLED issuer and therefore never reached the mint or
// write statements.

// withAgentTokenCacheDir points the package-level cache dir seam at a writable
// temp dir for the duration of the test, restoring the production value
// afterwards (same pattern as withModeFileGlob).
func withAgentTokenCacheDir(t *testing.T, dir string) {
	t.Helper()
	orig := agentTokenCacheDir
	agentTokenCacheDir = dir
	t.Cleanup(func() { agentTokenCacheDir = orig })
}

func TestAgentMintTokenCachePath_PerAgentDistinctFiles(t *testing.T) {
	a := AgentMintTokenCachePath("scanner")
	b := AgentMintTokenCachePath("reviewer")
	if a == b {
		t.Fatalf("cache paths must be per-agent, got %q for both", a)
	}
	// The path contract consumers depend on: inside the cache dir, named for
	// the agent, distinct from the App-token cache (which has no mint- prefix).
	if !strings.HasPrefix(a, agentTokenCacheDir+"/") {
		t.Errorf("path %q not under cache dir %q", a, agentTokenCacheDir)
	}
	if !strings.Contains(a, "mint-token-scanner") {
		t.Errorf("path %q does not embed the agent name in the mint-token file", a)
	}
}

func TestWriteAgentCredFile_WritesOwnerOnlyAtomically(t *testing.T) {
	dir := t.TempDir()
	withAgentTokenCacheDir(t, filepath.Join(dir, "agent-tokens"))
	path := AgentMintTokenCachePath("scanner")

	// agentUID 0 skips the chown (the only step that needs root), so this is
	// the full happy path on any host.
	if err := writeAgentCredFile(path, "tok-abc", 0); err != nil {
		t.Fatalf("writeAgentCredFile: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading written cred: %v", err)
	}
	if string(data) != "tok-abc" {
		t.Errorf("cred content = %q, want %q", data, "tok-abc")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != agentTokenCachePerms {
		t.Errorf("cred file mode = %v, want %v (owner-only)", perm, os.FileMode(agentTokenCachePerms))
	}
	// Atomicity contract: no .tmp litter left beside the final file.
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Errorf("temp file left behind after successful rename: %v", err)
	}

	// Overwrite path: a second write must replace the content in place.
	if err := writeAgentCredFile(path, "tok-def", 0); err != nil {
		t.Fatalf("second writeAgentCredFile: %v", err)
	}
	data, _ = os.ReadFile(path)
	if string(data) != "tok-def" {
		t.Errorf("cred content after rewrite = %q, want %q", data, "tok-def")
	}
}

func TestWriteAgentCredFile_ChownFailureRemovesTempFile(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: chown to an arbitrary uid would succeed")
	}
	dir := t.TempDir()
	withAgentTokenCacheDir(t, filepath.Join(dir, "agent-tokens"))
	path := AgentMintTokenCachePath("scanner")

	// A non-root process cannot chown to another uid → EPERM. The failed write
	// must be cleaned up: no cred file, and no plaintext token left in a .tmp.
	err := writeAgentCredFile(path, "tok-secret", 4242)
	if err == nil {
		t.Fatal("expected chown error for non-root chown to foreign uid")
	}
	if !strings.Contains(err.Error(), "chown cred cache") {
		t.Errorf("error = %v, want chown cred cache wrap", err)
	}
	if _, statErr := os.Stat(path); !os.IsNotExist(statErr) {
		t.Errorf("cred file must not exist after failed chown: %v", statErr)
	}
	if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
		t.Errorf("temp file with plaintext token left behind after failed chown: %v", statErr)
	}
}

func TestWriteAgentCredFile_MkdirFailure(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permissions do not bind")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	withAgentTokenCacheDir(t, filepath.Join(parent, "agent-tokens"))

	err := writeAgentCredFile(AgentMintTokenCachePath("scanner"), "tok", 0)
	if err == nil {
		t.Fatal("expected mkdir error under read-only parent")
	}
	if !strings.Contains(err.Error(), "creating agent token dir") {
		t.Errorf("error = %v, want creating agent token dir wrap", err)
	}
}

// mintManager builds a bare manager suitable for issueAgentMintToken tests.
func mintManager(t *testing.T, issuer AgentMintIssuer) *Manager {
	t.Helper()
	t.Setenv("HIVE_WORK_DIR", t.TempDir())
	m := NewManager(map[string]config.AgentConfig{
		"scanner": makeAgentConfig("claude", "sonnet"),
	}, discardLogger(), ProjectContext{})
	m.SetAgentMint(issuer)
	return m
}

func TestIssueAgentMintToken_SuccessWritesCache(t *testing.T) {
	dir := t.TempDir()
	withAgentTokenCacheDir(t, filepath.Join(dir, "agent-tokens"))
	m := mintManager(t, &fakeMintIssuer{enabled: true, token: "mint-tok-1"})

	m.issueAgentMintToken("scanner", "contributor", 0)

	data, err := os.ReadFile(AgentMintTokenCachePath("scanner"))
	if err != nil {
		t.Fatalf("mint token cache not written: %v", err)
	}
	if string(data) != "mint-tok-1" {
		t.Errorf("cache content = %q, want %q", data, "mint-tok-1")
	}
}

func TestIssueAgentMintToken_FailSafePaths(t *testing.T) {
	dir := t.TempDir()
	withAgentTokenCacheDir(t, filepath.Join(dir, "agent-tokens"))
	cache := AgentMintTokenCachePath("scanner")

	cases := []struct {
		name   string
		issuer AgentMintIssuer
	}{
		{"nil issuer", nil},
		{"disabled issuer", &fakeMintIssuer{enabled: false, token: "never"}},
		{"mint error", &fakeMintIssuer{enabled: true, err: errors.New("mint down")}},
		{"empty token", &fakeMintIssuer{enabled: true, token: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := mintManager(t, tc.issuer)
			// Must never panic and must never write a cache file.
			m.issueAgentMintToken("scanner", "contributor", 0)
			if _, err := os.Stat(cache); !os.IsNotExist(err) {
				t.Errorf("cache file must not exist for %s: %v", tc.name, err)
			}
		})
	}
}

func TestIssueAgentMintToken_WriteFailureIsSwallowed(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("running as root: directory permissions do not bind")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
	withAgentTokenCacheDir(t, filepath.Join(parent, "agent-tokens"))
	m := mintManager(t, &fakeMintIssuer{enabled: true, token: "mint-tok"})

	// The write fails (unwritable cache dir); issuance is fail-safe so this
	// must return normally rather than panic or propagate.
	m.issueAgentMintToken("scanner", "contributor", 0)
}
