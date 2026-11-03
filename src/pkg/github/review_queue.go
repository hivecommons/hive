package github

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/redmain"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// The PR review queue (hivecommons/hive#9590) is one ranked, maintainer-facing
// list of every open PR in the governed repos, whoever opened it. It joins the
// signals that already exist but live apart - the triage class
// (review_priority.go, #6183), the review swarm's 0-5 confidence score
// (pkg/review/confidence.go, #8182), CI state and age - into a single order,
// and records why each PR landed where it did.
//
// The rank is COMPUTED, never model-asserted: BuildReviewQueue is a pure
// function of the PRs and the verdict artifact, so the same inputs always
// produce the same order.

// ReviewPriority is the coarse, label-sized form of a PR's place in the queue.
type ReviewPriority string

const (
	// ReviewPriorityHigh is T0: fixes.
	ReviewPriorityHigh ReviewPriority = "high"
	// ReviewPriorityNormal is T1: refactors, docs, and unclassified PRs.
	ReviewPriorityNormal ReviewPriority = "normal"
	// ReviewPriorityLow is T2: additive tests.
	ReviewPriorityLow ReviewPriority = "low"

	// ReviewPriorityLabelPrefix namespaces the optional mechanical label
	// (review.priority_labels): review-priority/high|normal|low.
	ReviewPriorityLabelPrefix = "review-priority/"
)

// Label is the repo label that carries this priority.
func (p ReviewPriority) Label() string { return ReviewPriorityLabelPrefix + string(p) }

// reviewPriorityFor maps the triage class to a priority. The class is the
// queue's PRIMARY sort key, so a priority derived from it alone is monotonic
// along the ranked order: a label can never claim a PR is more urgent than
// one ranked above it.
func reviewPriorityFor(c ReviewClass) ReviewPriority {
	switch reviewRank(c) {
	case reviewRankFix:
		return ReviewPriorityHigh
	case reviewRankTests:
		return ReviewPriorityLow
	default:
		return ReviewPriorityNormal
	}
}

// Tier names as the triage doc uses them.
const (
	reviewTierFix          = "T0"
	reviewTierRefactorDocs = "T1"
	reviewTierTests        = "T2"
)

func reviewTier(c ReviewClass) string {
	switch reviewRank(c) {
	case reviewRankFix:
		return reviewTierFix
	case reviewRankTests:
		return reviewTierTests
	default:
		return reviewTierRefactorDocs
	}
}

// Confidence bands, as review.ConfidenceBand names them. An unreviewed PR has
// no score and is placed in the needs-attention band: an unreviewed change is
// unknown risk, never "safe".
const (
	ReviewQueueBandSafe           = "safe"
	ReviewQueueBandNeedsAttention = "needs attention"
	ReviewQueueBandDoNotMerge     = "do not merge"
)

// Band sort order: lower first. Safe PRs lead within a class because review
// minutes spent there convert to merges soonest; do-not-merge trails.
const (
	reviewBandRankSafe           = 0
	reviewBandRankNeedsAttention = 1
	reviewBandRankDoNotMerge     = 2
)

func reviewBandRank(band string) int {
	switch band {
	case ReviewQueueBandSafe:
		return reviewBandRankSafe
	case ReviewQueueBandDoNotMerge:
		return reviewBandRankDoNotMerge
	default:
		return reviewBandRankNeedsAttention
	}
}

// CI states in the queue. GitHub's raw CIStatus ("success" / "failure" /
// "pending" / unset) is folded to three: anything not settled green or red
// is pending.
const (
	ReviewQueueCIGreen   = "green"
	ReviewQueueCIPending = "pending"
	ReviewQueueCIRed     = "red"

	reviewCIStatusSuccess = "success"
	reviewCIStatusFailure = "failure"

	reviewCIRankGreen   = 0
	reviewCIRankPending = 1
	reviewCIRankRed     = 2
)

