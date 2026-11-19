package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func TestJobAgentsWithOwnModelSecret(t *testing.T) {
	cfg := &config.Config{
		Agents: map[string]config.AgentConfig{
			"job-a": {Backend: "litellm", Sandbox: &config.AgentSandboxOverride{Runtime: "job", Job: &config.SandboxJobConfig{EnvFromSecrets: []string{"creds"}}}},
			"job-b": {Backend: "litellm", Sandbox: &config.AgentSandboxOverride{Runtime: "job"}},
			"tmux":  {Backend: "litellm"},
			"other": {Backend: "x", Sandbox: &config.AgentSandboxOverride{Runtime: "job", Job: &config.SandboxJobConfig{EnvFromSecrets: []string{"c"}}}},
		},
	}
	got := jobAgentsWithOwnModelSecret(cfg, "litellm")
	if !strings.Contains(got, "job-a") || strings.Contains(got, "job-b") || strings.Contains(got, "tmux") || strings.Contains(got, "other") {
		t.Fatalf("unexpected warning: %q", got)
	}
	if jobAgentsWithOwnModelSecret(cfg, "none") != "" {
		t.Fatal("expected no warning")
	}
}
