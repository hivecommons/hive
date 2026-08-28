package outputschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"unicode"
)

const (
	// MaxValidationRetries bounds corrective re-prompt attempts after invalid output.
	MaxValidationRetries = 3

	defaultAgentReportDir = "/var/run/hive-metrics"

	AgentReportFilePrefix = "agent-report-"
	AgentReportFileSuffix = ".json"

	MaxLaneLength         = 64
	MaxKindLength         = 64
	MaxSummaryLength      = 2000
	MaxFindings           = 100
	MaxPRsOpened          = 50
	MaxBeadsFiled         = 50
	MaxArtifacts          = 100
	MaxTitleLength        = 200
	MaxFindingBodyLength  = 2000
	MaxRepoLength         = 200
	MaxURLLength          = 500
	MaxBeadIDLength       = 120
	MaxArtifactPathLength = 500
)

// AgentReportDir is where per-agent structured report files are read from and
// written to. Production leaves it at defaultAgentReportDir; a var (not
// const) only so governor and outputschema tests can point it at a temp dir,
// matching the knowledgeBaseDir pattern in pkg/knowledge/api.go.
var AgentReportDir = defaultAgentReportDir

type ReportKind string

const (
	KindFindings   ReportKind = "findings"
	KindFix        ReportKind = "fix"
	KindReview     ReportKind = "review"
	KindAdvisory   ReportKind = "advisory"
	KindSummary    ReportKind = "summary"
	KindInstrument ReportKind = "instrument"
)

type Severity string

const (
	SeverityInfo     Severity = "info"
	SeverityLow      Severity = "low"
	SeverityMedium   Severity = "medium"
	SeverityHigh     Severity = "high"
	SeverityCritical Severity = "critical"
)

type BeadType string

const (
	BeadTypeTask     BeadType = "task"
	BeadTypeBug      BeadType = "bug"
	BeadTypeAdvisory BeadType = "advisory"
	BeadTypeFeature  BeadType = "feature"
)

type AgentReport struct {
	Lane       string      `json:"lane"`
	Kind       ReportKind  `json:"kind"`
	Findings   []Finding   `json:"findings"`
	PRsOpened  []PROpened  `json:"prs_opened"`
	BeadsFiled []BeadFiled `json:"beads_filed"`
	Artifacts  []Artifact  `json:"artifacts,omitempty"`
	Summary    string      `json:"summary"`
}

