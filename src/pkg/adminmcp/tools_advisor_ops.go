package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Write operations for the advisor lane's settings, the same three edits the
// dashboard's Advisor Lane panel makes (Settings → Features), through the
// endpoints that panel saves with:
//
//   - advisor.settings: the fleet block, PUT /api/config/advisor. Only the
//     named fields are sent; the endpoint leaves the others alone.
//   - advisor.agent: one agent's override or opt-out, PUT
//     /api/config/advisor/agent/{name}. The endpoint replaces the override
//     whole, so a field left out of it is cleared; "inherit" clears it all.
//   - advisor.roles: the named model_roles map, PUT /api/config/advisor/roles.
//     The endpoint replaces the map whole, so this op reads the current map
//     and sends it back with the named roles set or removed.
//
// Each preview reads the advisor settings (settings_read section advisor),
// shows what changes, and records a fingerprint of what it read. ConfirmGuard
// reads them again at write_confirm and refuses when they moved after the
// preview, so a change made on the dashboard in between is never overwritten.
// The endpoints validate the edit the way hive.yaml loading does, save it,
// record a config_advisor audit entry naming the caller, and are owner only;
// their refusal comes back verbatim. An agent picks a change up at its next
// launch. Each endpoint answers with the refreshed section, which
// write_confirm returns as the result.
//
// None of these change what the advisor says to an agent: it still speaks
// and never acts (advisor_records reads what it said).
const (
	WriteOpAdvisorSettings = "advisor.settings"
	WriteOpAdvisorAgent    = "advisor.agent"
	WriteOpAdvisorRoles    = "advisor.roles"

	advisorSettingsPath    = "/api/config/advisor"
	advisorRolesPath       = "/api/config/advisor/roles"
	advisorAgentPathPrefix = "/api/config/advisor/agent/"
	advisorSettingsSection = "advisor"

	advisorInheritKeyword = "inherit"
)

func advisorWriteOps() []WriteOp {
	return []WriteOp{advisorSettingsOp{}, advisorAgentOp{}, advisorRolesOp{}}
}

type advisorSettingsOp struct{}
type advisorAgentOp struct{}
type advisorRolesOp struct{}

type advisorFieldKind int

const (
	advisorBool advisorFieldKind = iota
	advisorString
	advisorCount   // a whole number, 0 or more
	advisorBackend // "" or one of advisorBackends
)

// advisorBackends are the advisor.backend values, the same as
// governor.reviewer_backend's; "" follows governor.reviewer_backend.
var advisorBackends = []string{"auto", "http", "claude-cli"}

// advisorFields are the fields of the fleet block and of an agent override,
// in the order the endpoints' bodies declare them (advisorFleetBody and
// config.AdvisorOverride). TestAdminMCPAdvisorFieldsMatchHandlerBody
// (pkg/dashboard) fails when they differ.
var advisorFields = []struct {
	key  string
	kind advisorFieldKind
}{
	{"enabled", advisorBool},
	{"model", advisorString},
	{"instructions", advisorString},
	{"daily_budget_tokens", advisorCount},
	{"timeout_s", advisorCount},
	{"max_consecutive_blocks", advisorCount},
	{"backend", advisorBackend},
}

// AdvisorBodyFields lists the fleet and override fields advisor.settings and
// advisor.agent accept. pkg/dashboard checks it against the handler's body.
func AdvisorBodyFields() []string {
	out := make([]string, 0, len(advisorFields))
	for _, f := range advisorFields {
		out = append(out, f.key)
	}
	return out
}

const advisorEffectNextLaunch = "Hive saves the change to hive.yaml and records a config_advisor audit entry naming you. Each affected agent picks it up at its next launch; an agent already running keeps its current advisor until then."

