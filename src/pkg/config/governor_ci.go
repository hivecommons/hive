package config

// GovernorCIConfig controls approval of GitHub's gated workflow runs. Trust
// permits starting CI only; every existing review and merge gate still applies.
type GovernorCIConfig struct {
	AutoApproveWorkflows      *bool     `yaml:"auto_approve_workflows,omitempty" json:"auto_approve_workflows,omitempty"`
	AutoApproveWorkflowActors *[]string `yaml:"auto_approve_workflow_actors,omitempty" json:"auto_approve_workflow_actors,omitempty"`
}

func (c GovernorCIConfig) AutoApproveWorkflowsEnabled() bool {
	return c.AutoApproveWorkflows == nil || *c.AutoApproveWorkflows
}

// WorkflowApprovalActors distinguishes an omitted list (trusted bot defaults)
// from an explicit empty list (only the App and owner admission are trusted).
func (c GovernorCIConfig) WorkflowApprovalActors() []string {
	if c.AutoApproveWorkflowActors == nil {
		return []string{"dependabot[bot]", "renovate[bot]"}
	}
	return *c.AutoApproveWorkflowActors
}
