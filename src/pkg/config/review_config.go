package config

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"log"
	"sync"

	"github.com/hivecommons/hive/pkg/modelfamily"
	"github.com/hivecommons/hive/pkg/stageengine/prstage"
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

// ReviewModelSelection is the outcome of ReviewModelsConfig.Select: the
// backend/model a review should run with, and whether that came from the
// configured fallback rather than the pool. Skip is the "skip" fallback with
// no eligible pool entry: no model, and the review is not dispatched.
// RequiresHuman is the "requires_human" fallback in the same situation: the
// pinned model is returned so the caller can still name it, but the review
// should be marked for a person instead of dispatched as independent.
type ReviewModelSelection struct {
	Backend       string
	Model         string
	Fallback      bool
	Skip          bool
	RequiresHuman bool
}

// SelectReviewModel picks the first pool entry of r that is not excluded
// against the PR's author model, exactly as the cadence kick's `review_with=`
// annotation does. pinnedBackend/pinnedModel are the reviewer agent's own
// launch backend and model, returned under the "pinned" (default) fallback.
// Author and candidate models are compared normalized (modelfamily.Normalize),
// and family on the vendor prefix (modelfamily.Family). An empty pool with no
// exclusions configured still returns the pinned model as a fallback, so a
// caller that wants "no instruction" must check Configured() first.
//
// A package function rather than a method because ReviewModelsConfig is
// defined in the stdlib-only pkg/crux/config leaf and the model-family
// normalizer lives on the Hive side (hivecommons/hive#13536).
func SelectReviewModel(r ReviewModelsConfig, authorModel, pinnedBackend, pinnedModel string) ReviewModelSelection {
	author := modelfamily.Normalize(authorModel)
	authorFamily := modelfamily.Family(author)
	for _, entry := range r.Pool {
		candidate := modelfamily.Normalize(entry.Model)
		if r.ExcludeAuthorModelEnabled() && candidate == author {
			continue
		}
		if r.ExcludeAuthorFamily && modelfamily.Family(candidate) == authorFamily {
			continue
		}
		return ReviewModelSelection{Backend: strings.TrimSpace(entry.Backend), Model: strings.TrimSpace(entry.Model)}
	}
	switch r.EffectiveFallback() {
	case ReviewModelsFallbackSkip:
		return ReviewModelSelection{Fallback: true, Skip: true}
	case ReviewModelsFallbackRequiresHuman:
		return ReviewModelSelection{Backend: strings.TrimSpace(pinnedBackend), Model: strings.TrimSpace(pinnedModel), Fallback: true, RequiresHuman: true}
	default:
		return ReviewModelSelection{Backend: strings.TrimSpace(pinnedBackend), Model: strings.TrimSpace(pinnedModel), Fallback: true}
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
	RequireApproval bool `yaml:"require_approval,omitempty" json:"require_approval,omitempty"`
	// Shadow evaluates the approval gate without enforcing it, recording
	// shadow.review_gate.would_have_held on the evidence bundle. It never
	// blocks a merge and never weakens an enabled require_approval.
	Shadow bool `yaml:"shadow,omitempty" json:"shadow,omitempty"`
	FanOut bool `yaml:"fan_out,omitempty" json:"fan_out,omitempty"`
	// EventDriven lets pull_request webhooks trigger review dispatches when
	// the review swarm is otherwise enabled. Nil keeps the safe derived default.
	EventDriven *bool `yaml:"event_driven,omitempty" json:"event_driven,omitempty"`
	// EventDebounceS is the per-PR quiet period before webhook-triggered
	// dispatch. Zero uses DefaultReviewEventDebounceS.
	EventDebounceS     int      `yaml:"event_debounce_s,omitempty" json:"event_debounce_s,omitempty"`
	MaxParallelReviews int      `yaml:"max_parallel_reviews,omitempty" json:"max_parallel_reviews,omitempty"`
	ReviewerAgents     []string `yaml:"reviewer_agents,omitempty" json:"reviewer_agents,omitempty"`
	FixerAgent         string   `yaml:"fixer_agent,omitempty" json:"fixer_agent,omitempty"`
	// Severity controls which review findings block vs. comment/backlog.
	Severity ReviewSeverityConfig `yaml:"severity,omitempty" json:"severity,omitempty"`
	// Backlog controls where non-blocking review findings are filed.
	Backlog ReviewBacklogConfig `yaml:"backlog,omitempty" json:"backlog,omitempty"`
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
	// PerspectiveModels pins the backend/model one perspective's swarm review
	// runs with, keyed by perspective name. An entry replaces the reviewer
	// agent's review_models pool for that perspective only; the agent's
	// exclude_author_model / exclude_author_family / fallback settings still
	// apply to it, so a pinned model that matches the author's family is
	// still refused when exclude_author_family is on. Perspectives not named
	// here use the agent's pool as usual. Empty by default.
	PerspectiveModels map[string]ReviewModelPoolEntry `yaml:"perspective_models,omitempty" json:"perspective_models,omitempty"`
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
	//
	// Recommended on, but deliberately NOT default-on (hivecommons/hive#12126,
	// decision recorded here and in docs/review-swarm.md): a plain bool's
	// zero value is the dashboard toggle's "off", the relay derives its
	// per-head review quota from this flag, combined dispatch sends one
	// reviewer (not the fan-out set), and a single schema slip in a combined
	// verdict discards every perspective at once. Flipping the zero value
	// would change all of that under running hives mid-cycle. Operators opt
	// in explicitly; the example config and docs promote it.
	CombinedPerspectives bool `yaml:"combined_perspectives,omitempty" json:"combined_perspectives,omitempty"`
	// MaxReviewsPerHead caps how many top-level hive reviews one PR head may
	// receive before the relay refuses further ones (revisions and thread
	// replies excepted). 0 derives the cap: 1 with combined_perspectives, one
	// per perspective otherwise. Negative disables the backstop.
	MaxReviewsPerHead int `yaml:"max_reviews_per_head,omitempty" json:"max_reviews_per_head,omitempty"`
	// AllAuthors is accepted for backwards compatibility and does nothing.
	// Every open PR is reviewed regardless of who opened it — hive agent,
	// human, clanker, dependabot or any other bot — whenever review is on
	// (require_approval and fan_out); there is no author-class opt-out below
	// that switch (hivecommons/hive#12974). A set value is logged once as
	// deprecated by WarnDeprecatedReviewAllAuthors. The field stays so that
	// MigrateReviewFixHumanPRs can still read the legacy pairing.
	//
	// Deprecated: review dispatch no longer filters by author.
	AllAuthors bool `yaml:"all_authors,omitempty" json:"all_authors,omitempty"`
	// FixHumanPRs once let the review-fix kick push commits onto PRs that
	// this hive's agents did NOT open: a contributor's fork branch (through
	// "allow edits by maintainers"), a maintainer's branch in the upstream
	// repo, or another bot's PR (hivecommons/hive#8421). It no longer does:
	// the pushed commit is signed off by the hive, not the PR author, and the
	// squash sign-off gate rejects it as bot-signoff, a red only the author
	// can clear (hivecommons/hive#12897). A changes_requested verdict on such
	// a PR stops at the published review, with the fix proposed there as a
	// suggestion or patch block, whichever way this is set. On still makes
	// the review-thread reconciler answer review-bot threads on those PRs,
	// again with suggestions rather than pushes. AllAuthors never implies it.
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
	// PendingTTL is how long a dispatched review may go without a verdict
	// before its dispatch record expires and the perspectives are asked
	// again (Go duration string, default DefaultPendingReviewTTL). A reviewer
	// kick turn that dies — model timeout, proxy read timeout, pod restart —
	// posts no verdict, and before this the pending record made every
	// perspective look covered until someone pushed a new commit
	// (hivecommons/hive#12045). "0" or unset means the default; a negative
	// value disables expiry.
	PendingTTL string `yaml:"pending_ttl,omitempty" json:"pending_ttl,omitempty"`
	// PendingFixTTL is how long a dispatched review fix may go without a new
	// head before its pending row expires and the next bounded attempt is
	// dispatched (Go duration string, default DefaultPendingFixTTL;
	// hivecommons/hive#14604). "0" or unset means the default; a negative
	// value disables expiry.
	PendingFixTTL string `yaml:"pending_fix_ttl,omitempty" json:"pending_fix_ttl,omitempty"`
	// MaxRedispatch caps how many times one PR head's pending reviews may
	// expire before the planner stops re-kicking and records a requires_human
	// hold (reason prefix review-dispatch-exhausted). 0 means
	// DefaultMaxRedispatch.
	MaxRedispatch int `yaml:"max_redispatch,omitempty" json:"max_redispatch,omitempty"`
	// HeadMaxRetries caps how many times one head's review of a perspective
	// may be re-sent when it posts no verdict (agents.<name>.review_heads,
	// hivecommons/hive#13542). 0 means max_redispatch applies.
	HeadMaxRetries int `yaml:"head_max_retries,omitempty" json:"head_max_retries,omitempty"`
	// BotAuthors lists PR author logins whose PRs are reviewed with
	// BotAuthorModel instead of the reviewer's usual model. Entries match
	// case-insensitively; a leading `*` matches any login with that suffix
	// (`*[bot]`). The hive's own GitHub App login never matches. Unset means
	// DefaultReviewBotAuthors.
	BotAuthors []string `yaml:"bot_authors,omitempty" json:"bot_authors,omitempty"`
	// BotAuthorModel is the model (or backend/model) a review of a
	// BotAuthors PR runs with. Empty (the default) leaves the route off. The
	// reviewer agent's engine must be able to run the model.
	BotAuthorModel string `yaml:"bot_author_model,omitempty" json:"bot_author_model,omitempty"`
	// BotAuthorFullReviewPaths are path globs (`**` spans directories) that
	// keep a bot-authored PR on the full-strength model when it touches any of
	// them. Unset means DefaultReviewBotAuthorFullReviewPaths.
	BotAuthorFullReviewPaths []string `yaml:"bot_author_full_review_paths,omitempty" json:"bot_author_full_review_paths,omitempty"`
	// DeltaRereview scopes a re-review to what changed since the head the
	// hive last judged. When a PR already carries a verdict for an earlier
	// head SHA, the new head's kick carries that verdict and its findings
	// and tells the reviewer to read `git diff <prev_head>..<new_head>` plus
	// re-check the flagged items, not the whole PR again. Re-reviews were
	// 53% of one hosted hive's reviewer spend over two weeks (Oct 13-27
	// 2026). nil means on; `delta_rereview: false` turns it off. First
	// reviews, a prior `reject`, and a prior `changes_requested` whose
	// findings are older than delta_max_age stay full reviews.
	DeltaRereview *bool `yaml:"delta_rereview,omitempty" json:"delta_rereview,omitempty"`
	// DeltaTrivialLines is the delta size (added plus deleted lines) under
	// which a re-review skips the model call: when the diff between the
	// reviewed head and the new head is empty, or touches only files the
	// previous verdict had no finding in and is smaller than this, the
	// previous verdict is re-stamped onto the new head with a note. 0 means
	// DefaultReviewDeltaTrivialLines; negative disables the skip.
	DeltaTrivialLines int `yaml:"delta_trivial_lines,omitempty" json:"delta_trivial_lines,omitempty"`
	// DeltaMaxAge is how old a `changes_requested` verdict with findings may
	// be and still anchor a delta re-review (Go duration string). Older than
	// this the new head gets a full review, since the flagged code and the
	// reviewer's memory of it have both moved on. Empty means
	// DefaultReviewDeltaMaxAge.
	DeltaMaxAge string `yaml:"delta_max_age,omitempty" json:"delta_max_age,omitempty"`
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
	// SelfAuthoredDecisionPolicy controls maintainer-call review markers on
	// Hive-authored PRs. "resolve" (default) treats every decision question
	// as advisory and approves unless it matches a review.DecisionRiskClass
	// (permissions/credentials, security, sensitive paths, data loss,
	// merge/trust policy, external side effects, licensing, unverifiable) or
	// carries `<!-- hive-decision-risk: high -->`; "escalate" preserves the
	// old behavior where every decision question parks the PR.
	SelfAuthoredDecisionPolicy string `yaml:"self_authored_decision_policy,omitempty" json:"self_authored_decision_policy,omitempty"`
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
	PriorityLabels bool     `yaml:"priority_labels,omitempty" json:"priority_labels,omitempty"`
	RequireLabels  []string `yaml:"require_labels,omitempty" json:"require_labels,omitempty"`
	// VerdictLabels mirrors the review swarm's state for each PR's current
	// head onto exactly one label (hivecommons/hive#13833): reviewing while
	// the head's review is pending, then the settled verdict. Independent of
	// the automerge engine's stage labels, so a hive with repo auto-merge
	// off still shows the outcome on the PR. Like HumanDecisionLabel the
	// labels are never created; a repo without them is skipped. Labels are
	// informational and never block.
	VerdictLabels ReviewVerdictLabelsConfig `yaml:"verdict_labels,omitempty" json:"verdict_labels,omitzero"`
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
	// Reviewers configures the reviewer agents themselves; see
	// ReviewersConfig and review.reviewers.autoscale (hivecommons/hive#14685).
	Reviewers ReviewersConfig `yaml:"reviewers,omitempty" json:"reviewers,omitzero"`
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
	// Repos carries per-repository auto-merge overrides such as required
	// status checks.
	Repos map[string]AutoMergeRepoConfig `yaml:"repos,omitempty" json:"repos,omitempty"`
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
	// QueueOnApproveAndUnhold is the per-repo opt-in for RFC #13617 Proposal 2
	// (hivecommons/hive#13629): at ACMM L5, when one trusted merger both
	// submits an APPROVED review on an agent PR's current head and removes its
	// level `hold`, the queued sweep queues the PR through the same path as the
	// dashboard's queue-auto-merge button. Nothing else counts as consent.
	// Empty (the default) changes nothing. Entries are owner/repo or bare names.
	QueueOnApproveAndUnhold []string `yaml:"queue_on_approve_and_unhold,omitempty" json:"queue_on_approve_and_unhold,omitempty"`
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
	// BotTakeover lets the sweep re-author a trusted `github-actions[bot]` PR
	// whose required `pull_request` checks can never run because the bot
	// opened it with a workflow GITHUB_TOKEN (#12003). The App pushes the
	// same change as one signed-off commit to `hive/takeover-<n>`, opens a PR
	// that supersedes the bot PR, and closes the bot PR. It applies only when
	// github-actions[bot] is in TrustedBotAuthors.
	BotTakeover BotTakeoverAutoMergeConfig `yaml:"bot_takeover,omitempty" json:"bot_takeover,omitempty"`
	// TrustedAuthors enables a separate, default-off sweep for human-authored
	// PRs whose AUTHOR already holds merge authority. Unlike the App
	// self-authored lane, this never bypasses to create new authority: the
	// author must be in authorized_users at the configured role, and by default
	// GitHub must also report push/maintain/admin permission on the repository.
	TrustedAuthors TrustedAuthorAutoMergeConfig `yaml:"trusted_authors,omitempty" json:"trusted_authors,omitempty"`
	// ContributorLane merges PRs that THIS hive's contributor lane produced
	// for a contributor it dispatched (the linked issue carries the hive's
	// own `hive:claim kind=contributor` marker naming the PR author and this
	// hive) once CI is green, the reviewer verdict approves and the head has
	// soaked. Default ON: the maintainer already trusted the hive to hand the
	// work out, so a green hive-produced PR waiting for a manual `/lgtm` is
	// the stall the lane exists to remove (#11756, #11840 sat green for hours
	// as `untrusted-author-role` / `not-app-authored`). The claim alone only
	// proves a lease, so the tier also requires the marker's contributor to
	// hold an operator-granted tier (require_tiers) and a GitHub login proven
	// against GitHub (#12041); everyone else's PRs wait for a human.
	ContributorLane ContributorLaneAutoMergeConfig `yaml:"contributor_lane,omitempty" json:"contributor_lane,omitempty"`
	// ContributorTier controls automatic contributor trust ratchets used by
	// the contributor lane.
	ContributorTier ContributorTierAutoMergeConfig `yaml:"contributor_tier,omitempty" json:"contributor_tier,omitempty"`
	// HumanMergePaths maps owner/repo to glob patterns (same syntax as
	// intent.guardrail_path_patterns) for paths a person must merge. Unset
	// means no such paths. This key only declares the paths; enforcement
	// lives in the merge sweeps.
	HumanMergePaths map[string][]string `yaml:"human_merge_paths,omitempty" json:"human_merge_paths,omitempty"`
	// Unset uses the default owner-merge rule; explicit empty lists disable
	// their half of the title/label rule.
	HumanMergeTitlePrefixes []string `yaml:"human_merge_title_prefixes,omitempty" json:"human_merge_title_prefixes,omitempty"`
	HumanMergeLabels        []string `yaml:"human_merge_labels,omitempty" json:"human_merge_labels,omitempty"`
	// RequireCurrentBase lists owner/repo names whose automerge sweeps only
	// merge heads containing the current base tip. Explicit entries are
	// strict: inability to update the branch never bypasses the gate.
	RequireCurrentBase []string `yaml:"require_current_base,omitempty" json:"require_current_base,omitempty"`
	// RequireCurrentBaseDefault applies to repos in neither override list.
	// Unset is off. Default-only gating may fall back to the tested head
	// when branch updates are unavailable.
	RequireCurrentBaseDefault *bool `yaml:"require_current_base_default,omitempty" json:"require_current_base_default,omitempty"`
	// SkipCurrentBase opts repos out of the default; explicit on wins.
	SkipCurrentBase []string `yaml:"skip_current_base,omitempty" json:"skip_current_base,omitempty"`
	// MergeForwardTrustedHuman lists owner/repo names where a role/trusted
	// author/human PR that GitHub reports as conflicting ("dirty") gets the
	// base merged into its same-repo head branch as a new App-authored,
	// signed-off merge commit; no human commit is rewritten. A real conflict
	// leaves the branch untouched and posts one note naming the files both
	// sides changed. Fork heads are never written. Empty (the default) keeps
	// the PR parked for its author.
	MergeForwardTrustedHuman []string `yaml:"merge_forward_trusted_human,omitempty" json:"merge_forward_trusted_human,omitempty"`
	// SquashOnly lists owner/repo names whose policy forbids merge commits.
	// Elsewhere a role/trusted PR whose head mixes the contributor's commits
	// with hive commits, each signed off by its own author, is merged with a
	// merge commit (`merge-method/merge`) instead of being parked for a
	// person, because a squash would misattribute the hive's sign-off. In a
	// squash-only repo the lane keeps the squash convention and the PR goes
	// back to its author (or, adopted, to stage/085 naming this policy).
	SquashOnly []string `yaml:"squash_only,omitempty" json:"squash_only,omitempty"`
	// CreditIssueAuthors puts `Co-authored-by: <issue author>` on the commit
	// the hive's merge lands for every human who opened an issue the PR
	// closes (`Fixes|Closes|Resolves #N`), minus the PR author, skipping
	// bots, and never duplicating a trailer the branch already carries
	// (hivecommons/hive#14581). The credit is additive — the PR author
	// still authors the squash, every Signed-off-by is kept — and reaches
	// the PRs external clankers open, which bypass the hive's own
	// open-time attribution. nil/unset means ON; `credit_issue_authors:
	// false` sends the merge with GitHub's default message as before.
	CreditIssueAuthors *bool `yaml:"credit_issue_authors,omitempty" json:"credit_issue_authors,omitempty"`
	// MergeTrain orders approved PRs on the same base whose changed files
	// overlap: one front merges at a time, the siblings wait as
	// "queued-behind" (stage 080, never a human stage) and are updated or
	// rebased right after the front lands instead of each going
	// `conflicting` with nothing owning the repair
	// (hivecommons/hive#15117). Default ON.
	MergeTrain MergeTrainAutoMergeConfig `yaml:"merge_train,omitempty" json:"merge_train,omitempty"`
	// NativeMergeQueue lets the sweep lanes enqueue an eligible PR into
	// GitHub's native merge queue instead of merging it directly when the
	// base branch requires one (hivecommons/hive#15198). Default OFF and
	// opt-in per repo: a hive that never sets it keeps the pre-queue
	// behaviour, including the `merge-queue-required` park.
	NativeMergeQueue NativeMergeQueueAutoMergeConfig `yaml:"native_merge_queue,omitempty" json:"native_merge_queue,omitempty"`
}

type MergeTrainAutoMergeConfig struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// MaxRebaseAttempts bounds rebase-resolve tasks before human escalation.
	MaxRebaseAttempts int `yaml:"max_rebase_attempts,omitempty" json:"max_rebase_attempts,omitempty"`
}

