package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"

	"github.com/hivecommons/hive/crux/agentmode"
	"github.com/hivecommons/hive/pkg/config"
)

// agent.settings changes one agent's General settings, the agent config
// dialog's General tab, through the endpoint that tab saves with: PUT
// /api/config/agent/{name}/general. The endpoint reads only the keys it is
// sent, so the op sends only the fields the caller names and never resends a
// field it was not asked to change.
//
// The preview reads the agent's settings (settings_read section agent, GET
// /api/config/agent/{name}), shows each named field before and after, and
// names every change that lets the agent do more or run more as widening.
// It records a fingerprint of the named fields' current values; ConfirmGuard
// reads them again at write_confirm and refuses when one moved, so a change
// made on the dashboard in between is never overwritten.
//
// The endpoint is owner only, validates the body (validateAgentGeneralInput
// and its own checks), saves it, and records a config_agent_general audit
// entry naming the caller; its refusal comes back verbatim. It answers only
// {"status":"updated"}, so ConfirmResult reads the named fields back.
//
// Some fields the endpoint takes are not offered (agentSettingsRefused): they
// change what command the hive runs, how the agent is isolated, or which lane
// it is, and stay on the dashboard; model and the GitHub API cap have their
// own operations.
const (
	WriteOpAgentSettings = "agent.settings"

	agentSettingsSection = "agent"
)

func agentSettingsWriteOps() []WriteOp {
	return []WriteOp{agentSettingsOp{}}
}

type agentSettingsOp struct{}

type agentSettingKind int

const (
	agentSettingBool agentSettingKind = iota
	agentSettingString
	agentSettingCount   // a whole number, 0 or more
	agentSettingSeconds // a whole number of seconds, more than 0
	agentSettingList    // a list of strings
	agentSettingEnum    // a string from enum
)

type agentSettingField struct {
	key  string
	kind agentSettingKind
	enum []string
	desc string
}

// agentSettingsRestartStrategies and agentSettingsBeadRoles mirror the
// endpoint's validRestartStrategies and validBeadRoles (pkg/dashboard
// validation.go), which this package cannot import;
// TestAdminMCPAgentSettingsEnumsMatchEndpoint (pkg/dashboard) fails when they
// differ.
var (
	agentSettingsRestartStrategies = []string{"", "immediate"}
	agentSettingsBeadRoles         = []string{"", "worker", "supervisor"}
)

// agentSettingsModes are the values the endpoint takes for mode: every rung
// of the agentmode ladder, NO_GITHUB (a legacy alias for ADVISORY), and ""
// for the ACMM level's default.
func agentSettingsModes() []string {
	out := []string{""}
	for m := agentmode.ModeAdvisory; m <= agentmode.ModeIssuesPRsMerge; m++ {
		out = append(out, m.String())
	}
	return append(out, "NO_GITHUB")
}

