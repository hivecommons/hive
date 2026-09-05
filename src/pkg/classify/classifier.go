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

	// Lane routing sees the labels as DISCRETE labels, not as one joined
	// string (#5856). classifyTier below keeps the joined form, which is
	// correct for it: its label tests are whole label names
	// ("kind/security", "auto-qa") rather than short routing tokens.
	labelsLower := make([]string, 0, len(issue.Labels))
	for _, l := range issue.Labels {
		if t := strings.ToLower(strings.TrimSpace(l)); t != "" {
			labelsLower = append(labelsLower, t)
		}
	}

	c.Lane = classifyLane(titleLower, labelsLower)
	c.Tier = classifyTier(titleLower, labelsStr, issue.Labels)
	c.Model = tierToModel(c.Tier)
	c.ClusterKey = extractClusterKey(titleLower)

	return c
}

// labelMatchesRoutingToken reports whether one label routes to a token — a lane
// NAME or a lane KEYWORD.
//
// THE RULE: the token must be the whole label, or a whole "/"-delimited segment
// of it. Substring matching inside a label is deliberately not enough.
//
// WHY (#5856). Lane routing used to test tokens against every label with
// strings.Contains over the space-joined label list. Scanner's L4 keyword `fix`
// is a substring of `ai-fix-requested` — the label the dashboard's ACMM
// evaluator stamps on every maturity-gap issue it files
// (api_acmm_eval.go) — so EVERY ACMM gap issue routed to scanner, which is
// ISSUES_ONLY at L4 and cannot open a pull request. A live 4-repo L4 hive had
// 11 of 11 ACMM issues parked there. The label whose entire purpose is to mark
// an issue as AI-fixable was the reason no agent that could fix it ever saw it:
// filterByLane (scheduler.go) admits an issue to an agent only when
// issue.Lane == agentName || issue.Lane == "".
//
// The collision was demonstrable by toggling only the label — same title, same
// lane table:
//
//	"[ACMM L4] Add AI security policy" labels=""                      -> sec-check
//	"[ACMM L4] Add AI security policy" labels="acmm ai-fix-requested" -> scanner
//
// WHY "/" SEGMENTS STILL MATCH AND "-" PARTS DO NOT. The two characters mean
// different things in GitHub label practice. "/" is a NAMESPACE separator —
// `kind/regression`, `kind/bug`, `area/api` — so the segment after it is the
// label's actual subject, and ci-maintainer's `regression` keyword matching
// `kind/regression` is routing working as intended. This package already relies
// on that convention (classifyTier tests `kind/security` and `kind/regression`
// by name). "-" is part of a single label's own name: `ai-fix-requested` is one
// word meaning one thing, and it is not a request about "fix" any more than
// `do-not-merge` is a request about "merge". Splitting on "-" would leave the
// original bug in place, which is why this splits on "/" only.
//
// A SECOND BUG THIS CLOSES: the old joined-string test let a multi-word token
// straddle two unrelated labels. Keyword `breaking change` matched an issue
// labelled `breaking` + `change` — two separate labels — because the join put a
// space between them. Matching per-label makes that impossible.
//
// Titles are NOT affected. They keep substring matching, which is right for
// prose: "fix the login crash" should reach the `fix` lane.
func labelMatchesRoutingToken(label, token string) bool {
	if label == "" || token == "" {
		return false
	}
	if label == token {
		return true
	}
	for _, seg := range strings.Split(label, "/") {
		if seg == token {
			return true
		}
	}
	return false
}

// anyLabelMatchesRoutingToken reports whether any of an issue's labels routes
// to token, per labelMatchesRoutingToken. Both inputs are expected lowercased
// (Classify lowercases them once).
func anyLabelMatchesRoutingToken(labels []string, token string) bool {
	for _, l := range labels {
		if labelMatchesRoutingToken(l, token) {
			return true
		}
	}
	return false
}

// classifyLane picks the lane an issue routes to. First match wins, across
// three passes in order of decreasing explicitness: a title prefix naming an
// agent, a label naming an agent, then keywords.
//
// Within each pass the winner depends on the ORDER of activeLanes(), so the
// caller of SetLanes owes the classifier a deterministic order — see the sort
// in initAgentConfigDrivenSystems (cmd/hive/main.go), which supplies one for
// the config-driven table built by ranging over a Go map.
func classifyLane(titleLower string, labelsLower []string) Lane {
	// Title-prefix routing: issues prefixed with [agent-name] route back to
	// that agent's lane so agents see their own previously-filed issues.
	for _, lane := range activeLanes() {
		prefix := "[" + lane.Name + "]"
		if strings.HasPrefix(titleLower, prefix) {
			return Lane(lane.Name)
		}
	}

	// Label-based routing: a label naming a lane routes to that lane. This is
	// the documented operator override for a misclassified issue, so it stays
	// ahead of keywords.
	for _, lane := range activeLanes() {
		if anyLabelMatchesRoutingToken(labelsLower, lane.Name) {
			return Lane(lane.Name)
		}
	}

	// Keyword routing.
	for _, lane := range activeLanes() {
		for _, kw := range lane.Keywords {
			if strings.Contains(titleLower, kw) || anyLabelMatchesRoutingToken(labelsLower, kw) {
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
