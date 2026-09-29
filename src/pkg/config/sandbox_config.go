package config

// AgentSandboxConfig controls the sandbox kick launcher. The top-level block
// is a global gate; per-agent config can opt specific agents in.
//
// Runtime selects WHAT runs a sandboxed kick: "podman" (the default, a
// rootless container on the hive pod's own node) or "job" (a Kubernetes Job
// in the hive's namespace, #6311). The Job runtime is how an agent's kicks
// reach a different image, a node with accelerator hardware, and operator
// secrets by reference; Job holds its settings.
type AgentSandboxConfig struct {
	Enabled      bool              `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string            `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string          `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string            `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int               `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	WorkspaceDir string            `yaml:"workspace_dir,omitempty" json:"workspace_dir,omitempty"`
	Runtime      string            `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Job          *SandboxJobConfig `yaml:"job,omitempty" json:"job,omitempty"`
}

// AgentSandboxOverride is the per-agent sandbox opt-in block.
type AgentSandboxOverride struct {
	Enabled      *bool             `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Image        string            `yaml:"image,omitempty" json:"image,omitempty"`
	EnvAllowlist []string          `yaml:"env_allowlist,omitempty" json:"env_allowlist,omitempty"`
	NetworkMode  string            `yaml:"network_mode,omitempty" json:"network_mode,omitempty"`
	TimeoutS     int               `yaml:"timeout_s,omitempty" json:"timeout_s,omitempty"`
	Runtime      string            `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	Job          *SandboxJobConfig `yaml:"job,omitempty" json:"job,omitempty"`
}
