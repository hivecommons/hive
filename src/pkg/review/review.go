// Package review models Hive's structured multi-perspective PR review verdicts.
package review

import (
	"bytes"
	"encoding/json"
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
)

var ReviewVerdictsPath = filepath.Join(outputschema.AgentReportDir, ReviewVerdictsFile)

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
}

type PerspectiveFinding struct {
	Perspective Perspective          `json:"perspective"`
	Finding     outputschema.Finding `json:"finding"`
}

type Artifact struct {
	GeneratedAt time.Time   `json:"generated_at"`
	Items       []Aggregate `json:"items"`
}

func ValidateReport(raw []byte) (*PerspectiveReport, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, fmt.Errorf("review report must be a non-empty JSON object")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
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
	if !validPerspective(report.Perspective) {
		return nil, fmt.Errorf("perspective must be one of %s", joinPerspectives(DefaultPerspectives))
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
	if len(seenVerdicts) == 1 && seenVerdicts[VerdictApprove] && hasAllDefaultPerspectives(agg.Perspectives) {
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
	if dir == "" {
		dir = outputschema.AgentReportDir
	}
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

func LoadArtifact(path string) (Artifact, error) {
	if path == "" {
		path = ReviewVerdictsPath
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return Artifact{}, err
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

func validPerspective(p Perspective) bool {
	for _, want := range DefaultPerspectives {
		if p == want {
			return true
		}
	}
	return false
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

func hasAllDefaultPerspectives(got map[Perspective]Verdict) bool {
	if len(got) != len(DefaultPerspectives) {
		return false
	}
	for _, p := range DefaultPerspectives {
		if got[p] != VerdictApprove {
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
