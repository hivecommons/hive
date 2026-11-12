package adminmcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// governor.general_settings changes the settings on Settings → General that
// save through PUT /api/config/governor/general-advanced: the eval interval,
// the eval-loop pacing knobs, API budget pacing, the default explain mode and
// the attribution trailer. The endpoint is owner only and takes a body of
// pointers, so only the fields sent change.
//
// The preview reads the current values (settings_read section
// general_advanced), shows each change before and after, sends only the
// fields that differ, and records a fingerprint of the before-values it read.
// ConfirmGuard reads them again at write_confirm, which refuses with
// ErrConfirmationStale when one of them moved in between.
//
// It is named for the tab, like governor.feature_settings, because the
// endpoint carries more than the loop settings.
const (
	WriteOpGovernorGeneralSettings = "governor.general_settings"

	governorGeneralSettingsPath = "/api/config/governor/general-advanced"

	// governorGeneralSettingsSection is the settings_read section the preview reads.
	governorGeneralSettingsSection = "general_advanced"

	// The endpoint's bounds (pkg/dashboard: minGeneralEvalIntervalS,
	// maxGeneralEvalIntervalS).
	governorGeneralMinEvalIntervalS = 10
	governorGeneralMaxSeconds       = 86400
)

type governorGeneralSettingsOp struct{}

// governorGeneralField is one field of the endpoint's body.
type governorGeneralField struct {
	name string
	kind string // "bool", "mode", "interval" or "loop"
	// seconds bounds a loop field at governorGeneralMaxSeconds.
	seconds bool
	desc    string
	// lower says what a lower number lets the hive do, for the preview's
	// widening disclosure.
	lower string
}

// governorGeneralFields is every field of the endpoint's body, in the order
// a preview lists them. TestGeneralAdvancedBodyMatchesAdminMCPOp
// (pkg/dashboard) fails when the handler's body gains a field missing here.
var governorGeneralFields = []governorGeneralField{
	{name: "eval_interval_s", kind: "interval", lower: "the governor evaluates, and calls GitHub, more often", desc: "governor.eval_interval_s: seconds between governor evaluations, 10 to 86400."},
	{name: "api_budget_pacing", kind: "bool", desc: "governor.api_budget_pacing: spread the hive's GitHub API calls over the rate-limit window by lengthening the eval and sweep intervals when the current rate would use up the budget before the reset (default off)."},
	{name: "eval_interval_max_s", kind: "loop", seconds: true, lower: "a loop slowed down for a low GitHub budget is slowed less", desc: "governor.eval_interval_max_s: cap on the eval interval after any stretch; at least eval_interval_s. 0 restores the default (1800)."},
	{name: "eval_interval_webhook_s", kind: "loop", seconds: true, lower: "the governor evaluates, and calls GitHub, more often while webhooks are healthy", desc: "governor.eval_interval_webhook_s: base interval while GitHub webhooks are healthy. 0 restores the default (900)."},
	{name: "webhook_wake_debounce_s", kind: "loop", seconds: true, lower: "webhook deliveries are grouped less, so they can wake more eval cycles", desc: "governor.webhook_wake_debounce_s: how long a webhook delivery waits for more before it wakes an early eval cycle. 0 restores the default (10)."},
	{name: "webhook_wake_min_gap_s", kind: "loop", seconds: true, lower: "webhook deliveries can wake eval cycles more often", desc: "governor.webhook_wake_min_gap_s: shortest gap between two webhook-triggered eval cycles. 0 restores the default (30)."},
	{name: "conserve_interval_multiplier", kind: "loop", lower: "the loop slows down less when the GitHub budget is low", desc: "governor.conserve_interval_multiplier: eval interval multiplier in API budget conserve mode. 0 restores the default (2)."},
	{name: "optional_sweep_every_n_cycles", kind: "loop", lower: "the optional eval steps, and their GitHub calls, run more often", desc: "governor.optional_sweep_every_n_cycles: run the optional eval steps every Nth normal cycle. 0 restores the default (1)."},
	{name: "provider_budget_probe_interval_s", kind: "loop", seconds: true, lower: "a provider that refused on a spending limit is probed again sooner, which can spend a run", desc: "governor.provider_budget.probe_interval_s: how long a provider spending-limit refusal withholds kicks before one cycle probes again. 0 uses the default (1800)."},
	{name: "explain_mode", kind: "mode", desc: "governor.explain_mode: the hive-wide default explain mode, one of off, brief or full, or empty to clear it (HIVE_EXPLAIN_MODE then decides)."},
	{name: "attribution_trailer", kind: "bool", desc: "governor.attribution_trailer: append the visible attribution line to PRs and issues the hive creates (default on)."},
}

