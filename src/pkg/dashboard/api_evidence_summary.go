package dashboard

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

const evidenceSummaryModelUnknown = "unknown"

// evidenceSummaryBuckets are the confidence calibration buckets, lower bound
// inclusive at the midpoint between rubric bins. Legacy values use the
// nearest bin, including values below 0.55 and above 0.95.
var evidenceSummaryBuckets = []struct {
	name string
	low  float64
}{{"0.55", 0}, {"0.65", 0.6}, {"0.75", 0.7}, {"0.85", 0.8}, {"0.95", 0.9}}

// EvidenceSummary is the GET /api/evidence/summary response. It is read-only
// reporting over stored evidence bundles; nothing here gates or alters a merge.
type EvidenceSummary struct {
	Hours   int  `json:"hours"`
	AllTime bool `json:"all_time"`
	// From/To are set only for a custom range (?from=&to=); Hours is then the
	// rounded-up span so existing readers keep working.
	From       string `json:"from,omitempty"`
	To         string `json:"to,omitempty"`
	Repo       string `json:"repo,omitempty"`
	AuthorKind string `json:"author_kind,omitempty"`
	// N is the number of PRs (latest bundle per PR) in the window.
	N int `json:"n"`
	// AvailableWindows is the PR count the same repo/author_kind filter
	// would yield for each preset window ("24", "168", "720" hours and
	// "all"), so a client can tell "not enough data" from "window too short".
	AvailableWindows map[string]int              `json:"available_windows"`
	Outcomes         map[string]EvidenceOutcomes `json:"outcomes_by_author_kind"`
	// ControlArm is the descriptive within-repo on/off comparison; unassigned
	// records are disclosed separately and no merge or dispatch gate reads it.
	ControlArm EvidenceControl `json:"control_arm"`
	// OutcomesByAuthorFamily is Outcomes keyed by the author's model family
	// (claude / gpt / gemini / other, "human" for a person's PR, "unknown"
	// for an agent whose model was not recorded) — hivecommons/hive#13243.
	OutcomesByAuthorFamily map[string]EvidenceOutcomes     `json:"outcomes_by_author_family"`
	Calibration            []EvidencePerspectiveCalib      `json:"reviewer_calibration"`
	CalibrationDerived     []EvidencePerspectiveCalib      `json:"reviewer_calibration_derived"`
	CalibrationLegacy      int                             `json:"reviewer_calibration_legacy"`
	ReviewerModels         []EvidenceModelAccuracy         `json:"reviewer_accuracy_by_model"`
	SentinelPerWeek        EvidenceSentinelSeries          `json:"sentinel_hits_per_week"`
	ShadowGate             EvidenceShadowGate              `json:"shadow_gate"`
	ReviewerAccuracy       *review.ReviewerAccuracySummary `json:"reviewer_accuracy,omitempty"`
	// WriteCounters is the proxy's attempted-vs-blocked agent write totals per
	// intent tier (#12069); omitted when the counter file is absent or unreadable.
	WriteCounters map[string]EvidenceWriteCount `json:"write_counters,omitempty"`
	// ACMMChanges is the month each policy.acmm_level was first observed per
	// repo (#12369), computed over all stored bundles for the repo rather than
	// the latest bundle per PR, the author kind or the window (policy maturity
	// is a repo property; see evidenceACMMChanges). Omitted when no bundle
	// carries a valid level. Report-only; it never feeds back into eligibility.
	ACMMChanges []EvidenceACMMChange `json:"acmm_changes,omitempty"`
	// WriteActivity is every agent GitHub write observed in the window —
	// direct writes through the proxy (from the durable counter file's weekly
	// buckets) plus hive-mediated writes from the audit log — with the
	// refusals of each, so the boundary rate has an honest denominator.
	WriteActivity EvidenceWriteActivity `json:"write_activity"`
	// ClaimVsVerdict is panel 5 (#12365): per ISO week, agent-authored PRs
	// whose author said "done" versus what CI and the swarm said.
	ClaimVsVerdict []EvidenceClaimWeek `json:"claim_vs_verdict"`
	// CIRecovery is panel 5's headline: of agent PRs that were CI-red when
	// the agent claimed done, how many recovered (went green and merged),
	// how fast, and how many were abandoned, under the hive's CI policy.
	CIRecovery EvidenceCIRecovery `json:"ci_recovery"`
	// IntentCounters is the intent gate's evaluated-vs-would-block totals per
	// tier (#12362), same shape as WriteCounters: Attempted is evaluations,
	// Blocked is the subset BlocksMerge(true) would have withheld, whether or
	// not intent.enforce was on. Omitted when the counter file is absent or
	// unreadable. Record-only; it never feeds back into eligibility.
	IntentCounters map[string]EvidenceWriteCount `json:"intent_counters,omitempty"`
	// ComplexityByRepo is the latest evidenceComplexityMonths monthly
	// complexity snapshots per managed repo (#12363), oldest first; omitted
	// when no repo has a snapshot yet.
	ComplexityByRepo []EvidenceComplexityRepo `json:"complexity_by_repo,omitempty"`
	// WorkBalance is panel 6 (#12366): per ISO week, audited issues opened by
	// actor kind against issues closed, over the same window and repo.
	WorkBalance []EvidenceWorkBalanceWeek `json:"work_balance"`
	// WorkBalanceSources names where each work_balance series came from and
	// where GitHub-truth issue coverage begins (#12517).
	WorkBalanceSources EvidenceWorkBalanceSources `json:"work_balance_sources"`
	Missing            EvidenceMissing            `json:"missing"`
	// Cards carries, per dashboard panel key, the server-computed letter
	// grade (A+ … C-, or — when there is not enough data) and the one-line
	// plain-English summary shown above each chart (#13204). Derived from the
	// fields above by evidenceCards; see pkg/evidence/grade.go for the rubrics.
	Cards map[string]evidence.Card `json:"cards"`
	// IssueReopenRates always uses the trailing 30 days by closure time,
	// independent of the PR-open-time window and author filter.
	IssueReopenRates []evidence.ReopenRate `json:"issue_reopen_rates_30d"`
	// IssueOpensPRMerges is the "Issue opens : PR merges" card: hive-authored
	// issue opens ÷ hive-merged PRs over the window, per repo and fleet-wide,
	// judged against the governor's cadence band, plus a rolling 30-day
	// series judged against parity (api_evidence_opens_merges.go).
	IssueOpensPRMerges evidence.OpensMerges `json:"issue_opens_pr_merges"`
}

