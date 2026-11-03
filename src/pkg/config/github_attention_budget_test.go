package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// github.attention.recipient_budget / digest_recipients load from the
// github.attention block and are validated with it (#16271).
func TestGitHubAttentionBudgetKeysLoad(t *testing.T) {
	var c struct {
		GitHub GitHubConfig `yaml:"github"`
	}
	if err := yaml.Unmarshal([]byte("github:\n  attention:\n    recipient_budget: 3\n    digest_recipients: [\"@Alice\", bob]\n"), &c); err != nil {
		t.Fatal(err)
	}
	a := c.GitHub.Attention
	if a.RecipientBudget != 3 || len(a.DigestRecipients) != 2 || !a.DigestOptIn("alice") || a.DigestOptIn("carol") {
		t.Fatalf("budget keys not loaded: %+v", a)
	}
	if err := a.Validate(); err != nil {
		t.Fatal(err)
	}
	var off struct {
		GitHub GitHubConfig `yaml:"github"`
	}
	if err := yaml.Unmarshal([]byte("github:\n  attention:\n    flap_window: 1h\n"), &off); err != nil {
		t.Fatal(err)
	}
	if off.GitHub.Attention.Config.Enabled() {
		t.Fatal("recipient_budget must default to off")
	}
	off.GitHub.Attention.RecipientBudget = -1
	if err := off.GitHub.Attention.Validate(); err == nil || !strings.Contains(err.Error(), "recipient_budget") {
		t.Fatalf("negative recipient_budget accepted: %v", err)
	}
}
