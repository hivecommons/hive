package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/keystore"
)

const persistTestKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func keystoreHiveYAML(token string) string {
	return `
project:
  org: my-org
  repos:
    - repo-a
github:
  token: ` + token + `
agents:
  supervisor:
    backend: copilot
    enabled: true
`
}

// pointSaveAt redirects Save's PVC layers into dir for the test.
func pointSaveAt(t *testing.T, dir string) string {
	t.Helper()
	runtimePath := filepath.Join(dir, "hive.yaml.runtime")
	origOverlay, origRuntime := DashboardOverlayFile, RuntimeConfigFile
	DashboardOverlayFile = filepath.Join(dir, "hive.yaml.dashboard")
	RuntimeConfigFile = runtimePath
	t.Cleanup(func() { DashboardOverlayFile, RuntimeConfigFile = origOverlay, origRuntime })
	return runtimePath
}

// hivecommons/hive#18214: with a keystore provider active, Save writes
// github.token as an envelope (never the PAT) and Load opens it again; an
// envelope this process cannot open is a load error that names the cure.
func TestSaveSealsGitHubTokenAndLoadOpensIt(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "hive.yaml")
	runtimePath := pointSaveAt(t, dir)
	const pat = "ghp_examplepersonalaccesstoken1234567890"
	if err := os.WriteFile(cfgPath, []byte(keystoreHiveYAML(pat)), 0o600); err != nil {
		t.Fatal(err)
	}

	keystore.ResetForTest()
	t.Cleanup(keystore.ResetForTest)
	prov, err := keystore.NewEnvKeyProvider([]byte(persistTestKey))
	if err != nil {
		t.Fatal(err)
	}
	keystore.SetActive(prov)

	cfg, err := LoadWithOverrides(cfgPath, "-")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := cfg.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if cfg.GitHub.Token != pat {
		t.Fatalf("Save mutated the in-memory token to %q", cfg.GitHub.Token)
	}
	for _, p := range []string{cfgPath, runtimePath} {
		raw, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), pat) {
			t.Fatalf("%s holds the PAT in plaintext", p)
		}
		if !strings.Contains(string(raw), "token: "+keystore.Prefix) {
			t.Fatalf("%s does not carry github.token as an envelope:\n%s", p, raw)
		}
	}

	loaded, err := LoadWithOverrides(runtimePath, "-")
	if err != nil {
		t.Fatalf("Load sealed: %v", err)
	}
	if loaded.GitHub.Token != pat {
		t.Fatalf("loaded token = %q, want the PAT", loaded.GitHub.Token)
	}

	// A hive without the key cannot open it, and is told what to do.
	keystore.SetActive(nil)
	if _, err := LoadWithOverrides(runtimePath, "-"); err == nil || !strings.Contains(err.Error(), keystore.EnvSecretsKeyPrevious) {
		t.Fatalf("load without provider = %v, want an error naming %s", err, keystore.EnvSecretsKeyPrevious)
	}
	other, _ := keystore.NewEnvKeyProvider([]byte(strings.Repeat("z", 64)))
	keystore.SetActive(other)
	if _, err := LoadWithOverrides(runtimePath, "-"); err == nil {
		t.Fatal("load under a different key generation must fail, not yield an empty token")
	}
}

func TestLegacyPlaintextTokenLoadsWithOrWithoutProvider(t *testing.T) {
	dir := t.TempDir()
	pointSaveAt(t, dir)
	cfgPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(cfgPath, []byte(keystoreHiveYAML("ghp_legacyplaintext")), 0o600); err != nil {
		t.Fatal(err)
	}
	keystore.ResetForTest()
	t.Cleanup(keystore.ResetForTest)
	keystore.SetActive(nil)
	cfg, err := LoadWithOverrides(cfgPath, "-")
	if err != nil || cfg.GitHub.Token != "ghp_legacyplaintext" {
		t.Fatalf("legacy load = %q, %v", cfg.GitHub.Token, err)
	}
	// Legacy mode persists the value as it was.
	if persisted := cfg.redactedForPersist(); persisted.GitHub.Token != "ghp_legacyplaintext" {
		t.Fatalf("legacy persist changed the token to %q", persisted.GitHub.Token)
	}
	prov, _ := keystore.NewEnvKeyProvider([]byte(persistTestKey))
	keystore.SetActive(prov)
	cfg, err = LoadWithOverrides(cfgPath, "-")
	if err != nil || cfg.GitHub.Token != "ghp_legacyplaintext" {
		t.Fatalf("legacy load with provider = %q, %v", cfg.GitHub.Token, err)
	}
	if persisted := cfg.redactedForPersist(); !keystore.IsEnvelopeString(persisted.GitHub.Token) {
		t.Fatalf("persist with provider left the token in plaintext: %q", persisted.GitHub.Token)
	}
}
