package adminmcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// govGeneralProvider answers settings_read section general_advanced with a
// JSON round trip, as from the hive, and records writes. readErr stands in
// for the endpoint's owner-only refusal.
type govGeneralProvider struct {
	settings map[string]any
	readErr  error
	requests []WriteRequest
}

func (p *govGeneralProvider) Read(_ context.Context, tool string, args map[string]any) (any, error) {
	if tool != ToolSettingsRead || args["section"] != governorGeneralSettingsSection {
		return nil, fmt.Errorf("unexpected read %s %v", tool, args)
	}
	if p.readErr != nil {
		return nil, p.readErr
	}
	data, err := json.Marshal(p.settings)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	err = json.Unmarshal(data, &out)
	return out, err
}

func (p *govGeneralProvider) ExecuteWrite(_ context.Context, req WriteRequest) (any, error) {
	p.requests = append(p.requests, req)
	return map[string]any{"status": "updated"}, nil
}

// govGeneralFixture is GET /api/config/governor/general-advanced for a hive
// polling every 120 seconds with pacing off.
func govGeneralFixture() map[string]any {
	return map[string]any{
		"eval_interval_s":                  120,
		"attribution_trailer":              true,
		"explain_mode":                     "",
		"explain_mode_effective":           "off",
		"explain_mode_source":              "default",
		"eval_interval_max_s":              1800,
		"eval_interval_webhook_s":          900,
		"webhook_wake_debounce_s":          10,
		"webhook_wake_min_gap_s":           30,
		"conserve_interval_multiplier":     3,
		"optional_sweep_every_n_cycles":    1,
		"provider_budget_probe_interval_s": 0,
		"api_budget_pacing":                false,
		"api_budget_pacing_state":          map[string]any{"enabled": false, "active": false, "factor": 1, "effective_interval_s": 120, "burn_per_min": 0, "reason": "off (governor.api_budget_pacing)"},
		"thresholds_source":                "",
		"loop_defaults": map[string]any{
			"eval_interval_max_s": 1800, "eval_interval_webhook_s": 900, "webhook_wake_debounce_s": 10, "webhook_wake_min_gap_s": 30,
			"conserve_interval_multiplier": 2, "optional_sweep_every_n_cycles": 1, "provider_budget_probe_interval_s": 1800,
		},
	}
}

func govGeneralHandler(p *govGeneralProvider) *Handler {
	return NewHandler(p, WithWritesEnabled(true), WithPendingStore(NewMemoryPendingStore()), WithHiveID("hive-a"))
}

func govGeneralConfirm(t *testing.T, h *Handler, id string) {
	t.Helper()
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
}

func TestGovernorGeneralSettingsRegisteredOnItsEndpoint(t *testing.T) {
	op := mustOp(t, WriteOpGovernorGeneralSettings)
	if !strings.Contains(op.Description(), "PUT /api/config/governor/general-advanced") || !strings.Contains(op.Description(), "Owner only") {
		t.Errorf("description does not name the endpoint and the owner-only rule: %s", op.Description())
	}
	if _, ok := op.(confirmGuardOp); !ok {
		t.Error("no ConfirmGuard, so a stale preview could be confirmed")
	}
	if _, ok := op.(providerPreviewOp); !ok {
		t.Error("no PreviewWithProvider, so the preview could not show before and after")
	}
	schema := op.InputSchema()
	if schema["additionalProperties"] != false {
		t.Error("the schema accepts unknown fields")
	}
	props, _ := schema["properties"].(map[string]any)
	var got []string
	for name := range props {
		got = append(got, name)
	}
	want := GovernorGeneralSettingsFields()
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("schema properties = %v, want %v", got, want)
	}
	for _, name := range []string{"eval_interval_s", "api_budget_pacing"} {
		if _, ok := props[name]; !ok {
			t.Errorf("the schema lacks %s", name)
		}
	}
}

