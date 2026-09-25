package adminmcp

import (
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

const (
	WriteOpRepoPause             = "repository.pause"
	WriteOpRepoResume            = "repository.resume"
	WriteOpRepositoryItemHold    = "repository.item_hold"
	WriteOpBudgetUpdate          = "budget.update"
	WriteOpBudgetReset           = "budget.reset"
	WriteOpBudgetIgnore          = "budget.ignore"
	WriteOpContributorTrust      = "contributor.trust"
	WriteOpContributorAgentRole  = "contributor.agent_role"
	WriteOpContributorRoleGrants = "contributor.agent_role_grants"
	WriteOpContributorRevoke     = "contributor.revoke"
	WriteOpContributorRequeue    = "contributor.requeue"
	WriteOpContributorDelete     = "contributor.delete"
	WriteOpBackupCreate          = "backup.create"
	WriteOpCircuitBreakerEngage  = "circuit_breaker.engage"
	WriteOpCircuitBreakerRelease = "circuit_breaker.release"
)

func repoOpsWriteOps() []WriteOp {
	return []WriteOp{
		repoPauseOp{},
		repoResumeOp{},
		repositoryItemHoldOp{},
		budgetUpdateOp{},
		budgetResetOp{},
		budgetIgnoreOp{},
		contributorTrustOp{},
		contributorAgentRoleOp{},
		contributorAgentRoleGrantsOp{},
		contributorRevokeOp{},
		contributorRequeueOp{},
		contributorDeleteOp{},
		backupCreateOp{},
		circuitBreakerEngageOp{},
		circuitBreakerReleaseOp{},
	}
}

type repoPauseOp struct{}
type repoResumeOp struct{}
type repositoryItemHoldOp struct{}
type budgetUpdateOp struct{}
type budgetResetOp struct{}
type budgetIgnoreOp struct{}
type contributorTrustOp struct{}
type contributorAgentRoleOp struct{}
type contributorAgentRoleGrantsOp struct{}
type contributorRevokeOp struct{}
type contributorRequeueOp struct{}
type contributorDeleteOp struct{}
type backupCreateOp struct{}
type circuitBreakerEngageOp struct{}
type circuitBreakerReleaseOp struct{}

func (repoPauseOp) Name() string { return WriteOpRepoPause }
func (repoPauseOp) Description() string {
	return "Pause one watched repository through POST /api/repos/pause."
}
func (repoPauseOp) InputSchema() map[string]any { return repoPauseSchema() }
func (repoPauseOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	repo := stringArg(args, "repo")
	if repo == "" {
		return WritePreview{}, fmt.Errorf("repo is required")
	}
	reason := stringArg(args, "reason")
	body := map[string]any{"repo": repo}
	if reason != "" {
		body["reason"] = reason
	}
	return WritePreview{Operation: WriteOpRepoPause, Summary: "Pause repository " + repo, Target: repo, Request: WriteRequest{Method: http.MethodPost, Path: "/api/repos/pause", Body: body}, Effects: []string{"Agents stop being handed work for the watched repository and stop writing to it until resumed."}, WideningDisclosure: "No widening: this operation targets exactly one repository.", ConfirmationMessage: "Confirm pausing repository " + repo + ".", Details: body}, nil
}

func (repoResumeOp) Name() string { return WriteOpRepoResume }
func (repoResumeOp) Description() string {
	return "Resume one repository through POST /api/repos/resume."
}
func (repoResumeOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"repo": map[string]any{"type": "string", "minLength": 1}}, "required": []string{"repo"}, "additionalProperties": false}
}
func (repoResumeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	repo := stringArg(args, "repo")
	if repo == "" {
		return WritePreview{}, fmt.Errorf("repo is required")
	}
	body := map[string]any{"repo": repo}
	return WritePreview{Operation: WriteOpRepoResume, Summary: "Resume repository " + repo, Target: repo, Request: WriteRequest{Method: http.MethodPost, Path: "/api/repos/resume", Body: body}, Effects: []string{"Agents may receive and act on work for the repository again."}, WideningDisclosure: "No widening: this operation targets exactly one repository pause entry.", ConfirmationMessage: "Confirm resuming repository " + repo + ".", Details: body}, nil
}

