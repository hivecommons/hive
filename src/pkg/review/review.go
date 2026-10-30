// Package review models Hive's structured multi-perspective PR review verdicts.
package review

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
	"github.com/hivecommons/hive/pkg/review/reviewtext"
	"github.com/hivecommons/hive/pkg/sentinel"
)

const (
	PerspectiveCorrectness     Perspective = "correctness"
	PerspectiveSecurity        Perspective = "security"
	PerspectiveIntentAlignment Perspective = "intent-alignment"
	PerspectiveStyle           Perspective = "style"
	PerspectiveDocsCurrency    Perspective = "docs-currency"
	// PerspectivePlanMatch scores a PR against the approved plan wave it
	// claims to implement (hivecommons/hive#8317). Built in but NOT in
	// DefaultPerspectives: it runs only when review.plan_match.enabled is set,
	// because it needs the run trailers (#8310) and a plan to compare against,
	// and a hive without either would pay a perspective for nothing.
	PerspectivePlanMatch Perspective = "plan_match"
)

var DefaultPerspectives = []Perspective{
	PerspectiveCorrectness,
	PerspectiveSecurity,
	PerspectiveIntentAlignment,
	PerspectiveStyle,
	PerspectiveDocsCurrency,
}

type Perspective string

type Verdict string

const (
	VerdictApprove          Verdict = "approve"
	VerdictChangesRequested Verdict = "changes_requested"
	VerdictRequiresHuman    Verdict = "requires_human"
	VerdictReject           Verdict = "reject"
	// VerdictIncomplete is an aggregate-only state: every perspective that
	// reported approved, but the head has not yet received every perspective
	// it is owed. It is not a judgement — nothing disagreed and no human is
	// needed — it is a review still in progress, and the dispatcher keeps
	// asking for the missing perspectives. A perspective report may never
	// carry it (see validVerdict).
	VerdictIncomplete Verdict = "incomplete"
)

const (
	// DefaultHumanThreshold is the first finding severity that promotes an
	// aggregate review to requires_human even when perspective verdicts agree.
	DefaultHumanThreshold = outputschema.SeverityHigh

	// DefaultFixCycleCap mirrors escalation.MaxReEngagements so review-triggered
	// bot fix iterations share the same bounded-loop policy as CI re-engagements.
	DefaultFixCycleCap = escalation.MaxReEngagements

	ReviewReportFilePrefix = "review-report-"
	ReviewReportFileSuffix = ".json"
	ReviewVerdictsFile     = "review-verdicts.json"
	// DefaultVerdictsDir is the durable data dir, matching the dispatch state.
	DefaultVerdictsDir = DefaultDispatchStateDir
)

// DefaultReportDir is where the verdict relay drops each perspective's
// review-report-*.json and where Collect reads them back. It lives under the
// durable data dir for the same reason the artifact does: the reports used to
// sit in outputschema.AgentReportDir (/var/run/hive-metrics), on the
// container's ephemeral layer, and were merged into the artifact only on the
// next eval cycle. A pod replacement inside that window lost the verdict, the
// head looked unreviewed, and the reviewer posted the same review on the same
// SHA a second time. Observed on a bluefin spoke with the fleet updater rolling
// pods several times a day.
//
// A var, not a const, only so tests can keep accepted verdicts off the host's
// real data dir — the ReviewDispatchStatePath pattern.
var DefaultReportDir = DefaultDispatchStateDir + "/review-reports"

// ReportDir resolves the directory review reports are written to and read
// from: the caller's explicit choice, else DefaultReportDir.
func ReportDir(dir string) string {
	if dir == "" {
		return DefaultReportDir
	}
	return dir
}

// ReviewVerdictsPath is the other half of the reviewer's memory: the recorded
// verdicts that make PlanDispatch treat a (PR, head SHA) as already judged. If
// it is missing, AggregateFor reports "never reviewed" and the PR is dispatched
// again from scratch — the same PR, the same perspectives, another round of
// comments.
//
// It therefore lives on the durable data dir for the same reason the dispatch
// state does. Under AgentReportDir (/var/run/hive-metrics) it sat on the
// container's ephemeral writable layer, so every restart erased the record of
// what had already been judged. Observed on a bluefin spoke: nine restarts in
// one day, and one PR accumulated six reviews in seventy-seven minutes while
// fourteen of the hive's sixteen repositories were never reached at all.
//
// A var (not const) so tests can point it at a temp dir.
var ReviewVerdictsPath = filepath.Join(DefaultVerdictsDir, ReviewVerdictsFile)

// LegacyReviewVerdictsPath is the pre-migration location. LoadArtifact falls
// back to it once so an upgrading hive keeps the verdicts it already recorded
// instead of re-reviewing everything it had already judged.
var LegacyReviewVerdictsPath = filepath.Join(outputschema.AgentReportDir, ReviewVerdictsFile)

type PerspectiveReport struct {
	outputschema.AgentReport
	Perspective  Perspective `json:"perspective"`
	Verdict      Verdict     `json:"verdict"`
	Repo         string      `json:"repo"`
	Number       int         `json:"number"`
	HeadSHA      string      `json:"head_sha,omitempty"`
	BodyDigest   string      `json:"body_digest,omitempty"`
	HiveAuthored bool        `json:"hive_authored,omitempty"`
	// HiveSuperseding marks the hive App's own superseding copy of another
	// PR ("Supersedes #N" in its body), stamped by the verdict relay from
	// the PR, never from the reviewer's JSON. Its requires_human findings
	// — security-sensitive included — route to the fixer instead of
	// parking for an operator (hiveSupersedingFixRoute).
	HiveSuperseding bool   `json:"hive_superseding,omitempty"`
	AuthorModel     string `json:"author_model,omitempty"`
	ReviewModel     string `json:"review_model,omitempty"`
	// Head is the review head (agents.<name>.review_heads key, e.g.
	// code-reviewer-sol) this report came from, set by the relay from the
	// dispatch record. Empty for ordinary single-reviewer perspectives.
	Head string `json:"head,omitempty"`
	// Decided marks the decision step's reconciled report for this perspective
	// (hivecommons/hive#13542), set by the relay from the dispatch record,
	// never from the reviewer's JSON. When a PR has review heads, only decided
	// reports drive routing; the heads' raw reports are evidence.
	Decided bool `json:"decided,omitempty"`
	// Dispositions is the decision step's call on each finding the heads
	// raised for this perspective: confirmed, not_reproducible or
	// out_of_scope, with a reason.
	Dispositions []FindingDisposition `json:"dispositions,omitempty"`
	// Confidence is the optional binned self-report; it never feeds the gate.
	Confidence    *float64 `json:"confidence,omitempty"`
	ConfidenceRaw *float64 `json:"confidence_raw,omitempty"`
	// DecisionRequested means the review found no code defect it can route
	// to the author, but explicitly deferred a maintainer/product decision.
	DecisionRequested bool   `json:"decision_requested,omitempty"`
	DecisionQuestion  string `json:"decision_question,omitempty"`
	// RedundantOf is the PR the review found already carries this change
	// (`<!-- hive-redundant-of: #N -->`), set by the relay from the review
	// body. A decision question that names one is never self-resolved to
	// approve: the answer is a close, which the superseded closer performs
	// for the hive's own PRs (hivecommons/hive#15499).
	RedundantOf int `json:"redundant_of,omitempty"`
	// DecisionRiskHigh is the reviewer's explicit escape hatch: the review
	// body carried `<!-- hive-decision-risk: high -->`, so the question parks
	// the PR for a person even when its wording matches no risk class.
	DecisionRiskHigh bool `json:"decision_risk_high,omitempty"`
	// NotApplicable marks a report whose perspective had nothing to judge on
	// this PR -- today only plan_match on a PR with no Hive-Run / Hive-Plan
	// trailer. It is neither an approval nor a withheld one: the aggregate
	// treats it as approve for unanimity and the confidence score drops it
	// from the expected count, so it neither helps nor hurts.
	NotApplicable bool `json:"not_applicable,omitempty"`
	// ConcreteRemediation is set by the relay when the posted review body names
	// an agent-actionable fix. It lets self-authored decision policy route
	// mislabeled requires_human engineering findings back to the fixer lane
	// without storing the whole PR review body in the durable verdict file.
	ConcreteRemediation bool `json:"concrete_remediation,omitempty"`
	// PermissionsTighteningOnly is set by the relay when
	// WorkflowPermissionsTighteningOnly proved the PR's diff only removes or
	// narrows workflow `permissions:` scopes. It suppresses the direction-blind
	// permissions-credentials and sensitive-paths risk classes (and the
	// "permission"/"privilege" escalation terms) for this report; every other
	// risk class still parks the PR (hivecommons/hive#13068).
	PermissionsTighteningOnly bool `json:"permissions_tightening_only,omitempty"`
	// DeltaPreviousHead is the head SHA this report was judged as a delta
	// against (review.delta_rereview), stamped by the collector from the
	// dispatch record (ModelStamps), never from the reviewer's JSON. Empty
	// for a full review.
	DeltaPreviousHead string `json:"delta_previous_head,omitempty"`
}

