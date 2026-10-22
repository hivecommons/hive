package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

func fakeStageEngineStatus() stageengine.Status {
	return stageengine.Status{
		Mode: "shadow", Derivations: 12, Mismatches: 2, Anomalies: 1, Healed: 3,
		LastMismatchExamples: []stageengine.StatusExample{{
			Ref: "acme/widgets#7", Kind: "stage", Legacy: "stage/050", Engine: "stage/060",
			At: time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC),
		}},
	}
}

// governor.labels.stage.engine.mode round-trips through the Labels tab's
// GET/PUT, rejects unknown modes before touching the config, and the tab
// renders a control with the YAML hint and help mark (ADR-0025, PR-E).
func TestGovernorLabelsStageEngineModeRoundTrip(t *testing.T) {
	s := covApiServer(t)
	for _, bad := range []any{"audit", "ON", 7} {
		if rec := doPut(s, "/api/config/governor/labels", map[string]any{"stage_engine_mode": bad}); rec.Code != http.StatusBadRequest {
			t.Fatalf("%v: expected 400, got %d — %s", bad, rec.Code, rec.Body.String())
		}
	}
	if got := s.deps.Config.Governor.Labels.Stage.Engine.Mode; got != "" {
		t.Fatalf("a rejected PUT changed the mode to %q", got)
	}
	rec := doGet(s, "/api/config/governor")
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["stageEngineMode"] != "enforce" {
		t.Fatalf("default stageEngineMode = %v, want enforce", got["stageEngineMode"])
	}
	if rec := doPut(s, "/api/config/governor/labels", map[string]any{"stage_engine_mode": " shadow "}); rec.Code != http.StatusOK {
		t.Fatalf("labels PUT: %d — %s", rec.Code, rec.Body.String())
	}
	if got := s.deps.Config.Governor.Labels.Stage.Engine.Mode; got != "shadow" {
		t.Fatalf("mode = %q, want shadow", got)
	}
	rec = doGet(s, "/api/config/governor")
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["stageEngineMode"] != "shadow" {
		t.Fatalf("stageEngineMode after PUT = %v", got["stageEngineMode"])
	}

	html := indexHTML(t)
	labels := extractJSFunction(t, html, "renderGovLabels")
	for _, want := range []string{
		"yamlHint('governor.labels.stage.engine.mode')",
		`id="cfg-stage-engine-mode" data-change-action="markDirty" data-arg0="labels" data-arg1="stage_engine_mode" data-arg-types="s,s,v"`,
		"settingHelpMark('settings', 'labels', 'Central stage engine',",
		"_configState.data.stageEngineMode || 'enforce'",
		"['off',", "['shadow',", "['enforce',",
		"48h",
	} {
		if !strings.Contains(labels, want) {
			t.Errorf("renderGovLabels lacks %q", want)
		}
	}
	if !strings.Contains(html, "key: 'governor.labels.stage.engine.mode'") {
		t.Error("settings search index has no entry for governor.labels.stage.engine.mode")
	}
}

// /api/status carries stageEngine{mode,derivations,mismatches,anomalies,
// healed,lastMismatchExamples} when the hive wires a status source, and
// omits it otherwise.
func TestStatusStageEngineBlock(t *testing.T) {
	s := covApiServer(t)
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "engine-test"}
	s.statusMu.Unlock()

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var status map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if _, ok := status["stageEngine"]; ok {
		t.Fatal("stageEngine present without a status source")
	}

	s.deps.StageEngineStatus = fakeStageEngineStatus
	rec = httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	eng, ok := status["stageEngine"].(map[string]any)
	if !ok {
		t.Fatalf("stageEngine missing: %s", rec.Body.String())
	}
	for k, want := range map[string]any{"mode": "shadow", "derivations": 12.0, "mismatches": 2.0, "anomalies": 1.0, "healed": 3.0} {
		if eng[k] != want {
			t.Errorf("stageEngine.%s = %v, want %v", k, eng[k], want)
		}
	}
	ex, _ := eng["lastMismatchExamples"].([]any)
	if len(ex) != 1 {
		t.Fatalf("lastMismatchExamples = %v", eng["lastMismatchExamples"])
	}
	first, _ := ex[0].(map[string]any)
	if first["ref"] != "acme/widgets#7" || first["legacy"] != "stage/050" || first["engine"] != "stage/060" || first["kind"] != "stage" {
		t.Fatalf("example = %v", first)
	}
}

