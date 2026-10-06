package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
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

func TestAggregateContributeEffectiveModelsIdentityFooterAttribution(t *testing.T) {
	now := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	body := "Done\n\n— hive: backend=codex model=gpt-6-astra effort=medium\n\n---\n🐝 **Hive Agent**: `contributor` | **SHA:** `671737a3b`"
	var prs []ghpkg.PullRequest
	var runs []TaskRunRecord
	for number := 1; number <= effectiveModelsDefaultMinMergedPRs; number++ {
		meta, ok := ghpkg.ParseAttributionTrailer(body)
		if !ok {
			t.Fatal("production contributor PR body lost its attribution")
		}
		prs = append(prs, ghpkg.PullRequest{
			Repo: "org/repo", Number: number, HiveAttributed: ok,
			HiveBackend: meta.Backend, HiveModel: meta.Model,
			CreatedAt: now, MergedAt: now, Rework: ghpkg.PRReworkStats{FirstPass: true},
		})
		runs = append(runs, TaskRunRecord{
			TS: now.Format(time.RFC3339), Username: "contributor", Backend: "codex", Model: "gpt-6-astra",
			Outcome: outcomeCompleted, PRVerified: true, PRURL: fmt.Sprintf("https://github.com/org/repo/pull/%d", number),
		})
	}
	for _, filter := range []string{"all", "contributor"} {
		below := aggregateContributeEffectiveModels(prs[:4], runs, "30d", filter, effectiveModelsDefaultMinMergedPRs, now)
		if len(below.Ranked) != 0 || len(below.Insufficient) != 1 || below.Insufficient[0].MergedPRs != 4 {
			t.Fatalf("%s below threshold = %+v", filter, below)
		}
		got := aggregateContributeEffectiveModels(prs, runs, "30d", filter, effectiveModelsDefaultMinMergedPRs, now)
		if len(got.Ranked) != 1 || len(got.Insufficient) != 0 {
			t.Fatalf("%s at threshold = %+v", filter, got)
		}
		row := got.Ranked[0]
		if row.Model != "gpt-6-astra" || row.Backend != "codex" || row.MergedPRs != 5 || row.PRs != 5 || row.EffectivenessRank != 1 {
			t.Fatalf("%s attribution/ranking = %+v", filter, row)
		}
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
		`id="effective-models-toggle" aria-expanded="false" aria-controls="effective-models-body" data-ops-section="effective-models-card" data-ops-default-collapsed title="Expand panel"`,
		`<span class="section-chevron collapsed" aria-hidden="true">▼</span>`,
		`<div class="section-body collapsed" id="effective-models-body">`,
		`.section-body{overflow:visible;transition:opacity 200ms ease;max-height:none;opacity:1}`,
		`.section-body.collapsed{max-height:0!important;overflow:hidden;opacity:0;pointer-events:none}`,
		`hive-section-collapsed-`,
		`function initOpsCollapsiblePanels`,
		`localStorage.getItem(OPS_SECTION_LS_PREFIX+sectionId)`,
		`localStorage.setItem(OPS_SECTION_LS_PREFIX+sectionId,'1')`,
		`localStorage.setItem(OPS_SECTION_LS_PREFIX+sectionId,'0')`,
		`localStorage.removeItem(OPS_SECTION_LS_PREFIX+sectionId)`,
		`ccApplySectionCollapse(sectionId);`,
		`try{loadEffectiveModelsIfOpen();}catch`,
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

// TestContributeOperationsEffectiveModelsDefaultCollapsedBehaviour runs the
// shipped collapse/persist helpers in node (#10919): the models panel starts
// collapsed without a stored key, remembers being opened ('0') and closed ('1'),
// fetches only on first open, and a section without data-ops-default-collapsed
// keeps the original '1'/missing-key contract.
func TestContributeOperationsEffectiveModelsDefaultCollapsedBehaviour(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not installed; the presence assertions above still ran")
	}
	page := renderContributePage(t)
	var src strings.Builder
	for _, decl := range []string{
		"function ccOpsSectionDefaultCollapsed(sectionId){",
		"function ccOpsSectionRead(sectionId){",
		"function ccOpsSectionWrite(sectionId,collapsed){",
		"function ccApplySectionCollapse(sectionId){",
		"function ccToggleOpsSection(sectionId){",
		"function loadEffectiveModelsIfOpen(){",
		"function loadEffectiveModels(){",
	} {
		src.WriteString(extractJSFunc(t, page, decl))
		src.WriteString("\n")
	}
	script := `
var OPS_SECTION_LS_PREFIX='hive-section-collapsed-';
var effectiveModelsWindow='7d',effectiveModelsFilter='all',effectiveModelsRequested=false;
var fetches=0;function fetch(){fetches++;return new Promise(function(){});}
var store={},lsThrows=false;
var localStorage={
  getItem:function(k){if(lsThrows)throw new Error('blocked');return Object.prototype.hasOwnProperty.call(store,k)?store[k]:null;},
  setItem:function(k,v){if(lsThrows)throw new Error('blocked');store[k]=String(v);},
  removeItem:function(k){if(lsThrows)throw new Error('blocked');delete store[k];}
};
function El(attrs){this.attrs=attrs||{};this.cls={};var self=this;this.classList={toggle:function(c,on){self.cls[c]=!!on;}};}
El.prototype.getAttribute=function(k){return Object.prototype.hasOwnProperty.call(this.attrs,k)?this.attrs[k]:null;};
El.prototype.setAttribute=function(k,v){this.attrs[k]=String(v);};
El.prototype.hasAttribute=function(k){return Object.prototype.hasOwnProperty.call(this.attrs,k);};
function section(id,defaultCollapsed){
  var btnAttrs={'data-ops-section':id};if(defaultCollapsed)btnAttrs['data-ops-default-collapsed']='';
  var parts={btn:new El(btnAttrs),body:new El(),chevron:new El()};
  parts.root={querySelector:function(sel){if(sel==='.section-body')return parts.body;if(sel==='.section-chevron')return parts.chevron;if(sel==='[data-ops-section="'+id+'"]')return parts.btn;return null;}};
  return parts;
}
var secs={'effective-models-card':section('effective-models-card',true),'other-card':section('other-card',false)};
var mount=new El();
var document={
  getElementById:function(id){if(id==='effective-models-ranked')return mount;return secs[id]?secs[id].root:null;},
  querySelector:function(sel){for(var id in secs){if(sel==='[data-ops-section="'+id+'"]')return secs[id].btn;}return null;}
};
` + src.String() + `
var M='effective-models-card',KEY='hive-section-collapsed-'+M,OK='hive-section-collapsed-other-card';
function view(id){var s=secs[id];ccApplySectionCollapse(id);return {collapsed:ccOpsSectionRead(id),body:!!s.body.cls.collapsed,chevron:!!s.chevron.cls.collapsed,aria:s.btn.attrs['aria-expanded'],title:s.btn.attrs.title};}
var out={};
out.first=view(M);
loadEffectiveModelsIfOpen();out.fetchesWhileCollapsed=fetches;
ccToggleOpsSection(M);out.opened=view(M);out.storedOpen=store[KEY];out.fetchesAfterOpen=fetches;
ccToggleOpsSection(M);out.closed=view(M);out.storedClosed=store[KEY];
ccToggleOpsSection(M);out.fetchesAfterReopen=fetches;
effectiveModelsRequested=false;fetches=0;store[KEY]='0';
out.reloadOpen=view(M);loadEffectiveModelsIfOpen();out.reloadOpenFetches=fetches;
out.otherFirst=view('other-card');
ccToggleOpsSection('other-card');out.otherStoredClosed=store[OK];
ccToggleOpsSection('other-card');out.otherKeyAfterOpen=Object.prototype.hasOwnProperty.call(store,OK);out.otherOpen=view('other-card');
lsThrows=true;out.blockedDefault=ccOpsSectionRead(M);out.blockedOther=ccOpsSectionRead('other-card');
console.log(JSON.stringify(out));
`
	cmd := exec.Command(node, "-e", script)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("node failed: %v\n%s", err, raw)
	}
	type state struct {
		Collapsed bool   `json:"collapsed"`
		Body      bool   `json:"body"`
		Chevron   bool   `json:"chevron"`
		Aria      string `json:"aria"`
		Title     string `json:"title"`
	}
	var got struct {
		First                 state  `json:"first"`
		FetchesWhileCollapsed int    `json:"fetchesWhileCollapsed"`
		Opened                state  `json:"opened"`
		StoredOpen            string `json:"storedOpen"`
		FetchesAfterOpen      int    `json:"fetchesAfterOpen"`
		Closed                state  `json:"closed"`
		StoredClosed          string `json:"storedClosed"`
		FetchesAfterReopen    int    `json:"fetchesAfterReopen"`
		ReloadOpen            state  `json:"reloadOpen"`
		ReloadOpenFetches     int    `json:"reloadOpenFetches"`
		OtherFirst            state  `json:"otherFirst"`
		OtherStoredClosed     string `json:"otherStoredClosed"`
		OtherKeyAfterOpen     bool   `json:"otherKeyAfterOpen"`
		OtherOpen             state  `json:"otherOpen"`
		BlockedDefault        bool   `json:"blockedDefault"`
		BlockedOther          bool   `json:"blockedOther"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &got); err != nil {
		t.Fatalf("decode node output %q: %v", raw, err)
	}
	collapsed := state{Collapsed: true, Body: true, Chevron: true, Aria: "false", Title: "Expand panel"}
	expanded := state{Collapsed: false, Body: false, Chevron: false, Aria: "true", Title: "Collapse panel"}
	if got.First != collapsed {
		t.Errorf("no stored key: models panel = %+v, want collapsed %+v", got.First, collapsed)
	}
	if got.FetchesWhileCollapsed != 0 {
		t.Errorf("collapsed panel fetched %d times on load, want 0", got.FetchesWhileCollapsed)
	}
	if got.Opened != expanded || got.StoredOpen != "0" || got.FetchesAfterOpen != 1 {
		t.Errorf("first open: state=%+v stored=%q fetches=%d, want expanded, '0', 1 fetch", got.Opened, got.StoredOpen, got.FetchesAfterOpen)
	}
	if got.Closed != collapsed || got.StoredClosed != "1" {
		t.Errorf("close: state=%+v stored=%q, want collapsed, '1'", got.Closed, got.StoredClosed)
	}
	if got.FetchesAfterReopen != 1 {
		t.Errorf("reopen fetched again: total fetches=%d, want 1", got.FetchesAfterReopen)
	}
	if got.ReloadOpen != expanded || got.ReloadOpenFetches != 1 {
		t.Errorf("reload with stored '0': state=%+v fetches=%d, want expanded and 1 fetch", got.ReloadOpen, got.ReloadOpenFetches)
	}
	if got.OtherFirst != expanded || got.OtherStoredClosed != "1" || got.OtherKeyAfterOpen || got.OtherOpen != expanded {
		t.Errorf("non-default section changed contract: first=%+v storedClosed=%q keyAfterOpen=%v open=%+v", got.OtherFirst, got.OtherStoredClosed, got.OtherKeyAfterOpen, got.OtherOpen)
	}
	if !got.BlockedDefault || got.BlockedOther {
		t.Errorf("blocked localStorage: models collapsed=%v (want true), other collapsed=%v (want false)", got.BlockedDefault, got.BlockedOther)
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