// EvidenceClaimWeek is one ISO week of agent-authored PRs (latest bundle per
// PR). Claimed counts every such PR: opening or marking it ready is the agent
// saying "done". CINotGreen counts PRs whose captured CI had settled red
// (evidenceCIState); CICaptured is that column's denominator and counts only
// PRs whose CI had settled green or red. A record with no captured checks is
// disclosed under missing.records_empty_ci, and one whose checks were still
// running when captured under CIPending / missing.records_ci_pending; neither
// is counted as agreement or as disagreement. VerdictNotApprove counts PRs
// with any perspective verdict other than approve; Both counts PRs in both
// columns.
type EvidenceClaimWeek struct {
	Week              string `json:"week"`
	N                 int    `json:"n"`
	Claimed           int    `json:"claimed"`
	CICaptured        int    `json:"ci_captured"`
	CINotGreen        int    `json:"ci_not_green"`
	CIPending         int    `json:"ci_pending"`
	VerdictNotApprove int    `json:"verdict_not_approve"`
	Both              int    `json:"both"`
}

// CI states a bundle's captured checks can settle into (evidenceCIState).
const (
	evidenceCINone    = "none"
	evidenceCIGreen   = "green"
	evidenceCIRed     = "red"
	evidenceCIPending = "pending"
)

// evidenceCIFailingConclusions are the check-run conclusions the merge gate
// reads as a failure. "skipped" and "neutral" are not failures (a skipped
// job is green for the gate), and "in_progress", "queued" or "pending" mean
// the check had not finished when the snapshot was taken.
var evidenceCIFailingConclusions = map[string]bool{
	"failure": true, "cancelled": true, "timed_out": true, "action_required": true, "startup_failure": true, "stale": true,
}

// evidenceCIFinishedConclusions are the conclusions of a completed check
// that did not fail.
var evidenceCIFinishedConclusions = map[string]bool{"success": true, "skipped": true, "neutral": true}

// evidenceCIState classifies the CI snapshot a bundle carries. The gate's
// own verdict (ci.status, written at merge-eligibility time) wins when
// present; otherwise the per-check conclusions decide: any failing
// conclusion is red, all finished-clean is green, anything else (a check
// still running, an unrecognised conclusion) is pending. A snapshot with no
// checks is none. Counting a skipped or still-running check as "not green"
// would turn almost every captured record into a disagreement, which is a
// measurement error, not a finding.
func evidenceCIState(b *evidence.Bundle) string {
	if b == nil || len(b.CI.Checks) == 0 {
		return evidenceCINone
	}
	switch b.CI.Status {
	case "success":
		return evidenceCIGreen
	case "failure":
		return evidenceCIRed
	case "pending":
		return evidenceCIPending
	}
	state := evidenceCIGreen
	for _, c := range b.CI.Checks {
		switch {
		case evidenceCIFailingConclusions[c.Conclusion]:
			return evidenceCIRed
		case !evidenceCIFinishedConclusions[c.Conclusion]:
			state = evidenceCIPending
		}
	}
	return state
}

// evidenceVerdictNotApprove reports whether any perspective verdict is
// something other than approve. No verdicts is not disagreement.
func evidenceVerdictNotApprove(b *evidence.Bundle) bool {
	for _, v := range b.Verdicts {
		if v.Verdict != string(review.VerdictApprove) {
			return true
		}
	}
	return false
}

// evidenceComplexityMonths caps how many monthly snapshots per repo the
// summary carries; the full retention (complexity.RetainMonths) stays on disk.
const evidenceComplexityMonths = 12

// EvidenceComplexityRepo is one repo's monthly complexity series.
type EvidenceComplexityRepo struct {
	Repo   string                    `json:"repo"`
	Months []EvidenceComplexityMonth `json:"months"`
}

// EvidenceComplexityMonth is one monthly snapshot without its hotspot list.
type EvidenceComplexityMonth struct {
	Month                  string         `json:"month"`
	Ref                    string         `json:"ref,omitempty"`
	CapturedAt             time.Time      `json:"captured_at"`
	Files                  int            `json:"files"`
	Functions              int            `json:"functions"`
	MeanCyclomatic         float64        `json:"mean_cyclomatic"`
	MedianCyclomatic       int            `json:"median_cyclomatic"`
	P90Cyclomatic          int            `json:"p90_cyclomatic"`
	MaxCyclomatic          int            `json:"max_cyclomatic"`
	FunctionsOverThreshold int            `json:"functions_over_threshold"`
	Threshold              int            `json:"threshold"`
	StaticWarnings         int            `json:"static_warnings"`
	StaticWarningsByTool   map[string]int `json:"static_warnings_by_tool,omitempty"`
}

// loadEvidenceComplexity reads the per-repo monthly series under root; any
// failure yields nil (the summary omits the field rather than failing).
func loadEvidenceComplexity(root string) []EvidenceComplexityRepo {
	series, err := complexity.LoadSeries(root, evidenceComplexityMonths)
	if err != nil || len(series) == 0 {
		return nil
	}
	out := make([]EvidenceComplexityRepo, 0, len(series))
	for _, rs := range series {
		r := EvidenceComplexityRepo{Repo: rs.Repo, Months: make([]EvidenceComplexityMonth, 0, len(rs.Snapshots))}
		for _, s := range rs.Snapshots {
			r.Months = append(r.Months, EvidenceComplexityMonth{
				Month: s.Month, Ref: s.Ref, CapturedAt: s.CapturedAt,
				Files: s.Files, Functions: s.Functions,
				MeanCyclomatic: s.MeanCyclo, MedianCyclomatic: s.MedianCyclo,
				P90Cyclomatic: s.P90Cyclo, MaxCyclomatic: s.MaxCyclo,
				FunctionsOverThreshold: s.OverThreshold, Threshold: s.Threshold,
				StaticWarnings: s.Warnings, StaticWarningsByTool: s.WarningsByTool,
			})
		}
		out = append(out, r)
	}
	return out
}

