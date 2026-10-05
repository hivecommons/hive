package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublicKnowledgeDashboardOverlay(t *testing.T) {
	dir := t.TempDir()
	seedPath := filepath.Join(dir, "hive.yaml")
	seed := "project:\n  org: testorg\n  repos: [repo1]\ngithub:\n  token: test\nagents:\n  scanner:\n    backend: copilot\n    model: claude-sonnet-4-6\nknowledge:\n  public:\n    enabled: true\n    tags: [seed]\n"
	if err := os.WriteFile(seedPath, []byte(seed), 0600); err != nil {
		t.Fatal(err)
	}
	original := DashboardOverlayFile
	DashboardOverlayFile = filepath.Join(dir, "overlay.yaml")
	t.Cleanup(func() { DashboardOverlayFile = original })
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	for _, tc := range []struct {
		name, overlay string
		enabled       bool
		tag           string
	}{
		{"explicit disable", "knowledge:\n  public:\n    enabled: false\n    tags: [public]\n", false, "public"},
		{"absent preserves seed", "knowledge:\n  enabled: false\n", true, "seed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := os.WriteFile(DashboardOverlayFile, []byte(tc.overlay), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadWithDashboardOverlay(seedPath)
			if err != nil {
				t.Fatal(err)
			}
			p := cfg.Knowledge.Public
			if p == nil || p.Enabled != tc.enabled || len(p.Tags) != 1 || p.Tags[0] != tc.tag {
				t.Fatalf("restored public settings: %+v", p)
			}
		})
	}
}
