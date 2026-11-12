package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/adminmcp"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
	"gopkg.in/yaml.v3"
)

// generalAdvancedBodyJSONFields lists the JSON fields of a body struct,
// following embedded structs.
func generalAdvancedBodyJSONFields(t reflect.Type) []string {
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.Anonymous && f.Type.Kind() == reflect.Struct {
			out = append(out, generalAdvancedBodyJSONFields(f.Type)...)
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "" || name == "-" {
			continue
		}
		out = append(out, name)
	}
	return out
}

// The admin MCP's governor.general_settings operation refuses a field it does
// not know. This fails when the endpoint's body gains a field the operation
// was not taught (or the operation offers one the endpoint would ignore).
func TestGeneralAdvancedBodyMatchesAdminMCPOp(t *testing.T) {
	body := generalAdvancedBodyJSONFields(reflect.TypeOf(generalAdvancedBody{}))
	op := adminmcp.GovernorGeneralSettingsFields()
	sort.Strings(body)
	sort.Strings(op)
	if len(body) < 11 {
		t.Fatalf("only %d body fields found (%v); the reflection no longer reads the body", len(body), body)
	}
	if strings.Join(body, ",") != strings.Join(op, ",") {
		t.Fatalf("PUT /api/config/governor/general-advanced takes %v\nbut %s accepts %v\nteach the operation the new field (pkg/adminmcp/tools_governor_general_ops.go) and document it in docs/admin-mcp.md",
			body, adminmcp.WriteOpGovernorGeneralSettings, op)
	}
}

func apiBudgetPacingGet(t *testing.T, s *Server) (bool, map[string]any) {
	t.Helper()
	rec := doOwnerGet(s, generalAdvancedPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET general-advanced: %d %s", rec.Code, rec.Body.String())
	}
	got := decodeJSON(t, rec)
	on, ok := got["api_budget_pacing"].(bool)
	if !ok {
		t.Fatalf("GET general-advanced has no api_budget_pacing: %v", got)
	}
	state, ok := got["api_budget_pacing_state"].(map[string]any)
	if !ok {
		t.Fatalf("GET general-advanced has no api_budget_pacing_state: %v", got)
	}
	return on, state
}

