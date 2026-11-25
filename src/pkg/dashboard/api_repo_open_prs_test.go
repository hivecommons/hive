package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/governor"
)

// UI/API round trip: POST /api/repos/open-prs sets the mode, it is in force
// at once, it persists, and both the repo card and the Settings → Repos
// payload show it. It works below L6 (no level gate).
func TestRepoOpenPRsEndpointRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	cfg := srv.deps.Config
	low := 2
	cfg.ACMMLevel = &low

	w := postRepoPause(t, srv, "/api/repos/open-prs", `{"repo":"testrepo","mode":"workflow_only"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("set workflow_only: code = %d body = %s", w.Code, w.Body.String())
	}
	body := decodeRepoPauseBody(t, w)
	if body["mode"] != "workflow_only" || body["effective"] != "workflow_only" || body["changed"] != true || body["persisted"] != true {
		t.Fatalf("response = %v", body)
	}
	if got := cfg.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsWorkflowOnly {
		t.Fatalf("mode in force = %q", got)
	}
	reloaded, err := config.Load(cfg.SourcePath)
	if err != nil {
		t.Fatalf("Load persisted config: %v", err)
	}
	if got := reloaded.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsWorkflowOnly {
		t.Fatalf("mode not persisted: %q", got)
	}

	repos := buildRepos(cfg, nil, governor.State{})
	if len(repos) != 1 || repos[0].OpenPRs != "workflow_only" || repos[0].OpenPRsEffective != "workflow_only" {
		t.Fatalf("repo card = %+v", repos)
	}

	rec := doOwnerGet(srv, "/api/config/governor")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET governor: %d %s", rec.Code, rec.Body.String())
	}
	var gov struct {
		RepoOpenPRs     map[string]map[string]string `json:"repoOpenPRs"`
		OpenPRsHiveWide bool                         `json:"openPRsHiveWide"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &gov); err != nil {
		t.Fatal(err)
	}
	if !gov.OpenPRsHiveWide {
		t.Error("openPRsHiveWide = false with project.open_prs unset")
	}
	found := false
	for repo, e := range gov.RepoOpenPRs {
		if strings.HasSuffix(repo, "/testrepo") {
			found = e["value"] == "workflow_only" && e["effective"] == "workflow_only"
		}
	}
	if !found {
		t.Fatalf("Settings → Repos payload lacks the mode: %v", gov.RepoOpenPRs)
	}

	// Back to on clears the override.
	if w = postRepoPause(t, srv, "/api/repos/open-prs", `{"repo":"testrepo","mode":"on"}`, true); w.Code != http.StatusOK {
		t.Fatalf("set on: %d %s", w.Code, w.Body.String())
	}
	if got := cfg.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsOn {
		t.Fatalf("mode after on = %q", got)
	}
}

func TestRepoOpenPRsEndpointRejectsBadInput(t *testing.T) {
	srv := newFullServer(t)
	for body, want := range map[string]int{
		`{"repo":"testrepo","mode":"sometimes"}`: http.StatusBadRequest,
		`{"repo":"testrepo"}`:                    http.StatusBadRequest,
		`{"mode":"off"}`:                         http.StatusBadRequest,
		`{"repo":"not-watched","mode":"off"}`:    http.StatusBadRequest,
	} {
		if w := postRepoPause(t, srv, "/api/repos/open-prs", body, true); w.Code != want {
			t.Errorf("%s: code = %d, want %d (%s)", body, w.Code, want, w.Body.String())
		}
	}
}

// Same asymmetric gate as auto-merge (#9070): a repo-write user may narrow
// the mode, only a verified owner may widen it again.
func TestRepoOpenPRsEndpointAsymmetricGate(t *testing.T) {
	srv := newFullServer(t)
	cfg := srv.deps.Config
	postAsRepoWriter := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/repos/open-prs", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-User", "testorg")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}
	if w := postRepoPause(t, srv, "/api/repos/open-prs", `{"repo":"testrepo","mode":"off"}`, false); w.Code != http.StatusForbidden {
		t.Fatalf("anonymous narrow: code = %d, want 403", w.Code)
	}
	if w := postAsRepoWriter(`{"repo":"testrepo","mode":"off"}`); w.Code != http.StatusOK {
		t.Fatalf("repo-write narrow: code = %d body = %s", w.Code, w.Body.String())
	}
	if got := cfg.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsOff {
		t.Fatalf("mode after narrow = %q", got)
	}
	if w := postAsRepoWriter(`{"repo":"testrepo","mode":"workflow_only"}`); w.Code != http.StatusForbidden {
		t.Fatalf("repo-write widen: code = %d, want 403", w.Code)
	}
	if got := cfg.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsOff {
		t.Fatalf("repo-write user widened the mode: %q", got)
	}
	if w := postRepoPause(t, srv, "/api/repos/open-prs", `{"repo":"testrepo","mode":"workflow_only"}`, true); w.Code != http.StatusOK {
		t.Fatalf("owner widen: code = %d body = %s", w.Code, w.Body.String())
	}
	if got := cfg.RepoOpenPRsMode("testrepo"); got != config.RepoOpenPRsWorkflowOnly {
		t.Fatalf("mode after owner widen = %q", got)
	}
}

// The select sits next to the auto-merge switch on the repo card and on the
// Settings → Repos row, and posts to the endpoint.
func TestRepoOpenPRsStaticWiring(t *testing.T) {
	html, err := os.ReadFile("static/index.html")
	if err != nil {
		t.Fatalf("read static index: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"function repoOpenPRsSelectHtml(repo, setting, hiveOff, canNarrow, extraClass)",
		"async function setRepoOpenPRs(repo, mode, el)",
		"fetch('/api/repos/open-prs'",
		`data-change-action="setRepoOpenPRs"`,
		"${autoMergeBtn}${openPrsCtl}",
		"${cbRow}${opRow}",
		"_configState.data.repoOpenPRs",
		"{ value: 'workflow_only', label: 'Workflow only'",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("static dashboard missing %q", want)
		}
	}
}
