package adminmcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

const (
	WriteOpFleetAutonomyLevel = "fleet.autonomy_level"
	WriteOpPlanPropose        = "plan.propose"
	WriteOpPlanApprove        = "plan.approve"
	WriteOpPlanReject         = "plan.reject"
	WriteOpFeatureSettings    = "governor.feature_settings"
)

func fleetWriteOps() []WriteOp {
	return []WriteOp{
		fleetAutonomyLevelOp{},
		planProposeOp{},
		planApproveOp{},
		planRejectOp{},
		featureSettingsOp{},
	}
}

type fleetAutonomyLevelOp struct{}
type planProposeOp struct{}
type planApproveOp struct{}
type planRejectOp struct{}
type featureSettingsOp struct{}

func (fleetAutonomyLevelOp) Name() string { return WriteOpFleetAutonomyLevel }
func (fleetAutonomyLevelOp) Description() string {
	return "Set the hive autonomy level through PUT /api/packs/level with a capability preview."
}
func (fleetAutonomyLevelOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"level": map[string]any{"type": "integer", "minimum": config.MinACMMLevel, "maximum": config.MaxACMMLevel}}, "required": []string{"level"}, "additionalProperties": false}
}
func (fleetAutonomyLevelOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	level, err := intArg(args, "level")
	if err != nil {
		return WritePreview{}, err
	}
	if level < config.MinACMMLevel || level > config.MaxACMMLevel {
		return WritePreview{}, fmt.Errorf("level must be %d-%d", config.MinACMMLevel, config.MaxACMMLevel)
	}
	pack, err := config.ACMMPackByLevel(level)
	if err != nil {
		return WritePreview{}, err
	}
	agents := make([]map[string]any, 0, len(pack.Agents))
	var widened []string
	for _, a := range pack.Agents {
		if a.Hidden {
			continue
		}
		authority := capabilityForMode(a.Mode)
		agents = append(agents, map[string]any{"name": a.Name, "mode": a.Mode, "authority": authority, "on_demand": a.OnDemand})
		if capabilityWidens(a.Mode) {
			widened = append(widened, fmt.Sprintf("%s: %s", a.Name, authority))
		}
	}
	disclosure := "No widening detected in the target pack's agent authority."
	if len(widened) > 0 {
		disclosure = "Widening: the target level permits broader repository authority for " + strings.Join(widened, "; ") + ". Effects such as opened or merged pull requests do not unwind when the level is lowered."
	}
	effects := []string{
		fmt.Sprintf("Set the hive ACMM/autonomy level to L%d (%s).", level, pack.Name),
		"Apply the level pack, reconciling managed agents, visibility, mode files, and pack-owned governor cadences through Hive's existing handler.",
		"Use the hive's own readiness endpoints before confirming if the operator needs current fitness evidence.",
	}
	return WritePreview{Operation: WriteOpFleetAutonomyLevel, Summary: fmt.Sprintf("Set hive autonomy level to L%d", level), Target: fmt.Sprintf("fleet autonomy level L%d", level), Request: WriteRequest{Method: http.MethodPut, Path: "/api/packs/level", Body: map[string]any{"level": level}}, Effects: effects, WideningDisclosure: disclosure, ConfirmationMessage: fmt.Sprintf("Confirm setting hive autonomy level to L%d (%s).", level, pack.Name), Details: map[string]any{"level": level, "pack": pack.Name, "description": pack.Description, "agents": agents, "governor": pack.Governor}}, nil
}

func (planProposeOp) Name() string { return WriteOpPlanPropose }
func (planProposeOp) Description() string {
	return "Propose a draft plan through POST /api/plan/from-issue."
}
func (planProposeOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"repo": stringSchema(), "number": map[string]any{"type": "integer"}, "url": stringSchema(), "title": stringSchema(), "body": stringSchema()}, "required": []string{}, "additionalProperties": false}
}
func (planProposeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	body := map[string]any{}
	for _, key := range []string{"repo", "url", "title", "body"} {
		if v := stringArg(args, key); v != "" {
			body[key] = v
		}
	}
	if n, ok, err := optionalIntArg(args, "number"); err != nil {
		return WritePreview{}, err
	} else if ok {
		body["number"] = n
	}
	if body["title"] == nil && (body["repo"] == nil || body["number"] == nil) {
		return WritePreview{}, fmt.Errorf("plan proposal requires title or resolvable repo and number")
	}
	target := strings.TrimSpace(fmt.Sprint(body["title"]))
	if target == "" {
		target = fmt.Sprintf("%s#%v", body["repo"], body["number"])
	}
	return WritePreview{Operation: WriteOpPlanPropose, Summary: "Propose draft plan for " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: "/api/plan/from-issue", Body: body}, Effects: []string{"Mint or reuse a draft epic for the issue.", "May queue or kick the architect to decompose the draft.", "Does not release child work until a plan is approved."}, WideningDisclosure: "No direct authority widening: proposal creates draft planning state only. Hive may refuse below ACMM L5 or on critical ioscan findings.", ConfirmationMessage: "Confirm proposing a draft plan for " + target + ".", Details: body}, nil
}

