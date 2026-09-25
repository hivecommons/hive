package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	ToolAgentNudgeStatus = "agent_nudge_status"

	WriteOpAgentPause           = "agent.pause"
	WriteOpAgentResume          = "agent.resume"
	WriteOpAgentNudge           = "agent.nudge"
	WriteOpAgentRestart         = "agent.restart"
	WriteOpAgentAdd             = "agent.add"
	WriteOpAgentRemove          = "agent.remove"
	WriteOpAgentModel           = "agent.model"
	WriteOpAgentBackend         = "agent.backend"
	WriteOpAgentEffort          = "agent.effort"
	WriteOpAgentInteractionTier = "agent.interaction_tier"
)

func DefaultWriteOps() []WriteOp {
	agentOps := []WriteOp{
		agentPauseOp{}, agentResumeOp{}, agentNudgeOp{}, agentRestartOp{}, agentAddOp{}, agentRemoveOp{},
		agentModelOp{}, agentBackendOp{}, agentEffortOp{}, agentInteractionTierOp{},
	}
	ops := append(agentOps, repoOpsWriteOps()...)
	return append(ops, fleetWriteOps()...)
}

type agentPauseOp struct{}
type agentResumeOp struct{}
type agentNudgeOp struct{}
type agentRestartOp struct{}
type agentAddOp struct{}
type agentRemoveOp struct{}
type agentModelOp struct{}
type agentBackendOp struct{}
type agentEffortOp struct{}
type agentInteractionTierOp struct{}

func (agentPauseOp) Name() string                { return WriteOpAgentPause }
func (agentPauseOp) Description() string         { return "Pause one agent through POST /api/pause/{agent}." }
func (agentPauseOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentPauseOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/pause/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentPause, Summary: "Pause agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"The named agent stops accepting new work until resumed."}, WideningDisclosure: "No widening: this operation targets exactly one named agent.", ConfirmationMessage: "Confirm pausing agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func (agentResumeOp) Name() string { return WriteOpAgentResume }
func (agentResumeOp) Description() string {
	return "Resume one agent through POST /api/resume/{agent}."
}
func (agentResumeOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentResumeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/resume/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentResume, Summary: "Resume agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"The named agent may accept work again."}, WideningDisclosure: "No widening: this operation targets exactly one named agent.", ConfirmationMessage: "Confirm resuming agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func (agentNudgeOp) Name() string { return WriteOpAgentNudge }
func (agentNudgeOp) Description() string {
	return "Nudge one agent with a verbatim prompt through POST /api/kick/{agent}; check the asynchronous outcome with agent_nudge_status."
}
func (agentNudgeOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}, "prompt": map[string]any{"type": "string", "minLength": 1, "maxLength": 10000}}, "required": []string{"agent", "prompt"}, "additionalProperties": false}
}
func (agentNudgeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	prompt, err := verbatimPromptArg(args)
	if err != nil {
		return WritePreview{}, err
	}
	const maxKickPromptLen = 10000
	if len(prompt) > maxKickPromptLen {
		return WritePreview{}, fmt.Errorf("prompt too long (%d chars, max %d)", len(prompt), maxKickPromptLen)
	}
	path := "/api/kick/" + url.PathEscape(agent)
	return WritePreview{
		Operation: WriteOpAgentNudge,
		Summary:   "Nudge agent " + agent + " with the verbatim prompt shown in the confirmation message.",
		Target:    agent,
		Request:   WriteRequest{Method: http.MethodPost, Path: path, Body: map[string]any{"prompt": prompt}},
		Effects: []string{
			"The prompt is queued for asynchronous delivery to the named agent's CLI session.",
			"The POST only reports queued or in-flight delivery; call agent_nudge_status for the settled outcome.",
		},
		WideningDisclosure:  "No widening: this operation targets exactly one named agent, but the prompt may cause that agent to act within its existing authority.",
		ConfirmationMessage: "Confirm nudging agent " + agent + " with this verbatim prompt:\n\n" + prompt,
		Details:             map[string]any{"agent": agent, "prompt": prompt, "outcome_lookup": ToolAgentNudgeStatus},
	}, nil
}

