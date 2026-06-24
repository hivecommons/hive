package classify

import (
	"strings"
	"sync"

	"github.com/kubestellar/hive/v2/pkg/github"
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

var simpleKeywords = []string{
	"typo", "i18n", "rename", "const", "label", "badge",
	"tooltip", "placeholder", "aria", "alt text",
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
	for _, kw := range simpleKeywords {
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

	complexSignals := []string{"race condition", "deadlock", "memory leak", "performance", "api change"}
	for _, sig := range complexSignals {
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
