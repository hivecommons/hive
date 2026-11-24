package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
	"gopkg.in/yaml.v3"
)

// The Settings textarea and the Projects-card selector both write
// writeback.per_repo through the Features PUT; this is the one source they
// share, and the status payload both repaint from.
func TestGovernorFeatures_WritebackPerRepoRoundTrip(t *testing.T) {
	s := covApiServer(t)
	for _, bad := range []map[string]any{
		{"writebackPerRepo": map[string]string{"o/r": "sometimes"}},
		{"writebackMinLevel": 9},
		{"writebackKinds": map[string]any{"nope": map[string]any{"min_level": 5}}},
		{"writebackKinds": map[string]any{"sentinel": map[string]any{"min_level": 7}}},
	} {
		if rec := doPut(s, "/api/config/governor/features", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("PUT %v = %d, want 400: %s", bad, rec.Code, rec.Body.String())
		}
	}
	rec := doPut(s, "/api/config/governor/features", map[string]any{
		"writebackPerRepo":       map[string]string{"torch-spyre/torch-spyre": "Issues-Only", "o/r": ""},
		"writebackMinLevel":      6,
		"writebackRequireLabels": false,
		"writebackBackfill":      true,
		"writebackKinds":         map[string]any{"sentinel": map[string]any{"min_level": 4, "require_label": false}, "stage_label": nil},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d - %s", rec.Code, rec.Body.String())
	}
	wb := s.deps.Config.Writeback
	if wb.RepoModeFor("torch-spyre/torch-spyre") != writeback.RepoModeIssuesOnly || wb.RepoModeFor("o/r") != writeback.RepoModeDefault {
		t.Fatalf("per_repo not persisted: %+v", wb.PerRepo)
	}
	if wb.MinLevelOrDefault() != 6 || wb.RequireLabelsEnabled() || !wb.BackfillEnabled() {
		t.Fatalf("unsolicited knobs not persisted: %+v", wb.Unsolicited)
	}
	if p := wb.Policy(writeback.KindSentinel); p.MinLevel != 6 || p.RequireLabel {
		// the global floor (6) raises the per-kind 4; require_labels:false clears the label.
		t.Fatalf("sentinel policy = %+v", p)
	}
	if _, ok := wb.Kinds["stage_label"]; ok {
		t.Fatal("null kind override should clear, not persist")
	}

	// Setting min_level back to 0 clears the global floor; the Settings editor
	// still replaces the whole map, so an empty map clears per_repo.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"writebackMinLevel": 0, "writebackPerRepo": map[string]string{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT clear = %d - %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Writeback.Unsolicited.MinLevel != nil || s.deps.Config.Writeback.PerRepo != nil {
		t.Fatalf("clear did not reset: %+v", s.deps.Config.Writeback)
	}
}

