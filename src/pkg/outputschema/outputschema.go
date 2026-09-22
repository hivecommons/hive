package outputschema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hivecommons/hive/pkg/convergence/proof"
	"github.com/hivecommons/hive/pkg/effects"
)

const (
	// MaxValidationRetries bounds corrective re-prompt attempts after invalid output.
	MaxValidationRetries = 3

	defaultAgentReportDir = "/var/run/hive-metrics"

	AgentReportFilePrefix = "agent-report-"
	AgentReportFileSuffix = ".json"

	MaxLaneLength                   = 64
	MaxKindLength                   = 64
	MaxSummaryLength                = 2000
	MaxFindings                     = 100
	MaxPRsOpened                    = 50
	MaxBeadsFiled                   = 50
	MaxArtifacts                    = 100
	MaxTitleLength                  = 200
	MaxFindingBodyLength            = 2000
	MaxRepoLength                   = 200
	MaxURLLength                    = 500
	MaxBeadIDLength                 = 120
	MaxArtifactPathLength           = 500
	MaxReceiptFieldLength           = 500
	MaxReceiptProvenanceCheckRunIDs = 100
)

const StageReceiptSchemaVersion = "stage-receipt/v1"

// AgentReportDir is where per-agent structured report files are read from and
// written to. Production leaves it at defaultAgentReportDir; a var (not
// const) only so governor and outputschema tests can point it at a temp dir,
// matching the knowledgeBaseDir pattern in pkg/knowledge/api.go.
var AgentReportDir = defaultAgentReportDir

type ReportKind string

const (
	KindFindings     ReportKind = "findings"
	KindFix          ReportKind = "fix"
	KindReview       ReportKind = "review"
	KindAdvisory     ReportKind = "advisory"
	KindSummary      ReportKind = "summary"
	KindInstrument   ReportKind = "instrument"
	KindStageReceipt ReportKind = "stage_receipt"
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
	Lane       string        `json:"lane"`
	Kind       ReportKind    `json:"kind"`
	Findings   []Finding     `json:"findings"`
	PRsOpened  []PROpened    `json:"prs_opened"`
	BeadsFiled []BeadFiled   `json:"beads_filed"`
	Artifacts  []Artifact    `json:"artifacts,omitempty"`
	Summary    string        `json:"summary"`
	Receipt    *StageReceipt `json:"stage_receipt,omitempty"`
}

// Artifact records a repository file produced or materially updated by an agent.
// It lets artifact-producing lanes report their primary output independently of
// the pull request that happens to carry it.
type Artifact struct {
	Repo        string `json:"repo"`
	Path        string `json:"path"`
	Description string `json:"description"`
}

type StageReceiptResultClass string

const (
	ReceiptResultCompleted StageReceiptResultClass = "completed"
	ReceiptResultNoChange  StageReceiptResultClass = "no_change"
	ReceiptResultBlocked   StageReceiptResultClass = "blocked"
	ReceiptResultFailed    StageReceiptResultClass = "failed"
	ReceiptResultUnknown   StageReceiptResultClass = "unknown"
)

type StageReceipt struct {
	SchemaVersion     string                  `json:"schema_version"`
	WorkKey           string                  `json:"work_key"`
	AssignmentID      string                  `json:"assignment_id"`
	Generation        uint64                  `json:"generation"`
	Stage             string                  `json:"stage"`
	ContractRevision  string                  `json:"contract_revision"`
	ExecutionKey      string                  `json:"execution_key"`
	Engine            *StageReceiptEngine     `json:"engine"`
	RemoteRunID       string                  `json:"remote_run_id,omitempty"`
	RemoteIncarnation string                  `json:"remote_incarnation,omitempty"`
	InputRevision     string                  `json:"input_revision"`
	OutputDigest      string                  `json:"output_digest"`
	ResultClass       StageReceiptResultClass `json:"result_class"`
	StartedAt         string                  `json:"started_at"`
	EndedAt           string                  `json:"ended_at"`
	Provenance        *proof.Provenance       `json:"provenance"`
	Artifacts         []Artifact              `json:"artifacts"`
}

