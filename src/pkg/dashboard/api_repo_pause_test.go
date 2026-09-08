package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	repos := buildRepos(srv.deps.Config, nil)
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
	if repos := buildRepos(srv.deps.Config, nil); repos[0].Paused {
		t.Error("repo card still shows paused after resume")
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
	repos := buildRepos(srv.deps.Config, nil)
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
