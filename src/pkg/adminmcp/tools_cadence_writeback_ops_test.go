package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// cadenceWBProvider answers settings_read section agent (per agent) and
// section governor with a JSON round trip, as from the hive, and records
// writes. readErr and writeErr stand in for the endpoint's refusals.
type cadenceWBProvider struct {
	agents   map[string]map[string]any
	governor map[string]any
	readErr  error
	writeErr error
	requests []WriteRequest
}

func (p *cadenceWBProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if p.readErr != nil {
		return nil, p.readErr
	}
	if tool != ToolSettingsRead {
		return map[string]any{}, nil
	}
	var v any
	switch args["section"] {
	case "agent":
		a, ok := p.agents[fmt.Sprint(args["agent"])]
		if !ok {
			return nil, &HiveRefusalError{StatusCode: http.StatusNotFound, Message: "agent not found"}
		}
		v = a
	case SettingsSectionDefault:
		v = p.governor
	default:
		return map[string]any{}, nil
	}
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(data, &out)
	return out, err
}

func (p *cadenceWBProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	if p.writeErr != nil {
		return nil, p.writeErr
	}
	p.requests = append(p.requests, req)
	return map[string]any{"status": "updated"}, nil
}

// knuckleScanner is the knuckle hive's scanner on 2026-11-10: 4h in every
// mode.
func knuckleScanner() map[string]any {
	return map[string]any{
		"general":  map[string]any{"enabled": true},
		"cadences": map[string]any{"surge": 14400, "busy": 14400, "quiet": 14400, "idle": 14400},
		"models":   map[string]any{"surge": "", "busy": "", "quiet": "", "idle": ""},
	}
}

// knuckleWriteback is the knuckle hive's write-back settings on 2026-11-10:
// one override, stage_label without the opt-in label.
func knuckleWriteback() map[string]any {
	return map[string]any{"features": map[string]any{
		"writebackKinds":         map[string]any{"stage_label": map[string]any{"require_label": false}},
		"writebackRequireLabels": true,
		"writebackOptInLabels":   []string{"hive-review"},
	}}
}

