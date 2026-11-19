package config

import (
	"os"
	"path/filepath"
	"testing"
)

// overlayReviewSandboxSeed is a ConfigMap seed carrying a review gate and an
// agent sandbox that differ from what the dashboard later saved (#18804).
const overlayReviewSandboxSeed = `
project:
  org: testorg
  repos: [repo1]
github:
  token: ghp_test123456789
agents:
  scanner:
    backend: copilot
    model: claude-sonnet-4-6
review:
  combined_perspectives: false
  require_labels: [seed-label]
  max_parallel_reviews: 3
agent_sandbox:
  enabled: false
  image: seed/image:1
  job:
    workspace_claim: seed-claim
`

// overlayReviewSandboxOverlay is a short overlay (no agents) as the dashboard
// leaves it, so the fullness guard would bail and the blocks must be adopted
// regardless.
const overlayReviewSandboxOverlay = `
project:
  org: testorg
  repos: [repo1]
review:
  combined_perspectives: true
  require_labels: [overlay-label]
  max_parallel_reviews: 3
  perspectives_source:
    owner: testorg
    repo: perspectives
    path: perspectives.yaml
    ref: main
agent_sandbox:
  enabled: true
  image: overlay/image:2
  job:
    workspace_claim: overlay-claim
    ttl_seconds: 120
`

