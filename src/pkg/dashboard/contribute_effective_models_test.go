package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func TestAggregateContributeEffectiveModelsThresholdRankingAndPrivacy(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	prs := []ghpkg.PullRequest{
		{Repo: "org/repo", Number: 1, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		{Repo: "org/repo", Number: 2, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{ReviewRounds: 2, FixAttempts: 1}},
		{Repo: "org/repo", Number: 3, HiveAttributed: true, HiveModel: "sonnet", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		{Repo: "org/repo", Number: 4, HiveAttributed: true, HiveModel: "sonnet", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
	}
	runs := []TaskRunRecord{
		{TS: now.Format(time.RFC3339), Username: "alice", Backend: "copilot", Model: "opus", Outcome: outcomeCompleted, PRVerified: true, PRURL: "https://github.com/org/repo/pull/1"},
		{TS: now.Format(time.RFC3339), Username: "alice", Backend: "copilot", Model: "opus", Outcome: outcomeCompleted},
		{TS: now.Format(time.RFC3339), Username: "bob", Backend: "copilot", Model: "opus", Outcome: outcomeFailed},
	}
	got := aggregateContributeEffectiveModels(prs, runs, "7d", "all", 2, now)
	if len(got.Ranked) != 2 {
		t.Fatalf("ranked = %d, want 2: %+v", len(got.Ranked), got.Ranked)
	}
	if got.Ranked[0].Model != "sonnet" || got.Ranked[0].FirstPassMergeRate != 1 {
		t.Fatalf("top row = %+v, want sonnet sorted by first-pass rate", got.Ranked[0])
	}
	if got.Ranked[0].EffectivenessRank != 1 || got.Ranked[1].EffectivenessRank != 2 {
		t.Fatalf("ranks = %d/%d, want 1/2", got.Ranked[0].EffectivenessRank, got.Ranked[1].EffectivenessRank)
	}
	opus := got.Ranked[1]
	if opus.RunCount != 3 || opus.VerifiedPRRuns != 1 || opus.NothingToShipRuns != 1 || opus.FailedRuns != 1 {
		t.Fatalf("opus run stats = %+v", opus)
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "alice") || strings.Contains(string(body), "bob") {
		t.Fatalf("effective model response leaked username: %s", body)
	}

	contrib := aggregateContributeEffectiveModels(prs, runs, "7d", "contributor", 2, now)
	if len(contrib.Ranked) != 0 || len(contrib.Insufficient) != 1 || contrib.Insufficient[0].PRs != 1 {
		t.Fatalf("contributor filter = ranked %+v insufficient %+v, want only PR matched from run log", contrib.Ranked, contrib.Insufficient)
	}
}

func TestHandleContributeEffectiveModelsPublicAggregate(t *testing.T) {
	s := covApiServer(t)
	now := time.Now()
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		PRs: ghpkg.PRResult{Attributed: []ghpkg.PullRequest{
			{Repo: "org/repo", Number: 1, HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true}},
		}},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/effective-models?window=all&min_merged_prs=1", nil)
	rec := httptest.NewRecorder()
	s.handleContributeEffectiveModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body %s", rec.Code, rec.Body.String())
	}
	if body := rec.Body.String(); !strings.Contains(body, `"model":"opus"`) || strings.Contains(body, "username") {
		t.Fatalf("response body = %s", body)
	}
}

func TestContributeOperationsRendersEffectiveModelsPanel(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		"Most effective models",
		"/api/contribute/effective-models",
		"effective-models-ranked",
		"Not enough data yet",
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("contribute page missing %q", want)
		}
	}
}

func TestContributeOperationsEffectiveModelsPanelCollapsiblePersists(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		`id="effective-models-toggle" aria-expanded="true" aria-controls="effective-models-body"`,
		`data-ops-section="effective-models-card"`,
		`<span class="section-chevron" aria-hidden="true">▼</span>`,
		`<div class="section-body" id="effective-models-body">`,
		`.section-body{overflow:visible;transition:opacity 200ms ease;max-height:none;opacity:1}`,
		`.section-body.collapsed{max-height:0!important;overflow:hidden;opacity:0;pointer-events:none}`,
		`hive-section-collapsed-`,
		`function initOpsCollapsiblePanels`,
		`localStorage.getItem(OPS_SECTION_LS_PREFIX+sectionId)`,
		`localStorage.setItem(OPS_SECTION_LS_PREFIX+sectionId,'1')`,
		`localStorage.removeItem(OPS_SECTION_LS_PREFIX+sectionId)`,
		`ccApplySectionCollapse(sectionId);`,
		`try{initOpsCollapsiblePanels();}catch`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("effective models collapsible panel missing %q", want)
		}
	}

	bodyStart := strings.Index(page, `id="effective-models-body"`)
	controls := strings.Index(page, `class="effective-controls"`)
	tables := strings.Index(page, `id="effective-models-ranked"`)
	nextCard := strings.Index(page, `<!-- Fleet work`)
	if bodyStart < 0 || controls < bodyStart || tables < controls || nextCard < tables {
		t.Fatalf("effective model controls/tables are not nested under the collapsible body (body=%d controls=%d tables=%d next=%d)", bodyStart, controls, tables, nextCard)
	}
	if strings.Contains(page, `.section-body{overflow:hidden`) || strings.Contains(page, `max-height:5000px`) {
		t.Fatal("expanded effective models body must not clip long ranked/insufficient tables")
	}
}
