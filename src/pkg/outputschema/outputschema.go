// Package outputschema is the Hive-side name for the structured agent-report
// contract. The schema and its validator live in the Crux ring as
// pkg/crux/outputschema (the runtime validates a report before the governor
// sees it) and are re-exported here so governor-side callers keep their
// import path: Crux owns, Hive aliases (hivecommons/hive#13536). What stays
// here is host knowledge: where report files live on a hive and how an
// agent name becomes a file name.
package outputschema

import (
	"path/filepath"
	"strings"
	"unicode"

	cruxoutputschema "github.com/hivecommons/hive/pkg/crux/outputschema"
)

const (
	MaxValidationRetries = cruxoutputschema.MaxValidationRetries

	AgentReportFilePrefix = cruxoutputschema.AgentReportFilePrefix
	AgentReportFileSuffix = cruxoutputschema.AgentReportFileSuffix

	MaxLaneLength                   = cruxoutputschema.MaxLaneLength
	MaxKindLength                   = cruxoutputschema.MaxKindLength
	MaxSummaryLength                = cruxoutputschema.MaxSummaryLength
	MaxFindings                     = cruxoutputschema.MaxFindings
	MaxPRsOpened                    = cruxoutputschema.MaxPRsOpened
	MaxBeadsFiled                   = cruxoutputschema.MaxBeadsFiled
	MaxArtifacts                    = cruxoutputschema.MaxArtifacts
	MaxTitleLength                  = cruxoutputschema.MaxTitleLength
	MaxFindingBodyLength            = cruxoutputschema.MaxFindingBodyLength
	MaxRepoLength                   = cruxoutputschema.MaxRepoLength
	MaxURLLength                    = cruxoutputschema.MaxURLLength
	MaxBeadIDLength                 = cruxoutputschema.MaxBeadIDLength
	MaxArtifactPathLength           = cruxoutputschema.MaxArtifactPathLength
	MaxReceiptFieldLength           = cruxoutputschema.MaxReceiptFieldLength
	MaxReceiptProvenanceCheckRunIDs = cruxoutputschema.MaxReceiptProvenanceCheckRunIDs

	StageReceiptSchemaVersion = cruxoutputschema.StageReceiptSchemaVersion
)

const defaultAgentReportDir = "/var/run/hive-metrics"

// AgentReportDir is where per-agent structured report files are read from and
// written to. Production leaves it at defaultAgentReportDir; a var (not
// const) only so governor and outputschema tests can point it at a temp dir,
// matching the knowledgeBaseDir pattern in pkg/knowledge/api.go.
var AgentReportDir = defaultAgentReportDir

type (
	ReportKind              = cruxoutputschema.ReportKind
	Severity                = cruxoutputschema.Severity
	BeadType                = cruxoutputschema.BeadType
	AgentReport             = cruxoutputschema.AgentReport
	Artifact                = cruxoutputschema.Artifact
	StageReceiptResultClass = cruxoutputschema.StageReceiptResultClass
	StageReceipt            = cruxoutputschema.StageReceipt
	StageReceiptEngine      = cruxoutputschema.StageReceiptEngine
	Finding                 = cruxoutputschema.Finding
	PROpened                = cruxoutputschema.PROpened
	BeadFiled               = cruxoutputschema.BeadFiled
	Violation               = cruxoutputschema.Violation
	ValidationError         = cruxoutputschema.ValidationError
)

const (
	KindFindings     = cruxoutputschema.KindFindings
	KindFix          = cruxoutputschema.KindFix
	KindReview       = cruxoutputschema.KindReview
	KindAdvisory     = cruxoutputschema.KindAdvisory
	KindSummary      = cruxoutputschema.KindSummary
	KindInstrument   = cruxoutputschema.KindInstrument
	KindStageReceipt = cruxoutputschema.KindStageReceipt

	SeverityInfo     = cruxoutputschema.SeverityInfo
	SeverityLow      = cruxoutputschema.SeverityLow
	SeverityMedium   = cruxoutputschema.SeverityMedium
	SeverityHigh     = cruxoutputschema.SeverityHigh
	SeverityCritical = cruxoutputschema.SeverityCritical

	BeadTypeTask     = cruxoutputschema.BeadTypeTask
	BeadTypeBug      = cruxoutputschema.BeadTypeBug
	BeadTypeAdvisory = cruxoutputschema.BeadTypeAdvisory
	BeadTypeFeature  = cruxoutputschema.BeadTypeFeature

	ReceiptResultCompleted = cruxoutputschema.ReceiptResultCompleted
	ReceiptResultNoChange  = cruxoutputschema.ReceiptResultNoChange
	ReceiptResultBlocked   = cruxoutputschema.ReceiptResultBlocked
	ReceiptResultFailed    = cruxoutputschema.ReceiptResultFailed
	ReceiptResultUnknown   = cruxoutputschema.ReceiptResultUnknown
)

var (
	Validate         = cruxoutputschema.Validate
	CorrectivePrompt = cruxoutputschema.CorrectivePrompt
)

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
