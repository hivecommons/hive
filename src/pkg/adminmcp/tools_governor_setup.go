package adminmcp

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// ToolGovernorSetup answers "set up these governors for me" (#10034, part of
// #10019) for a new hive. It reads GET /api/config/governor — the same
// endpoint settings_read wraps, which already carries the repo count, cadence
// scope, scaling curve, raw/effective/pinned thresholds and budget — and
// returns a proposed governor setup: one entry per setting with a one-line
// reason and the registered write operation and args that would apply it
// through write_preview / write_confirm. It writes nothing itself.
const ToolGovernorSetup = "governor_setup_proposal"

// governorSetupSqrtRepoCount is the repo count from which the proposal
// prefers sqrt threshold scaling over linear. Linear multiplies every base by
// the repo count, so a hive watching this many repos would need a queue of
// 200+ items before it ever surged; sqrt keeps the ladder reachable.
const governorSetupSqrtRepoCount = 10

type governorSetupRecommendation struct {
	Setting     string         `json:"setting"`
	Current     any            `json:"current"`
	Recommended any            `json:"recommended"`
	Change      bool           `json:"change"`
	Reason      string         `json:"reason"`
	Operation   string         `json:"operation"`
	Args        map[string]any `json:"args,omitempty"`
	OwnerInput  []string       `json:"owner_input,omitempty"`
	Disclosure  string         `json:"disclosure,omitempty"`
}

// GovernorSetupProposal turns a decoded GET /api/config/governor response into
// the governor_setup_proposal answer. Recommendations come in apply order
// (scaling before thresholds, since thresholds are resolved against the
// curve), and only entries with change=true need applying.
func GovernorSetupProposal(data any) (any, error) {
	body, ok := data.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: unexpected governor settings response %T", ToolGovernorSetup, data)
	}
	repoCount := int(numberField(body, "repoCount"))
	if repoCount < 1 {
		repoCount = 1
	}
	cadenceScope, _ := body["cadenceScope"].(string)
	currentScaling, _ := body["thresholdScaling"].(string)
	if currentScaling == "" {
		currentScaling = config.ThresholdScalingLinear
	}
	effective := thresholdMap(body["effectiveThresholds"])
	pinned := pinnedModes(body["pinnedThresholds"])
	budget, _ := body["budget"].(map[string]any)

	scaling := scalingRecommendation(repoCount, cadenceScope, currentScaling)
	recScaling, _ := scaling.Recommended.(string)
	recs := []governorSetupRecommendation{
		scaling,
		thresholdsRecommendation(repoCount, cadenceScope, recScaling, effective, pinned),
		budgetRecommendation(budget),
	}
	changes := 0
	for _, r := range recs {
		if r.Change {
			changes++
		}
	}
	summary := "The current governor setup already matches this proposal; nothing needs applying."
	if changes > 0 {
		summary = fmt.Sprintf("%d of %d settings would change. Apply each recommendation with change=true by calling write_preview with its operation and args, then write_confirm; apply them in the order listed.", changes, len(recs))
	}
	return map[string]any{
		"type":    ToolGovernorSetup,
		"summary": summary,
		"inputs": map[string]any{
			"repo_count":           repoCount,
			"cadence_scope":        cadenceScope,
			"threshold_scaling":    currentScaling,
			"effective_thresholds": effective,
			"pinned_thresholds":    pinned,
			"budget":               budget,
		},
		"recommendations": recs,
		"not_considered":  "Autonomy level, queue depth and feature toggles are not part of this proposal: read autonomy_readiness, fleet_status and settings_read for those, and change them with fleet.autonomy_level or governor.feature_settings.",
	}, nil
}

func scalingRecommendation(repoCount int, cadenceScope, current string) governorSetupRecommendation {
	rec := governorSetupRecommendation{Setting: "threshold_scaling", Current: current, Operation: WriteOpGovernorThresholdScaling}
	switch {
	case cadenceScope == config.CadenceScopePerRepo:
		rec.Recommended = current
		rec.Reason = "cadence scope is per_repo, so each repo is laddered against unscaled base thresholds and the curve has no effect; keep it."
	case repoCount <= 1:
		rec.Recommended = current
		rec.Reason = "this hive watches one repo, so every curve yields the base thresholds; keep it."
	case repoCount >= governorSetupSqrtRepoCount:
		rec.Recommended = config.ThresholdScalingSqrt
		rec.Reason = fmt.Sprintf("with %d repos, linear scaling multiplies every base threshold by %d and the busier modes are rarely reached; sqrt keeps the ladder reachable while still growing with repo count.", repoCount, repoCount)
	default:
		rec.Recommended = config.ThresholdScalingLinear
		rec.Reason = fmt.Sprintf("with %d repos, linear scaling (the default) compares per-repo queue pressure against the base thresholds, so the modes mean the same thing as on a one-repo hive.", repoCount)
	}
	rec.Args = map[string]any{"scaling": rec.Recommended}
	rec.Change = rec.Recommended != current
	if rec.Change {
		rec.Disclosure = thresholdScalingDisclosure(rec.Recommended.(string))
	}
	return rec
}