type AggregateOptions struct {
	HumanThreshold outputschema.Severity
	FixAttempts    int
	MaxFixAttempts int
	// SelfAuthoredDecisionPolicy is "resolve" (default) or "escalate".
	// Resolve lets Hive-authored PRs keep moving when the only human hold is
	// a decision question that matches no DecisionRiskClass and carries no
	// `hive-decision-risk: high` marker.
	SelfAuthoredDecisionPolicy string
	// HiveSupersedingAutoMergeOff is `auto_merge.hive_superseding.auto_merge:
	// false`: the hive's superseding copy of another PR is then judged like
	// any other hive-authored PR, so a risk / security-sensitive
	// requires_human parks it for a person. Off (the zero value) is the
	// owner's rule: such findings go to the fixer and the PR auto-merges
	// once CI and review are clean.
	HiveSupersedingAutoMergeOff bool
	// SensitivePaths are the operator's extra `security.sensitive_paths`
	// globs; a contributor-PR finding on one of them (or on a
	// sentinel.DefaultSensitivePaths path) is a security finding that may
	// park the PR for a person during CI/Review (IsSecuritySensitiveFinding).
	SensitivePaths []string
	// MaxPerspectivesPerPR mirrors DispatchOptions.MaxPerspectivesPerPR. A
	// perspective the dispatcher never hands out cannot approve, so unanimity
	// must be judged against what the PR was eligible to receive. Zero means
	// "no cap": every perspective in the hive's set is required.
	MaxPerspectivesPerPR int
	// Perspectives is the set this hive reviews with. The zero value means the
	// built-in default set, so unanimity is judged against the perspectives
	// actually configured rather than a hardcoded five -- a hive that reviews
	// with three could otherwise never reach unanimity at all.
	Perspectives PerspectiveSet
	// ModelStamps fills author_model / review_model on reports that omitted
	// them, from what the planner recorded when it dispatched the review
	// (ModelStampsFromState). Nil leaves reports as written.
	ModelStamps ModelStamps
	// ExpectedHeads are the review head keys every perspective must hear from
	// (hivecommons/hive#13542). When a head's reports carry Head, an all-approve
	// aggregate stays incomplete until each expected head has reported each
	// perspective. Reports without Head are judged as before.
	ExpectedHeads []string
	// MissingHeads names, per PR head (reviewKey without a body digest), the
	// "<perspective>/<head>" pairs whose head ran out of retries
	// (review.head_max_retries) while another head answered the perspective
	// (MissingHeadsFromState). They count as settled: the decision proceeds
	// without them and says so.
	MissingHeads map[string][]string
}

type Aggregate struct {
	Repo              string `json:"repo"`
	Number            int    `json:"number"`
	HeadSHA           string `json:"head_sha,omitempty"`
	BodyDigest        string `json:"body_digest,omitempty"`
	AuthorModel       string `json:"author_model,omitempty"`
	ReviewModel       string `json:"review_model,omitempty"`
	HiveAuthored      bool   `json:"hive_authored,omitempty"`
	HiveAuthoredKnown bool   `json:"hive_authored_known,omitempty"`
	// HiveSuperseding is true when any report marked the PR as the hive's
	// superseding copy of another PR (PerspectiveReport.HiveSuperseding).
	HiveSuperseding   bool    `json:"hive_superseding,omitempty"`
	Verdict           Verdict `json:"verdict"`
	MergeEligible     bool    `json:"merge_eligible"`
	FixCycle          bool    `json:"fix_cycle"`
	RequiresHuman     bool    `json:"requires_human"`
	CloseRecommended  bool    `json:"close_recommended"`
	DecisionRequested bool    `json:"decision_requested,omitempty"`
	DecisionQuestion  string  `json:"decision_question,omitempty"`
	// DecisionFinding names what the review found that makes the decision
	// question a person's to answer — a duplicate, a finding at or above
	// the human threshold, a risk-class question (workflow, security, …)
	// or the reviewer's explicit high-risk marker. A question with no
	// finding behind it never parks a PR in CI/Review (owner rule, see
	// contributorDecisionBacking); the text is what the tracker and the
	// 085 comment print as "the review found".
	DecisionFinding string `json:"decision_finding,omitempty"`
	// RedundantOf is the PR a reviewer named as already carrying this
	// change, carried from the first report that set one.
	RedundantOf    int                     `json:"redundant_of,omitempty"`
	HumanSummaries []string                `json:"human_summaries,omitempty"`
	FixAttempts    int                     `json:"fix_attempts,omitempty"`
	MaxFixAttempts int                     `json:"max_fix_attempts,omitempty"`
	Threshold      outputschema.Severity   `json:"human_threshold"`
	Reasons        []string                `json:"reasons,omitempty"`
	Findings       []PerspectiveFinding    `json:"findings,omitempty"`
	Perspectives   map[Perspective]Verdict `json:"perspectives"`
	// Heads lists, per perspective, the review heads whose reports this
	// aggregate holds. Empty unless reports came from review_heads; with
	// several heads Perspectives holds the strictest of their verdicts.
	Heads map[Perspective][]string `json:"heads,omitempty"`
	// Review-heads decision state (hivecommons/hive#13542). Decided: the
	// verdict comes from the decision step's reconciled reports.
	// DecisionSkipped: every head approved every perspective with no
	// findings, so no decision session ran. AwaitingDecision: every head has
	// settled and the reconciliation is still owed (Verdict is incomplete).
	Decided          bool `json:"decided,omitempty"`
	DecisionSkipped  bool `json:"decision_skipped,omitempty"`
	AwaitingDecision bool `json:"awaiting_decision,omitempty"`
	// HeadReports are the heads' raw reports, kept as the decision step's
	// input and as the reference reviewer accuracy scores each head against.
	HeadReports []HeadReport `json:"head_reports,omitempty"`
	// Dispositions are the decision step's call on every head finding.
	Dispositions []FindingDisposition `json:"dispositions,omitempty"`
	// MissingHeads are "<perspective>/<head>" pairs that never answered after
	// review.head_max_retries; the decision was made without them.
	MissingHeads []string `json:"missing_heads,omitempty"`
	// Confidence is the derived 0–5 mergeability score (hivecommons/hive#8182),
	// computed from the same reports as the verdict so the two never disagree.
	Confidence Confidence `json:"confidence"`
	// RecordedAt is when this verdict was last merged into the durable
	// artifact. It exists so stale entries (PRs long since merged or closed)
	// can be pruned instead of accumulating forever.
	RecordedAt time.Time `json:"recorded_at,omitempty"`
	// PermissionsTighteningOnly mirrors the reports' flag so Normalize judges
	// risk escalation with the same carve-out AggregateReports did.
	PermissionsTighteningOnly bool `json:"permissions_tightening_only,omitempty"`
	// Delta is set when this verdict came from a delta re-review against an
	// earlier head (review.delta_rereview): either a session that read only
	// the diff between the two heads, or (Trivial) the earlier verdict
	// re-stamped without a session. nil is a full review.
	Delta *DeltaReviewInfo `json:"delta,omitempty"`
}

type PerspectiveFinding struct {
	Perspective Perspective          `json:"perspective"`
	Finding     outputschema.Finding `json:"finding"`
}

type Artifact struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Items       []Aggregate `json:"items"`
}