// parseAdvisorFields reads the advisor fields named in args into the body the
// endpoint takes. what names the object in error messages. A field outside
// advisorFields, or null, is refused: the endpoints read null as no change.
func parseAdvisorFields(args map[string]any, what string) (map[string]any, error) {
	known := map[string]advisorFieldKind{}
	for _, f := range advisorFields {
		known[f.key] = f.kind
	}
	body := map[string]any{}
	for key, raw := range args {
		kind, ok := known[key]
		if !ok {
			return nil, fmt.Errorf("%s: unknown field %q; accepted fields are %s", what, key, strings.Join(AdvisorBodyFields(), ", "))
		}
		if raw == nil {
			return nil, fmt.Errorf("%s: %s is null; give a value (an empty string clears model or instructions)", what, key)
		}
		switch kind {
		case advisorBool:
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s: %s must be true or false", what, key)
			}
			body[key] = b
		case advisorString:
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s: %s must be a string", what, key)
			}
			body[key] = strings.TrimSpace(s)
		case advisorBackend:
			s, ok := raw.(string)
			b := strings.ToLower(strings.TrimSpace(s))
			if !ok || (b != "" && !slices.Contains(advisorBackends, b)) {
				return nil, fmt.Errorf("%s: backend must be one of %s, or \"\" to follow governor.reviewer_backend", what, strings.Join(advisorBackends, ", "))
			}
			body[key] = b
		case advisorCount:
			n, _, err := optionalIntArg(map[string]any{key: raw}, key)
			if err != nil || n < 0 {
				return nil, fmt.Errorf("%s: %s must be a whole number of at least 0", what, key)
			}
			body[key] = n
		}
	}
	return body, nil
}

// advisorSection is what settings_read section advisor returned.
type advisorSection struct {
	fleet  map[string]any
	roles  map[string]any
	agents map[string]map[string]any
	// defaultBlocks is the built-in max_consecutive_blocks a 0 stands for.
	defaultBlocks int
}

func readAdvisorSection(ctx context.Context, provider Provider) (advisorSection, error) {
	if provider == nil {
		return advisorSection{}, fmt.Errorf("the advisor settings could not be read")
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": advisorSettingsSection})
	if err != nil {
		return advisorSection{}, fmt.Errorf("the advisor settings could not be read: %w", err)
	}
	m, _ := data.(map[string]any)
	if m == nil {
		return advisorSection{}, fmt.Errorf("the advisor settings could not be read: unexpected answer")
	}
	out := advisorSection{agents: map[string]map[string]any{}}
	out.fleet, _ = m["fleet"].(map[string]any)
	if out.fleet == nil {
		out.fleet = map[string]any{}
	}
	out.roles, _ = m["model_roles"].(map[string]any)
	if out.roles == nil {
		out.roles = map[string]any{}
	}
	defaults, _ := m["defaults"].(map[string]any)
	out.defaultBlocks = advisorCountValue(defaults, "max_consecutive_blocks")
	list, _ := m["agents"].([]any)
	for _, item := range list {
		row, _ := item.(map[string]any)
		if name, _ := row["name"].(string); name != "" {
			out.agents[name] = row
		}
	}
	return out, nil
}

// advisorFieldChanges describes each named field before and after, in field
// order.
func advisorFieldChanges(before, after map[string]any) []map[string]any {
	var out []map[string]any
	for _, f := range advisorFields {
		v, ok := after[f.key]
		if !ok {
			continue
		}
		out = append(out, map[string]any{"field": f.key, "before": before[f.key], "after": v})
	}
	return out
}

func advisorEnabled(m map[string]any) bool {
	b, _ := m["enabled"].(bool)
	return b
}

func advisorCountValue(m map[string]any, key string) int {
	n, _ := numberArg(m[key])
	return n
}

// advisorWidening names what the change lets the advisor do more of: review
// agents it did not review, spend more per day, or deliver more blockers in a
// row. from and to are the values in force before and after; a
// max_consecutive_blocks of 0 stands for defaultBlocks.
func advisorWidening(who string, from, to map[string]any, defaultBlocks int) []string {
	var out []string
	if !advisorEnabled(from) && advisorEnabled(to) {
		out = append(out, "the advisor starts reviewing "+who+": every finished turn waits for a review by the advisor model (up to timeout_s), which spends that model's tokens and can interject in the agent's session")
	}
	if advisorEnabled(to) {
		if b, a := advisorCountValue(from, "daily_budget_tokens"), advisorCountValue(to, "daily_budget_tokens"); b > 0 && (a == 0 || a > b) {
			out = append(out, fmt.Sprintf("the daily advisor token budget for %s goes from %d to %s", who, b, advisorBudgetLabel(a)))
		}
		if b, a := advisorBlocks(from, defaultBlocks), advisorBlocks(to, defaultBlocks); a > b && b > 0 {
			out = append(out, fmt.Sprintf("the advisor may hold %s with up to %d blockers in a row instead of %d", who, a, b))
		}
		if b, _ := to["backend"].(string); b == "claude-cli" {
			if was, _ := from["backend"].(string); was != "claude-cli" {
				out = append(out, "reviews of "+who+" run on this hive's Claude sign-in, which its agents also draw on")
			}
		}
	}
	return out
}

func advisorBlocks(m map[string]any, defaultBlocks int) int {
	if n := advisorCountValue(m, "max_consecutive_blocks"); n > 0 {
		return n
	}
	return defaultBlocks
}

func advisorBudgetLabel(n int) string {
	if n == 0 {
		return "no cap (0)"
	}
	return fmt.Sprint(n)
}

func advisorDisclosure(widening []string) string {
	if len(widening) == 0 {
		return "No widening: the advisor reviews no agent it did not review before, its daily budget is not raised and its blocker limit is not raised."
	}
	return "Widening: " + strings.Join(widening, "; ") + "."
}

func (advisorSettingsOp) Name() string { return WriteOpAdvisorSettings }
func (advisorSettingsOp) Description() string {
	return "Change the fleet advisor-lane settings (the advisor block, Settings → Features → Advisor Lane) through PUT " + advisorSettingsPath + ", the endpoint that panel saves with. Owner only. Pass only what changes: enabled, model (a model id, \"@<role>\" naming a model_roles entry, or \"\" for the trajectory reviewer's model), instructions, daily_budget_tokens (per agent per day, 0 = no cap), timeout_s and max_consecutive_blocks (0 = the built-in default), and backend (http, claude-cli for this hive's Claude sign-in, auto, or \"\" to follow governor.reviewer_backend). Only those fields are sent; agents with their own override keep it. The preview reads the current settings and shows each named field before and after; write_confirm refuses if the advisor settings changed after the preview. Agents pick the change up at their next launch."
}
func (advisorSettingsOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": advisorFieldsSchema(), "additionalProperties": false}
}
func (advisorSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, err := parseAdvisorSettingsArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpAdvisorSettings, "advisor", advisorSettingsPath, "advisor settings"), nil
}
func (op advisorSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

func parseAdvisorSettingsArgs(args map[string]any) (map[string]any, error) {
	body, err := parseAdvisorFields(args, WriteOpAdvisorSettings)
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("%s: name at least one field to change; accepted fields are %s", WriteOpAdvisorSettings, strings.Join(AdvisorBodyFields(), ", "))
	}
	return body, nil
}

