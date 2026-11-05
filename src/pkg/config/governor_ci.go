package config

// GovernorCIConfig is governor.ci: approval of workflow runs GitHub holds
// behind its "approve and run workflows" gate (hivecommons/hive#16603).
// Approval starts CI only; every review, hold and merge gate still applies.
type GovernorCIConfig struct {
	// AutoApproveWorkflows (default true) lets the hive approve gated runs
	// on trusted PR heads; false leaves the gate to a person.
	AutoApproveWorkflows *bool `yaml:"auto_approve_workflows,omitempty" json:"auto_approve_workflows,omitempty"`
	// AutoApproveWorkflowActors are the PR authors / triggering actors
	// trusted besides the hive's App and heads a person admitted from the
	// dashboard (stage-record admission, head-bound). Unset means
	// DefaultAutoApproveWorkflowActors; an explicit [] trusts neither.
	AutoApproveWorkflowActors *[]string `yaml:"auto_approve_workflow_actors,omitempty" json:"auto_approve_workflow_actors,omitempty"`
}

// DefaultAutoApproveWorkflowActors is governor.ci.auto_approve_workflow_actors
// when unset.
var DefaultAutoApproveWorkflowActors = []string{"dependabot[bot]", "renovate[bot]"}

// AutoApproveWorkflowsEnabled is the effective auto_approve_workflows.
func (c GovernorCIConfig) AutoApproveWorkflowsEnabled() bool {
	return c.AutoApproveWorkflows == nil || *c.AutoApproveWorkflows
}

// WorkflowApprovalActors is the effective auto_approve_workflow_actors.
func (c GovernorCIConfig) WorkflowApprovalActors() []string {
	if c.AutoApproveWorkflowActors == nil {
		return append([]string(nil), DefaultAutoApproveWorkflowActors...)
	}
	return *c.AutoApproveWorkflowActors
}
