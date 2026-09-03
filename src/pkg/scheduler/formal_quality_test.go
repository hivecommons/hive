package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func formalQualityScheduler(t *testing.T, level int, enabled bool, role string) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "custom-quality.md"), []byte("CUSTOM QUALITY POLICY\n${ISSUE_LIST}"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{
		ACMMLevel: &level,
		Quality:   config.QualityConfig{Formal: enabled},
		Project: config.ProjectConfig{
			Org: "acme", Name: "Acme", PrimaryRepo: "widget", Repos: []string{"widget"},
		},
		Agents: map[string]config.AgentConfig{
			"quality": {Role: role, KickTemplate: "custom-quality.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: dir},
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func TestFormalQualityCapabilityReachesCustomizedKicks(t *testing.T) {
	s := formalQualityScheduler(t, config.FormalQualityMinACMMLevel, true, "quality")
	msg := s.BuildAgentMessage("quality", nil, nil)
	for _, want := range []string{
		"[agent:quality]",
		"Formal verification capability — operator enabled",
		"formal/<subsystem>/{model.pml,run.sh,README.md}",
		"expected-pass properties and intentional expected-fail",
		"reporting-only CI job",
		"[formal:<subsystem>:<property-id>]",
		"plain-language actor-by-actor interleaving",
		"FIX-BEFORE-NEW",
		"CUSTOM QUALITY POLICY",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("formal quality kick missing %q:\n%s", want, msg)
		}
	}
	if strings.Index(msg, "Formal verification capability") > strings.Index(msg, "CUSTOM QUALITY POLICY") {
		t.Fatalf("formal contract must precede customizable work selection:\n%s", msg)
	}
}

func TestFormalQualityCapabilityIsHardGated(t *testing.T) {
	for _, tt := range []struct {
		name    string
		level   int
		enabled bool
		role    string
	}{
		{name: "disabled at L5", level: 5, role: "quality"},
		{name: "enabled below L5", level: 4, enabled: true, role: "quality"},
		{name: "enabled for non-quality role", level: 6, enabled: true, role: "scanner"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := formalQualityScheduler(t, tt.level, tt.enabled, tt.role)
			msg := s.BuildAgentMessage("quality", nil, nil)
			if strings.Contains(msg, "Formal verification capability") {
				t.Fatalf("formal contract rendered outside its capability gate:\n%s", msg)
			}
			if !strings.Contains(msg, "CUSTOM QUALITY POLICY") {
				t.Fatalf("ordinary quality policy was suppressed:\n%s", msg)
			}
		})
	}
}

func TestFormalQualityCapabilityAppliesAtL6(t *testing.T) {
	s := formalQualityScheduler(t, 6, true, "quality")
	if msg := s.BuildAgentMessage("quality", nil, nil); !strings.Contains(msg, "Formal verification capability") {
		t.Fatalf("L6 opt-in did not enable formal capability:\n%s", msg)
	}
}

func TestFormalQualityCapabilityAppliesToReplicas(t *testing.T) {
	s := formalQualityScheduler(t, 5, true, "quality")
	s.cfg.Agents["quality-2"] = config.AgentConfig{Role: "quality", ReplicaOf: "quality"}
	msg := s.BuildAgentMessage("quality-2", nil, nil)
	if !strings.Contains(msg, "[agent:quality-2]") || !strings.Contains(msg, "Formal verification capability") {
		t.Fatalf("quality replica did not inherit formal capability:\n%s", msg)
	}
}
