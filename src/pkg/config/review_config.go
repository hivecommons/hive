// Review-pipeline configuration: escalation, reviewer parallelism, and the
// auto-merge gate (required checks, self-authored ACMM gating).
package config

import (
	"strings"
)

// DefaultAutoMergeLabel is the label applied when a hive does not configure
// `governor.labels.automerge`. `lgtm` is the long-standing Prow/Kubernetes
// convention for "a second person signed off on this", which is exactly the
// decision the merger tier records, so a managed repository usually already
// has it and already understands it.
const DefaultAutoMergeLabel = "lgtm"

// EscalationConfig tunes the fix-loop circuit breaker (pkg/escalation): after
// Threshold distinct failed fix attempts on the same agent-authored PR, the
// hub stops dispatching further fixes and escalates to a human with the raw
// CI failure evidence.
type EscalationConfig struct {
	// Disabled turns the breaker off entirely (the zero value keeps it on —
	// escalation is a safety net and must be opt-out, not opt-in).
	Disabled bool `yaml:"disabled,omitempty" json:"disabled,omitempty"`
	// Threshold is the distinct-red-attempt count that triggers escalation.
	// Zero means DefaultEscalationThreshold.
	Threshold int `yaml:"threshold,omitempty" json:"threshold,omitempty"`
}

// ReviewConfig gates the optional structured review-swarm merge gate. The zero
// value preserves existing behavior: merge eligibility does not require a
// review-verdicts.json aggregate approval unless an operator opts in.
type ReviewConfig struct {
	RequireApproval    bool     `yaml:"require_approval,omitempty" json:"require_approval,omitempty"`
	FanOut             bool     `yaml:"fan_out,omitempty" json:"fan_out,omitempty"`
	MaxParallelReviews int      `yaml:"max_parallel_reviews,omitempty" json:"max_parallel_reviews,omitempty"`
	ReviewerAgents     []string `yaml:"reviewer_agents,omitempty" json:"reviewer_agents,omitempty"`
	FixerAgent         string   `yaml:"fixer_agent,omitempty" json:"fixer_agent,omitempty"`
}

// AutoMergeConfig gates the App-self-merge sweep (SweepSelfAuthoredAutoMerges).
// Prow structurally forbids self-approval on a PR's own author, so a PR the
// Forge App itself opens can never collect the lgtm+approved labels tide
// requires — nobody but the App authored it, and the App cannot review its
// own work. The sweep is Hive's only route to landing such a PR: it merges
// directly over the GitHub REST API (squash), bypassing tide entirely,
// exactly as the existing human-queue sweep already does for tide-pending/
// unstable states (see mergeableFromState). This block controls ONLY that
// self-authored path; the human "Approved ... for Hive auto-merge" queue
// (governor.labels.automerge) is untouched and still requires a distinct
// human queuer.
type AutoMergeConfig struct {
	// SelfAuthored enables SweepSelfAuthoredAutoMerges: the App merges its own
	// open, CI-green PRs without a human queue-approval review, at every
	// intent tier (including Tier3, which normally requires HumanApproval —
	// see intent.Evaluate). *bool so "unset" is distinguishable from an
	// explicit false: default is ON, matching the operator intent that the
	// App should never be blocked behind a review it is structurally unable
	// to obtain for its own PRs. Set `auto_merge.self_authored: false` to
	// disable and fall back to fully manual merges for App PRs.
	SelfAuthored *bool `yaml:"self_authored,omitempty" json:"self_authored,omitempty"`
	// MaxMerges caps merges per sweep pass, shared semantics with
	// AutoMergeSweepOptions.MaxMerges for the human queue sweep. Zero means
	// DefaultAutoMergeSweepMaxMerges.
	MaxMerges int `yaml:"max_merges,omitempty" json:"max_merges,omitempty"`
	// RequiredChecks is the operator-declared list of status-check
	// contexts/check-run names that the self-merge sweep's commitGreen must
	// gate on, e.g. ["build-gate"]. This is the scope-free alternative to
	// asking GitHub's branch-protection API (Repositories.GetRequiredStatusChecks)
	// which one, and only one, of these checks are actually required: older Hive
	// App installations often lacked administration:read, so that call failed
	// closed to the coarser isMetaCheck/isIgnorableCICheck allowlist — which
	// still blocked on non-required checks like "Detect untested files"
	// (cancelled) or "Analyze (python)" (CodeQL failure). Declaring the
	// branch's actual required set here removes the dependency on that API for
	// naming required checks.
	//
	// Unset/empty means "not config-declared" (RequiredCheckSet returns
	// requiredKnown=false) — callers then fall back to the branch-protection
	// API, and if that also cannot determine the set, to the allowlist. There
	// is deliberately no hardcoded default here: the required-checks set is
	// per-repo (e.g. console's main branch requires only "build-gate"), so
	// the operator must declare it per-hive in `auto_merge.required_checks`.
	RequiredChecks []string `yaml:"required_checks,omitempty" json:"required_checks,omitempty"`
	// AllowUnprotectedBase is the explicit per-repo exception list for the
	// merge-request relay's base-branch protection guard. By default the relay
	// refuses to merge into a base branch with no GitHub branch protection; a
	// repo listed here is allowed to rely on Hive's CI gate alone.
	AllowUnprotectedBase []string `yaml:"allow_unprotected_base,omitempty" json:"allow_unprotected_base,omitempty"`
	// NoCIOK is the explicit per-repo exception list for repositories that have
	// no CI by design. A listed repo may downgrade the merge-request relay's
	// "zero statuses + zero check runs + zero workflow runs" verdict from
	// unverified to green. Red or pending evidence still refuses/waits.
	NoCIOK []string `yaml:"no_ci_ok,omitempty" json:"no_ci_ok,omitempty"`
}

