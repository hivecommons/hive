package dashboard

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/writeback"
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

	// Setting min_level back to 0 clears the global floor; the Projects card
	// sends the whole map, so an empty map clears per_repo.
	if rec := doPut(s, "/api/config/governor/features", map[string]any{"writebackMinLevel": 0, "writebackPerRepo": map[string]string{}}); rec.Code != http.StatusOK {
		t.Fatalf("PUT clear = %d - %s", rec.Code, rec.Body.String())
	}
	if s.deps.Config.Writeback.Unsolicited.MinLevel != nil || s.deps.Config.Writeback.PerRepo != nil {
		t.Fatalf("clear did not reset: %+v", s.deps.Config.Writeback)
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
		// Both views write the same map through the same PUT.
		`body: JSON.stringify({ writebackPerRepo: perRepo })`,
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
