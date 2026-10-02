package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/governor"

	"github.com/hivecommons/hive/pkg/config"
)

func postRepoPause(t *testing.T, srv *Server, path, body string, owner bool) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if owner {
		markOwnerRequest(req)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func decodeRepoPauseBody(t *testing.T, w *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decoding %q: %v", w.Body.String(), err)
	}
	return out
}

// The round trip an operator actually performs: pause with a reason, see it
// recorded with who/when/why, resume, see it gone.
func TestRepoPauseResumeRoundTrip(t *testing.T) {
	srv := newFullServer(t)

	w := postRepoPause(t, srv, "/api/repos/pause", `{"repo":"testrepo","reason":"release freeze"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("pause: code = %d, body = %s", w.Code, w.Body.String())
	}
	body := decodeRepoPauseBody(t, w)
	if body["changed"] != true || body["state"] != "paused" {
		t.Errorf("pause response = %v, want changed=true state=paused", body)
	}
	pause, _ := body["pause"].(map[string]any)
	if pause["by"] != "local" || pause["reason"] != "release freeze" {
		t.Errorf("pause provenance = %v, want the acting user and the reason", pause)
	}
	if pause["at"] == "" || pause["at"] == nil {
		t.Error("pause has no timestamp")
	}
	if !srv.deps.Config.IsRepoPaused("testrepo") {
		t.Error("config does not report the repo as paused")
	}

	// A paused repo keeps its card — that is pause, not deletion.
	repos := buildRepos(srv.deps.Config, nil, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("buildRepos returned %d cards, want 1: a paused repo must not vanish", len(repos))
	}
	if !repos[0].Paused || repos[0].PauseReason != "release freeze" || repos[0].PausedBy != "local" {
		t.Errorf("repo card does not carry the pause: %+v", repos[0])
	}

	w = postRepoPause(t, srv, "/api/repos/resume", `{"repo":"testrepo"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("resume: code = %d, body = %s", w.Code, w.Body.String())
	}
	body = decodeRepoPauseBody(t, w)
	if body["changed"] != true || body["state"] != "running" {
		t.Errorf("resume response = %v, want changed=true state=running", body)
	}
	if srv.deps.Config.IsRepoPaused("testrepo") {
		t.Error("repo still paused after resume")
	}
	if repos := buildRepos(srv.deps.Config, nil, governor.State{}); repos[0].Paused {
		t.Error("repo card still shows paused after resume")
	}
}

func TestRepoAutoMergeEndpointPersistsAndAuthorizes(t *testing.T) {
	srv := newFullServer(t)
	w := postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":false}`, false)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-owner code = %d, want 403", w.Code)
	}

	w = postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":false}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("disable auto-merge: code = %d body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo auto-merge still enabled after endpoint disabled it")
	}
	repos := buildRepos(srv.deps.Config, nil, governor.State{})
	if len(repos) != 1 || repos[0].AutoMerge {
		t.Fatalf("repo card AutoMerge = %+v, want false", repos)
	}
	reloaded, err := config.Load(srv.deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("Load persisted config: %v", err)
	}
	if reloaded.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo auto-merge disable was not persisted")
	}

	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	w = postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("enable auto-merge: code = %d body = %s", w.Code, w.Body.String())
	}
	if !srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo auto-merge not re-enabled")
	}
}

func TestRepoAutoMergeEnableRejectedBelowL6(t *testing.T) {
	srv := newFullServer(t)
	disabled := false
	if _, err := srv.deps.Config.SetRepoAutoMergeForRepoAndSave("testrepo", &disabled); err != nil {
		t.Fatal(err)
	}

	w := postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":true}`, true)
	if w.Code != http.StatusConflict {
		t.Fatalf("enable below L6: code = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "auto-merge requires autonomy level 6") {
		t.Fatalf("enable below L6 body = %s, want clear L6 message", w.Body.String())
	}
	if srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo auto-merge became effective below L6")
	}
}

