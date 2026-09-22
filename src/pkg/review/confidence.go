package review

import (
	"fmt"
	"strings"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// Confidence is a 0–5 mergeability score derived from a PR's review verdicts
// (hivecommons/hive#8182). It exists so a maintainer triaging a queue can
// tell at a glance how safe a change is to merge without reading every
// finding, and so downstream tooling can sort or gate on one number.
//
// It is COMPUTED, not asked for. A model asked to rate its own confidence
// produces a number with no stable meaning across PRs, models, or days. This
// score is a pure function of what the perspectives actually reported —
// their verdicts, the severity of their findings, and whether every
// configured perspective weighed in — so two PRs with the same review land
// on the same score and a reader can reconstruct why from the reasons.
type Confidence struct {
	Score int `json:"score"`
	// Reasons names each deduction or cap that moved the score off the
	// maximum, worst first. Empty means a clean, fully-covered, unanimous
	// approval.
	Reasons []string `json:"reasons,omitempty"`
}

const (
	// ConfidenceMax is a clean, fully covered, unanimously approved PR.
	ConfidenceMax = 5
	// ConfidenceSafeFloor is the lowest score still read as "safe to merge";
	// anything below it needs attention (4–5 safe, 1–3 needs attention, 0
	// do not merge — the bands the issue borrows from other review tools).
	ConfidenceSafeFloor = 4
	// ConfidenceNeedsAttentionCap is the ceiling once any perspective withheld
	// approval or could not judge: a PR that a reviewer would not approve is
	// never "safe", however few findings it has.
	ConfidenceNeedsAttentionCap = 3
	// ConfidenceCoverageCap is the ceiling when a configured perspective did
	// not report. An unreviewed dimension is unknown risk, not zero risk.
	ConfidenceCoverageCap = 3
	// ConfidenceRejectScore is any reject verdict.
	ConfidenceRejectScore = 0

	confidenceDeductCritical = ConfidenceMax
	confidenceDeductHigh     = 2
	confidenceDeductMedium   = 1
)

// ConfidenceBand is the human-readable band for a score.
func ConfidenceBand(score int) string {
	switch {
	case score <= ConfidenceRejectScore:
		return "do not merge"
	case score >= ConfidenceSafeFloor:
		return "safe"
	default:
		return "needs attention"
	}
}

// ScoreConfidence derives the score from the per-perspective reports.
// expected is the number of perspectives this hive reviews with; 0 means "do
// not judge coverage" (for a single-perspective hive or a caller that has
// no configured set to compare against).
func ScoreConfidence(reports []PerspectiveReport, expected int) Confidence {
	c := Confidence{Score: ConfidenceMax}
	if len(reports) == 0 {
		c.Score = ConfidenceRejectScore
		c.Reasons = []string{"no review reports"}
		return c
	}

	seen := map[Perspective]bool{}
	verdicts := map[Verdict]int{}
	sevCounts := map[outputschema.Severity]int{}
	for _, r := range reports {
		seen[r.Perspective] = true
		verdicts[r.Verdict]++
		for _, f := range r.Findings {
			sevCounts[f.Severity]++
		}
	}

	deduct := func(n int, reason string) {
		if n <= 0 {
			return
		}
		c.Score -= n
		c.Reasons = append(c.Reasons, reason)
	}
	capAt := func(limit int, reason string) {
		if c.Score > limit {
			c.Score = limit
			c.Reasons = append(c.Reasons, reason)
		}
	}

	if n := sevCounts[outputschema.SeverityCritical]; n > 0 {
		deduct(confidenceDeductCritical*n, plural(n, "critical finding"))
	}
	if n := sevCounts[outputschema.SeverityHigh]; n > 0 {
		deduct(confidenceDeductHigh*n, plural(n, "high finding"))
	}
	if n := sevCounts[outputschema.SeverityMedium]; n > 0 {
		deduct(confidenceDeductMedium*n, plural(n, "medium finding"))
	}

	if verdicts[VerdictReject] > 0 {
		capAt(ConfidenceRejectScore, "reject verdict")
	}
	if n := verdicts[VerdictRequiresHuman]; n > 0 {
		capAt(ConfidenceNeedsAttentionCap, plural(n, "perspective")+" requires a human decision")
	}
	if n := verdicts[VerdictChangesRequested]; n > 0 {
		capAt(ConfidenceNeedsAttentionCap, plural(n, "perspective")+" requested changes")
	}
	if expected > 0 && len(seen) < expected {
		capAt(ConfidenceCoverageCap, fmt.Sprintf("%d of %d perspectives reported", len(seen), expected))
	}
	if c.Score < ConfidenceRejectScore {
		c.Score = ConfidenceRejectScore
	}
	return c
}

// Render is the one-line form appended to a review comment:
//
//	**Confidence: 4/5** (safe) — 1 medium finding
func (c Confidence) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "**Confidence: %d/%d** (%s)", c.Score, ConfidenceMax, ConfidenceBand(c.Score))
	if len(c.Reasons) > 0 {
		b.WriteString(" — ")
		b.WriteString(strings.Join(c.Reasons, "; "))
	}
	return b.String()
}

func plural(n int, noun string) string {
	if n == 1 {
		return fmt.Sprintf("1 %s", noun)
	}
	return fmt.Sprintf("%d %ss", n, noun)
}
