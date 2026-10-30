package review

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const (
	DefaultMaxParallelReviews = 5
	ReviewDispatchStateFile   = "review-dispatch-state.json"
	RecentDispatchTTL         = 24 * time.Hour
	// DefaultDispatchStateDir is the durable data dir. It matches the path the
	// knowledge engine and the dashboard config overlay already persist to,
	// which on a hosted spoke is the PersistentVolumeClaim.
	DefaultDispatchStateDir = "/data"
	DefaultFixerAgent       = "scanner"
	reviewRoleToken         = "review"
	// DefaultPendingReviewTTL is how long a dispatched review may sit without
	// a verdict before its pending entry expires and the perspectives are
	// re-dispatched (review.pending_ttl). A kick turn that dies — model
	// timeout, proxy read timeout, pod restart — leaves no verdict and no
	// error, and without an expiry its pending entries made every perspective
	// look covered forever (hivecommons/hive#12045 sat 3.5h at queue
	// position 1).
	DefaultPendingReviewTTL = 45 * time.Minute
	// DefaultMaxRedispatch is how many times the same head may have its
	// pending reviews expire before the planner stops re-kicking and hands
	// the PR to a human (review.max_redispatch).
	DefaultMaxRedispatch = 3
	// DefaultPendingFixTTL is how long a dispatched fix may sit without a new
	// head before its pending row expires and the next bounded attempt is
	// dispatched. A fixer turn that never pushes leaves no signal, so without
	// an expiry the row excluded the PR forever (hivecommons/hive#14604).
	DefaultPendingFixTTL = 2 * time.Hour
	// HoldReasonDispatchExhausted prefixes the requires_human reason recorded
	// when a head's reviews expired DefaultMaxRedispatch times.
	HoldReasonDispatchExhausted = "review-dispatch-exhausted"
)

// ReviewDispatchStatePath is where the review swarm records which
// (repo, number, headSHA, perspective) tuples it has already dispatched. It is
// NOT a cache: it is the reviewer's only memory of what it has already looked
// at. PlanDispatch keeps no cursor — it re-walks the actionable PR list from
// the top every cycle and relies on this file to skip the PRs it already
// dispatched, so the parallel budget lands on the NEXT PRs in the queue.
//
// It therefore lives on the durable data dir, not under AgentReportDir
// (/var/run/hive-metrics), which is scratch space for regenerable per-cycle
// artifacts — actionable.json, tokens.json, github-cache.json — on the
// container's ephemeral writable layer. Storing dispatch state there meant
// every pod restart wiped the reviewer's memory: it re-walked the queue from
// the top, re-reviewed the same first PRs, and never advanced to the rest of
// the queue. Once reviewers publish comments, that also re-posts on those same
// PRs. A var (not const) so tests can point it at a temp dir.
var ReviewDispatchStatePath = filepath.Join(DefaultDispatchStateDir, ReviewDispatchStateFile)

// LegacyReviewDispatchStatePath is the pre-migration location. LoadDispatchState
// falls back to it so a hive upgrading in place keeps the state it already has
// instead of restarting its sweep of the queue from the top.
var LegacyReviewDispatchStatePath = filepath.Join(outputschema.AgentReportDir, ReviewDispatchStateFile)

type AgentCapability struct {
	Name string
	// ReplicaOf is the base agent a derived replica was expanded from.
	ReplicaOf      string
	Enabled        bool
	Paused         bool
	OnDemand       bool
	UsesKick       bool
	Role           string
	LaneKeywords   []string
	DetectKeywords []string
	Aliases        []string
	// Backend/Model are the agent's own launch backend and model: what the
	// "pinned" review_models fallback returns, and what a verdict with no
	// review_model of its own is attributed to.
	Backend string
	Model   string
	// ReviewModels is agents.<name>.review_models. The swarm applies it per
	// perspective kick exactly as the cadence kick applies it per PR line.
	ReviewModels config.ReviewModelsConfig
	// ReviewHead is the head key when this agent was derived from a base's
	// review_heads (config.AgentConfig.ReviewHead), and ReviewHeadOf that
	// base. HasReviewHeads marks the base itself and its plain replicas: the
	// agents that declare heads and are kept out of head dispatch
	// (hivecommons/hive#13542).
	ReviewHead     string
	ReviewHeadOf   string
	HasReviewHeads bool
	// Draining marks a reviewer the autoscaler is retiring
	// (hivecommons/hive#14685): it finishes what it has but is handed no
	// new review, exactly as if it were paused.
	Draining bool
}

// CapabilityFromConfig builds the dispatcher's view of one configured lane
// from its agents.<name> entry. Every site that hands an agent to the review
// package goes through here so a field added to AgentCapability is populated
// everywhere at once. Callers layer runtime state (an agent-manager pause) on
// top of the result.
func CapabilityFromConfig(name string, ac config.AgentConfig) AgentCapability {
	return AgentCapability{
		Name:           name,
		ReplicaOf:      ac.ReplicaOf,
		Enabled:        ac.Enabled,
		Paused:         ac.Paused,
		OnDemand:       ac.OnDemand,
		UsesKick:       ac.UsesGovernorKick(),
		Role:           ac.Role,
		LaneKeywords:   ac.LaneKeywords,
		DetectKeywords: ac.DetectKeywords,
		Aliases:        ac.Aliases,
		Backend:        ac.Backend,
		Model:          ac.Model,
		ReviewModels:   ac.ReviewModels,
		ReviewHead:     ac.ReviewHead,
		ReviewHeadOf:   reviewHeadBase(ac),
		HasReviewHeads: len(ac.ReviewHeads) > 0 && !ac.IsReviewHead(),
	}
}

// reviewHeadBase is the base agent a review head was derived from, so that
// review.reviewer_agents naming the base admits its heads
// (hivecommons/hive#13542). Empty for every other agent.
func reviewHeadBase(ac config.AgentConfig) string {
	if ac.IsReviewHead() {
		return ac.ReplicaOf
	}
	return ""
}

// baseOf is the lane an agent's reviewer identity resolves to: the base agent
// for a replica, otherwise the agent itself (config.AgentConfig.BaseName).
func baseOf(a AgentCapability) string {
	if a.ReplicaOf != "" {
		return a.ReplicaOf
	}
	return a.Name
}

type DispatchOptions struct {
	RequireApproval    bool
	FanOut             bool
	MaxParallelReviews int
	// MaxPerspectivesPerPR caps how many perspectives one PR receives for a
	// given head SHA, across cycles rather than within a single one. Each
	// perspective is a separate review comment, so this is the control over
	// how much review traffic a single pull request attracts.
	// Zero means DefaultMaxPerspectivesPerPR.
	MaxPerspectivesPerPR int
	// Perspectives is the set this hive reviews with, including any it
	// defines itself. The zero value means the built-in defaults.
	Perspectives PerspectiveSet
	// CombinedPerspectives dispatches ONE kick covering every perspective a PR
	// still needs, instead of one kick per perspective.
	//
	// It exists because breadth and quiet were in direct conflict. One kick
	// per perspective meant five agent sessions and five review comments on a
	// single PR, and capping that (max_perspectives_per_pr, #7562) bought
	// quiet by never reviewing four of the five perspectives at all. Reviewing
	// all of them in one session costs one comment, and reads the diff and its
	// surrounding tree once rather than five times.
	//
	// The verdicts stay separate: the reviewer emits one per perspective, and
	// any single one of them can still withhold approval.
	CombinedPerspectives bool
	ReviewerAgents       []string
	FixerAgent           string
	ProjectOrg           string
	AIAuthor             string
	// PostComments carries config.ReviewConfig.PostComments into the prompt
	// builder, so reviewers are told to publish their verdict on the PR.
	PostComments bool
	// WritingGuideSection carries project.writing_guide into review-swarm
	// prompt builders, matching template-based issue/PR filing kicks.
	WritingGuideSection string
	// ReviewBotLogins carries classification.review_bots.logins into the
	// prompt builder, so reviewers read and answer those bots' unresolved
	// threads (hivecommons/hive#9360).
	ReviewBotLogins      []string
	ReviewBotMinPriority string
	// PerspectiveModels is review.perspective_models: a backend/model that
	// replaces the reviewer's review_models pool for the named perspective.
	// The reviewer's exclusions and fallback still apply to it.
	PerspectiveModels map[string]config.ReviewModelPoolEntry
	// BotAuthorRoute is review.bot_authors / bot_author_model /
	// bot_author_full_review_paths: the cheaper model a review of a PR opened
	// by a listed bot runs with. Off when its Model is empty.
	BotAuthorRoute BotAuthorRoute
	// AllAuthors is a no-op kept for configs that still set
	// review.all_authors. Every open PR is reviewed regardless of author
	// (hivecommons/hive#12974); the planner never reads this field.
	//
	// Deprecated: review dispatch no longer filters by author.
	AllAuthors bool
	// FixHumanPRs mirrors review.fix_human_prs. It no longer lets a fix kick
	// push onto a PR this hive's agents did not open: that commit would be
	// signed off by the hive, not the PR author, and the squash sign-off gate
	// rejects it as bot-signoff — a red only the author can clear
	// (hivecommons/hive#12897). Such a PR gets its review published with the
	// fix as a suggestion or patch block whichever way this is set; the value
	// is kept so the withheld-fix audit can say which way it was. AllAuthors
	// never implies pushing either (hivecommons/hive#8421).
	FixHumanPRs bool
	// AcknowledgeNoFindings carries config.ReviewConfig.AcknowledgeNoFindings
	// into the prompt builder, so a clean review still leaves a record.
	AcknowledgeNoFindings bool
	// ReviseRepos allowlists repos whose existing verdicts may be revisited
	// and whose reviews may be corrected in place. See
	// config.ReviewConfig.ReviseRepos.
	ReviseRepos []string
	// ReviseVerdictsBefore re-opens verdicts recorded before this instant for
	// a fresh review even though the PR's head SHA has not moved. Zero
	// disables revisiting entirely.
	ReviseVerdictsBefore time.Time
	// PendingTTL is how long a pending review entry stays authoritative with
	// no verdict for its head before it expires and is re-dispatched. Zero
	// means DefaultPendingReviewTTL; negative disables expiry.
	PendingTTL time.Duration
	// MaxRedispatch caps how many times one head's pending reviews may expire
	// before the PR is held for a human instead of re-kicked. Zero means
	// DefaultMaxRedispatch.
	MaxRedispatch int
	// PendingFixTTL is how long a pending fix row stays authoritative with
	// no new head before the next bounded attempt replaces it
	// (review.pending_fix_ttl). Zero means
	// DefaultPendingFixTTL; negative disables expiry. Attempts are capped by
	// escalation.MaxReEngagements.
	PendingFixTTL time.Duration
	// HeadMaxRetries caps how many times one review head's expired review of a
	// perspective is re-sent (review.head_max_retries). Zero means
	// MaxRedispatch.
	HeadMaxRetries int
	// DeltaRereview is review.delta_rereview: a PR with a verdict at an
	// earlier head is re-reviewed against the diff between that head and the
	// current one (planDelta), and a trivial delta re-stamps the earlier
	// verdict instead of running a session. DeltaTrivialLines is the
	// threshold for that skip (0 means DefaultDeltaTrivialLines, negative
	// disables it); DeltaMaxAge is how old a changes_requested verdict with
	// findings may be and still anchor a delta (0 means DefaultDeltaMaxAge).
	DeltaRereview     bool
	DeltaTrivialLines int
	DeltaMaxAge       time.Duration
	Agents            []AgentCapability
	Now               time.Time
}

