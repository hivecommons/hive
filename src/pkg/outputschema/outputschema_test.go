package outputschema

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/jsonextract"
)

func TestValidate(t *testing.T) {
	valid := `{
		"lane":"scanner",
		"kind":"findings",
		"findings":[{"title":"racy write","severity":"high","summary":"shared map is written without a lock","file":"pkg/x.go","line":42}],
		"prs_opened":[{"repo":"hivecommons/hive","number":2806,"title":"structured reports","url":"https://github.com/hivecommons/hive/pull/1"}],
		"beads_filed":[{"id":"bead-1","type":"task","title":"follow-up"}],
		"artifacts":[{"repo":"hivecommons/hive","path":"grafana/dashboards/api.json","description":"API service dashboard"}],
		"summary":"one finding, one PR, one bead"
	}`

	longSummary := strings.Repeat("x", MaxSummaryLength+1)
	tests := []struct {
		name    string
		raw     string
		wantErr []string
	}{
		{
			name: "valid full report",
			raw:  valid,
		},
		{
			name: "valid empty arrays",
			raw:  `{"lane":"fixer","kind":"fix","findings":[],"prs_opened":[],"beads_filed":[],"summary":"nothing to report"}`,
		},
		{
			name: "valid instrument report",
			raw:  `{"lane":"telemetry","kind":"instrument","findings":[],"prs_opened":[],"beads_filed":[],"artifacts":[{"repo":"hivecommons/hive","path":"deploy/servicemonitor.yaml","description":"Prometheus scrape target"}],"summary":"added monitoring"}`,
		},
		{
			name:    "invalid artifact",
			raw:     `{"lane":"telemetry","kind":"instrument","findings":[],"prs_opened":[],"beads_filed":[],"artifacts":[{"repo":"","path":"","description":""}],"summary":"bad artifact"}`,
			wantErr: []string{"artifacts[0].repo", "artifacts[0].path", "artifacts[0].description"},
		},
		{
			name:    "missing required arrays",
			raw:     `{"lane":"scanner","kind":"findings","summary":"missing arrays"}`,
			wantErr: []string{"findings", "prs_opened", "beads_filed"},
		},
		{
			name:    "bad enum values",
			raw:     `{"lane":"scanner","kind":"other","findings":[{"title":"x","severity":"severe","summary":"y"}],"prs_opened":[],"beads_filed":[{"id":"b","type":"unknown","title":"t"}],"summary":"bad enums"}`,
			wantErr: []string{"kind: must be one of findings, fix, review, advisory, summary, instrument", "findings[0].severity", "beads_filed[0].type"},
		},
		{
			name:    "bounded lengths",
			raw:     `{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"` + longSummary + `"}`,
			wantErr: []string{"summary", "at most"},
		},
		{
			name:    "invalid nested values",
			raw:     `{"lane":"scanner","kind":"fix","findings":[{"title":"x","severity":"low","summary":"y","line":-1}],"prs_opened":[{"repo":"hivecommons/hive","number":0,"title":"bad"}],"beads_filed":[],"summary":"bad nested"}`,
			wantErr: []string{"findings[0].line", "prs_opened[0].number"},
		},
		{
			name:    "unknown field rejected",
			raw:     `{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok","extra":true}`,
			wantErr: []string{"unknown field"},
		},
		{
			name:    "empty input",
			raw:     `  `,
			wantErr: []string{"non-empty JSON object"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			report, err := Validate([]byte(tt.raw))
			if len(tt.wantErr) == 0 {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				if report == nil || report.Lane == "" {
					t.Fatalf("Validate() returned empty report: %#v", report)
				}
				return
			}
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			got := err.Error()
			for _, want := range tt.wantErr {
				if !strings.Contains(got, want) {
					t.Fatalf("Validate() error %q missing %q", got, want)
				}
			}
		})
	}
}

func TestCorrectivePrompt(t *testing.T) {
	prompt := CorrectivePrompt(errors.New("kind: must be one of findings, fix"))
	for _, want := range []string{
		"structured agent report was invalid",
		"lane, kind, findings, prs_opened, beads_filed, summary",
		"instrument",
		"kind: must be one of findings, fix",
		"3 times",
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("CorrectivePrompt() = %q, missing %q", prompt, want)
		}
	}
	if got := CorrectivePrompt(nil); got != "" {
		t.Fatalf("CorrectivePrompt(nil) = %q, want empty", got)
	}
}

func TestAgentReportPathSanitizesAgentName(t *testing.T) {
	got := AgentReportPath("../scanner one")
	want := "/var/run/hive-metrics/agent-report-.._scanner_one.json"
	if got != want {
		t.Fatalf("AgentReportPath() = %q, want %q", got, want)
	}
}

// A nil or violation-free ValidationError still yields the generic message —
// callers wrap it blindly, so the fallback branch must not panic or go blank.
func TestValidationErrorEmptyAndNil(t *testing.T) {
	var nilErr *ValidationError
	if got := nilErr.Error(); got != "agent report validation failed" {
		t.Errorf("nil ValidationError.Error() = %q", got)
	}
	if got := (&ValidationError{}).Error(); got != "agent report validation failed" {
		t.Errorf("empty ValidationError.Error() = %q", got)
	}
}