// ValidateReport validates a verdict against the built-in perspective set. Use
// ValidateReportFor on a hive that defines its own perspectives.
func ValidateReport(raw []byte) (*PerspectiveReport, error) {
	return ValidateReportFor(raw, PerspectiveSet{})
}

// ValidateReportFor validates a verdict against the perspectives THIS hive
// reviews with. A verdict naming anything else is refused: nothing dispatched
// it, nothing is waiting on it, and accepting it would let a reviewer
// manufacture a judgment for a perspective no one asked for.
func ValidateReportFor(raw []byte, set PerspectiveSet) (*PerspectiveReport, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("review report must be a non-empty JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	// Unknown fields are ignored rather than rejected. The report is written by a
	// model that routinely adds descriptive keys ("detail", "message") alongside
	// the schema; refusing the whole object over one extra key discards a review
	// that is otherwise complete and correct. Every field the hive routes on is
	// validated explicitly below, so an unrecognised key cannot change a verdict.
	var report PerspectiveReport
	if err := dec.Decode(&report); err != nil {
		return nil, fmt.Errorf("decode review report: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, fmt.Errorf("review report must contain exactly one JSON object")
	}
	if _, err := outputschema.Validate(agentReportJSON(report.AgentReport)); err != nil {
		return nil, err
	}
	if report.Kind != outputschema.KindReview {
		return nil, fmt.Errorf("kind must be %q", outputschema.KindReview)
	}
	if !set.Known(report.Perspective) {
		return nil, fmt.Errorf("perspective must be one of %s", set.Names())
	}
	if !validVerdict(report.Verdict) {
		return nil, fmt.Errorf("verdict must be one of approve, changes_requested, requires_human, reject")
	}
	if strings.TrimSpace(report.Repo) == "" {
		return nil, fmt.Errorf("repo is required")
	}
	if !strings.Contains(strings.TrimSpace(report.Repo), "/") {
		return nil, fmt.Errorf("repo must be fully qualified as owner/name")
	}
	if report.Number <= 0 {
		return nil, fmt.Errorf("number must be greater than zero")
	}
	if err := validateDispositions(report.Dispositions); err != nil {
		return nil, err
	}
	normalizeReportedConfidence(&report)
	return &report, nil
}

func AggregateReports(reports []PerspectiveReport, opts AggregateOptions) Aggregate {
	if usesReviewHeads(reports) {
		return aggregateWithHeads(reports, opts)
	}
	return aggregatePlainReports(reports, opts)
}

// aggregatePlainReports is the aggregate of reports that need no
// reconciliation: one judgement per perspective.
func aggregatePlainReports(reports []PerspectiveReport, opts AggregateOptions) Aggregate {
	agg := aggregateReports(reports, opts)
	if agg.PermissionsTighteningOnly && agg.Verdict != VerdictRequiresHuman {
		// The suppression removed a human hold; say so where the verdict is
		// read (review-verdicts.json, stage comments) so it is auditable.
		agg.Reasons = append(agg.Reasons, PermissionsTighteningReason)
	}
	return agg
}

func aggregateReports(reports []PerspectiveReport, opts AggregateOptions) Aggregate {
	threshold := opts.HumanThreshold
	sensitivePaths := SecuritySensitivePaths(opts.SensitivePaths)
	if threshold == "" {
		threshold = DefaultHumanThreshold
	}
	maxFix := opts.MaxFixAttempts
	if maxFix <= 0 {
		maxFix = DefaultFixCycleCap
	}
	effectiveReports := ScopeEffectiveReports(reports)
	agg := Aggregate{
		Threshold:      threshold,
		FixAttempts:    opts.FixAttempts,
		MaxFixAttempts: maxFix,
		Perspectives:   map[Perspective]Verdict{},
	}
	agg.Confidence = ScoreConfidence(effectiveReports, opts.Perspectives.Len())
	if len(reports) == 0 {
		agg.Verdict = VerdictRequiresHuman
		agg.RequiresHuman = true
		agg.Reasons = []string{"no review reports were collected"}
		return agg
	}
	agg.HiveAuthoredKnown = true
	agg.HiveAuthored = reportsHiveAuthored(reports)
	agg.HiveSuperseding = reportsHiveSuperseding(reports)
	agg.PermissionsTighteningOnly = reportsPermissionsTighteningOnly(reports)
	seenVerdicts := map[Verdict]bool{}
	var decisionRequested bool
	var decisionQuestion string
	var decisionQuestions []string
	var hasReject, hasChanges, unresolvedRequiresHuman, riskEscalation bool
	var selfAuthoredHumanWithConcreteRemediation, selfAuthoredHumanWithoutRemediation bool
	for i, r := range reports {
		effective := effectiveReports[i]
		if agg.Repo == "" {
			agg.Repo, agg.Number, agg.HeadSHA = r.Repo, r.Number, r.HeadSHA
			agg.BodyDigest = strings.TrimSpace(r.BodyDigest)
			agg.AuthorModel, agg.ReviewModel = r.AuthorModel, r.ReviewModel
		}
		if agg.BodyDigest == "" {
			agg.BodyDigest = strings.TrimSpace(r.BodyDigest)
		}
		if agg.AuthorModel == "" {
			agg.AuthorModel = r.AuthorModel
		}
		if agg.ReviewModel == "" {
			agg.ReviewModel = r.ReviewModel
		}
		if agg.Delta == nil && strings.TrimSpace(r.DeltaPreviousHead) != "" {
			agg.Delta = &DeltaReviewInfo{PreviousHead: strings.TrimSpace(r.DeltaPreviousHead)}
		}
		recordAggregateHead(&agg, r)
		if r.NotApplicable {
			// Nothing to judge is not a judgement withheld. Findings on a
			// not-applicable report are ignored: there was no plan to find
			// anything against.
			setStrictestPerspectiveVerdict(agg.Perspectives, r.Perspective, VerdictApprove)
			seenVerdicts[VerdictApprove] = true
			continue
		}
		setStrictestPerspectiveVerdict(agg.Perspectives, r.Perspective, effective.Verdict)
		seenVerdicts[effective.Verdict] = true
		switch effective.Verdict {
		case VerdictReject:
			hasReject = true
		case VerdictChangesRequested:
			hasChanges = true
		case VerdictRequiresHuman:
			if selfAuthoredRequiresHumanReportIsResolvable(r, opts) {
				if reportHasConcreteRemediation(r) {
					selfAuthoredHumanWithConcreteRemediation = true
				} else {
					selfAuthoredHumanWithoutRemediation = true
				}
			} else {
				unresolvedRequiresHuman = true
			}
		}
		if reportRequiresRiskEscalation(r) {
			riskEscalation = true
		}
		if r.RedundantOf > 0 && agg.RedundantOf == 0 {
			agg.RedundantOf = r.RedundantOf
		}
		if r.DecisionRequested {
			decisionRequested = true
			if decisionQuestion == "" {
				decisionQuestion = strings.TrimSpace(r.DecisionQuestion)
			}
			decisionQuestions = append(decisionQuestions, strings.TrimSpace(r.DecisionQuestion))
			agg.Reasons = append(agg.Reasons, decisionReason(r.DecisionQuestion))
		}
		if effective.Verdict == VerdictRequiresHuman {
			if summary := humanSummaryReason(r); summary != "" {
				agg.HumanSummaries = append(agg.HumanSummaries, summary)
				agg.Reasons = append(agg.Reasons, summary)
			}
		}
		for _, f := range r.Findings {
			agg.Findings = append(agg.Findings, PerspectiveFinding{Perspective: r.Perspective, Finding: f})
			if InScopeFinding(f) && severityAtLeast(f.Severity, threshold) {
				agg.Reasons = append(agg.Reasons, fmt.Sprintf("%s finding %q is %s (threshold %s)", r.Perspective, f.Title, f.Severity, threshold))
			}
		}
	}
	sortReasons(agg.Reasons)
	sort.Strings(agg.HumanSummaries)

	if hiveSupersedingFixRoute(agg, opts, hasReject, decisionRequested, riskEscalation, unresolvedRequiresHuman) {
		// Owner rule: a PR the hive created to supersede another PR auto-
		// merges with no operator decision. A reviewer's requires_human —
		// a risk-class question, a security-sensitive finding — is a
		// finding the fixer addresses and the swarm re-reviews, exactly
		// like a changes_requested; only an exhausted fix cycle hands the
		// head to a person (the stage engine's hard-stops cover the
		// sentinel, holds, red CI and conflicts).
		if finding, backed := securityFindingAcross(reports, sensitivePaths); backed {
			agg.DecisionFinding = finding
		}
		if opts.FixAttempts >= maxFix {
			agg.Verdict = VerdictRequiresHuman
			agg.RequiresHuman = true
			agg.DecisionRequested = decisionRequested
			agg.DecisionQuestion = decisionQuestion
			agg.Reasons = []string{fmt.Sprintf("fix cycle cap reached (%d/%d)", opts.FixAttempts, maxFix)}
			return agg
		}
		agg.Verdict = VerdictChangesRequested
		agg.FixCycle = true
		markRequiresHumanPerspectivesAsChangesRequested(&agg)
		agg.RequiresHuman = false
		agg.DecisionRequested = false
		agg.DecisionQuestion = ""
		agg.Reasons = append(append([]string{}, agg.Reasons...), HiveSupersedingFixRouteReason)
		sortReasons(agg.Reasons)
		return agg
	}

	if decisionRequested {
		if finding, backed := securityFindingAcross(reports, sensitivePaths); !backed && contributorHandsFree(agg, opts) {
			// Owner rule (hivecommons/hive#15640): no human decision while
			// a contributor's PR is in CI/Review unless the review found
			// something fishy or insecure. A question with no security
			// finding behind it is advisory: the PR keeps moving and the
			// person has the last word at the merge gate (090), where the
			// untrusted-author merge is theirs. Twelve of hive's own PRs
			// sat 3–21h on such questions.
			return contributorHandsFreeAggregate(agg, opts, hasChanges || reportsHaveInScopeFinding(reports), maxFix, decisionQuestions)
		} else if backed {
			agg.DecisionFinding = finding
		}
		if resolveSelfAuthoredDecisionQuestions(reports, opts, decisionQuestions, hasReject || riskEscalation || unresolvedRequiresHuman) {
			if hasChanges || selfAuthoredHumanWithConcreteRemediation {
				if opts.FixAttempts >= maxFix {
					agg.Verdict = VerdictRequiresHuman
					agg.RequiresHuman = true
					agg.Reasons = []string{fmt.Sprintf("fix cycle cap reached (%d/%d)", opts.FixAttempts, maxFix)}
					return agg
				}
				agg.Verdict = VerdictChangesRequested
				agg.FixCycle = true
				markRequiresHumanPerspectivesAsChangesRequested(&agg)
				agg.RequiresHuman = false
				agg.DecisionRequested = false
				agg.DecisionQuestion = ""
				agg.Reasons = append(append([]string{}, agg.Reasons...), selfAuthoredDecisionAdvisory(decisionQuestions))
				sortReasons(agg.Reasons)
				return agg
			}
			agg.Verdict = VerdictApprove
			agg.MergeEligible = true
			agg.RequiresHuman = false
			agg.DecisionRequested = false
			agg.DecisionQuestion = ""
			agg.Reasons = []string{selfAuthoredDecisionAdvisory(decisionQuestions)}
			return agg
		}
		agg.Verdict = VerdictRequiresHuman
		agg.RequiresHuman = true
		agg.DecisionRequested = true
		agg.DecisionQuestion = decisionQuestion
		return agg
	}
	if seenVerdicts[VerdictReject] {
		agg.Verdict = VerdictReject
		agg.CloseRecommended = true
		return agg
	}

	if seenVerdicts[VerdictChangesRequested] {
		if opts.FixAttempts >= maxFix {
			agg.Verdict = VerdictRequiresHuman
			agg.RequiresHuman = true
			agg.Reasons = []string{fmt.Sprintf("fix cycle cap reached (%d/%d)", opts.FixAttempts, maxFix)}
			return agg
		}
		agg.Verdict = VerdictChangesRequested
		agg.FixCycle = true
		markRequiresHumanPerspectivesAsChangesRequested(&agg)
		agg.RequiresHuman = false
		return agg
	}
	if len(agg.Reasons) > 0 || seenVerdicts[VerdictRequiresHuman] {
		if seenVerdicts[VerdictRequiresHuman] && contributorHandsFree(agg, opts) {
			// Same rule for a bare requires_human on a contributor's PR
			// (hivecommons/hive#15640): only a security finding parks it.
			if finding, backed := securityFindingAcross(reports, sensitivePaths); backed {
				agg.DecisionFinding = finding
			} else {
				return contributorHandsFreeAggregate(agg, opts, hasChanges || reportsHaveInScopeFinding(reports), maxFix, agg.HumanSummaries)
			}
		}
		if !riskEscalation && selfAuthoredHumanWithConcreteRemediation {
			if opts.FixAttempts >= maxFix {
				agg.Verdict = VerdictRequiresHuman
				agg.RequiresHuman = true
				agg.Reasons = []string{fmt.Sprintf("fix cycle cap reached (%d/%d)", opts.FixAttempts, maxFix)}
				return agg
			}
			agg.Verdict = VerdictChangesRequested
			agg.FixCycle = true
			markRequiresHumanPerspectivesAsChangesRequested(&agg)
			agg.RequiresHuman = false
			return agg
		}
		if !riskEscalation && !hasChanges && selfAuthoredHumanWithoutRemediation && !selfAuthoredHumanWithConcreteRemediation && selfAuthoredResolvableRequiresHumanOnly(reports, opts) {
			agg.Verdict = VerdictApprove
			agg.MergeEligible = true
			agg.RequiresHuman = false
			agg.Reasons = []string{"self-authored non-risk requires_human recorded as advisory"}
			return agg
		}
		agg.Verdict = VerdictRequiresHuman
		agg.RequiresHuman = true
		return agg
	}
	if len(seenVerdicts) == 1 && seenVerdicts[VerdictApprove] {
		if hasAllRequiredPerspectives(agg.Perspectives, opts.MaxPerspectivesPerPR, opts.Perspectives.Len()) {
			agg.Verdict = VerdictApprove
			agg.MergeEligible = true
			return agg
		}
		// Every report that arrived approved; the set is just short. That is
		// a review still in flight, not one a human must settle: labelling
		// it requires_human parked the PR and stopped the dispatcher from
		// ever asking for the perspective that never reported.
		agg.Verdict = VerdictIncomplete
		agg.Reasons = []string{incompleteCoverageReason(agg.Perspectives, opts)}
		return agg
	}
	agg.Verdict = VerdictRequiresHuman
	agg.RequiresHuman = true
	agg.Reasons = []string{"review perspectives did not unanimously approve"}
	return agg
}

// Normalize returns the routing verdict Hive should use for this aggregate.
// Older review-verdicts.json entries may carry the raw reviewer verdict even
// after newer aggregation policy would route the same evidence to the fixer
// lane. Normalizing those entries on read makes gate, stage labels, and fix
// dispatch consume one verdict without needing a re-review of the head. Known
// non-Hive authorship and fix-cycle caps stay human-owned holds.
func (a Aggregate) Normalize() Aggregate {
	if a.Verdict == VerdictChangesRequested && a.FixCycle && !a.DecisionRequested && !aggregateRequiresRiskEscalation(a) {
		markRequiresHumanPerspectivesAsChangesRequested(&a)
		return a
	}
	if a.Verdict != VerdictRequiresHuman || !a.RequiresHuman || a.DecisionRequested {
		return a
	}
	if a.HiveAuthoredKnown && !a.HiveAuthored {
		return a
	}
	if FixCycleCapReached(a) {
		return a
	}
	if aggregateRequiresRiskEscalation(a) || (!aggregateHasConcreteRemediation(a) && !aggregateHasBlockingPerspective(a)) {
		return a
	}
	a.Verdict = VerdictChangesRequested
	a.RequiresHuman = false
	a.FixCycle = true
	a.DecisionRequested = false
	a.DecisionQuestion = ""
	markRequiresHumanPerspectivesAsChangesRequested(&a)
	return a
}

// Normalize returns a copy with every aggregate normalized for routing.
func (a Artifact) Normalize() Artifact {
	out := a
	out.Items = append([]Aggregate(nil), a.Items...)
	for i := range out.Items {
		out.Items[i] = out.Items[i].Normalize()
	}
	return out
}

func aggregateHasBlockingPerspective(a Aggregate) bool {
	for _, v := range a.Perspectives {
		if v == VerdictChangesRequested || v == VerdictReject {
			return true
		}
	}
	return false
}

func markRequiresHumanPerspectivesAsChangesRequested(a *Aggregate) {
	if a == nil || len(a.Perspectives) == 0 {
		return
	}
	for p, v := range a.Perspectives {
		if v == VerdictRequiresHuman {
			a.Perspectives[p] = VerdictChangesRequested
		}
	}
}

// markRequiresHumanPerspectivesAsApprove is the approve counterpart: the
// relay stamps every perspective of a decision-requested review
// requires_human, so once the question is resolved as advisory the
// perspectives read as the approvals they were.
func markRequiresHumanPerspectivesAsApprove(a *Aggregate) {
	if a == nil || len(a.Perspectives) == 0 {
		return
	}
	for p, v := range a.Perspectives {
		if v == VerdictRequiresHuman {
			a.Perspectives[p] = VerdictApprove
		}
	}
}

// FixCycleCapReached reports whether an aggregate is held because the review
// repair loop exhausted its configured retry budget.
func FixCycleCapReached(a Aggregate) bool {
	if a.MaxFixAttempts > 0 && a.FixAttempts >= a.MaxFixAttempts {
		return true
	}
	for _, reason := range a.Reasons {
		if strings.Contains(strings.ToLower(reason), "fix cycle cap reached") {
			return true
		}
	}
	return false
}

func aggregateHasConcreteRemediation(a Aggregate) bool {
	if ConcreteRemediationText(a.DecisionQuestion) {
		return true
	}
	for _, text := range append(append([]string{}, a.Reasons...), a.HumanSummaries...) {
		if ConcreteRemediationText(text) {
			return true
		}
	}
	for _, f := range a.Findings {
		if !InScopeFinding(f.Finding) {
			continue
		}
		if strings.TrimSpace(f.Finding.File) != "" || f.Finding.Line > 0 || ConcreteRemediationText(f.Finding.Title) || ConcreteRemediationText(f.Finding.Summary) {
			return true
		}
	}
	return false
}

func aggregateRequiresRiskEscalation(a Aggregate) bool {
	tightening := a.PermissionsTighteningOnly
	if _, risk := DecisionQuestionRiskClassFor(a.DecisionQuestion, tightening); risk {
		return true
	}
	for _, text := range append(append([]string{}, a.Reasons...), a.HumanSummaries...) {
		if text == PermissionsTighteningReason {
			continue
		}
		if containsRiskEscalationTermFor(text, tightening) {
			return true
		}
	}
	for _, f := range a.Findings {
		if containsRiskEscalationTermFor(f.Finding.Title, tightening) || containsRiskEscalationTermFor(f.Finding.Summary, tightening) {
			return true
		}
	}
	return false
}

func reportsPermissionsTighteningOnly(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if r.PermissionsTighteningOnly {
			return true
		}
	}
	return false
}

func selfAuthoredRequiresHumanReportIsResolvable(r PerspectiveReport, opts AggregateOptions) bool {
	if !r.HiveAuthored || strings.EqualFold(strings.TrimSpace(opts.SelfAuthoredDecisionPolicy), "escalate") {
		return false
	}
	if r.Verdict != VerdictRequiresHuman || reportRequiresRiskEscalation(r) {
		return false
	}
	if r.DecisionRequested {
		return selfAuthoredDecisionResolvable(r)
	}
	return true
}

func selfAuthoredResolvableRequiresHumanOnly(reports []PerspectiveReport, opts AggregateOptions) bool {
	found := false
	for _, r := range reports {
		if r.Verdict != VerdictRequiresHuman {
			continue
		}
		if !selfAuthoredRequiresHumanReportIsResolvable(r, opts) {
			return false
		}
		found = true
	}
	return found
}

func reportHasConcreteRemediation(r PerspectiveReport) bool {
	if r.ConcreteRemediation || ConcreteRemediationText(r.Summary) {
		return true
	}
	for _, f := range r.Findings {
		if !InScopeFinding(f) {
			continue
		}
		if strings.TrimSpace(f.File) != "" || f.Line > 0 || ConcreteRemediationText(f.Title) || ConcreteRemediationText(f.Summary) {
			return true
		}
	}
	return false
}

func reportRequiresRiskEscalation(r PerspectiveReport) bool {
	tightening := r.PermissionsTighteningOnly
	if _, risk := DecisionQuestionRiskClassFor(r.DecisionQuestion, tightening); risk || r.DecisionRiskHigh {
		return true
	}
	if containsRiskEscalationTermFor(r.Summary, tightening) {
		return true
	}
	for _, f := range r.Findings {
		if containsRiskEscalationTermFor(f.Title, tightening) || containsRiskEscalationTermFor(f.Summary, tightening) {
			return true
		}
	}
	return false
}

// ConcreteRemediationText reports whether review prose names an actionable
// engineering fix instead of merely saying a person should decide.
func ConcreteRemediationText(text string) bool { return reviewtext.ConcreteRemediationText(text) }

// RiskEscalationText reports whether review prose names a category that must
// remain a human decision instead of being normalized to a fixer route.
func RiskEscalationText(text string) bool {
	return containsRiskEscalationTerm(text)
}

func containsRiskEscalationTerm(text string) bool {
	return containsRiskEscalationTermFor(text, false)
}

// containsRiskEscalationTermFor is containsRiskEscalationTerm with the
// permissions-tightening carve-out: on a PR whose diff only narrows workflow
// permissions, prose that merely names permissions is not an escalation.
func containsRiskEscalationTermFor(text string, permissionsTighteningOnly bool) bool {
	q := strings.ToLower(strings.TrimSpace(text))
	if q == "" {
		return false
	}
	for _, term := range riskEscalationTerms() {
		if permissionsTighteningOnly && permissionsTighteningSuppressedTerms[term] {
			continue
		}
		if strings.Contains(q, term) {
			return true
		}
	}
	return false
}

func riskEscalationTerms() []string { return reviewtext.RiskEscalationTerms() }

func resolveSelfAuthoredDecisionQuestions(reports []PerspectiveReport, opts AggregateOptions, questions []string, blocked bool) bool {
	if blocked || strings.EqualFold(strings.TrimSpace(opts.SelfAuthoredDecisionPolicy), "escalate") {
		return false
	}
	if len(questions) == 0 || !reportsHiveAuthored(reports) {
		return false
	}
	for _, r := range reports {
		if !r.DecisionRequested {
			continue
		}
		if !selfAuthoredDecisionResolvable(r) {
			return false
		}
	}
	return true
}

// HiveSupersedingFixRouteReason is the aggregate reason recorded when a
// hive-authored superseding PR's requires_human was routed to the fixer
// instead of an operator decision (owner rule, @clubanderson: "if a pr is
// superseded by a pr that a hive agent creates, it should auto merge").
const HiveSupersedingFixRouteReason = "hive-authored superseding PR: reviewer requires_human routed to the fixer; auto-merges once CI and review are clean"

// hiveSupersedingFixRoute says whether this aggregate is one the owner's
// superseder rule turns from a human park into a fix cycle: the PR is the
// hive's superseding copy, the knob is on, no perspective rejected, and
// the only thing holding it is a requires_human / decision question the
// ordinary self-authored resolution would have escalated (a risk-class
// question, a security-sensitive finding, an unresolvable summary). A
// reject stays a reject: it is an emergent problem with the PR, not a
// finding to fix. Everything the self-authored path already resolves on
// its own (non-risk questions, concrete remediation) keeps that path.
func hiveSupersedingFixRoute(agg Aggregate, opts AggregateOptions, hasReject, decisionRequested, riskEscalation, unresolvedRequiresHuman bool) bool {
	if !agg.HiveSuperseding || !agg.HiveAuthored || opts.HiveSupersedingAutoMergeOff || hasReject {
		return false
	}
	if strings.EqualFold(strings.TrimSpace(opts.SelfAuthoredDecisionPolicy), "escalate") {
		return false
	}
	return unresolvedRequiresHuman || (decisionRequested && riskEscalation)
}

func reportsHiveSuperseding(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if r.HiveSuperseding {
			return true
		}
	}
	return false
}

