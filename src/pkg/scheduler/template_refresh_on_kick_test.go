package scheduler

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// withUserSavedPolicyDir points the user-saved policy dir at a temp dir for the
// duration of a test and restores the production default afterward. In
// production this is the fixed /data/policies path that the dashboard prompt
// editor (handleAgentPromptSave) writes to.
func withUserSavedPolicyDir(t *testing.T, dir string) {
	t.Helper()
	prev := userSavedPolicyDir
	userSavedPolicyDir = dir
	t.Cleanup(func() { userSavedPolicyDir = prev })
}

// TestKickUsesLatestSavedTemplate is the regression test for issue #3239: after
// a user edits an agent's prompt template in the dashboard, the NEXT kick must
// render the freshly-saved template — not the stale git-cloned examples copy
// that used to shadow the override.
//
// It reproduces the reporter's ACMM-L2 quality-agent scenario:
//   - the agent has a kick_template (quality-advisory.md),
//   - the git-cloned policies repo has an OLD copy under examples/…/agents/,
//   - the dashboard save wrote the NEW copy to <userSavedPolicyDir>/quality-advisory.md.
//
// The kick must reflect the NEW copy.
func TestKickUsesLatestSavedTemplate(t *testing.T) {
	localDir := t.TempDir()   // stands in for the git-cloned /data/policies repo
	savedDir := t.TempDir()   // stands in for /data/policies (user-saved overrides)
	withUserSavedPolicyDir(t, savedDir)

	// Old, git-cloned copy that used to win.
	examplesDir := filepath.Join(localDir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(examplesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const oldWorkflow = "## Workflow\n3. Analyze test coverage: `go test -coverprofile=coverage.out ./...` or equivalent\n"
	if err := os.WriteFile(filepath.Join(examplesDir, "quality-advisory.md"), []byte(oldWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project: config.ProjectConfig{
			Org: "testorg", Name: "test", PrimaryRepo: "testrepo",
			Repos: []string{"testrepo"},
		},
		Agents: map[string]config.AgentConfig{
			"quality": {Role: "quality", KickTemplate: "quality-advisory.md"},
		},
		Policies: config.PoliciesConfig{LocalDir: localDir},
	}
	s := New(cfg, slog.Default())

	// 1. Before any user edit: the kick uses the git-cloned copy.
	msg := s.BuildAgentMessage("quality", nil, nil)
	if !strings.Contains(msg, "coverprofile=coverage.out") {
		t.Fatalf("expected the git-cloned template before edit; got:\n%s", msg)
	}

	// 2. User edits the prompt in the dashboard. handleAgentPromptSave writes the
	//    edited template to <userSavedPolicyDir>/<KickTemplate>.
	const newWorkflow = "## Workflow\n3. Analyze test coverage.\n   For unit tests, use `go test -coverprofile=coverage.out ./...`.\n   For end-to-end tests, see the attached workflow results.\nUNIQUE_MARKER_3239\n"
	if err := os.WriteFile(filepath.Join(savedDir, "quality-advisory.md"), []byte(newWorkflow), 0o644); err != nil {
		t.Fatal(err)
	}

	// 3. The NEXT kick must reflect the freshly-saved template.
	msg = s.BuildAgentMessage("quality", nil, nil)
	if !strings.Contains(msg, "UNIQUE_MARKER_3239") {
		t.Fatalf("kick did not pick up the user-saved template edit (#3239); got:\n%s", msg)
	}
	if strings.Contains(msg, "or equivalent") {
		t.Fatalf("kick still rendered the stale git-cloned template (#3239); got:\n%s", msg)
	}
}

// TestLoadNamedTemplatePrefersUserSavedOverride proves the read-side precedence
// directly: a user-saved override wins over the git-cloned examples copy.
func TestLoadNamedTemplatePrefersUserSavedOverride(t *testing.T) {
	localDir := t.TempDir()
	savedDir := t.TempDir()
	withUserSavedPolicyDir(t, savedDir)

	examplesDir := filepath.Join(localDir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(examplesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(examplesDir, "quality-advisory.md"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(savedDir, "quality-advisory.md"), []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project:  config.ProjectConfig{Org: "testorg", Name: "test"},
		Agents:   map[string]config.AgentConfig{},
		Policies: config.PoliciesConfig{LocalDir: localDir},
	}
	s := New(cfg, slog.Default())

	if got := s.loadNamedTemplate("quality-advisory.md"); got != "NEW" {
		t.Fatalf("loadNamedTemplate: expected user-saved override %q, got %q", "NEW", got)
	}
}

// TestLoadPromptTemplatePrefersUserSavedOverride proves the convention-path read
// (<agent>.md) also prefers a user-saved override.
func TestLoadPromptTemplatePrefersUserSavedOverride(t *testing.T) {
	localDir := t.TempDir()
	savedDir := t.TempDir()
	withUserSavedPolicyDir(t, savedDir)

	examplesDir := filepath.Join(localDir, "examples", "kubestellar", "agents")
	if err := os.MkdirAll(examplesDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(examplesDir, "helper.md"), []byte("OLD"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(savedDir, "helper.md"), []byte("NEW"), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Project:  config.ProjectConfig{Org: "testorg", Name: "test"},
		Agents:   map[string]config.AgentConfig{},
		Policies: config.PoliciesConfig{LocalDir: localDir},
	}
	s := New(cfg, slog.Default())

	if got := s.loadPromptTemplate("helper"); got != "NEW" {
		t.Fatalf("loadPromptTemplate: expected user-saved override %q, got %q", "NEW", got)
	}
}