func (agentRestartOp) Name() string { return WriteOpAgentRestart }
func (agentRestartOp) Description() string {
	return "Restart one agent through POST /api/restart/{agent}."
}
func (agentRestartOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentRestartOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/restart/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentRestart, Summary: "Restart agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"The named agent session is restarted.", "Any pending kick for that agent may be cancelled by the restart."}, WideningDisclosure: "No widening: this operation restarts exactly one named agent without changing its configured authority.", ConfirmationMessage: "Confirm restarting agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func (agentAddOp) Name() string { return WriteOpAgentAdd }
func (agentAddOp) Description() string {
	return "Create one managed agent through POST /api/agents."
}
func (agentAddOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"name": map[string]any{"type": "string", "minLength": 1}, "config": map[string]any{"type": "object"}}, "required": []string{"name"}, "additionalProperties": false}
}
func (agentAddOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	name, err := requiredString(args, "name")
	if err != nil {
		return WritePreview{}, err
	}
	cfg, _ := args["config"].(map[string]any)
	if cfg == nil {
		cfg = map[string]any{}
	}
	return WritePreview{Operation: WriteOpAgentAdd, Summary: "Add managed agent " + name, Target: name, Request: WriteRequest{Method: http.MethodPost, Path: "/api/agents", Body: map[string]any{"name": name, "agent": cfg}}, Effects: []string{"Hive writes a managed agent overlay, reconciles the running agent set, and starts the agent when applicable."}, WideningDisclosure: "Potential widening: creating an enabled agent can add a new live process with the authority granted by its configured interaction tier and credentials.", ConfirmationMessage: "Confirm adding managed agent " + name + ".", Details: map[string]any{"agent": name, "config": cfg}}, nil
}

func (agentRemoveOp) Name() string { return WriteOpAgentRemove }
func (agentRemoveOp) Description() string {
	return "Remove one managed agent through DELETE /api/agents/{name}."
}
func (agentRemoveOp) InputSchema() map[string]any { return agentOpSchema() }
func (agentRemoveOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/agents/" + url.PathEscape(agent)
	return WritePreview{Operation: WriteOpAgentRemove, Summary: "Remove managed agent " + agent, Target: agent, Request: WriteRequest{Method: http.MethodDelete, Path: path}, Effects: []string{"Hive removes the managed agent overlay, reconciles running agents, and records a deletion tombstone."}, WideningDisclosure: "No widening: this operation removes exactly one managed agent.", ConfirmationMessage: "Confirm removing managed agent " + agent + ".", Details: map[string]any{"agent": agent}}, nil
}

func (agentModelOp) Name() string { return WriteOpAgentModel }
func (agentModelOp) Description() string {
	return "Set one agent's model through POST /api/model/{agent}/{model}."
}
func (agentModelOp) InputSchema() map[string]any { return agentValueOpSchema("model") }
func (agentModelOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, model, err := requiredAgentValue(args, "model")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/model/" + url.PathEscape(agent) + "/" + url.PathEscape(model)
	return WritePreview{Operation: WriteOpAgentModel, Summary: "Set model for agent " + agent + " to " + model, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"Hive validates the model against the effective backend, persists operator ownership, and restarts the agent session."}, WideningDisclosure: "No authority widening: this changes model selection for exactly one named agent but not its interaction tier.", ConfirmationMessage: "Confirm setting agent " + agent + " model to " + model + ".", Details: map[string]any{"agent": agent, "model": model}}, nil
}

func (agentBackendOp) Name() string { return WriteOpAgentBackend }
func (agentBackendOp) Description() string {
	return "Set one agent's backend through POST /api/switch/{agent}/{backend}."
}
func (agentBackendOp) InputSchema() map[string]any { return agentValueOpSchema("backend") }
func (agentBackendOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, backend, err := requiredAgentValue(args, "backend")
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/switch/" + url.PathEscape(agent) + "/" + url.PathEscape(backend)
	return WritePreview{Operation: WriteOpAgentBackend, Summary: "Set backend for agent " + agent + " to " + backend, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"Hive persists operator ownership of the backend and restarts the agent session."}, WideningDisclosure: "No authority widening: this changes backend selection for exactly one named agent but not its interaction tier.", ConfirmationMessage: "Confirm setting agent " + agent + " backend to " + backend + ".", Details: map[string]any{"agent": agent, "backend": backend}}, nil
}

func (agentEffortOp) Name() string { return WriteOpAgentEffort }
func (agentEffortOp) Description() string {
	return "Set one agent's reasoning effort through POST /api/effort/{agent}/{effort}."
}
func (agentEffortOp) InputSchema() map[string]any { return agentValueOpSchema("effort") }
func (agentEffortOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, effort, err := requiredAgentValue(args, "effort")
	if err != nil {
		return WritePreview{}, err
	}
	pathEffort := effort
	if strings.EqualFold(pathEffort, "default") {
		pathEffort = "default"
	}
	path := "/api/effort/" + url.PathEscape(agent) + "/" + url.PathEscape(pathEffort)
	return WritePreview{Operation: WriteOpAgentEffort, Summary: "Set reasoning effort for agent " + agent + " to " + effort, Target: agent, Request: WriteRequest{Method: http.MethodPost, Path: path}, Effects: []string{"Hive validates the effort against the effective backend and model, persists it, and restarts the agent session.", "The special effort value default clears the override."}, WideningDisclosure: "No authority widening: this changes reasoning effort for exactly one named agent but not its interaction tier.", ConfirmationMessage: "Confirm setting agent " + agent + " reasoning effort to " + effort + ".", Details: map[string]any{"agent": agent, "effort": effort}}, nil
}