// EvidenceWriteCount is one tier's attempted and proxy-blocked write counts.
// BlockedByReason is keyed by the proxy's DenyReason* code (#12368).
type EvidenceWriteCount struct {
	Attempted       int64            `json:"attempted"`
	Blocked         int64            `json:"blocked"`
	BlockedByReason map[string]int64 `json:"blocked_by_reason,omitempty"`
}

// loadEvidenceWriteCounters reads the durable counter file; any failure yields nil.
func loadEvidenceWriteCounters(path string) map[string]EvidenceWriteCount {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var f struct {
		Totals map[string]EvidenceWriteCount `json:"totals"`
	}
	if json.Unmarshal(data, &f) != nil || len(f.Totals) == 0 {
		return nil
	}
	return f.Totals
}

// EvidenceOutcomes counts PRs for one author kind. Merged is any record of
// the PR with a merge event: one this hive performed, or one the forge
// reported (MergedElsewhere, a subset of Merged: merged by hand, by another
// bot or by a different hive, learned from the review-outcome ledger or a
// backfill). Closed and Open come from a backfilled record
// (hivecommons/hive#12371) or from the review-outcome ledger: closed without
// merge, or still open when last observed. Unknown is a PR with live bundles
// only, no merge event and no ledger row: the PR may be open, merged by
// someone else or closed by hand and nothing on this hive says which. Rates
// must use Merged+Closed+Open as their denominator and report Unknown beside
// them, never fold it into "not merged".
//
// Change failures (hivecommons/hive#13241): RevertedWithin14d counts merged
// PRs with a reverted_by link (the linker only writes one for a revert
// merged within 14 days of the original); IssueReopenedWithin14d counts
// merged PRs with an issue_reopens entry dated within 14 days of the merge.
// A PR counts once in each. ChangeFailures is their sum and
// ChangeFailureRate is ChangeFailures ÷ Merged, nil with no merges.
type EvidenceOutcomes struct {
	N                      int      `json:"n"`
	Merged                 int      `json:"merged"`
	MergedElsewhere        int      `json:"merged_elsewhere"`
	Closed                 int      `json:"closed"`
	Open                   int      `json:"open"`
	Unknown                int      `json:"unknown"`
	RevertedWithin14d      *int     `json:"reverted_14d"`
	IssueReopenedWithin14d int      `json:"issue_reopened_14d"`
	ChangeFailures         int      `json:"change_failures"`
	ChangeFailureRate      *float64 `json:"change_failure_rate"`
}

// finishChangeFailures fills ChangeFailures and ChangeFailureRate from the
// two counts once the window has been folded.
func (o *EvidenceOutcomes) finishChangeFailures() {
	reverted := 0
	if o.RevertedWithin14d != nil {
		reverted = *o.RevertedWithin14d
	} else {
		o.RevertedWithin14d = new(int)
	}
	o.ChangeFailures = reverted + o.IssueReopenedWithin14d
	o.ChangeFailureRate = nil
	if o.Merged > 0 {
		r := float64(o.ChangeFailures) / float64(o.Merged)
		o.ChangeFailureRate = &r
	}
}

// add counts one PR under this bucket by its recorded outcome, including
// the change-failure signals on merged PRs.
func (o *EvidenceOutcomes) add(b *evidence.PR) {
	o.N++
	switch {
	case b.Merge != nil:
		o.Merged++
		if b.MergedElsewhere {
			o.MergedElsewhere++
		}
		if reverted, reopened := b.ChangeFailure(); reverted || reopened {
			if o.RevertedWithin14d == nil {
				o.RevertedWithin14d = new(int)
			}
			if reverted {
				*o.RevertedWithin14d++
			}
			if reopened {
				o.IssueReopenedWithin14d++
			}
		}
	case b.Closed:
		o.Closed++
	case b.OutcomeKnown():
		o.Open++
	default:
		o.Unknown++
	}
}

type EvidencePerspectiveCalib struct {
	Perspective string                `json:"perspective"`
	N           int                   `json:"n"`
	Buckets     []EvidenceCalibBucket `json:"buckets"`
}

// EvidenceCalibBucket is one confidence bucket of one perspective. Merged
// counts verdicts on PRs that merged; Unknown counts verdicts on PRs whose
// outcome is not recorded (EvidenceOutcomes.Unknown), so the merged rate is
// Merged / (N - Unknown), not Merged / N.
type EvidenceCalibBucket struct {
	Bucket    string `json:"bucket"`
	N         int    `json:"n"`
	Approvals int    `json:"approvals"`
	Blocks    int    `json:"blocks"`
	Merged    int    `json:"merged"`
	Unknown   int    `json:"unknown"`
}

type EvidenceModelAccuracy struct {
	Model    string                       `json:"model"`
	N        int                          `json:"n"`
	ByAuthor map[string]EvidenceModelStat `json:"by_author_kind"`
	// ByAuthorFamily is ByAuthor keyed by the author's model family bucket
	// (see EvidenceSummary.OutcomesByAuthorFamily).
	ByAuthorFamily map[string]EvidenceModelStat `json:"by_author_family"`
	// ByFamilyMatch splits this reviewer model's verdicts by whether it
	// shares a family with the PR's author model: "same", "cross", or
	// "unknown" when either side is unrecorded. It is the comparison a
	// cross-family review_models pool exists to make visible.
	ByFamilyMatch map[string]EvidenceModelStat `json:"by_family_match"`
}