type StageReceiptEngine struct {
	Name    string `json:"name"`
	Version string `json:"version"`
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
		violations = append(violations, Violation{Field: "kind", Message: "must be one of findings, fix, review, advisory, summary, instrument, stage_receipt"})
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
	violations = append(violations, validateArtifacts("artifacts", report.Artifacts, report.Artifacts != nil)...)
	if report.Kind == KindStageReceipt {
		violations = append(violations, validateStageReceipt(report.Receipt)...)
	}
	return violations
}

func CorrectivePrompt(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("Your previous structured agent report was invalid and could not be consumed by Hive. Return exactly one JSON object matching the AgentReport contract: required fields lane, kind, findings, prs_opened, beads_filed, summary; artifacts is optional. For kind stage_receipt, include stage_receipt with schema_version, work_key, assignment_id, generation, stage, contract_revision, execution_key, engine.name, engine.version, input_revision, output_digest, result_class, started_at, ended_at, provenance, and artifacts. Use [] for empty arrays. Allowed kind values: findings, fix, review, advisory, summary, instrument, stage_receipt. Fix these validation errors: %s. Hive will retry validation at most %d times.", err.Error(), MaxValidationRetries)
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

var (
	gitSHAPattern       = regexp.MustCompile(`^[0-9a-f]{40}$`)
	artifactRevisionPat = regexp.MustCompile(`^[^@\s]+@(sha256:)?[0-9a-f]{40,64}$`)
)

func validateStageReceipt(receipt *StageReceipt) []Violation {
	if receipt == nil {
		return []Violation{{Field: "stage_receipt", Message: "is required for kind stage_receipt"}}
	}
	var violations []Violation
	if receipt.SchemaVersion == "" {
		violations = append(violations, Violation{Field: "stage_receipt.schema_version", Message: "is required"})
	} else if receipt.SchemaVersion != StageReceiptSchemaVersion {
		violations = append(violations, Violation{Field: "stage_receipt.schema_version", Message: "must be stage-receipt/v1"})
	}
	violations = append(violations, requireBounded("stage_receipt.work_key", receipt.WorkKey, MaxReceiptFieldLength)...)
	violations = append(violations, requireBounded("stage_receipt.assignment_id", receipt.AssignmentID, MaxReceiptFieldLength)...)
	if receipt.Generation == 0 {
		violations = append(violations, Violation{Field: "stage_receipt.generation", Message: "is required and must be greater than zero"})
	}
	violations = append(violations, requireBounded("stage_receipt.stage", receipt.Stage, MaxReceiptFieldLength)...)
	violations = append(violations, requireBounded("stage_receipt.contract_revision", receipt.ContractRevision, MaxReceiptFieldLength)...)
	violations = append(violations, requireBounded("stage_receipt.execution_key", receipt.ExecutionKey, MaxReceiptFieldLength)...)
	if receipt.Engine == nil {
		violations = append(violations, Violation{Field: "stage_receipt.engine", Message: "is required"})
	} else {
		violations = append(violations, requireBounded("stage_receipt.engine.name", receipt.Engine.Name, MaxReceiptFieldLength)...)
		violations = append(violations, requireBounded("stage_receipt.engine.version", receipt.Engine.Version, MaxReceiptFieldLength)...)
	}
	violations = append(violations, optionalBounded("stage_receipt.remote_run_id", receipt.RemoteRunID, MaxReceiptFieldLength)...)
	violations = append(violations, optionalBounded("stage_receipt.remote_incarnation", receipt.RemoteIncarnation, MaxReceiptFieldLength)...)
	violations = append(violations, requireBounded("stage_receipt.input_revision", receipt.InputRevision, MaxReceiptFieldLength)...)
	if receipt.InputRevision != "" && !validInputRevision(receipt.InputRevision) {
		violations = append(violations, Violation{Field: "stage_receipt.input_revision", Message: "must be a 40-hex git SHA or artifact@hash"})
	}
	violations = append(violations, requireBounded("stage_receipt.output_digest", receipt.OutputDigest, MaxReceiptFieldLength)...)
	if receipt.ResultClass == "" {
		violations = append(violations, Violation{Field: "stage_receipt.result_class", Message: "is required"})
	} else if !validStageReceiptResultClass(receipt.ResultClass) {
		violations = append(violations, Violation{Field: "stage_receipt.result_class", Message: "must be one of completed, no_change, blocked, failed, unknown"})
	}
	startedAt, startedOK := validateReceiptTime("stage_receipt.started_at", receipt.StartedAt, &violations)
	endedAt, endedOK := validateReceiptTime("stage_receipt.ended_at", receipt.EndedAt, &violations)
	if startedOK && endedOK && endedAt.Before(startedAt) {
		violations = append(violations, Violation{Field: "stage_receipt.ended_at", Message: "must be at or after started_at"})
	}
	if receipt.Provenance == nil {
		violations = append(violations, Violation{Field: "stage_receipt.provenance", Message: "is required"})
	} else {
		violations = append(violations, requireBounded("stage_receipt.provenance.query", receipt.Provenance.Query, MaxReceiptFieldLength)...)
		if len(receipt.Provenance.CheckRunIDs) > MaxReceiptProvenanceCheckRunIDs {
			violations = append(violations, Violation{Field: "stage_receipt.provenance.check_run_ids", Message: fmt.Sprintf("must contain at most %d items", MaxReceiptProvenanceCheckRunIDs)})
		}
	}
	if receipt.Artifacts == nil {
		violations = append(violations, Violation{Field: "stage_receipt.artifacts", Message: "is required; use [] when there are no artifacts"})
	} else {
		artifactViolations := validateArtifacts("stage_receipt.artifacts", receipt.Artifacts, true)
		violations = append(violations, artifactViolations...)
		if receipt.ResultClass == ReceiptResultCompleted && len(receipt.Artifacts) == 0 {
			violations = append(violations, Violation{Field: "stage_receipt.artifacts", Message: "completed receipts require at least one artifact"})
		}
		if receipt.OutputDigest != "" && len(artifactViolations) == 0 {
			want := stageReceiptArtifactDigest(receipt.Artifacts)
			if receipt.OutputDigest != want {
				violations = append(violations, Violation{Field: "stage_receipt.output_digest", Message: "does not match artifacts digest"})
			}
		}
	}
	return violations
}

func validateReceiptTime(field, value string, violations *[]Violation) (time.Time, bool) {
	if strings.TrimSpace(value) == "" {
		*violations = append(*violations, Violation{Field: field, Message: "is required"})
		return time.Time{}, false
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		*violations = append(*violations, Violation{Field: field, Message: "must be RFC3339"})
		return time.Time{}, false
	}
	return parsed, true
}

func validateArtifacts(field string, artifacts []Artifact, present bool) []Violation {
	if !present {
		return nil
	}
	var violations []Violation
	for i, artifact := range artifacts {
		prefix := fmt.Sprintf("%s[%d]", field, i)
		violations = append(violations, requireBounded(prefix+".repo", artifact.Repo, MaxRepoLength)...)
		violations = append(violations, requireBounded(prefix+".path", artifact.Path, MaxArtifactPathLength)...)
		violations = append(violations, requireBounded(prefix+".description", artifact.Description, MaxFindingBodyLength)...)
	}
	return violations
}

func stageReceiptArtifactDigest(artifacts []Artifact) string {
	parts := make([]string, 0, len(artifacts))
	for _, artifact := range artifacts {
		parts = append(parts, strings.Join([]string{artifact.Repo, artifact.Path, artifact.Description}, "\x00"))
	}
	sort.Strings(parts)
	return effects.StableDigest(parts...)
}

func validInputRevision(revision string) bool {
	return gitSHAPattern.MatchString(revision) || artifactRevisionPat.MatchString(revision)
}

func validStageReceiptResultClass(result StageReceiptResultClass) bool {
	switch result {
	case ReceiptResultCompleted, ReceiptResultNoChange, ReceiptResultBlocked, ReceiptResultFailed, ReceiptResultUnknown:
		return true
	default:
		return false
	}
}

func validKind(kind ReportKind) bool {
	switch kind {
	case KindFindings, KindFix, KindReview, KindAdvisory, KindSummary, KindInstrument, KindStageReceipt:
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