type DispatchState struct {
	GeneratedAt time.Time         `json:"generated_at"`
	Pending     []PendingReview   `json:"pending_reviews,omitempty"`
	Recent      []RecentReview    `json:"recent_reviews,omitempty"`
	Fixes       []PendingFix      `json:"pending_fixes,omitempty"`
	FixAttempts []FixAttempt      `json:"fix_attempts,omitempty"`
	Human       []HumanReviewHold `json:"requires_human,omitempty"`
	// Withheld records the PR heads whose fix kick was refused because the PR
	// is not hive-authored. Keyed by head SHA like
	// Fixes, so each refusal is reported (and audited) once per head rather
	// than on every eval cycle, and a new push earns a fresh decision.
	Withheld []WithheldFix `json:"withheld_fixes,omitempty"`
	// Redispatches counts, per head, how many times its pending reviews
	// expired without a verdict. It is what turns a dead reviewer into a
	// bounded number of retries and then a human hold, rather than either an
	// indefinite stall or an endless re-kick.
	Redispatches []RedispatchCount `json:"redispatches,omitempty"`
	// Review heads (hivecommons/hive#13542). HeadRetries counts each
	// (PR head, perspective, head)'s expiries against review.head_max_retries;
	// MissingHeads are the heads that ran out while another head answered,
	// which the decision step proceeds without; Acknowledged are the PR heads
	// whose no-findings acknowledgement was already posted under the skip rule.
	HeadRetries  []HeadRetryCount   `json:"head_retries,omitempty"`
	MissingHeads []MissingHead      `json:"missing_heads,omitempty"`
	Acknowledged []AcknowledgedHead `json:"acknowledged,omitempty"`
	// RevisitStarved counts, per head, the consecutive cycles in which a
	// changes_requested verdict was sent down the revisit path instead of
	// the fix path. At RevisitStarvationCycles the planner forces the fix
	// (hivecommons/hive#14665). Cleared when the head is settled normally.
	RevisitStarved []RevisitStarvation `json:"revisit_starved,omitempty"`
	// Waits records, per head, the consecutive cycles in which the head
	// still owed perspectives but got no reviewer (plan.Waiting). Without
	// it a half-reviewed head could starve indefinitely behind the webhook
	// PRs the caller moves to the front of every cycle, and the stall was
	// invisible: nothing recorded that the head had been passed over. At
	// ReviewWaitStarvationCycles the planner moves the head ahead of the
	// rest of the list; the record is dropped once it is dispatched or
	// settled.
	Waits []ReviewWaitRecord `json:"review_waits,omitempty"`
}

// ReviewWaitRecord is the durable form of ReviewWait: the per-head count of
// consecutive cycles a head with perspectives still owed got no reviewer.
type ReviewWaitRecord struct {
	Repo    string    `json:"repo"`
	Number  int       `json:"number"`
	HeadSHA string    `json:"head_sha,omitempty"`
	Reason  string    `json:"reason"`
	Cycles  int       `json:"cycles"`
	FirstAt time.Time `json:"first_at"`
	LastAt  time.Time `json:"last_at"`
}

// ReviewWaitStarvationCycles is how many consecutive cycles a head may wait
// for a reviewer before the planner walks it ahead of every other PR,
// including the webhook-event PRs the caller promoted. Three cycles is long
// enough that an ordinary busy cycle never reorders the queue, and short
// enough that a head is not left half-reviewed for an hour while fresher PRs
// keep taking the free reviewers (hivecommons/hive#15229).
const ReviewWaitStarvationCycles = 3

// RevisitStarvation is the per-head counter behind RevisitStarvationCycles.
type RevisitStarvation struct {
	Repo    string    `json:"repo"`
	Number  int       `json:"number"`
	HeadSHA string    `json:"head_sha,omitempty"`
	Reason  string    `json:"reason"`
	Cycles  int       `json:"cycles"`
	LastAt  time.Time `json:"last_at"`
}

// ForcedFix is a fix kick the planner took despite a revisit being due,
// because the head had been starved of its fix for RevisitStarvationCycles
// consecutive cycles. The caller logs each one.
type ForcedFix struct {
	Repo    string
	Number  int
	HeadSHA string
	Reason  string
	Cycles  int
}

// RedispatchCount is the per-head expiry counter behind review.max_redispatch.
type RedispatchCount struct {
	Repo        string    `json:"repo"`
	Number      int       `json:"number"`
	HeadSHA     string    `json:"head_sha,omitempty"`
	Count       int       `json:"count"`
	LastExpired time.Time `json:"last_expired_at"`
}

// ExpiredPending is one pending review the planner dropped this cycle because
// it outlived PendingTTL with no verdict. The caller logs each so a reviewer
// that keeps dying is visible in the governor log, not only in the state file.
type ExpiredPending struct {
	Repo        string
	Number      int
	HeadSHA     string
	Perspective Perspective
	Agent       string
	Dispatched  time.Time
	Age         time.Duration
	// Count is the head's expiry count after this one.
	Count int
}

// WithheldFix is a fix kick the planner declined to dispatch. It carries what
// the audit trail needs to explain the silence: which PR, who opened it, and
// the setting that once allowed the push.
type WithheldFix struct {
	Repo    string `json:"repo"`
	Number  int    `json:"number"`
	HeadSHA string `json:"head_sha,omitempty"`
	Author  string `json:"author,omitempty"`
	Setting string `json:"setting"`
	// SettingEnabled is the value Setting had when the fix was withheld. On
	// is still withheld: the push would fail the squash sign-off gate
	// (hivecommons/hive#12897).
	SettingEnabled bool      `json:"setting_enabled,omitempty"`
	Reason         string    `json:"reason"`
	Withheld       time.Time `json:"withheld_at"`
}

// WithheldFixSetting names the config key an operator turns on to let the
// fixer push to PRs the hive did not open. It is what the audit entry cites.
const WithheldFixSetting = "review.fix_human_prs"

type PendingReview struct {
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
	BodyDigest  string      `json:"body_digest,omitempty"`
	Perspective Perspective `json:"perspective"`
	Agent       string      `json:"agent"`
	// Head is the review head key this perspective was dispatched to, empty
	// outside review_heads (hivecommons/hive#13542).
	Head string `json:"head,omitempty"`
	// Decision marks the decision step's dispatch: the base reviewer
	// reconciling the heads' reports for this perspective.
	Decision    bool   `json:"decision,omitempty"`
	AuthorAgent string `json:"author_agent,omitempty"`
	// HiveAuthored is the planner's hiveAuthored answer for the PR at
	// dispatch: App login, attributed agent, attribution trailer, or an
	// adoption the stage reconciler owns. The relay stamps it on the
	// verdict; before hivecommons/hive#15499 it derived the flag from
	// AuthorAgent alone, so an adoption with no agent attribution read as
	// a person's PR and its decision questions were never self-resolved.
	HiveAuthored bool `json:"hive_authored,omitempty"`
	// AuthorModel/ReviewModel are what the planner knew when it dispatched:
	// the PR's attribution-trailer model, and the model the kick told the
	// reviewer to run with (empty when no pool applied). Collect uses them
	// to stamp a verdict whose report omitted either (ModelStamps).
	AuthorModel string `json:"author_model,omitempty"`
	ReviewModel string `json:"review_model,omitempty"`
	// DeltaPreviousHead is the head this dispatch was a delta re-review
	// against (review.delta_rereview); empty for a full review. Collect
	// stamps it onto the verdict (ModelStamps) so the artifact says so.
	DeltaPreviousHead string    `json:"delta_previous_head,omitempty"`
	Dispatched        time.Time `json:"dispatched_at"`
}

type RecentReview struct {
	Repo              string      `json:"repo"`
	Number            int         `json:"number"`
	HeadSHA           string      `json:"head_sha,omitempty"`
	BodyDigest        string      `json:"body_digest,omitempty"`
	Perspective       Perspective `json:"perspective"`
	Agent             string      `json:"agent"`
	Head              string      `json:"head,omitempty"`
	Decision          bool        `json:"decision,omitempty"`
	AuthorAgent       string      `json:"author_agent,omitempty"`
	HiveAuthored      bool        `json:"hive_authored,omitempty"`
	AuthorModel       string      `json:"author_model,omitempty"`
	ReviewModel       string      `json:"review_model,omitempty"`
	DeltaPreviousHead string      `json:"delta_previous_head,omitempty"`
	Dispatched        time.Time   `json:"dispatched_at,omitempty"`
	Confirmed         time.Time   `json:"confirmed_at"`
}

type PendingFix struct {
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Agent      string    `json:"agent"`
	Attempts   int       `json:"attempts"`
	Dispatched time.Time `json:"dispatched_at"`
}

type FixAttempt struct {
	Repo       string    `json:"repo"`
	Number     int       `json:"number"`
	HeadSHA    string    `json:"head_sha,omitempty"`
	Agent      string    `json:"agent"`
	Attempts   int       `json:"attempts"`
	Dispatched time.Time `json:"dispatched_at"`
}

type HumanReviewHold struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	Reason    string    `json:"reason"`
	UpdatedAt time.Time `json:"updated_at"`
}

type DispatchKick struct {
	Agent       string
	Message     string
	PRRef       string
	Kind        string
	Repo        string
	Number      int
	HeadSHA     string
	BodyDigest  string
	Perspective Perspective
	AuthorAgent string
	// HiveAuthored mirrors PendingReview.HiveAuthored for the confirmed
	// (Recent) record.
	HiveAuthored bool
	// Head is the review head key for a head kick (hivecommons/hive#13542).
	Head string
	// Decision marks the decision kick that reconciles the heads' reports.
	Decision bool
	// Perspectives is every perspective this kick covers. It is set only by
	// combined dispatch; Perspective stays populated with the first of them so
	// existing consumers (logging, metrics, kick dedup) keep working unchanged
	// rather than silently seeing an empty perspective.
	Perspectives []Perspective
	// AuthorModel is the PR's attribution-trailer model; ReviewBackend and
	// ReviewModel are what the kick told the reviewer to run with, empty when
	// no review_models pool applied. ReviewModelFallback is true when the
	// agent's fallback (not its pool) chose the model.
	AuthorModel         string
	ReviewBackend       string
	ReviewModel         string
	ReviewModelFallback bool
	// BotAuthorRouted is true when ReviewModel came from review.bot_author_model
	// because the PR's author is a listed bot.
	BotAuthorRouted bool
	// DeltaPreviousHead is set when this kick is a delta re-review against
	// that head (review.delta_rereview); empty for a full review.
	DeltaPreviousHead string
	// Priority is why the PR was placed where it was in this cycle's
	// dispatch order (ReviewPriority*), for the dispatch log.
	Priority string
}

// SkippedReviewModel is a perspective kick the planner withheld because the
// reviewer's review_models fallback is "skip" and no pool entry was eligible
// against the PR's author model. The caller logs and audits each one; it is
// the swarm analogue of the cadence kick dropping the PR from ${PR_LIST}.
type SkippedReviewModel struct {
	Repo        string
	Number      int
	HeadSHA     string
	Perspective Perspective
	Agent       string
	AuthorModel string
}

