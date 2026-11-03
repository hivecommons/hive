package review

import (
	"math"
	"path"
	"strings"
)

// SnapConfidence maps a free self-report to the nearest fixed bin. Midpoints
// go to the higher bin; values outside the rubric clamp to its end bins.
func SnapConfidence(raw float64) float64 {
	switch {
	case raw < 0.60:
		return 0.55
	case raw < 0.70:
		return 0.65
	case raw < 0.80:
		return 0.75
	case raw < 0.90:
		return 0.85
	default:
		return 0.95
	}
}

func normalizeReportedConfidence(r *PerspectiveReport) {
	if r.Confidence == nil {
		r.ConfidenceRaw = nil
		return
	}
	raw := *r.Confidence
	// Preserve the original value when a normalized report is read again.
	if r.ConfidenceRaw != nil && SnapConfidence(*r.ConfidenceRaw) == raw {
		raw = *r.ConfidenceRaw
	}
	binned := SnapConfidence(raw)
	r.ConfidenceRaw, r.Confidence = &raw, &binned
}

// Every additive weight is named. This is an evidence-strength heuristic,
// not a probability of correctness and never an input to ScoreConfidence.
const (
	DerivedConfidenceBase   = 0.55
	DerivedAnchorWeight     = 0.10
	DerivedTestsWeight      = 0.10
	DerivedSmallDiffWeight  = 0.10
	DerivedMediumDiffWeight = 0.05
	DerivedLargeDiffWeight  = -0.05
	DerivedCISuccessWeight  = 0.05
	DerivedCIFailureWeight  = -0.05
	DerivedAgreementWeight  = 0.05
	DerivedSmallDiffLines   = 100
	DerivedMediumDiffLines  = 500
	DerivedConfidenceMin    = 0.55
	DerivedConfidenceMax    = 0.95
)

// ConfidenceSignals are observable inputs captured at verdict time. Unknown
// diff or CI data contributes nothing, rather than pretending to be green.
// Peers must describe the same repo, PR and head as the report; the function
// also checks that binding and counts each other perspective only once.
type ConfidenceSignals struct {
	DiffKnown    bool
	ChangedLines int
	TouchesTests bool
	CIState      string
	Peers        []PerspectiveReport
}

func DerivedConfidence(r PerspectiveReport, s ConfidenceSignals) float64 {
	score := DerivedConfidenceBase
	if len(r.Findings) > 0 {
		anchored := 0
		for _, f := range r.Findings {
			if strings.TrimSpace(f.File) != "" && f.File != "(pr)" && f.Line > 0 {
				anchored++
			}
		}
		score += DerivedAnchorWeight * float64(anchored) / float64(len(r.Findings))
	}
	if s.DiffKnown {
		if s.TouchesTests {
			score += DerivedTestsWeight
		}
		switch {
		case s.ChangedLines <= DerivedSmallDiffLines:
			score += DerivedSmallDiffWeight
		case s.ChangedLines <= DerivedMediumDiffLines:
			score += DerivedMediumDiffWeight
		default:
			score += DerivedLargeDiffWeight
		}
	}
	switch s.CIState {
	case "success":
		score += DerivedCISuccessWeight
	case "failure":
		score += DerivedCIFailureWeight
	}
	peers := map[Perspective]Verdict{}
	for _, p := range s.Peers {
		if p.HeadSHA == "" || p.HeadSHA != r.HeadSHA || p.Repo != r.Repo || p.Number != r.Number || p.Perspective == r.Perspective || p.NotApplicable {
			continue
		}
		peers[p.Perspective] = p.Verdict
	}
	if len(peers) > 0 {
		agree := 0
		for _, v := range peers {
			if v == r.Verdict {
				agree++
			}
		}
		// Unanimous agreement adds the weight; unanimous disagreement subtracts it.
		score += DerivedAgreementWeight * (2*float64(agree)/float64(len(peers)) - 1)
	}
	return math.Max(DerivedConfidenceMin, math.Min(DerivedConfidenceMax, score))
}

// ConfidenceTestPath recognizes conventional test files/directories across
// languages. It indicates tests were touched, not that they ran or passed.
func ConfidenceTestPath(name string) bool {
	name = strings.ToLower(strings.ReplaceAll(name, "\\", "/"))
	for _, part := range strings.Split(path.Dir(name), "/") {
		if part == "test" || part == "tests" || part == "__tests__" {
			return true
		}
	}
	base := path.Base(name)
	return strings.HasSuffix(base, "_test.go") || strings.HasPrefix(base, "test_") || strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || strings.HasSuffix(base, "_test.py")
}