// Each card sends only its selected repository, even before status refreshes.
func TestGovernorFeatures_WritebackRepoUpdates(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	cfg.Project.Org = "org"
	cfg.Project.Repos = []string{"first", "second"}
	put := func(repo, mode string) int {
		return doPut(s, "/api/config/governor/features", map[string]any{
			"writebackRepo": map[string]string{"repo": repo, "mode": mode},
		}).Code
	}
	for _, repo := range []string{"Org/First", "org/second"} {
		if code := put(repo, "off"); code != http.StatusOK {
			t.Fatalf("save %s = %d", repo, code)
		}
	}
	if code := put(" org/FIRST ", " Full "); code != http.StatusOK {
		t.Fatalf("update first = %d", code)
	}
	if wb := s.deps.Config.Writeback; len(wb.PerRepo) != 2 || wb.RepoModeFor("org/first") != writeback.RepoModeFull || wb.RepoModeFor("org/second") != writeback.RepoModeOff {
		t.Fatalf("second card or case-insensitive update lost a mode: %+v", wb.PerRepo)
	}
	if code := put("ORG/FIRST", ""); code != http.StatusOK {
		t.Fatalf("clear first = %d", code)
	}
	if wb := s.deps.Config.Writeback; len(wb.PerRepo) != 1 || wb.RepoModeFor("org/second") != writeback.RepoModeOff {
		t.Fatalf("clear first changed another repository: %+v", wb.PerRepo)
	}

	// The bare override must keep protecting other owners with the same repo name.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"writebackPerRepo": map[string]string{"FIRST": "off", "org/second": "off"},
	}); rec.Code != http.StatusOK {
		t.Fatalf("seed legacy config = %d %s", rec.Code, rec.Body.String())
	}
	if code := put("first", "full"); code != http.StatusOK {
		t.Fatalf("update bare alias = %d", code)
	}
	if wb := cfg.WritebackPolicy(); wb.RepoModeFor("first") != writeback.RepoModeFull || wb.RepoModeFor("other/first") != writeback.RepoModeOff {
		t.Fatalf("save did not isolate the mode: %+v", wb.PerRepo)
	}
	if code := put("org/first", ""); code != http.StatusOK {
		t.Fatalf("select default = %d", code)
	}
	wb := cfg.WritebackPolicy()
	if wb.RepoModeFor("org/first") != writeback.RepoModeDefault || wb.RepoModeFor("other/first") != writeback.RepoModeOff {
		t.Fatalf("default did not shadow the bare override for just this repo: %+v", wb.PerRepo)
	}
	perRepo := writebackPerRepoResponse(wb)
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"writebackPerRepo": perRepo}); rec.Code != http.StatusOK {
		t.Fatalf("round-trip Settings map = %d %s", rec.Code, rec.Body.String())
	}
	if got := cfg.WritebackPolicy(); !maps.Equal(got.PerRepo, wb.PerRepo) {
		t.Fatalf("Settings round-trip changed the overrides: %+v", got.PerRepo)
	}

	for _, bad := range []map[string]any{
		{"writebackRepo": map[string]string{"repo": "", "mode": "off"}},
		{"writebackRepo": map[string]string{"repo": "org/second"}},
		{"writebackRepo": map[string]string{"repo": "org/second", "mode": "sometimes"}},
		{"writebackRepo": map[string]string{"repo": "org/second", "mode": ""}, "writebackPerRepo": map[string]string{}},
	} {
		if rec := doPut(s, "/api/config/governor/features", bad); rec.Code != http.StatusBadRequest {
			t.Fatalf("invalid update %v = %d", bad, rec.Code)
		}
	}
	if s.deps.Config.Writeback.RepoModeFor("org/second") != writeback.RepoModeOff {
		t.Fatal("invalid request mutated the current map")
	}

	const count = 12
	s.SetWritebackGate(writeback.New(cfg.WritebackPolicy(), cfg.WritebackGateDeps(), ""))
	s.UpdateStatus(&StatusPayload{Repos: []FrontendRepo{{Name: "first", Full: "org/first"}}})
	start := make(chan struct{})
	results := make(chan int, count)
	pollResult := make(chan int, 1)
	go func() {
		<-start
		for range count {
			if code := doGet(s, "/api/status").Code; code != http.StatusOK {
				pollResult <- code
				return
			}
		}
		pollResult <- http.StatusOK
	}()
	for i := 0; i < count; i++ {
		go func(i int) {
			<-start
			results <- put(fmt.Sprintf("org/concurrent-%d", i), "labels-only")
		}(i)
	}
	close(start)
	for i := 0; i < count; i++ {
		if code := <-results; code != http.StatusOK {
			t.Errorf("concurrent update = %d", code)
		}
	}
	if code := <-pollResult; code != http.StatusOK {
		t.Errorf("poll status during card saves = %d", code)
	}
	wb = cfg.WritebackPolicy()
	if len(wb.PerRepo) != count+3 {
		t.Fatalf("concurrent card saves lost a mode: %+v", wb.PerRepo)
	}
	data, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	var persisted struct {
		Writeback writeback.Config `yaml:"writeback"`
	}
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatal(err)
	}
	if !maps.Equal(persisted.Writeback.PerRepo, wb.PerRepo) {
		t.Fatalf("saved config has different overrides: %+v", persisted.Writeback.PerRepo)
	}
}

func TestGovernorFeatures_WritebackRepoSaveConflict(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	cfg.Writeback.PerRepo = map[string]writeback.RepoOverride{
		"myorg/repo1": {Mode: writeback.RepoModeOff},
	}
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(cfg.SourcePath)
	if err != nil {
		t.Fatal(err)
	}
	external := append(data, []byte("\n# edited outside the dashboard\n")...)
	if err := os.WriteFile(cfg.SourcePath, external, 0o600); err != nil {
		t.Fatal(err)
	}

	rec := doPut(s, "/api/config/governor/features", map[string]any{
		"writebackRepo": map[string]string{"repo": "myorg/repo1", "mode": "full"},
	})
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "changed on disk") {
		t.Fatalf("save after external edit = %d %s", rec.Code, rec.Body.String())
	}
	if got := cfg.WritebackPolicy().RepoModeFor("myorg/repo1"); got != writeback.RepoModeOff {
		t.Fatalf("failed save changed the live mode to %q", got)
	}
	got, err := os.ReadFile(cfg.SourcePath)
	if err != nil || !bytes.Equal(got, external) {
		t.Fatalf("external edit did not survive the failed save: %v", err)
	}
}