// GovernorGeneralSettingsFields lists the body fields
// governor.general_settings accepts.
func GovernorGeneralSettingsFields() []string {
	out := make([]string, 0, len(governorGeneralFields))
	for _, f := range governorGeneralFields {
		out = append(out, f.name)
	}
	return out
}

func (governorGeneralSettingsOp) Name() string { return WriteOpGovernorGeneralSettings }
func (governorGeneralSettingsOp) Description() string {
	return "Change the Settings → General governor settings through PUT " + governorGeneralSettingsPath + ", the endpoint the General tab saves through. Owner only. Pass only what changes: " + strings.Join(GovernorGeneralSettingsFields(), ", ") + ". eval_interval_s sets how often the governor evaluates; api_budget_pacing turns GitHub API budget pacing on or off. The preview reads the current settings, shows each change before and after, and says when the hive would call GitHub more often. Fields already at the requested value are not sent. write_confirm is refused if a setting this change names moved after the preview. The loop reads these every cycle, so a change applies from the next eval cycle without a restart. This operation kicks, merges and labels nothing."
}

func (governorGeneralSettingsOp) InputSchema() map[string]any {
	props := map[string]any{}
	for _, f := range governorGeneralFields {
		switch f.kind {
		case "bool":
			props[f.name] = map[string]any{"type": "boolean", "description": f.desc}
		case "mode":
			props[f.name] = map[string]any{"type": "string", "enum": []string{"", config.ExplainModeOff, config.ExplainModeBrief, config.ExplainModeFull}, "description": f.desc}
		case "interval":
			props[f.name] = map[string]any{"type": "integer", "minimum": governorGeneralMinEvalIntervalS, "maximum": governorGeneralMaxSeconds, "description": f.desc}
		default:
			props[f.name] = map[string]any{"type": "integer", "minimum": 0, "description": f.desc}
		}
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{}, "additionalProperties": false}
}

// governorGeneralParseArgs checks the arguments with the endpoint's own
// bounds and returns the requested value per field.
func governorGeneralParseArgs(args map[string]any) (map[string]any, error) {
	names := GovernorGeneralSettingsFields()
	if err := rejectUnknownKeys(args, WriteOpGovernorGeneralSettings, names...); err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, f := range governorGeneralFields {
		raw, present := args[f.name]
		if !present {
			continue
		}
		switch f.kind {
		case "bool":
			v, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be true or false", f.name)
			}
			out[f.name] = v
		case "mode":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be %s, %s or %s, or empty to clear it", f.name, config.ExplainModeOff, config.ExplainModeBrief, config.ExplainModeFull)
			}
			s = strings.TrimSpace(s)
			if !config.ValidateExplainMode(s) {
				return nil, fmt.Errorf("%s must be %s, %s or %s, or empty to clear it", f.name, config.ExplainModeOff, config.ExplainModeBrief, config.ExplainModeFull)
			}
			out[f.name] = s
		default:
			n, ok, err := optionalIntArg(args, f.name)
			if err != nil || !ok {
				return nil, fmt.Errorf("%s must be a whole number", f.name)
			}
			switch {
			case f.kind == "interval" && (n < governorGeneralMinEvalIntervalS || n > governorGeneralMaxSeconds):
				return nil, fmt.Errorf("%s must be between %d and %d", f.name, governorGeneralMinEvalIntervalS, governorGeneralMaxSeconds)
			case n < 0:
				return nil, fmt.Errorf("%s must be 0 (default) or greater", f.name)
			case f.seconds && n > governorGeneralMaxSeconds:
				return nil, fmt.Errorf("%s must be at most %d", f.name, governorGeneralMaxSeconds)
			}
			out[f.name] = n
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s names no setting: give one or more of %s", WriteOpGovernorGeneralSettings, strings.Join(names, ", "))
	}
	return out, nil
}

