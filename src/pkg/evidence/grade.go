package evidence

import (
	"fmt"
	"math"
	"strings"
)

// Letter grades for the dashboard's Evidence cards (#13204). Every card gets
// one school-style letter, A+ … C-, or NoGradeLetter when the window holds too
// few samples to say anything. The rubrics below are the single source of
// truth: the dashboard renders what this package returns and never grades on
// its own. Grades describe the data in a window; nothing that decides a merge
// reads them.

// GradeMinN is the smallest denominator a card may be graded on. Below it the
// card reads "—" and says what it is waiting for.
const GradeMinN = 20

// NoGradeLetter is the letter shown when a card has not enough data.
const NoGradeLetter = "—"

// Grade bands colour the badge: a is green, b amber, c red, none grey.
const (
	BandA    = "a"
	BandB    = "b"
	BandC    = "c"
	BandNone = "none"
)

// Grade is one card's letter grade plus the one-liners that make it readable.
type Grade struct {
	Letter string `json:"letter"`
	Band   string `json:"band"`
	// Reason is the one-line "why this letter" for the current numbers.
	Reason string `json:"reason"`
	// EnoughData is false when Letter is NoGradeLetter.
	EnoughData bool `json:"enough_data"`
	// Rubric is the fixed one-line description of how the letter is computed.
	Rubric string `json:"rubric"`
}

// Card is what the dashboard shows above a chart: the grade and one
// plain-English sentence a reader without a statistics background can take in.
type Card struct {
	Grade        Grade  `json:"grade"`
	PlainSummary string `json:"plain_summary"`
}

// PlainSummaryMaxWords caps every PlainSummary; the tests enforce it.
const PlainSummaryMaxWords = 20

// Score cut-offs on a 0–1 scale for the cards graded on a single share. They
// follow the familiar US percentage scale so a reader needs no key.
var scoreLetters = []struct {
	min    float64
	letter string
}{
	{0.97, "A+"}, {0.93, "A"}, {0.90, "A-"},
	{0.87, "B+"}, {0.83, "B"}, {0.80, "B-"},
	{0.77, "C+"}, {0.73, "C"},
}

// letterFromScore maps a 0–1 score to A+ … C-; anything below the lowest cut
// is C-. NaN reads as C-.
func letterFromScore(score float64) string {
	for _, c := range scoreLetters {
		if score >= c.min {
			return c.letter
		}
	}
	return "C-"
}

// BandOf returns the colour band of a letter.
func BandOf(letter string) string {
	switch {
	case letter == "" || letter == NoGradeLetter:
		return BandNone
	case strings.HasPrefix(letter, "A"):
		return BandA
	case strings.HasPrefix(letter, "B"):
		return BandB
	default:
		return BandC
	}
}

// NoGrade is the "—" grade with the reason the card is waiting for.
func NoGrade(reason, rubric string) Grade {
	return Grade{Letter: NoGradeLetter, Band: BandNone, Reason: reason, EnoughData: false, Rubric: rubric}
}

func graded(letter, reason, rubric string) Grade {
	return Grade{Letter: letter, Band: BandOf(letter), Reason: reason, EnoughData: true, Rubric: rubric}
}