type DispatchPlan struct {
	ReviewKicks []DispatchKick
	FixKicks    []DispatchKick
	// SkippedReviewModels are perspective kicks withheld under a "skip"
	// review_models fallback this cycle.
	SkippedReviewModels []SkippedReviewModel
	// WithheldFixes are the refusals NEW this cycle — the ones the caller
	// should log and audit. Refusals already recorded in State.Withheld for
	// the same head are not repeated here.
	WithheldFixes []WithheldFix
	// FixExclusions are changes-requested PRs whose fix kick was deliberately
	// not added to FixKicks this cycle. Callers log them so a stuck PR never
	// disappears behind a silent planner branch.
	FixExclusions []FixDispatchExclusion
	// Expired are the pending reviews dropped THIS cycle for outliving
	// PendingTTL without a verdict; their perspectives are re-dispatched in
	// the same plan unless the head's retry budget is exhausted.
	Expired []ExpiredPending
	// Exhausted are the heads that hit MaxRedispatch this cycle and were
	// held for a human instead of re-kicked.
	Exhausted []HumanReviewHold
	// Acknowledgements are the no-findings comments to post this cycle for PR
	// heads the review-heads skip rule settled (every head clean), when
	// review.acknowledge_no_findings is on. The caller posts them.
	Acknowledgements []ReviewAcknowledgement
	// DeltaRestamps are verdicts carried from an earlier head onto the
	// current head without a session this cycle (review.delta_rereview,
	// trivial delta). The caller records them in the verdict artifact
	// (RecordDeltaRestamps) and logs each one.
	DeltaRestamps []DeltaRestamp
	// ForcedFixes are the changes_requested heads whose fix kick was forced
	// this cycle after RevisitStarvationCycles revisit-suppressed cycles.
	ForcedFixes []ForcedFix
	// Waiting are the PRs that needed a review kick this cycle and got none
	// because every eligible reviewer was busy or the parallel budget was
	// spent. They are the reviewer autoscaler's demand signal
	// (hivecommons/hive#14685): one entry per PR, on its current head.
	Waiting []ReviewWait
	// Promoted are the heads this cycle walked ahead of the rest of the
	// list because they had waited ReviewWaitStarvationCycles or more
	// consecutive cycles for a reviewer. The caller logs each one.
	Promoted []ReviewWaitRecord
	State    DispatchState
}

// ReviewWait is one PR left waiting for a reviewer this cycle.
type ReviewWait struct {
	Repo    string
	Number  int
	HeadSHA string
	Reason  string
}

// Key identifies the wait across cycles: a push to the PR starts a new wait.
func (w ReviewWait) Key() string { return fmt.Sprintf("%s#%d@%s", w.Repo, w.Number, w.HeadSHA) }

const (
	// ReviewWaitNoFreeReviewer: every reviewer eligible for the PR has a
	// review kick in flight.
	ReviewWaitNoFreeReviewer = "no-free-reviewer"
	// ReviewWaitNoParallelSlot: review.max_parallel_reviews is spent.
	ReviewWaitNoParallelSlot = "no-parallel-slot"
)

// recordWait notes a PR that still has perspectives to review but could not
// be dispatched this cycle. Idempotent per PR.
func (plan *DispatchPlan) recordWait(pr PullRequest, reason string) {
	for _, w := range plan.Waiting {
		if w.Repo == pr.Repo && w.Number == pr.Number {
			return
		}
	}
	plan.Waiting = append(plan.Waiting, ReviewWait{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: reason})
}

type FixDispatchExclusion struct {
	Repo     string
	Number   int
	HeadSHA  string
	Author   string
	Reason   string
	Attempts int
}

const (
	FixDispatchExcludedPending              = "pending-fix-already-dispatched"
	FixDispatchExcludedAttemptsExhausted    = "fix-attempts-exhausted"
	FixDispatchExcludedNoEnabledFixer       = "no-enabled-fixer"
	FixDispatchExcludedNotHiveAuthored      = "not-hive-authored"
	FixDispatchExcludedNotHiveAuthoredDCO   = "not-hive-authored-squash-signoff"
	FixAttemptsExhaustedHumanDecisionReason = "fix-attempts-exhausted"
	// The revisit reasons name why a changes_requested verdict was sent back
	// to the reviewers instead of the fixer this cycle (hivecommons/hive#14665).
	FixDispatchExcludedRevisitBodyEdited = "verdict-revisit:body-edited"
	FixDispatchExcludedRevisitStale      = "verdict-revisit:stale-reviewer"
)

func LoadDispatchState(path string) (DispatchState, error) {
	explicit := path != ""
	if !explicit {
		path = ReviewDispatchStatePath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// One-time migration: a hive that ran before the state moved to the
		// durable dir still has its dispatch record at the legacy scratch
		// path. Read it so the upgrade does not look like amnesia and re-walk
		// the queue from the top. The next WriteDispatchState lands on the
		// durable path, so this fallback stops firing on its own. Only for the
		// default path — an explicit path means a caller (or a test) asked for
		// exactly that file.
		if explicit || !os.IsNotExist(err) || LegacyReviewDispatchStatePath == path {
			return DispatchState{}, err
		}
		legacy, legacyErr := os.ReadFile(LegacyReviewDispatchStatePath)
		if legacyErr != nil {
			return DispatchState{}, err
		}
		data = legacy
	}
	var state DispatchState
	if err := json.Unmarshal(data, &state); err != nil {
		return DispatchState{}, err
	}
	return state, nil
}

