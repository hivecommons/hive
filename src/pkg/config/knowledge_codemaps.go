package config

// KnowledgeCodeMaps configures generated repository code maps (#11106): a
// deterministic summary of each listed repository written into a dedicated
// vault as repo-scoped knowledge and regenerated when the repository's HEAD
// moves. Generation is opt-in; absent or `enabled: false` does nothing.
type KnowledgeCodeMaps struct {
	Enabled *bool `yaml:"enabled,omitempty"`
	// Schedule is how often HEAD is checked: hourly (default) or daily.
	Schedule string `yaml:"schedule,omitempty"`
	// VaultPath is where maps are written; defaults to /data/vaults/code-maps.
	VaultPath string `yaml:"vault_path,omitempty"`
	// Layer is the knowledge layer recorded on each map (default project).
	Layer string `yaml:"layer,omitempty"`
	// MaxBytes caps the rendered size of each map (default 16384).
	MaxBytes int                    `yaml:"max_bytes,omitempty"`
	Repos    []KnowledgeCodeMapRepo `yaml:"repos,omitempty"`
}

// KnowledgeCodeMapRepo is one repository to map: a local checkout (path) or
// an https git URL cloned with the same hardening as knowledge.git_sources.
type KnowledgeCodeMapRepo struct {
	Name   string `yaml:"name,omitempty"`
	Path   string `yaml:"path,omitempty"`
	URL    string `yaml:"url,omitempty"`
	Branch string `yaml:"branch,omitempty"`
}

// IsEnabled reports whether code map generation is active (default false).
func (k KnowledgeCodeMaps) IsEnabled() bool {
	return k.Enabled != nil && *k.Enabled
}
