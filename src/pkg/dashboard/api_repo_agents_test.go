package dashboard

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/repoagents"
)

type fakeRepoAgentClient struct {
	mu      sync.Mutex
	files   map[string]string
	readErr error
}

func (f *fakeRepoAgentClient) ListDirectoryFiles(_ context.Context, owner, repo, dir, _ string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	prefix := owner + "/" + repo + "/" + dir + "/"
	var out []string
	for k := range f.files {
		if rest, ok := strings.CutPrefix(k, prefix); ok {
			out = append(out, rest)
		}
	}
	return out, nil
}

func (f *fakeRepoAgentClient) GetFileContentRef(_ context.Context, owner, repo, p, _ string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.readErr != nil {
		return "", f.readErr
	}
	c, ok := f.files[owner+"/"+repo+"/"+p]
	if !ok {
		return "", errors.New("not found")
	}
	return c, nil
}

const discoveredDef = `apiVersion: hive.kubestellar.io/v1
kind: AgentDefinition
metadata:
  name: Foo Bot
  displayName: Foo
spec:
  backend: claude
  model: opus
  mode: ISSUES_ONLY
  role: scanner
  tools:
    preset: full
`

const discoveredID = "myorg/repo1:.hive/agents/foo.yaml"

// repoAgentServer is an API server with discovery on and one pending entry
// for myorg/repo1/.hive/agents/foo.yaml (with a sibling foo.md prompt).
func repoAgentServer(t *testing.T) (*Server, *Dependencies, *repoagents.Discoverer, *fakeRepoAgentClient) {
	t.Helper()
	s, deps := apiServer(t)
	deps.Config.Data.AgentsDir = t.TempDir()
	enableDefinitionAllowlist(deps, "myorg/repo1")
	client := &fakeRepoAgentClient{files: map[string]string{
		"myorg/repo1/.hive/agents/foo.yaml": discoveredDef,
		"myorg/repo1/.hive/agents/foo.md":   "do foo",
	}}
	d := repoagents.New(repoagents.Options{
		Client: client,
		Allow:  deps.Config.GitHubDefinitionAllowed,
		Repos:  func() []string { return []string{"myorg/repo1"} },
	})
	d.ScanAll(context.Background())
	deps.RepoAgents = d
	return s, deps, d, client
}

func TestDiscoveredAgentsListDisabled(t *testing.T) {
	s, _ := apiServer(t)
	rec := doGet(s, "/api/agents/discovered")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("disabled list = %d %s", rec.Code, rec.Body.String())
	}
	for _, p := range []string{"/api/agents/discovered/approve", "/api/agents/discovered/reject", "/api/agents/discovered/refresh"} {
		if rec := doPost(s, p, map[string]any{"id": "x"}); rec.Code != http.StatusServiceUnavailable {
			t.Errorf("%s without discovery = %d, want 503", p, rec.Code)
		}
	}
}

func TestDiscoveredAgentsListAndRefresh(t *testing.T) {
	s, _, _, client := repoAgentServer(t)
	rec := doGet(s, "/api/agents/discovered")
	var got struct {
		Enabled bool               `json:"enabled"`
		Pending int                `json:"pending"`
		Entries []repoagents.Entry `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Enabled || got.Pending != 1 || len(got.Entries) != 1 || got.Entries[0].Name != "foo-bot" {
		t.Fatalf("list = %+v", got)
	}

	client.mu.Lock()
	client.files["myorg/repo1/.hive/agents/bar.yaml"] = strings.Replace(discoveredDef, "Foo Bot", "bar", 1)
	client.mu.Unlock()
	rec = doPost(s, "/api/agents/discovered/refresh", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"pending":2`) {
		t.Fatalf("refresh = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doPostNoOwner(s, "/api/agents/discovered/refresh", nil); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner refresh = %d", rec.Code)
	}
}

// TestDiscoveredAgentApprove: approval registers the agent with the repo
// file as its definition_source (marked discovered), the sibling .md as its
// prompt_source, and the owner's backend/model/mode, not the file's.
func TestDiscoveredAgentApprove(t *testing.T) {
	s, deps, d, _ := repoAgentServer(t)
	rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID, "model": "gpt-5", "mode": "ADVISORY"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	ac, ok := deps.Config.Agents["foo-bot"]
	if !ok {
		t.Fatal("agent foo-bot not registered")
	}
	if ac.Backend != defaultDiscoveredBackend || ac.Model != "gpt-5" || ac.Mode != "ADVISORY" || ac.Tools != nil {
		t.Errorf("owner-controlled fields from the file: backend=%q model=%q mode=%q tools=%+v", ac.Backend, ac.Model, ac.Mode, ac.Tools)
	}
	if ac.Role != "scanner" || ac.DisplayName != "Foo" {
		t.Errorf("definition fields not applied: role=%q display=%q", ac.Role, ac.DisplayName)
	}
	ds := ac.DefinitionSource
	if ds == nil || !ds.Discovered || ds.Owner != "myorg" || ds.Repo != "repo1" || ds.Path != ".hive/agents/foo.yaml" {
		t.Errorf("definition_source = %+v", ds)
	}
	ps := ac.PromptSource
	if ps == nil || ps.Path != ".hive/agents/foo.md" || ps.Owner != "myorg" {
		t.Errorf("prompt_source = %+v", ps)
	}
	if e, _ := d.Get(discoveredID); e.Status != repoagents.StatusApproved || e.Agent != "foo-bot" {
		t.Errorf("entry = %+v", e)
	}

	// A second approval of the same entry conflicts.
	if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID}); rec.Code != http.StatusConflict {
		t.Errorf("re-approve = %d", rec.Code)
	}
}