func (repositoryItemHoldOp) Name() string { return WriteOpRepositoryItemHold }
func (repositoryItemHoldOp) Description() string {
	return "Add or remove the Hive hold label on one repository item."
}
func (repositoryItemHoldOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"owner": stringSchema(), "repo": stringSchema(), "number": map[string]any{"type": "integer", "minimum": 1}, "held": map[string]any{"type": "boolean"}}, "required": []string{"owner", "repo", "number", "held"}, "additionalProperties": false}
}
func (repositoryItemHoldOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	owner := stringArg(args, "owner")
	repo := stringArg(args, "repo")
	number, err := positiveIntArg(args, "number")
	if err != nil {
		return WritePreview{}, err
	}
	held, ok := boolArg(args, "held")
	if owner == "" || repo == "" || !ok {
		return WritePreview{}, fmt.Errorf("owner, repo, number, and held are required")
	}
	target := owner + "/" + repo + "#" + strconv.Itoa(number)
	body := map[string]any{"held": held}
	action := "Add"
	effect := "The Hive hold label is added so agents will not act on this item."
	if !held {
		action = "Remove"
		effect = "Hive hold labels are removed so agents may act on this item when otherwise eligible."
	}
	path := "/api/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo) + "/items/" + strconv.Itoa(number) + "/hold"
	return WritePreview{Operation: WriteOpRepositoryItemHold, Summary: action + " item hold on " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: path, Body: body}, Effects: []string{effect}, WideningDisclosure: "No widening: this operation targets exactly one repository item.", ConfirmationMessage: "Confirm item hold change on " + target + ".", Details: map[string]any{"owner": owner, "repo": repo, "number": number, "held": held}}, nil
}

func (budgetUpdateOp) Name() string { return WriteOpBudgetUpdate }
func (budgetUpdateOp) Description() string {
	return "Update budget settings through PUT /api/config/governor/budget."
}
func (budgetUpdateOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"totalTokens": map[string]any{"type": "integer", "minimum": 0}, "periodDays": map[string]any{"type": "integer", "minimum": 1}, "criticalPct": map[string]any{"type": "integer", "minimum": 1, "maximum": 100}}, "additionalProperties": false}
}
func (budgetUpdateOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	body := map[string]any{}
	for _, key := range []string{"totalTokens", "periodDays", "criticalPct"} {
		if v, ok, err := optionalNonNegativeIntArg(args, key); err != nil {
			return WritePreview{}, err
		} else if ok {
			body[key] = v
		}
	}
	if len(body) == 0 {
		return WritePreview{}, fmt.Errorf("at least one budget field is required")
	}
	return WritePreview{Operation: WriteOpBudgetUpdate, Summary: "Update budget settings", Target: "governor budget", Request: WriteRequest{Method: http.MethodPut, Path: "/api/config/governor/budget", Body: body}, Effects: []string{"Changes the token budget gate and alert threshold used by the governor."}, WideningDisclosure: "May widen or narrow agent activity depending on whether the budget limit is raised, lowered, or disabled.", ConfirmationMessage: "Confirm updating budget settings.", Details: body}, nil
}

func (budgetResetOp) Name() string                { return WriteOpBudgetReset }
func (budgetResetOp) Description() string         { return "Reset the active budget window." }
func (budgetResetOp) InputSchema() map[string]any { return emptySchema() }
func (budgetResetOp) Preview(context.Context, map[string]any) (WritePreview, error) {
	return WritePreview{Operation: WriteOpBudgetReset, Summary: "Reset budget window", Target: "governor budget", Request: WriteRequest{Method: http.MethodPost, Path: "/api/config/governor/budget/reset"}, Effects: []string{"Spend re-anchors at zero for the current budget window and budget alerts re-arm."}, WideningDisclosure: "Widening: if the budget gate had stopped work, resetting the window can allow kicks and agent activity to resume immediately.", ConfirmationMessage: "Confirm resetting the budget window."}, nil
}