func reportsHiveAuthored(reports []PerspectiveReport) bool {
	for _, r := range reports {
		if r.HiveAuthored {
			return true
		}
	}
	return false
}

// selfAuthoredDecisionResolvable is the denylist test for one reviewer
// decision question on a Hive-authored PR: the hive resolves the question
// itself unless the reviewer marked it high-risk or its wording lands in a
// DecisionRiskClass. Before hivecommons/hive#13020 this was an allowlist of
// "non-risk" words (naming, scope, changelog, …) and every question outside
// it parked a green, agent-authored PR for a person -- including one whose
// answer the linked issue had already written down.
func selfAuthoredDecisionResolvable(r PerspectiveReport) bool {
	q := strings.TrimSpace(r.DecisionQuestion)
	if q == "" || r.DecisionRiskHigh || r.RedundantOf > 0 {
		// A redundancy question ("close as a no-op, #N already landed
		// it?") has no approve answer: resolving it would merge an empty
		// PR. It stays a decision so the superseded closer can act.
		return false
	}
	_, risk := DecisionQuestionRiskClassFor(q, r.PermissionsTighteningOnly)
	return !risk
}

// DecisionRiskClass names one category of reviewer decision question that
// stays a human's call on a self-authored PR. Terms are matched as lower-case
// substrings; Patterns are whole-word matches for terms too short to be safe
// as substrings ("auth" inside "author", "role" inside "controller").
type DecisionRiskClass struct {
	Name     string
	Terms    []string
	Patterns []*regexp.Regexp
}