func WriteDispatchState(path string, state DispatchState) error {
	if path == "" {
		path = ReviewDispatchStatePath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func PlanDispatch(prs []PullRequest, artifact Artifact, state DispatchState, opts DispatchOptions) DispatchPlan {
	now := opts.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	state.GeneratedAt = now
	state = pruneState(state, prs, opts.ProjectOrg)
	state.Pending = pruneUnconfiguredAgents(state.Pending, opts.Agents)
	plan := DispatchPlan{State: state}
	if !opts.RequireApproval || !opts.FanOut {
		return plan
	}
	capable := reviewCapableAgents(opts)
	heads, reviewers := dispatchReviewers(capable)
	headsMode := len(heads) > 0
	maxParallel := opts.MaxParallelReviews
	if maxParallel <= 0 {
		maxParallel = DefaultMaxParallelReviews
	}
	availableSlots := maxParallel
	prs, plan.Promoted = promoteStarvedWaits(prs, plan.State.Waits, opts.ProjectOrg)
	prs = orderByReviewPriority(prs, plan.Promoted, opts.ProjectOrg)
	for _, pr := range prs {
		pr.Repo = fullRepoName(pr.Repo, opts.ProjectOrg)
		if pr.Number <= 0 || pr.Repo == "" {
			continue
		}
		// Every open PR is reviewed, whoever opened it — hive agent, human,
		// clanker, dependabot or any other bot (hivecommons/hive#12974). The
		// only switch is whether review itself is on (RequireApproval and
		// FanOut, checked above). Author kind still decides what happens AFTER
		// the verdict: fixPushAllowed and the merge lanes, not this loop.
		revisiting := false
		var delivered map[Perspective]Verdict
		var deliveredHeads map[Perspective][]string
		var awaiting *Aggregate
		if req := pr.HumanChangeRequest; req != nil && SameHeadSHA(req.HeadSHA, pr.HeadSHA) {
			// A trusted maintainer answered this head — a changes_requested
			// review or `/hive decision <text>` — so whatever the swarm said
			// about it is superseded (hivecommons/hive#13041). Hand the
			// instruction to the author agent; the swarm re-reviews the head
			// the fix pushes. One kick per head: dispatchFix's pending check.
			plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
			plan.State.Human = removeHumanForHead(plan.State.Human, pr)
			agg, _ := artifact.AggregateForBody(pr.Repo, pr.Number, pr.HeadSHA, pr.BodyDigest)
			if fixPushAllowed(pr, opts) {
				plan.dispatchFix(pr, agg, opts, now)
			} else {
				plan.withholdFix(pr, opts, now)
			}
			continue
		}
		if agg, ok := artifact.AggregateForBody(pr.Repo, pr.Number, pr.HeadSHA, pr.BodyDigest); ok {
			// A verdict normally settles a PR until its head moves. That is
			// right while the reviewer is sound, and a trap once a
			// reviewer-side defect is found: without this, verdicts produced
			// by a known-broken reviewer stay frozen until someone happens to
			// push a commit.
			revisitReason := ""
			if staleVerdictRevisitable(pr.Repo, agg, opts) {
				revisitReason = FixDispatchExcludedRevisitStale
			} else if bodyEditedVerdictRevisitable(pr, agg) {
				revisitReason = FixDispatchExcludedRevisitBodyEdited
			}
			// A revisit re-asks the reviewers; it must not keep the fixer
			// away from a changes_requested head indefinitely. After
			// RevisitStarvationCycles consecutive revisit cycles with no fix
			// kick, the fix path is taken regardless (hivecommons/hive#14665).
			forcedFix := false
			if revisitReason != "" && agg.Verdict == VerdictChangesRequested {
				if starved := revisitStarvedCycles(plan.State, pr); starved >= RevisitStarvationCycles {
					plan.ForcedFixes = append(plan.ForcedFixes, ForcedFix{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: revisitReason, Cycles: starved})
					revisitReason = ""
					forcedFix = true
				}
			}
			if revisitReason == "" {
				plan.State.RevisitStarved = removeRevisitStarvedForHead(plan.State.RevisitStarved, pr)
				if agg.Verdict == VerdictIncomplete {
					// Every perspective that reported approved; some never
					// reported. The head is not settled, it is under-asked:
					// keep the delivered perspectives off the pending list
					// (their reports exist) and let the normal missing-
					// perspective path below re-ask only for the rest, under
					// the same TTL and max_redispatch budget as any head.
					plan.State.Pending = removePendingForPerspectivesAndHeads(plan.State.Pending, pr, agg.Perspectives, agg.Heads)
					delivered = agg.Perspectives
					deliveredHeads = agg.Heads
					if agg.AwaitingDecision {
						a := agg
						awaiting = &a
					}
					if plan.redispatchExhausted(pr) {
						continue
					}
					if headsMode {
						plan.expireHeadPending(pr, opts, deliveredHeads, now)
					}
					plan.expirePending(pr, opts, now)
				} else {
					// A forced fix leaves the in-flight revisit's pending
					// entries alone: the relay binds its verdicts to them.
					if !forcedFix {
						plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
					}
					if headsMode {
						plan.acknowledgeSkippedDecision(pr, agg, opts, now)
					}
					switch agg.Verdict {
					case VerdictChangesRequested:
						if fixPushAllowed(pr, opts) {
							plan.dispatchFix(pr, agg, opts, now)
						} else {
							plan.withholdFix(pr, opts, now)
						}
					case VerdictRequiresHuman:
						plan.holdForHuman(pr, agg, now)
					}
					continue
				}
			} else {
				if agg.Verdict == VerdictChangesRequested {
					plan.noteRevisitStarvation(pr, revisitReason, now)
				}
				// Pending entries are the lifetime record of what this head has
				// already been asked, and a PR with a verdict always has them —
				// so left in place they make every perspective look covered and
				// the revisit silently never dispatches. Clear them so the whole
				// set is asked again. But only once: a revisit already in flight
				// shows as entries dispatched after the cutoff, and clearing
				// those would re-kick the same PR every cycle until its new
				// verdict lands.
				if revisitInFlight(plan.State, pr, opts.ReviseVerdictsBefore) {
					// A revisit whose reviewer died is stuck the same way an
					// unreviewed head is: the stale verdict stands and nothing
					// re-asks. Let the TTL release it.
					// Only the entries that outlived the TTL were dropped; the
					// rest are perspectives still owed or already answered, so
					// they stay and only the missing ones are re-asked.
					if dropped := plan.expirePending(pr, opts, now); !dropped {
						continue
					}
				} else {
					plan.State.Pending = removePendingForHead(plan.State.Pending, pr)
				}
				revisiting = true
			}
		} else if plan.redispatchExhausted(pr) {
			continue
		} else {
			if headsMode {
				plan.expireHeadPending(pr, opts, nil, now)
			}
			plan.expirePending(pr, opts, now)
		}
		// Delta re-review: a head whose PR was judged at an earlier head is
		// reviewed against the diff from that head, and a trivial delta
		// carries the earlier verdict forward without a session. Not on a
		// same-head revisit (the prior verdict is the one being redone) and
		// not once this head already has dispatch entries (a review is in
		// flight or partly delivered; the restamp would race it).
		var delta *DeltaContext
		if !revisiting && delivered == nil {
			mode, ctx, prior := planDelta(pr, artifact, opts, now)
			switch mode {
			case deltaModeTrivial:
				if !plan.hasPendingForHead(pr) {
					plan.DeltaRestamps = append(plan.DeltaRestamps, DeltaRestamp{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, PreviousHead: ctx.PreviousHead, Aggregate: restampAggregate(prior, pr, *ctx, now)})
					continue
				}
				delta = ctx
			case deltaModeDelta:
				delta = ctx
			}
		}
		if availableSlots <= 0 {
			if len(pendingMissingPerspectives(plan.State, pr, opts.Perspectives.List(), delivered)) > 0 {
				plan.recordWait(pr, ReviewWaitNoParallelSlot)
			}
			continue
		}
		authorBase := prAuthorBase(pr, opts.Agents)
		if prHeads := reviewersForPR(heads, pr.AuthorAgent, authorBase); len(prHeads) > 0 {
			availableSlots = plan.dispatchHeads(pr, prHeads, opts, delivered, deliveredHeads, revisiting, delta, availableSlots, now)
			// Once every head has settled, the base reviews the heads'
			// reports and records the decided verdict (#13542).
			if awaiting != nil {
				availableSlots = plan.dispatchDecision(pr, *awaiting, reviewersForPR(decisionAgents(capable), pr.AuthorAgent, authorBase), opts, revisiting, availableSlots, now)
			}
		}
		// Plain reviewers (none in a hive whose config passed
		// validateReviewHeads with heads on) take the perspectives the heads
		// left unasked, on the ordinary path.
		prReviewers := reviewersForPR(reviewers, pr.AuthorAgent, authorBase)
		if len(prReviewers) == 0 {
			continue
		}
		missing := pendingMissingPerspectives(plan.State, pr, opts.Perspectives.List(), delivered)
		if len(missing) == 0 {
			continue
		}
		if availableSlots <= 0 {
			plan.recordWait(pr, ReviewWaitNoParallelSlot)
			continue
		}
		// One kick, every outstanding perspective, one comment. The per-PR cap
		// is deliberately not applied here: it exists to bound how many review
		// COMMENTS one PR collects, and a combined review produces exactly one
		// however many perspectives it covers. Applying it anyway would drop
		// perspectives to buy quiet that has already been bought.
		if opts.CombinedPerspectives {
			reviewer, free := freeReviewer(prReviewers, plan.inFlight(opts, now))
			if !free {
				plan.recordWait(pr, ReviewWaitNoFreeReviewer)
				continue
			}
			agent := reviewer.Name
			// One kick covers several perspectives, so a per-perspective
			// override can only apply when it is the only perspective.
			var overridePerspective Perspective
			if len(missing) == 1 {
				overridePerspective = missing[0]
			}
			choice := chooseReviewModel(reviewer, overridePerspective, pr.AuthorModel, opts.PerspectiveModels)
			choice, botRouted := applyBotAuthorRoute(choice, pr, opts)
			if choice.Skip {
				for _, p := range missing {
					plan.SkippedReviewModels = append(plan.SkippedReviewModels, SkippedReviewModel{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: p, Agent: agent, AuthorModel: pr.AuthorModel})
				}
				continue
			}
			if choice.RequiresHuman {
				plan.State.Human = upsertHuman(plan.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: noIndependentModelReason(pr.AuthorModel), UpdatedAt: now})
				continue
			}
			msg := BuildCombinedPrompt(pr, missing, PromptOptions{
				PostComments:          opts.PostComments,
				AcknowledgeNoFindings: opts.AcknowledgeNoFindings,
				Revise:                revisiting,
				Perspectives:          opts.Perspectives,
				ProposeFixesOnly:      !fixPushAllowed(pr, opts),
				WritingGuideSection:   opts.WritingGuideSection,
				ReviewBotLogins:       opts.ReviewBotLogins,
				ReviewBotMinPriority:  opts.ReviewBotMinPriority,
				ReviewBackend:         choice.Backend,
				ReviewModel:           choice.Model,
				Delta:                 delta,
			})
			plan.ReviewKicks = append(plan.ReviewKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: missing[0], Perspectives: missing, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewBackend: choice.Backend, ReviewModel: choice.Model, ReviewModelFallback: choice.Fallback, BotAuthorRouted: botRouted, DeltaPreviousHead: deltaPreviousHead(delta), Priority: pr.Priority})
			for _, p := range missing {
				plan.State.Pending = append(plan.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: p, Agent: agent, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewModel: stampedReviewModel(reviewer, choice), DeltaPreviousHead: deltaPreviousHead(delta), Dispatched: now})
			}
			availableSlots--
			continue
		}
		limit := len(missing)
		// Breadth before depth: the parallel budget is spent in PR order, so
		// an uncapped first PR would take every slot for its own perspectives
		// and leave the rest of the queue unreviewed this cycle.
		//
		// The cap is also a lifetime budget per head SHA, not merely a
		// per-cycle one. Perspectives a PR has already been given persist in
		// state.Pending, so without counting them a capped hive still works
		// through every perspective one cycle at a time and posts a separate
		// review comment for each. From a maintainer's side that is the same
		// pile of comments, just spread out. Counting what a head SHA has
		// already received is what makes "max perspectives per PR" mean what
		// it says. A force-push clears the pending entries, so genuinely new
		// code earns a fresh budget.
		if perPR := opts.effectiveMaxPerspectivesPerPR(); perPR > 0 {
			covered := opts.Perspectives.Len() - len(missing)
			remaining := perPR - covered
			if remaining <= 0 {
				continue
			}
			if limit > remaining {
				limit = remaining
			}
		}
		if len(prReviewers) == 1 && limit > 1 {
			limit = 1
		}
		if limit > availableSlots {
			limit = availableSlots
		}
		for i := 0; i < limit; i++ {
			reviewer, free := freeReviewer(prReviewers, plan.inFlight(opts, now))
			if !free {
				if i == 0 {
					plan.recordWait(pr, ReviewWaitNoFreeReviewer)
				}
				break
			}
			agent := reviewer.Name
			perspective := missing[i]
			choice := chooseReviewModel(reviewer, perspective, pr.AuthorModel, opts.PerspectiveModels)
			choice, botRouted := applyBotAuthorRoute(choice, pr, opts)
			if choice.Skip {
				plan.SkippedReviewModels = append(plan.SkippedReviewModels, SkippedReviewModel{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Perspective: perspective, Agent: agent, AuthorModel: pr.AuthorModel})
				continue
			}
			if choice.RequiresHuman {
				plan.State.Human = upsertHuman(plan.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: noIndependentModelReason(pr.AuthorModel), UpdatedAt: now})
				continue
			}
			msg := BuildPerspectivePromptWith(perspective, pr, PromptOptions{
				PostComments:          opts.PostComments,
				AcknowledgeNoFindings: opts.AcknowledgeNoFindings,
				Revise:                revisiting,
				Perspectives:          opts.Perspectives,
				ProposeFixesOnly:      !fixPushAllowed(pr, opts),
				WritingGuideSection:   opts.WritingGuideSection,
				ReviewBotLogins:       opts.ReviewBotLogins,
				ReviewBotMinPriority:  opts.ReviewBotMinPriority,
				ReviewBackend:         choice.Backend,
				ReviewModel:           choice.Model,
				Delta:                 delta,
			})
			plan.ReviewKicks = append(plan.ReviewKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "review", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: perspective, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewBackend: choice.Backend, ReviewModel: choice.Model, ReviewModelFallback: choice.Fallback, BotAuthorRouted: botRouted, DeltaPreviousHead: deltaPreviousHead(delta), Priority: pr.Priority})
			plan.State.Pending = append(plan.State.Pending, PendingReview{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, BodyDigest: pr.BodyDigest, Perspective: perspective, Agent: agent, AuthorAgent: pr.AuthorAgent, HiveAuthored: hiveAuthored(pr, opts), AuthorModel: pr.AuthorModel, ReviewModel: stampedReviewModel(reviewer, choice), DeltaPreviousHead: deltaPreviousHead(delta), Dispatched: now})
			availableSlots--
		}
	}
	plan.State.Waits = reconcileWaits(plan.State.Waits, plan.Waiting, opts.ProjectOrg, now)
	return plan
}

// promoteStarvedWaits returns prs with every head whose wait record has
// reached ReviewWaitStarvationCycles moved to the front, in their existing
// relative order, and the records it promoted. The caller has usually
// already moved webhook-event PRs to the front; a head that has been passed
// over for that many cycles goes ahead of them, so a steady stream of fresh
// events cannot leave a half-reviewed head waiting indefinitely
// (hivecommons/hive#15229). Every gate in PlanDispatch still applies.
func promoteStarvedWaits(prs []PullRequest, waits []ReviewWaitRecord, org string) ([]PullRequest, []ReviewWaitRecord) {
	if len(waits) == 0 || len(prs) < 2 {
		return prs, nil
	}
	starved := make([]bool, len(prs))
	var promoted []ReviewWaitRecord
	for i, pr := range prs {
		repo := fullRepoName(pr.Repo, org)
		for _, w := range waits {
			if w.Cycles >= ReviewWaitStarvationCycles && samePRHead(w.Repo, w.Number, w.HeadSHA, repo, pr.Number, pr.HeadSHA) {
				starved[i] = true
				promoted = append(promoted, w)
				break
			}
		}
	}
	if len(promoted) == 0 {
		return prs, nil
	}
	out := make([]PullRequest, 0, len(prs))
	for i, pr := range prs {
		if starved[i] {
			out = append(out, pr)
		}
	}
	for i, pr := range prs {
		if !starved[i] {
			out = append(out, pr)
		}
	}
	return out, promoted
}

