package outputschema

import (
	"strings"
	"testing"
)

func TestAgentReportPathSanitizesAgentName(t *testing.T) {
	got := AgentReportPath("../scanner one")
	want := "/var/run/hive-metrics/agent-report-.._scanner_one.json"
	if got != want {
		t.Fatalf("AgentReportPath() = %q, want %q", got, want)
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

// The schema is the Crux leaf's: a report validated here is the same type the
// runtime produces, and the kind vocabulary is shared by value.
func TestSchemaIsTheCruxLeaf(t *testing.T) {
	report, err := Validate([]byte(`{"lane":"l","kind":"summary","findings":[],"prs_opened":[],"beads_filed":[],"summary":"s"}`))
	if err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if report.Kind != KindSummary {
		t.Fatalf("Kind = %q, want %q", report.Kind, KindSummary)
	}
}