func (budgetIgnoreOp) Name() string        { return WriteOpBudgetIgnore }
func (budgetIgnoreOp) Description() string { return "Set global or per-agent budget bypasses." }
func (budgetIgnoreOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"ignored": map[string]any{"type": "boolean"}, "agents": map[string]any{"type": "array", "items": stringSchema()}}, "additionalProperties": false}
}
func (budgetIgnoreOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	body := map[string]any{}
	target := ""
	if ignored, ok := boolArg(args, "ignored"); ok {
		body["ignored"] = ignored
		if ignored {
			target = "global budget bypass on"
		} else {
			target = "global budget bypass off"
		}
	} else if agents, ok, err := stringSliceArg(args, "agents"); err != nil {
		return WritePreview{}, err
	} else if ok {
		body["ignored"] = agents
		target = "per-agent budget bypass"
	} else {
		return WritePreview{}, fmt.Errorf("ignored or agents is required")
	}
	return WritePreview{Operation: WriteOpBudgetIgnore, Summary: "Update " + target, Target: target, Request: WriteRequest{Method: http.MethodPost, Path: "/api/budget-ignore", Body: body}, Effects: []string{"Changes which budget checks the governor ignores."}, WideningDisclosure: "May widen agent activity by exempting all agents or named agents from the budget gate.", ConfirmationMessage: "Confirm updating budget bypasses.", Details: body}, nil
}

func (contributorTrustOp) Name() string        { return WriteOpContributorTrust }
func (contributorTrustOp) Description() string { return "Set a contributor trust tier." }
func (contributorTrustOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"contributor_id": stringSchema(), "tier": stringSchema()}, "required": []string{"contributor_id", "tier"}, "additionalProperties": false}
}
func (contributorTrustOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	tier := stringArg(args, "tier")
	if id == "" || tier == "" {
		return WritePreview{}, fmt.Errorf("contributor_id and tier are required")
	}
	body := map[string]any{"tier": tier}
	return contributorPreview(WriteOpContributorTrust, "Set contributor "+id+" trust tier to "+tier, id, http.MethodPut, "/api/contributors/"+url.PathEscape(id)+"/trust", body, []string{"Changes the contributor's trust tier; revoked disconnects live contributor sessions."}, "May widen or narrow contributor authority depending on the chosen trust tier.", "Confirm changing contributor "+id+" trust tier.")
}

func (contributorAgentRoleOp) Name() string        { return WriteOpContributorAgentRole }
func (contributorAgentRoleOp) Description() string { return "Assign a contributor agent role." }
func (contributorAgentRoleOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"contributor_id": stringSchema(), "agent_role": stringSchema()}, "required": []string{"contributor_id", "agent_role"}, "additionalProperties": false}
}
func (contributorAgentRoleOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	role := stringArg(args, "agent_role")
	if id == "" || role == "" {
		return WritePreview{}, fmt.Errorf("contributor_id and agent_role are required")
	}
	body := map[string]any{"agent_role": role}
	return contributorPreview(WriteOpContributorAgentRole, "Assign contributor "+id+" agent role "+role, id, http.MethodPut, "/api/contributors/"+url.PathEscape(id)+"/agent-role", body, []string{"Changes which contributor-agent role the contributor claims by default."}, "May widen or narrow delegated contributor capabilities depending on the role.", "Confirm changing contributor "+id+" agent role.")
}

func (contributorAgentRoleGrantsOp) Name() string { return WriteOpContributorRoleGrants }
func (contributorAgentRoleGrantsOp) Description() string {
	return "Set privileged contributor agent-role grants."
}
func (contributorAgentRoleGrantsOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"contributor_id": stringSchema(), "agent_role_grants": map[string]any{"type": "array", "items": stringSchema()}}, "required": []string{"contributor_id", "agent_role_grants"}, "additionalProperties": false}
}
func (contributorAgentRoleGrantsOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	grants, ok, err := stringSliceArg(args, "agent_role_grants")
	if err != nil {
		return WritePreview{}, err
	}
	if id == "" || !ok {
		return WritePreview{}, fmt.Errorf("contributor_id and agent_role_grants are required")
	}
	body := map[string]any{"agent_role_grants": grants}
	return contributorPreview(WriteOpContributorRoleGrants, "Set contributor "+id+" agent-role grants", id, http.MethodPut, "/api/contributors/"+url.PathEscape(id)+"/agent-role-grants", body, []string{"Replaces the contributor's privileged delegated role grants."}, "May widen or narrow delegated contributor capabilities depending on the grants.", "Confirm changing contributor "+id+" role grants.")
}

