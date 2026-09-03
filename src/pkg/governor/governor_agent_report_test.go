package governor

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/hivecommons/hive/pkg/outputschema"
)

// withAgentReportDir redirects outputschema.AgentReportDir to a temp dir for
// the duration of the test, restoring the production value on cleanup. This
// is the seam #4940 asked for: AgentReportDir changed from a const to a var
// so RecordKick's report-validation path can be exercised hermetically.
func withAgentReportDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	old := outputschema.AgentReportDir
	outputschema.AgentReportDir = dir
	t.Cleanup(func() { outputschema.AgentReportDir = old })
	return dir
}

func newTestGovernor() *Governor {
	cfg, agents := standardConfig("scanner")
	return New(cfg, agents, testLogger())
}

// TestRecordKickNoReportFileIsSteadyState covers the os.IsNotExist branch:
// no report file present must not create a record at all. Removing the
// os.IsNotExist short-circuit would instead record an Error-populated entry
// for every kick, which this test would catch via len(records) != 0.
func TestRecordKickNoReportFileIsSteadyState(t *testing.T) {
	withAgentReportDir(t)
	g := newTestGovernor()

	g.RecordKick("scanner")

	records := g.AgentReportRecords()
	if len(records) != 0 {
		t.Fatalf("AgentReportRecords() = %#v, want empty when no report file exists", records)
	}
}

// TestRecordKickUnreadableReportRecordsError covers the read-error branch:
// a path that exists but cannot be read (a directory, here) must still
// produce a record, with Error set and Valid left false.
func TestRecordKickUnreadableReportRecordsError(t *testing.T) {
	withAgentReportDir(t)
	g := newTestGovernor()

	// Make AgentReportPath("scanner") point at a directory instead of a
	// file, so os.ReadFile fails with something other than IsNotExist.
	path := outputschema.AgentReportPath("scanner")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("MkdirAll(%s): %v", path, err)
	}

	g.RecordKick("scanner")

	records := g.AgentReportRecords()
	rec, ok := records["scanner"]
	if !ok {
		t.Fatalf("AgentReportRecords() = %#v, want an entry for scanner", records)
	}
	if rec.Valid {
		t.Fatalf("record.Valid = true, want false for an unreadable report")
	}
	if rec.Error == "" {
		t.Fatalf("record.Error = %q, want a non-empty read error", rec.Error)
	}
	if rec.Path != path {
		t.Fatalf("record.Path = %q, want %q", rec.Path, path)
	}
}

// TestRecordKickInvalidJSONRecordsValidationError covers the
// outputschema.Validate error branch: malformed report JSON must produce a
// record with Error set (not Valid), proving the validation call is wired in
// rather than the read simply succeeding and being ignored.
func TestRecordKickInvalidJSONRecordsValidationError(t *testing.T) {
	withAgentReportDir(t)
	g := newTestGovernor()

	path := outputschema.AgentReportPath("scanner")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if err := os.WriteFile(path, []byte(`{"lane":"scanner"`), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	g.RecordKick("scanner")

	rec, ok := g.AgentReportRecords()["scanner"]
	if !ok {
		t.Fatal("expected a record for scanner")
	}
	if rec.Valid {
		t.Fatal("record.Valid = true, want false for invalid JSON")
	}
	if rec.Error == "" {
		t.Fatal("record.Error is empty, want the decode/validation error")
	}
}

// TestRecordKickValidReportCapturesFields covers the success branch end to
// end: a well-formed report file must produce a Valid record with lane,
// kind, and summary copied from the parsed report. Deleting the field
// assignments after outputschema.Validate succeeds would leave these zero
// while Valid stayed true, so each field is asserted individually.
func TestRecordKickValidReportCapturesFields(t *testing.T) {
	withAgentReportDir(t)
	g := newTestGovernor()

	path := outputschema.AgentReportPath("scanner")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	report := `{
		"lane":"scanner",
		"kind":"findings",
		"findings":[],
		"prs_opened":[],
		"beads_filed":[],
		"summary":"nothing to report"
	}`
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	g.RecordKick("scanner")

	rec, ok := g.AgentReportRecords()["scanner"]
	if !ok {
		t.Fatal("expected a record for scanner")
	}
	if !rec.Valid {
		t.Fatalf("record.Valid = false, want true; error=%q", rec.Error)
	}
	if rec.Lane != "scanner" || rec.Kind != "findings" || rec.Summary != "nothing to report" {
		t.Fatalf("record = %#v, want lane=scanner kind=findings summary=%q", rec, "nothing to report")
	}
}

// TestAgentReportRecordsReturnsIndependentCopy covers the snapshot-copy
// contract of AgentReportRecords: mutating the returned map must not affect
// governor-internal state, and a later kick must not retroactively change an
// already-returned snapshot. Returning g.agentReports directly (no copy)
// would fail the second assertion once a second kick runs.
func TestAgentReportRecordsReturnsIndependentCopy(t *testing.T) {
	withAgentReportDir(t)
	g := newTestGovernor()

	path := outputschema.AgentReportPath("scanner")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	report := `{"lane":"scanner","kind":"findings","findings":[],"prs_opened":[],"beads_filed":[],"summary":"first"}`
	if err := os.WriteFile(path, []byte(report), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	g.RecordKick("scanner")

	snapshot := g.AgentReportRecords()
	mutated := snapshot["scanner"]
	mutated.Summary = "mutated by caller"
	snapshot["scanner"] = mutated
	snapshot["ghost-agent"] = AgentReportRecord{Summary: "should not leak back"}

	fresh := g.AgentReportRecords()
	if fresh["scanner"].Summary != "first" {
		t.Fatalf("internal state leaked: got summary %q, want unaffected %q", fresh["scanner"].Summary, "first")
	}
	if _, exists := fresh["ghost-agent"]; exists {
		t.Fatal("write into a returned snapshot leaked into governor-internal state")
	}
}