// reconcileWaits folds this cycle's Waiting into the durable wait records:
// a head still waiting gains a cycle; a head that was waiting and now is
// not (dispatched or settled) loses its record. Records for PRs no longer
// open were already dropped by pruneState.
func reconcileWaits(records []ReviewWaitRecord, waiting []ReviewWait, org string, now time.Time) []ReviewWaitRecord {
	stillWaiting := make(map[string]ReviewWait, len(waiting))
	for _, w := range waiting {
		stillWaiting[reviewKey(fullRepoName(w.Repo, org), w.Number, w.HeadSHA, "")] = w
	}
	out := make([]ReviewWaitRecord, 0, len(waiting))
	seen := map[string]bool{}
	for _, r := range records {
		key := reviewKey(fullRepoName(r.Repo, org), r.Number, r.HeadSHA, "")
		w, waits := stillWaiting[key]
		if !waits {
			continue
		}
		r.Cycles++
		r.Reason = w.Reason
		r.LastAt = now
		if r.FirstAt.IsZero() {
			r.FirstAt = now
		}
		seen[key] = true
		out = append(out, r)
	}
	for _, w := range waiting {
		key := reviewKey(fullRepoName(w.Repo, org), w.Number, w.HeadSHA, "")
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, ReviewWaitRecord{Repo: fullRepoName(w.Repo, org), Number: w.Number, HeadSHA: w.HeadSHA, Reason: w.Reason, Cycles: 1, FirstAt: now, LastAt: now})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// WaitFor returns the wait record for this PR head, if the planner has been
// passing it over for lack of a reviewer.
func (s DispatchState) WaitFor(repo string, number int, headSHA string) (ReviewWaitRecord, bool) {
	for _, w := range s.Waits {
		if samePRHead(w.Repo, w.Number, w.HeadSHA, repo, number, headSHA) {
			return w, true
		}
	}
	return ReviewWaitRecord{}, false
}

// deltaPreviousHead is the head a delta kick is judged against, "" for a
// full review.
func deltaPreviousHead(d *DeltaContext) string {
	if d == nil {
		return ""
	}
	return strings.TrimSpace(d.PreviousHead)
}

// hasPendingForHead reports whether any dispatch entry exists for this PR
// head: a review in flight, or one already answered in part.
func (p *DispatchPlan) hasPendingForHead(pr PullRequest) bool {
	for _, e := range p.State.Pending {
		if samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return true
		}
	}
	return false
}

// stampedReviewModel is the model a pending review is attributed to when its
// report arrives without one: the chosen pool model, else the reviewer's own
// launch model — the same fallback pkg/github's evidence writer applies.
func stampedReviewModel(reviewer AgentCapability, choice ReviewModelChoice) string {
	if m := strings.TrimSpace(choice.Model); m != "" {
		return m
	}
	return strings.TrimSpace(reviewer.Model)
}

func noIndependentModelReason(authorModel string) string {
	reason := "no independent review model available (review_models fallback requires_human)"
	if m := strings.TrimSpace(authorModel); m != "" {
		reason += " for author model " + m
	}
	return reason
}

// pendingKickKey identifies the review kick a pending entry belongs to. A
// head kick covers one perspective; a combined kick and a decision kick cover
// several perspectives of one PR head in one session, so their entries share a
// key. Plain per-perspective entries cannot be told apart from a combined
// kick's (both carry no head), so they key on the PR head: for load counting
// that undercounts one agent holding several plain perspectives of the same PR,
// which the one-kick-in-flight rule in freeReviewer prevents for new dispatch.
func pendingKickKey(p PendingReview) string {
	base := fmt.Sprintf("%s#%d@%s", strings.ToLower(p.Repo), p.Number, p.HeadSHA)
	switch {
	case p.Decision:
		return base + "/decision"
	case p.Head != "":
		return base + "/" + string(p.Perspective) + "/" + p.Head
	default:
		return base
	}
}

// pendingKicksByAgent counts the review kicks still pending on each agent
// (dispatched and not yet answered), including kicks planned earlier in this
// cycle: the planner appends to state.Pending as it goes.
func pendingKicksByAgent(pending []PendingReview) map[string]int {
	kicks := map[string]map[string]bool{}
	for _, p := range pending {
		if kicks[p.Agent] == nil {
			kicks[p.Agent] = map[string]bool{}
		}
		kicks[p.Agent][pendingKickKey(p)] = true
	}
	out := make(map[string]int, len(kicks))
	for agent, keys := range kicks {
		out[agent] = len(keys)
	}
	return out
}

// freeReviewer picks the reviewer for a review kick: the first, in name order,
// with no review kick in flight (pending on it, counting this cycle's kicks),
// and false when every reviewer is busy.
//
// It replaces leastLoadedReviewer, which counted distinct PR heads per agent
// (#13401, itself replacing "always reviewers[0]"). Replicas holding the same
// PR tied on that count, so one PR's remaining perspectives all went to the
// first replica in name order: on a two-replica Sonnet head, four perspective
// kicks reached one Claude session within three minutes, it ran one turn, and
// three verdicts never came back. An agent's CLI works one prompt at a time; a
// second review kick typed into a busy session is merged into, or lost behind,
// the turn in progress, and its pending entry can only expire.
//
// When nobody is free the work waits for a later cycle. Nothing is recorded,
// so waiting costs no redispatch and no head retry, and pending_ttl starts
// when the kick is actually sent to an idle agent.
func freeReviewer(reviewers []AgentCapability, pending []PendingReview) (AgentCapability, bool) {
	load := pendingKicksByAgent(pending)
	for _, r := range reviewers {
		if load[r.Name] == 0 {
			return r, true
		}
	}
	return AgentCapability{}, false
}

// inFlight is the subset of pending entries that still mean "this agent is
// working on a kick", the only ones freeReviewer may count. A pending entry is
// also the lifetime record of what a head was asked, and two kinds outlive the
// work they stood for (hivecommons/hive#14361):
//
//   - entries of a head whose redispatch budget is exhausted: expirePending
//     deliberately leaves them in place so the head is never re-kicked, and
//     holds the PR for a human;
//   - entries older than pending_ttl that expirePending has not dropped,
//     because the planner did not reach their PR this cycle.
//
// Counted as load, they pinned every replica of every head (and the decision
// agent) indefinitely: three PRs that ran out of redispatches left 13 entries
// behind and the swarm dispatched nothing for any PR for ~36 hours while every
// reviewer sat idle. They stay in state for their bookkeeping; they just do not
// make anyone busy. An entry with no dispatched_at has no measurable age and
// still counts, as before.
func (p *DispatchPlan) inFlight(opts DispatchOptions, now time.Time) []PendingReview {
	ttl := opts.effectivePendingTTL()
	out := make([]PendingReview, 0, len(p.State.Pending))
	for _, e := range p.State.Pending {
		if ttl > 0 && !e.Dispatched.IsZero() && now.Sub(e.Dispatched) >= ttl {
			continue
		}
		if p.headExhausted(e.Repo, e.Number, e.HeadSHA) {
			continue
		}
		out = append(out, e)
	}
	return out
}

// headExhausted is redispatchExhausted keyed on a pending entry's PR head.
func (p *DispatchPlan) headExhausted(repo string, number int, headSHA string) bool {
	for _, h := range p.State.Human {
		if samePRHead(h.Repo, h.Number, h.HeadSHA, repo, number, headSHA) && strings.HasPrefix(h.Reason, HoldReasonDispatchExhausted) {
			return true
		}
	}
	return false
}

// pruneUnconfiguredAgents drops pending entries for agents the hive no longer
// runs, such as a replica or review head removed by lowering `replicas`. Their
// verdict can never arrive, so the entry would only hold its perspective as
// "asked" forever. A pruned perspective is simply asked again. With no agent
// list (callers that plan without one) nothing is pruned.
func pruneUnconfiguredAgents(pending []PendingReview, agents []AgentCapability) []PendingReview {
	if len(agents) == 0 {
		return pending
	}
	known := make(map[string]bool, len(agents))
	for _, a := range agents {
		known[a.Name] = true
	}
	kept := pending[:0]
	for _, e := range pending {
		if e.Agent != "" && !known[e.Agent] {
			continue
		}
		kept = append(kept, e)
	}
	return kept
}

// agentBase resolves an agent's base lane from the full configured set, so an
// authoring replica that is itself paused, on-demand or not review-capable
// still maps to its base.
func agentBase(all []AgentCapability, name string) string {
	name = strings.TrimSpace(name)
	for _, a := range all {
		if a.Name == name {
			return baseOf(a)
		}
	}
	return name
}

// prAuthorBase is the PR's author base: the caller-resolved AuthorBase when
// set, otherwise AuthorAgent resolved against agents.
func prAuthorBase(pr PullRequest, agents []AgentCapability) string {
	if base := strings.TrimSpace(pr.AuthorBase); base != "" {
		return base
	}
	return agentBase(agents, pr.AuthorAgent)
}

func reviewersForPR(reviewers []AgentCapability, authorAgent, authorBase string) []AgentCapability {
	authorAgent = strings.TrimSpace(authorAgent)
	if authorAgent == "" {
		return reviewers
	}
	if authorBase == "" {
		authorBase = authorAgent
	}
	out := make([]AgentCapability, 0, len(reviewers))
	for _, reviewer := range reviewers {
		if reviewer.Name != authorAgent && baseOf(reviewer) != authorBase {
			out = append(out, reviewer)
		}
	}
	return out
}

// holdForHuman records a settled requires_human verdict as a human hold so the
// operator's triage label is applied to the PR, not just written into the
// review body. Before this, only fixer-side exhaustion produced a hold, so the
// most common "a human must decide" outcome — the reviewer saying so — never
// reached the label at all.
//
// An aggregate with no perspectives means no report was ever collected; that
// is an unreviewed PR, not one awaiting a decision, and must not be labelled.
func (p *DispatchPlan) holdForHuman(pr PullRequest, agg Aggregate, now time.Time) {
	if len(agg.Perspectives) == 0 {
		return
	}
	reason := "reviewer verdict requires_human"
	if len(agg.Reasons) > 0 {
		reason = agg.Reasons[0]
	}
	p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: reason, UpdatedAt: now})
}

// bodyEditedVerdictRevisitable reports whether a non-approve verdict was
// judged against an earlier PR title/body than the one the PR carries now.
// Only a present-and-different digest proves an edit. A verdict with no
// digest is unknown, not edited: perspective reports may omit body_digest
// (the prompt allows it), and reading the gap as "edited" sent every such
// changes_requested verdict down the revisit path instead of the fix path,
// forever (hivecommons/hive#14665). The UpdatedAt fallback is for PR
// snapshots that predate digests altogether.
func bodyEditedVerdictRevisitable(pr PullRequest, agg Aggregate) bool {
	if agg.Verdict == VerdictApprove || agg.Verdict == VerdictIncomplete {
		return false
	}
	prDigest := strings.TrimSpace(pr.BodyDigest)
	aggDigest := strings.TrimSpace(agg.BodyDigest)
	if prDigest != "" {
		if aggDigest == "" {
			return false
		}
		return prDigest != aggDigest
	}
	return !pr.UpdatedAt.IsZero() && !agg.RecordedAt.IsZero() && pr.UpdatedAt.After(agg.RecordedAt)
}

// RevisitStarvationCycles is how many consecutive planner cycles a
// changes_requested head may be sent down the revisit path with no fix kick
// before the planner forces the fix path. The revisit exists to re-ask the
// reviewers; it must never become a reason the fixer is not asked at all.
const RevisitStarvationCycles = 3

