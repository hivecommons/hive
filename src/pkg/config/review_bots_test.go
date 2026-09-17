package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Defaults: absent block = off, max_attempts 1, resolve_after_fix true.
func TestReviewBotsConfig_Defaults(t *testing.T) {
	var rb ReviewBotsConfig
	if rb.Enabled() {
		t.Error("zero value must be disabled")
	}
	if rb.MaxAttempts() != DefaultReviewBotMaxAttempts || rb.MaxAttempts() != 1 {
		t.Errorf("MaxAttempts() = %d, want 1", rb.MaxAttempts())
	}
	if !rb.ResolveAfterFixEnabled() {
		t.Error("resolve_after_fix must default to true")
	}
	if rb.IsBot("Copilot") {
		t.Error("nothing is a bot when no logins are configured")
	}

	// Whitespace-only logins do not enable the feature.
	if (ReviewBotsConfig{Logins: []string{"", "  "}}).Enabled() {
		t.Error("blank logins must not enable the feature")
	}
	off := false
	rb = ReviewBotsConfig{Logins: []string{"Copilot"}, MaxAttemptsPerThread: 3, ResolveAfterFix: &off}
	if !rb.Enabled() || rb.MaxAttempts() != 3 || rb.ResolveAfterFixEnabled() {
		t.Errorf("explicit values not honoured: %+v", rb)
	}
	if (ReviewBotsConfig{Logins: []string{"x"}, MaxAttemptsPerThread: -2}).MaxAttempts() != 1 {
		t.Error("non-positive max_attempts_per_thread must fall back to 1")
	}
}

// IsBot matches case-insensitively and trims, exactly as GitHub renders logins.
func TestReviewBotsConfig_IsBot(t *testing.T) {
	rb := ReviewBotsConfig{Logins: []string{" chatgpt-codex-connector[bot] ", "Copilot"}}
	for _, login := range []string{"chatgpt-codex-connector[bot]", "COPILOT", " copilot "} {
		if !rb.IsBot(login) {
			t.Errorf("IsBot(%q) = false, want true", login)
		}
	}
	for _, login := range []string{"", "copilot-swe-agent[bot]", "alice", "chatgpt-codex-connector"} {
		if rb.IsBot(login) {
			t.Errorf("IsBot(%q) = true, want false", login)
		}
	}
}

// LoadProjectReviewBots reads only classification.review_bots out of a
// hive-project.yaml; the rest of the classification block is ignored, a
// missing file is the zero value, and a malformed file is an error.
func TestLoadProjectReviewBots(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive-project.yaml")
	yamlDoc := `project:
  org: acme
classification:
  complexity:
    simple:
      labels: ["auto-qa"]
  copilot_check:
    lookback_days: 1
  review_bots:
    logins:
      - "chatgpt-codex-connector[bot]"
      - "Copilot"
    max_attempts_per_thread: 2
    resolve_after_fix: false
`
	if err := os.WriteFile(path, []byte(yamlDoc), 0o644); err != nil {
		t.Fatal(err)
	}
	rb, err := LoadProjectReviewBots(path)
	if err != nil {
		t.Fatalf("LoadProjectReviewBots: %v", err)
	}
	if !rb.Enabled() || len(rb.Logins) != 2 || rb.MaxAttempts() != 2 || rb.ResolveAfterFixEnabled() {
		t.Errorf("unexpected block: %+v", rb)
	}

	rb, err = LoadProjectReviewBots(filepath.Join(dir, "missing.yaml"))
	if err != nil || rb.Enabled() {
		t.Errorf("missing file must be the zero value with no error; got %+v, %v", rb, err)
	}

	noKey := filepath.Join(dir, "nokey.yaml")
	if err := os.WriteFile(noKey, []byte("classification:\n  copilot_check: {lookback_days: 1}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rb, err = LoadProjectReviewBots(noKey)
	if err != nil || rb.Enabled() {
		t.Errorf("absent review_bots key must be off; got %+v, %v", rb, err)
	}

	bad := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(bad, []byte("classification: [not: a: map\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProjectReviewBots(bad); err == nil {
		t.Error("malformed project file must be an error, not a silent off")
	}
}

// EffectiveReviewBots: hive.yaml's block wins when it names a login; otherwise
// the project file is consulted.
func TestEffectiveReviewBots(t *testing.T) {
	dir := t.TempDir()
	project := filepath.Join(dir, "hive-project.yaml")
	if err := os.WriteFile(project, []byte("classification:\n  review_bots:\n    logins: [\"Copilot\"]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &Config{}
	rb, err := cfg.EffectiveReviewBots(project)
	if err != nil || !rb.IsBot("Copilot") {
		t.Errorf("expected project-file fallback; got %+v, %v", rb, err)
	}

	cfg.Classification.ReviewBots = ReviewBotsConfig{Logins: []string{"chatgpt-codex-connector[bot]"}}
	rb, err = cfg.EffectiveReviewBots(project)
	if err != nil || !rb.IsBot("chatgpt-codex-connector[bot]") || rb.IsBot("Copilot") {
		t.Errorf("hive.yaml block must win; got %+v, %v", rb, err)
	}

	var nilCfg *Config
	rb, err = nilCfg.EffectiveReviewBots(filepath.Join(dir, "absent.yaml"))
	if err != nil || rb.Enabled() {
		t.Errorf("nil config + absent project file must be off; got %+v, %v", rb, err)
	}
}

// The block round-trips through the Go Config schema under the same key path
// hive-project.yaml uses, so one snippet works in either file.
func TestConfig_ClassificationReviewBotsYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hive.yaml")
	doc := `project:
  org: acme
  repos: [acme/app]
github:
  token: ghp_tok
agents:
  scanner:
    backend: claude
classification:
  review_bots:
    logins: ["Copilot"]
`
	if err := os.WriteFile(path, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadWithOverrides(path, "-")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Classification.ReviewBots.IsBot("Copilot") {
		t.Errorf("classification.review_bots not loaded from hive.yaml: %+v", cfg.Classification)
	}
}