// agentSettingsFields are the fields agent.settings offers, with the JSON
// keys handleAgentConfigGeneral (pkg/dashboard) reads.
// TestAdminMCPAgentSettingsFieldsMatchHandler (pkg/dashboard) fails when one
// is not a key the handler reads, or when the handler reads a key that is
// neither offered nor refused here.
var agentSettingsFields = []agentSettingField{
	{"enabled", agentSettingBool, nil, "Turn the agent on or off. Off stops it and the governor no longer kicks it."},
	{"mode", agentSettingEnum, agentSettingsModes(), "GitHub interaction tier: ADVISORY < ISSUES_ONLY < ISSUES_AND_PRS < ISSUES_PRS_MERGE (NO_GITHUB is an alias for ADVISORY); \"\" uses the ACMM level's default."},
	{"onDemand", agentSettingBool, nil, "On demand: the governor never kicks the agent on a cadence; it runs only when started."},
	{"repos", agentSettingList, nil, "The agent's repository scope (owner/repo names); [] clears it, so the agent works hive-wide."},
	{"includeRepos", agentSettingBool, nil, "Whether each kick names the hive's repositories."},
	{"replicas", agentSettingCount, nil, fmt.Sprintf("How many copies of the agent run (1 to %d).", config.MaxAgentReplicas)},
	{"staleTimeout", agentSettingCount, nil, "Seconds without output before the agent is flagged stale; 0 disables."},
	{"restartStrategy", agentSettingEnum, agentSettingsRestartStrategies, "How a stale agent is restarted: \"immediate\", or \"\" for the default."},
	{"clearOnKick", agentSettingBool, nil, "Clear the agent's session before each kick."},
	{"continuous", agentSettingBool, nil, "Continuous mode: kick the agent again as soon as a turn ends."},
	{"continuousBudgetPct", agentSettingCount, nil, "Continuous mode stops re-kicking once this percent of the budget is used (1 to 100)."},
	{"continuousCooldown", agentSettingSeconds, nil, "Seconds continuous mode waits between turns (more than 0)."},
	{"contributorFirst", agentSettingBool, nil, "Contributor-first lane: the agent waits a grace period so contributors can take an issue first."},
	{"contributorFirstGrace", agentSettingCount, nil, "Contributor-first grace in seconds; 0 uses the default."},
	{"kickTemplate", agentSettingString, nil, "Prompt template file the agent is kicked with (a .md name); \"\" uses the default."},
	{"beadRole", agentSettingEnum, agentSettingsBeadRoles, "Bead role: worker or supervisor, or \"\" for none."},
	{"displayName", agentSettingString, nil, "Name shown on the dashboard."},
	{"description", agentSettingString, nil, "Description shown on the dashboard."},
	{"emoji", agentSettingString, nil, "Emoji shown on the dashboard."},
	{"color", agentSettingString, nil, "Hex color shown on the dashboard, such as #FF5733."},
	{"sortOrder", agentSettingCount, nil, "Position on the dashboard."},
	{"aliases", agentSettingList, nil, "Other names the agent answers to."},
	{"laneKeywords", agentSettingList, nil, "Keywords that route work to this agent's lane."},
	{"detectKeywords", agentSettingList, nil, "Keywords that detect this agent's kind of work."},
	{"bobSessionLabel", agentSettingString, nil, "Label of the agent's Bob reporting session."},
}

// agentSettingsRefused are fields the endpoint takes that agent.settings
// does not offer, each with where to change it instead.
var agentSettingsRefused = []struct{ key, reason string }{
	{"launchCmd", "it is the command the hive runs for this agent"},
	{"cliPinned", "it pins which CLI binary runs this agent"},
	{"cliPinValue", "it picks which CLI binary runs this agent"},
	{"sandboxEnabled", "it turns the agent's sandbox isolation on or off"},
	{"sandboxSubstrate", "it sets how the agent's sandbox isolates it"},
	{"engine", "it picks the experiment engine the agent runs"},
	{"role", "it is the agent's lane identity"},
	{"promptSource", "it points the agent's prompt at a repository"},
	{"definitionSource", "it links the agent's whole definition to a repository"},
	{"model", "use the agent.model operation instead"},
	{"githubApiHourlyCap", "use the agent.github_api_hourly_cap operation instead"},
}

// AgentSettingsFields lists the fields agent.settings accepts, in order.
// pkg/dashboard checks them against the handler.
func AgentSettingsFields() []string {
	out := make([]string, 0, len(agentSettingsFields))
	for _, f := range agentSettingsFields {
		out = append(out, f.key)
	}
	return out
}

// AgentSettingsRefusedFields lists the endpoint's fields agent.settings
// refuses.
func AgentSettingsRefusedFields() []string {
	out := make([]string, 0, len(agentSettingsRefused))
	for _, r := range agentSettingsRefused {
		out = append(out, r.key)
	}
	return out
}

// AgentSettingsEnums returns the values agent.settings takes for its
// restartStrategy and beadRole fields, which mirror the endpoint's lists.
func AgentSettingsEnums() map[string][]string {
	return map[string][]string{
		"restartStrategy": slices.Clone(agentSettingsRestartStrategies),
		"beadRole":        slices.Clone(agentSettingsBeadRoles),
	}
}

func agentSettingsRefusal(key string) (string, bool) {
	for _, r := range agentSettingsRefused {
		if r.key != key {
			continue
		}
		if strings.HasPrefix(r.reason, "use ") {
			return fmt.Sprintf("%s does not change %s: %s", WriteOpAgentSettings, key, r.reason), true
		}
		return fmt.Sprintf("%s does not change %s, because %s; change it on the dashboard (the agent's settings, General tab)", WriteOpAgentSettings, key, r.reason), true
	}
	return "", false
}