// add folds one verdict into the stat. known/merged describe the PR's
// recorded outcome; enforced is whether a gate read the verdict.
func (st *EvidenceModelStat) add(approve, block, known, merged, enforced bool) {
	st.N++
	if approve {
		st.Approvals++
		switch {
		case !known:
			st.ApprovedUnknown++
		case !merged:
			st.ApprovedNotMerged++
		}
	}
	if block {
		st.Blocks++
		if enforced {
			st.EnforcedBlocks++
		} else {
			st.AdvisoryBlocks++
		}
		switch {
		case !known:
			st.BlockedUnknown++
			if enforced {
				st.EnforcedBlockedUnknown++
			} else {
				st.AdvisoryBlockedUnknown++
			}
		case merged:
			st.MergedAfterBlock++
			if enforced {
				st.MergedAfterEnforcedBlock++
			} else {
				st.DissentBeforeMerge++
			}
		}
	}
}

// evidenceAuthorFamily is the family bucket for a bundle's author.
func evidenceAuthorFamily(a evidence.Author) string {
	hiveAuthored := a.Kind == evidence.AuthorAgent || a.Kind == evidence.AuthorBot
	return review.AuthorFamilyBucket(a.Model, a.Kind != "", hiveAuthored)
}

// evidenceVerdictFamilyMatch prefers the verdict's stored same_family (set
// when the bundle was written) and falls back to deriving it, so bundles
// written before the field existed are still classified.
func evidenceVerdictFamilyMatch(v evidence.Verdict, a evidence.Author) string {
	if v.SameFamily != nil {
		if *v.SameFamily {
			return review.FamilyMatchSame
		}
		return review.FamilyMatchCross
	}
	authorModel := v.AuthorModel
	if authorModel == "" {
		authorModel = a.Model
	}
	return review.FamilyMatchBucket(v.Model, authorModel)
}

// EvidenceModelStat: Approvals/Blocks are verdict counts; MergedAfterBlock and
// ApprovedNotMerged compare the verdict with the PR's recorded outcome.
// ApprovedNotMerged counts approvals on PRs whose record says closed without
// merge or still open at backfill time — a known non-merge. ApprovedUnknown
// and BlockedUnknown count verdicts on PRs with no recorded outcome; they are
// excluded from every rate's denominator and shown as unknown
// (false-approve rate = ApprovedNotMerged / (Approvals - ApprovedUnknown)).
//
// Blocks are split by the bundle's policy.require_approval
// (hivecommons/hive#12595). EnforcedBlocks were cast while a gate read the
// verdict, so a merge after one is a human override: false-block rate =
// MergedAfterEnforcedBlock / (EnforcedBlocks - EnforcedBlockedUnknown).
// AdvisoryBlocks were cast while nothing read the verdict, so a merge after
// one is not a reviewer error: it is DissentBeforeMerge, reported as a count
// and a share of AdvisoryBlocks, never as an error rate. MergedAfterBlock and
// BlockedUnknown stay the totals over both kinds.
type EvidenceModelStat struct {
	N                        int `json:"n"`
	Approvals                int `json:"approvals"`
	Blocks                   int `json:"blocks"`
	MergedAfterBlock         int `json:"merged_after_block"`
	ApprovedNotMerged        int `json:"approved_not_merged"`
	ApprovedUnknown          int `json:"approved_unknown"`
	BlockedUnknown           int `json:"blocked_unknown"`
	EnforcedBlocks           int `json:"enforced_blocks"`
	AdvisoryBlocks           int `json:"advisory_blocks"`
	MergedAfterEnforcedBlock int `json:"merged_after_enforced_block"`
	EnforcedBlockedUnknown   int `json:"enforced_blocked_unknown"`
	DissentBeforeMerge       int `json:"dissent_before_merge"`
	AdvisoryBlockedUnknown   int `json:"advisory_blocked_unknown"`
}

type EvidenceSentinelSeries struct {
	N     int                    `json:"n"`
	Weeks []EvidenceSentinelWeek `json:"weeks"`
}

type EvidenceSentinelWeek struct {
	Week string `json:"week"`
	Hits int    `json:"hits"`
}

// EvidenceShadowGate summarises shadow-mode gate observations (#12068). N is
// the PRs whose latest bundle carries any shadow observation; WouldHaveHeld
// (review gate) and WouldHaveBlocked (intent) count the PRs the gate would
// have stopped. ReviewGate and Intent compare those PRs' recorded outcomes
// with the PRs the gate would have let through.
//
// ReviewGateByLane splits ReviewGate by the lane that recorded the
// observation (hivecommons/hive#12592): the merge-eligible lane
// (review.shadow) or the self-merge lane's ReviewGate.
type EvidenceShadowGate struct {
	N                int                     `json:"n"`
	WouldHaveHeld    int                     `json:"would_have_held"`
	WouldHaveBlocked int                     `json:"would_have_blocked"`
	ReviewGate       EvidenceShadowGateSide  `json:"review_gate"`
	ReviewGateByLane EvidenceShadowGateLanes `json:"review_gate_by_lane"`
	Intent           EvidenceShadowGateSide  `json:"intent"`
}

// EvidenceShadowGateLanes is the review gate's shadow observations per lane.
type EvidenceShadowGateLanes struct {
	MergeEligible EvidenceShadowGateSide `json:"merge_eligible"`
	SelfMerge     EvidenceShadowGateSide `json:"self_merge"`
}

// evidenceShadowLaneSelfMerge is the ShadowGate.Lane the self-merge lane's
// ReviewGate records; any other value is the merge-eligible lane.
const evidenceShadowLaneSelfMerge = "self-merge"

// EvidenceShadowGateSide is one shadow gate: N PRs evaluated, split into the
// ones it would have stopped (Held) and the ones it would have passed.
type EvidenceShadowGateSide struct {
	N      int                   `json:"n"`
	Held   EvidenceShadowOutcome `json:"held"`
	Passed EvidenceShadowOutcome `json:"passed"`
}

// EvidenceShadowOutcome is how a set of PRs actually turned out. Reverted is
// a subset of Merged (the bundle has reverted_by links). Closed and Open come
// from a backfilled record of the same PR; a live-only v1 bundle with no
// merge event does not record what happened, so it counts as Unknown (see
// EvidenceOutcomes).
type EvidenceShadowOutcome struct {
	N        int `json:"n"`
	Merged   int `json:"merged"`
	Closed   int `json:"closed"`
	Open     int `json:"open"`
	Unknown  int `json:"unknown"`
	Reverted int `json:"reverted"`
}