func TestWritebackStatusForBareAndUnobservedRepos(t *testing.T) {
	s := covApiServer(t)
	cfg := s.deps.Config
	cfg.SourcePath = filepath.Join(t.TempDir(), "hive.yaml")
	cfg.Project.Org = "org"
	cfg.Project.Repos = []string{"first", "second"}
	cfg.Writeback.PerRepo = map[string]writeback.RepoOverride{
		"other/first": {Mode: writeback.RepoModeFull},
	}
	s.UpdateStatus(&StatusPayload{Repos: []FrontendRepo{
		{Name: "first", Full: "org/first"},
		{Name: "second", Full: "org/second"},
	}})
	g := writeback.New(cfg.WritebackPolicy(), cfg.WritebackGateDeps(), filepath.Join(t.TempDir(), "writeback.json"))
	g.ObserveRepo("first")
	s.SetWritebackGate(g)
	initial := g.Snapshot().Repos["first"]
	assertMode := func(repo string, want writeback.RepoMode) {
		t.Helper()
		rec := doGet(s, "/api/status")
		var payload struct {
			Writeback *writeback.Status `json:"writeback"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &payload) != nil || payload.Writeback == nil {
			t.Fatalf("status = %d %s", rec.Code, rec.Body.String())
		}
		rs, ok := payload.Writeback.Repos[repo]
		if !ok || rs.Mode != want {
			t.Fatalf("%s = %+v (present %v), want mode %q", repo, rs, ok, want)
		}
		if repo == "org/first" && (!rs.FirstSeen.Equal(initial.FirstSeen) || rs.Writable != initial.Writable || rs.PreExisting != initial.PreExisting) {
			t.Fatalf("display changed ledger history: %+v, initial %+v", rs, initial)
		}
	}
	assertMode("org/first", writeback.RepoModeDefault)
	for _, update := range []struct{ repo, mode string }{{"org/first", "off"}, {"org/second", "labels-only"}} {
		if rec := doPut(s, "/api/config/governor/features", map[string]any{
			"writebackRepo": map[string]string{"repo": update.repo, "mode": update.mode},
		}); rec.Code != http.StatusOK {
			t.Fatalf("save = %d %s", rec.Code, rec.Body.String())
		}
	}
	assertMode("org/first", writeback.RepoModeOff)
	assertMode("org/second", writeback.RepoModeLabelsOnly)
	if rec := doPut(s, "/api/config/governor/features", map[string]any{
		"writebackRepo": map[string]string{"repo": "org/first", "mode": ""},
	}); rec.Code != http.StatusOK {
		t.Fatalf("clear = %d %s", rec.Code, rec.Body.String())
	}
	assertMode("org/first", writeback.RepoModeDefault)
	assertMode("org/second", writeback.RepoModeLabelsOnly)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Read the initial SSE frame without waiting for another update.
	stream := httptest.NewRecorder()
	s.mux.ServeHTTP(stream, httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil))
	_, frame, ok := strings.Cut(stream.Body.String(), "data: ")
	var streamed struct {
		Writeback *writeback.Status `json:"writeback"`
	}
	if !ok || json.Unmarshal([]byte(frame), &streamed) != nil || streamed.Writeback == nil ||
		streamed.Writeback.Repos["org/second"].Mode != writeback.RepoModeLabelsOnly {
		t.Fatalf("initial status stream lost the saved mode: %s", stream.Body.String())
	}
	if len(g.Snapshot().Repos) != 1 {
		t.Fatal("status display must not add entries to the gate ledger")
	}
}

func TestWritebackStatusSuppressedAndSentinelEndpoints(t *testing.T) {
	s := covApiServer(t)
	// Without a gate the endpoints degrade, never 500.
	if rec := doGet(s, "/api/writeback/suppressed"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"enabled":false`) {
		t.Fatalf("no-gate suppressed = %d %s", rec.Code, rec.Body.String())
	}
	if rec := doGet(s, "/api/writeback/suppressed?limit=0"); rec.Code != http.StatusBadRequest {
		t.Fatalf("limit=0 = %d", rec.Code)
	}

	now := time.Now()
	g := writeback.New(writeback.Config{}, writeback.Deps{
		LevelFor:    func(string) int { return 4 },
		OptInLabels: func(string) []string { return []string{"hive-review"} },
	}, t.TempDir()+"/wb.json")
	g.ObserveRepo("torch-spyre/torch-spyre")
	s.SetWritebackGate(g)
	if d := g.AllowedPreview("torch-spyre/torch-spyre", writeback.Item{Number: 7, IsPR: true, CreatedAt: now.Add(-time.Hour)}, writeback.KindSentinel, "Sentinel alert"); d.Allowed {
		t.Fatalf("expected suppression: %+v", d)
	}
	g.Allowed("o/other", writeback.Item{Number: 8, IsPR: true, CreatedAt: now.Add(time.Minute)}, writeback.KindStageLabel)

	rec := doGet(s, "/api/writeback/suppressed?repo=torch-spyre/torch-spyre")
	var sup struct {
		Enabled bool                   `json:"enabled"`
		Total   int                    `json:"total_24h"`
		Items   []writeback.Suppressed `json:"items"`
		Status  *writeback.Status      `json:"status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sup); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("suppressed = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if !sup.Enabled || sup.Total != 2 || len(sup.Items) != 1 || sup.Items[0].Number != 7 || sup.Items[0].Preview != "Sentinel alert" {
		t.Fatalf("suppressed payload = %+v", sup)
	}
	if sup.Status == nil || sup.Status.Repos["torch-spyre/torch-spyre"].PreExisting != 1 || len(sup.Status.Banners) != 1 {
		t.Fatalf("status block = %+v", sup.Status)
	}
	if rec := doGet(s, "/api/writeback/suppressed?kind=stage_label"); !strings.Contains(rec.Body.String(), `"number":8`) || strings.Contains(rec.Body.String(), `"number":7`) {
		t.Fatalf("kind filter: %s", rec.Body.String())
	}

	// /api/status carries the same block.
	s.UpdateStatus(&StatusPayload{})
	if rec := doGet(s, "/api/status"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"writeback":{`) || !strings.Contains(rec.Body.String(), `"pre_existing_untouched":1`) {
		t.Fatalf("status = %d missing writeback block", rec.Code)
	}

	// Sentinel findings: suppressed ones are kept and filterable; the same
	// repo#number@head is deduplicated.
	ev := github.SentinelSweepEvent{Repo: "torch-spyre/torch-spyre", Number: 7, Author: "m", HeadSHA: "abc", Suppressed: true, SuppressedReason: writeback.ReasonBackfill}
	s.RecordSentinelFinding(ev)
	s.RecordSentinelFinding(ev)
	s.RecordSentinelFinding(github.SentinelSweepEvent{Repo: "o/other", Number: 9, Author: "x", HeadSHA: "def", Blocked: true})
	rec = doGet(s, "/api/sentinel?suppressed=true")
	var sf struct {
		Total      int               `json:"total"`
		Suppressed int               `json:"suppressed"`
		Findings   []SentinelFinding `json:"findings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &sf); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("sentinel = %d %s (%v)", rec.Code, rec.Body.String(), err)
	}
	if sf.Total != 2 || sf.Suppressed != 1 || len(sf.Findings) != 1 || sf.Findings[0].Number != 7 || !sf.Findings[0].Suppressed {
		t.Fatalf("sentinel payload = %+v", sf)
	}
	if rec := doGet(s, "/api/sentinel?repo=o/other&suppressed=false"); !strings.Contains(rec.Body.String(), `"number":9`) || strings.Contains(rec.Body.String(), `"number":7`) {
		t.Fatalf("sentinel filters: %s", rec.Body.String())
	}
}

// TestWritebackDashboardMarkup pins the two views of writeback.per_repo —
// the Settings textarea and the Projects-card selector + chip — and the
// Security-tab findings panel, all in index.html.
func TestWritebackDashboardMarkup(t *testing.T) {
	html := indexHTML(t)
	for _, snippet := range []string{
		// Settings → Features → Write-back containment
		`${renderWritebackContainmentSection(f, platformDefaults)}`,
		`id="writeback-containment"`,
		`data-arg1="writebackMinLevel"`,
		`data-key="writebackRequireLabels"`,
		`data-key="writebackBackfill"`,
		`data-change-action="markDirtyWritebackPerRepo"`,
		`id="wb-kind-table"`,
		`fetch('/api/writeback/suppressed?limit=100')`,
		`id="writeback-banners"`,
		// Projects card: chip + selector, rendered in the header actions
		`const writebackCtl = repoWritebackControlHtml(cardRepo, status);`,
		`<span class="repo-header-actions">${writebackCtl}${swarmBtn}${pauseBtn}</span>`,
		`class="repo-writeback-chip" data-mode="${esc(mode || 'default')}"`,
		`writes: ${esc(mode || 'default')}`,
		`data-change-action="setRepoWritebackMode" data-arg0="${esc(repo)}" data-arg-types="s,v,t"`,
		// Card updates target one repository through the same PUT.
		`body: JSON.stringify({ writebackRepo: { repo, mode } })`,
		`markDirty('features', 'writebackPerRepo', out);`,
		// Security tab findings
		`id="sentinel-findings-host"`,
		`fetch('/api/sentinel')`,
		// Search index entry
		`key: 'writeback.unsolicited.min_level'`,
	} {
		if !strings.Contains(html, snippet) {
			t.Errorf("index.html is missing %q", snippet)
		}
	}
	for _, mode := range writeback.RepoModes() {
		if !strings.Contains(html, "'"+string(mode)+"'") {
			t.Errorf("repo mode %q is not offered by the card selector", mode)
		}
	}
}
