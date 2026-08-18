package channels

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kubestellar/hive/pkg/config"
)

func TestMatchesBeadReadsVisualHiveAgentMetadata(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ux-scout.json")
	data := []byte(`{"title":"UX review","metadata":{"visual_hive_agent":"ux-scout"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}

	watcher := NewBeadWatcher(nil)
	if !watcher.matchesBead(path, map[string]string{"visual_hive_agent": "ux-scout"}) {
		t.Fatal("expected Visual Hive agent metadata to match")
	}
	if watcher.matchesBead(path, map[string]string{"visual_hive_agent": "quality"}) {
		t.Fatal("unexpected match for a different agent")
	}
}

func TestVisualHiveBeadTriggersMatchingAgentKick(t *testing.T) {
	beadsDir := t.TempDir()
	beadPath := filepath.Join(beadsDir, "vh-ux-review.json")
	if err := os.WriteFile(beadPath, []byte(`{
		"title": "Review API error flow",
		"metadata": {"visual_hive_agent": "ux-scout"}
	}`), 0o600); err != nil {
		t.Fatal(err)
	}

	kicked := make(chan string, 1)
	manager := NewManager(
		map[string]config.AgentConfig{
			"ux-scout": {BeadsDir: beadsDir},
		},
		func(agent, _ string) error {
			kicked <- agent
			return nil
		},
		nil,
		nil,
	)
	watcher := manager.bead
	watcher.checkBeadsForAgent("ux-scout", config.AgentConfig{BeadsDir: beadsDir}, config.ChannelConfig{
		Type:  "bead",
		Match: map[string]string{"visual_hive_agent": "ux-scout"},
	})

	select {
	case agent := <-kicked:
		if agent != "ux-scout" {
			t.Fatalf("kick sent to %q, want ux-scout", agent)
		}
	case <-time.After(time.Second):
		t.Fatal("matching Visual Hive bead did not trigger an agent kick")
	}
}