func (o *EvidenceShadowOutcome) add(b *evidence.PR) {
	o.N++
	if b.Merge == nil {
		switch {
		case b.Closed:
			o.Closed++
		case b.OutcomeKnown():
			o.Open++
		default:
			o.Unknown++
		}
		return
	}
	o.Merged++
	if len(b.RevertedBy) > 0 {
		o.Reverted++
	}
}

func (s *EvidenceShadowGateSide) add(b *evidence.PR, flag *bool) {
	if flag == nil {
		return
	}
	s.N++
	if *flag {
		s.Held.add(b)
	} else {
		s.Passed.add(b)
	}
}

type EvidenceMissing struct {
	VerdictsModelUnknown int `json:"verdicts_model_unknown"`
	RecordsEmptyCI       int `json:"records_empty_ci"`
	// RecordsCIPending counts PRs whose captured checks had not all finished
	// when the snapshot was taken; they are in neither CI column.
	RecordsCIPending int `json:"records_ci_pending"`
	// PRsOutcomeUnknown counts PRs in the window with live bundles only, no
	// merge event and no review-outcome ledger row (EvidenceOutcomes.Unknown
	// summed over author kinds): truly unresolved.
	PRsOutcomeUnknown int `json:"prs_outcome_unknown"`
	// PRsOutcomeFromLedger counts PRs in the window whose outcome (merged
	// elsewhere, closed or open) is known only from the review-outcome
	// ledger, stamped on the bundle or applied at read time.
	PRsOutcomeFromLedger int  `json:"prs_outcome_from_ledger"`
	RecordsUnreadable    int  `json:"records_unreadable"`
	ReviewerAccuracy     bool `json:"reviewer_accuracy_unavailable"`
	// IssuesHumanOpensUnobserved is true while some work_balance row relies on
	// the audit log for issues: the audit log only records hive-mediated issue
	// writes, so issues humans or third-party bots open directly on GitHub
	// never reach those rows. It clears once the GitHub rollup exists, or when
	// the historical issue series covers every row and none fell back to audit.
	IssuesHumanOpensUnobserved bool `json:"issues_human_opens_unobserved"`
	// ThrottleWindows is always true today: this hive reports no governor
	// issue-creation throttle windows, so work_balance carries no shading.
	ThrottleWindows bool `json:"throttle_windows_unavailable"`
}

// evidenceSummaryMaxRangeDays caps a custom ?from=&to= range. Backfilled
// history (hivecommons/hive#12371) reaches back years, so the cap is three
// years rather than one; the summary is a single pass over the bundles either
// way.
const evidenceSummaryMaxRangeDays = 3 * 366

// evidenceWindow is the time slice a summary covers. A zero From with Hours
// 0 means all time; a custom range sets both From and To.
type evidenceWindow struct {
	Hours  int
	From   time.Time
	To     time.Time
	Custom bool
}

// parseEvidenceTime accepts RFC3339 or YYYY-MM-DD (UTC midnight).
func parseEvidenceTime(raw string) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC(), true
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return t.UTC(), true
	}
	return time.Time{}, false
}

// parseEvidenceWindow reads hours= or from=/to= from the query. A date-only
// `to` is inclusive (end of that UTC day); a missing `to` means now.
func parseEvidenceWindow(q map[string][]string, now time.Time) (evidenceWindow, error) {
	get := func(k string) string {
		if v := q[k]; len(v) > 0 {
			return strings.TrimSpace(v[0])
		}
		return ""
	}
	fromRaw, toRaw := get("from"), get("to")
	if fromRaw == "" && toRaw != "" {
		return evidenceWindow{}, errors.New("to requires from")
	}
	if fromRaw != "" {
		from, ok := parseEvidenceTime(fromRaw)
		if !ok {
			return evidenceWindow{}, errors.New("from must be RFC3339 or YYYY-MM-DD")
		}
		to := now
		if toRaw != "" {
			t, ok := parseEvidenceTime(toRaw)
			if !ok {
				return evidenceWindow{}, errors.New("to must be RFC3339 or YYYY-MM-DD")
			}
			if len(toRaw) == len("2006-01-02") {
				t = t.Add(24 * time.Hour)
			}
			to = t
		}
		if to.Before(from) {
			return evidenceWindow{}, errors.New("to must not be before from")
		}
		if to.Sub(from) > evidenceSummaryMaxRangeDays*24*time.Hour {
			return evidenceWindow{}, fmt.Errorf("range must not exceed %d days", evidenceSummaryMaxRangeDays)
		}
		hours := int((to.Sub(from) + time.Hour - 1) / time.Hour)
		if hours < 1 {
			hours = 1
		}
		return evidenceWindow{Hours: hours, From: from, To: to, Custom: true}, nil
	}
	hours := prThroughputDefaultHours
	if raw := get("hours"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			return evidenceWindow{}, errors.New("hours must be a non-negative integer (0 = all time)")
		}
		hours = n
	}
	if hours > prThroughputMaxHours {
		hours = prThroughputMaxHours
	}
	win := evidenceWindow{Hours: hours, To: now}
	if hours > 0 {
		win.From = now.Add(-time.Duration(hours) * time.Hour)
	}
	return win, nil
}