func pct(num, den int) string {
	if den <= 0 {
		return "—"
	}
	return fmt.Sprintf("%d%%", int(math.Round(float64(num)/float64(den)*100)))
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ---- Outcome by author kind ----------------------------------------------

// OutcomeStats is one author kind's PR outcomes in the window.
type OutcomeStats struct {
	Kind     string
	N        int
	Merged   int
	Closed   int
	Open     int
	Unknown  int
	Reverted int // reverted within 14 d of merge
}

const outcomeRubric = "score = merged share of PRs with a known outcome × (1 − reverted share of merged); A+ ≥ 97% … C- < 73%; needs 20 PRs with a known outcome"

// GradeOutcome grades PR outcomes across every author kind: how many PRs with
// a known outcome merged, discounted by how many of those were reverted.
func GradeOutcome(stats []OutcomeStats) Grade {
	var known, merged, reverted int
	for _, s := range stats {
		known += s.Merged + s.Closed + s.Open
		merged += s.Merged
		reverted += s.Reverted
	}
	if known < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d PRs with a known outcome (have %d)", GradeMinN, known), outcomeRubric)
	}
	mergeShare := float64(merged) / float64(known)
	revertShare := 0.0
	if merged > 0 {
		revertShare = float64(reverted) / float64(merged)
	}
	score := mergeShare * (1 - revertShare)
	reason := fmt.Sprintf("%s of %d PRs with a known outcome merged; %d of %d merged %s reverted", pct(merged, known), known, reverted, merged, plural(reverted, "was", "were"))
	return graded(letterFromScore(score), reason, outcomeRubric)
}

// SummarizeOutcome is the plain sentence for the Outcome card.
func SummarizeOutcome(stats []OutcomeStats) string {
	byKind := map[string]OutcomeStats{}
	total, unknown := 0, 0
	for _, s := range stats {
		byKind[s.Kind] = s
		total += s.N
		unknown += s.Unknown
	}
	if total == 0 {
		return "No PRs with an evidence record in this window."
	}
	part := func(label, kind string) string {
		s, ok := byKind[kind]
		if !ok || s.N == 0 {
			return ""
		}
		return fmt.Sprintf("%s: %d merged, %d reverted.", label, s.Merged, s.Reverted)
	}
	parts := []string{}
	for _, p := range []string{part("Agents", AuthorAgent), part("Humans", AuthorHuman), part("Bots", AuthorBot)} {
		if p != "" {
			parts = append(parts, p)
		}
	}
	if unknown > 0 {
		parts = append(parts, fmt.Sprintf("%d %s not recorded yet.", unknown, plural(unknown, "outcome", "outcomes")))
	}
	return strings.Join(parts, " ")
}

// ---- Reviewer calibration ------------------------------------------------

// CalibrationBucket is one confidence bucket aggregated over every reviewer
// perspective, ordered lowest confidence first by the caller.
type CalibrationBucket struct {
	Bucket string
	Known  int // verdicts on PRs with a recorded outcome
	Merged int
}

// calibMinBucketN mirrors the chart's dashed-bucket threshold: buckets with
// fewer known-outcome verdicts cannot show a trend and are not checked for
// monotonicity.
const calibMinBucketN = 5

const calibrationRubric = "score = merged share in the top confidence bucket, minus 10 points when merged share does not rise with confidence; A+ ≥ 97% … C- < 73%; needs 20 known-outcome verdicts in the top bucket"

// GradeCalibration grades whether reviewer confidence means anything: the top
// bucket should merge most, and merge rate should rise bucket by bucket.
func GradeCalibration(buckets []CalibrationBucket) Grade {
	if len(buckets) == 0 {
		return NoGrade(fmt.Sprintf("needs %d known-outcome verdicts in the top confidence bucket (have 0)", GradeMinN), calibrationRubric)
	}
	top := buckets[len(buckets)-1]
	if top.Known < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d known-outcome verdicts in the %s bucket (have %d)", GradeMinN, top.Bucket, top.Known), calibrationRubric)
	}
	score := float64(top.Merged) / float64(top.Known)
	monotonic := true
	prev := -1.0
	for _, b := range buckets {
		if b.Known < calibMinBucketN {
			continue
		}
		rate := float64(b.Merged) / float64(b.Known)
		if rate < prev {
			monotonic = false
		}
		prev = rate
	}
	reason := fmt.Sprintf("%s of %d verdicts at %s confidence merged", pct(top.Merged, top.Known), top.Known, top.Bucket)
	if !monotonic {
		score -= 0.10
		reason += "; merged share does not rise with confidence"
	}
	return graded(letterFromScore(score), reason, calibrationRubric)
}