func reviewCIState(pr PullRequest) string {
	switch pr.CIStatus {
	case reviewCIStatusSuccess:
		return ReviewQueueCIGreen
	case reviewCIStatusFailure:
		return ReviewQueueCIRed
	default:
		return ReviewQueueCIPending
	}
}

func reviewCIRank(state string) int {
	switch state {
	case ReviewQueueCIGreen:
		return reviewCIRankGreen
	case ReviewQueueCIRed:
		return reviewCIRankRed
	default:
		return reviewCIRankPending
	}
}

const (
	// reviewQueueHoursPerDay converts an age in hours to days for reasons.
	reviewQueueHoursPerDay = 24
	// reviewQueueDaysAfterHours is the age at which reasons switch from
	// hours ("open 30h") to days ("open 3d").
	reviewQueueDaysAfterHours = 48
	// reviewQueueMergeableStateDirty is GitHub's mergeable_state for a PR
	// with conflicts against its base.
	reviewQueueMergeableStateDirty = "dirty"
	// reviewQueueBotLoginSuffix marks a GitHub App / bot login.
	reviewQueueBotLoginSuffix = "[bot]"
)

// ReviewQueueEntry is one PR in the ranked queue.
type ReviewQueueEntry struct {
	// Position is the 1-based place in the whole queue (not the page).
	Position  int       `json:"position"`
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author,omitempty"`
	URL       string    `json:"url,omitempty"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	CreatedAt time.Time `json:"created_at,omitzero"`
	AgeHours  int       `json:"age_hours"`
	Labels    []string  `json:"labels,omitempty"`
	// HiveAuthored is true when a hive agent opened the PR (the App login, the
	// configured AI author, a bot login, the attribution trailer, or an agent
	// lane label). False means an outside contributor or maintainer.
	HiveAuthored bool           `json:"hive_authored"`
	Held         bool           `json:"held,omitempty"`
	Class        ReviewClass    `json:"review_class"`
	Tier         string         `json:"tier"`
	Priority     ReviewPriority `json:"priority"`
	// Reviewed is true when the review swarm has an aggregate verdict for the
	// PR's CURRENT head. ConfidenceScore is set only then.
	Reviewed        bool   `json:"reviewed"`
	ConfidenceScore *int   `json:"confidence_score,omitempty"`
	ConfidenceBand  string `json:"confidence_band"`
	CIState         string `json:"ci_state"`
	// PendingSince is when the review swarm first dispatched a still-pending
	// review for the PR's CURRENT head, so an unreviewed PR reads as "review
	// dispatched Xm ago, no verdict" rather than merely not reviewed. Zero
	// when nothing is pending for this head. RedispatchCount is how many
	// times that head's pending reviews expired (review.pending_ttl) and
	// were asked again; at review.max_redispatch the PR is held for a human.
	PendingSince    time.Time `json:"pending_since,omitzero"`
	RedispatchCount int       `json:"redispatch_count,omitempty"`
	// Reasons explains the rank, in sort-key order (class, confidence, CI,
	// age), then any context that did not move it. Like Confidence.Reasons,
	// it is what a reader needs to reconstruct the order without the code.
	Reasons []string `json:"reasons"`
	// Superseded is set when the supersession sweep has announced it will
	// close this PR once its grace window ends (hivecommons/hive#11430).
	Superseded *ReviewQueueSuperseded `json:"superseded,omitempty"`
	// Stage is the stage-label engine's record for the PR (nil when the
	// engine is off or has not seen it): the stage, how long it has been
	// quiet, whether it waits on the hive, and its transition history —
	// the dashboard's replacement for the retired stage/regressed label
	// (hivecommons/hive#12534).
	Stage *ReviewQueueStage `json:"stage,omitempty"`
	// FixesMain is set when the PR fixes its repo's red default branch
	// (red-main circuit breaker, hivecommons/hive#13630); such PRs rank
	// ahead of every other PR. FixesMainVia names the signal (label,
	// tracking-issue, file-overlap).
	FixesMain    bool   `json:"fixes_main,omitempty"`
	FixesMainVia string `json:"fixes_main_via,omitempty"`
}

// ReviewQueueStage is ReviewQueueEntry.Stage.
type ReviewQueueStage struct {
	Stage          string    `json:"stage"`
	EnteredAt      time.Time `json:"entered_at,omitzero"`
	LastActivityAt time.Time `json:"last_activity_at,omitzero"`
	// QuietFor is how long nobody but the hive has touched the PR (Go
	// duration string, "" when unknown).
	QuietFor string `json:"quiet_for,omitempty"`
	// Stalled mirrors the stage/stalled label as the engine last decided it.
	Stalled bool `json:"stalled"`
	// WaitingOnHive says the PR waits on the hive (checks running, verdict
	// pending) and so cannot stall; when it sits past the stage's dwell the
	// signal belongs on the hive's own lane, not on the PR.
	WaitingOnHive bool `json:"waiting_on_hive"`
	// PastDwell is true when the PR has been quiet longer than the stage's
	// dwell, whichever side is holding it. With WaitingOnHive it is the
	// hive-held alert: the reviewer lane, not the author, needs attention.
	PastDwell bool                  `json:"past_dwell"`
	History   []ReviewQueueStageHop `json:"history,omitempty"`
}

// ReviewQueueStageHop is one stage transition.
type ReviewQueueStageHop struct {
	From      string    `json:"from"`
	To        string    `json:"to"`
	At        time.Time `json:"at"`
	Reason    string    `json:"reason,omitempty"`
	Regressed bool      `json:"regressed,omitempty"`
}

// reviewQueueStage projects the stage store's record for a PR, deciding
// stalled/past_dwell with the same dwell overrides and require_quiet
// policy the engine labels with (ReviewQueueOptions.StageDwell /
// StageStalledRequireQuiet) so the pill never disagrees with the label.
func reviewQueueStage(store *stagelabels.Store, full string, number int, opts ReviewQueueOptions, now time.Time) *ReviewQueueStage {
	if store == nil {
		return nil
	}
	rec, ok := store.Get(stagelabels.Key(full, number))
	if !ok || rec.Stage == stagelabels.None {
		return nil
	}
	out := &ReviewQueueStage{
		Stage:          rec.Stage.String(),
		EnteredAt:      rec.EnteredAt,
		LastActivityAt: rec.LastActivityAt,
		WaitingOnHive:  rec.WaitingOnHive,
		Stalled:        stagelabels.StalledWith(rec, opts.StageDwell, now, opts.stalledRequireQuiet()),
	}
	if q := rec.QuietFor(now); q > 0 {
		out.QuietFor = q.Truncate(time.Minute).String()
		if d := stagelabels.Dwell(rec.Stage, opts.StageDwell); d > 0 && q >= d {
			out.PastDwell = true
		}
	}
	for _, h := range rec.History {
		out.History = append(out.History, ReviewQueueStageHop{From: h.From.String(), To: h.Stage.String(), At: h.At, Reason: h.Reason, Regressed: h.Regressed})
	}
	return out
}

// ReviewQueueSupersededReason is ReviewQueueSuperseded.Reason when the PR is
// a subset of the merged PR (or the audit record predates the outcome field).
const ReviewQueueSupersededReason = "superseded"

// ReviewQueueSupersededExtraFilesReason is ReviewQueueSuperseded.Reason when
// the PR touches files the merged PR did not, so keeping it open is recommended.
const ReviewQueueSupersededExtraFilesReason = "superseded_extra_files"

// ReviewQueueSuperseded describes a PR whose claimed issue was closed by a
// different merged PR. Grace timestamps are present only for automatic closure.
type ReviewQueueSuperseded struct {
	KeepReason     string    `json:"keep_reason,omitempty"`
	Reason         string    `json:"reason"`
	Issue          string    `json:"issue,omitempty"`
	CloserRepo     string    `json:"closer_repo,omitempty"`
	CloserPR       int       `json:"closer_pr"`
	CloserURL      string    `json:"closer_url,omitempty"`
	ExtraFiles     []string  `json:"extra_files,omitempty"`
	GraceStartedAt time.Time `json:"grace_started_at,omitzero"`
	GraceEndsAt    time.Time `json:"grace_ends_at,omitzero"`
}

// ReviewQueueOptions carries the inputs BuildReviewQueue needs besides the PRs.
type ReviewQueueOptions struct {
	// Org qualifies bare repo names ("hive" -> "hivecommons/hive"). Verdicts
	// are recorded under GitHub's full name while governor.repos may be bare.
	Org string
	// AIAuthor is the configured agent login (config.EffectiveAIAuthor).
	AIAuthor string
	// Verdicts is the review swarm's verdict artifact.
	Verdicts review.Artifact
	// ChangedPaths optionally supplies a PR's changed files for the
	// contributor path fallback; nil or an empty result means "unknown".
	// It must not call GitHub: ranking runs on every eval cycle and on every
	// API request.
	ChangedPaths func(fullRepo string, number int, headSHA string) []string
	// Dispatch is the review swarm's dispatch state; it supplies
	// PendingSince and RedispatchCount. The zero value means unknown.
	Dispatch review.DispatchState
	// Held marks PRs (by ReviewQueueKey) that sit on a hold label.
	Held map[string]bool
	// StageStore, when set, supplies each entry's Stage (stage-label engine
	// record). nil leaves Stage unset.
	StageStore *stagelabels.Store
	// StageDwell is governor.labels.stage.dwell (per-slug overrides) and
	// StageStalledRequireQuiet is governor.labels.stage.stalled.require_quiet
	// as configured (nil when unset): the policy the engine labels
	// stage/stalled with. The zero values are the engine's defaults (table
	// dwell, quiet required), so a caller without config still agrees with
	// a default-configured engine.
	StageDwell               map[string]time.Duration
	StageStalledRequireQuiet *bool
	// RedMain is the per-repo red-main facts (ActionableResult.RedMain);
	// PRs that fix a red default branch rank first. nil means no repo is
	// red, and ReviewQueueFromActionable fills it from the snapshot.
	RedMain map[string]*redmain.Fact
	// Now is the reference time for ages. Zero means time.Now().
	Now time.Time
}

// stalledRequireQuiet resolves StageStalledRequireQuiet with the engine's
// default (config.StageStalledConfig.IsRequireQuiet): unset means required.
func (o ReviewQueueOptions) stalledRequireQuiet() bool {
	return o.StageStalledRequireQuiet == nil || *o.StageStalledRequireQuiet
}

// ReviewQueueKey is the case-insensitive identity of a PR in the queue.
func ReviewQueueKey(fullRepo string, number int) string {
	return strings.ToLower(fmt.Sprintf("%s#%d", fullRepo, number))
}

func reviewQueueFullRepo(repo, org string) string {
	repo = strings.TrimSpace(repo)
	org = strings.TrimSpace(org)
	if repo == "" || strings.Contains(repo, "/") || org == "" {
		return repo
	}
	return org + "/" + repo
}

func reviewQueueHiveAuthored(pr PullRequest, aiAuthor string) bool {
	author := strings.ToLower(strings.TrimSpace(pr.Author))
	if pr.AppAuthored || pr.HiveAttributed || hasAgentLaneLabel(pr.Labels) {
		return true
	}
	if author == "" {
		return false
	}
	if strings.HasSuffix(author, reviewQueueBotLoginSuffix) {
		return true
	}
	return aiAuthor != "" && author == strings.ToLower(strings.TrimSpace(aiAuthor))
}

// BuildReviewQueue ranks prs into the review queue: PRs that fix a red
// default branch first (FixesMain, hivecommons/hive#13630), then by triage class (T0 fix >
// T1 refactor/docs/unclassified > T2 tests), then confidence band (safe >
// needs attention, including unreviewed > do not merge), then CI (green >
// pending > red), then age (oldest first), then repo and number so the order
// is total. prs may repeat a PR (e.g. the actionable and held lists); the
// first occurrence wins. The input slice is not modified.
func BuildReviewQueue(prs []PullRequest, opts ReviewQueueOptions) []ReviewQueueEntry {
	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	seen := make(map[string]bool, len(prs))
	entries := make([]ReviewQueueEntry, 0, len(prs))
	for _, pr := range prs {
		full := reviewQueueFullRepo(pr.Repo, opts.Org)
		if full == "" || pr.Number <= 0 {
			continue
		}
		key := ReviewQueueKey(full, pr.Number)
		if seen[key] {
			continue
		}
		seen[key] = true
		entries = append(entries, buildReviewQueueEntry(pr, full, key, opts, now))
	}
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.FixesMain != b.FixesMain {
			return a.FixesMain
		}
		if ra, rb := reviewRank(a.Class), reviewRank(b.Class); ra != rb {
			return ra < rb
		}
		if ba, bb := reviewBandRank(a.ConfidenceBand), reviewBandRank(b.ConfidenceBand); ba != bb {
			return ba < bb
		}
		if ca, cb := reviewCIRank(a.CIState), reviewCIRank(b.CIState); ca != cb {
			return ca < cb
		}
		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Before(b.CreatedAt)
		}
		if ka, kb := strings.ToLower(a.Repo), strings.ToLower(b.Repo); ka != kb {
			return ka < kb
		}
		return a.Number < b.Number
	})
	for i := range entries {
		entries[i].Position = i + 1
	}
	return entries
}

func buildReviewQueueEntry(pr PullRequest, full, key string, opts ReviewQueueOptions, now time.Time) ReviewQueueEntry {
	e := ReviewQueueEntry{
		Repo:         full,
		Number:       pr.Number,
		Title:        pr.Title,
		Author:       pr.Author,
		URL:          pr.URL,
		HeadSHA:      pr.HeadSHA,
		CreatedAt:    pr.CreatedAt,
		Labels:       pr.Labels,
		HiveAuthored: reviewQueueHiveAuthored(pr, opts.AIAuthor),
		Held:         opts.Held[key],
		CIState:      reviewCIState(pr),
		Stage:        reviewQueueStage(opts.StageStore, full, pr.Number, opts, now),
	}
	if !pr.CreatedAt.IsZero() && now.After(pr.CreatedAt) {
		e.AgeHours = int(now.Sub(pr.CreatedAt).Hours())
	}

	if fact := RedMainFactFor(opts.RedMain, full); fact != nil {
		var fixPaths []string
		if opts.ChangedPaths != nil {
			fixPaths = opts.ChangedPaths(full, pr.Number, pr.HeadSHA)
		}
		if fix, ok := RedMainFixPR(fact, pr, fixPaths); ok {
			e.FixesMain = true
			e.FixesMainVia = fix.Via
			reason := fmt.Sprintf("fixes red main (%s red since %s; via %s)", fact.Check, fact.Since.UTC().Format(time.RFC3339), fix.Via)
			if fix.Confirmed {
				reason += "; confirmed: its checks pass while main fails"
			}
			e.Reasons = append(e.Reasons, reason)
		}
	}

	var paths []string
	if opts.ChangedPaths != nil && !hasAgentLaneLabel(pr.Labels) {
		paths = opts.ChangedPaths(full, pr.Number, pr.HeadSHA)
	}
	class, source := classifyContributorReviewClassWithSource(pr.Title, pr.Labels, paths)
	e.Class = class
	e.Tier = reviewTier(class)
	e.Priority = reviewPriorityFor(class)
	e.Reasons = append(e.Reasons, reviewClassReason(e.Tier, class, source))

	agg, reviewed := opts.Verdicts.AggregateFor(full, pr.Number, pr.HeadSHA)
	if !reviewed && full != pr.Repo {
		agg, reviewed = opts.Verdicts.AggregateFor(pr.Repo, pr.Number, pr.HeadSHA)
	}
	// A verdict without a head SHA cannot be tied to what is on the PR now,
	// and neither can a PR whose head is unknown.
	if strings.TrimSpace(pr.HeadSHA) == "" {
		reviewed = false
	}
	if reviewed {
		score := agg.Confidence.Score
		e.Reviewed = true
		e.ConfidenceScore = &score
		e.ConfidenceBand = review.ConfidenceBand(score)
		reason := fmt.Sprintf("confidence %d/%d (%s)", score, review.ConfidenceMax, e.ConfidenceBand)
		if len(agg.Confidence.Reasons) > 0 {
			reason += ": " + strings.Join(agg.Confidence.Reasons, "; ")
		}
		e.Reasons = append(e.Reasons, reason)
		// An incomplete aggregate is a review still being collected, not a
		// settled verdict: say what is missing and whether it is dispatched.
		if agg.Verdict == review.VerdictIncomplete {
			e.Reasons = append(e.Reasons, agg.Reasons...)
			if reason := reviewQueueDispatchReason(&e, pr, full, opts, now); reason != "" {
				e.Reasons = append(e.Reasons, reason)
			}
		}
	} else {
		e.ConfidenceBand = ReviewQueueBandNeedsAttention
		e.Reasons = append(e.Reasons, "not reviewed at this head: confidence unknown, ranked as needs attention")
		if reason := reviewQueueDispatchReason(&e, pr, full, opts, now); reason != "" {
			e.Reasons = append(e.Reasons, reason)
		}
	}

	switch e.CIState {
	case ReviewQueueCIGreen:
		e.Reasons = append(e.Reasons, "CI green")
	case ReviewQueueCIRed:
		if len(pr.FailingChecks) > 0 {
			e.Reasons = append(e.Reasons, "CI red: "+strings.Join(pr.FailingChecks, ", "))
		} else {
			e.Reasons = append(e.Reasons, "CI red")
		}
	default:
		e.Reasons = append(e.Reasons, "CI pending")
	}

	if pr.CreatedAt.IsZero() {
		e.Reasons = append(e.Reasons, "age unknown")
	} else {
		e.Reasons = append(e.Reasons, "open "+reviewQueueAge(e.AgeHours))
	}

	if !e.HiveAuthored {
		e.Reasons = append(e.Reasons, "contributor PR (not opened by a hive agent)")
	}
	if e.Held {
		e.Reasons = append(e.Reasons, "on hold")
	}
	if pr.MergeableState == reviewQueueMergeableStateDirty {
		e.Reasons = append(e.Reasons, "has merge conflicts")
	}
	return e
}

func reviewClassReason(tier string, class ReviewClass, source string) string {
	if class == ReviewClassUnknown {
		return fmt.Sprintf("%s unclassified (%s; ranks with refactors/docs)", tier, source)
	}
	return fmt.Sprintf("%s %s (from %s)", tier, class, source)
}

func reviewQueueAge(hours int) string {
	if hours < reviewQueueDaysAfterHours {
		return fmt.Sprintf("%dh", hours)
	}
	return fmt.Sprintf("%dd", hours/reviewQueueHoursPerDay)
}

// reviewQueueDispatchReason fills PendingSince and RedispatchCount from the
// dispatch state and returns the reason line that explains an unreviewed PR's
// dispatch status: a review is in flight, it has been re-dispatched after
// expiring, or the retry budget is spent and the PR waits on a human. Empty
// when the swarm has nothing recorded for this head.
func reviewQueueDispatchReason(e *ReviewQueueEntry, pr PullRequest, full string, opts ReviewQueueOptions, now time.Time) string {
	if strings.TrimSpace(pr.HeadSHA) == "" {
		return ""
	}
	since := opts.Dispatch.PendingSince(full, pr.Number, pr.HeadSHA)
	count := opts.Dispatch.RedispatchCountFor(full, pr.Number, pr.HeadSHA)
	if since.IsZero() && full != pr.Repo {
		since = opts.Dispatch.PendingSince(pr.Repo, pr.Number, pr.HeadSHA)
		count = opts.Dispatch.RedispatchCountFor(pr.Repo, pr.Number, pr.HeadSHA)
	}
	e.PendingSince = since
	e.RedispatchCount = count
	var exhausted string
	for _, h := range opts.Dispatch.Human {
		if h.Number == pr.Number && review.SameHeadSHA(h.HeadSHA, pr.HeadSHA) &&
			(strings.EqualFold(strings.TrimSpace(h.Repo), full) || strings.EqualFold(strings.TrimSpace(h.Repo), pr.Repo)) &&
			strings.HasPrefix(h.Reason, review.HoldReasonDispatchExhausted) {
			exhausted = h.Reason
			break
		}
	}
	if exhausted != "" {
		return "review dispatch exhausted: " + strings.TrimPrefix(exhausted, review.HoldReasonDispatchExhausted+": ") + "; waiting on a human"
	}
	wait, waiting := opts.Dispatch.WaitFor(full, pr.Number, pr.HeadSHA)
	if !waiting && full != pr.Repo {
		wait, waiting = opts.Dispatch.WaitFor(pr.Repo, pr.Number, pr.HeadSHA)
	}
	if since.IsZero() {
		if waiting {
			return reviewQueueWaitReason(wait, now)
		}
		return ""
	}
	reason := fmt.Sprintf("review dispatched %s ago, no verdict yet", reviewQueueDispatchAge(now.Sub(since)))
	if count > 0 {
		reason += fmt.Sprintf(" (re-dispatched %dx after expiry)", count)
	}
	if waiting {
		reason += "; " + reviewQueueWaitReason(wait, now)
	}
	return reason
}

// reviewQueueWaitReason explains a head the planner keeps passing over: the
// perspectives it still owes have had no reviewer for this many consecutive
// cycles (hivecommons/hive#15229). Past review.ReviewWaitStarvationCycles the
// planner walks the head ahead of the queue, and the line says so.
func reviewQueueWaitReason(w review.ReviewWaitRecord, now time.Time) string {
	cause := "every eligible reviewer busy"
	if w.Reason == review.ReviewWaitNoParallelSlot {
		cause = "max_parallel_reviews spent"
	}
	reason := fmt.Sprintf("review waiting for a free reviewer: %d consecutive cycle", w.Cycles)
	if w.Cycles != 1 {
		reason += "s"
	}
	if !w.FirstAt.IsZero() {
		reason += fmt.Sprintf(" over %s", reviewQueueDispatchAge(now.Sub(w.FirstAt)))
	}
	reason += " (" + cause + ")"
	if w.Cycles >= review.ReviewWaitStarvationCycles {
		reason += "; promoted ahead of the queue, takes the next free reviewer"
	}
	return reason
}

// reviewQueueDispatchAge renders how long a review has been in flight:
// whole minutes under an hour, then reviewQueueAge's hours/days.
func reviewQueueDispatchAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return reviewQueueAge(int(d.Hours()))
}

// ReviewQueueFromActionable ranks every open PR the enumeration saw - the
// actionable list and the held list, which at ACMM L5 hold-gated IS the
// human review queue - into one queue.
func ReviewQueueFromActionable(actionable *ActionableResult, opts ReviewQueueOptions) []ReviewQueueEntry {
	if actionable == nil {
		return nil
	}
	prs := make([]PullRequest, 0, len(actionable.PRs.Items)+len(actionable.PRs.Held))
	prs = append(prs, actionable.PRs.Items...)
	prs = append(prs, actionable.PRs.Held...)
	held := make(map[string]bool, len(actionable.PRs.Held)+len(opts.Held))
	for k, v := range opts.Held {
		held[k] = v
	}
	for _, pr := range actionable.PRs.Held {
		held[ReviewQueueKey(reviewQueueFullRepo(pr.Repo, opts.Org), pr.Number)] = true
	}
	opts.Held = held
	if opts.RedMain == nil {
		opts.RedMain = actionable.RedMain
	}
	return BuildReviewQueue(prs, opts)
}

// StampReviewQueue ranks the actionable snapshot's PRs and writes each PR's
// queue position, priority and reasons onto it (ReviewRank, ReviewPriority,
// ReviewRankReasons), so last-actionable.json carries the rank next to
// review_class. The lists keep their existing order: the stamp is additive
// and nothing that reads Items or Held changes behaviour. It returns the
// queue it stamped.
func StampReviewQueue(actionable *ActionableResult, opts ReviewQueueOptions) []ReviewQueueEntry {
	queue := ReviewQueueFromActionable(actionable, opts)
	if len(queue) == 0 {
		return queue
	}
	byKey := make(map[string]ReviewQueueEntry, len(queue))
	for _, e := range queue {
		byKey[ReviewQueueKey(e.Repo, e.Number)] = e
	}
	stamp := func(prs []PullRequest) {
		for i := range prs {
			e, ok := byKey[ReviewQueueKey(reviewQueueFullRepo(prs[i].Repo, opts.Org), prs[i].Number)]
			if !ok {
				continue
			}
			prs[i].ReviewRank = e.Position
			prs[i].ReviewPriority = e.Priority
			prs[i].ReviewRankReasons = e.Reasons
		}
	}
	stamp(actionable.PRs.Items)
	stamp(actionable.PRs.Held)
	return queue
}

// DefaultReviewPriorityLabelMaxChanges bounds how many PRs one eval cycle may
// relabel. Turning review.priority_labels on over a deep queue would
// otherwise relabel hundreds of PRs in one burst; a cap drains it over a few
// cycles instead, and every cycle is idempotent.
const DefaultReviewPriorityLabelMaxChanges = 20

// ReviewPriorityLabelChange is one PR whose review-priority label is out of
// step with its rank.
type ReviewPriorityLabelChange struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	// Add is the label to apply; empty when the PR already carries it.
	Add string `json:"add,omitempty"`
	// Remove lists stale review-priority/* labels on the PR.
	Remove []string `json:"remove,omitempty"`
}

// PlanReviewPriorityLabels compares each entry's priority with the
// review-priority/* labels it already carries and returns the changes needed
// so that every PR carries exactly the one label matching its rank. PRs
// already in step cost nothing. At most maxChanges PRs are returned (<= 0
// means DefaultReviewPriorityLabelMaxChanges), in queue order, so the top of
// the queue is corrected first.
func PlanReviewPriorityLabels(queue []ReviewQueueEntry, maxChanges int) []ReviewPriorityLabelChange {
	if maxChanges <= 0 {
		maxChanges = DefaultReviewPriorityLabelMaxChanges
	}
	var changes []ReviewPriorityLabelChange
	for _, e := range queue {
		if len(changes) >= maxChanges {
			break
		}
		want := e.Priority.Label()
		has := false
		var remove []string
		for _, l := range e.Labels {
			trimmed := strings.TrimSpace(l)
			if !strings.HasPrefix(strings.ToLower(trimmed), ReviewPriorityLabelPrefix) {
				continue
			}
			if strings.EqualFold(trimmed, want) {
				has = true
				continue
			}
			remove = append(remove, trimmed)
		}
		if has && len(remove) == 0 {
			continue
		}
		ch := ReviewPriorityLabelChange{Repo: e.Repo, Number: e.Number, Remove: remove}
		if !has {
			ch.Add = want
		}
		changes = append(changes, ch)
	}
	return changes
}

// CachedPRChangedPaths returns a PR's changed files from the duplicate
// sweep's head-SHA-keyed cache, or nil when the sweep has not fingerprinted
// that head. It never calls GitHub, so it is safe as
// ReviewQueueOptions.ChangedPaths.
func CachedPRChangedPaths(fullRepo string, number int, headSHA string) []string {
	if headSHA == "" || !strings.Contains(fullRepo, "/") {
		return nil
	}
	key := fmt.Sprintf("%s#%d@%s", fullRepo, number, headSHA)
	dupSweepCacheMu.Lock()
	defer dupSweepCacheMu.Unlock()
	fp, ok := dupSweepCache[key]
	if !ok {
		return nil
	}
	return append([]string(nil), fp.files...)
}