// handleEvidenceSummary serves GET /api/evidence/summary?hours=&from=&to=&repo=&author_kind=.
func (s *Server) handleEvidenceSummary(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	win, werr := parseEvidenceWindow(r.URL.Query(), now)
	if werr != nil {
		jsonError(w, werr.Error(), http.StatusBadRequest)
		return
	}
	hours := win.Hours
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if repo != "" && !validPRThroughputRepo(repo) {
		jsonError(w, "repo must be owner/name", http.StatusBadRequest)
		return
	}
	kind := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("author_kind")))
	switch kind {
	case "", evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot:
	default:
		jsonError(w, "author_kind must be human, agent or bot", http.StatusBadRequest)
		return
	}

	verifier, err := s.reviewEvidenceVerifier()
	if err != nil {
		jsonError(w, "review evidence verification unavailable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	bundles, unreadable, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot, verifier)
	if err != nil {
		jsonError(w, "review evidence unreadable: "+err.Error(), http.StatusInternalServerError)
		return
	}
	artifact, aerr := review.LoadArtifact("")
	ledger, lerr := review.LoadOutcomeLedger("")
	hints := evidenceOutcomeHints(ledger)
	resp := buildEvidenceSummaryWindowHints(bundles, hints, win, repo, kind)
	resp.IssueReopenRates = evidence.IssueReopenRates(bundles, now, repo)
	resp.AvailableWindows = evidenceAvailableWindows(bundles, now, repo, kind)
	resp.Missing.RecordsUnreadable = unreadable
	resp.WriteCounters = loadEvidenceWriteCounters(writeCountersPath)
	resp.IntentCounters = loadEvidenceWriteCounters(intentCountersPath)
	resp.ComplexityByRepo = loadEvidenceComplexity(ghpkg.ReviewEvidenceRoot)
	balanceEntries := evidenceWorkBalanceEntries(s.audit)
	repoPRs := evidenceFoldWithHints(bundles, hints, repo, "")
	resp.WorkBalance, resp.WorkBalanceSources = buildEvidenceWorkBalance(evidenceWorkBalanceInputs{
		Entries: balanceEntries,
		Rollup:  loadIssueActivityRollup(issueActivityPath),
		Series:  evidence.LoadIssueSeries(ghpkg.ReviewEvidenceRoot),
		PRs:     repoPRs,
		Buckets: s.evidenceWorkBalanceBuckets(win),
	}, win, repo)
	resp.IssueOpensPRMerges = buildEvidenceOpensMerges(evidenceOpensMergesEvents(balanceEntries, repoPRs, repo), s.evidenceGovernorMode(repo), win, now)
	resp.Missing.IssuesHumanOpensUnobserved = resp.WorkBalanceSources.IssuesGitHubSince == "" && (resp.WorkBalanceSources.IssueSeriesWeeks == 0 || resp.WorkBalanceSources.AuditOnlyWeeks > 0)
	resp.WriteActivity = buildEvidenceWriteActivity(writeCountersPath, evidenceWriteActivityEntries(s.audit), win, repo, now)
	allPRs := evidenceFoldWithHints(bundles, hints, "", "")
	resp.CIRecovery = buildEvidenceCIRecovery(evidenceCIClaims(bundles, allPRs), evidenceGateMerges(bundles, allPRs), loadCIRecoveryRollup(ciRecoveryPath), win, repo, evidenceCIPolicy(s))

	days := (hours + 23) / 24
	if days < 1 || days > reviewerAccuracyMaxWindowDays {
		days = reviewerAccuracyMaxWindowDays
		if hours > 0 && hours <= reviewerAccuracyMaxWindowDays*24 {
			days = (hours + 23) / 24
		}
	}
	if (aerr != nil && !errors.Is(aerr, os.ErrNotExist)) || lerr != nil {
		resp.Missing.ReviewerAccuracy = true
	} else {
		acc := review.SummarizeReviewerAccuracyWith(now, time.Duration(days)*24*time.Hour, artifact, ledger, s.reviewerAccuracyEvidence(), s.reviewerAccuracyOptions())
		resp.ReviewerAccuracy = &acc
	}
	resp.Cards = evidenceCards(&resp, win, now)
	jsonResponse(w, resp)
}

// loadEvidenceBundles reads every bundle under root
// (<owner>/<repo>/<pr>/<sha>.json). Unparseable files and bundles that fail
// integrity verification are counted, not included in dashboard aggregates.
func loadEvidenceBundles(root string, verifier ghpkg.ReviewEvidenceVerifier) ([]*evidence.Bundle, int, error) {
	var out []*evidence.Bundle
	unreadable := 0
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.IsDir() {
			// Reporting series share the root but are not review bundles.
			rel, _ := filepath.Rel(root, path)
			issueSeries := d.Name() == evidence.IssueSeriesDirName && len(strings.Split(rel, string(filepath.Separator))) == 3
			if (d.Name() == complexity.DirName || issueSeries) && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) != ".json" {
			return nil
		}
		b, lerr := ghpkg.LoadVerifiedReviewEvidence(path, verifier)
		if lerr != nil || b.SchemaVersion != evidence.SchemaVersion {
			unreadable++
			return nil
		}
		out = append(out, b)
		return nil
	})
	return out, unreadable, err
}

// evidenceWindowHas reports whether a PR opened inside the window.
func evidenceWindowHas(win evidenceWindow, opened time.Time) bool {
	bounded := win.Custom || win.Hours > 0
	if bounded && opened.Before(win.From) {
		return false
	}
	if win.Custom && !opened.Before(win.To) {
		return false
	}
	return true
}

// evidenceLatestPerPR folds bundles per PR (evidence.FoldPRs), resolves the
// outcome of PRs whose records state none from hints (the review-outcome
// ledger; nil when unavailable) and keeps the PRs opened in win.
func evidenceLatestPerPR(bundles []*evidence.Bundle, hints map[string]evidence.OutcomeHint, win evidenceWindow, repo, kind string) map[string]*evidence.PR {
	prs := evidenceFoldWithHints(bundles, hints, repo, kind)
	for k, p := range prs {
		if !evidenceWindowHas(win, p.OpenedAt) {
			delete(prs, k)
		}
	}
	return prs
}

// evidenceAvailableWindowHours are the preset windows reported in
// available_windows; 0 is all time and is keyed "all".
var evidenceAvailableWindowHours = []int{24, 168, 720, 0}

// evidenceAvailableWindows counts PRs per preset window under the same
// repo/author_kind filter, so the card can say "window too short" instead
// of "not enough data" when a longer window would have samples.
func evidenceAvailableWindows(bundles []*evidence.Bundle, now time.Time, repo, kind string) map[string]int {
	prs := evidence.FoldPRs(bundles, repo, kind)
	out := make(map[string]int, len(evidenceAvailableWindowHours))
	for _, h := range evidenceAvailableWindowHours {
		key := "all"
		win := evidenceWindow{Hours: h, To: now}
		if h > 0 {
			key = strconv.Itoa(h)
			win.From = now.Add(-time.Duration(h) * time.Hour)
		}
		n := 0
		for _, p := range prs {
			if evidenceWindowHas(win, p.OpenedAt) {
				n++
			}
		}
		out[key] = n
	}
	return out
}

