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

func TestAggregateContributeEffectiveModelsContributorFilterShortRepoNames(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	prs := []ghpkg.PullRequest{
		{Repo: "documentation", Number: 7, URL: "https://github.com/projectbluefin/documentation/pull/7", HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now},
		{Repo: "projectbluefin/bluefin", Number: 8, URL: "https://github.com/projectbluefin/bluefin/pull/8", HiveAttributed: true, HiveModel: "opus", HiveBackend: "copilot", CreatedAt: now, MergedAt: now},
		{Repo: "documentation", Number: 9, URL: "https://github.com/otherorg/documentation/pull/9", HiveAttributed: true, HiveModel: "sonnet", HiveBackend: "copilot", CreatedAt: now, MergedAt: now},
	}
	runs := []TaskRunRecord{
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "opus", Outcome: outcomeCompleted, PRVerified: true, PRURL: "https://github.com/projectbluefin/documentation/pull/7"},
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "opus", Outcome: outcomeCompleted, PRVerified: true, PRURL: "https://github.com/projectbluefin/bluefin/pull/8"},
		{TS: now.Format(time.RFC3339), Backend: "copilot", Model: "sonnet", Outcome: outcomeCompleted, PRVerified: true, PRURL: "https://github.com/projectbluefin/documentation/pull/9"},
	}
	prCount := func(resp contributeEffectiveModelsResponse) map[string]int {
		out := map[string]int{}
		for _, row := range append(append([]contributeEffectiveModelRow{}, resp.Ranked...), resp.Insufficient...) {
			out[row.Model] += row.PRs
		}
		return out
	}
	contrib := prCount(aggregateContributeEffectiveModels(prs, runs, "7d", "contributor", 1, now))
	if contrib["opus"] != 2 || contrib["sonnet"] != 0 {
		t.Fatalf("contributor PRs = %v, want short and qualified repo PRs matched, other owner excluded", contrib)
	}
	hive := prCount(aggregateContributeEffectiveModels(prs, runs, "7d", "hive", 1, now))
	if hive["opus"] != 0 || hive["sonnet"] != 1 {
		t.Fatalf("hive PRs = %v, want contributor PRs excluded and same-name repo under other owner kept", hive)
	}
}

