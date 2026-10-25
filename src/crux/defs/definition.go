package defs

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/crux/config"
)

// Definition is the portable YAML format for a whole agent: the dashboard's
// import/export schema and the document pkg/defsrc parses in a Hive. Only the
// fields listed here, and applied by Merge, can be set by a definition;
// anything absent is, by construction, not settable from a file. The struct
// is kept field-for-field with pkg/defsrc.AgentDefinition so a document
// written for one loads in the other.
type Definition struct {
	APIVersion string   `yaml:"apiVersion" json:"apiVersion"`
	Kind       string   `yaml:"kind" json:"kind"`
	Metadata   Metadata `yaml:"metadata" json:"metadata"`
	Spec       Spec     `yaml:"spec" json:"spec"`
}

// Metadata names and presents the agent.
type Metadata struct {
	Name        string `yaml:"name" json:"name"`
	DisplayName string `yaml:"displayName,omitempty" json:"displayName,omitempty"`
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
	Emoji       string `yaml:"emoji,omitempty" json:"emoji,omitempty"`
	Color       string `yaml:"color,omitempty" json:"color,omitempty"`
	SpecVersion int    `yaml:"specVersion,omitempty" json:"specVersion,omitempty"`
}

// Spec is the agent's behaviour. Cadences is accepted so a Hive export parses,
// but it is governor scheduling (pkg/config.Cadence) that no bare crux runs,
// so Merge ignores it — as pkg/defsrc's merge does.
type Spec struct {
	Backend         string                    `yaml:"backend,omitempty" json:"backend,omitempty"`
	Model           string                    `yaml:"model,omitempty" json:"model,omitempty"`
	Role            string                    `yaml:"role,omitempty" json:"role,omitempty"`
	Mode            string                    `yaml:"mode,omitempty" json:"mode,omitempty"`
	SortOrder       int                       `yaml:"sortOrder,omitempty" json:"sortOrder,omitempty"`
	BeadRole        string                    `yaml:"beadRole,omitempty" json:"beadRole,omitempty"`
	StaleTimeout    int                       `yaml:"staleTimeout,omitempty" json:"staleTimeout,omitempty"`
	RestartStrategy string                    `yaml:"restartStrategy,omitempty" json:"restartStrategy,omitempty"`
	ClearOnKick     bool                      `yaml:"clearOnKick,omitempty" json:"clearOnKick,omitempty"`
	IncludeRepos    bool                      `yaml:"includeRepos,omitempty" json:"includeRepos,omitempty"`
	LaneKeywords    []string                  `yaml:"laneKeywords,omitempty" json:"laneKeywords,omitempty"`
	DetectKeywords  []string                  `yaml:"detectKeywords,omitempty" json:"detectKeywords,omitempty"`
	Aliases         []string                  `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	Cadences        map[string]string         `yaml:"cadences,omitempty" json:"cadences,omitempty"`
	PromptTemplate  string                    `yaml:"promptTemplate,omitempty" json:"promptTemplate,omitempty"`
	Channels        []config.ChannelConfig    `yaml:"channels,omitempty" json:"channels,omitempty"`
	Tools           *config.ToolsConfig       `yaml:"tools,omitempty" json:"tools,omitempty"`
	Connections     []config.ConnectionConfig `yaml:"connections,omitempty" json:"connections,omitempty"`
}

// ParseDefinition parses and structurally validates a portable definition.
func ParseDefinition(data []byte) (*Definition, error) {
	var def Definition
	if err := yaml.Unmarshal(data, &def); err != nil {
		return nil, fmt.Errorf("invalid definition YAML: %w", err)
	}
	if def.Kind != DefinitionKind {
		return nil, fmt.Errorf("expected kind %q, got %q", DefinitionKind, def.Kind)
	}
	if def.Metadata.Name == "" {
		return nil, fmt.Errorf("metadata.name is required")
	}
	return &def, nil
}

// Name is the agent name the definition yields: metadata.name lower-cased
// with spaces turned into hyphens, as the dashboard import names it.
func (d *Definition) Name() string {
	return strings.ToLower(strings.ReplaceAll(d.Metadata.Name, " ", "-"))
}

// Merge applies the definition's fields over base and returns the result,
// through the same allow-list as pkg/defsrc.MergeAllowedFields: empty
// strings and nil slices leave base alone; clearOnKick and includeRepos carry
// meaning at their zero value, so the definition's value is authoritative.
func (d *Definition) Merge(base config.AgentConfig) config.AgentConfig {
	merged := base
	if d.Metadata.DisplayName != "" {
		merged.DisplayName = d.Metadata.DisplayName
	}
	if d.Metadata.Description != "" {
		merged.Description = d.Metadata.Description
	}
	if d.Metadata.Emoji != "" {
		merged.Emoji = d.Metadata.Emoji
	}
	if d.Metadata.Color != "" {
		merged.Color = d.Metadata.Color
	}
	if d.Spec.Backend != "" {
		merged.Backend = d.Spec.Backend
	}
	if d.Spec.Model != "" {
		merged.Model = d.Spec.Model
	}
	if d.Spec.Role != "" {
		merged.Role = d.Spec.Role
	}
	if d.Spec.Mode != "" {
		merged.Mode = d.Spec.Mode
	}
	if d.Spec.SortOrder != 0 {
		merged.SortOrder = d.Spec.SortOrder
	}
	if d.Spec.BeadRole != "" {
		merged.BeadRole = d.Spec.BeadRole
	}
	if d.Spec.StaleTimeout != 0 {
		merged.StaleTimeout = d.Spec.StaleTimeout
	}
	if d.Spec.RestartStrategy != "" {
		merged.RestartStrategy = d.Spec.RestartStrategy
	}
	merged.ClearOnKick = d.Spec.ClearOnKick
	includeRepos := d.Spec.IncludeRepos
	merged.IncludeRepos = &includeRepos
	if len(d.Spec.LaneKeywords) > 0 {
		merged.LaneKeywords = d.Spec.LaneKeywords
	}
	if len(d.Spec.DetectKeywords) > 0 {
		merged.DetectKeywords = d.Spec.DetectKeywords
	}
	if len(d.Spec.Aliases) > 0 {
		merged.Aliases = d.Spec.Aliases
	}
	if len(d.Spec.Channels) > 0 {
		merged.Channels = d.Spec.Channels
	}
	if d.Spec.Tools != nil {
		merged.Tools = d.Spec.Tools
	}
	if len(d.Spec.Connections) > 0 {
		merged.Connections = d.Spec.Connections
	}
	// A portable definition has no clear_on_kick key to detect, so its value
	// is explicit by construction: the merge above already wrote it.
	merged.MarkExplicit(false, true)
	return merged
}