// SummarizeCalibration is the plain sentence for the Calibration card.
func SummarizeCalibration(buckets []CalibrationBucket) string {
	var known int
	for _, b := range buckets {
		known += b.Known
	}
	if len(buckets) == 0 || known == 0 {
		return "No reviewer verdicts with a known PR outcome in this window yet."
	}
	top := buckets[len(buckets)-1]
	if top.Known == 0 {
		return fmt.Sprintf("No high-confidence verdicts yet; %d lower-confidence %s have a known outcome.", known, plural(known, "verdict", "verdicts"))
	}
	return fmt.Sprintf("When reviewers were most sure (%s), %s of %d PRs merged.", top.Bucket, pct(top.Merged, top.Known), top.Known)
}

// ---- Reviewer accuracy × model family -----------------------------------

// AccuracyStats carries the rework-based reviewer accuracy (pkg/review) when
// the outcome ledger is available, plus the raw verdict count from bundles
// for the sentence shown while it is not.
type AccuracyStats struct {
	Available        bool
	Samples          int
	FalseApproveRate float64
	FalseBlockRate   float64
	BundleVerdicts   int
}

const accuracyRubric = "score = 1 − worse of rework-based false-approve and false-block rate; A+ ≥ 97% … C- < 73%; needs the reviewer outcome ledger and 20 samples"

// GradeAccuracy grades the reviewers on the rework-based ledger only; bundle
// outcomes alone cannot tell a wrong approval from a PR merged elsewhere.
func GradeAccuracy(a AccuracyStats) Grade {
	if !a.Available {
		return NoGrade("needs the reviewer outcome ledger (rework-based accuracy)", accuracyRubric)
	}
	if a.Samples < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d reviewed PRs with a recorded outcome (have %d)", GradeMinN, a.Samples), accuracyRubric)
	}
	worst := math.Max(a.FalseApproveRate, a.FalseBlockRate)
	reason := fmt.Sprintf("false approve %d%%, false block %d%% over %d PRs", int(math.Round(a.FalseApproveRate*100)), int(math.Round(a.FalseBlockRate*100)), a.Samples)
	return graded(letterFromScore(1-worst), reason, accuracyRubric)
}

// SummarizeAccuracy is the plain sentence for the Accuracy card.
func SummarizeAccuracy(a AccuracyStats) string {
	if a.Available && a.Samples > 0 {
		return fmt.Sprintf("Reviewers wrongly approved %d%% and wrongly blocked %d%% of %d PRs with a known result.", int(math.Round(a.FalseApproveRate*100)), int(math.Round(a.FalseBlockRate*100)), a.Samples)
	}
	if a.BundleVerdicts == 0 {
		return "No reviewer verdicts in this window yet."
	}
	return fmt.Sprintf("%d reviewer %s recorded; accuracy is graded once the outcome ledger covers them.", a.BundleVerdicts, plural(a.BundleVerdicts, "verdict", "verdicts"))
}

// ---- Maintainability trend ----------------------------------------------

// ComplexityTrend is one repo's first and latest monthly snapshot in the
// window; the caller only passes repos with at least two snapshots.
type ComplexityTrend struct {
	Repo        string
	FirstMedian int
	LastMedian  int
	FirstOver   int // functions over the cyclomatic threshold
	LastOver    int
}

const maintainabilityRubric = "median cyclomatic complexity and functions over threshold, first vs latest snapshot: both down A+, flat A, one rising B, both rising C; needs a repo with two monthly snapshots"

