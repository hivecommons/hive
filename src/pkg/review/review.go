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
	"sort"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/outputschema"
)

const (
	PerspectiveCorrectness     Perspective = "correctness"
	PerspectiveSecurity        Perspective = "security"
	PerspectiveIntentAlignment Perspective = "intent-alignment"
	PerspectiveStyle           Perspective = "style"
	PerspectiveDocsCurrency    Perspective = "docs-currency"
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
	Perspective Perspective `json:"perspective"`
	Verdict     Verdict     `json:"verdict"`
	Repo        string      `json:"repo"`
	Number      int         `json:"number"`
	HeadSHA     string      `json:"head_sha,omitempty"`
}

type AggregateOptions struct {
	HumanThreshold outputschema.Severity
	FixAttempts    int
	MaxFixAttempts int
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
}

type Aggregate struct {
	Repo             string                  `json:"repo"`
	Number           int                     `json:"number"`
	HeadSHA          string                  `json:"head_sha,omitempty"`
	Verdict          Verdict                 `json:"verdict"`
	MergeEligible    bool                    `json:"merge_eligible"`
	FixCycle         bool                    `json:"fix_cycle"`
	RequiresHuman    bool                    `json:"requires_human"`
	CloseRecommended bool                    `json:"close_recommended"`
	FixAttempts      int                     `json:"fix_attempts,omitempty"`
	MaxFixAttempts   int                     `json:"max_fix_attempts,omitempty"`
	Threshold        outputschema.Severity   `json:"human_threshold"`
	Reasons          []string                `json:"reasons,omitempty"`
	Findings         []PerspectiveFinding    `json:"findings,omitempty"`
	Perspectives     map[Perspective]Verdict `json:"perspectives"`
	// RecordedAt is when this verdict was last merged into the durable
	// artifact. It exists so stale entries (PRs long since merged or closed)
	// can be pruned instead of accumulating forever.
	RecordedAt time.Time `json:"recorded_at,omitempty"`
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
	return &report, nil
}

func AggregateReports(reports []PerspectiveReport, opts AggregateOptions) Aggregate {
	threshold := opts.HumanThreshold
	if threshold == "" {
		threshold = DefaultHumanThreshold
	}
	maxFix := opts.MaxFixAttempts
	if maxFix <= 0 {
		maxFix = DefaultFixCycleCap
	}
	agg := Aggregate{
		Threshold:      threshold,
		FixAttempts:    opts.FixAttempts,
		MaxFixAttempts: maxFix,
		Perspectives:   map[Perspective]Verdict{},
	}
	if len(reports) == 0 {
		agg.Verdict = VerdictRequiresHuman
		agg.RequiresHuman = true
		agg.Reasons = []string{"no review reports were collected"}
		return agg
	}
	seenVerdicts := map[Verdict]bool{}
	for _, r := range reports {
		if agg.Repo == "" {
			agg.Repo, agg.Number, agg.HeadSHA = r.Repo, r.Number, r.HeadSHA
		}
		agg.Perspectives[r.Perspective] = r.Verdict
		seenVerdicts[r.Verdict] = true
		for _, f := range r.Findings {
			agg.Findings = append(agg.Findings, PerspectiveFinding{Perspective: r.Perspective, Finding: f})
			if severityAtLeast(f.Severity, threshold) {
				agg.Reasons = append(agg.Reasons, fmt.Sprintf("%s finding %q is %s (threshold %s)", r.Perspective, f.Title, f.Severity, threshold))
			}
		}
	}
	sortReasons(agg.Reasons)

	if seenVerdicts[VerdictReject] {
		agg.Verdict = VerdictReject
		agg.CloseRecommended = true
		return agg
	}
	if len(agg.Reasons) > 0 || seenVerdicts[VerdictRequiresHuman] {
		agg.Verdict = VerdictRequiresHuman
		agg.RequiresHuman = true
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
		return agg
	}
	if len(seenVerdicts) == 1 && seenVerdicts[VerdictApprove] && hasAllRequiredPerspectives(agg.Perspectives, opts.MaxPerspectivesPerPR, opts.Perspectives.Len()) {
		agg.Verdict = VerdictApprove
		agg.MergeEligible = true
		return agg
	}
	agg.Verdict = VerdictRequiresHuman
	agg.RequiresHuman = true
	agg.Reasons = []string{"review perspectives did not unanimously approve"}
	return agg
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
		report, err := ValidateReport(raw)
		if err != nil {
			return Artifact{}, fmt.Errorf("%s: %w", name, err)
		}
		groups[reviewKey(report.Repo, report.Number, report.HeadSHA)] = append(groups[reviewKey(report.Repo, report.Number, report.HeadSHA)], *report)
	}
	artifact := Artifact{GeneratedAt: time.Now().UTC()}
	for _, key := range sortedKeys(groups) {
		artifact.Items = append(artifact.Items, AggregateReports(groups[key], opts))
	}
	return artifact, nil
}

func WriteArtifact(path string, artifact Artifact) error {
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
	return artifact, WriteArtifact(path, artifact)
}

// VerdictRetention bounds how long a verdict stays in the durable artifact
// after it was last seen. Long enough to outlive any realistic review cycle,
// short enough that the file does not grow without bound as PRs close.
const VerdictRetention = 30 * 24 * time.Hour

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
// repo/number/head-SHA, previously recorded ones survive, and anything not
// re-confirmed within VerdictRetention is dropped.
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
			key := reviewKey(item.Repo, item.Number, item.HeadSHA)
			if _, seen := merged[key]; !seen {
				order = append(order, key)
			}
			merged[key] = item
		}
	}

	for _, item := range fresh.Items {
		item.RecordedAt = now
		key := reviewKey(item.Repo, item.Number, item.HeadSHA)
		if _, seen := merged[key]; !seen {
			order = append(order, key)
		}
		merged[key] = item
	}

	artifact := Artifact{GeneratedAt: now, Items: make([]Aggregate, 0, len(order))}
	for _, key := range order {
		artifact.Items = append(artifact.Items, merged[key])
	}
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
	return artifact, nil
}

func (a Artifact) HasAggregateApproval(repo string, number int, headSHA string) bool {
	wantRepo := strings.TrimSpace(repo)
	wantSHA := strings.TrimSpace(headSHA)
	for _, item := range a.Items {
		if item.Number != number || strings.TrimSpace(item.Repo) != wantRepo || !item.MergeEligible || item.Verdict != VerdictApprove {
			continue
		}
		if strings.TrimSpace(item.HeadSHA) == wantSHA {
			return true
		}
	}
	return false
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
	required := configured
	if required <= 0 {
		required = len(DefaultPerspectives)
	}
	if maxPerPR > 0 && maxPerPR < required {
		required = maxPerPR
	}
	if len(got) < required {
		return false
	}
	for _, v := range got {
		if v != VerdictApprove {
			return false
		}
	}
	return true
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

func reviewKey(repo string, number int, headSHA string) string {
	return fmt.Sprintf("%s#%d@%s", repo, number, headSHA)
}