func agentGeneralPath(agent string) string {
	return "/api/config/agent/" + url.PathEscape(agent) + "/general"
}

// parseAgentSettingsArgs returns the agent and the body to send: only the
// named fields, typed as the endpoint reads them (it ignores a value of the
// wrong type, so a wrong type is refused here rather than sent).
func parseAgentSettingsArgs(args map[string]any) (string, map[string]any, error) {
	known := map[string]agentSettingField{}
	for _, f := range agentSettingsFields {
		known[f.key] = f
	}
	// Refused and unknown fields first, in a stable order, so the error does
	// not depend on map order.
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "agent" {
			continue
		}
		if msg, refused := agentSettingsRefusal(key); refused {
			return "", nil, fmt.Errorf("%s", msg)
		}
		if _, ok := known[key]; !ok {
			return "", nil, fmt.Errorf("%s: unknown field %q; accepted fields are %s", WriteOpAgentSettings, key, strings.Join(AgentSettingsFields(), ", "))
		}
	}
	agent, err := requiredString(args, "agent")
	if err != nil {
		return "", nil, err
	}
	body := map[string]any{}
	for _, key := range keys {
		if key == "agent" {
			continue
		}
		f := known[key]
		raw := args[key]
		if raw == nil {
			return "", nil, fmt.Errorf("%s: %s is null; give a value", WriteOpAgentSettings, key)
		}
		v, err := agentSettingValue(f, raw)
		if err != nil {
			return "", nil, err
		}
		body[key] = v
	}
	if len(body) == 0 {
		return "", nil, fmt.Errorf("%s: name at least one field to change; accepted fields are %s", WriteOpAgentSettings, strings.Join(AgentSettingsFields(), ", "))
	}
	return agent, body, nil
}