// Preview without a provider checks the arguments and names the endpoint;
// write_preview always goes through PreviewWithProvider, and write_confirm
// uses this only to check the method and path.
func (governorGeneralSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := governorGeneralParseArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpGovernorGeneralSettings, "Settings → General governor settings", governorGeneralSettingsPath, "General governor settings"), nil
}

// ConfirmGuard reads the settings again at write_confirm and returns the
// fingerprint of the before-values, which must still match the preview's.
func (op governorGeneralSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// governorGeneralRead reads settings_read section general_advanced (GET
// /api/config/governor/general-advanced), which only a verified owner may read.
func governorGeneralRead(ctx context.Context, provider Provider) (map[string]any, error) {
	if provider == nil {
		return nil, fmt.Errorf("%s reads the current General settings first, and this connection cannot read them; nothing was previewed", WriteOpGovernorGeneralSettings)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": governorGeneralSettingsSection})
	if err != nil {
		if errors.Is(err, ErrForbidden) {
			return nil, fmt.Errorf("%s needs the current General settings, which only the hive's verified owner can read: %w", WriteOpGovernorGeneralSettings, err)
		}
		return nil, fmt.Errorf("%s could not read the current General settings (settings_read section %s): %w; nothing was previewed", WriteOpGovernorGeneralSettings, governorGeneralSettingsSection, err)
	}
	m, _ := data.(map[string]any)
	if len(m) == 0 {
		return nil, fmt.Errorf("%s could not read the current General settings: settings_read section %s returned no object; nothing was previewed", WriteOpGovernorGeneralSettings, governorGeneralSettingsSection)
	}
	return m, nil
}

// governorGeneralResolved is the value a loop field ends up with: 0 restores
// the default the hive reports (loop_defaults).
func governorGeneralResolved(f governorGeneralField, want int, defaults map[string]any) int {
	if want != 0 || f.kind != "loop" {
		return want
	}
	if def, ok := numberArg(defaults[f.name]); ok && def > 0 {
		return def
	}
	return want
}

// PreviewWithProvider reads the current settings and builds the change.
func (governorGeneralSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	want, err := governorGeneralParseArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	current, err := governorGeneralRead(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	defaults, _ := current["loop_defaults"].(map[string]any)
	body := map[string]any{}
	before := map[string]any{}
	after := map[string]int{}
	var changes []settingChange
	var unchanged, widen, narrow []string
	for _, f := range governorGeneralFields {
		v, named := want[f.name]
		if !named {
			continue
		}
		raw, present := current[f.name]
		if !present {
			return WritePreview{}, missingSetting("the General setting "+f.name, f.name)
		}
		c := settingChange{Field: f.name}
		switch f.kind {
		case "bool":
			was, ok := raw.(bool)
			if !ok {
				return WritePreview{}, missingSetting("the General setting "+f.name, f.name)
			}
			before[f.name] = was
			if was == v.(bool) {
				unchanged = append(unchanged, f.name)
				continue
			}
			c.Before, c.After = was, v
			if f.name == "api_budget_pacing" {
				if v.(bool) {
					c.Note = "the eval and sweep intervals lengthen when the hive's GitHub call rate would use up the budget before the rate-limit window resets; they are never shortened"
					narrow = append(narrow, "with pacing on the hive calls GitHub no more often than today, and less often when the budget would otherwise run out")
				} else {
					c.Note = "the loop goes back to its usual interval even when that spends the GitHub budget before the reset"
					widen = append(widen, "with pacing off the hive's loops may call GitHub more often than pacing allowed, and can spend the rate-limit budget before the window resets")
				}
			}
		case "mode":
			was, ok := raw.(string)
			if !ok {
				return WritePreview{}, missingSetting("the General setting "+f.name, f.name)
			}
			before[f.name] = was
			if was == v.(string) {
				unchanged = append(unchanged, f.name)
				continue
			}
			c.Before, c.After = governorGeneralModeLabel(was), governorGeneralModeLabel(v.(string))
		default:
			was, ok := numberArg(raw)
			if !ok {
				return WritePreview{}, missingSetting("the General setting "+f.name, f.name)
			}
			before[f.name] = was
			// The hive keeps the provider probe interval's 0 as 0 and reads
			// it as the default, so both sides are compared resolved.
			was = governorGeneralResolved(f, was, defaults)
			resolved := governorGeneralResolved(f, v.(int), defaults)
			after[f.name] = resolved
			if was == resolved {
				unchanged = append(unchanged, f.name)
				continue
			}
			c.Before, c.After = was, resolved
			if v.(int) == 0 && resolved != 0 {
				c.Note = "0 restores the default"
			}
			if resolved < was && f.lower != "" {
				widen = append(widen, fmt.Sprintf("%s goes from %d to %d: %s", f.name, was, resolved, f.lower))
			}
		}
		body[f.name] = v
		changes = append(changes, c)
	}
	if len(changes) == 0 {
		return WritePreview{}, fmt.Errorf("%s changes nothing: %s already as requested", WriteOpGovernorGeneralSettings, listLabel(unchanged))
	}
	// The endpoint's own cross-field check, on the values it would hold.
	if _, sent := body["eval_interval_s"]; sent || body["eval_interval_max_s"] != nil {
		base, baseOK := after["eval_interval_s"]
		if !baseOK {
			base, _ = numberArg(current["eval_interval_s"])
		}
		maxS, maxOK := after["eval_interval_max_s"]
		if !maxOK {
			maxS, _ = numberArg(current["eval_interval_max_s"])
		}
		if maxS > 0 && base > 0 && maxS < base {
			return WritePreview{}, fmt.Errorf("eval_interval_max_s (%d) must be greater than or equal to eval_interval_s (%d)", maxS, base)
		}
	}
	fingerprint, err := stateFingerprint(before)
	if err != nil {
		return WritePreview{}, err
	}
	fields := make([]string, 0, len(changes))
	effects := make([]string, 0, len(changes)+4)
	for _, c := range changes {
		fields = append(fields, c.Field)
		effects = append(effects, c.effect())
	}
	effects = append(effects,
		"Hive sends PUT "+governorGeneralSettingsPath+" with only the fields listed here; every other General setting is kept.",
		"The endpoint validates the change, saves it to hive.yaml and the dashboard overlay, and records a config_governor_general_advanced audit entry with you as the actor. It is owner only.",
		"The governor loop reads these settings every cycle, so the change applies from the next eval cycle without a restart.",
	)
	if len(unchanged) > 0 {
		effects = append(effects, "Already as requested, so not sent: "+strings.Join(unchanged, ", ")+".")
	}
	narrowNote := ""
	if len(narrow) > 0 {
		narrowNote = " Narrowing: " + strings.Join(narrow, "; ") + "."
	}
	disclosure := "No widening: none of these changes makes the hive call GitHub more often or act where it could not before." + narrowNote
	if len(widen) > 0 {
		disclosure = "Widening: " + strings.Join(widen, "; ") + "." + narrowNote
	}
	details := map[string]any{"fields": fields, "changes": changeDetails(changes)}
	if len(unchanged) > 0 {
		details["unchanged"] = unchanged
	}
	return WritePreview{
		Operation:           WriteOpGovernorGeneralSettings,
		Summary:             "Change General governor settings: " + strings.Join(fields, ", "),
		Target:              "Settings → General governor settings",
		Request:             WriteRequest{Method: http.MethodPut, Path: governorGeneralSettingsPath, Body: body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing General governor settings: " + strings.Join(effects[:len(changes)], "; ") + ".",
		Details:             details,
		StateFingerprint:    fingerprint,
	}, nil
}

func governorGeneralModeLabel(mode string) string {
	if mode == "" {
		return "not set"
	}
	return mode
}