// buildEvidenceSummary aggregates the newest bundle per PR whose open time
// falls in the trailing window. hours==0 means all time.
func buildEvidenceSummary(bundles []*evidence.Bundle, now time.Time, hours int, repo, kind string) EvidenceSummary {
	win := evidenceWindow{Hours: hours, To: now}
	if hours > 0 {
		win.From = now.Add(-time.Duration(hours) * time.Hour)
	}
	return buildEvidenceSummaryWindow(bundles, win, repo, kind)
}

// buildEvidenceSummaryWindow is buildEvidenceSummary over an explicit window;
// a custom range keeps PRs opened in [From, To).
func buildEvidenceSummaryWindow(bundles []*evidence.Bundle, win evidenceWindow, repo, kind string) EvidenceSummary {
	return buildEvidenceSummaryWindowHints(bundles, nil, win, repo, kind)
}

// evidenceOutcomeHints turns the review-outcome ledger into per-PR hints
// keyed like evidence.PRKey, so a PR the ledger resolved (or still sees
// open) but whose bundle carries no outcome is not reported as unknown. The
// ledger does not record the merger, so a hinted merge has no actor.
func evidenceOutcomeHints(ledger *review.OutcomeLedger) map[string]evidence.OutcomeHint {
	if ledger == nil || len(ledger.Items) == 0 {
		return nil
	}
	hints := make(map[string]evidence.OutcomeHint, len(ledger.Items))
	for _, o := range ledger.Items {
		if o == nil {
			continue
		}
		h := evidence.OutcomeHint{At: o.OutcomeAt}
		switch o.Outcome {
		case review.OutcomeMerged:
			h.State, h.MergedAt = evidence.BackfillStateMerged, o.OutcomeAt
		case review.OutcomeClosed:
			h.State, h.ClosedAt = evidence.BackfillStateClosed, o.OutcomeAt
		case review.OutcomeOpen:
			h.State, h.At = evidence.BackfillStateOpen, ledger.GeneratedAt
		default:
			continue
		}
		hints[evidence.PRKey(o.Repo, o.Number)] = h
	}
	return hints
}

// evidenceFoldWithHints is evidence.FoldPRs followed by ApplyOutcomeHints.
func evidenceFoldWithHints(bundles []*evidence.Bundle, hints map[string]evidence.OutcomeHint, repo, kind string) map[string]*evidence.PR {
	prs := evidence.FoldPRs(bundles, repo, kind)
	evidence.ApplyOutcomeHints(prs, hints)
	return prs
}

