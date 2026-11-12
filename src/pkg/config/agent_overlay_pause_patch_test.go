package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestSetOverlayPausedNode(t *testing.T) {
	tests := []struct {
		name        string
		src         string
		paused      bool
		wantChanged bool
		wantErr     bool
		wantHas     string
	}{
		{"empty file, pause", "", true, true, false, "paused: true"},
		{"empty file, resume", "", false, false, false, ""},
		{"absent key, pause", "model: x\n", true, true, false, "paused: true"},
		{"absent key, resume", "model: x\n", false, false, false, ""},
		{"already equal true", "paused: true\n", true, false, false, ""},
		{"already equal false", "paused: false\n", false, false, false, ""},
		{"flip to false", "paused: true\nmodel: x\n", false, true, false, "paused: false"},
		{"flip to true", "model: x\npaused: false # note\n", true, true, false, "paused: true"},
		{"non-bool value", "paused: [a]\n", true, false, true, ""},
		{"non-map top level", "- a\n- b\n", true, false, true, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var doc yaml.Node
			if err := yaml.Unmarshal([]byte(tc.src), &doc); err != nil {
				t.Fatal(err)
			}
			changed, err := setOverlayPausedNode(&doc, tc.paused)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v", changed, tc.wantChanged)
			}
			if tc.wantHas != "" {
				out, err := yaml.Marshal(&doc)
				if err != nil {
					t.Fatal(err)
				}
				if !strings.Contains(string(out), tc.wantHas) {
					t.Errorf("output %q lacks %q", out, tc.wantHas)
				}
			}
		})
	}
}

func TestSetOverlayPausedNode_DocumentWithoutContent(t *testing.T) {
	doc := &yaml.Node{Kind: yaml.DocumentNode}
	changed, err := setOverlayPausedNode(doc, true)
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	if len(doc.Content) != 1 || doc.Content[0].Kind != yaml.MappingNode || len(doc.Content[0].Content) != 2 {
		t.Fatalf("unexpected document: %+v", doc)
	}
}

func TestSyncAgentOverlayPausedLocked_Branches(t *testing.T) {
	t.Run("empty AgentsDir", func(t *testing.T) {
		cfg, _, _ := overlayPauseSyncConfig(t)
		cfg.Data.AgentsDir = ""
		cfg.syncAgentOverlayPausedLocked("worker", true)
	})

	t.Run("production overlay dir is never touched", func(t *testing.T) {
		cfg, _, _ := overlayPauseSyncConfig(t)
		cfg.Data.AgentsDir = productionAgentOverlayDir
		cfg.syncAgentOverlayPausedLocked("worker", true)
	})

	t.Run("path-like names are ignored", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"../evil", "a/b", `a\b`} {
			cfg.syncAgentOverlayPausedLocked(name, true)
		}
		entries, err := os.ReadDir(agentsDir)
		if err != nil {
			t.Fatal(err)
		}
		if len(entries) != 0 {
			t.Errorf("unexpected files written: %v", entries)
		}
	})

	t.Run("missing overlay is left alone", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		cfg.syncAgentOverlayPausedLocked("worker", true)
		if _, err := os.Stat(filepath.Join(agentsDir, "worker.yaml")); !os.IsNotExist(err) {
			t.Errorf("overlay created for an agent without one: %v", err)
		}
	})

	t.Run("unreadable overlay", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(filepath.Join(agentsDir, "worker.yaml"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", true)
	})

	t.Run("malformed overlay is untouched", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agentsDir, "worker.yaml")
		const bad = "model: [unclosed\n"
		if err := os.WriteFile(p, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", true)
		got, _ := os.ReadFile(p)
		if string(got) != bad {
			t.Errorf("malformed overlay rewritten: %q", got)
		}
	})

	t.Run("non-map overlay is untouched", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agentsDir, "worker.yaml")
		const list = "- a\n- b\n"
		if err := os.WriteFile(p, []byte(list), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", true)
		got, _ := os.ReadFile(p)
		if string(got) != list {
			t.Errorf("non-map overlay rewritten: %q", got)
		}
	})

	t.Run("already equal is not rewritten", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agentsDir, "worker.yaml")
		const same = "model:   x\npaused: true\n"
		if err := os.WriteFile(p, []byte(same), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", true)
		got, _ := os.ReadFile(p)
		if string(got) != same {
			t.Errorf("overlay rewritten despite no change: %q", got)
		}
	})

	t.Run("write failure leaves the overlay intact", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agentsDir, "worker.yaml")
		const orig = "model: x\npaused: true\n"
		if err := os.WriteFile(p, []byte(orig), 0o644); err != nil {
			t.Fatal(err)
		}
		// A directory squatting on the temp path makes the write fail
		// regardless of the user's privileges.
		if err := os.MkdirAll(p+".tmp", 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", false)
		got, _ := os.ReadFile(p)
		if string(got) != orig {
			t.Errorf("overlay changed after failed write: %q", got)
		}
	})

	t.Run("patch succeeds and leaves no temp file", func(t *testing.T) {
		cfg, _, agentsDir := overlayPauseSyncConfig(t)
		if err := os.MkdirAll(agentsDir, 0o755); err != nil {
			t.Fatal(err)
		}
		p := filepath.Join(agentsDir, "worker.yaml")
		if err := os.WriteFile(p, []byte("model: x\npaused: true\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg.syncAgentOverlayPausedLocked("worker", false)
		if overlayPaused(t, agentsDir, "worker") {
			t.Error("overlay still paused")
		}
		if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
			t.Errorf("temp file left behind: %v", err)
		}
	})
}