func (contributorRevokeOp) Name() string                { return WriteOpContributorRevoke }
func (contributorRevokeOp) Description() string         { return "Revoke a contributor." }
func (contributorRevokeOp) InputSchema() map[string]any { return contributorIDSchema() }
func (contributorRevokeOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	if id == "" {
		return WritePreview{}, fmt.Errorf("contributor_id is required")
	}
	return contributorPreview(WriteOpContributorRevoke, "Revoke contributor "+id, id, http.MethodPost, "/api/contributors/"+url.PathEscape(id)+"/revoke", nil, []string{"Sets the contributor trust tier to revoked and disconnects live sessions."}, "Narrowing: this removes contributor access rather than widening it.", "Confirm revoking contributor "+id+".")
}

func (contributorRequeueOp) Name() string { return WriteOpContributorRequeue }
func (contributorRequeueOp) Description() string {
	return "Release and reassign a contributor's in-flight task."
}
func (contributorRequeueOp) InputSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"contributor_id": stringSchema(), "reason": map[string]any{"type": "string"}}, "required": []string{"contributor_id"}, "additionalProperties": false}
}
func (contributorRequeueOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	if id == "" {
		return WritePreview{}, fmt.Errorf("contributor_id is required")
	}
	var body any
	if reason := stringArg(args, "reason"); reason != "" {
		body = map[string]any{"reason": reason}
	}
	return contributorPreview(WriteOpContributorRequeue, "Requeue contributor "+id, id, http.MethodPost, "/api/contributors/"+url.PathEscape(id)+"/requeue", body, []string{"Releases the contributor's current task and may assign different eligible work."}, "May widen work movement by returning one item to the ready queue and assigning another.", "Confirm requeueing contributor "+id+".")
}

func (contributorDeleteOp) Name() string                { return WriteOpContributorDelete }
func (contributorDeleteOp) Description() string         { return "Delete a contributor profile." }
func (contributorDeleteOp) InputSchema() map[string]any { return contributorIDSchema() }
func (contributorDeleteOp) Preview(_ context.Context, args map[string]any) (WritePreview, error) {
	id := stringArg(args, "contributor_id")
	if id == "" {
		return WritePreview{}, fmt.Errorf("contributor_id is required")
	}
	return contributorPreview(WriteOpContributorDelete, "Delete contributor "+id, id, http.MethodDelete, "/api/contributors/"+url.PathEscape(id), nil, []string{"Permanently removes the contributor profile from this hive."}, "Narrowing: this removes contributor access rather than widening it.", "Confirm deleting contributor "+id+".")
}

func (backupCreateOp) Name() string { return WriteOpBackupCreate }
func (backupCreateOp) Description() string {
	return "Create an encrypted hive backup through POST /api/backup."
}
func (backupCreateOp) InputSchema() map[string]any { return emptySchema() }
func (backupCreateOp) Preview(context.Context, map[string]any) (WritePreview, error) {
	return WritePreview{Operation: WriteOpBackupCreate, Summary: "Create encrypted hive backup", Target: "hive backup", Request: WriteRequest{Method: http.MethodPost, Path: "/api/backup"}, Effects: []string{"Builds an encrypted backup if the hive has a backup key configured."}, WideningDisclosure: "No fleet-authority widening. The admin MCP result must report backup metadata only and never expose archive bytes.", ConfirmationMessage: "Confirm creating an encrypted hive backup."}, nil
}