// buildEvidenceSummaryWindowHints is buildEvidenceSummaryWindow with
// review-outcome ledger hints for PRs whose records state no outcome.
func buildEvidenceSummaryWindowHints(bundles []*evidence.Bundle, hints map[string]evidence.OutcomeHint, win evidenceWindow, repo, kind string) EvidenceSummary {
	hours := win.Hours
	resp := EvidenceSummary{
		Hours: hours, AllTime: hours == 0 && !win.Custom, Repo: repo, AuthorKind: kind,
		ACMMChanges:        evidenceACMMChanges(bundles, repo),
		Outcomes:           map[string]EvidenceOutcomes{},
		Calibration:        []EvidencePerspectiveCalib{},
		CalibrationDerived: []EvidencePerspectiveCalib{},

		OutcomesByAuthorFamily: map[string]EvidenceOutcomes{},
		ReviewerModels:         []EvidenceModelAccuracy{},
		SentinelPerWeek:        EvidenceSentinelSeries{Weeks: []EvidenceSentinelWeek{}},
		ClaimVsVerdict:         []EvidenceClaimWeek{},
		WorkBalance:            []EvidenceWorkBalanceWeek{},
		Missing:                EvidenceMissing{IssuesHumanOpensUnobserved: true, ThrottleWindows: true},
	}
	for _, k := range []string{evidence.AuthorHuman, evidence.AuthorAgent, evidence.AuthorBot} {
		resp.Outcomes[k] = EvidenceOutcomes{}
	}
	if win.Custom {
		resp.From = win.From.Format(time.RFC3339)
		resp.To = win.To.Format(time.RFC3339)
	}
	latest := evidenceLatestPerPR(bundles, hints, win, repo, kind)
	resp.ControlArm = evidenceControl(latest)
	keys := make([]string, 0, len(latest))
	for k := range latest {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	type calib struct {
		n       int
		buckets []EvidenceCalibBucket
	}
	calibs := map[string]*calib{}
	derivedCalibs := map[string]*calib{}
	models := map[string]*EvidenceModelAccuracy{}
	weeks := map[string]int{}
	claims := map[string]*EvidenceClaimWeek{}

	for _, key := range keys {
		b := latest[key]
		resp.N++
		merged := b.Merge != nil
		known := b.OutcomeKnown()
		o := resp.Outcomes[b.Author.Kind]
		o.add(b)
		resp.Outcomes[b.Author.Kind] = o
		family := evidenceAuthorFamily(b.Author)
		fo := resp.OutcomesByAuthorFamily[family]
		fo.add(b)
		resp.OutcomesByAuthorFamily[family] = fo
		if !merged && !b.Closed && !known {
			resp.Missing.PRsOutcomeUnknown++
		}
		if known && b.Outcome != nil && !b.Backfilled && (!merged || b.MergedElsewhere) {
			resp.Missing.PRsOutcomeFromLedger++
		}
		ciState := evidenceCIState(b.Bundle)
		switch ciState {
		case evidenceCINone:
			resp.Missing.RecordsEmptyCI++
		case evidenceCIPending:
			resp.Missing.RecordsCIPending++
		}
		if b.Author.Kind == evidence.AuthorAgent {
			y, w := b.OpenedAt.UTC().ISOWeek()
			wk := fmt.Sprintf("%04d-W%02d", y, w)
			c := claims[wk]
			if c == nil {
				c = &EvidenceClaimWeek{Week: wk}
				claims[wk] = c
			}
			c.N++
			c.Claimed++
			ciBad := ciState == evidenceCIRed
			switch ciState {
			case evidenceCIGreen, evidenceCIRed:
				c.CICaptured++
			case evidenceCIPending:
				c.CIPending++
			}
			swarmBad := evidenceVerdictNotApprove(b.Bundle)
			if ciBad {
				c.CINotGreen++
			}
			if swarmBad {
				c.VerdictNotApprove++
			}
			if ciBad && swarmBad {
				c.Both++
			}
		}

		if sh := b.Shadow; sh != nil {
			var held, blocked *bool
			if sh.ReviewGate != nil {
				held = sh.ReviewGate.WouldHaveHeld
			}
			if sh.Intent != nil {
				blocked = sh.Intent.WouldHaveBlocked
			}
			if held != nil || blocked != nil {
				resp.ShadowGate.N++
			}
			resp.ShadowGate.ReviewGate.add(b, held)
			if sh.ReviewGate != nil && sh.ReviewGate.Lane == evidenceShadowLaneSelfMerge {
				resp.ShadowGate.ReviewGateByLane.SelfMerge.add(b, held)
			} else {
				resp.ShadowGate.ReviewGateByLane.MergeEligible.add(b, held)
			}
			resp.ShadowGate.Intent.add(b, blocked)
		}

		for range b.Sentinel {
			y, w := b.OpenedAt.UTC().ISOWeek()
			weeks[fmt.Sprintf("%04d-W%02d", y, w)]++
			resp.SentinelPerWeek.N++
		}

		for _, v := range b.Verdicts {
			model := strings.TrimSpace(v.Model)
			if model == "" || model == evidenceSummaryModelUnknown {
				model = evidenceSummaryModelUnknown
				resp.Missing.VerdictsModelUnknown++
			}
			approve := v.Verdict == string(review.VerdictApprove)
			block := v.Verdict == string(review.VerdictChangesRequested) || v.Verdict == string(review.VerdictReject)

			if v.ConfidenceRaw == nil {
				resp.CalibrationLegacy++
			}
			for _, series := range []struct {
				buckets    map[string]*calib
				confidence *float64
			}{
				{calibs, &v.Confidence}, {derivedCalibs, v.ConfidenceDerived},
			} {
				if series.confidence == nil {
					continue
				}
				c := series.buckets[v.Perspective]
				if c == nil {
					c = &calib{buckets: make([]EvidenceCalibBucket, len(evidenceSummaryBuckets))}
					for i, bk := range evidenceSummaryBuckets {
						c.buckets[i].Bucket = bk.name
					}
					series.buckets[v.Perspective] = c
				}
				c.n++
				idx := 0
				for i, bk := range evidenceSummaryBuckets {
					if *series.confidence >= bk.low {
						idx = i
					}
				}
				c.buckets[idx].N++
				if approve {
					c.buckets[idx].Approvals++
				}
				if block {
					c.buckets[idx].Blocks++
				}
				if merged {
					c.buckets[idx].Merged++
				}
				if !known {
					c.buckets[idx].Unknown++
				}
			}

			m := models[model]
			if m == nil {
				m = &EvidenceModelAccuracy{Model: model, ByAuthor: map[string]EvidenceModelStat{}, ByAuthorFamily: map[string]EvidenceModelStat{}, ByFamilyMatch: map[string]EvidenceModelStat{}}
				models[model] = m
			}
			m.N++
			enforced := b.Policy.RequireApproval
			st := m.ByAuthor[b.Author.Kind]
			st.add(approve, block, known, merged, enforced)
			m.ByAuthor[b.Author.Kind] = st
			fst := m.ByAuthorFamily[family]
			fst.add(approve, block, known, merged, enforced)
			m.ByAuthorFamily[family] = fst
			match := evidenceVerdictFamilyMatch(v, b.Author)
			mst := m.ByFamilyMatch[match]
			mst.add(approve, block, known, merged, enforced)
			m.ByFamilyMatch[match] = mst
		}
	}

	for _, series := range []struct {
		buckets map[string]*calib
		dest    *[]EvidencePerspectiveCalib
	}{
		{calibs, &resp.Calibration}, {derivedCalibs, &resp.CalibrationDerived},
	} {
		persp := make([]string, 0, len(series.buckets))
		for p := range series.buckets {
			persp = append(persp, p)
		}
		sort.Strings(persp)
		for _, p := range persp {
			c := series.buckets[p]
			*series.dest = append(*series.dest, EvidencePerspectiveCalib{Perspective: p, N: c.n, Buckets: c.buckets})
		}
	}
	names := make([]string, 0, len(models))
	for n := range models {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		resp.ReviewerModels = append(resp.ReviewerModels, *models[n])
	}
	wk := make([]string, 0, len(weeks))
	for w := range weeks {
		wk = append(wk, w)
	}
	sort.Strings(wk)
	for _, w := range wk {
		resp.SentinelPerWeek.Weeks = append(resp.SentinelPerWeek.Weeks, EvidenceSentinelWeek{Week: w, Hits: weeks[w]})
	}
	cw := make([]string, 0, len(claims))
	for w := range claims {
		cw = append(cw, w)
	}
	sort.Strings(cw)
	for _, w := range cw {
		resp.ClaimVsVerdict = append(resp.ClaimVsVerdict, *claims[w])
	}
	resp.ShadowGate.WouldHaveHeld = resp.ShadowGate.ReviewGate.Held.N
	resp.ShadowGate.WouldHaveBlocked = resp.ShadowGate.Intent.Held.N
	for k, o := range resp.Outcomes {
		o.finishChangeFailures()
		resp.Outcomes[k] = o
	}
	for k, o := range resp.OutcomesByAuthorFamily {
		o.finishChangeFailures()
		resp.OutcomesByAuthorFamily[k] = o
	}
	return resp
}