func thresholdsRecommendation(repoCount int, cadenceScope, scaling string, effective map[string]int, pinned []string) governorSetupRecommendation {
	rec := governorSetupRecommendation{Setting: "thresholds", Current: effective, Operation: WriteOpGovernorThresholds}
	if len(pinned) == 0 {
		rec.Recommended = "leave unset"
		rec.Reason = "no thresholds are operator-pinned, so the defaults (or the autonomy pack's) are scaled to this hive's size automatically; writing explicit values would pin them."
		rec.Disclosure = "Applying governor.thresholds makes the WHOLE set operator-owned absolutes that repo-count scaling no longer adjusts (#4037), which is why no values are proposed here."
		return rec
	}
	scaleRepos := repoCount
	if cadenceScope == config.CadenceScopePerRepo {
		scaleRepos = 1
	}
	want := map[string]int{
		"quiet": config.ScaleThreshold(config.DefaultThresholdQuiet, scaleRepos, scaling),
		"busy":  config.ScaleThreshold(config.DefaultThresholdBusy, scaleRepos, scaling),
		"surge": config.ScaleThreshold(config.DefaultThresholdSurge, scaleRepos, scaling),
	}
	rec.Recommended = want
	rec.Disclosure = "Explicit thresholds are absolute: the whole set stays operator-owned and repo-count scaling does not apply to any mode (#4037), so these values will not follow the hive if repos are added or removed — re-run this proposal then. Re-applying the autonomy level (fleet.autonomy_level) is what hands thresholds back to scaling."
	if sameThresholds(effective, want) {
		rec.Reason = fmt.Sprintf("thresholds %s are operator-pinned but already equal the %s-scaled defaults for %d repo(s); keep them.", describeThresholds(effective), scaling, scaleRepos)
		return rec
	}
	rec.Change = true
	rec.Args = map[string]any{"quiet": want["quiet"], "busy": want["busy"], "surge": want["surge"]}
	rec.Reason = fmt.Sprintf("thresholds %s are operator-pinned (%s) and differ from the %s-scaled defaults for %d repo(s); set them to %s.", describeThresholds(effective), strings.Join(pinned, ", "), scaling, scaleRepos, describeThresholds(want))
	return rec
}

func budgetRecommendation(budget map[string]any) governorSetupRecommendation {
	total := int(numberField(budget, "totalTokens"))
	rec := governorSetupRecommendation{Setting: "budget", Current: budget, Operation: WriteOpBudgetUpdate}
	if total > 0 {
		rec.Recommended = "keep"
		rec.Reason = fmt.Sprintf("a token budget of %d per period is already set, so the governor has a spend gate.", total)
		return rec
	}
	rec.Recommended = "set totalTokens"
	rec.Change = true
	rec.Args = map[string]any{}
	rec.OwnerInput = []string{"totalTokens"}
	rec.Reason = "no token budget is set, so nothing gates spend; choose the most tokens you are willing to spend per budget period and pass it as totalTokens."
	rec.Disclosure = "Setting a budget narrows agent activity once spend reaches it; only the owner can say what that cap should be, so no number is proposed."
	return rec
}

func numberField(m map[string]any, key string) float64 {
	switch v := m[key].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	default:
		return 0
	}
}

func thresholdMap(raw any) map[string]int {
	out := map[string]int{}
	m, _ := raw.(map[string]any)
	for _, mode := range governorThresholdModes {
		if _, ok := m[mode]; ok {
			out[mode] = int(numberField(m, mode))
		}
	}
	return out
}

func pinnedModes(raw any) []string {
	m, _ := raw.(map[string]any)
	out := []string{}
	for mode, v := range m {
		if v == true {
			out = append(out, mode)
		}
	}
	sort.Strings(out)
	return out
}

func sameThresholds(a, b map[string]int) bool {
	for _, mode := range governorThresholdModes {
		if a[mode] != b[mode] {
			return false
		}
	}
	return true
}