// DecisionRiskClasses is the denylist that parks a Hive-authored PR's
// reviewer decision question at stage/085-needs-decision. Anything a
// reviewer asks that matches none of these is the hive's own engineering
// call and is resolved by policy; the question is kept as an audit reason.
// The sensitive-path class is derived from sentinel.DefaultSensitivePaths so
// the two lists cannot drift apart.
var DecisionRiskClasses = []DecisionRiskClass{
	{
		Name: "permissions-credentials",
		Terms: []string{
			"permission", "privilege", "token", "secret", "credential", "password",
			"api key", "apikey", "write access", "admin access", ": write", "contents: write",
			"be granted", "grant ", "escalat", "sudo", "root access",
		},
	},
	{
		Name: "security",
		Terms: []string{
			"security", "vulnerab", "cve-", "exploit", "injection", "sanitiz", "xss", "csrf",
			"crypto", "cipher", "signing key", "certificate", "tls ", "ssl ", "authentication", "authorization",
		},
		Patterns: []*regexp.Regexp{
			regexp.MustCompile(`\bauth[nz]?\b`), regexp.MustCompile(`\boauth\b`), regexp.MustCompile(`\bsso\b`),
		},
	},
	{
		Name:  "sensitive-paths",
		Terms: append([]string{"workflow", "codeowners", "branch protection", "ruleset"}, sentinelSensitivePathTerms()...),
	},
	{
		Name: "data-loss-irreversible",
		Terms: []string{
			"data loss", "delete", "destructive", "irreversible", "migration", "truncate",
			"wipe", "purge", "overwrite", "drop the", "drop a ", "drop column", "drop table",
			"breaking", "public-facing", "cannot be undone", "can't be undone", "force-push", "force push",
		},
	},
	{
		Name: "merge-trust-policy",
		Terms: []string{
			"merge polic", "automerge", "auto-merge", "auto merge", "trust tier", "trusted tier", "trust level",
			"acmm", "maintainer tier", "approver", "governance", "policy", "owners file", "who may merge", "who can merge",
		},
		Patterns: []*regexp.Regexp{regexp.MustCompile(`\brole\b`), regexp.MustCompile(`\broles\b`), regexp.MustCompile(`\blevel [0-9]\b`)},
	},
	{
		Name: "external-side-effects",
		Terms: []string{
			"publish", "release", "tag a version", "deploy to", "billing", "spend", "cost", "paid ",
			"email", "e-mail", "notify humans", "notification to", "notifications to", "outbound", "webhook",
			"announce", "post to slack", "post to discord", "npm registry", "pypi", "docker hub", "ghcr",
		},
	},
	{
		Name: "licensing-legal",
		Terms: []string{
			"license", "licensing", "copyright", "legal", "privacy", "compliance", "gdpr", "trademark", "cla ",
		},
	},
	{
		// A question the reviewer admits it could not judge is not a question
		// the hive can answer for it either.
		Name: "unverifiable",
		Terms: []string{
			"unreadable", "could not read", "cannot read", "can't read", "could not judge", "cannot judge",
			"missing context", "unfamiliar subsystem", "ambiguous", "cannot test", "can't test", "high-risk",
		},
	},
}

