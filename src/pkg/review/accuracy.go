package review

import (
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/modelfamily"
)

const ReviewerAccuracyDefaultWindow = 30 * 24 * time.Hour

type ReviewerAccuracyEvidence struct {
	// ReworkedAfterApproval is GitHub-history evidence that an approval was
	// followed by corrective work before merge: human change requests, fix
	// attempts, or follow-up commits in the reviewed PR.
	ReworkedAfterApproval bool
	// MergedUnchangedAfterBlock is GitHub-history evidence that a
	// changes-requested verdict was not acted on before merge.
	MergedUnchangedAfterBlock bool
	// MaintainerOverride records an explicit override of a blocking verdict.
	MaintainerOverride bool
}

type ReviewerAccuracySummary struct {
	GeneratedAt      time.Time               `json:"generated_at"`
	WindowDays       int                     `json:"window_days"`
	Samples          int                     `json:"samples"`
	RecordedVerdicts int                     `json:"recorded_verdicts"`
	MissingOutcomes  int                     `json:"missing_outcomes"`
	Perspectives     []ReviewerAccuracyGroup `json:"perspectives"`
	ReviewerModels   []ReviewerAccuracyGroup `json:"reviewer_models"`
	// AuthorFamilies groups the same verdicts by the PR author's model
	// family (modelfamily.Bucket: claude / gpt / gemini / other, "unknown"
	// for an agent whose model was not recorded). A person's PR has no
	// author model and lands under "human".
	AuthorFamilies []ReviewerAccuracyGroup `json:"author_families"`
	// FamilyMatch splits verdicts into "same" (reviewer and author model
	// share a family), "cross" and "unknown" (either side unrecorded), so a
	// cross-family review_models pool has a visible effect to compare.
	FamilyMatch []ReviewerAccuracyGroup `json:"family_match"`
	// ReviewHeads scores each review head's raw reports against the decided
	// outcome of the PR heads it judged (hivecommons/hive#13542), grouped by
	// the head's review model. The decided verdict and the decision step's
	// dispositions are the reference, so it needs no merge outcome.
	ReviewHeads []ReviewerHeadAgreementGroup `json:"review_heads,omitempty"`
}

// ReviewerHeadAgreementGroup is one review model's record as a review head.
// A sample is one (PR head, perspective) the head judged. MissedBlocks: the
// head approved a perspective the decision blocked. OverBlocks: the head
// blocked a perspective the decision approved. Findings are counted against
// the decision step's dispositions: confirmed, or rejected (not reproducible
// or out of scope).
type ReviewerHeadAgreementGroup struct {
	Name              string  `json:"name"`
	Samples           int     `json:"samples"`
	Agreements        int     `json:"agreements"`
	MissedBlocks      int     `json:"missed_blocks"`
	OverBlocks        int     `json:"over_blocks"`
	FindingsRaised    int     `json:"findings_raised"`
	FindingsConfirmed int     `json:"findings_confirmed"`
	FindingsRejected  int     `json:"findings_rejected"`
	AgreementRate     float64 `json:"agreement_rate"`
	ConfirmRate       float64 `json:"confirm_rate"`
}

// ReviewerAccuracyOptions tunes SummarizeReviewerAccuracyWith.
type ReviewerAccuracyOptions struct {
	// FallbackReviewModel is attributed to a verdict that recorded no
	// review_model: the reviewer agent's launch model, the same fallback
	// pkg/github applies when it writes evidence bundles. Empty keeps such
	// verdicts under "unknown".
	FallbackReviewModel string
}

// Family-match group names.
const (
	FamilyMatchSame    = "same"
	FamilyMatchCross   = "cross"
	FamilyMatchUnknown = "unknown"
)

// AuthorFamilyBucket is the author-family group a verdict belongs to: the
// model's family bucket when one was recorded, "human" when the PR is known
// to be a person's (no model, not hive-authored), "unknown" otherwise.
func AuthorFamilyBucket(authorModel string, hiveAuthoredKnown, hiveAuthored bool) string {
	if strings.TrimSpace(authorModel) != "" {
		return modelfamily.Bucket(authorModel)
	}
	if hiveAuthoredKnown && !hiveAuthored {
		return modelfamily.BucketHuman
	}
	return modelfamily.Unknown
}

// FamilyMatchBucket classifies a reviewer/author model pair.
func FamilyMatchBucket(reviewModel, authorModel string) string {
	same, known := modelfamily.SameFamily(reviewModel, authorModel)
	switch {
	case !known:
		return FamilyMatchUnknown
	case same:
		return FamilyMatchSame
	default:
		return FamilyMatchCross
	}
}

type ReviewerAccuracyGroup struct {
	Name              string                                `json:"name"`
	Samples           int                                   `json:"samples"`
	Approvals         int                                   `json:"approvals"`
	Blocks            int                                   `json:"blocks"`
	FalseApprovals    int                                   `json:"false_approvals"`
	FalseBlocks       int                                   `json:"false_blocks"`
	FalseApproveRate  float64                               `json:"false_approve_rate"`
	FalseBlockRate    float64                               `json:"false_block_rate"`
	ConfidenceBuckets []ReviewerConfidenceCalibrationBucket `json:"confidence_buckets,omitempty"`
}

