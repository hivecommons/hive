package config

import (
	"fmt"
	"sort"
	"strings"
)

// DocSourceConfigYAML describes an external document to import as knowledge.
type DocSourceConfigYAML struct {
	Name     string `yaml:"name"`
	URL      string `yaml:"url,omitempty"`
	FilePath string `yaml:"file_path,omitempty"`
	Layer    string `yaml:"layer"`
}

// KnowledgeContext7Import is one library imported from Context7 at startup.
type KnowledgeContext7Import struct {
	ID    string `yaml:"id"`
	Query string `yaml:"query,omitempty"`
}

// KnowledgeContext7 holds the legacy Context7 auto-import list.
type KnowledgeContext7 struct {
	AutoImport []KnowledgeContext7Import `yaml:"auto_import,omitempty"`
}

type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"`
	Engine          string                `yaml:"engine"`
	Layers          []KnowledgeLayer      `yaml:"layers"`
	Vaults          []VaultConfig         `yaml:"vaults"`
	GitSources      []GitSourceConfigYAML `yaml:"git_sources"`
	Documents       []DocSourceConfigYAML `yaml:"documents"`
	Connectors      []KnowledgeConnector  `yaml:"connectors,omitempty"`
	Context7        KnowledgeContext7     `yaml:"context7,omitempty"`
	Publish         KnowledgePublish      `yaml:"publish,omitempty"`
	Public          PublicKnowledgeConfig `yaml:"public,omitempty"`
	Curator         KnowledgeCurator      `yaml:"curator"`
	Primer          KnowledgePrimer       `yaml:"primer"`
	BeadSynthesizer BeadSynthesizerConfig `yaml:"bead_synthesizer"`
	CodeMaps        KnowledgeCodeMaps     `yaml:"code_maps,omitempty"`
	// AgentScopes binds an agent (by name) to the knowledge it may see in its
	// kick primer and through agent-identified TOC/entry reads. A missing
	// entry or an empty field is unrestricted.
	AgentScopes map[string]KnowledgeAgentScope `yaml:"agent_scopes,omitempty"`
}

// PublicKnowledgeConfig is the dashboard-persisted owner override for the
// anonymous read-only MCP endpoint. Enabled is a pointer so an absent setting
// can keep honoring the legacy HIVE_PUBLIC_KNOWLEDGE environment switch.
type PublicKnowledgeConfig struct {
	Enabled *bool    `yaml:"enabled,omitempty"`
	Tags    []string `yaml:"tags,omitempty"`
}

// BeadSynthesizerConfig controls automatic synthesis of completed beads into wiki facts.
// Enabled defaults to true when knowledge is enabled; set to false to opt out.
type BeadSynthesizerConfig struct {
	Enabled          *bool                  `yaml:"enabled,omitempty"`
	Schedule         string                 `yaml:"schedule"`
	MinConfidence    float64                `yaml:"min_confidence"`
	TargetLayer      string                 `yaml:"target_layer"`
	MaxFactsPerCycle int                    `yaml:"max_facts_per_cycle"`
	VaultPath        string                 `yaml:"vault_path"`
	RetentionPolicy  *RetentionPolicy       `yaml:"retention_policy"`
	Publish          BeadSynthPublishConfig `yaml:"publish,omitempty"`
	// MaxAPICallsPerCycle caps charged GitHub PR GETs per synthesis cycle.
	// 0/unset defaults to 40; HIVE_BEAD_SYNTH_MAX_API_CALLS still overrides.
	MaxAPICallsPerCycle int `yaml:"max_api_calls_per_cycle"`
}

// BeadSynthPublishConfig stores the owner's intended repository publication
// settings. It is separate from connector publishing (KnowledgeConfig.Publish).
// This block alone does not enable publishing; policy and sensitivity gates
// must be enforced by a future publisher, including for mode=commit.
// Empty Repo selects the hive's primary repository; empty BaseBranch selects
// that repository's default branch.
type BeadSynthPublishConfig struct {
	Enabled    bool   `yaml:"enabled" json:"enabled"`
	Repo       string `yaml:"repo" json:"repo"`
	Path       string `yaml:"path" json:"path"`
	BaseBranch string `yaml:"base_branch" json:"base_branch"`
	Mode       string `yaml:"mode" json:"mode"`
	Cadence    string `yaml:"cadence" json:"cadence"`
}

// WithDefaults returns settings with cleared fields restored to safe defaults.
func (p BeadSynthPublishConfig) WithDefaults() BeadSynthPublishConfig {
	if p.Path == "" {
		p.Path = DefaultBeadSynthPublishPath
	}
	if p.Mode == "" {
		p.Mode = DefaultBeadSynthPublishMode
	}
	if p.Cadence == "" {
		p.Cadence = DefaultBeadSynthPublishCadence
	}
	return p
}