// DecisionQuestionRiskClass classifies a reviewer decision question against
// DecisionRiskClasses and returns the first class it lands in.
func DecisionQuestionRiskClass(question string) (string, bool) {
	return DecisionQuestionRiskClassFor(question, false)
}

// DecisionQuestionRiskClassFor is DecisionQuestionRiskClass with the
// permissions-tightening carve-out (hivecommons/hive#13068): when the PR's
// workflow diff was proved to only narrow `permissions:`, the
// direction-blind permissions-credentials and sensitive-paths classes are
// skipped, so "confirm the permissions narrowing is acceptable" is the
// hive's own call. Every other class still applies to the same question.
func DecisionQuestionRiskClassFor(question string, permissionsTighteningOnly bool) (string, bool) {
	q := strings.ToLower(strings.TrimSpace(question))
	if q == "" {
		return "", false
	}
	for _, class := range DecisionRiskClasses {
		if permissionsTighteningOnly && permissionsTighteningSuppressedClasses[class.Name] {
			continue
		}
		for _, term := range class.Terms {
			if strings.Contains(q, term) {
				return class.Name, true
			}
		}
		for _, pat := range class.Patterns {
			if pat.MatchString(q) {
				return class.Name, true
			}
		}
	}
	return "", false
}

// sentinelSensitivePathTerms flattens sentinel.DefaultSensitivePaths globs
// into lower-case substrings a decision question could name: "**/OWNERS" ->
// "owners", ".github/workflows/**" -> ".github/workflows". Fragments shorter
// than four characters ("*.tf" -> ".tf") are dropped as too noisy.
func sentinelSensitivePathTerms() []string {
	const minLen = 4
	seen := map[string]bool{}
	var terms []string
	for _, glob := range sentinel.DefaultSensitivePaths {
		term := strings.ToLower(glob)
		term = strings.ReplaceAll(term, "**/", "")
		term = strings.ReplaceAll(term, "/**", "")
		term = strings.ReplaceAll(term, "*", "")
		term = strings.Trim(term, "/")
		if len(term) < minLen || seen[term] {
			continue
		}
		seen[term] = true
		terms = append(terms, term)
	}
	sort.Strings(terms)
	return terms
}

// decisionTopic gives the audit line a short name for what kind of
// engineering choice the hive resolved. It is descriptive only: the
// resolve/escalate decision is DecisionQuestionRiskClass's.
func decisionTopic(question string) string {
	q := strings.ToLower(question)
	topics := []struct {
		name  string
		terms []string
	}{
		{"sequencing", []string{"land before", "land now", "land later", "sequenc", "groundwork", "consumer"}},
		{"naming", []string{"naming", "rename", "namespace", "flag name", "label name", "called"}},
		{"default-value", []string{"default"}},
		{"config-shape", []string{"config shape", "configuration shape", "config namespace", "nesting", "top-level", "ledger path"}},
		{"scope", []string{"scope", "placement", "organize", "organisation", "organization", "dead-code", "dead code", "loader"}},
		{"changelog", []string{"changelog", "fragment", "duplicate", "trim", "reword"}},
	}
	for _, t := range topics {
		for _, term := range t.terms {
			if strings.Contains(q, term) {
				return t.name
			}
		}
	}
	return "engineering-choice"
}