// RequiredCheckSet returns the config-declared required-status-check set as a
// membership map, and whether the config actually declared one. An
// empty/unset RequiredChecks list returns (nil, false) — "not config-declared"
// — so callers can distinguish that from a genuinely empty required set (e.g.
// an unprotected branch) and fall through to their next source (the
// branch-protection API, then the allowlist fallback). A non-empty list
// always returns requiredKnown=true; entries are matched by exact string
// equality against status-context / check-run names.
func (a AutoMergeConfig) RequiredCheckSet() (map[string]bool, bool) {
	if len(a.RequiredChecks) == 0 {
		return nil, false
	}
	set := make(map[string]bool, len(a.RequiredChecks))
	for _, name := range a.RequiredChecks {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[name] = true
	}
	if len(set) == 0 {
		return nil, false
	}
	return set, true
}

func (a AutoMergeConfig) AllowUnprotectedBaseSet() map[string]bool {
	return repoListSet(a.AllowUnprotectedBase)
}

func (a AutoMergeConfig) NoCIOKSet() map[string]bool {
	return repoListSet(a.NoCIOK)
}

func repoListSet(repos []string) map[string]bool {
	if len(repos) == 0 {
		return nil
	}
	set := make(map[string]bool, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo != "" {
			set[repo] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

// SelfAuthoredEnabled reports whether the App-self-merge sweep is on for this
// hive. Default ON (nil == enabled): see AutoMergeConfig.SelfAuthored.
func (a AutoMergeConfig) SelfAuthoredEnabled() bool {
	return a.SelfAuthored == nil || *a.SelfAuthored
}

// SelfMergeMinACMMLevel is the lowest ACMM level at which the App is allowed
// to self-merge its own PRs. examples/acmm/l5.md is explicit that L5 does NOT
// grant merge authority ("Merge pull requests — no; L5 PRs remain hold-gated
// for human review"), and examples/acmm/l4.md is even more explicit ("DO NOT
// merge pull requests — L4 is not an auto-merge level"). examples/acmm/l6.md
// is the first level whose Responsibilities include "Merge PRs when CI
// passes". So the gate sits at L6, not L5: an L4 (or L5) hive must never run
// the self-authored auto-merge sweep, regardless of the auto_merge.self_authored
// flag below. This was the root cause of ks/hive (acmm_level: 4) wrongly
// self-merging its own PR — the sweep previously had no ACMM check at all.
const SelfMergeMinACMMLevel = 6

// SelfAuthoredAutoMergeAllowed reports whether the App-self-merge sweep may
// run at all for this hive, given its ACMM level. Both conditions must hold:
// the auto_merge.self_authored flag must be ON (SelfAuthoredEnabled) AND the
// hive's ACMM level must be at or above SelfMergeMinACMMLevel. A nil/unset
// ACMM level is treated as NOT high enough (fail closed) — an operator who
// has not explicitly opted a hive into a high ACMM level never gets
// self-merge by accident.
func (a AutoMergeConfig) SelfAuthoredAutoMergeAllowed(acmmLevel *int) bool {
	if !a.SelfAuthoredEnabled() {
		return false
	}
	if acmmLevel == nil {
		return false
	}
	return *acmmLevel >= SelfMergeMinACMMLevel
}

// DefaultEscalationThreshold matches escalation.DefaultThreshold; duplicated
// here (a constant, checked by test) to avoid a config→escalation import.
const DefaultEscalationThreshold = 3

// DefaultMaxParallelReviews matches review.DefaultMaxParallelReviews; duplicated
// here to avoid a config→review import cycle.
const DefaultMaxParallelReviews = 5

// EffectiveMaxParallelReviews resolves the review fan-out width.
func (r ReviewConfig) EffectiveMaxParallelReviews() int {
	if r.MaxParallelReviews > 0 {
		return r.MaxParallelReviews
	}
	return DefaultMaxParallelReviews
}

// EffectiveThreshold resolves the configured threshold with its default.
func (e EscalationConfig) EffectiveThreshold() int {
	if e.Threshold > 0 {
		return e.Threshold
	}
	return DefaultEscalationThreshold
}