// Exactly one JSON object: trailing content after the report is rejected.
func TestValidateRejectsTrailingJSON(t *testing.T) {
	raw := `{"lane":"scanner","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"ok"}{"lane":"x"}`
	_, err := Validate([]byte(raw))
	if err == nil {
		t.Fatal("Validate() accepted two JSON objects")
	}
	if !strings.Contains(err.Error(), "exactly one JSON object") {
		t.Errorf("Validate() error = %q, want the exactly-one-object violation", err)
	}
}

// Every collection cap has an over-limit branch; each must fire independently.
func TestValidateReportOverMaxCollections(t *testing.T) {
	report := AgentReport{
		Lane:       "scanner",
		Kind:       KindSummary,
		Findings:   make([]Finding, MaxFindings+1),
		PRsOpened:  make([]PROpened, MaxPRsOpened+1),
		BeadsFiled: make([]BeadFiled, MaxBeadsFiled+1),
		Artifacts:  make([]Artifact, MaxArtifacts+1),
		Summary:    "too much of everything",
	}
	violations := validateReport(report)
	for _, field := range []string{"findings", "prs_opened", "beads_filed", "artifacts"} {
		found := false
		for _, v := range violations {
			if v.Field == field && strings.Contains(v.Message, "must contain at most") {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("no over-max violation for %q in %v", field, violations)
		}
	}
}

// A blank agent name must not produce the path "agent-report-.json".
func TestAgentReportPathBlankNameFallsBackToUnknown(t *testing.T) {
	for _, name := range []string{"", "   "} {
		got := AgentReportPath(name)
		if !strings.Contains(got, AgentReportFilePrefix+"unknown"+AgentReportFileSuffix) {
			t.Errorf("AgentReportPath(%q) = %q, want the unknown fallback", name, got)
		}
	}
}

func TestValidateStageReceipt(t *testing.T) {
	raw := mustReadStageReceipt(t)
	report, err := Validate(raw)
	if err != nil {
		t.Fatalf("Validate() stage receipt error = %v", err)
	}
	if report.Receipt == nil || report.Receipt.SchemaVersion != StageReceiptSchemaVersion {
		t.Fatalf("Validate() returned receipt %#v", report.Receipt)
	}
}

func TestValidateStageReceiptMissingRequiredFields(t *testing.T) {
	tests := []struct {
		name  string
		field string
		edit  func(map[string]any)
	}{
		{name: "receipt", field: "stage_receipt", edit: func(root map[string]any) { delete(root, "stage_receipt") }},
		{name: "schema version", field: "stage_receipt.schema_version", edit: deleteReceiptField("schema_version")},
		{name: "work key", field: "stage_receipt.work_key", edit: deleteReceiptField("work_key")},
		{name: "assignment id", field: "stage_receipt.assignment_id", edit: deleteReceiptField("assignment_id")},
		{name: "generation", field: "stage_receipt.generation", edit: deleteReceiptField("generation")},
		{name: "stage", field: "stage_receipt.stage", edit: deleteReceiptField("stage")},
		{name: "contract revision", field: "stage_receipt.contract_revision", edit: deleteReceiptField("contract_revision")},
		{name: "execution key", field: "stage_receipt.execution_key", edit: deleteReceiptField("execution_key")},
		{name: "engine", field: "stage_receipt.engine", edit: deleteReceiptField("engine")},
		{name: "engine name", field: "stage_receipt.engine.name", edit: deleteEngineField("name")},
		{name: "engine version", field: "stage_receipt.engine.version", edit: deleteEngineField("version")},
		{name: "input revision", field: "stage_receipt.input_revision", edit: deleteReceiptField("input_revision")},
		{name: "output digest", field: "stage_receipt.output_digest", edit: deleteReceiptField("output_digest")},
		{name: "result class", field: "stage_receipt.result_class", edit: deleteReceiptField("result_class")},
		{name: "started at", field: "stage_receipt.started_at", edit: deleteReceiptField("started_at")},
		{name: "ended at", field: "stage_receipt.ended_at", edit: deleteReceiptField("ended_at")},
		{name: "provenance", field: "stage_receipt.provenance", edit: deleteReceiptField("provenance")},
		{name: "provenance query", field: "stage_receipt.provenance.query", edit: deleteProvenanceField("query")},
		{name: "artifacts", field: "stage_receipt.artifacts", edit: deleteReceiptField("artifacts")},
		{name: "artifact repo", field: "stage_receipt.artifacts[0].repo", edit: deleteArtifactField("repo")},
		{name: "artifact path", field: "stage_receipt.artifacts[0].path", edit: deleteArtifactField("path")},
		{name: "artifact description", field: "stage_receipt.artifacts[0].description", edit: deleteArtifactField("description")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := mutateStageReceipt(t, tt.edit)
			_, err := Validate(raw)
			if err == nil {
				t.Fatal("Validate() error = nil, want missing field violation")
			}
			validationErr := requireValidationError(t, err)
			if countViolations(validationErr, tt.field) != 1 {
				t.Fatalf("violations for %q = %v, want exactly one", tt.field, validationErr.Violations)
			}
		})
	}
}