func setupOverlayReviewSandbox(t *testing.T, seed, overlay string) string {
	t.Helper()
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	if err := os.WriteFile(seedPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	overlayPath := filepath.Join(dir, "hive.yaml.dashboard")
	if err := os.WriteFile(overlayPath, []byte(overlay), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := DashboardOverlayFile
	DashboardOverlayFile = overlayPath
	t.Cleanup(func() { DashboardOverlayFile = orig })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	return seedPath
}

// TestLoadWithDashboardOverlay_AdoptsReviewAndAgentSandbox: every reload entry
// point (the agent loader, the hub loader) must let the overlay win for the
// review gate and the agent sandbox, and keep the rest of the seed.
func TestLoadWithDashboardOverlay_AdoptsReviewAndAgentSandbox(t *testing.T) {
	seedPath := setupOverlayReviewSandbox(t, overlayReviewSandboxSeed, overlayReviewSandboxOverlay)
	loaders := map[string]func(string) (*Config, error){
		"LoadWithDashboardOverlay":       LoadWithDashboardOverlay,
		"LoadWithDashboardOverlayForHub": LoadWithDashboardOverlayForHub,
	}
	for name, load := range loaders {
		t.Run(name, func(t *testing.T) {
			merged, err := load(seedPath)
			if err != nil {
				t.Fatalf("%s: %v", name, err)
			}
			if !merged.Review.CombinedPerspectives {
				t.Errorf("review.combined_perspectives lost: overlay true, got false")
			}
			if got := merged.Review.RequireLabels; len(got) != 1 || got[0] != "overlay-label" {
				t.Errorf("review.require_labels = %v, want [overlay-label]", got)
			}
			ps := merged.Review.PerspectivesSource
			if ps == nil || ps.Repo != "perspectives" || ps.Path != "perspectives.yaml" {
				t.Errorf("review.perspectives_source lost: %+v", ps)
			}
			if !merged.AgentSandbox.Enabled {
				t.Errorf("agent_sandbox.enabled lost: overlay true, got false")
			}
			if merged.AgentSandbox.Image != "overlay/image:2" {
				t.Errorf("agent_sandbox.image = %q, want overlay/image:2", merged.AgentSandbox.Image)
			}
			job := merged.AgentSandbox.Job
			if job == nil || job.WorkspaceClaim != "overlay-claim" || job.TTLSeconds != 120 {
				t.Errorf("agent_sandbox.job lost: %+v", job)
			}
			// The rest of the seed is intact.
			if merged.Project.Org != "testorg" || merged.GitHub.Token != "ghp_test123456789" {
				t.Errorf("seed project/github clobbered: %+v %q", merged.Project, merged.GitHub.Token)
			}
			if _, ok := merged.Agents["scanner"]; !ok {
				t.Errorf("seed agents clobbered: %v", merged.Agents)
			}
		})
	}
}

// TestLoadWithDashboardOverlay_SilentOverlayKeepsSeedReviewAndAgentSandbox: an
// overlay that carries neither block must not blank the seed's.
func TestLoadWithDashboardOverlay_SilentOverlayKeepsSeedReviewAndAgentSandbox(t *testing.T) {
	seedPath := setupOverlayReviewSandbox(t, overlayReviewSandboxSeed, "project:\n  org: testorg\n  repos: [repo1]\n")
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if got := merged.Review.RequireLabels; len(got) != 1 || got[0] != "seed-label" {
		t.Errorf("seed review.require_labels clobbered by silent overlay: %v", got)
	}
	if merged.AgentSandbox.Image != "seed/image:1" {
		t.Errorf("seed agent_sandbox.image clobbered by silent overlay: %q", merged.AgentSandbox.Image)
	}
}

// TestLoadWithDashboardOverlay_OverlayVariablesStaySeedOnly pins the security
// invariant next to the new adoption: widening the adopted blocks must never
// widen into variables.security.
func TestLoadWithDashboardOverlay_OverlayVariablesStaySeedOnly(t *testing.T) {
	overlay := overlayReviewSandboxOverlay + `
variables:
  security:
    allow_exec: true
`
	seedPath := setupOverlayReviewSandbox(t, overlayReviewSandboxSeed, overlay)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	if merged.Variables.Security.AllowExec {
		t.Errorf("overlay variables.security.allow_exec was adopted; it must stay seed-only")
	}
}

// TestLoadWithDashboardOverlay_AgentFileOutranksDashboardOverlay reproduces the
// standalone OpenShift report: an operator set `paused: true` and a `sandbox:`
// block in /data/agent-configs/reviewer.yaml, but the dashboard overlay's
// older copy of agents.reviewer (paused false, sandbox disabled) replaced it
// on load. The agent file is the higher layer and must win; other agents the
// file does not name keep the overlay's values.
func TestLoadWithDashboardOverlay_AgentFileOutranksDashboardOverlay(t *testing.T) {
	dir := t.TempDir()
	agentsDir := filepath.Join(dir, "agent-configs")
	if err := os.MkdirAll(agentsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := "project:\n  org: testorg\n  repos: [repo1]\ngithub:\n  token: ghp_test123456789\ndata:\n  agents_dir: " + agentsDir + "\nagents:\n  reviewer:\n    backend: copilot\n    model: claude-sonnet-4-6\n  scanner:\n    backend: copilot\n    model: claude-sonnet-4-6\n"
	overlay := `
project:
  org: testorg
  repos: [repo1]
agents:
  reviewer:
    backend: copilot
    model: claude-sonnet-4-6
    paused: false
    pause_owner: operator
    sandbox:
      enabled: false
  scanner:
    backend: copilot
    model: overlay-model
`
	file := `
backend: copilot
model: claude-sonnet-4-6
paused: true
pause_owner: operator
sandbox:
  enabled: true
  image: reviewer/sandbox:1
`
	if err := os.WriteFile(filepath.Join(agentsDir, "reviewer.yaml"), []byte(file), 0o644); err != nil {
		t.Fatal(err)
	}
	seedPath := setupOverlayReviewSandbox(t, seed, overlay)
	merged, err := LoadWithDashboardOverlay(seedPath)
	if err != nil {
		t.Fatalf("LoadWithDashboardOverlay: %v", err)
	}
	r := merged.Agents["reviewer"]
	if !r.Paused {
		t.Errorf("agent file paused: true lost to the dashboard overlay")
	}
	if r.Sandbox == nil || r.Sandbox.Image != "reviewer/sandbox:1" || r.Sandbox.Enabled == nil || !*r.Sandbox.Enabled {
		t.Errorf("agent file sandbox block lost to the dashboard overlay: %+v", r.Sandbox)
	}
	if got := merged.Agents["scanner"].Model; got != "overlay-model" {
		t.Errorf("agent without a file must keep the overlay value, got %q", got)
	}
}