// GradeMaintainability grades the direction of Go complexity, not its level.
func GradeMaintainability(trends []ComplexityTrend) Grade {
	if len(trends) == 0 {
		return NoGrade("needs a repo with two monthly complexity snapshots in the window", maintainabilityRubric)
	}
	var dMedian, dOver int
	for _, t := range trends {
		dMedian += t.LastMedian - t.FirstMedian
		dOver += t.LastOver - t.FirstOver
	}
	dir := func(d int) string {
		switch {
		case d < 0:
			return "down"
		case d > 0:
			return "up"
		}
		return "flat"
	}
	worse, better := 0, 0
	for _, d := range []int{dMedian, dOver} {
		if d > 0 {
			worse++
		} else if d < 0 {
			better++
		}
	}
	var letter string
	switch worse {
	case 0:
		letter = []string{"A-", "A", "A+"}[better]
	case 1:
		letter = "B"
		if better == 1 {
			letter = "B+"
		}
	default:
		letter = "C"
	}
	reason := fmt.Sprintf("median complexity %s (%+d), functions over threshold %s (%+d) across %d %s", dir(dMedian), dMedian, dir(dOver), dOver, len(trends), plural(len(trends), "repo", "repos"))
	return graded(letter, reason, maintainabilityRubric)
}

// SummarizeMaintainability is the plain sentence for the Maintainability card.
// reposWithSnapshots counts every repo with at least one snapshot.
func SummarizeMaintainability(trends []ComplexityTrend, reposWithSnapshots int) string {
	if reposWithSnapshots == 0 {
		return "No monthly code-complexity snapshots in this window."
	}
	if len(trends) == 0 {
		return fmt.Sprintf("%d %s one complexity snapshot; a trend needs two months.", reposWithSnapshots, plural(reposWithSnapshots, "repo has", "repos have"))
	}
	var firstOver, lastOver, dMedian int
	for _, t := range trends {
		firstOver += t.FirstOver
		lastOver += t.LastOver
		dMedian += t.LastMedian - t.FirstMedian
	}
	median := "flat"
	if dMedian > 0 {
		median = "rising"
	} else if dMedian < 0 {
		median = "falling"
	}
	return fmt.Sprintf("Code complexity across %d %s: median %s, %d complex %s (was %d).", len(trends), plural(len(trends), "repo", "repos"), median, lastOver, plural(lastOver, "function", "functions"), firstOver)
}

// ---- Boundary enforcement -----------------------------------------------

// BoundaryStats is every agent GitHub write the hive saw in the window.
type BoundaryStats struct {
	TotalWrites  int64
	Blocked      int64
	SentinelHits int
}

const boundaryRubric = "blocked share of all agent GitHub writes: 0 < share ≤ 1% A+, ≤ 2% A, ≤ 5% B, ≤ 10% C+, more C-; no refusals at all A- (boundary not yet exercised); needs 20 writes"

// GradeBoundary grades how often the proxy had to refuse an agent write. A
// few refusals in thousands of writes is the boundary working quietly.
func GradeBoundary(b BoundaryStats) Grade {
	if b.TotalWrites < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d agent writes (have %d)", GradeMinN, b.TotalWrites), boundaryRubric)
	}
	share := float64(b.Blocked) / float64(b.TotalWrites)
	reason := fmt.Sprintf("%d of %d agent writes refused (%.2f%%)", b.Blocked, b.TotalWrites, share*100)
	var letter string
	switch {
	case b.Blocked == 0:
		letter = "A-"
		reason = fmt.Sprintf("no refusals in %d agent writes; the boundary has not been exercised yet", b.TotalWrites)
	case share <= 0.01:
		letter = "A+"
	case share <= 0.02:
		letter = "A"
	case share <= 0.05:
		letter = "B"
	case share <= 0.10:
		letter = "C+"
	default:
		letter = "C-"
	}
	return graded(letter, reason, boundaryRubric)
}

