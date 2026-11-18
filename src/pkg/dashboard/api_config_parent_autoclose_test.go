package dashboard

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19070: the owner-only repos.<repo>.parent_autoclose editor.

const parentAutoClosePath = "/api/config/repos/parent-autoclose"

func decodeParentAutoClose(t *testing.T, body []byte) parentAutoCloseResponse {
	t.Helper()
	var got parentAutoCloseResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	return got
}

func TestParentAutoClose_OwnerOnly(t *testing.T) {
	s := covApiServer(t)
	if rec := doGetNoRole(s, parentAutoClosePath); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner GET: expected 403, got %d", rec.Code)
	}
	if rec := doPutNoRole(s, parentAutoClosePath, `{"repo":"repo1","value":"false"}`); rec.Code != http.StatusForbidden {
		t.Fatalf("non-owner PUT: expected 403, got %d", rec.Code)
	}
	if got := s.deps.Config.ParentAutoCloseFor("repo1"); got != config.ParentAutoCloseOn {
		t.Fatalf("a refused PUT changed the policy to %q", got)
	}
}

func TestParentAutoClose_GetListsMonitoredReposAndOptions(t *testing.T) {
	s := covApiServer(t)
	s.deps.Config.Repos = map[string]config.RepoSettings{"repo1": {ParentAutoClose: config.ParentAutoCloseEpicsToo}}
	s.deps.Config.Project.Repos = []string{"repo1", "repo2"}
	rec := doOwnerGet(s, parentAutoClosePath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeParentAutoClose(t, rec.Body.Bytes())
	if got.Default != config.ParentAutoCloseOn || len(got.Options) != 3 {
		t.Fatalf("default=%q options=%v", got.Default, got.Options)
	}
	want := []parentAutoCloseRepo{
		{Repo: "repo1", Value: config.ParentAutoCloseEpicsToo, Explicit: true},
		{Repo: "repo2", Value: config.ParentAutoCloseOn, Explicit: false},
	}
	if len(got.Repos) != len(want) {
		t.Fatalf("repos = %v", got.Repos)
	}
	for i := range want {
		if got.Repos[i] != want[i] {
			t.Errorf("repos[%d] = %+v, want %+v", i, got.Repos[i], want[i])
		}
	}
}

func TestParentAutoClose_PutAppliesLiveAndAudits(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	rec := doPut(s, parentAutoClosePath, map[string]any{"repo": "myorg/repo1", "value": "epics_too"})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if got := cfg.ParentAutoCloseFor("repo1"); got != config.ParentAutoCloseEpicsToo {
		t.Fatalf("live policy = %q, want epics_too", got)
	}
	if got := decodeParentAutoClose(t, rec.Body.Bytes()); len(got.Repos) != 1 || got.Repos[0].Value != config.ParentAutoCloseEpicsToo || !got.Repos[0].Explicit {
		t.Fatalf("response repos = %+v", got.Repos)
	}
	if s.audit != nil {
		found := false
		for _, entry := range s.audit.Recent(50) {
			if entry.Action == "config_repo_parent_autoclose" {
				found = true
			}
		}
		if !found {
			t.Error("missing config_repo_parent_autoclose audit")
		}
	}

	// Choosing the default clears the override.
	if rec := doPut(s, parentAutoClosePath, map[string]any{"repo": "repo1", "value": "true"}); rec.Code != http.StatusOK {
		t.Fatalf("PUT default: %d %s", rec.Code, rec.Body.String())
	}
	if cfg.ParentAutoCloseExplicit("repo1") || len(cfg.Repos) != 0 {
		t.Fatalf("default must leave no override, got %v", cfg.Repos)
	}
}

func TestParentAutoClose_PutValidation(t *testing.T) {
	s := covApiServer(t)
	cases := []struct {
		name string
		body map[string]any
		code int
	}{
		{"bad value", map[string]any{"repo": "repo1", "value": "maybe"}, http.StatusBadRequest},
		{"empty value", map[string]any{"repo": "repo1", "value": ""}, http.StatusBadRequest},
		{"missing repo", map[string]any{"value": "false"}, http.StatusBadRequest},
		{"unmonitored repo", map[string]any{"repo": "other/thing", "value": "false"}, http.StatusNotFound},
	}
	for _, tc := range cases {
		if rec := doPut(s, parentAutoClosePath, tc.body); rec.Code != tc.code {
			t.Errorf("%s: code %d, want %d (%s)", tc.name, rec.Code, tc.code, rec.Body.String())
		}
	}
	if len(s.deps.Config.Repos) != 0 {
		t.Fatalf("a refused PUT stored %v", s.deps.Config.Repos)
	}
}

func TestRestoreParentAutoClose(t *testing.T) {
	cfg := &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widgets"}}}
	if err := cfg.SetParentAutoClose("widgets", config.ParentAutoCloseOff); err != nil {
		t.Fatal(err)
	}
	prev, had := cfg.RepoSettingsFor("widgets")
	if err := cfg.SetParentAutoClose("widgets", config.ParentAutoCloseEpicsToo); err != nil {
		t.Fatal(err)
	}
	restoreParentAutoClose(cfg, "widgets", config.ParentAutoCloseOff, prev, had)
	if got := cfg.ParentAutoCloseFor("widgets"); got != config.ParentAutoCloseOff {
		t.Fatalf("restored = %q, want false", got)
	}
	// A repo that inherited the default goes back to inheriting it.
	restoreParentAutoClose(cfg, "widgets", config.ParentAutoCloseOn, config.RepoSettings{}, false)
	if cfg.ParentAutoCloseExplicit("widgets") {
		t.Fatal("restore to default left an override")
	}
}