func TestDiscoveredAgentApproveCustomNameAndBackend(t *testing.T) {
	s, deps, _, _ := repoAgentServer(t)
	rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID, "name": "foo2", "backend": "claude"})
	if rec.Code != http.StatusOK {
		t.Fatalf("approve = %d %s", rec.Code, rec.Body.String())
	}
	if ac := deps.Config.Agents["foo2"]; ac.Backend != "claude" || ac.Model != "" {
		t.Errorf("agent = backend %q model %q", ac.Backend, ac.Model)
	}
}

func TestDiscoveredAgentApproveRefusals(t *testing.T) {
	t.Run("not owner", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPostNoOwner(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID}); rec.Code != http.StatusForbidden {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("bad body", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPostRaw(s, "/api/agents/discovered/approve", "{nope"); rec.Code != http.StatusBadRequest {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("bad mode", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID, "mode": "GOD_MODE"}); rec.Code != http.StatusBadRequest {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("unknown id", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": "nope"}); rec.Code != http.StatusNotFound {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("repo dropped off allowlist", func(t *testing.T) {
		s, deps, _, _ := repoAgentServer(t)
		deps.Config.Variables.Security.GitHubPromptAllowlist = nil
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID}); rec.Code != http.StatusForbidden {
			t.Errorf("got %d %s", rec.Code, rec.Body.String())
		}
		if _, ok := deps.Config.Agents["foo-bot"]; ok {
			t.Error("agent registered from a de-allowlisted repo")
		}
	})
	t.Run("file unreadable", func(t *testing.T) {
		s, _, _, client := repoAgentServer(t)
		client.readErr = errors.New("boom")
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID}); rec.Code != http.StatusBadGateway {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("name taken", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID, "name": "scanner"}); rec.Code != http.StatusConflict {
			t.Errorf("got %d", rec.Code)
		}
	})
	t.Run("bad name", func(t *testing.T) {
		s, _, _, _ := repoAgentServer(t)
		if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID, "name": "../etc"}); rec.Code != http.StatusBadRequest {
			t.Errorf("got %d", rec.Code)
		}
	})
}

func TestDiscoveredAgentReject(t *testing.T) {
	s, deps, d, _ := repoAgentServer(t)
	if rec := doPostNoOwner(s, "/api/agents/discovered/reject", map[string]any{"id": discoveredID}); rec.Code != http.StatusForbidden {
		t.Errorf("non-owner reject = %d", rec.Code)
	}
	if rec := doPostRaw(s, "/api/agents/discovered/reject", "{nope"); rec.Code != http.StatusBadRequest {
		t.Errorf("bad body = %d", rec.Code)
	}
	if rec := doPost(s, "/api/agents/discovered/reject", map[string]any{"id": "nope"}); rec.Code != http.StatusNotFound {
		t.Errorf("unknown = %d", rec.Code)
	}
	rec := doPost(s, "/api/agents/discovered/reject", map[string]any{"id": discoveredID})
	if rec.Code != http.StatusOK {
		t.Fatalf("reject = %d %s", rec.Code, rec.Body.String())
	}
	if e, _ := d.Get(discoveredID); e.Status != repoagents.StatusRejected {
		t.Errorf("entry = %+v", e)
	}
	if _, ok := deps.Config.Agents["foo-bot"]; ok {
		t.Error("rejected definition registered an agent")
	}
	if rec := doPost(s, "/api/agents/discovered/reject", map[string]any{"id": discoveredID}); rec.Code != http.StatusConflict {
		t.Errorf("re-reject = %d", rec.Code)
	}
	if rec := doPost(s, "/api/agents/discovered/approve", map[string]any{"id": discoveredID}); rec.Code != http.StatusConflict {
		t.Errorf("approve rejected = %d", rec.Code)
	}
}

func TestRepoAgentsNilDeps(t *testing.T) {
	if (&Server{}).repoAgents() != nil {
		t.Fatal("nil deps must report no discoverer")
	}
}