// Turning pacing on: the diff, the request and the bytes that are sent.
func TestGovernorGeneralSettingsPreviewsAndSendsPacingOnly(t *testing.T) {
	p := &govGeneralProvider{settings: govGeneralFixture()}
	h := govGeneralHandler(p)
	preview, id := capPreview(t, h, WriteOpGovernorGeneralSettings, map[string]any{"api_budget_pacing": true})
	if preview.Request.Method != http.MethodPut || preview.Request.Path != "/api/config/governor/general-advanced" {
		t.Fatalf("request = %s %s", preview.Request.Method, preview.Request.Path)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"api_budget_pacing":true}` {
		t.Fatalf("body = %s, want only api_budget_pacing", got)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, want := range []string{"api_budget_pacing: off → on", "never shortened", "config_governor_general_advanced", "next eval cycle"} {
		if !strings.Contains(effects, want) {
			t.Errorf("effects lack %q:\n%s", want, effects)
		}
	}
	if isWidening(preview) || !strings.Contains(preview.WideningDisclosure, "no more often than today") {
		t.Errorf("turning pacing on must not read as widening: %s", preview.WideningDisclosure)
	}
	if got := bodyJSON(t, preview.Details["changes"]); got != `[{"after":true,"before":false,"field":"api_budget_pacing","note":"the eval and sweep intervals lengthen when the hive's GitHub call rate would use up the budget before the rate-limit window resets; they are never shortened"}]` {
		t.Errorf("details changes = %s", got)
	}
	if preview.StateFingerprint == "" {
		t.Error("the preview recorded no fingerprint")
	}
	govGeneralConfirm(t, h, id)
	if len(p.requests) != 1 || p.requests[0].Method != http.MethodPut || p.requests[0].Path != "/api/config/governor/general-advanced" {
		t.Fatalf("executed %+v", p.requests)
	}
	if got := bodyJSON(t, p.requests[0].Body); got != `{"api_budget_pacing":true}` {
		t.Fatalf("sent %s, want only the named field", got)
	}
}

// Setting the eval interval: only that field is sent, and a shorter interval
// is disclosed as widening.
func TestGovernorGeneralSettingsPreviewsAndSendsEvalIntervalOnly(t *testing.T) {
	p := &govGeneralProvider{settings: govGeneralFixture()}
	h := govGeneralHandler(p)
	preview, id := capPreview(t, h, WriteOpGovernorGeneralSettings, map[string]any{"eval_interval_s": 300})
	if got := bodyJSON(t, preview.Request.Body); got != `{"eval_interval_s":300}` {
		t.Fatalf("body = %s, want only eval_interval_s", got)
	}
	if effects := strings.Join(preview.Effects, "\n"); !strings.Contains(effects, "eval_interval_s: 120 → 300") {
		t.Errorf("effects lack the before → after line:\n%s", effects)
	}
	if isWidening(preview) {
		t.Errorf("a longer interval read as widening: %s", preview.WideningDisclosure)
	}
	govGeneralConfirm(t, h, id)
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"eval_interval_s":300}` {
		t.Fatalf("executed %+v", p.requests)
	}

	shorter, _ := capPreview(t, h, WriteOpGovernorGeneralSettings, map[string]any{"eval_interval_s": 60})
	if !isWidening(shorter) || !strings.Contains(shorter.WideningDisclosure, "eval_interval_s goes from 120 to 60") || !strings.Contains(shorter.WideningDisclosure, "calls GitHub, more often") {
		t.Errorf("a shorter interval was not disclosed as widening: %s", shorter.WideningDisclosure)
	}
}

func TestGovernorGeneralSettingsTurningPacingOffIsWidening(t *testing.T) {
	fixture := govGeneralFixture()
	fixture["api_budget_pacing"] = true
	h := govGeneralHandler(&govGeneralProvider{settings: fixture})
	preview, _ := capPreview(t, h, WriteOpGovernorGeneralSettings, map[string]any{"api_budget_pacing": false})
	if !isWidening(preview) || !strings.Contains(preview.WideningDisclosure, "before the window resets") {
		t.Fatalf("disclosure = %s", preview.WideningDisclosure)
	}
	if got := bodyJSON(t, preview.Request.Body); got != `{"api_budget_pacing":false}` {
		t.Fatalf("body = %s", got)
	}
}