func agentSettingValue(f agentSettingField, raw any) (any, error) {
	where := WriteOpAgentSettings + ": " + f.key
	switch f.kind {
	case agentSettingBool:
		b, ok := raw.(bool)
		if !ok {
			return nil, fmt.Errorf("%s must be true or false", where)
		}
		return b, nil
	case agentSettingString:
		s, ok := raw.(string)
		if !ok {
			return nil, fmt.Errorf("%s must be a string", where)
		}
		return strings.TrimSpace(s), nil
	case agentSettingEnum:
		s, ok := raw.(string)
		s = strings.TrimSpace(s)
		if !ok || !slices.Contains(f.enum, s) {
			return nil, fmt.Errorf("%s must be one of %s", where, enumLabel(f.enum))
		}
		return s, nil
	case agentSettingCount, agentSettingSeconds:
		n, ok := numberArg(raw)
		if !ok {
			return nil, fmt.Errorf("%s must be a whole number of at least 0", where)
		}
		if f.kind == agentSettingSeconds && n == 0 {
			return nil, fmt.Errorf("%s must be a whole number of seconds greater than 0", where)
		}
		if f.key == "replicas" && (n < 1 || n > config.MaxAgentReplicas) {
			return nil, fmt.Errorf("%s must be between 1 and %d", where, config.MaxAgentReplicas)
		}
		return n, nil
	case agentSettingList:
		items, ok := raw.([]any)
		if !ok {
			if ss, isStrings := raw.([]string); isStrings {
				for _, s := range ss {
					items = append(items, s)
				}
				ok = true
			}
		}
		if !ok {
			return nil, fmt.Errorf("%s must be a list of strings", where)
		}
		out := make([]string, 0, len(items))
		for _, item := range items {
			s, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be a list of strings", where)
			}
			s = strings.TrimSpace(s)
			if s == "" {
				return nil, fmt.Errorf("%s has a blank entry; give [] to clear the list", where)
			}
			out = append(out, s)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s: unsupported field kind", where)
}

func enumLabel(values []string) string {
	out := make([]string, len(values))
	for i, v := range values {
		out[i] = fmt.Sprintf("%q", v)
	}
	return strings.Join(out, ", ")
}

// agentGeneralState is the agent's General settings as the preview read them.
type agentGeneralState struct {
	general map[string]any
	// replicaOf names the base agent when this agent is a replica or a review
	// head, which the hive rebuilds from the base's settings.
	replicaOf string
}

// readAgentGeneral reads settings_read section agent, the view the agent
// config dialog reads (GET /api/config/agent/{name}), and returns its
// general block. An agent that does not exist is refused by that read.
func readAgentGeneral(ctx context.Context, provider Provider, agent string) (agentGeneralState, error) {
	if provider == nil {
		return agentGeneralState{}, fmt.Errorf("%s reads agent %s's current settings first, and this connection cannot read them; nothing was previewed", WriteOpAgentSettings, agent)
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": agentSettingsSection, "agent": agent})
	if err != nil {
		return agentGeneralState{}, fmt.Errorf("%s could not read agent %s's settings (settings_read section agent): %w; nothing was previewed", WriteOpAgentSettings, agent, err)
	}
	m, _ := data.(map[string]any)
	general, _ := m["general"].(map[string]any)
	if general == nil {
		return agentGeneralState{}, missingSetting("agent "+agent+"'s general settings", "general")
	}
	st := agentGeneralState{general: general}
	st.replicaOf, _ = general["githubApiHourlyCapBase"].(string)
	return st, nil
}

// agentSettingsFingerprint covers the current value of every named field and
// whether the agent is a copy of another.
func agentSettingsFingerprint(st agentGeneralState, body map[string]any) (string, error) {
	cur := map[string]any{"_replicaOf": st.replicaOf}
	for k := range body {
		cur[k] = st.general[k]
	}
	return stateFingerprint(cur)
}

func generalBool(m map[string]any, key string) bool {
	b, _ := m[key].(bool)
	return b
}

func generalInt(m map[string]any, key string) int {
	n, _ := numberArg(m[key])
	return n
}

// generalList reads a list of strings the settings answered; null reads as
// empty.
func generalList(m map[string]any, key string) []string {
	list, _ := stringsField(m, key)
	return list
}

// agentModeRank is the mode's rung on the agentmode ladder
// (crux/agentmode: ADVISORY < ISSUES_ONLY < ISSUES_AND_PRS <
// ISSUES_PRS_MERGE; NO_GITHUB is ADVISORY). It reports false for "", the
// ACMM level's default, whose rung this op does not read.
func agentModeRank(mode string) (agentmode.AgentMode, bool) {
	if mode == "" {
		return 0, false
	}
	return agentmode.ParseAgentMode(mode)
}

func agentModeLabel(mode string) string {
	if mode == "" {
		return "the ACMM level's default"
	}
	return mode
}

// agentSettingsWidening names each change that lets the agent do more on
// GitHub, work in more places, or run more.
func agentSettingsWidening(agent string, cur, body map[string]any) []string {
	var out []string
	after := func(key string) (any, bool) {
		v, ok := body[key]
		return v, ok
	}
	if v, ok := after("enabled"); ok && v.(bool) && !generalBool(cur, "enabled") {
		out = append(out, agent+" is turned on (enabled false → true): it starts and the governor kicks it again")
	}
	if v, ok := after("mode"); ok {
		from, _ := cur["mode"].(string)
		to := v.(string)
		fromRank, fromKnown := agentModeRank(from)
		toRank, toKnown := agentModeRank(to)
		switch {
		case from == to:
		case fromKnown && toKnown && toRank > fromRank:
			out = append(out, fmt.Sprintf("%s's GitHub tier goes up from %s to %s, so it may %s", agent, from, to, agentModeCan(toRank)))
		case !fromKnown && toKnown && toRank > agentmode.ModeAdvisory:
			out = append(out, fmt.Sprintf("%s's GitHub tier is set to %s, which may be above the ACMM level's default it has now, so it may %s", agent, to, agentModeCan(toRank)))
		case fromKnown && !toKnown && fromRank < agentmode.ModeIssuesPRsMerge:
			out = append(out, fmt.Sprintf("%s's GitHub tier goes from %s to the ACMM level's default, which may be higher", agent, from))
		}
	}
	if v, ok := after("onDemand"); ok && !v.(bool) && generalBool(cur, "onDemand") {
		out = append(out, agent+" leaves on-demand (onDemand true → false): the governor kicks it on its cadence")
	}
	if v, ok := after("repos"); ok {
		from := generalList(cur, "repos")
		to := v.([]string)
		var added []string
		for _, r := range to {
			if !slices.Contains(from, r) {
				added = append(added, r)
			}
		}
		switch {
		case len(from) > 0 && len(to) == 0:
			out = append(out, agent+"'s repository scope is cleared, so it works on every repository of the hive (was "+listLabel(from)+")")
		case len(from) > 0 && len(added) > 0:
			out = append(out, agent+"'s repository scope gains "+listLabel(added))
		}
	}
	if v, ok := after("includeRepos"); ok && v.(bool) && !generalBool(cur, "includeRepos") {
		out = append(out, agent+"'s kicks name the hive's repositories again (includeRepos false → true)")
	}
	if v, ok := after("replicas"); ok {
		from := generalInt(cur, "replicas")
		if from < 1 {
			from = 1
		}
		if to := v.(int); to > from {
			out = append(out, fmt.Sprintf("%s runs as %d copies instead of %d: more spend", agent, to, from))
		}
	}
	continuousAfter := generalBool(cur, "continuous")
	if v, ok := after("continuous"); ok {
		continuousAfter = v.(bool)
		if continuousAfter && !generalBool(cur, "continuous") {
			out = append(out, agent+" runs continuously (continuous false → true): it is kicked again as soon as a turn ends, which spends more")
		}
	}
	if continuousAfter {
		if v, ok := after("continuousBudgetPct"); ok {
			if from, to := generalInt(cur, "continuousBudgetPct"), v.(int); to > from {
				out = append(out, fmt.Sprintf("continuous mode keeps re-kicking %s until %d%% of the budget is used instead of %d%%", agent, to, from))
			}
		}
		if v, ok := after("continuousCooldown"); ok {
			if from, to := generalInt(cur, "continuousCooldown"), v.(int); to < from {
				out = append(out, fmt.Sprintf("continuous mode waits %ds between %s's turns instead of %ds", to, agent, from))
			}
		}
	}
	if v, ok := after("contributorFirst"); ok && v.(bool) && !generalBool(cur, "contributorFirst") {
		out = append(out, agent+" joins the contributor-first lane (contributorFirst false → true)")
	}
	return out
}

func agentModeCan(m agentmode.AgentMode) string {
	switch {
	case m.CanMerge():
		return "open issues and pull requests and merge on green CI"
	case m.CanCreatePRs():
		return "open issues and pull requests"
	case m.CanCreateIssues():
		return "open issues"
	}
	return "only advise"
}

func agentSettingsChanged(body map[string]any) string {
	var names []string
	for _, f := range agentSettingsFields {
		if _, ok := body[f.key]; ok {
			names = append(names, f.key)
		}
	}
	return strings.Join(names, ", ")
}

func (agentSettingsOp) Name() string { return WriteOpAgentSettings }
func (agentSettingsOp) Description() string {
	return "Change one agent's General settings (the agent config dialog's General tab) through PUT /api/config/agent/{name}/general, the endpoint that tab saves with. Owner only. Pass agent and only the fields that change, for example {\"agent\": \"redteam\", \"enabled\": false} to turn an agent off; only those fields are sent. Fields: " + strings.Join(AgentSettingsFields(), ", ") + ". Not offered (refused): launchCmd, cliPinned, cliPinValue, sandboxEnabled, sandboxSubstrate, engine, role, promptSource and definitionSource stay on the dashboard; model has agent.model and githubApiHourlyCap has agent.github_api_hourly_cap. The preview reads the agent's settings, shows each named field before and after, and names as widening turning the agent on, a higher GitHub tier (mode), leaving on-demand, a wider repository scope, more replicas, continuous mode, and joining the contributor-first lane. write_confirm refuses if any named field changed after the preview. A replica or review head is refused: change its base agent."
}
func (agentSettingsOp) InputSchema() map[string]any {
	props := map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}}
	for _, f := range agentSettingsFields {
		var s map[string]any
		switch f.kind {
		case agentSettingBool:
			s = map[string]any{"type": "boolean"}
		case agentSettingString:
			s = map[string]any{"type": "string"}
		case agentSettingEnum:
			s = map[string]any{"type": "string", "enum": slices.Clone(f.enum)}
		case agentSettingCount:
			s = map[string]any{"type": "integer", "minimum": 0}
		case agentSettingSeconds:
			s = map[string]any{"type": "integer", "minimum": 1}
		case agentSettingList:
			s = map[string]any{"type": "array", "items": stringSchema()}
		}
		s["description"] = f.desc
		props[f.key] = s
	}
	return map[string]any{"type": "object", "properties": props, "required": []string{"agent"}, "additionalProperties": false}
}