func cadenceWBHandler(p *cadenceWBProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func newCadenceWBProvider() *cadenceWBProvider {
	return &cadenceWBProvider{agents: map[string]map[string]any{"scanner": knuckleScanner()}, governor: knuckleWriteback()}
}

func TestCadenceWritebackOpsRegisteredOnTheirEndpoints(t *testing.T) {
	for name, path := range map[string]string{
		WriteOpAgentCadences:       "PUT /api/config/agent/{name}/cadences",
		WriteOpWritebackKindPolicy: "PUT /api/config/governor/features",
	} {
		op := mustOp(t, name)
		if !strings.Contains(op.Description(), path) || !strings.Contains(op.Description(), "Owner only") {
			t.Errorf("%s description does not name %s and the owner-only rule", name, path)
		}
		if _, ok := op.(confirmGuardOp); !ok {
			t.Errorf("%s has no ConfirmGuard, so a stale preview could be confirmed", name)
		}
	}
}

// ---- agent.cadences ----

func TestAgentCadencesOpRejectsUnknownFields(t *testing.T) {
	p := newCadenceWBProvider()
	// A hive that configures no surge mode: the endpoint would skip it.
	p.agents["lean"] = map[string]any{"cadences": map[string]any{"idle": 3600}, "models": map[string]any{"idle": "", "quiet": ""}}
	h := cadenceWBHandler(p)
	for name, args := range map[string]map[string]any{
		"top-level typo":       {"agent": "scanner", "mode": map[string]any{"surge": "1h"}},
		"unknown mode":         {"agent": "scanner", "modes": map[string]any{"weekend": "1h"}},
		"not a duration":       {"agent": "scanner", "modes": map[string]any{"surge": "soon"}},
		"negative duration":    {"agent": "scanner", "modes": map[string]any{"surge": "-1h"}},
		"sub-second duration":  {"agent": "scanner", "modes": map[string]any{"surge": "1500ms"}},
		"number not string":    {"agent": "scanner", "modes": map[string]any{"surge": 3600}},
		"no mode":              {"agent": "scanner", "modes": map[string]any{}},
		"no agent":             {"modes": map[string]any{"surge": "1h"}},
		"unknown agent":        {"agent": "nobody", "modes": map[string]any{"surge": "1h"}},
		"mode not on the hive": {"agent": "lean", "modes": map[string]any{"surge": "1h"}},
		"already that cadence": {"agent": "scanner", "modes": map[string]any{"surge": "4h"}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpAgentCadences, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a refused preview minted a confirmation: %+v", pending)
	}
}

// Only the named modes are sent; a named mode already at that cadence is
// left out too, so the endpoint does not claim ownership of it.
func TestAgentCadencesOpKeepsUntouchedModes(t *testing.T) {
	p := newCadenceWBProvider()
	h := cadenceWBHandler(p)
	preview, id := capPreview(t, h, WriteOpAgentCadences, map[string]any{"agent": "scanner", "modes": map[string]any{"surge": "1h", "busy": "60m", "quiet": "4h"}})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/agent/scanner/cadences" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"busy":3600,"surge":3600}` {
		t.Fatalf("body = %s, want only surge and busy", got)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, want := range []string{"surge: 4h → 1h", "busy: 4h → 1h", "operator-owned", "fleet.autonomy_level"} {
		if !strings.Contains(effects, want) {
			t.Errorf("effects lack %q:\n%s", want, effects)
		}
	}
	if strings.Contains(effects, "quiet:") {
		t.Errorf("effects list the unchanged quiet mode:\n%s", effects)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"busy":3600,"surge":3600}` {
		t.Fatalf("executed %+v", p.requests)
	}
}

func TestAgentCadencesOpFlagsWidening(t *testing.T) {
	p := newCadenceWBProvider()
	p.agents["paused"] = map[string]any{"cadences": map[string]any{"surge": 0, "busy": 7200}, "models": map[string]any{"surge": "", "busy": "", "quiet": "", "idle": ""}}
	// An agent whose cadences are not plain intervals: continuous in surge, a
	// cron string in busy and a schedule object in quiet. An interval cannot
	// be compared with those, so the preview names them instead of guessing.
	p.agents["shifted"] = map[string]any{
		"cadences": map[string]any{"surge": "continuous", "busy": "0 9 * * 1-5", "quiet": map[string]any{"cron": "0 */6 * * *"}},
		"models":   map[string]any{"surge": "", "busy": "", "quiet": "", "idle": ""},
	}
	h := cadenceWBHandler(p)
	for name, tc := range map[string]struct {
		agent, mode, value string
		widening           bool
		want               string
	}{
		"shorter":              {"scanner", "surge", "1h", true, "scanner runs more often in surge: more spend"},
		"longer":               {"scanner", "idle", "8h", false, "runs less often in idle"},
		"pause":                {"scanner", "quiet", "pause", false, "stops running on a cadence in quiet"},
		"from paused":          {"paused", "surge", "6h", true, "paused runs more often in surge: more spend (every 6h, was paused)"},
		"from unset":           {"paused", "idle", "12h", true, "paused runs more often in idle: more spend"},
		"longer from 2h":       {"paused", "busy", "3h", false, "runs less often in busy"},
		"shorter from 2h":      {"paused", "busy", "30m", true, "paused runs more often in busy"},
		"seconds are exact":    {"scanner", "busy", "90m", true, "every 90m, was every 4h"},
		"from continuous":      {"shifted", "surge", "1h", false, "Narrowing: shifted runs every 1h in surge instead of continuously"},
		"from cron string":     {"shifted", "busy", "1h", true, "every 1h replaces 0 9 * * 1-5, which may have run less often"},
		"from schedule object": {"shifted", "quiet", "2h", true, `every 2h replaces schedule {"cron":"0 */6 * * *"}, which may have run less often`},
	} {
		t.Run(name, func(t *testing.T) {
			preview, _ := capPreview(t, h, WriteOpAgentCadences, map[string]any{"agent": tc.agent, "modes": map[string]any{tc.mode: tc.value}})
			if isWidening(preview) != tc.widening {
				t.Fatalf("widening = %v, want %v: %s", isWidening(preview), tc.widening, preview.WideningDisclosure)
			}
			if !strings.Contains(preview.WideningDisclosure, tc.want) {
				t.Fatalf("disclosure %q lacks %q", preview.WideningDisclosure, tc.want)
			}
		})
	}
}

func TestAgentCadencesOpRefusesStalePreview(t *testing.T) {
	p := newCadenceWBProvider()
	h := cadenceWBHandler(p)
	args := map[string]any{"agent": "scanner", "modes": map[string]any{"surge": "1h"}}
	_, id := capPreview(t, h, WriteOpAgentCadences, args)

	// Somebody edits another mode on the dashboard in between.
	p.agents["scanner"]["cadences"].(map[string]any)["quiet"] = 7200
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", p.requests)
	}

	_, id = capPreview(t, h, WriteOpAgentCadences, args)
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("fresh confirm: %v", err)
	}
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"surge":3600}` {
		t.Fatalf("executed %+v", p.requests)
	}
}

// The cadence endpoint is owner only. A caller the hive refuses gets no
// confirmation, and a confirm the endpoint refuses writes nothing.
func TestAgentCadencesOpRefusesNonOwner(t *testing.T) {
	cadenceWBRefusesNonOwner(t, WriteOpAgentCadences, map[string]any{"agent": "scanner", "modes": map[string]any{"surge": "1h"}})
}

func cadenceWBRefusesNonOwner(t *testing.T, op string, args map[string]any) {
	t.Helper()
	p := newCadenceWBProvider()
	p.readErr = fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	h := cadenceWBHandler(p)
	if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": op, "args": args}); err == nil || !strings.Contains(err.Error(), "owner access required") {
		t.Fatalf("refused read: preview err = %v, want the owner refusal", err)
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a refused read minted a confirmation: %+v", pending)
	}

	p.readErr = nil
	p.writeErr = &HiveRefusalError{StatusCode: http.StatusForbidden, Message: "owner access required"}
	_, id := capPreview(t, h, op, args)
	_, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id})
	if refusal, ok := HiveRefusalFromError(err); !ok || refusal.StatusCode != http.StatusForbidden {
		t.Fatalf("confirm err = %v, want the endpoint's 403", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}

// ---- writeback.kind_policy ----

func TestWritebackKindPolicyOpRejectsUnknownFields(t *testing.T) {
	h := cadenceWBHandler(newCadenceWBProvider())
	for name, args := range map[string]map[string]any{
		"top-level typo":          {"kind": map[string]any{"base_refresh": map[string]any{"min_level": 6}}},
		"no kinds":                {},
		"empty kinds":             {"kinds": map[string]any{}},
		"kinds not an object":     {"kinds": []any{"base_refresh"}},
		"unknown kind":            {"kinds": map[string]any{"base_refesh": map[string]any{"min_level": 6}}},
		"unknown field":           {"kinds": map[string]any{"base_refresh": map[string]any{"max_level": 6}}},
		"no field":                {"kinds": map[string]any{"base_refresh": map[string]any{}}},
		"level above 6":           {"kinds": map[string]any{"base_refresh": map[string]any{"min_level": 7}}},
		"negative level":          {"kinds": map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": -1}}},
		"fractional level":        {"kinds": map[string]any{"base_refresh": map[string]any{"min_level": 5.5}}},
		"level as string":         {"kinds": map[string]any{"base_refresh": map[string]any{"min_level": "6"}}},
		"flag as string":          {"kinds": map[string]any{"base_refresh": map[string]any{"require_label": "no"}}},
		"override not an object":  {"kinds": map[string]any{"base_refresh": 6}},
		"remove absent override":  {"kinds": map[string]any{"base_refresh": nil}},
		"drop field not set":      {"kinds": map[string]any{"stage_label": map[string]any{"min_level": nil}}},
		"same as the current":     {"kinds": map[string]any{"stage_label": map[string]any{"require_label": false}}},
		"same kind twice by case": {"kinds": map[string]any{"stage_label": nil, "Stage_Label": nil}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpWritebackKindPolicy, "args": args}); err == nil {
				t.Fatalf("args %v accepted", args)
			}
		})
	}
}