func selfAuthoredDecisionAdvisory(questions []string) string {
	var cleaned []string
	for _, q := range questions {
		if q = strings.TrimSpace(q); q != "" {
			cleaned = append(cleaned, q)
		}
	}
	sort.Strings(cleaned)
	if len(cleaned) == 0 {
		return "decision resolved by policy: engineering-choice — not a risk question"
	}
	topics := map[string]bool{}
	var names []string
	for _, q := range cleaned {
		if t := decisionTopic(q); !topics[t] {
			topics[t] = true
			names = append(names, t)
		}
	}
	sort.Strings(names)
	return "decision resolved by policy: " + strings.Join(names, ", ") + " — not a risk question; recorded as advisory: " + strings.Join(cleaned, " | ")
}

func decisionReason(question string) string {
	question = strings.TrimSpace(question)
	if question == "" {
		return "maintainer decision requested"
	}
	return "maintainer decision requested: " + question
}

func humanSummaryReason(r PerspectiveReport) string {
	summary := strings.TrimSpace(r.Summary)
	if summary == "" {
		return ""
	}
	return fmt.Sprintf("%s requires human: %s", r.Perspective, summary)
}

// incompleteCoverageReason names the configured perspectives this head has
// not yet received a report for. Under a per-PR cap the head may not be owed
// all of them, so the reason also says how many more are needed.
func incompleteCoverageReason(got map[Perspective]Verdict, opts AggregateOptions) string {
	var missing []Perspective
	for _, p := range opts.Perspectives.List() {
		if _, ok := got[p]; !ok {
			missing = append(missing, p)
		}
	}
	if len(missing) == 0 {
		return "review coverage incomplete"
	}
	required := requiredPerspectiveCount(opts.MaxPerspectivesPerPR, opts.Perspectives.Len())
	if need := required - len(got); need > 0 && need < len(missing) {
		return fmt.Sprintf("review coverage incomplete: %d more of %s", need, joinPerspectives(missing))
	}
	return "review coverage incomplete: missing " + joinPerspectives(missing)
}

// ScopeEffectiveReports returns copies of reports with out-of-scope findings
// removed from verdict/confidence inputs. If a report only contains
// out-of-scope findings, its effective verdict is approve: the findings still
// travel in the original report for backlog filing, but do not block the PR.
func ScopeEffectiveReports(reports []PerspectiveReport) []PerspectiveReport {
	out := make([]PerspectiveReport, len(reports))
	for i, r := range reports {
		out[i] = r
		if r.NotApplicable {
			continue
		}
		inScope := make([]outputschema.Finding, 0, len(r.Findings))
		outOfScope := 0
		for _, f := range r.Findings {
			if InScopeFinding(f) {
				inScope = append(inScope, f)
			} else {
				outOfScope++
			}
		}
		out[i].Findings = append([]outputschema.Finding(nil), inScope...)
		if outOfScope > 0 && len(inScope) == 0 {
			out[i].Verdict = VerdictApprove
		}
	}
	return out
}

// InScopeFinding reports whether a review finding counts against the PR under
// the linked issue's scope contract. Reports written before review_scope
// existed are treated as in-scope to preserve their original routing semantics.
func InScopeFinding(f outputschema.Finding) bool {
	return strings.TrimSpace(f.ReviewScope) != "out-of-scope"
}

func Collect(dir string, opts AggregateOptions) (Artifact, error) {
	dir = ReportDir(dir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return Artifact{}, err
	}
	groups := map[string][]PerspectiveReport{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ReviewReportFilePrefix) || !strings.HasSuffix(name, ReviewReportFileSuffix) {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return Artifact{}, err
		}
		// Reports on disk are accepted for every built-in perspective plus
		// whatever this hive configured, not the selected set alone: a
		// perspective toggled off after its report landed must not make the
		// whole collect fail on a file the hive itself wrote.
		report, err := ValidateReportFor(raw, opts.Perspectives.acceptingBuiltins())
		if err != nil {
			return Artifact{}, fmt.Errorf("%s: %w", name, err)
		}
		opts.ModelStamps.apply(report)
		key := reviewKey(report.Repo, report.Number, report.HeadSHA, report.BodyDigest)
		groups[key] = append(groups[key], *report)
	}
	groups = foldDigestlessReports(groups)
	artifact := Artifact{GeneratedAt: time.Now().UTC()}
	for _, key := range sortedKeys(groups) {
		artifact.Items = append(artifact.Items, AggregateReports(groups[key], opts))
	}
	return artifact, nil
}

// foldDigestlessReports merges reports that carry no body digest into the
// digest-keyed group for the same repo/number/head when exactly one such
// group exists. Perspective reports from one swarm run do not all record the
// digest (observed: style on hivecommons/hive#12421, correctness and
// docs-currency on #12332), and keyed apart they read as two "incomplete"
// aggregates — a 5/5 approval never became merge-eligible. With several
// digest groups for a head the digestless reports cannot be attributed and
// stay separate.
func foldDigestlessReports(groups map[string][]PerspectiveReport) map[string][]PerspectiveReport {
	byHead := map[string][]string{}
	for key, reports := range groups {
		if len(reports) == 0 || strings.TrimSpace(reports[0].BodyDigest) == "" {
			continue
		}
		head := reviewKey(reports[0].Repo, reports[0].Number, reports[0].HeadSHA, "")
		byHead[head] = append(byHead[head], key)
	}
	for head, keys := range byHead {
		if len(keys) != 1 {
			continue
		}
		loose, ok := groups[head]
		if !ok {
			continue
		}
		groups[keys[0]] = append(groups[keys[0]], loose...)
		delete(groups, head)
	}
	return groups
}