// #9070: the gate is asymmetric. A repo-write user (here: the GitHub user who
// owns the repo namespace, which canToggleRepoHold accepts without a GitHub
// lookup) may switch auto-merge OFF, but switching it back ON restores Hive's
// merge authority and stays owner-only.
func TestRepoAutoMergeEnableRequiresVerifiedOwner(t *testing.T) {
	srv := newFullServer(t)
	postAsRepoWriter := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/repos/auto-merge", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-User", "testorg")
		w := httptest.NewRecorder()
		srv.mux.ServeHTTP(w, req)
		return w
	}

	w := postAsRepoWriter(`{"repo":"testrepo","enabled":false}`)
	if w.Code != http.StatusOK {
		t.Fatalf("repo-write disable: code = %d body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo auto-merge still enabled after repo-write user disabled it")
	}

	w = postAsRepoWriter(`{"repo":"testrepo","enabled":true}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("repo-write enable: code = %d, want 403; body = %s", w.Code, w.Body.String())
	}
	if srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("repo-write user re-enabled auto-merge despite 403")
	}

	// A spoofed owner role without the server-only verification marker is
	// still not an owner.
	req := httptest.NewRequest(http.MethodPost, "/api/repos/auto-merge", strings.NewReader(`{"repo":"testrepo","enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set("X-Hive-User", "testorg")
	w = httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified owner enable: code = %d, want 403", w.Code)
	}

	level := config.SelfMergeMinACMMLevel
	srv.deps.Config.ACMMLevel = &level
	w = postRepoPause(t, srv, "/api/repos/auto-merge", `{"repo":"testrepo","enabled":true}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("verified owner enable: code = %d body = %s", w.Code, w.Body.String())
	}
	if !srv.deps.Config.RepoAutoMergeEnabled("testrepo") {
		t.Fatal("verified owner could not re-enable auto-merge")
	}
}

// changed=false distinguishes a real transition from a no-op, so a dashboard
// with a stale belief cannot silently re-pause a repo the operator was trying
// to resume — and re-pausing must not rewrite the original provenance.
func TestRepoPause_NoOpDoesNotRestampProvenance(t *testing.T) {
	srv := newFullServer(t)

	if w := postRepoPause(t, srv, "/api/repos/pause", `{"repo":"testrepo","reason":"first"}`, true); w.Code != http.StatusOK {
		t.Fatalf("first pause: %d %s", w.Code, w.Body.String())
	}
	w := postRepoPause(t, srv, "/api/repos/pause", `{"repo":"testrepo","reason":"second"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("second pause: %d %s", w.Code, w.Body.String())
	}
	body := decodeRepoPauseBody(t, w)
	if body["changed"] != false {
		t.Errorf("re-pause reported changed=%v, want false", body["changed"])
	}
	pause, _ := body["pause"].(map[string]any)
	if pause["reason"] != "first" {
		t.Errorf("re-pause rewrote the reason to %v; the original must survive", pause["reason"])
	}

	// Resuming a repo that is not paused is likewise a reported no-op.
	if _, err := srv.deps.Config.SetRepoPausedAndSave("testrepo", false, "local", ""); err != nil {
		t.Fatalf("resume for setup: %v", err)
	}
	w = postRepoPause(t, srv, "/api/repos/resume", `{"repo":"testrepo"}`, true)
	if body := decodeRepoPauseBody(t, w); body["changed"] != false || body["state"] != "running" {
		t.Errorf("no-op resume = %v, want changed=false state=running", body)
	}
}

// Pausing a repo the hive does not watch would write an entry that matches
// nothing and report success — the operator would believe a repo was quiet when
// no enforcement point had ever heard of it.
func TestRepoPause_RejectsUnwatchedRepo(t *testing.T) {
	srv := newFullServer(t)
	w := postRepoPause(t, srv, "/api/repos/pause", `{"repo":"not-in-config"}`, true)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("code = %d, want 400 for a repo outside project.repos (body %s)", w.Code, w.Body.String())
	}
	if srv.deps.Config.IsRepoPaused("not-in-config") {
		t.Error("an unwatched repo was recorded as paused anyway")
	}
}

// Resume is deliberately laxer: an entry left behind by a repo removed while
// paused must still be clearable without hand-editing the config file.
func TestRepoResume_ClearsPauseForUnwatchedRepo(t *testing.T) {
	srv := newFullServer(t)
	srv.deps.Config.Project.PausedRepos = []config.RepoPause{{Repo: "departed-repo"}}

	w := postRepoPause(t, srv, "/api/repos/resume", `{"repo":"departed-repo"}`, true)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	if srv.deps.Config.IsRepoPaused("departed-repo") {
		t.Error("stale pause entry not cleared")
	}
}

func TestRepoPause_RequiresOwner(t *testing.T) {
	srv := newFullServer(t)
	for _, path := range []string{"/api/repos/pause", "/api/repos/resume"} {
		w := postRepoPause(t, srv, path, `{"repo":"testrepo"}`, false)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s without owner role: code = %d, want 403", path, w.Code)
		}
	}
	if srv.deps.Config.IsRepoPaused("testrepo") {
		t.Error("a non-owner request changed the pause state")
	}
}

func TestRepoPause_RejectsMissingRepo(t *testing.T) {
	srv := newFullServer(t)
	if w := postRepoPause(t, srv, "/api/repos/pause", `{"reason":"no repo"}`, true); w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for a body with no repo", w.Code)
	}
	if w := postRepoPause(t, srv, "/api/repos/pause", `not json`, true); w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for an unparsable body", w.Code)
	}
}

// The listing is read-only, so any authenticated role may call it — the same
// state is already on the repository cards.
func TestRepoPausesListing(t *testing.T) {
	srv := newFullServer(t)
	if _, err := srv.deps.Config.SetRepoPausedAndSave("testrepo", true, "bketelsen", "incident"); err != nil {
		t.Fatalf("pause: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/repos/pauses", nil)
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}
	body := decodeRepoPauseBody(t, w)
	pauses, _ := body["pauses"].([]any)
	if len(pauses) != 1 {
		t.Fatalf("pauses = %v, want exactly one", pauses)
	}
	entry, _ := pauses[0].(map[string]any)
	if entry["repo"] != "testrepo" || entry["by"] != "bketelsen" || entry["reason"] != "incident" {
		t.Errorf("listing lost the provenance: %v", entry)
	}
	if entry["paused"] != true {
		t.Errorf("listing entry not marked paused: %v", entry)
	}
}

// A hive that never uses the feature must serve exactly what it served before.
func TestRepoCards_UnpausedCarryNoPauseFields(t *testing.T) {
	srv := newFullServer(t)
	repos := buildRepos(srv.deps.Config, nil, governor.State{})
	if len(repos) != 1 {
		t.Fatalf("want 1 repo card, got %d", len(repos))
	}
	if repos[0].Paused || repos[0].PausedBy != "" || repos[0].PausedAt != "" || repos[0].PauseReason != "" {
		t.Errorf("unpaused card carries pause fields: %+v", repos[0])
	}
	raw, err := json.Marshal(repos[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "paused") {
		t.Errorf("pause fields are not omitempty in the status payload: %s", raw)
	}
}