// SummarizeBoundary is the plain sentence for the Boundary card.
func SummarizeBoundary(b BoundaryStats) string {
	if b.TotalWrites == 0 {
		return "No agent writes to GitHub recorded in this window."
	}
	sentinel := "No sentinel hits."
	if b.SentinelHits > 0 {
		sentinel = fmt.Sprintf("%d sentinel %s.", b.SentinelHits, plural(b.SentinelHits, "hit", "hits"))
	}
	return fmt.Sprintf("%d agent writes to GitHub; %d %s refused by the proxy (%s). %s", b.TotalWrites, b.Blocked, plural(int(b.Blocked), "was", "were"), pctShare(b.Blocked, b.TotalWrites), sentinel)
}

func pctShare(num, den int64) string {
	if den <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f%%", float64(num)/float64(den)*100)
}

// ---- CI gate ------------------------------------------------------------

// CIGateStats is the merge-gate integrity read plus the convergence numbers
// used only for the sentence.
type CIGateStats struct {
	MergedN      int
	WithChecks   int // merged PRs with a known merge-time check state
	Breaches     int
	AgentSettled int
	MedianPasses *float64
}

const ciGateRubric = "confirmed breaches (merged with a required check red or missing) ÷ merged PRs with a known check state: 0 breaches A+, ≤ 1% C+, ≤ 3% C, more C-; needs 20 merged PRs with a known state"

// GradeCIGate grades gate integrity. A single merge past a red required check
// is a policy breach, so anything above zero sits in the C band.
func GradeCIGate(c CIGateStats) Grade {
	if c.WithChecks < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d merged PRs with a known merge-time check state (have %d)", GradeMinN, c.WithChecks), ciGateRubric)
	}
	share := float64(c.Breaches) / float64(c.WithChecks)
	var letter string
	switch {
	case c.Breaches == 0:
		letter = "A+"
	case share <= 0.01:
		letter = "C+"
	case share <= 0.03:
		letter = "C"
	default:
		letter = "C-"
	}
	reason := fmt.Sprintf("%d confirmed %s in %d merged PRs with a known check state", c.Breaches, plural(c.Breaches, "breach", "breaches"), c.WithChecks)
	return graded(letter, reason, ciGateRubric)
}

// SummarizeCIGate is the plain sentence for the CI gate card.
func SummarizeCIGate(c CIGateStats) string {
	if c.MergedN == 0 {
		return "No PRs merged in this window."
	}
	if c.WithChecks == 0 {
		return fmt.Sprintf("%d PRs merged; none has a known check state at merge yet.", c.MergedN)
	}
	var head string
	if c.Breaches == 0 {
		head = fmt.Sprintf("All %d merged PRs with a known state had green required checks.", c.WithChecks)
	} else {
		head = fmt.Sprintf("%d of %d merged PRs had a red or missing required check.", c.Breaches, c.WithChecks)
	}
	if c.MedianPasses != nil && c.AgentSettled > 0 {
		return fmt.Sprintf("%s Agent PRs needed a median %s CI %s.", head, trimFloat(*c.MedianPasses), plural(int(math.Round(*c.MedianPasses)), "pass", "passes"))
	}
	return head
}

func trimFloat(f float64) string {
	s := fmt.Sprintf("%.1f", f)
	return strings.TrimSuffix(s, ".0")
}

// ---- Work balance -------------------------------------------------------

// WorkBalanceStats sums the window's issue and PR flow.
type WorkBalanceStats struct {
	IssuesOpened int
	IssuesClosed int
	PRsOpened    int
	PRsMerged    int
	QueueIssues  *int // open issues at the end of the last week, when sampled
	QueuePRs     *int
}

const workBalanceRubric = "issues opened ÷ issues closed: ≤ 0.8 A+, ≤ 0.9 A, ≤ 1.0 A-, ≤ 1.15 B+, ≤ 1.3 B, ≤ 1.5 B-, ≤ 1.75 C+, ≤ 2.0 C, more C-; needs 20 issues opened"