// Artifact records a repository file produced or materially updated by an agent.
// It lets artifact-producing lanes report their primary output independently of
// the pull request that happens to carry it.
type Artifact struct {
	Repo        string `json:"repo"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

type Finding struct {
	Title    string   `json:"title"`
	Severity Severity `json:"severity"`
	Summary  string   `json:"summary"`
	File     string   `json:"file,omitempty"`
	Line     int      `json:"line,omitempty"`
}

type PROpened struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Title  string `json:"title"`
}

type BeadFiled struct {
	ID    string   `json:"id"`
	Type  BeadType `json:"type"`
	Title string   `json:"title"`
	URL   string   `json:"url,omitempty"`
}

type Violation struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

type ValidationError struct {
	Violations []Violation `json:"violations"`
}

func (e *ValidationError) Error() string {
	if e == nil || len(e.Violations) == 0 {
		return "agent report validation failed"
	}
	parts := make([]string, 0, len(e.Violations))
	for _, v := range e.Violations {
		parts = append(parts, fmt.Sprintf("%s: %s", v.Field, v.Message))
	}
	return "agent report validation failed: " + strings.Join(parts, "; ")
}

func Validate(raw []byte) (*AgentReport, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, &ValidationError{Violations: []Violation{{Field: "$", Message: "must be a non-empty JSON object"}}}
	}

	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var report AgentReport
	if err := dec.Decode(&report); err != nil {
		return nil, fmt.Errorf("decode agent report: %w", err)
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return nil, &ValidationError{Violations: []Violation{{Field: "$", Message: "must contain exactly one JSON object"}}}
	}

	if violations := validateReport(report); len(violations) > 0 {
		return nil, &ValidationError{Violations: violations}
	}
	return &report, nil
}

func validateReport(report AgentReport) []Violation {
	var violations []Violation
	violations = append(violations, requireBounded("lane", report.Lane, MaxLaneLength)...)
	violations = append(violations, requireBounded("kind", string(report.Kind), MaxKindLength)...)
	if report.Kind != "" && !validKind(report.Kind) {
		violations = append(violations, Violation{Field: "kind", Message: "must be one of findings, fix, review, advisory, summary, instrument"})
	}
	if report.Findings == nil {
		violations = append(violations, Violation{Field: "findings", Message: "is required; use [] when there are no findings"})
	} else if len(report.Findings) > MaxFindings {
		violations = append(violations, Violation{Field: "findings", Message: fmt.Sprintf("must contain at most %d items", MaxFindings)})
	}
	if report.PRsOpened == nil {
		violations = append(violations, Violation{Field: "prs_opened", Message: "is required; use [] when no PRs were opened"})
	} else if len(report.PRsOpened) > MaxPRsOpened {
		violations = append(violations, Violation{Field: "prs_opened", Message: fmt.Sprintf("must contain at most %d items", MaxPRsOpened)})
	}
	if report.BeadsFiled == nil {
		violations = append(violations, Violation{Field: "beads_filed", Message: "is required; use [] when no beads were filed"})
	} else if len(report.BeadsFiled) > MaxBeadsFiled {
		violations = append(violations, Violation{Field: "beads_filed", Message: fmt.Sprintf("must contain at most %d items", MaxBeadsFiled)})
	}
	if len(report.Artifacts) > MaxArtifacts {
		violations = append(violations, Violation{Field: "artifacts", Message: fmt.Sprintf("must contain at most %d items", MaxArtifacts)})
	}
	violations = append(violations, requireBounded("summary", report.Summary, MaxSummaryLength)...)

	for i, finding := range report.Findings {
		prefix := fmt.Sprintf("findings[%d]", i)
		violations = append(violations, requireBounded(prefix+".title", finding.Title, MaxTitleLength)...)
		violations = append(violations, requireBounded(prefix+".severity", string(finding.Severity), MaxKindLength)...)
		if finding.Severity != "" && !validSeverity(finding.Severity) {
			violations = append(violations, Violation{Field: prefix + ".severity", Message: "must be one of info, low, medium, high, critical"})
		}
		violations = append(violations, requireBounded(prefix+".summary", finding.Summary, MaxFindingBodyLength)...)
		if finding.Line < 0 {
			violations = append(violations, Violation{Field: prefix + ".line", Message: "must be zero or positive"})
		}
	}
	for i, pr := range report.PRsOpened {
		prefix := fmt.Sprintf("prs_opened[%d]", i)
		violations = append(violations, requireBounded(prefix+".repo", pr.Repo, MaxRepoLength)...)
		violations = append(violations, requireBounded(prefix+".title", pr.Title, MaxTitleLength)...)
		if pr.Number <= 0 {
			violations = append(violations, Violation{Field: prefix + ".number", Message: "must be greater than zero"})
		}
		violations = append(violations, optionalBounded(prefix+".url", pr.URL, MaxURLLength)...)
	}
	for i, bead := range report.BeadsFiled {
		prefix := fmt.Sprintf("beads_filed[%d]", i)
		violations = append(violations, requireBounded(prefix+".id", bead.ID, MaxBeadIDLength)...)
		violations = append(violations, requireBounded(prefix+".type", string(bead.Type), MaxKindLength)...)
		if bead.Type != "" && !validBeadType(bead.Type) {
			violations = append(violations, Violation{Field: prefix + ".type", Message: "must be one of task, bug, advisory, feature"})
		}
		violations = append(violations, requireBounded(prefix+".title", bead.Title, MaxTitleLength)...)
		violations = append(violations, optionalBounded(prefix+".url", bead.URL, MaxURLLength)...)
	}
	for i, artifact := range report.Artifacts {
		prefix := fmt.Sprintf("artifacts[%d]", i)
		violations = append(violations, requireBounded(prefix+".repo", artifact.Repo, MaxRepoLength)...)
		violations = append(violations, requireBounded(prefix+".path", artifact.Path, MaxArtifactPathLength)...)
		violations = append(violations, requireBounded(prefix+".description", artifact.Description, MaxFindingBodyLength)...)
	}
	return violations
}

func CorrectivePrompt(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("Your previous structured agent report was invalid and could not be consumed by Hive. Return exactly one JSON object matching the AgentReport contract: required fields lane, kind, findings, prs_opened, beads_filed, summary; artifacts is optional. Use [] for empty arrays. Allowed kind values: findings, fix, review, advisory, summary, instrument. Fix these validation errors: %s. Hive will retry validation at most %d times.", err.Error(), MaxValidationRetries)
}

func AgentReportPath(agentName string) string {
	return filepath.Join(AgentReportDir, AgentReportFilePrefix+safeAgentName(agentName)+AgentReportFileSuffix)
}

func safeAgentName(agentName string) string {
	agentName = strings.TrimSpace(agentName)
	if agentName == "" {
		return "unknown"
	}
	var b strings.Builder
	for _, r := range agentName {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
			continue
		}
		b.WriteByte('_')
	}
	return b.String()
}

func requireBounded(field, value string, max int) []Violation {
	value = strings.TrimSpace(value)
	if value == "" {
		return []Violation{{Field: field, Message: "is required"}}
	}
	return optionalBounded(field, value, max)
}

func optionalBounded(field, value string, max int) []Violation {
	if value == "" {
		return nil
	}
	if len([]rune(value)) > max {
		return []Violation{{Field: field, Message: fmt.Sprintf("must be at most %d characters", max)}}
	}
	return nil
}

func validKind(kind ReportKind) bool {
	switch kind {
	case KindFindings, KindFix, KindReview, KindAdvisory, KindSummary, KindInstrument:
		return true
	default:
		return false
	}
}

func validSeverity(severity Severity) bool {
	switch severity {
	case SeverityInfo, SeverityLow, SeverityMedium, SeverityHigh, SeverityCritical:
		return true
	default:
		return false
	}
}

func validBeadType(beadType BeadType) bool {
	switch beadType {
	case BeadTypeTask, BeadTypeBug, BeadTypeAdvisory, BeadTypeFeature:
		return true
	default:
		return false
	}
}