// The Features endpoint replaces the whole writebackKinds map, so a change
// to one kind must send every other override unchanged. On knuckle that is
// stage_label's require_label: false, which must survive the base_refresh
// change.
func TestWritebackKindPolicyOpKeepsUntouchedKinds(t *testing.T) {
	p := newCadenceWBProvider()
	h := cadenceWBHandler(p)
	preview, id := capPreview(t, h, WriteOpWritebackKindPolicy, map[string]any{"kinds": map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}}})
	want := `{"writebackKinds":{"base_refresh":{"unlabeled_min_level":6},"stage_label":{"require_label":false}}}`
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/governor/features" || bodyJSON(t, preview.Request.Body) != want {
		t.Fatalf("request = %s %s %s, want PUT /api/config/governor/features %s", preview.Request.Method, preview.Request.Path, bodyJSON(t, preview.Request.Body), want)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, s := range []string{"writeback.kinds.base_refresh: no override → {unlabeled_min_level: 6}", "L6 with the opt-in label → L6, any item", "(stage_label) are sent unchanged"} {
		if !strings.Contains(effects, s) {
			t.Errorf("effects lack %q:\n%s", s, effects)
		}
	}
	// Settings → Features' kind table now edits and keeps all four fields,
	// so the preview no longer warns that a dashboard save would drop them.
	if strings.Contains(effects, "would drop") {
		t.Errorf("effects still warn that the dashboard drops fields:\n%s", effects)
	}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != want {
		t.Fatalf("executed %+v", p.requests)
	}

	for name, tc := range map[string]struct {
		replace bool // replace knuckle's overrides with kinds
		kinds   map[string]any
		args    map[string]any
		want    string
	}{
		"a field merges into the override": {
			args: map[string]any{"stage_label": map[string]any{"min_level": 6}},
			want: `{"writebackKinds":{"stage_label":{"min_level":6,"require_label":false}}}`,
		},
		"null drops one field": {
			replace: true,
			kinds:   map[string]any{"stage_label": map[string]any{"require_label": false, "min_level": 6}},
			args:    map[string]any{"stage_label": map[string]any{"min_level": nil}},
			want:    `{"writebackKinds":{"stage_label":{"require_label":false}}}`,
		},
		// One row per droppable field: each has its own arm in
		// applyWritebackKindChange, and dropping one must leave the others.
		"null drops require_label": {
			replace: true,
			kinds:   map[string]any{"stage_label": map[string]any{"require_label": false, "min_level": 6}},
			args:    map[string]any{"stage_label": map[string]any{"require_label": nil}},
			want:    `{"writebackKinds":{"stage_label":{"min_level":6}}}`,
		},
		"null drops unlabeled_min_level": {
			replace: true,
			kinds:   map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6, "min_level": 5}},
			args:    map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": nil}},
			want:    `{"writebackKinds":{"base_refresh":{"min_level":5}}}`,
		},
		"null drops own_only": {
			replace: true,
			kinds:   map[string]any{"hold_gate_notice": map[string]any{"own_only": true, "require_label": false}},
			args:    map[string]any{"hold_gate_notice": map[string]any{"own_only": nil}},
			want:    `{"writebackKinds":{"hold_gate_notice":{"require_label":false}}}`,
		},
		"null removes only that kind": {
			replace: true,
			kinds:   map[string]any{"stage_label": map[string]any{"require_label": false}, "sentinel": map[string]any{"min_level": 6}},
			args:    map[string]any{"sentinel": nil},
			want:    `{"writebackKinds":{"stage_label":{"require_label":false}}}`,
		},
		"removing the last override sends an empty map": {
			args: map[string]any{"stage_label": nil},
			want: `{"writebackKinds":{}}`,
		},
		"no overrides yet": {
			replace: true,
			kinds:   nil,
			args:    map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}},
			want:    `{"writebackKinds":{"base_refresh":{"unlabeled_min_level":6}}}`,
		},
		"the dependency-bot rebase request kind": {
			replace: true,
			kinds:   nil,
			args:    map[string]any{"bot_rebase_request": map[string]any{"unlabeled_min_level": 6}},
			want:    `{"writebackKinds":{"bot_rebase_request":{"unlabeled_min_level":6}}}`,
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newCadenceWBProvider()
			if tc.replace {
				p.governor["features"].(map[string]any)["writebackKinds"] = tc.kinds
			}
			preview, _ := capPreview(t, cadenceWBHandler(p), WriteOpWritebackKindPolicy, map[string]any{"kinds": tc.args})
			if got := bodyJSON(t, preview.Request.Body); got != tc.want {
				t.Fatalf("body = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestWritebackKindPolicyOpFlagsWidening(t *testing.T) {
	for name, tc := range map[string]struct {
		kinds         map[string]any
		requireLabels bool
		widening      bool
		want          string
	}{
		"unlabeled level added": {
			kinds: map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}}, requireLabels: true, widening: true,
			want: "base_refresh now writes on items without the opt-in label (hive-review) from L6; before, it needed the label",
		},
		"min level lowered": {
			kinds: map[string]any{"ci_rerun": map[string]any{"min_level": 5}}, requireLabels: true, widening: true,
			want: "ci_rerun now writes on opted-in items from L5 (was L6)",
		},
		"label no longer required": {
			kinds: map[string]any{"issue_close": map[string]any{"require_label": false}}, requireLabels: true, widening: true,
			want: "issue_close now writes on items without the opt-in label (hive-review) from L5",
		},
		"own_only lifted": {
			kinds: map[string]any{"hold_gate_notice": map[string]any{"own_only": false}}, requireLabels: true, widening: true,
			want: "hold_gate_notice now writes on items the hive did not author",
		},
		"label required again": {
			kinds: map[string]any{"stage_label": nil}, requireLabels: true, widening: false,
			want: "Narrowing: stage_label no longer writes on items without the opt-in label (was from L5)",
		},
		"min level raised": {
			kinds: map[string]any{"verdict_label": map[string]any{"min_level": 5}}, requireLabels: true, widening: false,
			want: "verdict_label writes on opted-in items from L5 only (was L3)",
		},
		"labels off hive-wide": {
			kinds: map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}}, requireLabels: false, widening: false,
			want: "No widening",
		},
	} {
		t.Run(name, func(t *testing.T) {
			p := newCadenceWBProvider()
			p.governor["features"].(map[string]any)["writebackRequireLabels"] = tc.requireLabels
			preview, _ := capPreview(t, cadenceWBHandler(p), WriteOpWritebackKindPolicy, map[string]any{"kinds": tc.kinds})
			if isWidening(preview) != tc.widening {
				t.Fatalf("widening = %v, want %v: %s", isWidening(preview), tc.widening, preview.WideningDisclosure)
			}
			if !strings.Contains(preview.WideningDisclosure, tc.want) {
				t.Fatalf("disclosure %q lacks %q", preview.WideningDisclosure, tc.want)
			}
		})
	}
}