func (planApproveOp) Name() string { return WriteOpPlanApprove }
func (planApproveOp) Description() string {
	return "Approve a draft plan through POST /api/plan/{epicID}/approve."
}
func (planApproveOp) InputSchema() map[string]any { return epicSchema() }
func (planApproveOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	epicID, err := epicIDArg(args)
	if err != nil {
		return WritePreview{}, err
	}
	return WritePreview{Operation: WriteOpPlanApprove, Summary: "Approve plan " + epicID, Target: epicID, Request: WriteRequest{Method: http.MethodPost, Path: "/api/plan/" + url.PathEscape(epicID) + "/approve"}, Effects: []string{"Release the plan's children through Hive's Ready() gate.", "May advance a held plan lease; Hive returns a conflict if lease release fails.", "May mirror the approved checklist to the source issue when configured."}, WideningDisclosure: "Widening: approving a plan can release child work to agents.", ConfirmationMessage: "Confirm approving plan " + epicID + ".", Details: map[string]any{"epic_id": epicID}}, nil
}

func (planRejectOp) Name() string { return WriteOpPlanReject }
func (planRejectOp) Description() string {
	return "Reject a plan through POST /api/plan/{epicID}/reject."
}
func (planRejectOp) InputSchema() map[string]any { return epicSchema() }
func (planRejectOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	epicID, err := epicIDArg(args)
	if err != nil {
		return WritePreview{}, err
	}
	return WritePreview{Operation: WriteOpPlanReject, Summary: "Reject plan " + epicID, Target: epicID, Request: WriteRequest{Method: http.MethodPost, Path: "/api/plan/" + url.PathEscape(epicID) + "/reject"}, Effects: []string{"Return the plan to draft state and re-gate its children.", "Reset any implement-stage run lease back to plan review."}, WideningDisclosure: "No widening: rejection narrows execution by re-gating plan children.", ConfirmationMessage: "Confirm rejecting plan " + epicID + ".", Details: map[string]any{"epic_id": epicID}}, nil
}

func (featureSettingsOp) Name() string { return WriteOpFeatureSettings }
func (featureSettingsOp) Description() string {
	return "Update governor feature settings through PUT /api/config/governor/features."
}
func (featureSettingsOp) InputSchema() map[string]any {
	props := map[string]any{}
	for _, name := range boolFeatureFields() {
		props[name] = map[string]any{"type": "boolean"}
	}
	for _, name := range intFeatureFields() {
		props[name] = map[string]any{"type": "integer"}
	}
	for _, name := range stringFeatureFields() {
		props[name] = stringSchema()
	}
	props["triageSpecLabels"] = map[string]any{"type": "array", "items": stringSchema()}
	props["triageFixLabels"] = map[string]any{"type": "array", "items": stringSchema()}
	props["otelHeaders"] = map[string]any{"type": "object", "additionalProperties": stringSchema()}
	props["rotationProviders"] = map[string]any{"type": "object"}
	props["rotationAgents"] = map[string]any{"type": "object", "additionalProperties": stringSchema()}
	props["otelSampleRatio"] = map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	props["tracingSampleRatio"] = map[string]any{"type": "number", "minimum": 0, "maximum": 1}
	return map[string]any{"type": "object", "properties": props, "additionalProperties": false}
}
func (op featureSettingsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	allowed := featureFieldSet(op.InputSchema())
	body := map[string]any{}
	var fields []string
	for k, v := range args {
		if !allowed[k] {
			return WritePreview{}, fmt.Errorf("unsupported feature setting %q", k)
		}
		body[k] = v
		fields = append(fields, k)
	}
	sort.Strings(fields)
	if len(fields) == 0 {
		return WritePreview{}, fmt.Errorf("at least one feature setting is required")
	}
	disclosure := featureDisclosure(args)
	return WritePreview{Operation: WriteOpFeatureSettings, Summary: "Update governor feature settings: " + strings.Join(fields, ", "), Target: "governor feature settings", Request: WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/features", Body: body}, Effects: []string{"Persist the provided feature settings; omitted fields remain unchanged.", "Hive validates the same fields as the dashboard feature dialog."}, WideningDisclosure: disclosure, ConfirmationMessage: "Confirm updating governor feature settings: " + strings.Join(fields, ", ") + ".", Details: map[string]any{"fields": fields, "body": body}}, nil
}

func capabilityForMode(mode string) string {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "ISSUES_PRS_MERGE":
		return "may create issues, open pull requests, push branches, and merge on green CI"
	case "ISSUES_AND_PRS":
		return "may create issues, open pull requests, and push branches"
	case "ISSUES_ONLY":
		return "may create issues"
	default:
		return "advisory only; does not create issues, pull requests, pushes, or merges"
	}
}