// GradeWorkBalance grades whether the queue keeps up: opened against closed.
func GradeWorkBalance(w WorkBalanceStats) Grade {
	if w.IssuesOpened < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d issues opened in the window (have %d)", GradeMinN, w.IssuesOpened), workBalanceRubric)
	}
	ratio := math.Inf(1)
	if w.IssuesClosed > 0 {
		ratio = float64(w.IssuesOpened) / float64(w.IssuesClosed)
	}
	cuts := []struct {
		max    float64
		letter string
	}{
		{0.8, "A+"}, {0.9, "A"}, {1.0, "A-"}, {1.15, "B+"}, {1.3, "B"}, {1.5, "B-"}, {1.75, "C+"}, {2.0, "C"},
	}
	letter := "C-"
	for _, c := range cuts {
		if ratio <= c.max {
			letter = c.letter
			break
		}
	}
	ratioText := "∞"
	if !math.IsInf(ratio, 1) {
		ratioText = fmt.Sprintf("%.2f", ratio)
	}
	reason := fmt.Sprintf("%d issues opened for %d closed (%s opened per closed)", w.IssuesOpened, w.IssuesClosed, ratioText)
	return graded(letter, reason, workBalanceRubric)
}

// SummarizeWorkBalance is the plain sentence for the Work balance card.
func SummarizeWorkBalance(w WorkBalanceStats) string {
	if w.IssuesOpened+w.IssuesClosed+w.PRsOpened+w.PRsMerged == 0 {
		return "No issues or PRs opened, closed or merged in this window."
	}
	trend := ""
	switch {
	case w.IssuesOpened > 0 && w.IssuesClosed > w.IssuesOpened:
		trend = " The issue queue is shrinking."
	case w.IssuesOpened > 0 && w.IssuesClosed < w.IssuesOpened:
		trend = " The issue queue is growing."
	case w.IssuesOpened > 0:
		trend = " The issue queue is holding steady."
	}
	return fmt.Sprintf("%d issues opened, %d closed. %d PRs opened, %d merged.%s", w.IssuesOpened, w.IssuesClosed, w.PRsOpened, w.PRsMerged, trend)
}

// ---- Shadow gates -------------------------------------------------------

// ShadowStats sums shadow-mode observations across the review and intent gates.
type ShadowStats struct {
	N              int // PRs with a shadow observation
	Held           int // PRs a gate would have held or blocked
	HeldMerged     int
	HeldReverted   int
	PassedReverted int
}

const shadowRubric = "share of observed PRs a shadow gate would have held: ≤ 5% A+, ≤ 10% A, ≤ 15% A-, ≤ 20% B+, ≤ 30% B, ≤ 40% B-, ≤ 50% C+, ≤ 65% C, more C-; needs 20 observed PRs"

// GradeShadow grades how noisy the gates would be if switched on: a gate that
// would stop most PRs is not ready to enforce.
func GradeShadow(s ShadowStats) Grade {
	if s.N < GradeMinN {
		return NoGrade(fmt.Sprintf("needs %d PRs with a shadow observation (have %d)", GradeMinN, s.N), shadowRubric)
	}
	share := float64(s.Held) / float64(s.N)
	cuts := []struct {
		max    float64
		letter string
	}{
		{0.05, "A+"}, {0.10, "A"}, {0.15, "A-"}, {0.20, "B+"}, {0.30, "B"}, {0.40, "B-"}, {0.50, "C+"}, {0.65, "C"},
	}
	letter := "C-"
	for _, c := range cuts {
		if share <= c.max {
			letter = c.letter
			break
		}
	}
	reason := fmt.Sprintf("would have held %d of %d observed PRs (%s)", s.Held, s.N, pct(s.Held, s.N))
	return graded(letter, reason, shadowRubric)
}

// SummarizeShadow is the plain sentence for the Shadow gates card.
func SummarizeShadow(s ShadowStats) string {
	if s.N == 0 {
		return "No shadow-gate observations in this window; nothing was held or blocked."
	}
	return fmt.Sprintf("The shadow gates would have held %d of %d PRs; nothing was actually blocked.", s.Held, s.N)
}