// The activity tracker payload carries the engine block and the page renders
// it as a .ham-engine strip (mode / mismatches / anomalies) with hover text on
// the items only — no inline style, no raw hex.
func TestStageTrackerEngineStrip(t *testing.T) {
	now := time.Date(2026, 10, 17, 11, 0, 0, 0, time.UTC)
	s, store := stageTrackerFixture(t, now)
	resp := s.buildStageTracker(nil, store, now)
	if resp.Engine != nil {
		t.Fatal("engine block present without a status source")
	}
	if s.deps == nil {
		s.deps = &Dependencies{}
	}
	s.deps.StageEngineStatus = fakeStageEngineStatus
	s.statusMu.RLock()
	status := s.status
	s.statusMu.RUnlock()
	resp = s.buildStageTracker(status, store, now)
	if resp.Engine == nil || resp.Engine.Mode != "shadow" || resp.Engine.Mismatches != 2 {
		t.Fatalf("engine = %+v", resp.Engine)
	}
	raw, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"engine":{"mode":"shadow"`) {
		t.Fatalf("payload lacks engine block: %s", raw)
	}

	html := indexHTML(t)
	fn := extractJSFunction(t, html, "stageTrackerEngineHtml")
	for _, want := range []string{
		`<span class="ham-engine" data-mode="${esc(e.mode)}">`,
		`class="ham-engine-mode" title=`,
		`class="ham-engine-mismatch${mismatches ? ' ham-engine-warn' : ''}" title=`,
		`class="ham-engine-anomaly${anomalies ? ' ham-engine-warn' : ''}" title=`,
		`e.lastMismatchExamples`,
	} {
		if !strings.Contains(fn, want) {
			t.Errorf("stageTrackerEngineHtml lacks %q", want)
		}
	}
	if strings.Contains(fn, "style=") || strings.Contains(fn, "#") {
		t.Error("the engine strip must not carry inline style or raw colours")
	}
	for _, want := range []string{
		"${stageTrackerEngineHtml(d)}${stageTrackerUpdatedHtml(d, nowMs)}",
		".ham-engine {",
		`.ham-engine[data-mode="enforce"] .ham-engine-mode { color: var(--status-ok); }`,
		".ham-engine .ham-engine-warn { color: var(--status-error);",
	} {
		if !strings.Contains(html, want) {
			t.Errorf("tracker page lacks %q", want)
		}
	}
}

// /metrics renders every stageengine series (zero when idle) with the
// hive_ prefix and sorted labels.
func TestWriteStageEngineMetrics(t *testing.T) {
	c := stageengine.NewCounters()
	var b strings.Builder
	headers := 0
	writeHeader := func(name, help, typ string) {
		headers++
		if typ != "counter" || help == "" || !strings.HasPrefix(name, "hive_stage_") {
			t.Errorf("header %s %q %s", name, help, typ)
		}
	}
	writeStageEngineMetrics(&b, "h1", writeHeader, c)
	out := b.String()
	if headers != 5 {
		t.Fatalf("headers = %d, want 5", headers)
	}
	for _, want := range []string{
		`hive_stage_derivations_total{hive_id="h1"} 0`,
		`hive_stage_shadow_mismatch_total{hive_id="h1"} 0`,
		`hive_stage_transition_anomaly_total{hive_id="h1"} 0`,
		`hive_stage_writer_writes_total{hive_id="h1"} 0`,
		`hive_stage_duplicate_pills_healed_total{hive_id="h1"} 0`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("idle output lacks %q:\n%s", want, out)
		}
	}

	c.Derivation(stagelabels.PRItem, stagelabels.ReviewPending)
	c.ShadowMismatch(stagelabels.PRItem, stagelabels.ReviewPending, stagelabels.Reviewed)
	c.TransitionAnomaly(stagelabels.IssueItem, stagelabels.Claimed, stagelabels.Actionable)
	b.Reset()
	writeStageEngineMetrics(&b, "h1", func(string, string, string) {}, c)
	out = b.String()
	for _, want := range []string{
		`hive_stage_derivations_total{hive_id="h1",kind="pr",stage=`,
		`hive_stage_shadow_mismatch_total{hive_id="h1",engine=`,
		`hive_stage_transition_anomaly_total{hive_id="h1",from=`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	b.Reset()
	writeStageEngineMetrics(&b, "h1", writeHeader, nil)
	if b.Len() != 0 {
		t.Fatal("nil counters wrote output")
	}
}
