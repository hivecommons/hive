package dashboard

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/kubestellar/hive/v2/pkg/config"
)

// enableDefinitionAllowlist puts a slug on the seed-only GitHub allowlist so the
// keep-linked / prompt-source paths are permitted for it in tests.
func enableDefinitionAllowlist(deps *Dependencies, slugs ...string) {
	deps.Config.Variables.Security.AllowGitHubPrompt = true
	deps.Config.Variables.Security.GitHubPromptAllowlist = slugs
}

func TestDefinitionSourceFromURL(t *testing.T) {
	s := &Server{deps: &Dependencies{Config: &config.Config{}}}
	cases := []struct {
		name        string
		url         string
		wantErr     bool
		owner, repo string
		path, ref   string
	}{
		{
			name:  "blob url",
			url:   "https://github.com/kubestellar/hive/blob/v2/v2/examples/agents/x.yaml",
			owner: "kubestellar", repo: "hive", ref: "v2", path: "v2/examples/agents/x.yaml",
		},
		{
			name:  "raw url",
			url:   "https://raw.githubusercontent.com/kubestellar/hive/main/agents/scanner.yaml",
			owner: "kubestellar", repo: "hive", ref: "main", path: "agents/scanner.yaml",
		},
		{name: "not a file url", url: "https://github.com/kubestellar/hive", wantErr: true},
		{name: "garbage", url: "::::", wantErr: true},
		{name: "no host", url: "/kubestellar/hive/blob/v2/x.yaml", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := s.definitionSourceFromURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q", tc.url)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ds.Owner != tc.owner || ds.Repo != tc.repo || ds.Ref != tc.ref || ds.Path != tc.path {
				t.Errorf("parsed = %+v, want owner=%s repo=%s ref=%s path=%s", ds, tc.owner, tc.repo, tc.ref, tc.path)
			}
			if ds.URL != tc.url {
				t.Errorf("URL not round-tripped: %q", ds.URL)
			}
		})
	}
}

// TestImport_UncheckedStaysPlain: a normal import (no keepLinked) produces a
// baked agent with NO definition_source link.
func TestImport_UncheckedStaysPlain(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: plain-agent
spec:
  backend: claude
`, exportKind)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(yamlContent))
	}))
	defer ts.Close()

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":     "url",
		"url":        ts.URL + "/kubestellar/hive/blob/main/plain.yaml",
		"keepLinked": false,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	agent, ok := deps.Config.Agents["plain-agent"]
	if !ok {
		t.Fatal("plain-agent should exist")
	}
	if agent.DefinitionSource != nil {
		t.Errorf("unchecked import must NOT set definition_source, got %+v", agent.DefinitionSource)
	}
}

// TestImport_KeepLinkedPersistsDefinitionSource: keep-linked on an allowlisted
// repo persists definition_source (owner/repo/path/ref derived from the URL).
func TestImport_KeepLinkedPersistsDefinitionSource(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: linked-agent
spec:
  backend: claude
`, exportKind)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(yamlContent))
	}))
	defer ts.Close()

	// The blob path makes the parsed slug "kubestellar/hive"; allowlist it.
	enableDefinitionAllowlist(deps, "kubestellar/hive")

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":     "url",
		"url":        ts.URL + "/kubestellar/hive/blob/main/agents/linked.yaml",
		"keepLinked": true,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	agent, ok := deps.Config.Agents["linked-agent"]
	if !ok {
		t.Fatal("linked-agent should exist")
	}
	if agent.DefinitionSource == nil {
		t.Fatal("keep-linked import must set definition_source")
	}
	ds := agent.DefinitionSource
	if ds.Owner != "kubestellar" || ds.Repo != "hive" || ds.Path != "agents/linked.yaml" || ds.Ref != "main" {
		t.Errorf("definition_source = %+v, want kubestellar/hive agents/linked.yaml@main", ds)
	}
}

// TestImport_KeepLinkedRejectsNonAllowlistedRepo: keep-linked at a repo that is
// NOT on the seed-only allowlist is rejected server-side (a user cannot link an
// agent to an arbitrary repo).
func TestImport_KeepLinkedRejectsNonAllowlistedRepo(t *testing.T) {
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()
	// Feature enabled, but only a DIFFERENT repo is allowlisted.
	enableDefinitionAllowlist(deps, "kubestellar/docs")

	yamlContent := fmt.Sprintf(`apiVersion: hive.kubestellar.io/v1
kind: %s
metadata:
  name: evil-agent
spec:
  backend: claude
`, exportKind)

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(yamlContent))
	}))
	defer ts.Close()

	rec := doPost(s, "/api/agents/import", map[string]interface{}{
		"source":     "url",
		"url":        ts.URL + "/attacker/evil/blob/main/agent.yaml",
		"keepLinked": true,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for non-allowlisted keep-linked repo, got %d: %s", rec.Code, rec.Body.String())
	}
	if _, exists := deps.Config.Agents["evil-agent"]; exists {
		t.Error("agent must not be created when keep-linked repo is denied")
	}
}
