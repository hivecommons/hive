package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// #18137: an overlay-sourced reviewer-2 (a serialized derived replica, so
// ReplicaOf is empty) next to reviewer.replicas: 2 must not fail the load; the
// stray is dropped and exactly two reviewers run.
func TestExpandAgentReplicasDropsStrayReplicaOverlay(t *testing.T) {
	stray := AgentConfig{ID: "reviewer-2", Backend: "copilot", Replicas: 2, DisplayName: "reviewer"}
	stray.SetLoadState("reviewer-2", "/data/agent-configs/reviewer-2.yaml")
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"reviewer":   {ID: "reviewer", Backend: "copilot", Enabled: true, Replicas: 2},
		"reviewer-2": stray,
	}}}

	if err := cfg.ExpandAgentReplicas(); err != nil {
		t.Fatalf("ExpandAgentReplicas() error = %v, want nil (a stray replica overlay must not brick the hive)", err)
	}
	var reviewers []string
	for name := range cfg.Agents {
		if strings.HasPrefix(name, "reviewer") {
			reviewers = append(reviewers, name)
		}
	}
	if len(reviewers) != 2 {
		t.Fatalf("reviewer agents = %v, want exactly reviewer and reviewer-2", reviewers)
	}
	if got := cfg.Agents["reviewer-2"]; got.ReplicaOf != "reviewer" || got.ReplicaIndex != 2 {
		t.Fatalf("reviewer-2 = %+v, want the replica derived from reviewer", got)
	}
}

// A colliding agent declared in the main config (no overlay file) is still
// rejected: only a stale overlay copy is safe to drop.
func TestExpandAgentReplicasStillRejectsDeclaredCollision(t *testing.T) {
	cfg := &Config{AgentRuntimeConfig: AgentRuntimeConfig{Agents: map[string]AgentConfig{
		"reviewer":   {Replicas: 2},
		"reviewer-2": {Replicas: 1},
	}}}
	if err := cfg.ExpandAgentReplicas(); err == nil || !strings.Contains(err.Error(), "reviewer-2") {
		t.Fatalf("collision error = %v, want an error naming reviewer-2", err)
	}
}

// The incident end to end: a stray worker-2.yaml beside a worker overlay with
// replicas: 2 must load.
func TestLoadWithOverrides_StrayReplicaOverlayDoesNotFailLoad(t *testing.T) {
	configPath, _ := writeOverlayConfig(t, map[string]string{
		"worker":   "id: worker\nbackend: claude\nenabled: true\nreplicas: 2\n",
		"worker-2": "id: worker-2\nbackend: claude\nenabled: true\nreplicas: 2\ndisplay_name: worker\n",
	})
	cfg, err := LoadWithOverrides(configPath, "-")
	if err != nil {
		t.Fatalf("LoadWithOverrides() error = %v, want nil", err)
	}
	if got := cfg.Agents["worker-2"]; got.ReplicaOf != "worker" {
		t.Fatalf("worker-2.ReplicaOf = %q, want worker", got.ReplicaOf)
	}
}

// The writer half of #18137: saving a derived replica writes the base agent's
// overlay, never <base>-N.yaml.
func TestSaveAgentFileRoutesReplicaToBase(t *testing.T) {
	dir := t.TempDir()
	replica := AgentConfig{
		ID:           "reviewer-2",
		Backend:      "copilot",
		Model:        "gpt-5",
		BeadsDir:     "/data/beads/reviewer-2",
		Replicas:     2,
		ReplicaOf:    "reviewer",
		ReplicaIndex: 2,
		ReplicaCount: 2,
	}
	if err := SaveAgentFile(dir, "reviewer-2", replica); err != nil {
		t.Fatalf("SaveAgentFile() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "reviewer-2.yaml")); !os.IsNotExist(err) {
		t.Fatalf("reviewer-2.yaml exists (stat err %v); a derived replica must never get its own overlay", err)
	}
	agents, err := LoadAgentOverrides(dir)
	if err != nil {
		t.Fatalf("LoadAgentOverrides() error = %v", err)
	}
	base, ok := agents["reviewer"]
	if !ok {
		t.Fatalf("reviewer.yaml not written; overlays = %v", agents)
	}
	if base.ID != "reviewer" || base.BeadsDir != "/data/beads/reviewer" || base.Model != "gpt-5" || base.Replicas != 2 {
		t.Fatalf("routed overlay = %+v, want the base identity with the replica's edit", base)
	}
}

func TestSaveAgentFileRefusesReviewHead(t *testing.T) {
	dir := t.TempDir()
	head := AgentConfig{ID: "reviewer-gpt", Backend: "codex", ReplicaOf: "reviewer", ReviewHead: "gpt"}
	if err := SaveAgentFile(dir, "reviewer-gpt", head); err == nil {
		t.Fatal("SaveAgentFile() error = nil, want a refusal for a review head")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("overlay dir has %d entries, want none", len(entries))
	}
}