func TestValidateStageReceiptRules(t *testing.T) {
	tests := []struct {
		name  string
		field string
		edit  func(map[string]any)
	}{
		{name: "schema version", field: "stage_receipt.schema_version", edit: setReceiptField("schema_version", "stage-receipt/v2")},
		{name: "input revision", field: "stage_receipt.input_revision", edit: setReceiptField("input_revision", "not-a-revision")},
		{name: "output digest", field: "stage_receipt.output_digest", edit: setReceiptField("output_digest", "bad-digest")},
		{name: "result class", field: "stage_receipt.result_class", edit: setReceiptField("result_class", "success")},
		{name: "time ordering", field: "stage_receipt.ended_at", edit: setReceiptField("ended_at", "2026-09-22T16:59:59Z")},
		{name: "started RFC3339", field: "stage_receipt.started_at", edit: setReceiptField("started_at", "2026-09-22 17:00:00")},
		{name: "provenance check run cap", field: "stage_receipt.provenance.check_run_ids", edit: func(root map[string]any) {
			ids := make([]any, MaxReceiptProvenanceCheckRunIDs+1)
			for i := range ids {
				ids[i] = float64(i + 1)
			}
			receiptMap(root)["provenance"].(map[string]any)["check_run_ids"] = ids
		}},
		{name: "completed requires artifact", field: "stage_receipt.artifacts", edit: func(root map[string]any) {
			receiptMap(root)["artifacts"] = []any{}
			receiptMap(root)["output_digest"] = stageReceiptArtifactDigest(nil)
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Validate(mutateStageReceipt(t, tt.edit))
			if err == nil {
				t.Fatal("Validate() error = nil, want rule violation")
			}
			if countViolations(requireValidationError(t, err), tt.field) == 0 {
				t.Fatalf("Validate() error %q missing field %q", err, tt.field)
			}
		})
	}
}

func TestValidateStageReceiptNoChangeAllowsNoArtifacts(t *testing.T) {
	raw := mutateStageReceipt(t, func(root map[string]any) {
		receipt := receiptMap(root)
		receipt["result_class"] = "no_change"
		receipt["artifacts"] = []any{}
		receipt["output_digest"] = stageReceiptArtifactDigest(nil)
	})
	if _, err := Validate(raw); err != nil {
		t.Fatalf("Validate() no_change empty artifacts error = %v", err)
	}
}

func TestValidateStageReceiptExtractedFromProse(t *testing.T) {
	prose := "stage finished; receipt follows:\n```json\n" + string(mustReadStageReceipt(t)) + "\n```\nready for handoff"
	extracted := jsonextract.Object(prose)
	if extracted == "" {
		t.Fatal("jsonextract.Object() returned empty")
	}
	if _, err := Validate([]byte(extracted)); err != nil {
		t.Fatalf("Validate(jsonextract.Object(prose)) error = %v", err)
	}
}

func TestCorrectivePromptStageReceipt(t *testing.T) {
	prompt := CorrectivePrompt(&ValidationError{Violations: []Violation{{Field: "stage_receipt.output_digest", Message: "does not match artifacts digest"}}})
	for _, want := range []string{"stage_receipt", "schema_version", "work_key", "assignment_id", "generation", "contract_revision", "execution_key", "engine.name", "engine.version", "input_revision", "output_digest", "result_class", "started_at", "ended_at", "provenance", "artifacts"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("CorrectivePrompt() = %q, missing %q", prompt, want)
		}
	}
}

func mustReadStageReceipt(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/stage_receipt_v1.json")
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func mutateStageReceipt(t *testing.T, edit func(map[string]any)) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(mustReadStageReceipt(t), &root); err != nil {
		t.Fatal(err)
	}
	edit(root)
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func receiptMap(root map[string]any) map[string]any {
	return root["stage_receipt"].(map[string]any)
}

func deleteReceiptField(field string) func(map[string]any) {
	return func(root map[string]any) { delete(receiptMap(root), field) }
}

func deleteEngineField(field string) func(map[string]any) {
	return func(root map[string]any) { delete(receiptMap(root)["engine"].(map[string]any), field) }
}

func deleteProvenanceField(field string) func(map[string]any) {
	return func(root map[string]any) { delete(receiptMap(root)["provenance"].(map[string]any), field) }
}

func deleteArtifactField(field string) func(map[string]any) {
	return func(root map[string]any) { delete(receiptMap(root)["artifacts"].([]any)[0].(map[string]any), field) }
}

func setReceiptField(field string, value any) func(map[string]any) {
	return func(root map[string]any) { receiptMap(root)[field] = value }
}

func requireValidationError(t *testing.T, err error) *ValidationError {
	t.Helper()
	validationErr, ok := err.(*ValidationError)
	if !ok {
		t.Fatalf("Validate() error = %T %v, want *ValidationError", err, err)
	}
	return validationErr
}

func countViolations(err *ValidationError, field string) int {
	count := 0
	for _, violation := range err.Violations {
		if violation.Field == field {
			count++
		}
	}
	return count
}
