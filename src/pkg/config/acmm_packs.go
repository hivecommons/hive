package config

import (
	"embed"
	"fmt"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

//go:embed packs/*.yaml
var packsFS embed.FS

// ACMMPack defines a curated set of agents for a given ACMM maturity level.
type ACMMPack struct {
	Level       int          `json:"level" yaml:"level"`
	Name        string       `json:"name" yaml:"name"`
	Description string       `json:"description" yaml:"description"`
	Agents      []PackAgent  `json:"agents" yaml:"agents"`
	Governor    PackGovernor `json:"governor" yaml:"governor"`
}

// PackAgent describes an agent within an ACMM pack.
type PackAgent struct {
	Name         string   `json:"name" yaml:"name"`
	DisplayName  string   `json:"displayName" yaml:"display_name"`
	Role         string   `json:"role" yaml:"role"`
	Description  string   `json:"description" yaml:"description"`
	Emoji        string   `json:"emoji" yaml:"emoji"`
	Color        string   `json:"color" yaml:"color"`
	SortOrder    int      `json:"sortOrder" yaml:"sort_order"`
	Backend      string   `json:"backend" yaml:"backend"`
	Model        string   `json:"model" yaml:"model"`
	BeadRole     string   `json:"beadRole" yaml:"bead_role"`
	KickTemplate string   `json:"kickTemplate" yaml:"kick_template"`
	IncludeRepos bool     `json:"includeRepos" yaml:"include_repos"`
	LaneKeywords []string `json:"laneKeywords" yaml:"lane_keywords"`
	Interactions string   `json:"interactions" yaml:"interactions"`
	KnowledgeUse string   `json:"knowledgeUse" yaml:"knowledge_use"`
	Hidden       bool     `json:"hidden,omitempty" yaml:"hidden"`
	StaleTimeout int      `json:"staleTimeout,omitempty" yaml:"stale_timeout,omitempty"`
	Mode         string   `json:"mode,omitempty" yaml:"mode,omitempty"`
	OnDemand     bool     `json:"onDemand,omitempty" yaml:"on_demand,omitempty"`
	CavemanMode  string   `json:"cavemanMode,omitempty" yaml:"caveman_mode,omitempty"`
}

// PackGovernor describes the governor configuration recommended for a level.
type PackGovernor struct {
	Modes         string                       `json:"modes" yaml:"modes"`
	MergePolicy   string                       `json:"mergePolicy" yaml:"merge_policy"`
	EvalIntervalS int                          `json:"evalIntervalS,omitempty" yaml:"eval_interval_s,omitempty"`
	Cadences      map[string]map[string]string `json:"cadences,omitempty" yaml:"cadences,omitempty"`
	Thresholds    map[string]int               `json:"thresholds,omitempty" yaml:"thresholds,omitempty"`
	// PlanAutoApprove controls the Phase 2 plan-review gate for this maturity
	// level. When false (default, low levels), a decomposed epic's plan stays
	// plan_status=draft and its children are withheld from Ready() until a human
	// approves it. When true (high-trust levels L5/L6), decomposition sets
	// plan_status=approved immediately, releasing the children without a manual
	// review step.
	PlanAutoApprove bool `json:"planAutoApprove,omitempty" yaml:"plan_auto_approve,omitempty"`
}

// PlanAutoApproveForLevel reports whether the ACMM pack at the given level
// enables automatic plan approval (plan_auto_approve). It is the single lookup
// the decomposition path uses to decide whether a fresh plan is drafted (human
// review required) or approved immediately. Unknown levels return false so the
// safe default (require review) always wins.
func PlanAutoApproveForLevel(level int) bool {
	p, err := ACMMPackByLevel(level)
	if err != nil {
		return false
	}
	return p.Governor.PlanAutoApprove
}

// ACMMPacks returns the built-in ACMM level pack definitions loaded from
// embedded YAML files. These files live in src/packs/ and can be forked,
// tweaked, or contributed by the community.
func ACMMPacks() []ACMMPack {
	entries, err := packsFS.ReadDir("packs")
	if err != nil {
		return nil
	}

	var packs []ACMMPack
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		data, err := packsFS.ReadFile("packs/" + entry.Name())
		if err != nil {
			continue
		}
		var pack ACMMPack
		if err := yaml.Unmarshal(data, &pack); err != nil {
			continue
		}
		packs = append(packs, pack)
	}

	sort.Slice(packs, func(i, j int) bool {
		return packs[i].Level < packs[j].Level
	})
	return packs
}

// OnDemandAgentsFromPacks returns the set of agent names marked as on-demand
// across ALL pack levels. Used to prevent auto-starting these agents regardless
// of which level is active.
func OnDemandAgentsFromPacks() map[string]bool {
	result := make(map[string]bool)
	for _, pack := range ACMMPacks() {
		for _, a := range pack.Agents {
			if a.OnDemand {
				result[a.Name] = true
			}
		}
	}
	return result
}

// ACMMPackByLevel returns the pack for a specific level, or an error if not found.
func ACMMPackByLevel(level int) (ACMMPack, error) {
	for _, p := range ACMMPacks() {
		if p.Level == level {
			return p, nil
		}
	}
	return ACMMPack{}, fmt.Errorf("ACMM pack level %d not found", level)
}
