package outputschema

import (
	"errors"
	"strings"
	"testing"
)

func TestValidate(t *testing.T) {
	valid := `{
		"lane":"scanner",
		"kind":"findings",
		"findings":[{"title":"racy write","severity":"high","summary":"shared map is written without a lock","file":"pkg/x.go","line":42}],
		"prs_opened":[{"repo":"kubestellar/hive","number":2806,"title":"structured reports","url":"https://github.com/hivecommons/hive/pull/1"}],
		"beads_filed":[{"id":"bead-1","type":"task","title":"follow-up"}],
		"artifacts":[{"repo":"kubestellar/hive","path":"grafana/dashboards/api.json","description":"API service dashboard"}],
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
			raw:  `{"lane":"telemetry","kind":"instrument","findings":[],"prs_opened":[],"beads_filed":[],"artifacts":[{"repo":"kubestellar/hive","path":"deploy/servicemonitor.yaml","description":"Prometheus scrape target"}],"summary":"added monitoring"}`,
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
			raw:     `{"lane":"scanner","kind":"fix","findings":[{"title":"x","severity":"low","summary":"y","line":-1}],"prs_opened":[{"repo":"kubestellar/hive","number":0,"title":"bad"}],"beads_filed":[],"summary":"bad nested"}`,
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
