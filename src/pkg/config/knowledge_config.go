package config

// DocSourceConfigYAML describes an external document to import as knowledge.
type DocSourceConfigYAML struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url,omitempty"`
	FilePath string `yaml:"file_path,omitempty"`
	Layer    string `yaml:"layer"`
}

// PublicKnowledgeConfig overrides the environment when explicitly configured.
type PublicKnowledgeConfig struct {
	Enabled bool     `yaml:"enabled" json:"enabled"`
	Tags    []string `yaml:"tags" json:"tags"`
}

type KnowledgeConfig struct {
	Public          *PublicKnowledgeConfig `yaml:"public,omitempty"`
	Enabled         bool                   `yaml:"enabled"`
	Engine          string                 `yaml:"engine"`
	Layers          []KnowledgeLayer       `yaml:"layers"`
	Vaults          []VaultConfig          `yaml:"vaults"`
	GitSources      []GitSourceConfigYAML  `yaml:"git_sources"`
	Documents       []DocSourceConfigYAML  `yaml:"documents"`
	Curator         KnowledgeCurator       `yaml:"curator"`
	Primer          KnowledgePrimer        `yaml:"primer"`
	BeadSynthesizer BeadSynthesizerConfig  `yaml:"bead_synthesizer"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool            `yaml:"enabled,omitempty"`
	Schedule         string           `yaml:"schedule"`
	MinConfidence    float64          `yaml:"min_confidence"`
	TargetLayer      string           `yaml:"target_layer"`
	MaxFactsPerCycle int              `yaml:"max_facts_per_cycle"`
	VaultPath        string           `yaml:"vault_path"`
	RetentionPolicy  *RetentionPolicy `yaml:"retention_policy"`
}

// RetentionPolicy controls intelligent bead lifecycle management.
type RetentionPolicy struct {
	MaxBeads               int  `yaml:"max_beads"`
	ArchiveAfterSynthDays  int  `yaml:"archive_after_synth_days"`
	HighPriorityRetainDays int  `yaml:"high_priority_retain_days"`
	PreserveWithDeps       bool `yaml:"preserve_with_deps"`
}

// IsEnabled returns whether bead synthesis is enabled (defaults to true).
func (b BeadSynthesizerConfig) IsEnabled() bool {
	if b.Enabled == nil {
		return true
	}
	return *b.Enabled
}

// GitSourceConfigYAML describes a remote git repo (or subdirectory) to index
// as a knowledge source. Any layer level can have git sources.
type GitSourceConfigYAML struct {
	Name    string `yaml:"name"`
	URL     string `yaml:"url"`
	Branch  string `yaml:"branch,omitempty"`
	Subpath string `yaml:"subpath,omitempty"`
	Layer   string `yaml:"layer"`
}

// VaultConfig describes a file-based Obsidian vault to auto-connect on startup.
type VaultConfig struct {
	Name      string `yaml:"name"`
	Path      string `yaml:"path"`
	AutoIndex bool   `yaml:"auto_index"`
	GitSync   bool   `yaml:"git_sync"`
}

type KnowledgeLayer struct {
	Type   string `yaml:"type"`
	Path   string `yaml:"path,omitempty"`
	URL    string `yaml:"url,omitempty"`
	Shared bool   `yaml:"shared"`
}

type KnowledgeCurator struct {
	// Enabled gates the scheduled auto-promotion loop. It is a pointer so an
	// absent key is distinguishable from an explicit `enabled: false`, and it
	// defaults to FALSE — unlike BeadSynthesizer, which defaults to true.
	//
	// The asymmetry is deliberate. Auto-promotion copies facts into a
	// higher-precedence knowledge layer with no human review, and `schedule`
	// has been parsed-but-unactioned since it was introduced (#5430), so every
	// existing hive that set it did so without ever having the loop run. If
	// the loop defaulted on, upgrading would silently begin mutating the org
	// layer on hives that never opted in. Scheduled promotion is therefore
	// opt-in: `schedule` alone does NOT start it.
	Enabled              *bool    `yaml:"enabled,omitempty"`
	Schedule             string   `yaml:"schedule"`
	ExtractFrom          []string `yaml:"extract_from"`
	AutoPromoteThreshold float64  `yaml:"auto_promote_threshold"`
	// PromoteFrom / PromoteTo name the source and target layers for the
	// scheduled promotion sweep. Empty values fall back to project→org.
	PromoteFrom string `yaml:"promote_from,omitempty"`
	PromoteTo   string `yaml:"promote_to,omitempty"`
}

// IsEnabled reports whether scheduled auto-promotion is active. Absent (nil)
// means DISABLED — see the Enabled field comment for why this defaults false.
func (k KnowledgeCurator) IsEnabled() bool {
	return k.Enabled != nil && *k.Enabled
}

type KnowledgePrimer struct {
	MaxFacts      int      `yaml:"max_facts"`
	Priority      []string `yaml:"priority"`
	MergeStrategy string   `yaml:"merge_strategy"`
}