// PreviewWithProvider reads the fleet block and shows each named field before
// and after.
func (advisorSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	body, err := parseAdvisorSettingsArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	cur, err := readAdvisorSection(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fingerprint, err := stateFingerprint(cur.fleet)
	if err != nil {
		return WritePreview{}, err
	}
	after := map[string]any{}
	for k, v := range cur.fleet {
		after[k] = v
	}
	for k, v := range body {
		after[k] = v
	}
	var overridden []string
	for name, row := range cur.agents {
		if o, _ := row["override"].(map[string]any); len(o) > 0 {
			overridden = append(overridden, name)
		}
	}
	sort.Strings(overridden)
	effects := []string{advisorEffectNextLaunch}
	if len(overridden) > 0 {
		effects = append(effects, "Agents with their own advisor override keep the fields it sets: "+strings.Join(overridden, ", ")+".")
	}
	return WritePreview{
		Operation:           WriteOpAdvisorSettings,
		Summary:             "Change the fleet advisor settings: " + advisorChangedFields(body),
		Target:              "advisor",
		Request:             WriteRequest{Method: http.MethodPut, Path: advisorSettingsPath, Body: body},
		Effects:             effects,
		WideningDisclosure:  advisorDisclosure(advisorWidening("every agent without an override", cur.fleet, after, cur.defaultBlocks)),
		ConfirmationMessage: "Confirm changing the fleet advisor settings (" + advisorChangedFields(body) + ").",
		Details:             map[string]any{"changes": advisorFieldChanges(cur.fleet, body)},
		StateFingerprint:    fingerprint,
	}, nil
}

func advisorChangedFields(body map[string]any) string {
	var names []string
	for _, f := range advisorFields {
		if _, ok := body[f.key]; ok {
			names = append(names, f.key)
		}
	}
	return strings.Join(names, ", ")
}

func advisorFieldsSchema() map[string]any {
	return map[string]any{
		"enabled":                map[string]any{"type": "boolean", "description": "Turn the advisor on or off."},
		"model":                  map[string]any{"type": "string", "description": "Advisor model id, \"@<role>\" naming a model_roles entry, or \"\" for the trajectory reviewer's model."},
		"instructions":           map[string]any{"type": "string", "description": "Standing instructions folded into the review prompt; \"\" clears them."},
		"daily_budget_tokens":    map[string]any{"type": "integer", "minimum": 0, "description": "Advisor token budget per agent per day; 0 = no cap."},
		"timeout_s":              map[string]any{"type": "integer", "minimum": 0, "description": "Seconds one review may hold a turn before the agent goes on; 0 = the built-in default."},
		"max_consecutive_blocks": map[string]any{"type": "integer", "minimum": 0, "description": "Blockers in a row before the advisor's blockers become asides; must stay below the Copilot CLI bound (8); 0 = the built-in default."},
		"backend":                map[string]any{"type": "string", "enum": []string{"", "auto", "http", "claude-cli"}, "description": "How a review reaches the model: http (the reviewer endpoint and its API key), claude-cli (this hive's Claude sign-in, every tool off) or auto (the endpoint when one is set, else the Claude sign-in); \"\" follows governor.reviewer_backend."},
	}
}

func (advisorAgentOp) Name() string { return WriteOpAdvisorAgent }
func (advisorAgentOp) Description() string {
	return "Set or clear one agent's advisor override (agents.<name>.advisor, the per-agent rows of Settings → Features → Advisor Lane) through PUT " + advisorAgentPathPrefix + "{name}. Owner only. override is an object with any of enabled, model, instructions, daily_budget_tokens, timeout_s, max_consecutive_blocks and backend, and replaces the agent's override whole: a field left out of it is cleared and follows the fleet setting again. {\"enabled\": false} opts the agent out. override \"inherit\" (or {}) clears the override, so the agent follows the fleet settings. The preview shows the override and the settings in force before and after; write_confirm refuses if the advisor settings changed after the preview. The agent picks the change up at its next launch."
}
func (advisorAgentOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"agent": map[string]any{"type": "string", "minLength": 1},
		"override": map[string]any{
			"anyOf": []any{
				map[string]any{"type": "object", "properties": advisorFieldsSchema(), "additionalProperties": false},
				map[string]any{"type": "string", "enum": []string{advisorInheritKeyword}},
			},
			"description": "The agent's whole override, or \"inherit\" to clear it.",
		},
	}, "required": []string{"agent", "override"}, "additionalProperties": false}
}
func (advisorAgentOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, _, err := parseAdvisorAgentArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpAdvisorAgent, agent, advisorAgentPathPrefix+url.PathEscape(agent), "advisor settings"), nil
}
func (op advisorAgentOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// parseAdvisorAgentArgs returns the agent and its new override; a nil
// override clears it.
func parseAdvisorAgentArgs(args map[string]any) (string, map[string]any, error) {
	for key := range args {
		if key != "agent" && key != "override" {
			return "", nil, fmt.Errorf("%s: unknown argument %q; it takes agent and override", WriteOpAdvisorAgent, key)
		}
	}
	agent, err := requiredString(args, "agent")
	if err != nil {
		return "", nil, err
	}
	switch o := args["override"].(type) {
	case string:
		if strings.EqualFold(strings.TrimSpace(o), advisorInheritKeyword) {
			return agent, nil, nil
		}
	case map[string]any:
		body, err := parseAdvisorFields(o, WriteOpAdvisorAgent+" override")
		if err != nil {
			return "", nil, err
		}
		if len(body) == 0 {
			return agent, nil, nil
		}
		return agent, body, nil
	}
	return "", nil, fmt.Errorf("%s: override must be an object of advisor fields (%s) or %q", WriteOpAdvisorAgent, strings.Join(AdvisorBodyFields(), ", "), advisorInheritKeyword)
}

// PreviewWithProvider reads the agent's current override and the fleet block,
// and shows the override and the settings in force before and after.
func (advisorAgentOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	agent, override, err := parseAdvisorAgentArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	cur, err := readAdvisorSection(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	row, ok := cur.agents[agent]
	if !ok {
		return WritePreview{}, fmt.Errorf("%s: agent %q is not configured on this hive", WriteOpAdvisorAgent, agent)
	}
	prior, _ := row["override"].(map[string]any)
	effective, _ := row["effective"].(map[string]any)
	fingerprint, err := stateFingerprint(map[string]any{"fleet": cur.fleet, "override": prior})
	if err != nil {
		return WritePreview{}, err
	}
	// In force after: the fleet block with the new override's fields on top.
	after := map[string]any{}
	for k, v := range cur.fleet {
		after[k] = v
	}
	for k, v := range override {
		after[k] = v
	}
	var cleared []string
	for _, f := range advisorFields {
		if _, was := prior[f.key]; was {
			if _, still := override[f.key]; !still {
				cleared = append(cleared, f.key)
			}
		}
	}
	summary := "Clear agent " + agent + "'s advisor override so it follows the fleet settings"
	label := "inherit the fleet settings"
	if override != nil {
		summary = "Set agent " + agent + "'s advisor override: " + advisorChangedFields(override)
		label = "override " + advisorChangedFields(override)
		if b, ok := override["enabled"].(bool); ok && !b && len(override) == 1 {
			summary = "Opt agent " + agent + " out of the advisor"
			label = "opt out"
		}
	}
	effects := []string{advisorEffectNextLaunch}
	if len(cleared) > 0 {
		effects = append(effects, "The override's "+strings.Join(cleared, ", ")+" is cleared, so the agent follows the fleet value for it again.")
	}
	if backend, _ := row["backend"].(string); backend != "" && advisorEnabled(after) && !config.AdvisorSupportedBackend(backend) {
		effects = append(effects, "The advisor is not active on this agent's backend ("+backend+"; supported: "+config.AdvisorSupportedBackendList+"), so the setting is saved but produces no reviews.")
	}
	var body any
	if override != nil {
		body = override
	}
	return WritePreview{
		Operation:           WriteOpAdvisorAgent,
		Summary:             summary,
		Target:              agent,
		Request:             WriteRequest{Method: http.MethodPut, Path: advisorAgentPathPrefix + url.PathEscape(agent), Body: map[string]any{"override": body}},
		Effects:             effects,
		WideningDisclosure:  advisorDisclosure(advisorWidening("agent "+agent, effective, after, cur.defaultBlocks)),
		ConfirmationMessage: "Confirm agent " + agent + "'s advisor setting: " + label + ".",
		Details:             map[string]any{"agent": agent, "override_before": prior, "override_after": body, "cleared": cleared},
		StateFingerprint:    fingerprint,
	}, nil
}

func (advisorRolesOp) Name() string { return WriteOpAdvisorRoles }
func (advisorRolesOp) Description() string {
	return "Add, change or remove named model roles (model_roles, the roles list of Settings → Features → Advisor Lane) through PUT " + advisorRolesPath + ". Owner only. set maps a role name to {model, backend, reasoning_effort} (model is required) and replaces that role whole; remove lists role names to delete. Roles not named keep their value: the op reads the current map and sends it back with only these changes. A role the advisor block or an agent override still names (\"@<role>\") cannot be removed; the endpoint refuses. The preview shows each named role before and after; write_confirm refuses if the advisor settings changed after the preview."
}
func (advisorRolesOp) InputSchema() map[string]any {
	role := map[string]any{"type": "object", "properties": map[string]any{
		"model":            map[string]any{"type": "string", "minLength": 1},
		"backend":          map[string]any{"type": "string", "description": "Backend the role is pinned to; empty for any."},
		"reasoning_effort": map[string]any{"type": "string", "description": "Effort for the role, checked against the backend's levels."},
	}, "required": []string{"model"}, "additionalProperties": false}
	return map[string]any{"type": "object", "properties": map[string]any{
		"set":    map[string]any{"type": "object", "additionalProperties": role, "description": "Role name → role. Names take no spaces or @."},
		"remove": map[string]any{"type": "array", "items": map[string]any{"type": "string", "minLength": 1}},
	}, "additionalProperties": false}
}
func (advisorRolesOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	if _, _, err := parseAdvisorRolesArgs(args); err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpAdvisorRoles, "model_roles", advisorRolesPath, "model roles"), nil
}
func (op advisorRolesOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

func parseAdvisorRolesArgs(args map[string]any) (map[string]map[string]any, []string, error) {
	for key := range args {
		if key != "set" && key != "remove" {
			return nil, nil, fmt.Errorf("%s: unknown argument %q; it takes set and remove", WriteOpAdvisorRoles, key)
		}
	}
	set := map[string]map[string]any{}
	if raw, ok := args["set"]; ok {
		m, ok := raw.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s: set must be an object of role name → {model, backend, reasoning_effort}", WriteOpAdvisorRoles)
		}
		for rawName, rawRole := range m {
			name := strings.TrimSpace(rawName)
			if name == "" || strings.ContainsAny(name, " \t@") {
				return nil, nil, fmt.Errorf("%s: invalid role name %q (no spaces or @)", WriteOpAdvisorRoles, rawName)
			}
			r, ok := rawRole.(map[string]any)
			if !ok {
				return nil, nil, fmt.Errorf("%s: role %s must be an object with model, backend and reasoning_effort", WriteOpAdvisorRoles, name)
			}
			role := map[string]any{}
			for key, v := range r {
				if key != "model" && key != "backend" && key != "reasoning_effort" {
					return nil, nil, fmt.Errorf("%s: role %s: unknown field %q; a role takes model, backend and reasoning_effort", WriteOpAdvisorRoles, name, key)
				}
				s, ok := v.(string)
				if !ok {
					return nil, nil, fmt.Errorf("%s: role %s: %s must be a string", WriteOpAdvisorRoles, name, key)
				}
				if s = strings.TrimSpace(s); s != "" {
					role[key] = s
				}
			}
			if role["model"] == nil {
				return nil, nil, fmt.Errorf("%s: role %s: model is required", WriteOpAdvisorRoles, name)
			}
			set[name] = role
		}
	}
	var remove []string
	if raw, ok := args["remove"]; ok {
		list, ok := raw.([]any)
		if !ok {
			return nil, nil, fmt.Errorf("%s: remove must be a list of role names", WriteOpAdvisorRoles)
		}
		for _, item := range list {
			name, _ := item.(string)
			if name = strings.TrimSpace(name); name == "" {
				return nil, nil, fmt.Errorf("%s: remove must list role names", WriteOpAdvisorRoles)
			}
			if _, both := set[name]; both {
				return nil, nil, fmt.Errorf("%s: role %s is both set and removed", WriteOpAdvisorRoles, name)
			}
			remove = append(remove, name)
		}
	}
	if len(set) == 0 && len(remove) == 0 {
		return nil, nil, fmt.Errorf("%s: name at least one role to set or remove", WriteOpAdvisorRoles)
	}
	sort.Strings(remove)
	return set, remove, nil
}

// PreviewWithProvider reads the current roles map, applies the named changes
// and sends the whole map back, as the endpoint replaces it.
func (advisorRolesOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	set, remove, err := parseAdvisorRolesArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	cur, err := readAdvisorSection(ctx, provider)
	if err != nil {
		return WritePreview{}, err
	}
	fingerprint, err := stateFingerprint(cur.roles)
	if err != nil {
		return WritePreview{}, err
	}
	next := map[string]any{}
	for name, role := range cur.roles {
		next[name] = role
	}
	for _, name := range remove {
		if _, ok := next[name]; !ok {
			return WritePreview{}, fmt.Errorf("%s: role %s does not exist on this hive", WriteOpAdvisorRoles, name)
		}
		delete(next, name)
	}
	changed := make([]string, 0, len(set)+len(remove))
	var changes []map[string]any
	for name, role := range set {
		next[name] = role
		changed = append(changed, name)
	}
	changed = append(changed, remove...)
	sort.Strings(changed)
	for _, name := range changed {
		changes = append(changes, map[string]any{"role": name, "before": cur.roles[name], "after": next[name]})
	}
	// A changed or removed role the advisor names now resolves differently.
	inUse := advisorRoleUsers(cur, changed)
	disclosure := "No widening: no role the advisor uses changes."
	effects := []string{advisorEffectNextLaunch}
	if len(inUse) > 0 {
		disclosure = "Potential widening: the advisor model changes for " + strings.Join(inUse, ", ") + ", which may cost more per review."
		effects = append(effects, "The advisor resolves these roles for: "+strings.Join(inUse, ", ")+".")
	}
	return WritePreview{
		Operation:           WriteOpAdvisorRoles,
		Summary:             "Change model roles: " + strings.Join(changed, ", "),
		Target:              "model_roles",
		Request:             WriteRequest{Method: http.MethodPut, Path: advisorRolesPath, Body: map[string]any{"model_roles": next}},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing model roles " + strings.Join(changed, ", ") + ".",
		Details:             map[string]any{"changes": changes},
		StateFingerprint:    fingerprint,
	}, nil
}

// advisorRoleUsers names who reaches one of roles through "@<role>": the
// fleet block ("the fleet setting") and each agent whose settings in force
// name it.
func advisorRoleUsers(cur advisorSection, roles []string) []string {
	named := map[string]bool{}
	for _, r := range roles {
		named["@"+r] = true
	}
	var out []string
	if m, _ := cur.fleet["model"].(string); named[strings.TrimSpace(m)] {
		out = append(out, "the fleet setting")
	}
	var agents []string
	for name, row := range cur.agents {
		eff, _ := row["effective"].(map[string]any)
		if m, _ := eff["model"].(string); named[strings.TrimSpace(m)] {
			agents = append(agents, "agent "+name)
		}
	}
	sort.Strings(agents)
	return append(out, agents...)
}
