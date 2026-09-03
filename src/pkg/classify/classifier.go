package classify

import (
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/github"
)

type Tier string

const (
	TierSimple  Tier = "Simple"
	TierMedium  Tier = "Medium"
	TierComplex Tier = "Complex"
)

type ModelRecommendation string

const (
	ModelHaiku  ModelRecommendation = "haiku"
	ModelSonnet ModelRecommendation = "sonnet"
	ModelOpus   ModelRecommendation = "opus"
)

type Lane string

// LaneConfig maps a lane name to its keywords for classification.
type LaneConfig struct {
	Name     string
	Keywords []string
}

// DefaultLane is the fallback lane for issues that don't match any keywords.
const DefaultLane = "scanner"

// Backward-compatible lane constants used by tests and other packages.
const (
	LaneScanner      Lane = "scanner"
	LaneCIMaintainer Lane = "ci-maintainer"
	LaneArchitect    Lane = "architect"
	LaneOutreach     Lane = "outreach"
	LaneQuality      Lane = "quality"
)

type Classification struct {
	Tier       Tier                `json:"complexity_tier"`
	Model      ModelRecommendation `json:"model_recommendation"`
	Lane       Lane                `json:"lane"`
	ClusterKey string              `json:"cluster_key,omitempty"`
}

// defaultSimpleKeywords are the built-in title keywords that mark an issue as
// Tier "Simple". Kept as the fallback when no config-driven set is provided via
// SetTierKeywords, so behavior is unchanged when the classifier block is absent
// from hive.yaml.
var defaultSimpleKeywords = []string{
	"typo", "i18n", "rename", "const", "label", "badge",
	"tooltip", "placeholder", "aria", "alt text",
}

// defaultComplexSignals are the built-in title signals that mark an issue as
// Tier "Complex". Like defaultSimpleKeywords, this is the fallback used when
// SetTierKeywords has not been called with a non-empty complex set.
var defaultComplexSignals = []string{
	"race condition", "deadlock", "memory leak", "performance", "api change",
}

var tierMu sync.RWMutex
var configuredSimpleKeywords []string
var configuredComplexSignals []string

// SetTierKeywords replaces the tier-classification keyword sets used by
// classifyTier, mirroring SetLanes. Empty slices leave the corresponding
// default in force, so a partial config (only simple, only complex, or neither)
// still classifies with the built-in list for the unset side. It is safe to
// call concurrently with Classify.
func SetTierKeywords(simple, complex []string) {
	tierMu.Lock()
	defer tierMu.Unlock()
	configuredSimpleKeywords = simple
	configuredComplexSignals = complex
}

// activeSimpleKeywords returns the configured Simple-tier keywords, or the
// built-in defaults when none are configured.
func activeSimpleKeywords() []string {
	tierMu.RLock()
	defer tierMu.RUnlock()
	if len(configuredSimpleKeywords) > 0 {
		return configuredSimpleKeywords
	}
	return defaultSimpleKeywords
}

// activeComplexSignals returns the configured Complex-tier signals, or the
// built-in defaults when none are configured.
func activeComplexSignals() []string {
	tierMu.RLock()
	defer tierMu.RUnlock()
	if len(configuredComplexSignals) > 0 {
		return configuredComplexSignals
	}
	return defaultComplexSignals
}

// TierKeywords returns copies of the currently-effective Simple keywords and
// Complex signals (config-driven when set, else the built-in defaults). The
// dashboard governor-config endpoint surfaces these so operators can see exactly
// which lists are in force. Copies are returned so callers cannot mutate
// classifier state.
func TierKeywords() (simple, complex []string) {
	return append([]string(nil), activeSimpleKeywords()...),
		append([]string(nil), activeComplexSignals()...)
}