const DefaultMergeTrainMaxRebaseAttempts = 2

func (m MergeTrainAutoMergeConfig) IsEnabled() bool {
	return m.Enabled == nil || *m.Enabled
}

// EffectiveMaxRebaseAttempts applies the default rebase loop guard.
func (m MergeTrainAutoMergeConfig) EffectiveMaxRebaseAttempts() int {
	if m.MaxRebaseAttempts <= 0 {
		return DefaultMergeTrainMaxRebaseAttempts
	}
	return m.MaxRebaseAttempts
}

// CreditIssueAuthorsEnabled reports the auto_merge.credit_issue_authors
// switch; unset is on.
func (a AutoMergeConfig) CreditIssueAuthorsEnabled() bool {
	return a.CreditIssueAuthors == nil || *a.CreditIssueAuthors
}

// RequireCurrentBaseFor resolves explicit on, explicit off, then the default.
func (a AutoMergeConfig) RequireCurrentBaseFor(repo string) bool {
	key := strings.ToLower(strings.TrimSpace(repo))
	if key == "" {
		return false
	}
	if v := a.CurrentBaseOverrideFor(key); v != nil {
		return *v
	}
	return a.CurrentBaseDefaultEnabled()
}

// CurrentBaseDefaultEnabled resolves the hive-wide default (unset is off).
func (a AutoMergeConfig) CurrentBaseDefaultEnabled() bool {
	return a.RequireCurrentBaseDefault != nil && *a.RequireCurrentBaseDefault
}

