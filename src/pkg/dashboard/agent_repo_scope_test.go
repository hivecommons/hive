package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

func putAgentGeneral(t *testing.T, srv *Server, name, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPut, "/api/config/agent/"+name+"/general", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

// The round trip an operator performs: scope an agent to one repo, see it
// reported, then clear the scope.
func TestAgentRepoScope_SetAndClear(t *testing.T) {
	srv := newFullServer(t)

	w := putAgentGeneral(t, srv, "scanner", `{"repos":["testrepo"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("scope: code = %d, body = %s", w.Code, w.Body.String())
	}
	got := srv.deps.Config.Agents["scanner"]
	if len(got.Repos) != 1 || got.Repos[0] != "testrepo" {
		t.Fatalf("Repos = %v, want [testrepo]", got.Repos)
	}
	if !got.ReposIsOperatorOwned() {
		t.Error("an operator scope edit did not claim ownership of the field")
	}
	if srv.deps.Config.AgentServesRepo("scanner", "some-other-repo") {
		t.Error("scoped agent still serves a repo outside its scope")
	}

	// Clearing returns the agent to hive-wide. An empty array is meaningful and
	// must not be read as "field absent, leave it alone".
	w = putAgentGeneral(t, srv, "scanner", `{"repos":[]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.Agents["scanner"].IsRepoScoped() {
		t.Errorf("agent still scoped after clearing: %v", srv.deps.Config.Agents["scanner"].Repos)
	}
	if !srv.deps.Config.AgentServesRepo("scanner", "some-other-repo") {
		t.Error("cleared agent does not serve every repo")
	}
}

// A body that omits repos must not touch the scope: the general handler is a
// partial update and every other field behaves that way.
func TestAgentRepoScope_OmittedFieldLeavesScopeAlone(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.deps.Config.SetAgentReposAndSave("scanner", []string{"testrepo"}); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if w := putAgentGeneral(t, srv, "scanner", `{"displayName":"Scanner Two"}`); w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if got := srv.deps.Config.Agents["scanner"].Repos; len(got) != 1 || got[0] != "testrepo" {
		t.Errorf("Repos = %v after an unrelated edit, want the scope preserved", got)
	}
}

// A scope entry that cannot name a repository is rejected on the request that
// caused it, not as an agent that silently has nowhere to work.
func TestAgentRepoScope_RejectsUnusableEntries(t *testing.T) {
	srv := newFullServer(t)
	for _, body := range []string{
		`{"repos":["https://github.com/acme/console"]}`,
		`{"repos":["acme/console/extra"]}`,
		`{"repos":"testrepo"}`,
		`{"repos":[123]}`,
		`{"repos":["   "]}`,
	} {
		w := putAgentGeneral(t, srv, "scanner", body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("body %s: code = %d, want 400", body, w.Code)
		}
	}
	if srv.deps.Config.Agents["scanner"].IsRepoScoped() {
		t.Error("a rejected request still changed the scope")
	}
}

// The agents list reports the declared scope AND the watched intersection.
// Reporting them separately is what makes a scope entry the hive does not watch
// visible instead of silently disappearing.
func TestAgentsList_ReportsScope(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Agents["scanner"] = func() config.AgentConfig {
		ac := srv.deps.Config.Agents["scanner"]
		ac.Repos = []string{"testrepo", "not-watched"}
		return ac
	}()

	req := httptest.NewRequest(http.MethodGet, "/api/agents", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}

	var entries []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var scanner, other map[string]any
	for _, e := range entries {
		switch e["name"] {
		case "scanner":
			scanner = e
		case "reviewer":
			other = e
		}
	}
	if scanner == nil {
		t.Fatalf("scanner missing from %v", entries)
	}
	declared, _ := scanner["repos"].([]any)
	watched, _ := scanner["watchedRepos"].([]any)
	if len(declared) != 2 {
		t.Errorf("repos = %v, want both declared entries", declared)
	}
	if len(watched) != 1 || watched[0] != "testrepo" {
		t.Errorf("watchedRepos = %v, want only the entry this hive watches", watched)
	}
	// An unscoped agent carries neither field, so the payload is unchanged for
	// every hive that does not use the feature.
	if other != nil {
		if _, ok := other["repos"]; ok {
			t.Errorf("unscoped agent carries a repos field: %v", other)
		}
		if _, ok := other["watchedRepos"]; ok {
			t.Errorf("unscoped agent carries a watchedRepos field: %v", other)
		}
	}
}

// The config dialog needs both the agent's scope and the hive's repo list, so
// it can offer real choices instead of a free-text field spelled from memory.
func TestAgentConfigGet_CarriesScopeAndProjectRepos(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.deps.Config.SetAgentReposAndSave("scanner", []string{"testrepo"}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/config/agent/scanner", nil)
	markOwnerRequest(req)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	var payload struct {
		General struct {
			Repos        []string `json:"repos"`
			ProjectRepos []string `json:"projectRepos"`
		} `json:"general"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(payload.General.Repos) != 1 || payload.General.Repos[0] != "testrepo" {
		t.Errorf("general.repos = %v, want [testrepo]", payload.General.Repos)
	}
	if len(payload.General.ProjectRepos) == 0 {
		t.Error("general.projectRepos is empty; the dialog cannot offer choices")
	}
}

func TestValidateAgentRepoRef(t *testing.T) {
	for _, ok := range []string{"console", "acme/console", "my-repo.v2", "a_b-c"} {
		if err := validateAgentRepoRef(ok); err != nil {
			t.Errorf("validateAgentRepoRef(%q) = %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"https://github.com/a/b", "a/b/c", "a b", strings.Repeat("x", 201)} {
		if err := validateAgentRepoRef(bad); err == nil {
			t.Errorf("validateAgentRepoRef(%q) = nil, want an error", bad)
		}
	}
}