// revisitStarvedCycles returns how many consecutive cycles this head's
// changes_requested verdict was suppressed by the revisit path so far.
func revisitStarvedCycles(state DispatchState, pr PullRequest) int {
	for _, s := range state.RevisitStarved {
		if samePRHead(s.Repo, s.Number, s.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return s.Cycles
		}
	}
	return 0
}

// noteRevisitStarvation counts one more cycle in which the revisit path
// suppressed this head's fix kick, and records the exclusion so the caller
// logs why fix_kicks stayed at zero.
func (p *DispatchPlan) noteRevisitStarvation(pr PullRequest, reason string, now time.Time) {
	cycles := 0
	found := false
	for i := range p.State.RevisitStarved {
		s := &p.State.RevisitStarved[i]
		if samePRHead(s.Repo, s.Number, s.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			s.Cycles++
			s.Reason = reason
			s.LastAt = now
			cycles = s.Cycles
			found = true
			break
		}
	}
	if !found {
		cycles = 1
		p.State.RevisitStarved = append(p.State.RevisitStarved, RevisitStarvation{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: reason, Cycles: cycles, LastAt: now})
	}
	p.recordFixExclusion(pr, reason, cycles)
}

func removeRevisitStarvedForHead(items []RevisitStarvation, pr PullRequest) []RevisitStarvation {
	kept := items[:0]
	for _, s := range items {
		if samePRHead(s.Repo, s.Number, s.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			continue
		}
		kept = append(kept, s)
	}
	return kept
}

// effectivePendingTTL resolves PendingTTL: zero is the default, negative
// turns expiry off.
func (o DispatchOptions) effectivePendingTTL() time.Duration {
	if o.PendingTTL < 0 {
		return 0
	}
	if o.PendingTTL == 0 {
		return DefaultPendingReviewTTL
	}
	return o.PendingTTL
}

func (o DispatchOptions) effectiveMaxRedispatch() int {
	if o.MaxRedispatch <= 0 {
		return DefaultMaxRedispatch
	}
	return o.MaxRedispatch
}

// expirePending drops this head's pending entries that have outlived
// PendingTTL with no verdict, so pendingMissingPerspectives re-dispatches
// them, and reports whether it dropped any. Each cycle that drops something
// counts one expiry against the head; once the count reaches MaxRedispatch
// the entries are left in place (so nothing is re-kicked) and the PR is held
// for a human with a HoldReasonDispatchExhausted reason.
//
// An entry with no dispatched_at has no measurable age and is never expired.
func (p *DispatchPlan) expirePending(pr PullRequest, opts DispatchOptions, now time.Time) bool {
	ttl := opts.effectivePendingTTL()
	if ttl <= 0 || p.redispatchExhausted(pr) {
		return false
	}
	var expired []ExpiredPending
	for _, e := range p.State.Pending {
		if !samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) || e.Dispatched.IsZero() {
			continue
		}
		age := now.Sub(e.Dispatched)
		if age < ttl {
			continue
		}
		expired = append(expired, ExpiredPending{Repo: e.Repo, Number: e.Number, HeadSHA: e.HeadSHA, Perspective: e.Perspective, Agent: e.Agent, Dispatched: e.Dispatched, Age: age})
	}
	if len(expired) == 0 {
		return false
	}
	idx := redispatchIndex(p.State.Redispatches, pr)
	if idx < 0 {
		p.State.Redispatches = append(p.State.Redispatches, RedispatchCount{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA})
		idx = len(p.State.Redispatches) - 1
	}
	p.State.Redispatches[idx].Count++
	p.State.Redispatches[idx].LastExpired = now
	count := p.State.Redispatches[idx].Count
	for i := range expired {
		expired[i].Count = count
	}
	p.Expired = append(p.Expired, expired...)
	if count >= opts.effectiveMaxRedispatch() {
		hold := HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA,
			Reason:    fmt.Sprintf("%s: %d review dispatches for this head ended without a verdict", HoldReasonDispatchExhausted, count),
			UpdatedAt: now}
		p.State.Human = upsertHuman(p.State.Human, hold)
		p.Exhausted = append(p.Exhausted, hold)
		return false
	}
	kept := p.State.Pending[:0]
	for _, e := range p.State.Pending {
		if samePRHead(e.Repo, e.Number, e.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) && !e.Dispatched.IsZero() && now.Sub(e.Dispatched) >= ttl {
			continue
		}
		kept = append(kept, e)
	}
	p.State.Pending = kept
	return true
}

// redispatchExhausted reports whether this head already spent its
// MaxRedispatch budget, so the planner neither re-kicks it nor counts another
// expiry against it every cycle.
func (p *DispatchPlan) redispatchExhausted(pr PullRequest) bool {
	for _, h := range p.State.Human {
		if samePRHead(h.Repo, h.Number, h.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) && strings.HasPrefix(h.Reason, HoldReasonDispatchExhausted) {
			return true
		}
	}
	return false
}

func redispatchIndex(items []RedispatchCount, pr PullRequest) int {
	for i, r := range items {
		if samePRHead(r.Repo, r.Number, r.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return i
		}
	}
	return -1
}

// RedispatchCountFor is the number of times this head's pending reviews have
// expired, for the review queue's status line.
func (s DispatchState) RedispatchCountFor(repo string, number int, headSHA string) int {
	for _, r := range s.Redispatches {
		if samePRHead(r.Repo, r.Number, r.HeadSHA, repo, number, headSHA) {
			return r.Count
		}
	}
	return 0
}

// PendingSince is the earliest dispatch time among this head's pending
// reviews, or zero when none is pending.
func (s DispatchState) PendingSince(repo string, number int, headSHA string) time.Time {
	var since time.Time
	for _, p := range s.Pending {
		if !samePRHead(p.Repo, p.Number, p.HeadSHA, repo, number, headSHA) || p.Dispatched.IsZero() {
			continue
		}
		if since.IsZero() || p.Dispatched.Before(since) {
			since = p.Dispatched
		}
	}
	return since
}

// hiveAuthored reports whether this hive is answerable for the PR: its login
// is an agent's (isAgentAuthored), the audit trail attributes it to one of
// this hive's agents (AuthorAgent), its body carries the hive attribution
// trailer (HiveAttributed — a PR an agent opened on a person's credentials),
// or the stage reconciler says the hive adopted the contributor PR in place.
// Anything else was opened by someone the hive does not speak for.
func hiveAuthored(pr PullRequest, opts DispatchOptions) bool {
	return isAgentAuthored(pr.Author, opts.AIAuthor) || strings.TrimSpace(pr.AuthorAgent) != "" || pr.HiveAttributed || hasLabelFold(pr.Labels, stagelabels.AdoptedByHive.Label())
}

// fixPushAllowed is the authorship check in front of every fix kick. A fix
// kick tells an agent to push a commit onto the PR branch, so it is only ever
// dispatched when the PR's LOGIN author is an agent or App login. AllAuthors
// is deliberately not consulted: it widens what is REVIEWED, and reviewing a
// contributor's PR is not a reason to rewrite it (hivecommons/hive#8421).
// review.fix_human_prs is not consulted either. The fixer's commit is signed
// off by the hive, the squash merge is attributed to the PR's login author,
// and the squash sign-off gate skips only when that login is a bot; otherwise
// every sign-off must name the author, so a hive-signed commit fails as
// bot-signoff and the hive cannot repair it (it cannot sign off for a person).
// That holds for a HiveAttributed or AuthorAgent PR under a person login too
// (a contributor-relay PR): the hive is answerable for it, but a hive-signed
// fix would still fail, so the push is withheld and the fix is proposed in the
// review. The contributor relay's fix-pr task is the path that can land a
// correctly signed commit (hivecommons/hive#12897, hivecommons/hive#13124).
// The one person-login exception is a PR the stage reconciler adopted in
// place (`stage/074-adopted-by-hive`): adoption is the explicit decision that the
// hive owns the branch and fixes it there (hivecommons/hive#12987).
func fixPushAllowed(pr PullRequest, opts DispatchOptions) bool {
	return isAgentAuthored(pr.Author, opts.AIAuthor) || hasLabelFold(pr.Labels, stagelabels.AdoptedByHive.Label())
}

// withholdFix records that a changes_requested PR got no fix kick because
// fixPushAllowed refused it. Once per head: the state entry is what stops
// the next cycle re-reporting the same refusal, and the plan entry is what
// the caller audits this cycle.
func (p *DispatchPlan) withholdFix(pr PullRequest, opts DispatchOptions, now time.Time) {
	for _, w := range p.State.Withheld {
		if samePRHead(w.Repo, w.Number, w.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			return
		}
	}
	reason := "PR's login author is not a hive agent; review published, fix not pushed"
	if opts.FixHumanPRs {
		reason = "PR's login author is not a hive agent; a hive-signed fix commit would fail the squash sign-off gate, so the fix was proposed in the review, not pushed"
	}
	w := WithheldFix{
		Repo:           pr.Repo,
		Number:         pr.Number,
		HeadSHA:        pr.HeadSHA,
		Author:         strings.TrimSpace(pr.Author),
		Setting:        WithheldFixSetting,
		SettingEnabled: opts.FixHumanPRs,
		Reason:         reason,
		Withheld:       now,
	}
	p.State.Withheld = append(p.State.Withheld, w)
	p.WithheldFixes = append(p.WithheldFixes, w)
	reason = FixDispatchExcludedNotHiveAuthored
	if opts.FixHumanPRs {
		reason = FixDispatchExcludedNotHiveAuthoredDCO
	}
	p.recordFixExclusion(pr, reason, 0)
}

func (p *DispatchPlan) dispatchFix(pr PullRequest, agg Aggregate, opts DispatchOptions, now time.Time) {
	idx := fixIndex(p.State.Fixes, pr.Repo, pr.Number, pr.HeadSHA)
	pendingAttempts := 0
	if idx >= 0 {
		if !adoptedHiveOwnedFix(pr, opts) && !pendingFixExpired(p.State.Fixes[idx], opts, now) {
			p.recordFixExclusion(pr, FixDispatchExcludedPending, p.State.Fixes[idx].Attempts)
			return
		}
		pendingAttempts = p.State.Fixes[idx].Attempts
		p.State.Fixes = append(p.State.Fixes[:idx], p.State.Fixes[idx+1:]...)
	}
	attempts := maxFixAttemptsForPR(p.State.Fixes, pr.Repo, pr.Number)
	if pendingAttempts > attempts {
		attempts = pendingAttempts
	}
	if historical := maxHistoricalFixAttemptsForPR(p.State.FixAttempts, pr.Repo, pr.Number); historical > attempts {
		attempts = historical
	}
	if attempts >= escalation.MaxReEngagements {
		p.recordFixExclusion(pr, FixDispatchExcludedAttemptsExhausted, attempts)
		p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: FixAttemptsExhaustedHumanDecisionReason, UpdatedAt: now})
		return
	}
	agent := selectFixerAgent(pr, opts)
	if agent == "" {
		p.recordFixExclusion(pr, FixDispatchExcludedNoEnabledFixer, attempts)
		p.State.Human = upsertHuman(p.State.Human, HumanReviewHold{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Reason: "no enabled kick-capable fixer agent is available", UpdatedAt: now})
		return
	}
	attempts++
	msg := BuildFixPrompt(pr, agg, attempts, escalation.MaxReEngagements)
	p.FixKicks = append(p.FixKicks, DispatchKick{Agent: agent, Message: msg, PRRef: fmt.Sprintf("%s#%d", pr.Repo, pr.Number), Kind: "fix", Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA})
	p.State.Fixes = append(p.State.Fixes, PendingFix{Repo: pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Agent: agent, Attempts: attempts, Dispatched: now})
}

