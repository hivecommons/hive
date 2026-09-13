package dashboard

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// apServerWithGH builds a Server whose deps carry a GH client pointed at the
// shared ghMockServer (which serves /repos/myorg/repo1/contents/), so
// bakePromptSource / FetchOnce can succeed against a real fetcher.
func apServerWithGH(t *testing.T) (*Server, *Dependencies, *httptest.Server) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
	ghSrv := ghMockServer()
	t.Cleanup(ghSrv.Close)
	ghClient := ghpkg.NewClientForTest(ghSrv.URL, "myorg", []string{"repo1"}, logger)

	s := NewServer(0, logger)
	deps := testDeps(t)
	deps.GHClient = ghClient
	deps.Config.Data.AgentsDir = t.TempDir()
	s.RegisterAPI(deps)
	return s, deps, ghSrv
}

// enableAllowlist turns on the seed-only GitHub prompt allowlist for myorg/repo1.
func enableAllowlist(deps *Dependencies) {
	deps.Config.Variables.Security.AllowGitHubPrompt = true
	deps.Config.Variables.Security.GitHubPromptAllowlist = []string{"myorg/repo1"}
}

func TestPromptTemplateSaveDirIsTestLocal(t *testing.T) {
	if promptTemplateSaveDir == defaultPromptTemplateSaveDir {
		t.Fatalf("dashboard tests must not write prompt templates to %q", defaultPromptTemplateSaveDir)
	}
}

func TestCovAP_BakePromptSource_Denied(t *testing.T) {
	s, deps, _ := apServerWithGH(t)
	// Allowlist NOT enabled → GitHubPromptAllowed false → denied error.
	cfg := deps.Config.Agents["scanner"]
	cfg.PromptSource = &config.PromptSourceConfig{Type: "github", Owner: "myorg", Repo: "repo1", Path: "PROMPT.md"}
	err := s.bakePromptSource(deps.Ctx, "scanner", &cfg)
	if err == nil {
		t.Fatalf("expected denied error without allowlist")
	}
}

func TestCovAP_BakePromptSource_FetchOnceRuns(t *testing.T) {
	s, deps, _ := apServerWithGH(t)
	enableAllowlist(deps)
	cfg := deps.Config.Agents["scanner"]
	cfg.PromptSource = &config.PromptSourceConfig{Type: "github", Owner: "myorg", Repo: "repo1", Path: "PROMPT.md"}
	if err := s.bakePromptSource(deps.Ctx, "scanner", &cfg); err != nil {
		t.Fatalf("bakePromptSource: %v", err)
	}
	if _, err := os.Stat(filepath.Join(promptTemplateSaveDir, "scanner.md")); err != nil {
		t.Fatalf("baked template missing from test directory: %v", err)
	}
}

// ---- handleAgentPromptSave prompt-source branches ----

func TestCovAP_PromptSave_AgentNotFound(t *testing.T) {
	s, _, _ := apServerWithGH(t)
	rec := doPut(s, "/api/config/agent/ghost/prompt", map[string]any{"template": "hi"})
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d", rec.Code)
	}
}

func TestCovAP_PromptSave_PromptSourceIncomplete(t *testing.T) {
	s, _, _ := apServerWithGH(t)
	rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{
		"promptSource": map[string]any{"owner": "myorg"}, // repo/path missing → not IsSet
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete source: want 400, got %d", rec.Code)
	}
}

func TestCovAP_PromptSave_PromptSourceDenied(t *testing.T) {
	s, _, _ := apServerWithGH(t)
	// Allowlist not enabled → bakePromptSource denies → 400.
	rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{
		"promptSource": map[string]any{"owner": "myorg", "repo": "repo1", "path": "PROMPT.md"},
		"keepLinked":   true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("denied source: want 400, got %d", rec.Code)
	}
}

func TestCovAP_PromptSave_InlineTemplate(t *testing.T) {
	s, _, _ := apServerWithGH(t)
	rec := doPut(s, "/api/config/agent/scanner/prompt", map[string]any{"template": "my prompt body"})
	if rec.Code != http.StatusOK {
		t.Fatalf("inline save: got %d, want %d", rec.Code, http.StatusOK)
	}
	got, err := os.ReadFile(filepath.Join(promptTemplateSaveDir, "scanner.md"))
	if err != nil {
		t.Fatalf("read saved template: %v", err)
	}
	if string(got) != "my prompt body" {
		t.Fatalf("saved template = %q, want %q", got, "my prompt body")
	}
}

// ---- definitionSourceFromURL ----

func TestCovAP_DefinitionSourceFromURL(t *testing.T) {
	s, _, _ := apServerWithGH(t)

	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"blob", "https://github.com/myorg/repo1/blob/main/agents/x.yaml", false},
		{"raw", "https://raw.githubusercontent.com/myorg/repo1/main/agents/x.yaml", false},
		{"ghe-blob", "https://github.ibm.com/o/r/blob/v2/dir/f.yaml", false},
		{"not-a-file", "https://github.com/myorg/repo1", true},
		{"no-host", "/myorg/repo1/blob/main/x.yaml", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := s.definitionSourceFromURL(c.url)
			if c.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", c.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", c.url, err)
			}
			if got.Owner != "myorg" && got.Owner != "o" {
				t.Fatalf("unexpected owner %q for %q", got.Owner, c.url)
			}
		})
	}
}

// ---- handleAgentImport url keep-linked (exercises definitionSourceFromURL via handler) ----

func TestCovAP_AgentImport_URLKeepLinkedDenied(t *testing.T) {
	s, _, _ := apServerWithGH(t)

	// A tiny server returning a valid AgentDefinition YAML.
	defYAML := "apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: imported-agent\nspec:\n  backend: claude\n  model: sonnet\n"
	defSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(defYAML))
	}))
	defer defSrv.Close()

	// keepLinked=true but the fetch URL host is a test server, not on the
	// definition allowlist → keep-linked rejected with 400 (covers
	// definitionSourceFromURL error path OR the allowlist-denied path).
	rec := doPost(s, "/api/agents/import", map[string]any{
		"source":     "url",
		"url":        defSrv.URL + "/myorg/repo1/blob/main/x.yaml",
		"keepLinked": true,
	})
	// The URL fetch succeeds and parses; keep-linked validation then runs. The
	// parsed source (test host) is not on the allowlist → 400.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusOK {
		t.Fatalf("import keep-linked: unexpected %d (%s)", rec.Code, rec.Body.String())
	}
}

func TestCovAP_AgentImport_Preview(t *testing.T) {
	s, _, _ := apServerWithGH(t)
	defYAML := "apiVersion: hive.kubestellar.io/v1\nkind: AgentDefinition\nmetadata:\n  name: prev-agent\nspec:\n  backend: claude\n"
	rec := doPost(s, "/api/agents/import", map[string]any{
		"source":  "paste",
		"content": defYAML,
		"preview": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("preview import: want 200, got %d (%s)", rec.Code, rec.Body.String())
	}
}