// A named field already at that value is left out of the request; 0 on a
// loop knob is sent as 0 and shown as the default it restores.
func TestGovernorGeneralSettingsSendsOnlyWhatChanges(t *testing.T) {
	p := &govGeneralProvider{settings: govGeneralFixture()}
	h := govGeneralHandler(p)
	preview, _ := capPreview(t, h, WriteOpGovernorGeneralSettings, map[string]any{
		"eval_interval_s": 120, "attribution_trailer": true, "explain_mode": "brief",
		"conserve_interval_multiplier": 0, "provider_budget_probe_interval_s": 1800,
	})
	if got := bodyJSON(t, preview.Request.Body); got != `{"conserve_interval_multiplier":0,"explain_mode":"brief"}` {
		t.Fatalf("body = %s", got)
	}
	effects := strings.Join(preview.Effects, "\n")
	for _, want := range []string{
		"conserve_interval_multiplier: 3 → 2 (0 restores the default)",
		"explain_mode: not set → brief",
		"Already as requested, so not sent: eval_interval_s, provider_budget_probe_interval_s, attribution_trailer.",
	} {
		if !strings.Contains(effects, want) {
			t.Errorf("effects lack %q:\n%s", want, effects)
		}
	}
	if !isWidening(preview) || !strings.Contains(preview.WideningDisclosure, "conserve_interval_multiplier goes from 3 to 2") {
		t.Errorf("a lower conserve multiplier was not disclosed: %s", preview.WideningDisclosure)
	}
}

func TestGovernorGeneralSettingsRejectsBadArguments(t *testing.T) {
	p := &govGeneralProvider{settings: govGeneralFixture()}
	h := govGeneralHandler(p)
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"unknown field":         {map[string]any{"eval_interval": 300}, `unsupported field "eval_interval"`},
		"unknown beside known":  {map[string]any{"api_budget_pacing": true, "pacing": true}, `unsupported field "pacing"`},
		"no field":              {map[string]any{}, "names no setting"},
		"pacing not a bool":     {map[string]any{"api_budget_pacing": "yes"}, "api_budget_pacing must be true or false"},
		"interval too short":    {map[string]any{"eval_interval_s": 5}, "eval_interval_s must be between 10 and 86400"},
		"interval too long":     {map[string]any{"eval_interval_s": 90000}, "eval_interval_s must be between 10 and 86400"},
		"interval not whole":    {map[string]any{"eval_interval_s": 12.5}, "eval_interval_s must be a whole number"},
		"negative knob":         {map[string]any{"webhook_wake_min_gap_s": -1}, "webhook_wake_min_gap_s must be 0 (default) or greater"},
		"knob over a day":       {map[string]any{"eval_interval_max_s": 90000}, "eval_interval_max_s must be at most 86400"},
		"unknown explain mode":  {map[string]any{"explain_mode": "verbose"}, "explain_mode must be off, brief or full"},
		"max under interval":    {map[string]any{"eval_interval_max_s": 60}, "eval_interval_max_s (60) must be greater than or equal to eval_interval_s (120)"},
		"interval over max":     {map[string]any{"eval_interval_s": 3600}, "eval_interval_max_s (1800) must be greater than or equal to eval_interval_s (3600)"},
		"already as requested":  {map[string]any{"eval_interval_s": 120, "api_budget_pacing": false}, "changes nothing"},
		"default already there": {map[string]any{"eval_interval_webhook_s": 0}, "changes nothing"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorGeneralSettings, "args": tc.args})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("args %v: err = %v, want %q", tc.args, err, tc.want)
			}
		})
	}
	// An unknown field is refused with the list of fields that are accepted.
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorGeneralSettings, "args": map[string]any{"eval_interval": 300}})
	for _, field := range GovernorGeneralSettingsFields() {
		if err == nil || !strings.Contains(err.Error(), field) {
			t.Errorf("the unknown-field refusal does not list %s: %v", field, err)
		}
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a refused preview minted a confirmation: %+v", pending)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a refused preview wrote: %+v", p.requests)
	}
}