func TestReadEffectiveModelRunsIncludesRotatedHistory(t *testing.T) {
	path := t.TempDir() + "/" + taskRunLogFileName
	old := time.Now().UTC().Add(-20 * 24 * time.Hour).Format(time.RFC3339)
	recent := time.Now().UTC().Add(-time.Hour).Format(time.RFC3339)
	if err := os.WriteFile(path, []byte(`{"ts":"`+recent+`","model":"opus","backend":"copilot"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, rotated, err := readEffectiveModelRuns(path, 30*24*time.Hour)
	if err != nil || rotated || len(runs) != 1 {
		t.Fatalf("live only: runs=%d rotated=%v err=%v, want 1/false/nil", len(runs), rotated, err)
	}
	if err := os.WriteFile(path+".1", []byte(`{"ts":"`+old+`","model":"opus","backend":"copilot"}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	runs, rotated, err = readEffectiveModelRuns(path, 30*24*time.Hour)
	if err != nil || !rotated || len(runs) != 2 || runs[0].TS != old {
		t.Fatalf("with rotation: runs=%+v rotated=%v err=%v, want rotated history first", runs, rotated, err)
	}
	runs, _, _ = readEffectiveModelRuns(path, 7*24*time.Hour)
	if len(runs) != 1 {
		t.Fatalf("7d window runs = %d, want rotated run outside window dropped", len(runs))
	}
}

func TestEffectiveModelsCoverageReportsPartialHistory(t *testing.T) {
	now := time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC)
	lookback := 14 * 24 * time.Hour
	runs := []TaskRunRecord{{TS: now.Add(-3 * 24 * time.Hour).Format(time.RFC3339)}, {TS: now.Add(-time.Hour).Format(time.RFC3339)}}

	week := effectiveModelsCoverage(7*24*time.Hour, lookback, runs, true, now)
	if week.ClosedPRLookbackDays != 14 || week.ClosedPRsPartial || !week.RunsPartial || week.RunsSince != runs[0].TS {
		t.Fatalf("7d coverage = %+v, want full PRs, partial rotated runs since oldest run", week)
	}
	month := effectiveModelsCoverage(30*24*time.Hour, lookback, runs, false, now)
	if !month.ClosedPRsPartial || month.RunsPartial {
		t.Fatalf("30d coverage = %+v, want partial closed PRs and complete unrotated runs", month)
	}
	all := effectiveModelsCoverage(0, lookback, runs, true, now)
	if !all.ClosedPRsPartial || !all.RunsPartial {
		t.Fatalf("all coverage = %+v, want both partial", all)
	}
	full := effectiveModelsCoverage(7*24*time.Hour, lookback, []TaskRunRecord{{TS: now.Add(-8 * 24 * time.Hour).Format(time.RFC3339)}}, true, now)
	if full.RunsPartial {
		t.Fatalf("coverage = %+v, want runs complete when retained history predates window", full)
	}
}

func TestHandleContributeEffectiveModelsReportsCoverage(t *testing.T) {
	s := covApiServer(t)
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{}}
	req := httptest.NewRequest(http.MethodGet, "/api/contribute/effective-models?window=all", nil)
	rec := httptest.NewRecorder()
	s.handleContributeEffectiveModels(rec, req)
	var got contributeEffectiveModelsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v body %s", err, rec.Body.String())
	}
	if got.Coverage.ClosedPRLookbackDays <= 0 || !got.Coverage.ClosedPRsPartial {
		t.Fatalf("coverage = %+v, want closed-PR lookback reported and All marked partial", got.Coverage)
	}
}

func TestEffectiveModelRuntimeClassifiesInferenceLocation(t *testing.T) {
	for _, tc := range []struct{ model, backend, want string }{
		{"openai-codex/gpt-6-astra", "pi", "hosted"},
		{"anthropic/claude-opus", "pi", "hosted"},
		{"lemonade/qwen3-coder", "pi", "local"},
		{"ollama/llama3", "pi", "local"},
		{"qwen3-coder-gguf", "pi", "local"},
		{"mystery-model", "pi", "unknown"},
		{"claude-sonnet-5", "copilot", "hosted"},
		{"gpt-6-astra", "codex", "unknown"},
	} {
		if got := effectiveModelRuntime(tc.model, tc.backend); got != tc.want {
			t.Errorf("effectiveModelRuntime(%q, %q) = %q, want %q", tc.model, tc.backend, got, tc.want)
		}
	}
}

func TestContributeOperationsEffectiveModelsUnmeasuredRunRates(t *testing.T) {
	raw, err := os.ReadFile("contribute_landing.go")
	if err != nil {
		t.Fatal(err)
	}
	page := string(raw)
	for _, want := range []string{
		`function effectiveRunPct(x,v){return Number(x&&x.runs||0)>0?effectivePct(v):'—';}`,
		`effectiveRunPct(x,x.verified_pr_run_rate)`,
		`effectiveRunPct(x,x.failure_rate)`,
		`effectiveRunPct(x,x.nothing_to_ship_rate)`,
		`Run columns come from the contributor task-run log only`,
		`id="effective-models-coverage"`,
		`effectiveCoverageText(data.coverage)`,
	} {
		if !strings.Contains(page, want) {
			t.Fatalf("effective models panel missing %q", want)
		}
	}
	for _, bad := range []string{`effectivePct(x.verified_pr_run_rate)`, `effectivePct(x.failure_rate)`, `effectivePct(x.nothing_to_ship_rate)`} {
		if strings.Contains(page, bad) {
			t.Fatalf("run-derived rate still rendered unconditionally: %q", bad)
		}
	}
}
