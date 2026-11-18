package config

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
