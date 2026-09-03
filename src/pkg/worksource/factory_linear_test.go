package worksource

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/linearagent"
)

// TestFromConfig_LinearAssignedOnly verifies the fail-closed contract:
// assigned_only without a connected Linear agent is a configuration error,
// never a silent fallback to enumerating the whole backlog.
func TestFromConfig_LinearAssignedOnly(t *testing.T) {
	logger := slog.Default()
	cfg := config.WorkSourceConfig{Type: "linear"}
	cfg.Linear.APIKey = "key"
	cfg.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "acme/app"}}
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

func TestFromConfig_LinearValidation(t *testing.T) {
	logger := slog.Default()
	base := config.WorkSourceConfig{Type: "linear"}
	base.Linear.APIKey = "key"
	base.Linear.Teams = []config.LinearTeamSourceConfig{{Key: "ENG", Repo: "acme/app"}}
	if _, err := FromConfig(base, nil, "", "", logger); err != nil {
		t.Fatalf("valid linear config: %v", err)
	}

	cases := []struct {
		name string
		mut  func(*config.WorkSourceConfig)
		want string
	}{
		{"missing api key", func(c *config.WorkSourceConfig) { c.Linear.APIKey = "" }, "api_key"},
		{"missing teams", func(c *config.WorkSourceConfig) { c.Linear.Teams = nil }, "teams"},
		{"missing team key", func(c *config.WorkSourceConfig) { c.Linear.Teams[0].Key = "" }, "key"},
		{"missing team repo", func(c *config.WorkSourceConfig) { c.Linear.Teams[0].Repo = "" }, "repo"},
		{"unknown cycles", func(c *config.WorkSourceConfig) { c.Linear.Teams[0].Cycles = "next" }, "cycles"},
		{"missing project name", func(c *config.WorkSourceConfig) {
			c.Linear.Teams[0].Projects = []config.LinearProjectSourceConfig{{Repo: "acme/app"}}
		}, "projects"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			cfg.Linear.Teams = append([]config.LinearTeamSourceConfig(nil), base.Linear.Teams...)
			tc.mut(&cfg)
			_, err := FromConfig(cfg, nil, "", "", logger)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want containing %q", err, tc.want)
			}
		})
	}
}