// DefaultBeadSynthMaxAPICallsPerCycle is the default per-cycle PR GET cap.
const DefaultBeadSynthMaxAPICallsPerCycle = 40

// Knowledge pipeline defaults that applyDefaults sets at Load; runtime editors
// substitute the same values so a cleared field restores the default.
const (
	DefaultBeadSynthPublishPath         = "docs/knowledge/"
	DefaultBeadSynthPublishMode         = "pr"
	DefaultBeadSynthPublishCadence      = "per_run"
	DefaultBeadSynthSchedule            = "hourly"
	DefaultBeadSynthMinConfidence       = 0.5
	DefaultBeadSynthTargetLayer         = "project"
	DefaultBeadSynthMaxFactsPerCycle    = 20
	DefaultBeadSynthVaultPath           = "/data/vaults/bead-synth-wiki"
	DefaultCuratorSchedule              = "daily"
	DefaultCuratorAutoPromoteThreshold  = 0.9
	DefaultKnowledgePrimerMaxFacts      = 25
	DefaultKnowledgePrimerMaxBytes      = 4096
	DefaultKnowledgePrimerMergeStrategy = "precedence"
)

// KnowledgeSchedules are the schedule values the bead synthesizer, curator
// and code map loops understand (knowledge.ParseSynthSchedule).
var KnowledgeSchedules = []string{"hourly", "daily"}

// ValidateKnowledgePipeline checks the scalar bead_synthesizer, curator,
// code_maps and primer keys the dashboard edits: schedules must be hourly or
// daily (empty uses the default), layers must be personal, project, org or
// community, confidences lie in [0, 1], counts and byte caps are not
// negative, and scheduled promotion may not promote a layer into itself.
// Bead-synth publish paths must be relative without parent components; modes
// are pr or commit and cadences are per_run or daily.
// Unset (zero) fields pass, so callers can validate just the keys they edit.
func ValidateKnowledgePipeline(k KnowledgeConfig) error {
	p := k.BeadSynthesizer.Publish
	if p.Mode != "" && p.Mode != "pr" && p.Mode != "commit" {
		return fmt.Errorf("knowledge.bead_synthesizer.publish.mode must be pr or commit")
	}
	if p.Cadence != "" && p.Cadence != "per_run" && p.Cadence != "daily" {
		return fmt.Errorf("knowledge.bead_synthesizer.publish.cadence must be per_run or daily")
	}
	// Repository paths use forward slashes. Reject Windows separators and
	// drive paths too, so validation is independent of the host platform.
	if strings.HasPrefix(p.Path, "/") || strings.ContainsAny(p.Path, "\\:\x00") {
		return fmt.Errorf("knowledge.bead_synthesizer.publish.path must be relative with no .. components")
	}
	for _, part := range strings.Split(p.Path, "/") {
		if part == ".." {
			return fmt.Errorf("knowledge.bead_synthesizer.publish.path must be relative with no .. components")
		}
	}
	for _, s := range []struct{ field, v string }{
		{"knowledge.bead_synthesizer.schedule", k.BeadSynthesizer.Schedule},
		{"knowledge.curator.schedule", k.Curator.Schedule},
		{"knowledge.code_maps.schedule", k.CodeMaps.Schedule},
	} {
		v := strings.ToLower(strings.TrimSpace(s.v))
		if v != "" && v != "hourly" && v != "daily" {
			return fmt.Errorf("%s %q must be hourly or daily", s.field, s.v)
		}
	}
	for _, l := range []struct{ field, v string }{
		{"knowledge.bead_synthesizer.target_layer", k.BeadSynthesizer.TargetLayer},
		{"knowledge.curator.promote_from", k.Curator.PromoteFrom},
		{"knowledge.curator.promote_to", k.Curator.PromoteTo},
		{"knowledge.code_maps.layer", k.CodeMaps.Layer},
	} {
		if v := strings.TrimSpace(l.v); v != "" && !knowledgeLayerNames[v] {
			return fmt.Errorf("%s %q must be personal, project, org or community", l.field, l.v)
		}
	}
	from, to := strings.TrimSpace(k.Curator.PromoteFrom), strings.TrimSpace(k.Curator.PromoteTo)
	if from == "" {
		from = "project"
	}
	if to == "" {
		to = "org"
	}
	if from == to {
		return fmt.Errorf("knowledge.curator.promote_from and promote_to must name different layers (both %q)", from)
	}
	for _, f := range []struct {
		field string
		v     float64
	}{
		{"knowledge.bead_synthesizer.min_confidence", k.BeadSynthesizer.MinConfidence},
		{"knowledge.curator.auto_promote_threshold", k.Curator.AutoPromoteThreshold},
	} {
		if f.v < 0 || f.v > 1 {
			return fmt.Errorf("%s must be between 0 and 1", f.field)
		}
	}
	type intField struct {
		field string
		v     int
	}
	ints := []intField{
		{"knowledge.bead_synthesizer.max_facts_per_cycle", k.BeadSynthesizer.MaxFactsPerCycle},
		{"knowledge.bead_synthesizer.max_api_calls_per_cycle", k.BeadSynthesizer.MaxAPICallsPerCycle},
		{"knowledge.code_maps.max_bytes", k.CodeMaps.MaxBytes},
		{"knowledge.primer.max_facts", k.Primer.MaxFacts},
		{"knowledge.primer.max_bytes", k.Primer.MaxBytes},
	}
	if rp := k.BeadSynthesizer.RetentionPolicy; rp != nil {
		ints = append(ints,
			intField{"knowledge.bead_synthesizer.retention_policy.max_beads", rp.MaxBeads},
			intField{"knowledge.bead_synthesizer.retention_policy.archive_after_synth_days", rp.ArchiveAfterSynthDays},
			intField{"knowledge.bead_synthesizer.retention_policy.high_priority_retain_days", rp.HighPriorityRetainDays},
		)
	}
	for _, f := range ints {
		if f.v < 0 {
			return fmt.Errorf("%s must be 0 (default) or greater", f.field)
		}
	}
	return nil
}