// A setting the change names moved after the preview: the confirm is refused.
// A setting it does not name may move freely.
func TestGovernorGeneralSettingsRefusesStalePreview(t *testing.T) {
	p := &govGeneralProvider{settings: govGeneralFixture()}
	h := govGeneralHandler(p)
	args := map[string]any{"eval_interval_s": 300}
	_, id := capPreview(t, h, WriteOpGovernorGeneralSettings, args)
	// Somebody saves a different interval on the dashboard in between.
	p.settings["eval_interval_s"] = 240
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); !errors.Is(err, ErrConfirmationStale) {
		t.Fatalf("stale confirm err = %v, want ErrConfirmationStale", err)
	}
	if len(p.requests) != 0 {
		t.Fatalf("a stale confirm wrote: %+v", p.requests)
	}

	_, id = capPreview(t, h, WriteOpGovernorGeneralSettings, args)
	p.settings["explain_mode"] = "brief"
	govGeneralConfirm(t, h, id)
	if len(p.requests) != 1 || bodyJSON(t, p.requests[0].Body) != `{"eval_interval_s":300}` {
		t.Fatalf("executed %+v", p.requests)
	}
}

// The settings are owner only. A caller the hive does not verify as owner
// cannot read them, so the preview is refused with no confirmation minted;
// and a confirmation whose re-read is refused writes nothing.
func TestGovernorGeneralSettingsRefusesNonOwner(t *testing.T) {
	ownerOnly := fmt.Errorf("%w: Hive API returned HTTP 403: owner access required", ErrForbidden)
	p := &govGeneralProvider{settings: govGeneralFixture(), readErr: ownerOnly}
	h := govGeneralHandler(p)
	args := map[string]any{"api_budget_pacing": true}
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorGeneralSettings, "args": args})
	if err == nil || !errors.Is(err, ErrForbidden) || !strings.Contains(err.Error(), "verified owner") {
		t.Fatalf("non-owner preview err = %v", err)
	}
	if pending, _ := h.PendingStore.List(context.Background()); len(pending) != 0 {
		t.Fatalf("a non-owner preview minted a confirmation: %+v", pending)
	}

	p.readErr = nil
	_, id := capPreview(t, h, WriteOpGovernorGeneralSettings, args)
	p.readErr = ownerOnly
	if _, err := h.ConfirmWrite(context.Background(), map[string]any{"confirmation_id": id}); err == nil {
		t.Fatal("a confirm whose re-read was refused went through")
	}
	if len(p.requests) != 0 {
		t.Fatalf("wrote without owner access: %+v", p.requests)
	}
}

// A hive older than the pacing switch does not report it: the change is
// refused instead of being sent to an endpoint that would ignore it.
func TestGovernorGeneralSettingsRefusesAFieldTheHiveDoesNotReport(t *testing.T) {
	fixture := govGeneralFixture()
	delete(fixture, "api_budget_pacing")
	h := govGeneralHandler(&govGeneralProvider{settings: fixture})
	_, err := h.PreviewWrite(context.Background(), map[string]any{"operation": WriteOpGovernorGeneralSettings, "args": map[string]any{"api_budget_pacing": true}})
	if err == nil || !strings.Contains(err.Error(), "api_budget_pacing") || !strings.Contains(err.Error(), "older") {
		t.Fatalf("err = %v", err)
	}
	// Without a provider nothing is read, so nothing is previewed as a change.
	op := mustOp(t, WriteOpGovernorGeneralSettings)
	bare, err := op.Preview(context.Background(), map[string]any{"api_budget_pacing": true})
	if err != nil || bare.Request.Body != nil || bare.Request.Path != "/api/config/governor/general-advanced" || bare.StateFingerprint != "" {
		t.Fatalf("provider-less preview = %+v, err %v", bare, err)
	}
}