func (agentInteractionTierOp) Name() string { return WriteOpAgentInteractionTier }
func (agentInteractionTierOp) Description() string {
	return "Set one agent's interaction tier through PUT /api/config/agent/{name}/general."
}
func (agentInteractionTierOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}, "tier": map[string]any{"type": "string", "enum": []string{"ADVISORY", "ISSUES_ONLY", "ISSUES_AND_PRS", "ISSUES_PRS_MERGE", "NO_GITHUB", "default"}}}, "required": []string{"agent", "tier"}, "additionalProperties": false}
}
func (agentInteractionTierOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	agent, tier, err := requiredAgentValue(args, "tier")
	if err != nil {
		return WritePreview{}, err
	}
	mode, disclosure, err := normalizeInteractionTier(tier)
	if err != nil {
		return WritePreview{}, err
	}
	path := "/api/config/agent/" + url.PathEscape(agent) + "/general"
	label := tier
	if mode == "" {
		label = "default ACMM-derived tier"
	}
	return WritePreview{Operation: WriteOpAgentInteractionTier, Summary: "Set interaction tier for agent " + agent + " to " + label, Target: agent, Request: WriteRequest{Method: http.MethodPut, Path: path, Body: map[string]any{"mode": mode}}, Effects: interactionTierEffects(mode), WideningDisclosure: disclosure, ConfirmationMessage: "Confirm setting agent " + agent + " interaction tier to " + label + ".", Details: map[string]any{"agent": agent, "tier": label, "mode": mode}}, nil
}

func agentOpSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"agent"}, "additionalProperties": false}
}

func agentValueOpSchema(valueName string) map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"agent": map[string]any{"type": "string", "minLength": 1}, valueName: map[string]any{"type": "string", "minLength": 1}}, "required": []string{"agent", valueName}, "additionalProperties": false}
}

func requiredAgentValue(args map[string]any, valueName string) (string, string, error) {
	agent, err := requiredString(args, "agent")
	if err != nil {
		return "", "", err
	}
	value, err := requiredString(args, valueName)
	if err != nil {
		return "", "", err
	}
	return agent, value, nil
}

func requiredString(args map[string]any, key string) (string, error) {
	value := stringArg(args, key)
	if value == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return value, nil
}

func verbatimPromptArg(args map[string]any) (string, error) {
	value, _ := args["prompt"].(string)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("prompt is required")
	}
	return value, nil
}

func normalizeInteractionTier(tier string) (string, string, error) {
	tier = strings.TrimSpace(tier)
	if strings.EqualFold(tier, "default") {
		return "", "Potential widening: the default ACMM-derived tier may grant more authority than the agent's current explicit tier; confirm only after checking the agent's current config.", nil
	}
	valid := map[string]string{
		"ADVISORY":         "No widening if the agent was already advisory or stronger; this tier cannot create GitHub issues, PRs, or merges.",
		"ISSUES_ONLY":      "Potential widening: ISSUES_ONLY can create GitHub issues but not pull requests or merges.",
		"ISSUES_AND_PRS":   "Potential widening: ISSUES_AND_PRS can create GitHub issues and pull requests but cannot merge them.",
		"ISSUES_PRS_MERGE": "Potential widening: ISSUES_PRS_MERGE can create GitHub issues, create pull requests, and merge on green CI.",
		"NO_GITHUB":        "No widening of GitHub authority: NO_GITHUB prevents GitHub issue, pull request, and merge actions.",
	}
	disclosure, ok := valid[tier]
	if !ok {
		return "", "", fmt.Errorf("tier must be one of: ADVISORY, ISSUES_ONLY, ISSUES_AND_PRS, ISSUES_PRS_MERGE, NO_GITHUB, default")
	}
	return tier, disclosure, nil
}

func interactionTierEffects(mode string) []string {
	if mode == "" {
		return []string{"Hive clears the explicit agent mode so the ACMM level default applies."}
	}
	return []string{"Hive persists the agent mode through the general agent configuration endpoint.", "The new tier governs what GitHub actions the agent may take on future work."}
}

func cleanOperationName(name string) string { return strings.TrimSpace(name) }