// Preview without a provider checks the arguments and names the endpoint;
// write_preview always goes through PreviewWithProvider, and write_confirm
// uses this only to check the method and path.
func (agentSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, _, err := parseAgentSettingsArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	return unreadSettingsPreview(WriteOpAgentSettings, agent, agentGeneralPath(agent), "settings of agent "+agent), nil
}

// ConfirmGuard reads the agent's settings again at write_confirm and returns
// the named fields' fingerprint, which must still match the preview's.
func (op agentSettingsOp) ConfirmGuard(ctx context.Context, args map[string]any, provider Provider) (string, error) {
	return confirmGuardFingerprint(op.PreviewWithProvider(ctx, args, provider))
}

// PreviewWithProvider reads the agent's settings, shows each named field
// before and after, and discloses each widening change.
func (agentSettingsOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	agent, body, err := parseAgentSettingsArgs(args)
	if err != nil {
		return WritePreview{}, err
	}
	st, err := readAgentGeneral(ctx, provider, agent)
	if err != nil {
		return WritePreview{}, err
	}
	if st.replicaOf != "" {
		// The endpoint would save the change and then rebuild this copy from
		// its base agent (config.ExpandAgentReplicas), so it would not last.
		return WritePreview{}, fmt.Errorf("%s: agent %s is a replica or review head of %s and takes its settings from it, so a change here would not be kept; change %s instead", WriteOpAgentSettings, agent, st.replicaOf, st.replicaOf)
	}
	fingerprint, err := agentSettingsFingerprint(st, body)
	if err != nil {
		return WritePreview{}, err
	}
	var changes []settingChange
	for _, f := range agentSettingsFields {
		v, ok := body[f.key]
		if !ok {
			continue
		}
		c := settingChange{Field: f.key, Before: st.general[f.key], After: v}
		switch f.kind {
		case agentSettingList:
			c.Before = generalList(st.general, f.key)
		case agentSettingEnum:
			if f.key == "mode" {
				from, _ := st.general["mode"].(string)
				c.Before, c.After = agentModeLabel(from), agentModeLabel(v.(string))
			}
		}
		changes = append(changes, c)
	}
	effects := make([]string, 0, len(changes)+3)
	for _, c := range changes {
		effects = append(effects, c.effect())
	}
	effects = append(effects,
		"Hive saves it through agent "+agent+"'s General settings (PUT "+agentGeneralPath(agent)+"), the same save the dashboard makes, validates it there, and records a config_agent_general audit entry naming you. Fields not named here are not sent and keep their value.",
	)
	if _, ok := body["enabled"]; ok {
		effects = append(effects, "Turning an agent off stops its session and the governor stops kicking it; turning it on starts it, unless it is on demand.")
	}
	if _, ok := body["mode"]; ok {
		effects = append(effects, "mode becomes operator-owned, so a later ACMM level change or pack apply no longer resets it.")
	}
	if _, ok := body["repos"]; ok {
		effects = append(effects, "repos becomes operator-owned, so a pack apply no longer resets it.")
	}
	widening := agentSettingsWidening(agent, st.general, body)
	disclosure := "No widening: " + agent + " is not turned on, its GitHub tier is not raised, its repository scope is not widened, and it does not run more."
	if len(widening) > 0 {
		disclosure = "Potential widening: " + strings.Join(widening, "; ") + "."
	}
	return WritePreview{
		Operation:           WriteOpAgentSettings,
		Summary:             "Change agent " + agent + "'s settings: " + agentSettingsChanged(body),
		Target:              agent,
		Request:             WriteRequest{Method: http.MethodPut, Path: agentGeneralPath(agent), Body: body},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm changing agent " + agent + "'s " + agentSettingsChanged(body) + ".",
		Details:             map[string]any{"agent": agent, "changes": changeDetails(changes)},
		StateFingerprint:    fingerprint,
	}, nil
}

// ConfirmResult reads the agent's settings back after the write and returns
// each named field as the hive now holds it.
func (agentSettingsOp) ConfirmResult(ctx context.Context, args map[string]any, provider Provider) (any, error) {
	agent, body, err := parseAgentSettingsArgs(args)
	if err != nil {
		return nil, err
	}
	st, err := readAgentGeneral(ctx, provider, agent)
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for k := range body {
		out[k] = st.general[k]
	}
	return out, nil
}
