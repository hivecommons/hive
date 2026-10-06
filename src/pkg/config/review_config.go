package config

import (
	"strings"
	"time"

	"log"
	"sync"
)

func repoListSet(repos []string) map[string]bool {
	if len(repos) == 0 {
		return nil
	}
	set := make(map[string]bool, len(repos))
	for _, repo := range repos {
		repo = strings.TrimSpace(repo)
		if repo != "" {
			set[strings.ToLower(repo)] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	return set
}

const (
	ReviewModelsFallbackPinned        = "pinned"
	ReviewModelsFallbackSkip          = "skip"
	ReviewModelsFallbackRequiresHuman = "requires_human"
)

type ReviewModelPoolEntry struct {
	Backend string `yaml:"backend" json:"backend,omitempty"`
	Model   string `yaml:"model" json:"model,omitempty"`
}

type ReviewModelsConfig struct {
	Pool                []ReviewModelPoolEntry `yaml:"pool,omitempty" json:"pool,omitempty"`
	ExcludeAuthorModel  *bool                  `yaml:"exclude_author_model,omitempty" json:"exclude_author_model,omitempty"`
	ExcludeAuthorFamily bool                   `yaml:"exclude_author_family,omitempty" json:"exclude_author_family,omitempty"`
	Fallback            string                 `yaml:"fallback,omitempty" json:"fallback,omitempty"`
}

func (r ReviewModelsConfig) Configured() bool {
	return len(r.Pool) > 0 || r.ExcludeAuthorModel != nil || r.ExcludeAuthorFamily || strings.TrimSpace(r.Fallback) != ""
}

func (r ReviewModelsConfig) ExcludeAuthorModelEnabled() bool {
	return r.ExcludeAuthorModel == nil || *r.ExcludeAuthorModel
}

func (r ReviewModelsConfig) EffectiveFallback() string {
	switch strings.TrimSpace(r.Fallback) {
	case "":
		return ReviewModelsFallbackPinned
	case ReviewModelsFallbackSkip, ReviewModelsFallbackRequiresHuman:
		return strings.TrimSpace(r.Fallback)
	default:
		return strings.TrimSpace(r.Fallback)
	}
}

func ValidateReviewModelsFallback(v string) bool {
	switch strings.TrimSpace(v) {
	case "", ReviewModelsFallbackPinned, ReviewModelsFallbackSkip, ReviewModelsFallbackRequiresHuman:
		return true
	default:
		return false
	}
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
	// PostComments tells review-swarm reviewers to publish their verdict as a
	// PR comment via the `hive-review` relay, in addition to returning the
	// JSON aggregate. Opt-in: the zero value keeps the verdict internal, which
	// is the only safe default because on a hive WITHOUT auto-merge the
	// aggregate has no consumer and the reviewer is silent by construction.
	// Turning this on is what makes a review reach the human who has to decide.
	PostComments bool `yaml:"post_comments,omitempty" json:"post_comments,omitempty"`
	// MaxPerspectivesPerPR caps how many review perspectives one PR may be
	// given, as a LIFETIME budget per head SHA — not a per-cycle limit. It
	// exists because parallel review slots are a fixed budget spent in PR
	// order: without a cap, the first PR in a deep queue absorbs every slot
	// for its own perspectives, so adding reviewers buys more opinions on one
	// PR instead of coverage across many. Capping it spends the same budget
	// breadth-first.
	//
	// Coverage IS traded away deliberately, and that is the point. A
	// per-cycle cap could not bound anything a publishing reviewer does to a
	// PR: the perspectives skipped this cycle stayed "missing" next cycle and
	// were dispatched then, so a 5-perspective default still arrived as 5
	// separate comments on the same PR, one per cadence interval. Setting
	// this to 1 is what makes "the hive left one review comment on this PR"
	// true. See hivecommons/hive#7562.
	//
	// The budget is keyed on the head SHA, so pushing new commits earns a
	// fresh budget — a rewritten PR is reviewed again, an untouched one is
	// not. Zero means no cap, which stays the default, so this only changes
	// a hive that opts in because its queue is too deep to review in depth.
	MaxPerspectivesPerPR int `yaml:"max_perspectives_per_pr,omitempty" json:"max_perspectives_per_pr,omitempty"`
	// Perspectives selects which review perspectives run against each PR, in
	// dispatch order. Empty means the built-in set.
	//
	// It is a per-hive editorial choice, not a behavioural invariant: a fleet
	// governing infrastructure repos wants security on every PR, a docs fleet
	// wants docs-currency and little else. It may also name a perspective this
	// hive invented, which is defined by giving it focus text in
	// PerspectivePrompts. A name that is neither built in nor described there
	// fails config load rather than being dropped — a typo'd "sekurity" that
	// silently disappeared would read as enabled everywhere it is displayed
	// while nothing reviewed it.
	Perspectives []string `yaml:"perspectives,omitempty" json:"perspectives,omitempty"`
	// PerspectivePrompts is what each perspective is told to look for, keyed by
	// perspective name. It overrides the built-in focus line and is how a
	// hive-defined perspective is described at all.
	//
	// This is the editorial half of the setting above. A repo with its own
	// conventions wants the style perspective told what those conventions ARE
	// rather than left to infer them from the tree; a hive can only get that
	// by writing it down. An entry that is blank means "use the built-in".
	PerspectivePrompts map[string]string `yaml:"perspective_prompts,omitempty" json:"perspective_prompts,omitempty"`
	// CombinedPerspectives reviews every perspective in ONE agent session that
	// leaves ONE comment, instead of one session and one comment per
	// perspective.
	//
	// Breadth and quiet used to be in direct conflict: covering five
	// perspectives meant five comments on one PR, and MaxPerspectivesPerPR
	// bought quiet by never running four of the five. Combining them removes
	// the trade — one comment however many perspectives are covered, and the
	// expensive part of a review (reading the diff and the surrounding tree)
	// is done once instead of five times. The verdicts stay separate, so any
	// single perspective can still withhold approval.
	CombinedPerspectives bool `yaml:"combined_perspectives,omitempty" json:"combined_perspectives,omitempty"`
	// MaxReviewsPerHead caps how many top-level hive reviews one PR head may
	// receive before the relay refuses further ones (revisions and thread
	// replies excepted). 0 derives the cap: 1 with combined_perspectives, one
	// per perspective otherwise. Negative disables the backstop.
	MaxReviewsPerHead int `yaml:"max_reviews_per_head,omitempty" json:"max_reviews_per_head,omitempty"`
	// AllAuthors makes every open PR eligible for review regardless of who
	// opened it. By default the review swarm looks only at agent-authored
	// PRs — the work the hive is answerable for. On a repo whose queue is the
	// problem that restriction is backwards: a contributor's PR waiting on a
	// reviewer is no less stuck than an agent's, and it is the one with a
	// person waiting on the other end.
	AllAuthors bool `yaml:"all_authors,omitempty" json:"all_authors,omitempty"`
	// FixHumanPRs lets the review-fix kick push commits onto PRs that this
	// hive's agents did NOT open: a contributor's fork branch (through "allow
	// edits by maintainers"), a maintainer's branch in the upstream repo, or
	// another bot's PR. AllAuthors alone never implies it. AllAuthors promises
	// that every PR is REVIEWED; a changes_requested verdict on a PR the hive
	// did not author then stops at the published review, and the author
	// decides what to change. Only with this on does the fixer agent
	// (FixerAgent, then the PR lane, then scanner) check out someone else's
	// branch and push to it (hivecommons/hive#8421).
	//
	// Pointer so "never set" is distinguishable from an explicit false: a hive
	// that already ran with all_authors on was pushing fixes to every PR, and
	// an upgrade must not silently stop that. MigrateReviewFixHumanPRs turns a
	// nil into an explicit true on such a hive, once, and logs it; an explicit
	// false is never overwritten. Read via FixHumanPRsEnabled(), never
	// dereferenced raw.
	FixHumanPRs *bool `yaml:"fix_human_prs,omitempty" json:"fix_human_prs,omitempty"`
	// ContributorPRs gates owner-authorized maintenance actions on contributor
	// PRs. Zero value is deliberately off: only an owner who opts in may let
	// Hive ask GitHub to sync a fork PR branch with the base branch.
	ContributorPRs ContributorPRsConfig `yaml:"contributor_prs,omitempty" json:"contributor_prs,omitempty"`
	// AcknowledgeNoFindings makes a clean review leave a one-line record
	// instead of nothing. Silence keeps a PR uncluttered but is
	// indistinguishable from a reviewer that never ran, so where review
	// coverage itself is the thing being demonstrated, a reviewed-and-clean
	// PR should say so. Requires PostComments.
	AcknowledgeNoFindings bool `yaml:"acknowledge_no_findings,omitempty" json:"acknowledge_no_findings,omitempty"`
	// ReviseRepos allowlists repos where the reviewer revises its own previous
	// review in place instead of posting a second one. Editing a review does
	// not notify anyone, so correcting a verdict the hive got wrong costs the
	// maintainers nothing; posting again costs every subscriber a
	// notification. Empty means no repo may be revised, which is the right
	// default: silently rewriting what a maintainer already read is a power
	// worth granting deliberately, per repo.
	ReviseRepos []string `yaml:"revise_repos,omitempty" json:"revise_repos,omitempty"`
	// ReviseVerdictsBefore (RFC3339) re-opens PRs whose verdict was recorded
	// before this instant, even though their head SHA has not moved. It exists
	// for the case where the reviewer itself was wrong rather than the PR:
	// dispatch normally skips any PR that already has a verdict for its head
	// SHA, which is correct while the reviewer is trustworthy and a trap once
	// a reviewer-side defect is found. Without it, verdicts produced by a
	// known-broken reviewer stay frozen until someone happens to push a
	// commit.
	//
	// The cutoff is self-limiting: a re-review records a fresh timestamp that
	// is necessarily after it, so each PR is revisited at most once per bump.
	// Only repos in ReviseRepos are eligible, so this cannot re-post anywhere
	// the hive has not been granted the quieter in-place path.
	ReviseVerdictsBefore string `yaml:"revise_verdicts_before,omitempty" json:"revise_verdicts_before,omitempty"`
	// HumanDecisionLabel names an EXISTING repo label to apply when the review
	// swarm holds a PR for a human. The review comment's in-body marker is
	// always the primary signal and never depends on this: a label makes the
	// holds filterable from the PR list, which a comment buried in a thread
	// cannot do, but a queue is triaged by people reading comments.
	//
	// The label is never created. A hive reviews other people's repos, so
	// inventing a label there would edit someone else's taxonomy uninvited.
	// If the name is empty, misspelled, or absent from the repo, labeling is
	// skipped and the marker still lands — a typo must degrade to today's
	// behaviour, never suppress a review.
	//
	// There is deliberately NO default and no built-in name. Label taxonomies
	// are per-project: the name that means "a person must decide" in one org
	// does not exist in another, so shipping a default would be a name that
	// resolves nowhere for most hives while looking configured. Each hive
	// names a label its own governed repos already maintain.
	HumanDecisionLabel string `yaml:"human_decision_label,omitempty" json:"human_decision_label,omitempty"`
	// ConfidenceScore appends a one-line 0–5 mergeability score to each review
	// comment the hive posts (hivecommons/hive#8182): "**Confidence: 4/5**
	// (safe) — 1 medium finding". The score is derived from the verdicts and
	// finding severities the perspectives reported, never asked of the model,
	// so it means the same thing on every PR. It is always recorded in the
	// verdict artifact; this only controls whether the comment shows it. Off
	// by default: a review surface a repo did not ask for is noise, and the
	// verdict marker already routes the decision.
	ConfidenceScore bool `yaml:"confidence_score,omitempty" json:"confidence_score,omitempty"`
	// PriorityLabels mirrors each open PR's place in the PR review queue onto
	// exactly one review-priority/high|normal|low label (hivecommons/hive#9590;
	// option 2 of src/docs/review-queue-triage.md), so the order shows up in
	// plain `gh` searches and outside tools. The governor applies it
	// mechanically from the computed rank; no agent, and never the PR's
	// author, writes it. Like HumanDecisionLabel the labels are never created:
	// a repo that does not already have them is skipped. Off by default: a
	// label on every PR is churn a repo must ask for.
	PriorityLabels bool `yaml:"priority_labels,omitempty" json:"priority_labels,omitempty"`
	// OutOfScopeBacklogDisabled opts out of filing cited out-of-scope review
	// findings as follow-up issues. Default is enabled: the review can stay
	// narrow without losing real adjacent defects.
	OutOfScopeBacklogDisabled bool `yaml:"out_of_scope_backlog_disabled,omitempty" json:"out_of_scope_backlog_disabled,omitempty"`
	// MaxOutOfScopeBacklogIssues caps how many backlog issues one PR review may
	// file. Zero uses github.DefaultReviewBacklogIssueCap.
	MaxOutOfScopeBacklogIssues int `yaml:"max_out_of_scope_backlog_issues,omitempty" json:"max_out_of_scope_backlog_issues,omitempty"`
	// Recommendations maintains a single, continuously-updated issue per
	// repository that answers "what should I merge next?" for a human working
	// the queue by hand.
	Recommendations RecommendationsConfig `yaml:"recommendations,omitempty" json:"recommendations,omitempty"`
	// PlanMatch gates the plan_match review perspective
	// (hivecommons/hive#8317), which scores a PR against the approved plan
	// wave its Hive-Run / Hive-Plan trailers name.
	PlanMatch PlanMatchConfig `yaml:"plan_match,omitempty" json:"plan_match,omitempty"`
}

// ContributorPRsConfig is the owner-only gate for contributor PR maintenance.
type ContributorPRsConfig struct {
	// BaseSync enables automatic update-branch attempts for fork PRs that are
	// behind and have "allow edits by maintainers" on. Default off.
	BaseSync bool `yaml:"base_sync,omitempty" json:"base_sync,omitempty"`
}

// PlanMatchConfig is the switch for the plan_match review perspective
// (hivecommons/hive#8317). Off by default: the perspective only earns its cost
// on a hive whose implementation PRs carry run trailers and whose plans live
// in its bead stores. When on, plan_match is appended to the hive's review
// perspective set; a PR without a run trailer gets a not-applicable report
// that neither helps nor hurts its confidence score.
type PlanMatchConfig struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// DuplicateSweepConfig gates the cross-PR duplicate sweep
// (hivecommons/hive#7469, capability B): a periodic pass that clusters open
// PRs by changed-file set and suggests which one to keep.
//
// TWO knobs, both zero-valued off, because they authorize different things.
// Enabled turns the clustering on — that alone only reads, logs and audits,
// which is the dry run an operator should read before the hive speaks on
// contributors' PRs. PostComments is the separate grant that lets it write.
// Neither one ever closes, labels, approves or merges: changed-file identity
// is a candidate generator whose false positives (three dependency bumps
// sharing one manifest; three unrelated fixes to one busy file) are
// indistinguishable from true positives at this layer, so the product is a
// suggestion a human acts on, and there is no code path that acts on it here.
type DuplicateSweepConfig struct {
	// Enabled turns the sweep on in report-only mode.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// PostComments additionally authorizes the suggestion comment. It has no
	// effect unless Enabled is also set — a hive that has not opted into the
	// scan cannot be made to comment by this key alone.
	PostComments bool `yaml:"post_comments,omitempty" json:"post_comments,omitempty"`
	// MaxComments caps comments written per pass. Zero means
	// github.DefaultDuplicateSweepMaxComments.
	MaxComments int `yaml:"max_comments,omitempty" json:"max_comments,omitempty"`
	// MaxPRsPerRepo caps how many open PRs are fingerprinted per repo per
	// pass — the sweep's API budget. Zero means
	// github.DefaultDuplicateSweepMaxPRsPerRepo.
	MaxPRsPerRepo int `yaml:"max_prs_per_repo,omitempty" json:"max_prs_per_repo,omitempty"`
	// BotAuthors are extra logins to treat as a regenerating bot in addition
	// to accounts whose login ends in "[bot]". A bot series gets one summary
	// comment on the newest PR instead of one comment per PR.
	BotAuthors []string `yaml:"bot_authors,omitempty" json:"bot_authors,omitempty"`
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
	// MinHeadAge is the minimum age of a PR head before automerge may trust an
	// unknown required-check set. It closes the post-push window where slow
	// GitHub Actions check-runs have not registered yet. Non-positive values
	// use DefaultAutoMergeMinHeadAge.
	MinHeadAge time.Duration `yaml:"min_head_age,omitempty" json:"min_head_age,omitempty"`
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
	// AllowUnprotectedBase is deprecated and no longer changes merge-request
	// behavior. It remains in the schema so existing configs keep loading; the
	// watcher now merges into any protected or unprotected branch the App can
	// write, subject to the positive CI-evidence gate below.
	AllowUnprotectedBase []string `yaml:"allow_unprotected_base,omitempty" json:"allow_unprotected_base,omitempty"`
	// NoCIOK is the explicit per-repo exception list for repositories that have
	// no CI by design. A listed repo may downgrade the merge-request relay's
	// "zero statuses + zero check runs + zero workflow runs" verdict from
	// unverified to green. Red or pending evidence still refuses/waits.
	NoCIOK []string `yaml:"no_ci_ok,omitempty" json:"no_ci_ok,omitempty"`
	// TrustedBotAuthors lists bot logins whose open PRs the self-authored
	// automerge sweep treats like the App's own: merged once mergeable and
	// CI-green through the identical gates (required checks, hold/exempt
	// labels, intent tier, approval desk, head re-verify). Without this the
	// sweep skipped every non-App PR as "not-app-authored" and a dependency
	// bot burst (26 dependabot PRs in one morning on kubestellar/console) sat
	// green for hours waiting on an agent's capped quick-merge window.
	//
	// nil/unset means DefaultTrustedBotAuthors. An explicit empty list
	// (`trusted_bot_authors: []`) disables the lane. Matched case-insensitively
	// by exact login, e.g. "dependabot[bot]".
	TrustedBotAuthors []string `yaml:"trusted_bot_authors,omitempty" json:"trusted_bot_authors,omitempty"`
}

// DefaultAutoMergeMinHeadAge is the fail-closed post-push quiet period used
// when auto_merge.min_head_age is unset.
const DefaultAutoMergeMinHeadAge = 3 * time.Minute

// DefaultTrustedBotAuthors is the TrustedBotAuthors value when the operator
// declares none. Only dependabot: its PRs are single-dependency bumps whose
// safety is entirely established by the repo's own CI, which the sweep gates on.
var DefaultTrustedBotAuthors = []string{"dependabot[bot]"}

// KnownBotAuthors are dependency/maintenance bots the dashboard offers as
// one-click toggles for TrustedBotAuthors. Only DefaultTrustedBotAuthors are on
// by default; the rest are listed so an operator can enable them without
// having to know the exact login spelling.
var KnownBotAuthors = []string{
	"dependabot[bot]",
	"renovate[bot]",
	"mergeraptor[bot]",
	"pre-commit-ci[bot]",
	"github-actions[bot]",
}

// TrustedBotAuthorSet returns the lower-cased membership set of bot logins the
// self-authored sweep may merge. nil TrustedBotAuthors → DefaultTrustedBotAuthors;
// an explicit empty list → empty set (lane disabled).
func (a AutoMergeConfig) TrustedBotAuthorSet() map[string]bool {
	src := a.TrustedBotAuthors
	if src == nil {
		src = DefaultTrustedBotAuthors
	}
	set := make(map[string]bool, len(src))
	for _, login := range src {
		login = strings.ToLower(strings.TrimSpace(login))
		if login == "" {
			continue
		}
		set[login] = true
	}
	return set
}

func (a AutoMergeConfig) AllowUnprotectedBaseSet() map[string]bool {
	return repoListSet(a.AllowUnprotectedBase)
}

func (a AutoMergeConfig) NoCIOKSet() map[string]bool {
	return repoListSet(a.NoCIOK)
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

// SelfAuthoredEnabled reports whether the App-self-merge sweep is on for this
// hive. Default ON (nil == enabled): see AutoMergeConfig.SelfAuthored.
func (a AutoMergeConfig) SelfAuthoredEnabled() bool {
	return a.SelfAuthored == nil || *a.SelfAuthored
}

// ACMMLevelOrZero returns the configured ACMM level, or 0 when unset. It is the
// convenience the effective-fail-mode and other level-gated lookups use so they
// need not repeat the nil-pointer dance on cfg.ACMMLevel.
func (c *Config) ACMMLevelOrZero() int {
	if c == nil || c.ACMMLevel == nil {
		return 0
	}
	return *c.ACMMLevel
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

// FixHumanPRsEnabled reports whether the review-fix kick may push to PRs the
// hive's own agents did not open. Nil (never set) and an explicit false both
// mean no: pushing to someone else's branch is a power to grant deliberately.
func (r ReviewConfig) FixHumanPRsEnabled() bool {
	return r.FixHumanPRs != nil && *r.FixHumanPRs
}

// reviewFixHumanPRsMigrationLogOnce keeps the upgrade notice to one line per
// process. Load runs at boot and on every config reload, and until the next
// Save materialises the migrated value each reload would repeat it.
var reviewFixHumanPRsMigrationLogOnce sync.Once

// MigrateReviewFixHumanPRs is the upgrade step for review.fix_human_prs
// (hivecommons/hive#8421). Before that setting existed, review.all_authors
// alone made the fixer agent push commits onto every changes_requested PR,
// whoever opened it. A hive that ran that way must keep running that way
// across the upgrade, so:
//
//   - all_authors true and fix_human_prs never set: fix_human_prs becomes an
//     explicit true, logged once, so the dashboard shows it ON and the operator
//     can turn it off deliberately.
//   - anything else (new hives, all_authors off): left nil, which reads as
//     false. An explicit false is never touched.
//
// Every other review field is left exactly as the operator set it. Returns
// true when it changed the config.
func (c *Config) MigrateReviewFixHumanPRs() bool {
	if c == nil || !c.Review.AllAuthors || c.Review.FixHumanPRs != nil {
		return false
	}
	enabled := true
	c.Review.FixHumanPRs = &enabled
	reviewFixHumanPRsMigrationLogOnce.Do(func() {
		log.Printf("[config] migrating review.fix_human_prs: all_authors is on and fix_human_prs was never set, so it is now stored as true to keep this hive pushing review fixes to PRs its agents did not open (as it did before #8421). Turn it off under Features > Review Gate > Reviewers if that is not wanted.")
	})
	return true
}

// EffectiveThreshold resolves the configured threshold with its default.
func (e EscalationConfig) EffectiveThreshold() int {
	if e.Threshold > 0 {
		return e.Threshold
	}
	return DefaultEscalationThreshold
}

// RecommendationsConfig controls the hive's "what should I merge next?" issue.
//
// This exists for a repository whose maintainers do NOT want the hive merging
// anything — the common case when trust is still being earned. A hive that may
// not merge can still do the expensive part of the work: read every open PR,
// establish which ones are actually mergeable right now, and put that answer
// somewhere a person can act on in one sitting. The issue is rewritten in
// place on a cadence, so it is one notification, not a stream.
type RecommendationsConfig struct {
	// Enabled turns the recommendations issue on. Off by default: opening an
	// issue in someone's repository is not something to do uninvited.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repos limits which repositories get an issue. Empty means every
	// repository the hive already watches that has open PRs.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// Labels are applied when the issue is first opened. They are never
	// re-applied on an update, so a maintainer who removes one keeps it
	// removed. The labels must already exist in the repository — a hive does
	// not invent labels in someone else's taxonomy (the same rule
	// HumanDecisionLabel follows).
	Labels []string `yaml:"labels,omitempty" json:"labels,omitempty"`
	// MinReadyToOpen is how many mergeable PRs must exist before the hive
	// opens the issue for the first time. It defaults to 1: an issue that
	// opens saying "nothing is ready" is noise. Once the issue exists it is
	// kept current regardless, including when the answer becomes "nothing is
	// ready right now" — that is a useful state to be able to read.
	MinReadyToOpen int `yaml:"min_ready_to_open,omitempty" json:"min_ready_to_open,omitempty"`
}

func (a AutoMergeConfig) EffectiveMinHeadAge() time.Duration {
	if a.MinHeadAge > 0 {
		return a.MinHeadAge
	}
	return DefaultAutoMergeMinHeadAge
}