func TestWritebackKindPolicyOpRefusesStalePreview(t *testing.T) {
	p := newCadenceWBProvider()
	h := cadenceWBHandler(p)
	args := map[string]any{"kinds": map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}}}
	_, id := capPreview(t, h, WriteOpWritebackKindPolicy, args)

	// Somebody saves another kind's override on Settings → Features in
	// between; confirming the old preview would wipe it.
	p.governor["features"].(map[string]any)["writebackKinds"] = map[string]any{"stage_label": map[string]any{"require_label": false}, "ci_rerun": map[string]any{"min_level": 5}}
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirm wrote %+v", p.requests)
	}

	_, id = capPreview(t, h, WriteOpWritebackKindPolicy, args)
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("fresh confirm: %v", err)
	}
	want := `{"writebackKinds":{"base_refresh":{"unlabeled_min_level":6},"ci_rerun":{"min_level":5},"stage_label":{"require_label":false}}}`
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != want {
		t.Fatalf("executed %+v, want body %s", p.requests, want)
	}
}

func TestWritebackKindPolicyOpRefusesNonOwner(t *testing.T) {
	cadenceWBRefusesNonOwner(t, WriteOpWritebackKindPolicy, map[string]any{"kinds": map[string]any{"base_refresh": map[string]any{"unlabeled_min_level": 6}}})
}
