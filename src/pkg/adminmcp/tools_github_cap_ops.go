package adminmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// Write operations for the GitHub API proxy's per-agent hourly cap: one
// agent's override (agents.<name>.github_api_hourly_cap, through the agent's
// General settings) and the fleet default (agents.github_api_hourly_cap,
// through the Forge App settings). Both go through the endpoints the
// dashboard controls use, so they save and audit the same way.
//
// A higher cap, no cap (0), or an inherited value higher than the current cap
// lets agents spend more of the App installation's one hourly rate-limit
// bucket, which the hive's own loops share, so the preview discloses it as
// widening. Lowering a cap is not widening.
const (
	WriteOpAgentGitHubAPIHourlyCap = "agent.github_api_hourly_cap"
	WriteOpGitHubAgentAPIHourlyCap = "github.agent_api_hourly_cap"

	// defaultAgentGitHubAPIHourlyCap mirrors config.DefaultAgentsGitHubAPIHourlyCap;
	// it is only used to word a preview when the fleet value cannot be read.
	defaultAgentGitHubAPIHourlyCap = 300
)

func githubCapWriteOps() []WriteOp {
	return []WriteOp{agentGitHubCapOp{}, fleetGitHubCapOp{}}
}

type agentGitHubCapOp struct{}
type fleetGitHubCapOp struct{}

const githubCapBucketDisclosure = "Every agent's GitHub API calls draw on the one hourly rate-limit bucket of this hive's GitHub App installation, which the hive's own scan, merge and review loops share, so agents may now spend more of it. The agent reserve floor (github.agent_reserve_floor, default 400) still applies: once the bucket's remaining calls drop below it, agent reads are refused whatever their cap."

func (agentGitHubCapOp) Name() string { return WriteOpAgentGitHubAPIHourlyCap }
func (agentGitHubCapOp) Description() string {
	return "Set one agent's GitHub API hourly cap in the API proxy (agents.<name>.github_api_hourly_cap) through PUT /api/config/agent/{name}/general: a whole number, 0 for no cap, or inherit to follow the fleet default. Set it on a base agent; its replicas and review heads use the same cap."
}
func (agentGitHubCapOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"agent": map[string]any{"type": "string", "minLength": 1},
		"cap":   githubCapArgSchema("inherit", "the fleet default"),
	}, "required": []string{"agent", "cap"}, "additionalProperties": false}
}
func (op agentGitHubCapOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}

// PreviewWithProvider reads the agent's settings to compare the current cap
// with the new one; without a reading the change is disclosed as potential
// widening.
func (agentGitHubCapOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	newCap, err := githubCapArg(args, "inherit")
	if err != nil {
		return WritePreview{}, err
	}
	cur := readAgentGitHubCap(ctx, provider, agent)
	if cur.base != "" {
		return WritePreview{}, fmt.Errorf("agent %s is a replica or review head of %s and takes its GitHub API hourly cap from it; set the cap on %s", agent, cur.base, cur.base)
	}
	label := githubCapOverrideLabel(newCap)
	to, toKnown := 0, false
	if newCap != nil {
		to, toKnown = *newCap, true
	} else if cur.fleetKnown {
		to, toKnown = cur.fleet, true
	}
	if newCap == nil {
		label = "inherit the fleet default"
		if cur.fleetKnown {
			label += " (" + githubCapLabel(cur.fleet) + ")"
		}
	}
	disclosure := githubCapDisclosure("agent "+agent+"'s cap", cur.effective, cur.effectiveKnown, to, toKnown)
	var body any
	if newCap != nil {
		body = *newCap
	}
	effects := []string{
		"Hive writes agents." + agent + ".github_api_hourly_cap through the agent's General settings (" + githubCapEffectWord(newCap) + "), saves it to hive.yaml and the dashboard overlay, and records a config_agent_general audit entry naming the old and new value.",
		"The proxy applies the new cap to this agent's next GitHub API request, without a restart. Replicas and review heads of this agent take the same cap.",
		"Calls already counted in the current sliding hour still count against the new cap.",
	}
	details := map[string]any{"agent": agent, "cap": githubCapDetail(newCap)}
	if cur.effectiveKnown {
		details["current_cap"] = cur.effective
	}
	return WritePreview{
		Operation:           WriteOpAgentGitHubAPIHourlyCap,
		Summary:             "Set the GitHub API hourly cap for agent " + agent + " to " + label,
		Target:              agent,
		Request:             WriteRequest{Method: http.MethodPut, Path: "/api/config/agent/" + url.PathEscape(agent) + "/general", Body: map[string]any{"githubApiHourlyCap": body}},
		Effects:             effects,
		WideningDisclosure:  disclosure,
		ConfirmationMessage: "Confirm setting agent " + agent + "'s GitHub API hourly cap to " + label + ".",
		Details:             details,
	}, nil
}