// The pacing switch defaults off and round-trips through the general-advanced
// GET and PUT, hive.yaml and the dashboard overlay.
func TestAPIBudgetPacingSettingRoundTrips(t *testing.T) {
	t.Cleanup(github.ResetAPIBudgetPacingForTest(time.Now))
	s, deps := apiServer(t)
	overlay := capTestLayers(t, deps.Config)

	on, state := apiBudgetPacingGet(t, s)
	if on || state["enabled"] != false || state["active"] != false || state["factor"] != float64(1) {
		t.Fatalf("default: api_budget_pacing = %v state = %v, want off", on, state)
	}
	for _, key := range []string{"enabled", "active", "factor", "effective_interval_s", "burn_per_min", "reason"} {
		if _, ok := state[key]; !ok {
			t.Errorf("api_budget_pacing_state lacks %s: %v", key, state)
		}
	}

	if rec := doPutRaw(s, generalAdvancedPath, `{"api_budget_pacing": "yes"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("a non-boolean api_budget_pacing: status = %d, want 400", rec.Code)
	}
	if deps.Config.Governor.APIBudgetPacing {
		t.Fatal("a rejected PUT turned pacing on")
	}

	rec := doPut(s, generalAdvancedPath, map[string]any{"api_budget_pacing": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("PUT: %d %s", rec.Code, rec.Body.String())
	}
	if !deps.Config.Governor.APIBudgetPacing {
		t.Fatal("PUT did not turn governor.api_budget_pacing on")
	}
	resp := decodeJSON(t, rec)
	if resp["api_budget_pacing"] != true {
		t.Fatalf("PUT response = %v", resp)
	}
	// The loop has not looked yet: the state says so instead of claiming off.
	on, state = apiBudgetPacingGet(t, s)
	if !on || state["enabled"] != true || state["active"] != false || !strings.Contains(state["reason"].(string), "next eval cycle") {
		t.Fatalf("after PUT: api_budget_pacing = %v state = %v", on, state)
	}
	if o := readOverlayConfig(t, overlay); !o.Governor.APIBudgetPacing {
		t.Fatal("the dashboard overlay does not carry governor.api_budget_pacing")
	}
	data, err := os.ReadFile(deps.Config.SourcePath)
	if err != nil {
		t.Fatalf("hive.yaml not written: %v", err)
	}
	var saved config.Config
	if err := yaml.Unmarshal(data, &saved); err != nil {
		t.Fatal(err)
	}
	if !saved.Governor.APIBudgetPacing {
		t.Fatalf("hive.yaml does not carry governor.api_budget_pacing:\n%s", data)
	}
	if e := lastCapAudit(t, s); e.Action != "config_governor_general_advanced" {
		t.Fatalf("audit = %+v", e)
	}

	// A PUT that does not name it leaves it alone.
	if rec := doPut(s, generalAdvancedPath, map[string]any{"eval_interval_s": 120}); rec.Code != http.StatusOK {
		t.Fatalf("PUT eval_interval_s: %d", rec.Code)
	}
	if !deps.Config.Governor.APIBudgetPacing || deps.Config.Governor.EvalIntervalS != 120 {
		t.Fatalf("an unrelated PUT changed pacing: %+v", deps.Config.Governor.APIBudgetPacing)
	}

	if rec := doPut(s, generalAdvancedPath, map[string]any{"api_budget_pacing": false}); rec.Code != http.StatusOK {
		t.Fatalf("PUT off: %d", rec.Code)
	}
	if deps.Config.Governor.APIBudgetPacing || readOverlayConfig(t, overlay).Governor.APIBudgetPacing {
		t.Fatal("PUT false did not turn pacing off in the config and the overlay")
	}
}

// apiBudgetPacingEngage records ten minutes of 200 calls a minute against a
// window with 4000 left that resets in 30 minutes, and lets the pacer decide
// for a 120s loop: factor 1.88, interval 226s.
func apiBudgetPacingEngage(t *testing.T) (advance func(time.Duration)) {
	t.Helper()
	start := time.Date(2026, 11, 12, 15, 0, 0, 0, time.UTC)
	now := start
	t.Cleanup(github.ResetAPIBudgetPacingForTest(func() time.Time { return now }))
	reset := start.Add(40 * time.Minute)
	for i := 0; i < 2000; i++ {
		now = start.Add(time.Duration(i) * 300 * time.Millisecond)
		h := http.Header{}
		h.Set("X-RateLimit-Limit", "7000")
		h.Set("X-RateLimit-Remaining", strconv.Itoa(5999-i))
		h.Set("X-RateLimit-Reset", strconv.FormatInt(reset.Unix(), 10))
		h.Set("X-RateLimit-Resource", "core")
		github.RecordRESTRequest("hive:eval_cycle", http.MethodGet, "/repos/acme/widgets/pulls", http.StatusOK, h)
	}
	now = start.Add(10 * time.Minute)
	st, _ := github.ObserveAPIBudgetPacing(github.APIBudgetPacingInput{Enabled: true, Interval: 120 * time.Second, MaxInterval: 1800 * time.Second, Reserve: 800})
	if !st.Active || st.Factor != 1.88 || st.EffectiveIntervalS != 226 {
		t.Fatalf("pacer did not engage: %+v", st)
	}
	return func(d time.Duration) { now = now.Add(d) }
}

// The general-advanced GET reports what the loop is doing with the switch.
func TestGeneralAdvancedReportsLivePacingState(t *testing.T) {
	apiBudgetPacingEngage(t)
	s, deps := apiServer(t)
	deps.Config.Governor.APIBudgetPacing = true
	on, state := apiBudgetPacingGet(t, s)
	if !on || state["enabled"] != true || state["active"] != true || state["factor"] != 1.88 ||
		state["effective_interval_s"] != float64(226) || state["burn_per_min"] != float64(200) {
		t.Fatalf("api_budget_pacing = %v state = %v", on, state)
	}
	if reason, _ := state["reason"].(string); !strings.Contains(reason, "would run out before the reset") {
		t.Fatalf("reason = %q", state["reason"])
	}
}

// /api/status carries the pacing state next to the eval interval and mode.
func TestStatusReportsAPIBudgetPacing(t *testing.T) {
	apiBudgetPacingEngage(t)
	s := newTestServer()
	s.deps = &Dependencies{EvalLoop: func() EvalLoopStatus {
		pacing := github.APIBudgetPacingSnapshot()
		return EvalLoopStatus{Mode: "poll", IntervalS: pacing.EffectiveIntervalS, APIBudgetPacing: &pacing}
	}}
	s.statusMu.Lock()
	s.status = &StatusPayload{HiveID: "pacing-test"}
	s.statusMu.Unlock()

	rec := httptest.NewRecorder()
	s.handleStatus(rec, httptest.NewRequest(http.MethodGet, "/api/status", nil))
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	pacing, ok := got["api_budget_pacing"].(map[string]any)
	if !ok {
		t.Fatalf("status has no api_budget_pacing block: %s", rec.Body.String())
	}
	if got["evalIntervalS"] != float64(226) || pacing["enabled"] != true || pacing["active"] != true || pacing["factor"] != 1.88 ||
		pacing["effective_interval_s"] != float64(226) || pacing["burn_per_min"] != float64(200) || pacing["reason"] == "" {
		t.Fatalf("status pacing = %v (evalIntervalS %v)", pacing, got["evalIntervalS"])
	}
	for _, internal := range []string{"Remaining", "ResetIn", "remaining", "reset_in"} {
		if _, ok := pacing[internal]; ok {
			t.Errorf("status pacing serves the stale reading field %q", internal)
		}
	}
}

// The next-kick estimate follows the paced interval while a stretch is held,
// and the unpaced one before and after.
func TestBuildGovernorNextRunFollowsAPIBudgetPacing(t *testing.T) {
	lastEval := time.Date(2026, 11, 12, 15, 10, 0, 0, time.UTC)
	cfg := &config.Config{Governor: config.GovernorConfig{EvalIntervalS: 120, EvalIntervalMaxS: 1800, Modes: map[string]config.ModeConfig{}}}
	state := governor.State{Mode: governor.ModeIdle, LastEval: lastEval}
	if got, want := buildGovernor(state, cfg).NextKickAt, "2026-11-12T15:12:00Z"; got != want {
		t.Fatalf("unpaced nextKickAt = %q, want %q", got, want)
	}
	advance := apiBudgetPacingEngage(t)
	if got, want := buildGovernor(state, cfg).NextKickAt, "2026-11-12T15:13:46Z"; got != want {
		t.Fatalf("paced nextKickAt = %q, want %q (226s after the last eval)", got, want)
	}
	// Past the reset the stretch no longer applies.
	advance(31 * time.Minute)
	if got, want := buildGovernor(state, cfg).NextKickAt, "2026-11-12T15:12:00Z"; got != want {
		t.Fatalf("nextKickAt after the reset = %q, want %q", got, want)
	}
}

// The toggle renders under the eval interval with its YAML hint, its help
// mark and the live state line, and saves through the general-advanced PUT.
func TestAPIBudgetPacingToggleRendersOnGeneralTab(t *testing.T) {
	html := indexHTML(t)
	general := extractJSFunction(t, html, "renderGovGeneral")
	if !strings.Contains(general, "${renderGovApiBudgetPacing(genAdv)}") {
		t.Fatal("the General tab does not render the API budget pacing toggle")
	}
	if strings.Index(general, "renderGovApiBudgetPacing(") < strings.Index(general, "yamlHint('governor.eval_interval_s')") {
		t.Error("the pacing toggle is not placed after the eval interval field")
	}
	toggle := extractJSFunction(t, html, "renderGovApiBudgetPacing")
	for _, want := range []string{
		"yamlHint('governor.api_budget_pacing')",
		"settingHelpMark('settings', 'general', 'Pace GitHub API calls over the rate-limit window'",
		`data-action="toggleApiBudgetPacing"`,
		`id="gov-api-budget-pacing-state"`,
		"apiBudgetPacingStateHtml(genAdv.api_budget_pacing_state)",
		"genAdv.api_budget_pacing === true",
	} {
		if !strings.Contains(toggle, want) {
			t.Errorf("renderGovApiBudgetPacing lacks %s", want)
		}
	}
	save := extractJSFunction(t, html, "toggleApiBudgetPacing")
	if !strings.Contains(save, "'/api/config/governor/general-advanced'") || !strings.Contains(save, "JSON.stringify({ api_budget_pacing: turningOn })") {
		t.Error("the toggle does not save api_budget_pacing through the general-advanced PUT")
	}
	state := extractJSFunction(t, html, "apiBudgetPacingStateHtml")
	for _, field := range []string{"st.enabled", "st.active", "st.factor", "st.effective_interval_s", "st.burn_per_min", "st.reason"} {
		if !strings.Contains(state, field) {
			t.Errorf("the state line does not show %s", field)
		}
	}
	if !strings.Contains(html, "governor.api_budget_pacing governor.eval_interval_s") {
		t.Error("the settings search index does not find the pacing toggle")
	}
}
