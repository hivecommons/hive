package config

type DeploymentConfig struct {
	Runtime       string `yaml:"runtime,omitempty" json:"runtime,omitempty"`
	PodmanMode    string `yaml:"podman_mode,omitempty" json:"podman_mode,omitempty"`
	UpgradeHelper string `yaml:"upgrade_helper,omitempty" json:"upgrade_helper,omitempty"`
}
