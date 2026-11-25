package config

import (
	"fmt"
	"regexp"
	"strings"
)

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
// different node than hive, the claim has to be ReadWriteMany. With
// WorkspaceTransport "copy" the Job mounts no claim: its workspace is a
// pod-local emptyDir that hive fills and pulls back (docs/sandbox-isolation.md),
// and WorkspaceClaim is not needed.
//
// EnvFromSecrets is the ONLY credential path into the Job. Hive's brokered
// GitHub token never enters it; hive pushes and opens the PR from the
// workspace after the Job exits, exactly as it does for the Podman runtime.
type SandboxJobConfig struct {
	WorkspaceClaim      string                 `yaml:"workspace_claim,omitempty" json:"workspace_claim,omitempty"`
	WorkspaceClaimMount string                 `yaml:"workspace_claim_mount,omitempty" json:"workspace_claim_mount,omitempty"`
	NodeSelector        map[string]string      `yaml:"node_selector,omitempty" json:"node_selector,omitempty"`
	Affinity            *SandboxJobAffinity    `yaml:"affinity,omitempty" json:"affinity,omitempty"`
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
	// ScheduleTimeoutS fails a kick whose pod stays Unschedulable this long
	// (reason waiting_for_node, #18811). Zero uses DefaultSandboxScheduleTimeoutS.
	ScheduleTimeoutS int `yaml:"schedule_timeout_s,omitempty" json:"schedule_timeout_s,omitempty"`
	// RelayImage is the image of the init container that copies hive's
	// sandbox relays (hive-review) into the Job (#18512). Empty resolves the
	// running hive image by digest from hive's own pod; "none" skips the
	// relays entirely.
	RelayImage string `yaml:"relay_image,omitempty" json:"relay_image,omitempty"`
	// RelayRunAsUser pins the relay init container's uid. Unset leaves it to
	// the cluster on OpenShift (the SCC assigns one from the namespace range)
	// and uses 65534 elsewhere, since the hive image's default user is root.
	RelayRunAsUser   *int64   `yaml:"relay_run_as_user,omitempty" json:"relay_run_as_user,omitempty"`
	ImagePullSecrets []string `yaml:"image_pull_secrets,omitempty" json:"image_pull_secrets,omitempty"`
	// GitHubReadProxy (#18865) and InferenceProxy (#19619) are hive-wide Job
	// listeners, read from agent_sandbox.job only; a per-agent block cannot set them.
	GitHubReadProxy *SandboxGitHubReadProxy `yaml:"github_read_proxy,omitempty" json:"github_read_proxy,omitempty"`
	InferenceProxy  *SandboxInferenceProxy  `yaml:"inference_proxy,omitempty" json:"inference_proxy,omitempty"`
	// WorkspaceTransport is how the Job gets the workspace (#19621): empty
	// or "claim" mounts WorkspaceClaim at a subPath; "copy" copies the
	// workspace into a pod-local emptyDir and back, so WorkspaceClaim is not
	// needed and the Job never mounts hive's data volume.
	WorkspaceTransport string `yaml:"workspace_transport,omitempty" json:"workspace_transport,omitempty"`
	// WorkspaceSizeLimit is the sizeLimit of the copy transport's workspace
	// emptyDir, a Kubernetes quantity such as "20Gi" (default 10Gi). The node
	// counts emptyDir data as ephemeral storage; pair it with an
	// ephemeral-storage request and limit in resources.
	WorkspaceSizeLimit string `yaml:"workspace_size_limit,omitempty" json:"workspace_size_limit,omitempty"`
}

// Job workspace transports, SandboxJobConfig.WorkspaceTransport.
const (
	SandboxJobWorkspaceClaim = "claim"
	SandboxJobWorkspaceCopy  = "copy"
)

var sizeQuantity = regexp.MustCompile(`^[0-9]+(\.[0-9]+)?(Ki|Mi|Gi|Ti|Pi|Ei|k|M|G|T|P|E)?$`)

// ValidateSandboxJobTransport rejects a workspace_transport other than
// "claim" or "copy" (a typo such as "Copy" would otherwise read as the claim
// transport and fail only when a kick runs) and a workspace_size_limit that
// is not a quantity.
func ValidateSandboxJobTransport(job *SandboxJobConfig) error {
	if job == nil {
		return nil
	}
	switch strings.TrimSpace(job.WorkspaceTransport) {
	case "", SandboxJobWorkspaceClaim, SandboxJobWorkspaceCopy:
	default:
		return fmt.Errorf("job.workspace_transport must be %q or %q, got %q", SandboxJobWorkspaceClaim, SandboxJobWorkspaceCopy, job.WorkspaceTransport)
	}
	if l := strings.TrimSpace(job.WorkspaceSizeLimit); l != "" && !sizeQuantity.MatchString(l) {
		return fmt.Errorf("job.workspace_size_limit must be a quantity such as 20Gi, got %q", job.WorkspaceSizeLimit)
	}
	return nil
}

// CopiesWorkspace reports whether the Job uses the copy transport, which
// needs no workspace_claim.
func (j SandboxJobConfig) CopiesWorkspace() bool {
	return strings.TrimSpace(j.WorkspaceTransport) == SandboxJobWorkspaceCopy
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
// selector, affinity, tolerations, resources, secrets, volumes, service
// account) — an agent that needs an accelerator names its own scheduling. The
// claim fields and the TTL are cluster facts, not per-agent choices, so they
// fall back to the global block when the per-agent one leaves them empty.
func (a *AgentConfig) SandboxJob(global AgentSandboxConfig) SandboxJobConfig {
	var g SandboxJobConfig
	if global.Job != nil {
		g = *global.Job
	}
	if a == nil || a.Sandbox == nil || a.Sandbox.Job == nil {
		return g
	}
	out := *a.Sandbox.Job
	// The Job listeners are hive-wide: a per-agent block cannot enable them.
	out.GitHubReadProxy, out.InferenceProxy = g.GitHubReadProxy, g.InferenceProxy
	if strings.TrimSpace(out.WorkspaceClaim) == "" {
		out.WorkspaceClaim = g.WorkspaceClaim
	}
	if strings.TrimSpace(out.WorkspaceClaimMount) == "" {
		out.WorkspaceClaimMount = g.WorkspaceClaimMount
	}
	if strings.TrimSpace(out.WorkspaceTransport) == "" {
		out.WorkspaceTransport = g.WorkspaceTransport
	}
	if strings.TrimSpace(out.WorkspaceSizeLimit) == "" {
		out.WorkspaceSizeLimit = g.WorkspaceSizeLimit
	}
	if out.TTLSeconds == 0 {
		out.TTLSeconds = g.TTLSeconds
	}
	if out.ScheduleTimeoutS == 0 {
		out.ScheduleTimeoutS = g.ScheduleTimeoutS
	}
	return inheritPullSecrets(out, g)
}
