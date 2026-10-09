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

type KnowledgeConfig struct {
	Enabled         bool                  `yaml:"enabled"`
	Engine          string                `yaml:"engine"`
	Layers          []KnowledgeLayer      `yaml:"layers"`
	Vaults          []VaultConfig         `yaml:"vaults"`
	GitSources      []GitSourceConfigYAML `yaml:"git_sources"`
	Documents       []DocSourceConfigYAML `yaml:"documents"`
	Connectors      []KnowledgeConnector  `yaml:"connectors,omitempty"`
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