// EffectiveMaxAPICallsPerCycle returns the configured cap or the default.
func (b BeadSynthesizerConfig) EffectiveMaxAPICallsPerCycle() int {
	if b.MaxAPICallsPerCycle <= 0 {
		return DefaultBeadSynthMaxAPICallsPerCycle
	}
	return b.MaxAPICallsPerCycle
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
	// MaxBytes caps the rendered knowledge primer per kick; facts past it are
	// dropped whole and a marker says so. Zero takes the knowledge package
	// default (4096).
	MaxBytes int `yaml:"max_bytes"`
}

// KnowledgeAgentScope is one `knowledge.agent_scopes` entry. Each field is an
// allow-list; empty means unrestricted. The scope is intersected with any
// request scope and can never widen it. IncludeStates lists the lifecycle
// states the agent may be shown in addition to approved; empty defers to the
// request (or primer) default, and a listed state is admitted only when the
// request includes it too.
type KnowledgeAgentScope struct {
	Layers        []string `yaml:"layers,omitempty" json:"layers,omitempty"`
	Repos         []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	Types         []string `yaml:"types,omitempty" json:"types,omitempty"`
	Tags          []string `yaml:"tags,omitempty" json:"tags,omitempty"`
	IncludeStates []string `yaml:"include_states,omitempty" json:"include_states,omitempty"`
}

var knowledgeLifecycleStateNames = map[string]bool{"draft": true, "approved": true, "deprecated": true, "superseded": true, "all": true}

// ValidateKnowledgeAgentScopes checks every `knowledge.agent_scopes` entry:
// agent names must be non-empty, layers must be personal, project, org or
// community, and include_states must be draft, approved, deprecated,
// superseded or all.
func ValidateKnowledgeAgentScopes(scopes map[string]KnowledgeAgentScope) error {
	names := make([]string, 0, len(scopes))
	for name := range scopes {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("knowledge.agent_scopes: agent name is required")
		}
		sc := scopes[name]
		field := "knowledge.agent_scopes." + name
		for i, l := range sc.Layers {
			if !knowledgeLayerNames[strings.ToLower(strings.TrimSpace(l))] {
				return fmt.Errorf("%s.layers[%d] %q must be personal, project, org or community", field, i, l)
			}
		}
		for i, st := range sc.IncludeStates {
			if !knowledgeLifecycleStateNames[strings.ToLower(strings.TrimSpace(st))] {
				return fmt.Errorf("%s.include_states[%d] %q must be draft, approved, deprecated, superseded or all", field, i, st)
			}
		}
	}
	return nil
}

// KnowledgeAgentScope returns the knowledge scope bound to agent. A replica
// without its own entry inherits its base agent's scope. ok is false when the
// agent is unrestricted.
func (c *Config) KnowledgeAgentScope(agent string) (KnowledgeAgentScope, bool) {
	if c == nil || len(c.Knowledge.AgentScopes) == 0 || agent == "" {
		return KnowledgeAgentScope{}, false
	}
	if sc, ok := c.Knowledge.AgentScopes[agent]; ok {
		return sc, true
	}
	sc, ok := c.Knowledge.AgentScopes[c.BaseAgentName(agent)]
	return sc, ok
}
