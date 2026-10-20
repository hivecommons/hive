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
	for _, item := range artifact.Items {
		if item.RecordedAt.IsZero() || item.RecordedAt.Before(since) {
			continue
		}
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
	return sum
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