func capabilityWidens(mode string) bool {
	switch strings.ToUpper(strings.TrimSpace(mode)) {
	case "ISSUES_ONLY", "ISSUES_AND_PRS", "ISSUES_PRS_MERGE":
		return true
	default:
		return false
	}
}

func featureDisclosure(args map[string]any) string {
	var warnings []string
	for _, key := range []string{"ioscanEnabled", "checkpointSpecEnabled", "checkpointPlanEnabled", "checkpointImplementEnabled", "claimsEnabled"} {
		if v, ok := args[key].(bool); ok && !v {
			warnings = append(warnings, key+" disables a protection or coordination gate")
		}
	}
	for _, key := range []string{"autonomyAutoPromote", "runStages", "triageEnabled", "publicationEnabled", "extFlueEnabled", "extOmpEnabled", "mintEnabled", "formalEnabled"} {
		if v, ok := args[key].(bool); ok && v {
			warnings = append(warnings, key+" enables additional fleet behavior")
		}
	}
	if len(warnings) == 0 {
		return "No obvious authority widening detected from the provided feature fields."
	}
	sort.Strings(warnings)
	return "Widening/disclosure: " + strings.Join(warnings, "; ") + "."
}

func intArg(args map[string]any, key string) (int, error) {
	n, ok, err := optionalIntArg(args, key)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("%s is required", key)
	}
	return n, nil
}

func optionalIntArg(args map[string]any, key string) (int, bool, error) {
	switch v := args[key].(type) {
	case nil:
		return 0, false, nil
	case int:
		return v, true, nil
	case float64:
		if v != float64(int(v)) {
			return 0, true, fmt.Errorf("%s must be an integer", key)
		}
		return int(v), true, nil
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			return 0, true, fmt.Errorf("%s must be an integer", key)
		}
		return n, true, nil
	default:
		return 0, true, fmt.Errorf("%s must be an integer", key)
	}
}

func epicIDArg(args map[string]any) (string, error) {
	epicID := stringArg(args, "epic_id")
	if epicID == "" {
		epicID = stringArg(args, "epicID")
	}
	if epicID == "" {
		return "", fmt.Errorf("epic_id is required")
	}
	return epicID, nil
}

func epicSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"epic_id": stringSchema()}, "required": []string{"epic_id"}, "additionalProperties": false}
}

func featureFieldSet(schema map[string]any) map[string]bool {
	props, _ := schema["properties"].(map[string]any)
	out := map[string]bool{}
	for k := range props {
		out[k] = true
	}
	return out
}

func boolFeatureFields() []string {
	return []string{"ioscanEnabled", "tracingEnabled", "otelEnabled", "retroEnabled", "mintEnabled", "planFromLabel", "formalEnabled", "personaLearningEnabled", "checkpointSpecEnabled", "checkpointPlanEnabled", "checkpointImplementEnabled", "planMatchEnabled", "spektacularEnabled", "runStages", "triageEnabled", "triageClarifyComment", "publicationEnabled", "extFlueEnabled", "extOmpEnabled", "wavefrontEnabled", "claimsEnabled", "autonomyAutoPromote", "autonomyAutoDemote", "rotationEnabled", "otelInsecure"}
}

func intFeatureFields() []string {
	return []string{"runWaitTimeoutSeconds", "spektacularPollS", "maxStageRetries", "triageMinBodyChars", "claimsTtlS", "autonomyPromoteAfter", "autonomyMaxLevel", "autonomyCooldownDays", "rotationThresholdPct", "rotationHighVolumeCadenceS"}
}

func stringFeatureFields() []string {
	return []string{"tracingEndpoint", "otelEndpoint", "otelServiceName", "retroAnalysisModel", "mintIssuer", "runWaitSeverity", "spektacularBinary", "publicationPrivateChannel", "publicationOwner", "extFlueMode", "extFlueEndpoint", "extFlueWorkflowVersion", "extOmpMode", "wavefrontPath", "wavefrontUrl", "wavefrontRepo", "wavefrontReceiptsDir", "autonomyDemoteOn"}
}