type ReviewerConfidenceCalibrationBucket struct {
	Bucket         string  `json:"bucket"`
	Samples        int     `json:"samples"`
	Merged         int     `json:"merged"`
	BadOutcomes    int     `json:"bad_outcomes"`
	MergeRate      float64 `json:"merge_rate"`
	BadOutcomeRate float64 `json:"bad_outcome_rate"`
}

// SummarizeReviewerAccuracy calibrates recorded reviewer verdicts against the
// outcome ledger and cached GitHub history. It deliberately accepts the data as
// parameters so the dashboard endpoint and tests use the same pure aggregation.
func SummarizeReviewerAccuracy(now time.Time, window time.Duration, artifact Artifact, ledger *OutcomeLedger, evidence map[string]ReviewerAccuracyEvidence) ReviewerAccuracySummary {
	return SummarizeReviewerAccuracyWith(now, window, artifact, ledger, evidence, ReviewerAccuracyOptions{})
}

// SummarizeReviewerAccuracyWith is SummarizeReviewerAccuracy with options.
func SummarizeReviewerAccuracyWith(now time.Time, window time.Duration, artifact Artifact, ledger *OutcomeLedger, evidence map[string]ReviewerAccuracyEvidence, ropts ReviewerAccuracyOptions) ReviewerAccuracySummary {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	now = now.UTC()
	if window <= 0 {
		window = ReviewerAccuracyDefaultWindow
	}
	since := now.Add(-window)
	sum := ReviewerAccuracySummary{GeneratedAt: now, WindowDays: int(window.Hours() / 24)}
	if ledger == nil {
		ledger = &OutcomeLedger{Items: map[string]*PROutcome{}}
	}
	perspectives := map[string]*accuracyAccumulator{}
	models := map[string]*accuracyAccumulator{}
	families := map[string]*accuracyAccumulator{}
	matches := map[string]*accuracyAccumulator{}
	fallbackModel := strings.TrimSpace(ropts.FallbackReviewModel)
	seen := map[string]bool{}
	seenRecorded := map[string]bool{}
	missingOutcomes := map[string]bool{}
	heads := map[string]*ReviewerHeadAgreementGroup{}
	for _, item := range artifact.Items {
		if item.RecordedAt.IsZero() || item.RecordedAt.Before(since) {
			continue
		}
		scoreReviewHeads(heads, item, fallbackModel)
		key := reviewKey(item.Repo, item.Number, item.HeadSHA, "")
		if !seenRecorded[key] {
			seenRecorded[key] = true
			sum.RecordedVerdicts++
		}
		outcome := ledger.Items[outcomeKey(item.Repo, item.Number)]
		if outcome == nil || outcome.Outcome == OutcomeOpen {
			missingOutcomes[key] = true
			continue
		}
		if !seen[key] {
			seen[key] = true
			sum.Samples++
		}
		ev := evidence[outcomeKey(item.Repo, item.Number)]
		for p, verdict := range item.Perspectives {
			name := strings.TrimSpace(string(p))
			if name == "" {
				name = "unknown"
			}
			acc := accuracyAcc(perspectives, name)
			acc.add(verdict, item.Confidence.Score, outcome, ev)
		}
		model := strings.TrimSpace(item.ReviewModel)
		if model == "" {
			model = fallbackModel
		}
		if model == "" {
			model = modelfamily.Unknown
		}
		accuracyAcc(models, model).add(item.Verdict, item.Confidence.Score, outcome, ev)
		accuracyAcc(families, AuthorFamilyBucket(item.AuthorModel, item.HiveAuthoredKnown, item.HiveAuthored)).add(item.Verdict, item.Confidence.Score, outcome, ev)
		accuracyAcc(matches, FamilyMatchBucket(model, item.AuthorModel)).add(item.Verdict, item.Confidence.Score, outcome, ev)
	}
	sum.MissingOutcomes = len(missingOutcomes)
	sum.Perspectives = finishAccuracyGroups(perspectives)
	sum.ReviewerModels = finishAccuracyGroups(models)
	sum.AuthorFamilies = finishAccuracyGroups(families)
	sum.FamilyMatch = finishAccuracyGroups(matches)
	sum.ReviewHeads = finishHeadAgreementGroups(heads)
	return sum
}

