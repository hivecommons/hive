package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// buildAgentMinter is the opt-in credential issuer for per-agent mint tokens.
// It was previously 0% covered even though it sits on the auth path: a config
// mistake here silently disables (or mis-scopes) every minted agent credential.
// These tests pin the fail-closed validation, the key bootstrap, and the
// end-to-end mint round-trip.

func testMintLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestBuildAgentMinterRequiresKeyPath(t *testing.T) {
	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	cfg.Mint.Issuer = "https://mint.example.test"

	am, err := buildAgentMinter(cfg, testMintLogger())
	if err == nil {
		t.Fatal("expected error for empty mint.key_path, got nil")
	}
	if !strings.Contains(err.Error(), "mint.key_path is required") {
		t.Errorf("error = %q, want mention of mint.key_path", err)
	}
	if am != nil {
		t.Errorf("expected nil AgentMinter on validation failure, got %v", am)
	}
}

func TestBuildAgentMinterRequiresIssuer(t *testing.T) {
	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	cfg.Mint.KeyPath = filepath.Join(t.TempDir(), "mint.pem")

	am, err := buildAgentMinter(cfg, testMintLogger())
	if err == nil {
		t.Fatal("expected error for empty mint.issuer, got nil")
	}
	if !strings.Contains(err.Error(), "mint.issuer is required") {
		t.Errorf("error = %q, want mention of mint.issuer", err)
	}
	if am != nil {
		t.Errorf("expected nil AgentMinter on validation failure, got %v", am)
	}
	// Validation must fail BEFORE touching the filesystem: no key file may be
	// created for a config that is rejected.
	if _, statErr := os.Stat(cfg.Mint.KeyPath); !os.IsNotExist(statErr) {
		t.Errorf("key file %s should not exist after issuer validation failure", cfg.Mint.KeyPath)
	}
}

func TestBuildAgentMinterKeyLoadFailure(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "mint.pem")
	// A present-but-garbage key must surface a load error, not silently mint
	// with a fresh key (which would invalidate every previously issued token).
	if err := os.WriteFile(keyPath, []byte("not a pem key"), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	cfg.Mint.KeyPath = keyPath
	cfg.Mint.Issuer = "https://mint.example.test"

	am, err := buildAgentMinter(cfg, testMintLogger())
	if err == nil {
		t.Fatal("expected error for corrupt signing key, got nil")
	}
	if !strings.Contains(err.Error(), "loading mint signing key") {
		t.Errorf("error = %q, want 'loading mint signing key' wrap", err)
	}
	if am != nil {
		t.Errorf("expected nil AgentMinter on key load failure, got %v", am)
	}
}

func TestBuildAgentMinterCreatesKeyAndMints(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "mint.pem")

	cfg := &config.Config{}
	cfg.HiveID = "test-hive"
	cfg.Mint.Enabled = true
	cfg.Mint.KeyPath = keyPath
	cfg.Mint.Issuer = "https://mint.example.test"
	cfg.Mint.MaxTTLSeconds = 300

	am, err := buildAgentMinter(cfg, testMintLogger())
	if err != nil {
		t.Fatalf("buildAgentMinter: %v", err)
	}
	if am == nil || !am.Enabled() {
		t.Fatal("expected an enabled AgentMinter")
	}

	// The bootstrap must create the signing key with owner-only perms.
	info, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("signing key not created at %s: %v", keyPath, err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("signing key perms = %o, want 600", perm)
	}

	tok, err := am.MintAgentToken("quality", "trusted")
	if err != nil {
		t.Fatalf("MintAgentToken: %v", err)
	}
	if tok == "" {
		t.Fatal("expected a non-empty token from an enabled minter")
	}
	// Fail closed on an unattributable token request.
	if _, err := am.MintAgentToken("", "trusted"); err == nil {
		t.Error("expected error minting for empty agent name")
	}
}

func TestBuildAgentMinterReusesExistingKey(t *testing.T) {
	dir := t.TempDir()
	keyPath := filepath.Join(dir, "mint.pem")

	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	cfg.Mint.KeyPath = keyPath
	cfg.Mint.Issuer = "https://mint.example.test"

	if _, err := buildAgentMinter(cfg, testMintLogger()); err != nil {
		t.Fatalf("first buildAgentMinter: %v", err)
	}
	first, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}

	// A second build against the same path must REUSE the persisted key —
	// regenerating it would orphan every outstanding minted credential.
	if _, err := buildAgentMinter(cfg, testMintLogger()); err != nil {
		t.Fatalf("second buildAgentMinter: %v", err)
	}
	second, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Error("signing key was regenerated on second build; expected reuse of the persisted key")
	}
}

func TestBuildAgentMinterZeroTTLUsesDefault(t *testing.T) {
	dir := t.TempDir()

	cfg := &config.Config{}
	cfg.Mint.Enabled = true
	cfg.Mint.KeyPath = filepath.Join(dir, "mint.pem")
	cfg.Mint.Issuer = "https://mint.example.test"
	cfg.Mint.MaxTTLSeconds = 0 // package default must apply, not a zero TTL

	am, err := buildAgentMinter(cfg, testMintLogger())
	if err != nil {
		t.Fatalf("buildAgentMinter: %v", err)
	}
	tok, err := am.MintAgentToken("quality", "trusted")
	if err != nil {
		t.Fatalf("MintAgentToken with zero MaxTTLSeconds: %v", err)
	}
	if tok == "" {
		t.Fatal("expected a usable token when MaxTTLSeconds is 0")
	}
}