func (fleetGitHubCapOp) Name() string { return WriteOpGitHubAgentAPIHourlyCap }
func (fleetGitHubCapOp) Description() string {
	return "Set the fleet default GitHub API hourly cap per agent in the API proxy (agents.github_api_hourly_cap) through PUT /api/config/github: a whole number, 0 for no cap, or default to remove the key so the built-in default (300) applies. Agents with their own cap keep it."
}
func (fleetGitHubCapOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{
		"cap": githubCapArgSchema("default", "the built-in default (300)"),
	}, "required": []string{"cap"}, "additionalProperties": false}
}
func (op fleetGitHubCapOp) Preview(ctx context.Context, args map[string]any) (WritePreview, error) {
	return op.PreviewWithProvider(ctx, args, nil)
}

// PreviewWithProvider reads the Forge App settings to compare the current
// fleet cap with the new one.
func (fleetGitHubCapOp) PreviewWithProvider(ctx context.Context, args map[string]any, provider Provider) (WritePreview, error) {
	newCap, err := githubCapArg(args, "default")
	if err != nil {
		return WritePreview{}, err
	}
	cur, curKnown := readFleetGitHubCap(ctx, provider)
	to := defaultAgentGitHubAPIHourlyCap
	label := "the default (" + githubCapLabel(defaultAgentGitHubAPIHourlyCap) + ")"
	if newCap != nil {
		to = *newCap
		label = githubCapLabel(*newCap)
	}
	var body any
	if newCap != nil {
		body = *newCap
	}
	details := map[string]any{"cap": githubCapDetail(newCap)}
	if curKnown {
		details["current_cap"] = cur
	}
	return WritePreview{
		Operation: WriteOpGitHubAgentAPIHourlyCap,
		Summary:   "Set the fleet default GitHub API hourly cap per agent to " + label,
		Target:    "agents.github_api_hourly_cap",
		Request:   WriteRequest{Method: http.MethodPut, Path: "/api/config/github", Body: map[string]any{"agent_github_api_hourly_cap": body}},
		Effects: []string{
			"Hive writes agents.github_api_hourly_cap through the Forge App settings (" + githubCapEffectWord(newCap) + "), saves it to hive.yaml and the dashboard overlay, and records a config_github audit entry naming the old and new value.",
			"The proxy applies it to the next GitHub API request of every agent without a cap of its own, without a restart; agents with their own cap keep it.",
			"As with any save of these settings, the hive also rebuilds its GitHub App client from the saved App settings.",
		},
		WideningDisclosure:  githubCapDisclosure("the fleet default cap", cur, curKnown, to, true),
		ConfirmationMessage: "Confirm setting the fleet default GitHub API hourly cap per agent to " + label + ".",
		Details:             details,
	}, nil
}

func githubCapArgSchema(keyword, meaning string) map[string]any {
	return map[string]any{
		"anyOf":       []any{map[string]any{"type": "integer", "minimum": 0}, map[string]any{"type": "string", "enum": []string{keyword}}},
		"description": "Charged GitHub API responses allowed per sliding hour: a whole number, 0 for no cap, or \"" + keyword + "\" for " + meaning + ".",
	}
}

