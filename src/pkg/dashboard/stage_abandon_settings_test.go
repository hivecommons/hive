package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// governor.labels.stage.abandon_after and abandon_close_repos are edited on
// the Labels tab and saved through PUT /api/config/governor/labels.
func TestHandleGovernorLabels_AbandonRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	stage := func() (string, []string) {
		st := srv.deps.Config.Governor.Labels.Stage
		return st.AbandonAfter, st.AbandonCloseRepos
	}
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", body))
		return w
	}

	if w := put(`{"stage_abandon_after":" 168h ","stage_abandon_close_repos":["testrepo"]}`); w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	after, repos := stage()
	if after != "168h" || !reflect.DeepEqual(repos, []string{"testorg/testrepo"}) {
		t.Fatalf("saved abandon_after=%q repos=%v, want 168h [testorg/testrepo]", after, repos)
	}
	got := decodeGovernorConfigGet(t, srv)
	if got["stageAbandonAfter"] != "168h" {
		t.Fatalf("GET stageAbandonAfter = %v", got["stageAbandonAfter"])
	}
	if list, ok := got["stageAbandonCloseRepos"].([]any); !ok || len(list) != 1 || list[0] != "testorg/testrepo" {
		t.Fatalf("GET stageAbandonCloseRepos = %#v", got["stageAbandonCloseRepos"])
	}

	// A save that touches neither key leaves both alone.
	if w := put(`{"stage_nudge_ci_failure_cooldown":"2h"}`); w.Code != http.StatusOK {
		t.Fatalf("unrelated save: code = %d", w.Code)
	}
	if after, repos = stage(); after != "168h" || len(repos) != 1 {
		t.Fatalf("unrelated save changed abandon_after=%q repos=%v", after, repos)
	}

	for _, bad := range []string{
		`{"stage_abandon_after":"soon"}`,
		`{"stage_abandon_after":"-1h"}`,
		`{"stage_abandon_close_repos":["elsewhere/unwatched"]}`,
	} {
		if w := put(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400; body = %s", bad, w.Code, w.Body.String())
		}
	}
	if after, repos = stage(); after != "168h" || len(repos) != 1 {
		t.Fatalf("rejected saves mutated abandon_after=%q repos=%v", after, repos)
	}

	// "0" (or "") turns the idle clock off; an empty list clears the opt-in.
	if w := put(`{"stage_abandon_after":"0","stage_abandon_close_repos":[]}`); w.Code != http.StatusOK {
		t.Fatalf("clear: code = %d, body = %s", w.Code, w.Body.String())
	}
	if after, repos = stage(); after != "" || len(repos) != 0 {
		t.Fatalf("clear left abandon_after=%q repos=%v", after, repos)
	}
	if srv.deps.Config.Governor.Labels.Stage.EffectiveAbandonAfter() != 0 {
		t.Fatal("abandon_after 0 must read as off")
	}
}

// governor.labels.stage.abandon_after_trusted is saved through the same
// endpoint, never below abandon_after.
func TestHandleGovernorLabels_AbandonTrustedRoundTrip(t *testing.T) {
	srv := newFullServer(t)
	put := func(body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		srv.handleGovernorLabels(w, ownerPUT(t, "/api/config/governor/labels", body))
		return w
	}
	trusted := func() string { return srv.deps.Config.Governor.Labels.Stage.AbandonAfterTrusted }

	if got := decodeGovernorConfigGet(t, srv); got["stageAbandonAfterTrusted"] != "" {
		t.Fatalf("GET stageAbandonAfterTrusted = %#v, want \"\" (off)", got["stageAbandonAfterTrusted"])
	}
	if w := put(`{"stage_abandon_after":"48h","stage_abandon_after_trusted":" 168h "}`); w.Code != http.StatusOK {
		t.Fatalf("code = %d, body = %s", w.Code, w.Body.String())
	}
	if trusted() != "168h" || srv.deps.Config.Governor.Labels.Stage.EffectiveAbandonAfterTrusted() != 168*time.Hour {
		t.Fatalf("saved abandon_after_trusted = %q", trusted())
	}
	if got := decodeGovernorConfigGet(t, srv); got["stageAbandonAfterTrusted"] != "168h" {
		t.Fatalf("GET stageAbandonAfterTrusted = %v", got["stageAbandonAfterTrusted"])
	}

	for _, bad := range []string{
		`{"stage_abandon_after_trusted":"soon"}`,
		`{"stage_abandon_after_trusted":"-1h"}`,
		`{"stage_abandon_after_trusted":"24h"}`,
		`{"stage_abandon_after":"336h"}`,
		`{"stage_abandon_after":"336h","stage_abandon_after_trusted":"200h"}`,
	} {
		if w := put(bad); w.Code != http.StatusBadRequest {
			t.Fatalf("%s: code = %d, want 400; body = %s", bad, w.Code, w.Body.String())
		}
	}
	if trusted() != "168h" || srv.deps.Config.Governor.Labels.Stage.AbandonAfter != "48h" {
		t.Fatalf("rejected saves mutated abandon_after=%q abandon_after_trusted=%q", srv.deps.Config.Governor.Labels.Stage.AbandonAfter, trusted())
	}

	// Equal to abandon_after is allowed; "0" turns it off; abandon_after
	// may then rise freely.
	if w := put(`{"stage_abandon_after_trusted":"48h"}`); w.Code != http.StatusOK {
		t.Fatalf("equal: code = %d, body = %s", w.Code, w.Body.String())
	}
	if w := put(`{"stage_abandon_after_trusted":"0"}`); w.Code != http.StatusOK || trusted() != "" {
		t.Fatalf("off: code = %d, saved %q", w.Code, trusted())
	}
	if w := put(`{"stage_abandon_after":"336h"}`); w.Code != http.StatusOK {
		t.Fatalf("raise with the trusted timer off: code = %d, body = %s", w.Code, w.Body.String())
	}
}

// The Labels tab carries both controls, with their YAML keys, and the
// settings search finds them.
func TestLabelsTabHasAbandonControls(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("static-src", "js", "config-governor.js"))
	if err != nil {
		t.Fatal(err)
	}
	js := string(src)
	for _, want := range []string{
		`data-arg1="stage_abandon_after"`,
		`data-arg1="stage_abandon_after_trusted"`,
		`yamlHint('governor.labels.stage.abandon_after_trusted')`,
		`yamlHint('governor.labels.stage.abandon_after')`,
		`markDirtyStageAbandonCloseRepos`,
		`yamlHint('governor.labels.stage.abandon_close_repos[]')`,
		`stageAbandonAfter`,
		`stageAbandonCloseRepos`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("config-governor.js lacks %s", want)
		}
	}
	html := indexHTML(t)
	if !strings.Contains(html, "function markDirtyStageAbandonCloseRepos(") {
		t.Error("index.html lacks markDirtyStageAbandonCloseRepos; regenerate it from static-src")
	}
}
