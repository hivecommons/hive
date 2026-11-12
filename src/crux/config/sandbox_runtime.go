package config

import "strings"

// Sandbox runtimes. SandboxRuntimePodman is the default when nothing is set.
const (
	SandboxRuntimePodman    = "podman"
	SandboxRuntimeJob       = "job"
	SandboxRuntimeOpenShell = "openshell"
	SandboxRuntimeSubstrate = "substrate"
)

// ValidSandboxRuntimes is the closed set a config may name.
var ValidSandboxRuntimes = map[string]bool{SandboxRuntimePodman: true, SandboxRuntimeJob: true, SandboxRuntimeOpenShell: true, SandboxRuntimeSubstrate: true}

// SandboxJobConfig is everything the Job runtime needs beyond the image
// (#6311). The pod-template fields pass through to Kubernetes verbatim.
//
// The workspace is shared between hive and the Job by mounting the SAME
// PersistentVolumeClaim in both: WorkspaceClaim names it, and
// WorkspaceClaimMount is where hive sees it (default /data, where the
// standalone and hub-provisioned deployments mount hive-data). The sandbox
// workspace root must live under that mount. When the Job lands on a
// different node than hive, the claim has to be ReadWriteMany.
//
// EnvFromSecrets is the ONLY credential path into the Job. Hive's brokered
// GitHub token never enters it; hive pushes and opens the PR from the
// workspace after the Job exits, exactly as it does for the Podman runtime.
type SandboxJobConfig struct {
	WorkspaceClaim      string                 `yaml:"workspace_claim,omitempty" json:"workspace_claim,omitempty"`
	WorkspaceClaimMount string                 `yaml:"workspace_claim_mount,omitempty" json:"workspace_claim_mount,omitempty"`
	NodeSelector        map[string]string      `yaml:"node_selector,omitempty" json:"node_selector,omitempty"`
	Tolerations         []SandboxJobToleration `yaml:"tolerations,omitempty" json:"tolerations,omitempty"`
	Resources           SandboxJobResources    `yaml:"resources,omitempty" json:"resources,omitempty"`
	ServiceAccount      string                 `yaml:"service_account,omitempty" json:"service_account,omitempty"`
	// SchedulerName sets the pod's schedulerName. Accelerator clusters often
	// gate device-requesting pods behind a custom scheduler via an admission
	// webhook (e.g. `A pod must use "schedulerName: spyre-scheduler"`);
	// without this the Job create is denied before any pod exists.
	SchedulerName  string             `yaml:"scheduler_name,omitempty" json:"scheduler_name,omitempty"`
	EnvFromSecrets []string           `yaml:"env_from_secrets,omitempty" json:"env_from_secrets,omitempty"`
	Volumes        []SandboxJobVolume `yaml:"volumes,omitempty" json:"volumes,omitempty"`
	TTLSeconds     int                `yaml:"ttl_seconds,omitempty" json:"ttl_seconds,omitempty"`
}

// SandboxJobToleration mirrors the subset of a Kubernetes toleration an
// operator writes by hand.
type SandboxJobToleration struct {
	Key      string `yaml:"key,omitempty" json:"key,omitempty"`
	Operator string `yaml:"operator,omitempty" json:"operator,omitempty"`
	Value    string `yaml:"value,omitempty" json:"value,omitempty"`
	Effect   string `yaml:"effect,omitempty" json:"effect,omitempty"`
}

// SandboxJobResources mirrors Kubernetes resource requirements with string
// quantities so a device limit (`<vendor>.com/<device>: "2"`) passes through.
type SandboxJobResources struct {
	Limits   map[string]string `yaml:"limits,omitempty" json:"limits,omitempty"`
	Requests map[string]string `yaml:"requests,omitempty" json:"requests,omitempty"`
}

// SandboxJobVolume is an additional PVC mounted into the Job (a compile
// cache, a model store).
type SandboxJobVolume struct {
	Name      string `yaml:"name,omitempty" json:"name,omitempty"`
	Claim     string `yaml:"claim" json:"claim"`
	MountPath string `yaml:"mount_path" json:"mount_path"`
	ReadOnly  bool   `yaml:"read_only,omitempty" json:"read_only,omitempty"`
}

// SandboxRuntime returns the per-agent runtime override, then the global
// runtime, then SandboxRuntimePodman. The value is returned as written; the
// manager rejects one outside ValidSandboxRuntimes at kick time.
func (a *AgentConfig) SandboxRuntime(global AgentSandboxConfig) string {
	if a != nil && a.Sandbox != nil && strings.TrimSpace(a.Sandbox.Runtime) != "" {
		return strings.TrimSpace(a.Sandbox.Runtime)
	}
	if strings.TrimSpace(global.Runtime) != "" {
		return strings.TrimSpace(global.Runtime)
	}
	return SandboxRuntimePodman
}

// SandboxJob returns the effective Job runtime settings. A per-agent job
// block replaces the global one wholesale for the pod-template fields (node
// selector, tolerations, resources, secrets, volumes, service account) — an
// agent that needs an accelerator names its own scheduling. The claim
// fields and the TTL are cluster facts rather than per-agent choices, so
// they fall back to the global block when the per-agent one leaves them
// empty.
func (a *AgentConfig) SandboxJob(global AgentSandboxConfig) SandboxJobConfig {
	var g SandboxJobConfig
	if global.Job != nil {
		g = *global.Job
	}
	if a == nil || a.Sandbox == nil || a.Sandbox.Job == nil {
		return g
	}
	out := *a.Sandbox.Job
	if strings.TrimSpace(out.WorkspaceClaim) == "" {
		out.WorkspaceClaim = g.WorkspaceClaim
	}
	if strings.TrimSpace(out.WorkspaceClaimMount) == "" {
		out.WorkspaceClaimMount = g.WorkspaceClaimMount
	}
	if out.TTLSeconds == 0 {
		out.TTLSeconds = g.TTLSeconds
	}
	return out
}
