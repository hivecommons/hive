package worksource

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
	"github.com/kubestellar/hive/pkg/linearagent"
)

// TestFromConfig_LinearAssignedOnly verifies the fail-closed contract:
// assigned_only without a connected Linear agent is a configuration error,
// never a silent fallback to enumerating the whole backlog.
func TestFromConfig_LinearAssignedOnly(t *testing.T) {
	logger := slog.Default()
	cfg := config.WorkSourceConfig{Type: "linear"}
	cfg.Linear.AssignedOnly = true

	t.Setenv(linearagent.StoreEnvVar, filepath.Join(t.TempDir(), "missing.json"))
	if _, err := FromConfig(cfg, nil, "", "", logger); err == nil || !strings.Contains(err.Error(), "assigned_only") {
		t.Fatalf("err = %v, want assigned_only fail-closed error", err)
	}

	// With a connected install, the stored viewer id flows into the source.
	storePath := filepath.Join(t.TempDir(), "linear-agent.json")
	store, err := linearagent.NewStore(storePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Set(linearagent.Install{ViewerID: "viewer-9"}); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linearagent.StoreEnvVar, storePath)

	ws, err := FromConfig(cfg, nil, "", "", logger)
	if err != nil {
		t.Fatalf("FromConfig with install: %v", err)
	}
	ls, ok := ws.(*LinearSource)
	if !ok {
		t.Fatalf("source type %T", ws)
	}
	if ls.cfg.ViewerID != "viewer-9" {
		t.Errorf("ViewerID = %q", ls.cfg.ViewerID)
	}

	// A corrupt store also fails closed.
	badPath := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(badPath, []byte("{corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(linearagent.StoreEnvVar, badPath)
	if _, err := FromConfig(cfg, nil, "", "", logger); err == nil {
		t.Error("corrupt store did not fail closed")
	}
}