// pendingFixExpired reports whether a pending fix outlived PendingFixTTL with
// no head change. A row with no dispatched_at has no measurable age.
func pendingFixExpired(f PendingFix, opts DispatchOptions, now time.Time) bool {
	ttl := opts.PendingFixTTL
	if ttl < 0 || f.Dispatched.IsZero() {
		return false
	}
	if ttl == 0 {
		ttl = DefaultPendingFixTTL
	}
	return now.Sub(f.Dispatched) >= ttl
}

func adoptedHiveOwnedFix(pr PullRequest, opts DispatchOptions) bool {
	return hasLabelFold(pr.Labels, stagelabels.AdoptedByHive.Label()) && hiveAuthored(pr, opts)
}

func (p *DispatchPlan) recordFixExclusion(pr PullRequest, reason string, attempts int) {
	p.FixExclusions = append(p.FixExclusions, FixDispatchExclusion{
		Repo:     pr.Repo,
		Number:   pr.Number,
		HeadSHA:  pr.HeadSHA,
		Author:   strings.TrimSpace(pr.Author),
		Reason:   reason,
		Attempts: attempts,
	})
}

func hasLabelFold(labels []string, want string) bool {
	for _, label := range labels {
		if strings.EqualFold(strings.TrimSpace(label), want) {
			return true
		}
	}
	return false
}

func selectFixerAgent(pr PullRequest, opts DispatchOptions) string {
	candidates := []string{strings.TrimSpace(opts.FixerAgent), strings.TrimSpace(pr.Lane), DefaultFixerAgent}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		for _, a := range opts.Agents {
			if a.Name == candidate && a.Enabled && !a.Paused && !a.OnDemand && a.UsesKick {
				return candidate
			}
		}
	}
	return ""
}

// ConfirmDeliveredInProgress is ConfirmDelivered for a kick pass that is still
// delivering: it records the kicks delivered so far and drops nothing for the
// planned kicks not yet delivered. A kick still being sent may already have
// reached its agent, and the verdict relay binds that agent's verdict to the
// pending rows on disk; dropping them at an intermediate persist bound a
// decision verdict as advisory (hivecommons/hive#15559). The end-of-pass
// persist uses ConfirmDelivered to release the kicks that never landed.
func ConfirmDeliveredInProgress(state DispatchState, delivered []DispatchKick) DispatchState {
	return ConfirmDelivered(state, delivered, delivered)
}

func ConfirmDelivered(state DispatchState, planned, delivered []DispatchKick) DispatchState {
	now := time.Now().UTC()
	deliveredSet := map[string]bool{}
	for _, k := range delivered {
		deliveredSet[dispatchKickKey(k)] = true
		if k.Kind == "review" {
			// One recent entry per perspective the kick covered, not one for
			// the kick. The verdict relay binds a late verdict to a recent
			// entry by perspective, so a combined kick recorded under its
			// first perspective alone would leave every other verdict in the
			// array with nothing to bind to once its pending entry is gone.
			for _, p := range kickPerspectives(k) {
				state.Recent = upsertRecentReview(state.Recent, RecentReview{Repo: k.Repo, Number: k.Number, HeadSHA: k.HeadSHA, BodyDigest: k.BodyDigest, Perspective: p, Agent: k.Agent, Head: k.Head, Decision: k.Decision, AuthorAgent: k.AuthorAgent, HiveAuthored: k.HiveAuthored, AuthorModel: k.AuthorModel, ReviewModel: k.ReviewModel, DeltaPreviousHead: k.DeltaPreviousHead, Confirmed: now})
			}
		}
	}
	// Undelivered is keyed per PERSPECTIVE, because pending entries are. A
	// combined kick wrote one pending entry for each perspective it covered;
	// if it never reached the agent, every one of those must be released or
	// the ones after the first stay "pending" for a review that never
	// happened — never re-dispatched, never judged, and holding the PR's
	// unanimity check open indefinitely.
	undelivered := map[string]bool{}
	for _, k := range planned {
		if deliveredSet[dispatchKickKey(k)] {
			continue
		}
		if k.Kind != "review" {
			undelivered[dispatchKickKey(k)] = true
			continue
		}
		for _, p := range kickPerspectives(k) {
			one := k
			one.Perspective = p
			undelivered[dispatchKickKey(one)] = true
		}
	}
	var pending []PendingReview
	for _, p := range state.Pending {
		k := DispatchKick{Kind: "review", Agent: p.Agent, Repo: p.Repo, Number: p.Number, HeadSHA: p.HeadSHA, Perspective: p.Perspective}
		if !undelivered[dispatchKickKey(k)] {
			pending = append(pending, p)
		}
	}
	state.Pending = pending
	var fixes []PendingFix
	for _, f := range state.Fixes {
		k := DispatchKick{Kind: "fix", Agent: f.Agent, Repo: f.Repo, Number: f.Number, HeadSHA: f.HeadSHA}
		if deliveredSet[dispatchKickKey(k)] && !hasFixAttempt(state.FixAttempts, f) {
			state.FixAttempts = append(state.FixAttempts, FixAttempt{Repo: f.Repo, Number: f.Number, HeadSHA: f.HeadSHA, Agent: f.Agent, Attempts: f.Attempts, Dispatched: f.Dispatched})
		}
		if !undelivered[dispatchKickKey(k)] {
			fixes = append(fixes, f)
		}
	}
	state.Fixes = fixes
	state.Recent = pruneRecentReviews(state.Recent, now)
	return state
}

// kickPerspectives is every perspective a kick covers: the combined list when
// set, else the single Perspective. Never empty for a review kick.
func kickPerspectives(k DispatchKick) []Perspective {
	if len(k.Perspectives) > 0 {
		return k.Perspectives
	}
	return []Perspective{k.Perspective}
}