// defaultLanes is the built-in lane config used when no config-driven lanes are provided.
var defaultLanes = []LaneConfig{
	{Name: "architect", Keywords: []string{"rfc", "architecture", "refactor", "redesign", "migration", "breaking change", "protocol", "api design"}},
	{Name: "ci-maintainer", Keywords: []string{"workflow-failure", "ci-failure", "nightly", "coverage", "regression", "ga4", "analytics"}},
	{Name: "outreach", Keywords: []string{"adopters", "outreach", "community", "engagement"}},
	{Name: "quality", Keywords: []string{"test-gap", "test-strategy", "test-coverage", "test-scaffold", "untested", "missing-tests"}},
}

var lanesMu sync.RWMutex
var configuredLanes []LaneConfig

// SetLanes replaces the lane configuration used by the classifier.
func SetLanes(lanes []LaneConfig) {
	lanesMu.Lock()
	defer lanesMu.Unlock()
	configuredLanes = lanes
}

func activeLanes() []LaneConfig {
	lanesMu.RLock()
	defer lanesMu.RUnlock()
	if len(configuredLanes) > 0 {
		return configuredLanes
	}
	return defaultLanes
}

func Classify(issue github.Issue) Classification {
	c := Classification{
		Tier:  TierMedium,
		Model: ModelSonnet,
		Lane:  Lane(DefaultLane),
	}

	titleLower := strings.ToLower(issue.Title)
	labelsStr := strings.ToLower(strings.Join(issue.Labels, " "))

	c.Lane = classifyLane(titleLower, labelsStr)
	c.Tier = classifyTier(titleLower, labelsStr, issue.Labels)
	c.Model = tierToModel(c.Tier)
	c.ClusterKey = extractClusterKey(titleLower)

	return c
}

func classifyLane(titleLower, labelsStr string) Lane {
	// Title-prefix routing: issues prefixed with [agent-name] route back to
	// that agent's lane so agents see their own previously-filed issues.
	for _, lane := range activeLanes() {
		prefix := "[" + lane.Name + "]"
		if strings.HasPrefix(titleLower, prefix) {
			return Lane(lane.Name)
		}
	}

	// Label-based routing: a label matching a lane name routes to that lane.
	for _, lane := range activeLanes() {
		if strings.Contains(labelsStr, lane.Name) {
			return Lane(lane.Name)
		}
	}

	// Keyword routing (original behavior).
	for _, lane := range activeLanes() {
		for _, kw := range lane.Keywords {
			if strings.Contains(titleLower, kw) || strings.Contains(labelsStr, kw) {
				return Lane(lane.Name)
			}
		}
	}
	return Lane(DefaultLane)
}

func classifyTier(titleLower, labelsStr string, labels []string) Tier {
	for _, kw := range activeSimpleKeywords() {
		if strings.Contains(titleLower, kw) {
			return TierSimple
		}
	}

	for _, l := range labels {
		if l == "auto-qa" || l == "auto-qa-finding" {
			return TierSimple
		}
	}

	if strings.Contains(labelsStr, "kind/security") || strings.Contains(labelsStr, "kind/regression") {
		return TierComplex
	}

	for _, sig := range activeComplexSignals() {
		if strings.Contains(titleLower, sig) {
			return TierComplex
		}
	}

	return TierMedium
}

func tierToModel(t Tier) ModelRecommendation {
	switch t {
	case TierSimple:
		return ModelHaiku
	case TierComplex:
		return ModelOpus
	default:
		return ModelSonnet
	}
}

func extractClusterKey(titleLower string) string {
	prefixes := []string{
		"dashboard", "card", "sidebar", "navbar", "modal",
		"api", "webhook", "ci", "nightly", "drasi",
		"benchmark", "gpu", "mission", "studio",
	}

	for _, p := range prefixes {
		if strings.Contains(titleLower, p) {
			return p
		}
	}
	return ""
}

func ClassifyAll(issues []github.Issue) []github.Issue {
	for i := range issues {
		c := Classify(issues[i])
		issues[i].ComplexityTier = string(c.Tier)
		issues[i].ModelRec = string(c.Model)
		issues[i].Lane = string(c.Lane)
	}
	return issues
}