func (circuitBreakerEngageOp) Name() string        { return WriteOpCircuitBreakerEngage }
func (circuitBreakerEngageOp) Description() string { return "Engage the fleet circuit breaker." }
func (circuitBreakerEngageOp) InputSchema() map[string]any {
	return emptySchema()
}
func (circuitBreakerEngageOp) Preview(context.Context, map[string]any) (WritePreview, error) {
	return WritePreview{Operation: WriteOpCircuitBreakerEngage, Summary: "Engage fleet circuit breaker", Target: "fleet breaker", Request: WriteRequest{Method: http.MethodPost, Path: "/api/breaker/engage"}, Effects: []string{"Pauses every running non-on-demand agent that the breaker owns."}, WideningDisclosure: "Narrowing: engaging the breaker halts fleet activity rather than widening it.", ConfirmationMessage: "Confirm engaging the fleet circuit breaker."}, nil
}

func (circuitBreakerReleaseOp) Name() string        { return WriteOpCircuitBreakerRelease }
func (circuitBreakerReleaseOp) Description() string { return "Release the fleet circuit breaker." }
func (circuitBreakerReleaseOp) InputSchema() map[string]any {
	return emptySchema()
}
func (circuitBreakerReleaseOp) Preview(context.Context, map[string]any) (WritePreview, error) {
	return WritePreview{Operation: WriteOpCircuitBreakerRelease, Summary: "Release fleet circuit breaker", Target: "fleet breaker", Request: WriteRequest{Method: http.MethodPost, Path: "/api/breaker/release"}, Effects: []string{"Resumes only the agents the breaker paused and still owns."}, WideningDisclosure: "Widening: releasing the breaker can resume multiple agents and allow fleet work to continue.", ConfirmationMessage: "Confirm releasing the fleet circuit breaker."}, nil
}

func contributorPreview(operation, summary, target, method, path string, body any, effects []string, widening, confirm string) (WritePreview, error) {
	return WritePreview{Operation: operation, Summary: summary, Target: target, Request: WriteRequest{Method: method, Path: path, Body: body}, Effects: effects, WideningDisclosure: widening, ConfirmationMessage: confirm, Details: map[string]any{"contributor_id": target}}, nil
}

func repoPauseSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"repo": stringSchema(), "reason": map[string]any{"type": "string"}}, "required": []string{"repo"}, "additionalProperties": false}
}

func contributorIDSchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{"contributor_id": stringSchema()}, "required": []string{"contributor_id"}, "additionalProperties": false}
}

func emptySchema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}, "additionalProperties": false}
}

func stringSchema() map[string]any {
	return map[string]any{"type": "string", "minLength": 1}
}

func boolArg(args map[string]any, key string) (bool, bool) {
	v, ok := args[key]
	if !ok {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func positiveIntArg(args map[string]any, key string) (int, error) {
	n, ok, err := optionalNonNegativeIntArg(args, key)
	if err != nil {
		return 0, err
	}
	if !ok || n <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return n, nil
}

func optionalNonNegativeIntArg(args map[string]any, key string) (int, bool, error) {
	v, ok := args[key]
	if !ok {
		return 0, false, nil
	}
	switch x := v.(type) {
	case int:
		if x < 0 {
			return 0, false, fmt.Errorf("%s must be non-negative", key)
		}
		return x, true, nil
	case int64:
		if x < 0 || x > math.MaxInt {
			return 0, false, fmt.Errorf("%s is out of range", key)
		}
		return int(x), true, nil
	case float64:
		if x < 0 || math.Trunc(x) != x || x > math.MaxInt {
			return 0, false, fmt.Errorf("%s must be a non-negative integer", key)
		}
		return int(x), true, nil
	default:
		return 0, false, fmt.Errorf("%s must be a non-negative integer", key)
	}
}

func stringSliceArg(args map[string]any, key string) ([]string, bool, error) {
	v, ok := args[key]
	if !ok {
		return nil, false, nil
	}
	raw, ok := v.([]any)
	if !ok {
		if typed, ok := v.([]string); ok {
			out := make([]string, 0, len(typed))
			for _, item := range typed {
				item = strings.TrimSpace(item)
				if item != "" {
					out = append(out, item)
				}
			}
			return out, true, nil
		}
		return nil, false, fmt.Errorf("%s must be a list of strings", key)
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, false, fmt.Errorf("%s must be a list of strings", key)
		}
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return out, true, nil
}