// CurrentBaseOverrideFor returns explicit on/off, or nil to inherit.
func (a AutoMergeConfig) CurrentBaseOverrideFor(repo string) *bool {
	key := strings.ToLower(strings.TrimSpace(repo))
	if key == "" {
		return nil
	}
	if repoListSet(a.RequireCurrentBase)[key] {
		v := true
		return &v
	}
	if repoListSet(a.SkipCurrentBase)[key] {
		v := false
		return &v
	}
	return nil
}

// SetCurrentBaseOverride records the dashboard's per-repo tri-state,
// removing the repo from the other list (or both when inheriting).
func (a *AutoMergeConfig) SetCurrentBaseOverride(repo string, value *bool) {
	if a == nil {
		return
	}
	repo = strings.TrimSpace(repo)
	if repo == "" {
		return
	}
	drop := func(list []string) []string {
		out := list[:0:0]
		for _, r := range list {
			if !strings.EqualFold(strings.TrimSpace(r), repo) {
				out = append(out, r)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	}
	a.RequireCurrentBase = drop(a.RequireCurrentBase)
	a.SkipCurrentBase = drop(a.SkipCurrentBase)
	switch {
	case value == nil:
	case *value:
		a.RequireCurrentBase = append(a.RequireCurrentBase, repo)
	default:
		a.SkipCurrentBase = append(a.SkipCurrentBase, repo)
	}
}

// RequireCurrentBaseStrictFor reports explicit on, which must not fall back
// to an un-updated head when update-branch is unavailable.
func (a AutoMergeConfig) RequireCurrentBaseStrictFor(repo string) bool {
	return repoListSet(a.RequireCurrentBase)[strings.ToLower(strings.TrimSpace(repo))]
}

// HumanMergeTitlePrefixesEffective preserves explicit empty lists.
func (a AutoMergeConfig) HumanMergeTitlePrefixesEffective() []string {
	if a.HumanMergeTitlePrefixes == nil {
		return append([]string(nil), prstage.DefaultHumanMergeTitlePrefixes...)
	}
	return append(make([]string, 0, len(a.HumanMergeTitlePrefixes)), a.HumanMergeTitlePrefixes...)
}

// HumanMergeLabelsEffective preserves explicit empty lists.
func (a AutoMergeConfig) HumanMergeLabelsEffective() []string {
	if a.HumanMergeLabels == nil {
		return append([]string(nil), prstage.DefaultHumanMergeLabels...)
	}
	return append(make([]string, 0, len(a.HumanMergeLabels)), a.HumanMergeLabels...)
}

// HumanMergeRule is the shared rule for the automerge lanes and dashboard.
func (a AutoMergeConfig) HumanMergeRule() prstage.HumanMergeRule {
	return prstage.HumanMergeRule{TitlePrefixes: a.HumanMergeTitlePrefixesEffective(), Labels: a.HumanMergeLabelsEffective()}
}

// MergeForwardTrustedHumanFor reports whether repo (owner/repo, matched
// case-insensitively) is listed in auto_merge.merge_forward_trusted_human.
func (a AutoMergeConfig) MergeForwardTrustedHumanFor(repo string) bool {
	return repoListSet(a.MergeForwardTrustedHuman)[strings.ToLower(strings.TrimSpace(repo))]
}

// SquashOnlyFor reports whether repo (owner/repo, matched case-insensitively)
// is listed in auto_merge.squash_only.
func (a AutoMergeConfig) SquashOnlyFor(repo string) bool {
	return repoListSet(a.SquashOnly)[strings.ToLower(strings.TrimSpace(repo))]
}

// SupersessionSweepConfig tunes the supersession sweep
// (github.SweepSupersededOpenPRs), which finds open PRs whose claimed issue was
// already closed by a different merged PR (hivecommons/hive#11418).
type SupersessionSweepConfig struct {
	// CloseContributorPRs authorizes closing superseded human-authored PRs
	// after the grace window. Default off.
	CloseContributorPRs bool `yaml:"close_contributor_prs,omitempty" json:"close_contributor_prs,omitempty"`
	// GracePeriod is how long a human-authored PR stays open after the
	// supersession notice. Non-positive means the sweep default.
	GracePeriod time.Duration `yaml:"grace_period,omitempty" json:"grace_period,omitempty"`
}

// SharedCIRerunConfig gates the shared-CI rerun sweep
// (github.SweepSharedCIReruns): when a `[shared-ci]` incident issue closes,
// Hive re-runs the failed jobs of every open PR stamped with that incident's
// `<!-- hive-shared-ci-<n> -->` marker, once per PR per incident.
type SharedCIRerunConfig struct {
	// Enabled is *bool so unset means on; set `shared_ci_rerun.enabled: false`
	// to disable.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

// IsEnabled reports whether the sweep runs; unset means on.
func (c SharedCIRerunConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// AutoMergeRepoConfig carries per-repository auto-merge overrides.
type AutoMergeRepoConfig struct {
	// RequiredChecks overrides auto_merge.required_checks for this repository.
	// An explicit empty list means the repo declares no required checks; an
	// omitted field falls back to the global default.
	RequiredChecks []string `yaml:"required_checks,omitempty" json:"required_checks,omitempty"`
}

// BotTakeoverAutoMergeConfig is auto_merge.bot_takeover.*.
type BotTakeoverAutoMergeConfig struct {
	// Enabled turns the takeover on. nil/unset means ON; an explicit
	// `enabled: false` disables it.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
}

func (b BotTakeoverAutoMergeConfig) IsEnabled() bool {
	return b.Enabled == nil || *b.Enabled
}

type TrustedAuthorAutoMergeConfig struct {
	Enabled                 bool     `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	Repos                   []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	RequireRole             string   `yaml:"require_role,omitempty" json:"require_role,omitempty"`
	RequireGitHubPermission *bool    `yaml:"require_github_permission,omitempty" json:"require_github_permission,omitempty"`
	ExcludeLabels           []string `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
}

// ContributorLaneAutoMergeConfig is the policy for the contributor-lane
// auto-merge tier (auto_merge.contributor_lane.*). Every field is optional;
// the zero value is the default-on policy described on each field.
type ContributorLaneAutoMergeConfig struct {
	// Enabled turns the tier on. nil/unset means ON (see
	// AutoMergeConfig.ContributorLane for why the default is on); an explicit
	// `enabled: false` disables it.
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// SoakMinutes is how long the head SHA must stay unchanged and green
	// before the tier merges. 0/unset means DefaultContributorLaneSoakMinutes.
	SoakMinutes int `yaml:"soak_minutes,omitempty" json:"soak_minutes,omitempty"`
	// RequireReviewerVerdict requires an approving aggregate reviewer verdict
	// for the current head whenever a reviewer agent is enabled. nil/unset
	// means ON. A hive with no reviewer agent satisfies the gate vacuously.
	RequireReviewerVerdict *bool `yaml:"require_reviewer_verdict,omitempty" json:"require_reviewer_verdict,omitempty"`
	// Repos restricts the tier to these repositories (bare name or
	// owner/repo). Empty means every watched repository.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// ExcludeLabels keeps a PR out of the tier. nil means
	// DefaultContributorLaneExcludeLabels.
	ExcludeLabels []string `yaml:"exclude_labels,omitempty" json:"exclude_labels,omitempty"`
	// RequireTiers lists the contributor trust tiers whose claimed PRs the
	// tier may merge; the tier= stamped on the hive:claim marker must be one
	// of them. nil/empty means DefaultContributorLaneRequireTiers. Only
	// operator-granted tiers are accepted (ContributorLaneGrantableTiers):
	// newcomer and contributor are reached by self-registration and
	// automatic promotion, so they never confer unattended merge.
	RequireTiers []string `yaml:"require_tiers,omitempty" json:"require_tiers,omitempty"`
	// RegistryTiersSufficient admits a PR whose author holds a RequireTiers
	// tier in this hive's contributor registry (the operator-set trust tier
	// on the contributor profile) without a linked issue or hive:claim
	// marker. nil/unset means ON. With it off, only the claim-marker path
	// admits a PR, so a trusted contributor's PR that references rather
	// than closes its issue, or was claimed outside the relay, waits for a
	// human (`contributor-lane:no-linked-issue` / `no-claim`).
	RegistryTiersSufficient *bool `yaml:"registry_tiers_sufficient,omitempty" json:"registry_tiers_sufficient,omitempty"`
	// AutoPromotionScope is where the clean-merged-PR ratchet
	// (auto_merge.contributor_tier.auto_promote_after_merged) counts an
	// author's merges. `hive` (default) counts clean merged PRs across every
	// repository this hive manages: trust in a contributor is a property of
	// the person, and the small satellite repos of a multi-repo hive never
	// reach a per-repo threshold on their own. `repo` counts only the PR's
	// own repository (the pre-#14517 behaviour). Revert and sentinel-alert
	// evidence is applied per PR in either scope.
	AutoPromotionScope string `yaml:"auto_promotion_scope,omitempty" json:"auto_promotion_scope,omitempty"`
	// ApproveHeldRunsForRegistryTrusted lets the contributor lane approve
	// `pull_request` workflow runs GitHub holds for approval
	// (`action_required`) on a PR whose author the registry path admitted at
	// a RequireTiers tier, once per head, when the only thing between the PR
	// and merge is a required check those held runs would produce. Authors
	// admitted by claim marker, auto-promotion or association are never
	// approved for. nil/unset means ON. Needs the App's Actions: Read and
	// write permission; without it the approve call fails and the lane keeps
	// waiting for a person.
	ApproveHeldRunsForRegistryTrusted *bool `yaml:"approve_held_runs_for_registry_trusted,omitempty" json:"approve_held_runs_for_registry_trusted,omitempty"`
	// OwnerApprovalReleasesWorkflowFence lets a hive owner / trusted
	// merger's explicit approval on the current head — a GitHub APPROVED
	// review, or a dashboard / `/hive decision approve` — release the
	// contributor lane's workflow-path fence, so the lane merges a PR that
	// edits `.github/workflows/**` or `.github/actions/**` itself once the
	// other gates are clear (hivecommons/hive#14664). Approval on an earlier
	// head does not count. nil/unset means ON. Merging such a PR needs the
	// App's Workflows: Read and write permission; without it the lane marks
	// the PR hive:unauthorized and parks it for a person's merge click.
	// false keeps every workflow-path PR a person's merge.
	OwnerApprovalReleasesWorkflowFence *bool `yaml:"owner_approval_releases_workflow_fence,omitempty" json:"owner_approval_releases_workflow_fence,omitempty"`
}

// Contributor-lane auto-promotion scopes
// (auto_merge.contributor_lane.auto_promotion_scope).
const (
	ContributorLaneAutoPromotionScopeHive = "hive"
	ContributorLaneAutoPromotionScopeRepo = "repo"
)

// ContributorLaneAutoPromotionScopes lists the accepted
// auto_promotion_scope values.
var ContributorLaneAutoPromotionScopes = []string{ContributorLaneAutoPromotionScopeHive, ContributorLaneAutoPromotionScopeRepo}

// EffectiveAutoPromotionScope returns the normalized auto_promotion_scope,
// defaulting to ContributorLaneAutoPromotionScopeHive.
func (c ContributorLaneAutoMergeConfig) EffectiveAutoPromotionScope() string {
	scope := strings.ToLower(strings.TrimSpace(c.AutoPromotionScope))
	if scope == "" {
		return ContributorLaneAutoPromotionScopeHive
	}
	return scope
}

// ContributorTierAutoMergeConfig is auto_merge.contributor_tier.*.
type ContributorTierAutoMergeConfig struct {
	// AutoPromoteAfterMerged lets the contributor lane conservatively treat a
	// contributor as trusted after this many recently merged PRs by the same
	// author in the repository, provided that window has no reverts or sentinel
	// alerts — including reverts of those PRs merged by someone else — and
	// the author is not `revoked` in the contributor registry. 0/unset means
	// DefaultContributorTierAutoPromoteAfterMerged;
	// negative disables the ratchet.
	AutoPromoteAfterMerged int `yaml:"auto_promote_after_merged,omitempty" json:"auto_promote_after_merged,omitempty"`
}

// DefaultContributorTierAutoPromoteAfterMerged is the clean merged-PR count
// needed before the contributor lane trusts a contributor automatically.
const DefaultContributorTierAutoPromoteAfterMerged = 5

func (c ContributorTierAutoMergeConfig) EffectiveAutoPromoteAfterMerged() int {
	if c.AutoPromoteAfterMerged < 0 {
		return 0
	}
	if c.AutoPromoteAfterMerged == 0 {
		return DefaultContributorTierAutoPromoteAfterMerged
	}
	return c.AutoPromoteAfterMerged
}

// DefaultContributorLaneRequireTiers is the contributor-lane tier allow-list
// when auto_merge.contributor_lane.require_tiers is unset.
var DefaultContributorLaneRequireTiers = []string{"trusted", "merger"}

// ContributorLaneGrantableTiers are the contributor trust tiers only an
// operator can grant, and so the only values require_tiers accepts.
var ContributorLaneGrantableTiers = []string{"trusted", "merger", "advisor"}

// DefaultContributorLaneSoakMinutes is the contributor-lane soak when
// auto_merge.contributor_lane.soak_minutes is unset: thirty minutes of
// unchanged, green head gives a maintainer who does want to look a window
// without parking reviewed work for hours.
const DefaultContributorLaneSoakMinutes = 30

// DefaultContributorLaneExcludeLabels is the contributor-lane exclude set when
// auto_merge.contributor_lane.exclude_labels is unset.
var DefaultContributorLaneExcludeLabels = []string{"hold", "do-not-merge", "needs-human", "blocked", "needs-rebase"}

func (c ContributorLaneAutoMergeConfig) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

func (c ContributorLaneAutoMergeConfig) EffectiveSoakMinutes() int {
	if c.SoakMinutes <= 0 {
		return DefaultContributorLaneSoakMinutes
	}
	return c.SoakMinutes
}

func (c ContributorLaneAutoMergeConfig) EffectiveRequireReviewerVerdict() bool {
	return c.RequireReviewerVerdict == nil || *c.RequireReviewerVerdict
}

// EffectiveRegistryTiersSufficient reports whether a RequireTiers tier in
// the contributor registry admits a PR without a claim marker (default on).
func (c ContributorLaneAutoMergeConfig) EffectiveRegistryTiersSufficient() bool {
	return c.RegistryTiersSufficient == nil || *c.RegistryTiersSufficient
}

// EffectiveApproveHeldRunsForRegistryTrusted reports whether the contributor
// lane may approve held workflow runs for registry-admitted authors (default on).
func (c ContributorLaneAutoMergeConfig) EffectiveApproveHeldRunsForRegistryTrusted() bool {
	return c.ApproveHeldRunsForRegistryTrusted == nil || *c.ApproveHeldRunsForRegistryTrusted
}

// EffectiveOwnerApprovalReleasesWorkflowFence reports whether a trusted
// merger's explicit approval on the current head lets the contributor lane
// merge a workflow-path PR (default on).
func (c ContributorLaneAutoMergeConfig) EffectiveOwnerApprovalReleasesWorkflowFence() bool {
	return c.OwnerApprovalReleasesWorkflowFence == nil || *c.OwnerApprovalReleasesWorkflowFence
}

func (c ContributorLaneAutoMergeConfig) RepoSet() map[string]bool {
	return repoListSet(c.Repos)
}

func (c ContributorLaneAutoMergeConfig) ExcludeLabelSet() map[string]bool {
	labels := c.ExcludeLabels
	if labels == nil {
		labels = DefaultContributorLaneExcludeLabels
	}
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		set[label] = true
	}
	return set
}

// RequireTierSet returns the normalized require_tiers set, falling back to
// DefaultContributorLaneRequireTiers when none is configured.
func (c ContributorLaneAutoMergeConfig) RequireTierSet() map[string]bool {
	tiers := c.RequireTiers
	set := make(map[string]bool, len(tiers))
	for _, tier := range tiers {
		tier = strings.ToLower(strings.TrimSpace(tier))
		if tier != "" {
			set[tier] = true
		}
	}
	if len(set) == 0 {
		for _, tier := range DefaultContributorLaneRequireTiers {
			set[tier] = true
		}
	}
	return set
}

// EffectiveRequireTiers returns RequireTierSet as a sorted list for display.
func (c ContributorLaneAutoMergeConfig) EffectiveRequireTiers() []string {
	set := c.RequireTierSet()
	out := make([]string, 0, len(set))
	for tier := range set {
		out = append(out, tier)
	}
	sort.Strings(out)
	return out
}

// IsContributorLaneGrantableTier reports whether tier is one an operator
// must grant (see ContributorLaneGrantableTiers).
func IsContributorLaneGrantableTier(tier string) bool {
	tier = strings.ToLower(strings.TrimSpace(tier))
	for _, t := range ContributorLaneGrantableTiers {
		if tier == t {
			return true
		}
	}
	return false
}

func (c ContributorLaneAutoMergeConfig) Validate() error {
	if c.SoakMinutes < 0 {
		return fmt.Errorf("auto_merge.contributor_lane.soak_minutes must be 0 (default 30) or greater, got %d", c.SoakMinutes)
	}
	for _, tier := range c.RequireTiers {
		if !IsContributorLaneGrantableTier(tier) {
			return fmt.Errorf("auto_merge.contributor_lane.require_tiers: %q is not an operator-granted tier (allowed: %s)", tier, strings.Join(ContributorLaneGrantableTiers, ", "))
		}
	}
	if scope := c.EffectiveAutoPromotionScope(); scope != ContributorLaneAutoPromotionScopeHive && scope != ContributorLaneAutoPromotionScopeRepo {
		return fmt.Errorf("auto_merge.contributor_lane.auto_promotion_scope: %q is not valid (allowed: %s)", c.AutoPromotionScope, strings.Join(ContributorLaneAutoPromotionScopes, ", "))
	}
	return nil
}

func (c ContributorTierAutoMergeConfig) Validate() error {
	return nil
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

var DefaultTrustedAuthorExcludeLabels = []string{"hold", "do-not-merge", "needs-human"}

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

func (t TrustedAuthorAutoMergeConfig) EffectiveRequireRole() string {
	role := strings.ToLower(strings.TrimSpace(t.RequireRole))
	if role == "" {
		return RoleMerger
	}
	return role
}

func (t TrustedAuthorAutoMergeConfig) EffectiveRequireGitHubPermission() bool {
	return t.RequireGitHubPermission == nil || *t.RequireGitHubPermission
}

func (t TrustedAuthorAutoMergeConfig) RepoSet() map[string]bool {
	return repoListSet(t.Repos)
}

func (t TrustedAuthorAutoMergeConfig) ExcludeLabelSet() map[string]bool {
	labels := t.ExcludeLabels
	if labels == nil {
		labels = DefaultTrustedAuthorExcludeLabels
	}
	set := make(map[string]bool, len(labels))
	for _, label := range labels {
		label = strings.ToLower(strings.TrimSpace(label))
		if label == "" {
			continue
		}
		set[label] = true
	}
	return set
}

func (t TrustedAuthorAutoMergeConfig) Validate() error {
	if !t.Enabled {
		return nil
	}
	role := t.EffectiveRequireRole()
	if role != RoleMerger && role != RoleOwner {
		return fmt.Errorf("auto_merge.trusted_authors.require_role must be %q or %q, got %q", RoleMerger, RoleOwner, t.RequireRole)
	}
	return nil
}

func (a AutoMergeConfig) AllowUnprotectedBaseSet() map[string]bool {
	return repoListSet(a.AllowUnprotectedBase)
}

func (a AutoMergeConfig) NoCIOKSet() map[string]bool {
	return repoListSet(a.NoCIOK)
}

// QueueOnApproveAndUnholdACMMLevel is the only ACMM level at which
// auto_merge.queue_on_approve_and_unhold applies (hivecommons/hive#13629).
const QueueOnApproveAndUnholdACMMLevel = 5

// QueueOnApproveAndUnholdAllowed reports whether repo (owner/repo or bare
// name, matched case-insensitively) opted into queueing on a trusted merger's
// approval plus level-hold removal, and the hive runs at
// QueueOnApproveAndUnholdACMMLevel. An unset level fails closed.
func (a AutoMergeConfig) QueueOnApproveAndUnholdAllowed(repo string, acmmLevel *int) bool {
	if acmmLevel == nil || *acmmLevel != QueueOnApproveAndUnholdACMMLevel {
		return false
	}
	set := repoListSet(a.QueueOnApproveAndUnhold)
	if len(set) == 0 {
		return false
	}
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return false
	}
	if set[repo] {
		return true
	}
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return set[repo[i+1:]]
	}
	return false
}

// HumanMergePathsFor returns the human-merge path patterns configured for repo
// (owner/repo, matched case-insensitively), or nil when none are set.
func (a AutoMergeConfig) HumanMergePathsFor(repo string) []string {
	repo = strings.TrimSpace(repo)
	for k, patterns := range a.HumanMergePaths {
		if strings.EqualFold(strings.TrimSpace(k), repo) && len(patterns) > 0 {
			return patterns
		}
	}
	return nil
}

// RequiredCheckSet returns the global config-declared required-status-check set
// as a membership map, and whether the global config actually declared one.
// Empty/unset RequiredChecks returns (nil, false) — "not config-declared".
func (a AutoMergeConfig) RequiredCheckSet() (map[string]bool, bool) {
	return requiredCheckSetFromList(a.RequiredChecks, false)
}

// RequiredCheckSetForRepo returns the per-repo required-check override for repo
// when one is configured, otherwise the global RequiredCheckSet. Repo may be a
// bare name or owner/repo; configured keys are matched case-insensitively by
// exact owner/repo or by bare repo name. An explicit per-repo empty list is a
// known empty set.
func (a AutoMergeConfig) RequiredCheckSetForRepo(repo string) (map[string]bool, bool) {
	repo = strings.TrimSpace(repo)
	bare := repo
	if i := strings.LastIndex(bare, "/"); i >= 0 {
		bare = bare[i+1:]
	}
	for key, cfg := range a.Repos {
		if strings.EqualFold(strings.TrimSpace(key), repo) {
			return requiredCheckSetFromRepoConfig(a, cfg)
		}
	}
	for key, cfg := range a.Repos {
		kBare := strings.TrimSpace(key)
		if i := strings.LastIndex(kBare, "/"); i >= 0 {
			kBare = kBare[i+1:]
		}
		if bare != "" && strings.EqualFold(kBare, bare) {
			return requiredCheckSetFromRepoConfig(a, cfg)
		}
	}
	return a.RequiredCheckSet()
}

func requiredCheckSetFromRepoConfig(a AutoMergeConfig, cfg AutoMergeRepoConfig) (map[string]bool, bool) {
	if cfg.RequiredChecks == nil {
		return a.RequiredCheckSet()
	}
	return requiredCheckSetFromList(cfg.RequiredChecks, true)
}

func requiredCheckSetFromList(checks []string, declaredEmptyKnown bool) (map[string]bool, bool) {
	if len(checks) == 0 {
		if declaredEmptyKnown {
			return map[string]bool{}, true
		}
		return nil, false
	}
	set := make(map[string]bool, len(checks))
	for _, name := range checks {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		set[name] = true
	}
	if len(set) == 0 && !declaredEmptyKnown {
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

// FixHumanPRsEnabled reports whether review.fix_human_prs is on for PRs the
// hive's own agents did not open (see FixHumanPRs: it no longer permits a
// push). Nil (never set) and an explicit false both
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
// whoever opened it. The stored value is kept for audit/UI continuity even
// though fix dispatch now still requires hive authorship, so:
//
//   - all_authors true and fix_human_prs never set: fix_human_prs becomes an
//     explicit true, logged once, so the dashboard shows the legacy state and
//     the operator can turn it off deliberately.
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
		log.Printf("[config] migrating review.fix_human_prs: all_authors is on and fix_human_prs was never set, so it is now stored as true for audit/UI continuity. Review-fix pushes still require hive authorship; turn the legacy setting off under Features > Review Gate > Reviewers if it is not wanted.")
	})
	return true
}

// reviewAllAuthorsDeprecationLogOnce keeps the deprecation notice to one
// line per process across boot and every reload.
var reviewAllAuthorsDeprecationLogOnce sync.Once

// WarnDeprecatedReviewAllAuthors logs once when review.all_authors is set.
// The knob is a no-op: every open PR is reviewed whenever review is on
// (hivecommons/hive#12974). The value is left in place for audit/UI
// continuity and for MigrateReviewFixHumanPRs. Returns true when the knob
// is set.
func (c *Config) WarnDeprecatedReviewAllAuthors() bool {
	if c == nil || !c.Review.AllAuthors {
		return false
	}
	reviewAllAuthorsDeprecationLogOnce.Do(func() {
		log.Printf("[config] review.all_authors is deprecated and has no effect: the review swarm reviews every open PR regardless of author whenever review is on (require_approval and fan_out). Remove it from hive.yaml.")
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

// NativeMergeQueueAutoMergeConfig is auto_merge.native_merge_queue.*.
type NativeMergeQueueAutoMergeConfig struct {
	// Enabled turns the native-queue hand-off on. Default off.
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty"`
	// Repos restricts the hand-off to these repositories (owner/repo,
	// case-insensitive). Empty means every watched repository once Enabled.
	Repos []string `yaml:"repos,omitempty" json:"repos,omitempty"`
	// SkipCurrentBaseGate lets a queue-enabled base bypass the
	// auto_merge.require_current_base gate and the behind update-branch,
	// because the merge group re-tests the merged result and a new head
	// would drop the PR from the queue. Default off: the gate stays in
	// force on queue bases exactly as on any other base.
	SkipCurrentBaseGate bool `yaml:"skip_current_base_gate,omitempty" json:"skip_current_base_gate,omitempty"`
	// ProbeTTL bounds how long a base branch's "has a merge queue" answer
	// is reused before it is re-read from GraphQL. 0/unset means
	// DefaultNativeMergeQueueProbeTTL.
	ProbeTTL time.Duration `yaml:"probe_ttl,omitempty" json:"probe_ttl,omitempty"`
}

// DefaultNativeMergeQueueProbeTTL is how long a base branch's merge-queue
// probe is cached when auto_merge.native_merge_queue.probe_ttl is unset.
const DefaultNativeMergeQueueProbeTTL = 30 * time.Minute

// RepoSet is auto_merge.native_merge_queue.repos as a lower-cased membership
// map; nil means the allow-list is empty (every repo once Enabled).
func (n NativeMergeQueueAutoMergeConfig) RepoSet() map[string]bool {
	return repoListSet(n.Repos)
}

// EnabledFor reports whether repo (owner/repo, matched case-insensitively)
// may hand PRs to a native merge queue: Enabled and either no allow-list or
// listed in it.
func (n NativeMergeQueueAutoMergeConfig) EnabledFor(repo string) bool {
	if !n.Enabled {
		return false
	}
	set := n.RepoSet()
	if len(set) == 0 {
		return true
	}
	return set[strings.ToLower(strings.TrimSpace(repo))]
}

// EffectiveProbeTTL is probe_ttl with the default applied.
func (n NativeMergeQueueAutoMergeConfig) EffectiveProbeTTL() time.Duration {
	if n.ProbeTTL <= 0 {
		return DefaultNativeMergeQueueProbeTTL
	}
	return n.ProbeTTL
}