// githubCapArg returns the requested cap, or nil for the keyword.
func githubCapArg(args map[string]any, keyword string) (*int, error) {
	if s, ok := args["cap"].(string); ok && strings.EqualFold(strings.TrimSpace(s), keyword) {
		return nil, nil
	}
	n, ok, err := optionalIntArg(args, "cap")
	if err != nil || !ok || n < 0 {
		return nil, fmt.Errorf("cap must be a whole number of at least 0 (0 = no cap), or %q", keyword)
	}
	return &n, nil
}

// githubCapWidens reports whether moving a cap from one value to another lets
// an agent make more calls: 0 is no cap.
func githubCapWidens(from, to int) bool {
	if from == 0 {
		return false
	}
	return to == 0 || to > from
}

func githubCapDisclosure(what string, from int, fromKnown bool, to int, toKnown bool) string {
	if fromKnown && toKnown {
		if githubCapWidens(from, to) {
			return "Widening: " + what + " goes from " + githubCapLabel(from) + " to " + githubCapLabel(to) + ". " + githubCapBucketDisclosure
		}
		return "No widening: " + what + " goes from " + githubCapLabel(from) + " to " + githubCapLabel(to) + ", which does not raise it."
	}
	return "Potential widening: the current or resulting cap could not be read. If this raises " + what + " or removes it (0), it is widening. " + githubCapBucketDisclosure
}

func githubCapLabel(n int) string {
	if n == 0 {
		return "no cap (0)"
	}
	return strconv.Itoa(n)
}

func githubCapOverrideLabel(cap *int) string {
	if cap == nil {
		return "inherit"
	}
	return githubCapLabel(*cap)
}

func githubCapEffectWord(cap *int) string {
	if cap == nil {
		return "removing the key"
	}
	return "setting it to " + strconv.Itoa(*cap)
}

func githubCapDetail(cap *int) any {
	if cap == nil {
		return nil
	}
	return *cap
}

type agentGitHubCapReading struct {
	effective      int
	effectiveKnown bool
	fleet          int
	fleetKnown     bool
	base           string
}

// readAgentGitHubCap reads the agent's General settings (settings_read
// section agent): its effective cap, the fleet default and, for a replica or
// review head, its base agent. A failed read leaves everything unknown.
func readAgentGitHubCap(ctx context.Context, provider Provider, agent string) agentGitHubCapReading {
	var out agentGitHubCapReading
	if provider == nil {
		return out
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": "agent", "agent": agent})
	if err != nil {
		return out
	}
	m, _ := data.(map[string]any)
	general, _ := m["general"].(map[string]any)
	if general == nil {
		return out
	}
	out.effective, out.effectiveKnown = numberArg(general["githubApiHourlyCapEffective"])
	out.fleet, out.fleetKnown = numberArg(general["githubApiHourlyCapFleet"])
	out.base, _ = general["githubApiHourlyCapBase"].(string)
	return out
}

// readFleetGitHubCap reads the fleet default from the Forge App settings
// (settings_read section github_apps).
func readFleetGitHubCap(ctx context.Context, provider Provider) (int, bool) {
	if provider == nil {
		return 0, false
	}
	data, err := provider.Read(ctx, ToolSettingsRead, map[string]any{"section": "github_apps"})
	if err != nil {
		return 0, false
	}
	m, _ := data.(map[string]any)
	settings, _ := m["settings"].(map[string]any)
	if settings == nil {
		return 0, false
	}
	return numberArg(settings["agent_github_api_hourly_cap"])
}

func numberArg(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n < 0 || n != float64(int(n)) {
			return 0, false
		}
		return int(n), true
	case int:
		return n, n >= 0
	case json.Number:
		i, err := n.Int64()
		if err != nil || i < 0 {
			return 0, false
		}
		return int(i), true
	}
	return 0, false
}