func WriteArtifact(path string, artifact Artifact) error {
	artifact = artifact.Normalize()
	if path == "" {
		path = ReviewVerdictsPath
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func CollectAndWrite(dir, path string, opts AggregateOptions) (Artifact, error) {
	artifact, err := Collect(dir, opts)
	if err != nil {
		return Artifact{}, err
	}
	artifact = artifact.Normalize()
	return artifact, WriteArtifact(path, artifact)
}

// VerdictRetention bounds how long a verdict stays in the durable artifact
// after it was last seen. Long enough to outlive any realistic review cycle,
// short enough that the file does not grow without bound as PRs close.
const VerdictRetention = 30 * 24 * time.Hour

// retainsOverIncomplete reports whether a prior aggregate verdict is a settled
// non-approve decision that a partial re-review must not displace.
func retainsOverIncomplete(v Verdict) bool {
	return v == VerdictRequiresHuman || v == VerdictChangesRequested || v == VerdictReject
}

// CollectAndMerge refreshes the durable verdict artifact without losing
// verdicts whose per-perspective reports have aged out of the report dir.
//
// Collect rebuilds from review-report-*.json files, which live on the
// container's ephemeral layer. A plain collect-and-replace therefore shrinks
// the artifact every time that layer is reset: the hive forgets what it had
// already judged and re-reviews those PRs, posting a second (and sixth) round
// of comments on work it had already handled.
//
// Merging keeps the union. Freshly collected verdicts win for a given
// repo/number/head-SHA/body-digest, except that incomplete coverage cannot
// replace a settled non-approve verdict (requires_human,
// changes_requested, reject). Previously recorded verdicts survive,
// and anything not re-confirmed within VerdictRetention is dropped.
func CollectAndMerge(dir, path string, opts AggregateOptions, now time.Time) (Artifact, error) {
	fresh, err := Collect(dir, opts)
	if err != nil {
		return Artifact{}, err
	}
	if now.IsZero() {
		now = time.Now().UTC()
	}

	// A missing or unreadable prior artifact is not fatal: this may be the
	// first run, in which case the fresh collect is the whole truth.
	merged := map[string]Aggregate{}
	order := []string{}
	if existing, loadErr := LoadArtifact(path); loadErr == nil {
		for _, item := range existing.Items {
			if now.Sub(item.RecordedAt) > VerdictRetention && !item.RecordedAt.IsZero() {
				continue
			}
			key := reviewKey(item.Repo, item.Number, item.HeadSHA, item.BodyDigest)
			if _, seen := merged[key]; !seen {
				order = append(order, key)
			}
			merged[key] = item
		}
	}

	for _, item := range fresh.Items {
		item.RecordedAt = now
		key := reviewKey(item.Repo, item.Number, item.HeadSHA, item.BodyDigest)
		if prior, ok := merged[key]; ok && retainsOverIncomplete(prior.Verdict) && item.Verdict == VerdictIncomplete {
			// A re-review can arrive one perspective at a time. Missing
			// reports do not withdraw a settled non-approve verdict (human
			// decision and its question, fix cycle, or reject): doing so sends
			// a settled head back to 050 — requires_human from 085, and
			// changes_requested/reject from the fix path — on the same head.
			// Keep its timestamp too, so partial coverage cannot extend the
			// retention window or make a deliberate revisit look complete.
			continue
		}
		if _, seen := merged[key]; !seen {
			order = append(order, key)
		}
		merged[key] = item
	}

	artifact := Artifact{GeneratedAt: now, Items: make([]Aggregate, 0, len(order))}
	for _, key := range order {
		artifact.Items = append(artifact.Items, merged[key])
	}
	artifact = artifact.Normalize()
	if err := WriteArtifact(path, artifact); err != nil {
		return artifact, err
	}
	// Only after the merge is durable: a report is redundant once its verdict
	// is in the artifact, but it is deleted on the artifact's own horizon, not
	// immediately, because a combined verdict lands one file per perspective
	// and a collect between two of those files must not turn the survivors
	// into a partial aggregate.
	_, _ = PruneReports(dir, VerdictRetention, now)
	return artifact, nil
}

// PruneReports removes review-report-*.json files in dir whose modification
// time is older than olderThan. Reports live on the durable data dir now
// (DefaultReportDir), so without this they would accumulate for the life of
// the volume. It is best-effort: an entry that cannot be inspected or removed
// is skipped, never fatal, because a stale file costs a few kilobytes and a
// failed collect costs a review cycle. Returns the number of files removed.
func PruneReports(dir string, olderThan time.Duration, now time.Time) (int, error) {
	dir = ReportDir(dir)
	if now.IsZero() {
		now = time.Now().UTC()
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, ReviewReportFilePrefix) || !strings.HasSuffix(name, ReviewReportFileSuffix) {
			continue
		}
		info, err := entry.Info()
		if err != nil || now.Sub(info.ModTime()) <= olderThan {
			continue
		}
		if err := os.Remove(filepath.Join(dir, name)); err == nil {
			removed++
		}
	}
	return removed, nil
}

func LoadArtifact(path string) (Artifact, error) {
	explicit := path != ""
	if !explicit {
		path = ReviewVerdictsPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		// Only the default path migrates. An explicit path is a caller's
		// deliberate choice and must not silently read some other file.
		if !explicit && errors.Is(err, os.ErrNotExist) && LegacyReviewVerdictsPath != ReviewVerdictsPath {
			legacy, legacyErr := os.ReadFile(LegacyReviewVerdictsPath)
			if legacyErr != nil {
				return Artifact{}, err
			}
			data = legacy
		} else {
			return Artifact{}, err
		}
	}
	var artifact Artifact
	if err := json.Unmarshal(data, &artifact); err != nil {
		return Artifact{}, err
	}
	return artifact.Normalize(), nil
}

func (a Artifact) HasAggregateApproval(repo string, number int, headSHA string) bool {
	wantRepo := strings.TrimSpace(repo)
	wantSHA := strings.TrimSpace(headSHA)
	var match Aggregate
	var ok bool
	for _, item := range a.Items {
		item = item.Normalize()
		if item.Number != number || strings.TrimSpace(item.Repo) != wantRepo || !SameHeadSHA(item.HeadSHA, wantSHA) {
			continue
		}
		if !ok || item.RecordedAt.After(match.RecordedAt) {
			match = item
			ok = true
		}
	}
	return ok && match.MergeEligible && match.Verdict == VerdictApprove
}

func agentReportJSON(r outputschema.AgentReport) []byte {
	data, _ := json.Marshal(r)
	return data
}

func validVerdict(v Verdict) bool {
	switch v {
	case VerdictApprove, VerdictChangesRequested, VerdictRequiresHuman, VerdictReject:
		return true
	default:
		return false
	}
}

func severityAtLeast(got, threshold outputschema.Severity) bool {
	return severityRank(got) >= severityRank(threshold)
}

func severityRank(s outputschema.Severity) int {
	switch s {
	case outputschema.SeverityInfo:
		return 1
	case outputschema.SeverityLow:
		return 2
	case outputschema.SeverityMedium:
		return 3
	case outputschema.SeverityHigh:
		return 4
	case outputschema.SeverityCritical:
		return 5
	default:
		return 0
	}
}

// hasAllRequiredPerspectives decides whether enough perspectives approved to
// call the review unanimous.
//
// The bar was every perspective in DefaultPerspectives. That was right while
// every PR received all five, but max_perspectives_per_pr caps how many a PR is
// ever given — and a perspective that is never dispatched can never approve. At
// a cap of 1, "unanimous" became unreachable: every PR aggregated to
// requires_human with the reason "review perspectives did not unanimously
// approve", approve and merge_eligible could not occur, and a hive running
// require_approval could never clear anything.
//
// The bar is therefore what the PR was eligible to receive, not the full set.
//
// configured is how many perspectives the hive reviews with (0 means the
// built-in set). A hive that selected three perspectives must reach unanimity
// on three -- holding it to five would make approve unreachable for the same
// reason the cap did.
func hasAllRequiredPerspectives(got map[Perspective]Verdict, maxPerPR, configured int) bool {
	if len(got) < requiredPerspectiveCount(maxPerPR, configured) {
		return false
	}
	for _, v := range got {
		if v != VerdictApprove {
			return false
		}
	}
	return true
}

// requiredPerspectiveCount is how many perspectives a head must hear from
// before its approvals count as unanimous: the configured set, capped by
// max_perspectives_per_pr.
func requiredPerspectiveCount(maxPerPR, configured int) int {
	required := configured
	if required <= 0 {
		required = len(DefaultPerspectives)
	}
	if maxPerPR > 0 && maxPerPR < required {
		required = maxPerPR
	}
	return required
}

func joinPerspectives(ps []Perspective) string {
	parts := make([]string, 0, len(ps))
	for _, p := range ps {
		parts = append(parts, string(p))
	}
	return strings.Join(parts, ", ")
}

func sortReasons(reasons []string) {
	sort.Strings(reasons)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func reviewKey(repo string, number int, headSHA, bodyDigest string) string {
	if strings.TrimSpace(bodyDigest) == "" {
		return fmt.Sprintf("%s#%d@%s", repo, number, headSHA)
	}
	return fmt.Sprintf("%s#%d@%s body:%s", repo, number, headSHA, bodyDigest)
}

// verdictStrictness orders verdicts for folding several reports on one
// perspective into the aggregate's per-perspective verdict: the strictest
// wins. Review heads' raw reports are folded this way only for display while
// the decision step is owed (hivecommons/hive#13542).
func verdictStrictness(v Verdict) int {
	switch v {
	case VerdictReject:
		return 4
	case VerdictRequiresHuman:
		return 3
	case VerdictChangesRequested:
		return 2
	case VerdictApprove:
		return 1
	default:
		return 0
	}
}

// setStrictestPerspectiveVerdict records v for p unless a stricter verdict
// for p is already recorded. With one report per perspective, the case for
// every hive without review_heads, it is a plain assignment.
func setStrictestPerspectiveVerdict(m map[Perspective]Verdict, p Perspective, v Verdict) {
	if cur, ok := m[p]; ok && verdictStrictness(cur) >= verdictStrictness(v) {
		return
	}
	m[p] = v
}

// recordAggregateHead notes which review head a report came from.
func recordAggregateHead(agg *Aggregate, r PerspectiveReport) {
	head := strings.TrimSpace(r.Head)
	if head == "" {
		return
	}
	if agg.Heads == nil {
		agg.Heads = map[Perspective][]string{}
	}
	for _, h := range agg.Heads[r.Perspective] {
		if h == head {
			return
		}
	}
	agg.Heads[r.Perspective] = append(agg.Heads[r.Perspective], head)
	sort.Strings(agg.Heads[r.Perspective])
}