func upsertRecentReview(items []RecentReview, item RecentReview) []RecentReview {
	for i := range items {
		if recentReviewSameDispatch(items[i], item) {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func pruneRecentReviews(items []RecentReview, now time.Time) []RecentReview {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	cutoff := now.Add(-RecentDispatchTTL)
	out := items[:0]
	for _, item := range items {
		if item.Confirmed.IsZero() || !item.Confirmed.Before(cutoff) {
			out = append(out, item)
		}
	}
	return out
}

func recentReviewSameDispatch(a, b RecentReview) bool {
	return strings.EqualFold(strings.TrimSpace(a.Repo), strings.TrimSpace(b.Repo)) &&
		a.Number == b.Number &&
		strings.TrimSpace(a.HeadSHA) == strings.TrimSpace(b.HeadSHA) &&
		a.Perspective == b.Perspective &&
		a.Agent == b.Agent
}

func BuildFixPrompt(pr PullRequest, agg Aggregate, attempt, maxAttempts int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "[review-fix:%s#%d]\n", pr.Repo, pr.Number)
	fmt.Fprintf(&b, "Fix review findings for PR %s#%d", pr.Repo, pr.Number)
	if pr.Title != "" {
		fmt.Fprintf(&b, " — %s", pr.Title)
	}
	b.WriteString(".\n")
	if pr.URL != "" {
		fmt.Fprintf(&b, "URL: %s\n", pr.URL)
	}
	if pr.HeadSHA != "" {
		fmt.Fprintf(&b, "Head SHA: %s\n", pr.HeadSHA)
	}
	fmt.Fprintf(&b, "Auto-fix attempt %d of %d. Stay narrowly scoped to the review findings below; push a new commit to the PR branch.\n\n", attempt, maxAttempts)
	fmt.Fprintf(&b, "When committing the fix, include this exact git trailer so rework metrics can count the attempt: Hive-Fix-Attempt: %d/%d\n\n", attempt, maxAttempts)
	b.WriteString("REQUIRED: every follow-up commit MUST carry a Signed-off-by trailer — create it with `git commit -s`, and run `git rebase --signoff` before pushing if any new commit lacks one. An unsigned fix commit bounces the PR to no-dco.\n\n")
	if req := pr.HumanChangeRequest; req != nil && strings.TrimSpace(req.Body) != "" {
		b.WriteString(humanChangeRequestPromptSection(*req))
	}
	b.WriteString("Aggregated review findings:\n")
	if len(agg.Findings) == 0 && len(agg.Reasons) == 0 {
		if pr.HumanChangeRequest != nil {
			b.WriteString("- No structured swarm findings for this head; the maintainer instruction above is the whole scope.\n")
		} else {
			b.WriteString("- Review requested changes but did not include structured findings; inspect the PR conversation and latest diff.\n")
		}
	}
	for _, reason := range agg.Reasons {
		fmt.Fprintf(&b, "- %s\n", reason)
	}
	for _, f := range agg.Findings {
		fmt.Fprintf(&b, "- [%s/%s] %s: %s\n", f.Perspective, f.Finding.Severity, f.Finding.Title, f.Finding.Summary)
	}
	b.WriteString("\nReturn the standard fix AgentReport when done. If the findings are unsafe or ambiguous, request human review instead of guessing.\n")
	return b.String()
}

// humanChangeRequestPromptSection renders a maintainer's instruction into
// the fix prompt. The body goes in verbatim: it is the maintainer's words,
// and paraphrasing a conditional answer is how the condition gets lost.
func humanChangeRequestPromptSection(req HumanChangeRequest) string {
	var b strings.Builder
	actor := strings.TrimSpace(req.Actor)
	if actor == "" {
		actor = "maintainer"
	}
	what := "changes-requested review"
	if req.Source == HumanChangeRequestSourceDecision {
		what = "/hive decision"
	}
	fmt.Fprintf(&b, "Maintainer instruction (@%s via %s", actor, what)
	if url := strings.TrimSpace(req.URL); url != "" {
		fmt.Fprintf(&b, ", %s", url)
	}
	b.WriteString("). This is the primary instruction; implement it as written and treat the swarm findings below as secondary:\n")
	for _, line := range strings.Split(strings.TrimRight(req.Body, "\n"), "\n") {
		fmt.Fprintf(&b, "> %s\n", line)
	}
	b.WriteString("\n")
	return b.String()
}

func removeHumanForHead(items []HumanReviewHold, pr PullRequest) []HumanReviewHold {
	kept := items[:0]
	for _, h := range items {
		if samePRHead(h.Repo, h.Number, h.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

func (a Artifact) AggregateFor(repo string, number int, headSHA string) (Aggregate, bool) {
	return a.AggregateForBody(repo, number, headSHA, "")
}

func (a Artifact) AggregateForBody(repo string, number int, headSHA, bodyDigest string) (Aggregate, bool) {
	wantRepo := strings.TrimSpace(repo)
	wantSHA := strings.TrimSpace(headSHA)
	wantBody := strings.TrimSpace(bodyDigest)
	var fallback Aggregate
	var haveFallback bool
	for _, item := range a.Items {
		item = item.Normalize()
		if item.Number != number || strings.TrimSpace(item.Repo) != wantRepo || !SameHeadSHA(item.HeadSHA, wantSHA) {
			continue
		}
		if wantBody != "" && strings.TrimSpace(item.BodyDigest) == wantBody {
			return item, true
		}
		if !haveFallback || item.RecordedAt.After(fallback.RecordedAt) {
			fallback = item
			haveFallback = true
		}
	}
	return fallback, haveFallback
}

// effectiveMaxPerspectivesPerPR returns the per-PR perspective cap, or 0 for
// "no cap". Unlimited is the default deliberately: fanning every perspective
// out at once is the review swarm's designed behavior, and a hive that wants
// depth on each PR should keep getting it. The cap is for the opposite
// situation — a queue too deep to review in depth — and is opt-in so no
// existing hive silently changes shape.
func (o DispatchOptions) effectiveMaxPerspectivesPerPR() int {
	if o.MaxPerspectivesPerPR <= 0 {
		return 0
	}
	return o.MaxPerspectivesPerPR
}

func reviewCapableAgents(opts DispatchOptions) []AgentCapability {
	allowed := ReviewerAgentSet(opts.ReviewerAgents)
	var out []AgentCapability
	for _, a := range opts.Agents {
		if a.Name == "" || !a.Enabled || a.Paused || a.Draining || a.OnDemand || !a.UsesKick {
			continue
		}
		if ReviewCapable(a, allowed) {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ReviewCapable reports whether one agent is a reviewer under the same rule
// the dispatcher uses to hand out review kicks: when review.reviewer_agents
// names lanes, membership in that set decides; otherwise the agent's name,
// role, aliases or keywords must carry the review token. The relay uses it to
// decide whose dispatch-less verdicts may be recorded. A replica is review-
// capable when its base agent is a listed reviewer.
func ReviewCapable(a AgentCapability, reviewerAgents map[string]bool) bool {
	if len(reviewerAgents) > 0 {
		// Naming a base admits its replicas and the heads derived from its
		// review_heads.
		return reviewerAgents[a.Name] || reviewerAgents[baseOf(a)] ||
			(a.ReviewHeadOf != "" && reviewerAgents[a.ReviewHeadOf])
	}
	return hasReviewCapability(a)
}

// ReviewerAgentSet builds the membership set ReviewCapable consults from the
// configured review.reviewer_agents list.
func ReviewerAgentSet(names []string) map[string]bool {
	set := map[string]bool{}
	for _, name := range names {
		if n := strings.TrimSpace(name); n != "" {
			set[n] = true
		}
	}
	return set
}

func hasReviewCapability(a AgentCapability) bool {
	fields := append([]string{a.Name, a.Role}, a.Aliases...)
	fields = append(fields, a.LaneKeywords...)
	fields = append(fields, a.DetectKeywords...)
	for _, f := range fields {
		if strings.Contains(strings.ToLower(f), reviewRoleToken) {
			return true
		}
	}
	return false
}

// pendingMissingPerspectives lists the perspectives this head still needs
// asked: not already dispatched (state.Pending) and not already reported
// (delivered, the perspectives of an incomplete aggregate for this head).
func pendingMissingPerspectives(state DispatchState, pr PullRequest, perspectives []Perspective, delivered map[Perspective]Verdict) []Perspective {
	pending := map[Perspective]bool{}
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			pending[p.Perspective] = true
		}
	}
	for p := range delivered {
		pending[p] = true
	}
	if len(perspectives) == 0 {
		perspectives = DefaultPerspectives
	}
	var missing []Perspective
	for _, p := range perspectives {
		if !pending[p] {
			missing = append(missing, p)
		}
	}
	return missing
}

// revisitInFlight reports whether this head already has a review dispatched
// after the revise cutoff — i.e. the revisit has been kicked and its verdict
// has not yet replaced the stale one.
func revisitInFlight(state DispatchState, pr PullRequest, cutoff time.Time) bool {
	for _, p := range state.Pending {
		if samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) && p.Dispatched.After(cutoff) {
			return true
		}
	}
	return false
}

// removePendingForPerspectives drops this head's pending entries for the
// perspectives that have already reported, leaving the ones still owed so
// their TTL keeps running.
func removePendingForPerspectives(pending []PendingReview, pr PullRequest, reported map[Perspective]Verdict) []PendingReview {
	out := pending[:0]
	for _, p := range pending {
		if _, done := reported[p.Perspective]; done && samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			continue
		}
		out = append(out, p)
	}
	return out
}

func removePendingForHead(pending []PendingReview, pr PullRequest) []PendingReview {
	out := pending[:0]
	for _, p := range pending {
		if !samePRHead(p.Repo, p.Number, p.HeadSHA, pr.Repo, pr.Number, pr.HeadSHA) {
			out = append(out, p)
		}
	}
	return out
}

func pruneState(state DispatchState, prs []PullRequest, org string) DispatchState {
	openHeads := map[string]bool{}
	authorAgents := map[string]string{}
	openPRs := map[string]bool{}
	for _, pr := range prs {
		repo := fullRepoName(pr.Repo, org)
		key := reviewKey(repo, pr.Number, pr.HeadSHA, "")
		openHeads[key] = true
		if strings.TrimSpace(pr.AuthorAgent) != "" {
			authorAgents[key] = pr.AuthorAgent
		}
		openPRs[fmt.Sprintf("%s#%d", repo, pr.Number)] = true
	}
	var pending []PendingReview
	for _, p := range state.Pending {
		key := reviewKey(p.Repo, p.Number, p.HeadSHA, "")
		if openHeads[key] {
			if strings.TrimSpace(p.AuthorAgent) == "" {
				p.AuthorAgent = authorAgents[key]
			}
			if strings.TrimSpace(p.AuthorAgent) != "" && p.Agent == p.AuthorAgent {
				continue
			}
			pending = append(pending, p)
		}
	}
	state.Pending = pending
	state.Recent = pruneRecentReviews(state.Recent, time.Now().UTC())
	var fixes []PendingFix
	for _, f := range state.Fixes {
		if openPRs[fmt.Sprintf("%s#%d", f.Repo, f.Number)] {
			fixes = append(fixes, f)
		}
	}
	state.Fixes = fixes
	var human []HumanReviewHold
	for _, h := range state.Human {
		if openHeads[reviewKey(h.Repo, h.Number, h.HeadSHA, "")] {
			human = append(human, h)
		}
	}
	state.Human = human
	var withheld []WithheldFix
	for _, w := range state.Withheld {
		if openHeads[reviewKey(w.Repo, w.Number, w.HeadSHA, "")] {
			withheld = append(withheld, w)
		}
	}
	state.Withheld = withheld
	var redispatches []RedispatchCount
	for _, r := range state.Redispatches {
		if openHeads[reviewKey(r.Repo, r.Number, r.HeadSHA, "")] {
			redispatches = append(redispatches, r)
		}
	}
	state.Redispatches = redispatches
	var starved []RevisitStarvation
	for _, s := range state.RevisitStarved {
		if openHeads[reviewKey(s.Repo, s.Number, s.HeadSHA, "")] {
			starved = append(starved, s)
		}
	}
	state.RevisitStarved = starved
	var waits []ReviewWaitRecord
	for _, w := range state.Waits {
		if openHeads[reviewKey(w.Repo, w.Number, w.HeadSHA, "")] {
			waits = append(waits, w)
		}
	}
	state.Waits = waits
	state = pruneReviewHeadState(state, openHeads)
	return state
}

func fullRepoName(repo, org string) string {
	if repo == "" || strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

func isAgentAuthored(author, aiAuthor string) bool {
	author = strings.TrimSpace(author)
	return author != "" && (strings.EqualFold(author, strings.TrimSpace(aiAuthor)) || strings.HasSuffix(author, "[bot]"))
}

func samePRHead(repo string, number int, sha string, wantRepo string, wantNumber int, wantSHA string) bool {
	return number == wantNumber && strings.TrimSpace(repo) == strings.TrimSpace(wantRepo) && SameHeadSHA(sha, wantSHA)
}

func fixIndex(fixes []PendingFix, repo string, number int, headSHA string) int {
	for i, f := range fixes {
		if samePRHead(f.Repo, f.Number, f.HeadSHA, repo, number, headSHA) {
			return i
		}
	}
	return -1
}

func maxFixAttemptsForPR(fixes []PendingFix, repo string, number int) int {
	max := 0
	for _, f := range fixes {
		if f.Repo == repo && f.Number == number && f.Attempts > max {
			max = f.Attempts
		}
	}
	return max
}

func maxHistoricalFixAttemptsForPR(fixes []FixAttempt, repo string, number int) int {
	max := 0
	for _, f := range fixes {
		if f.Repo == repo && f.Number == number && f.Attempts > max {
			max = f.Attempts
		}
	}
	return max
}

func hasFixAttempt(items []FixAttempt, pending PendingFix) bool {
	for _, item := range items {
		if item.Repo == pending.Repo && item.Number == pending.Number && item.HeadSHA == pending.HeadSHA && item.Agent == pending.Agent && item.Attempts == pending.Attempts {
			return true
		}
	}
	return false
}

func upsertHuman(items []HumanReviewHold, item HumanReviewHold) []HumanReviewHold {
	for i := range items {
		if samePRHead(items[i].Repo, items[i].Number, items[i].HeadSHA, item.Repo, item.Number, item.HeadSHA) {
			items[i] = item
			return items
		}
	}
	return append(items, item)
}

func dispatchKickKey(k DispatchKick) string {
	return fmt.Sprintf("%s|%s|%s#%d@%s|%s", k.Kind, k.Agent, strings.TrimSpace(k.Repo), k.Number, strings.TrimSpace(k.HeadSHA), k.Perspective)
}

// staleVerdictRevisitable reports whether an existing verdict should be set
// aside so the PR is reviewed again, despite its head SHA being unchanged.
//
// Both gates must pass. The repo must be allowlisted for revision, so a
// revisit can only happen where the hive also holds the quieter in-place
// correction path and cannot stack a second review on someone's PR. And the
// verdict must predate the configured cutoff, which is what scopes the
// correction to verdicts produced by the reviewer that was wrong.
//
// The cutoff is self-limiting: a complete re-review records a fresh timestamp
// that is necessarily after it, so a PR is revisited at most once per bump
// rather than entering a loop. An incomplete re-review of a requires_human
// head keeps the prior verdict and its RecordedAt (see CollectAndMerge), so
// that PR stays revisitable until full perspective coverage replaces it.
func staleVerdictRevisitable(repo string, agg Aggregate, opts DispatchOptions) bool {
	if opts.ReviseVerdictsBefore.IsZero() || len(opts.ReviseRepos) == 0 {
		return false
	}
	if !reviseRepoAllowed(repo, opts.ReviseRepos) {
		return false
	}
	if agg.RecordedAt.IsZero() {
		// An undated verdict cannot be shown to predate the cutoff. Leave it
		// alone rather than guess: re-reviewing on a guess is how a narrow
		// correction turns into a sweep.
		return false
	}
	return agg.RecordedAt.Before(opts.ReviseVerdictsBefore)
}

// reviseRepoAllowed reports whether a repo is in the revision allowlist.
func reviseRepoAllowed(repo string, allowlist []string) bool {
	want := strings.ToLower(strings.TrimSpace(repo))
	if want == "" {
		return false
	}
	for _, entry := range allowlist {
		if strings.EqualFold(strings.TrimSpace(entry), want) {
			return true
		}
	}
	return false
}