// scoreReviewHeads adds one aggregate's head reports to the per-model head
// scores. Only a settled review-heads outcome is a reference: decided, or the
// skip rule's unanimous clean approval.
func scoreReviewHeads(groups map[string]*ReviewerHeadAgreementGroup, item Aggregate, fallbackModel string) {
	if len(item.HeadReports) == 0 || (!item.Decided && !item.DecisionSkipped) {
		return
	}
	for _, r := range item.HeadReports {
		decided, ok := item.Perspectives[r.Perspective]
		if !ok {
			continue
		}
		model := strings.TrimSpace(r.ReviewModel)
		if model == "" {
			model = fallbackModel
		}
		if model == "" {
			model = modelfamily.Unknown
		}
		g := groups[model]
		if g == nil {
			g = &ReviewerHeadAgreementGroup{Name: model}
			groups[model] = g
		}
		g.Samples++
		headBlocks := !r.NotApplicable && r.Verdict != VerdictApprove
		decidedBlocks := decided != VerdictApprove
		switch {
		case headBlocks == decidedBlocks:
			g.Agreements++
		case decidedBlocks:
			g.MissedBlocks++
		default:
			g.OverBlocks++
		}
		g.FindingsRaised += len(r.Findings)
		for _, f := range r.Findings {
			switch dispositionFor(item.Dispositions, r.Perspective, r.Head, f.Title) {
			case DispositionConfirmed:
				g.FindingsConfirmed++
			case DispositionNotReproducible, DispositionOutOfScope:
				g.FindingsRejected++
			}
		}
	}
}

func dispositionFor(ds []FindingDisposition, p Perspective, head, title string) string {
	for _, d := range ds {
		if (d.Perspective == "" || d.Perspective == p) && strings.TrimSpace(d.Head) == strings.TrimSpace(head) && strings.EqualFold(strings.TrimSpace(d.Title), strings.TrimSpace(title)) {
			return strings.TrimSpace(d.Disposition)
		}
	}
	return ""
}

func finishHeadAgreementGroups(m map[string]*ReviewerHeadAgreementGroup) []ReviewerHeadAgreementGroup {
	if len(m) == 0 {
		return nil
	}
	out := make([]ReviewerHeadAgreementGroup, 0, len(m))
	for _, g := range m {
		if g.Samples > 0 {
			g.AgreementRate = float64(g.Agreements) / float64(g.Samples)
		}
		if judged := g.FindingsConfirmed + g.FindingsRejected; judged > 0 {
			g.ConfirmRate = float64(g.FindingsConfirmed) / float64(judged)
		}
		out = append(out, *g)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Name < out[j].Name
	})
	return out
}

type accuracyAccumulator struct {
	ReviewerAccuracyGroup
	conf map[string]*ReviewerConfidenceCalibrationBucket
}

func accuracyAcc(m map[string]*accuracyAccumulator, name string) *accuracyAccumulator {
	acc := m[name]
	if acc == nil {
		acc = &accuracyAccumulator{ReviewerAccuracyGroup: ReviewerAccuracyGroup{Name: name}, conf: map[string]*ReviewerConfidenceCalibrationBucket{}}
		m[name] = acc
	}
	return acc
}

func (a *accuracyAccumulator) add(verdict Verdict, confidence int, outcome *PROutcome, evidence ReviewerAccuracyEvidence) {
	a.Samples++
	bad := evidence.ReworkedAfterApproval
	merged := outcome.Outcome == OutcomeMerged
	switch verdict {
	case VerdictApprove:
		a.Approvals++
		if merged && bad {
			a.FalseApprovals++
		}
	case VerdictChangesRequested:
		a.Blocks++
		if merged && (evidence.MergedUnchangedAfterBlock || evidence.MaintainerOverride) {
			a.FalseBlocks++
		}
	}
	bucket := confidenceBucket(confidence)
	cb := a.conf[bucket]
	if cb == nil {
		cb = &ReviewerConfidenceCalibrationBucket{Bucket: bucket}
		a.conf[bucket] = cb
	}
	cb.Samples++
	if merged {
		cb.Merged++
	}
	if bad {
		cb.BadOutcomes++
	}
}

func finishAccuracyGroups(m map[string]*accuracyAccumulator) []ReviewerAccuracyGroup {
	out := make([]ReviewerAccuracyGroup, 0, len(m))
	for _, acc := range m {
		if acc.Approvals > 0 {
			acc.FalseApproveRate = float64(acc.FalseApprovals) / float64(acc.Approvals)
		}
		if acc.Blocks > 0 {
			acc.FalseBlockRate = float64(acc.FalseBlocks) / float64(acc.Blocks)
		}
		for _, cb := range acc.conf {
			if cb.Samples > 0 {
				cb.MergeRate = float64(cb.Merged) / float64(cb.Samples)
				cb.BadOutcomeRate = float64(cb.BadOutcomes) / float64(cb.Samples)
			}
			acc.ConfidenceBuckets = append(acc.ConfidenceBuckets, *cb)
		}
		sort.Slice(acc.ConfidenceBuckets, func(i, j int) bool {
			return acc.ConfidenceBuckets[i].Bucket < acc.ConfidenceBuckets[j].Bucket
		})
		out = append(out, acc.ReviewerAccuracyGroup)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Samples != out[j].Samples {
			return out[i].Samples > out[j].Samples
		}
		return out[i].Name < out[j].Name
	})
	return out
}

func confidenceBucket(score int) string {
	if score <= ConfidenceRejectScore {
		return "0"
	}
	if score >= ConfidenceMax {
		return "5"
	}
	return string(rune('0' + score))
}
