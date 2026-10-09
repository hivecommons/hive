package compliance

import (
	"encoding/csv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

func exportRun(at time.Time, statuses map[string]PostureStatus) PostureRun {
	run := PostureRun{At: at, Trigger: TriggerSchedule}
	for _, c := range PostureCatalogue() {
		st, ok := statuses[c.ID]
		if !ok {
			continue
		}
		run.Results = append(run.Results, Result{CheckID: c.ID, Title: c.Title, ControlIDs: c.ControlIDs, At: at, Status: st, Pass: st == PosturePass})
	}
	return run
}

func TestBuildPostureSeries(t *testing.T) {
	cat := PostureCatalogue()
	first := cat[0].ID
	t0 := attestNow.Add(-3 * time.Hour)
	runs := []PostureRun{
		exportRun(t0, map[string]PostureStatus{first: PosturePass}),
		exportRun(t0.Add(time.Hour), map[string]PostureStatus{first: PostureFail}),
		exportRun(t0.Add(2*time.Hour), map[string]PostureStatus{first: PostureSkip}),
		exportRun(t0.Add(3*time.Hour), map[string]PostureStatus{first: PostureError}),
	}
	// A result for a check no longer in the catalogue, with no own timestamp.
	runs[3].Results = append(runs[3].Results, Result{CheckID: "retired", Title: "Retired", Status: PosturePass})

	got := BuildPostureSeries(runs)
	if len(got) != len(cat)+1 {
		t.Fatalf("series = %d, want %d", len(got), len(cat)+1)
	}
	s := got[0]
	if s.CheckID != first || len(s.Points) != 4 {
		t.Fatalf("first series = %s with %d points", s.CheckID, len(s.Points))
	}
	if s.Summary != (PostureSummary{Pass: 1, Fail: 1, Skip: 1, Error: 1}) {
		t.Fatalf("summary = %+v", s.Summary)
	}
	if s.LastFailAt == nil || !s.LastFailAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("last_fail_at = %v", s.LastFailAt)
	}
	if s.Last == nil || s.Last.Status != PostureError {
		t.Fatalf("last = %+v", s.Last)
	}
	if got[1].Points == nil || len(got[1].Points) != 0 || got[1].Last != nil {
		t.Fatalf("idle check should have empty points and no last: %+v", got[1])
	}
	retired := got[len(got)-1]
	if retired.CheckID != "retired" || len(retired.Points) != 1 || !retired.Points[0].At.Equal(runs[3].At) {
		t.Fatalf("retired series = %+v", retired)
	}
	if empty := BuildPostureSeries(nil); len(empty) != len(cat) {
		t.Fatalf("empty series = %d", len(empty))
	}
}

func TestPostureHistoryBetween(t *testing.T) {
	h, _ := NewPostureHistory("", 0, 0)
	for i := 0; i < 5; i++ {
		_ = h.Append(PostureRun{At: attestNow.Add(time.Duration(i) * time.Hour)})
	}
	cases := []struct {
		name         string
		since, until time.Time
		limit        int
		want         int
		truncated    bool
	}{
		{name: "all", want: 5},
		{name: "since", since: attestNow.Add(2 * time.Hour), want: 3},
		{name: "until exclusive", until: attestNow.Add(2 * time.Hour), want: 2},
		{name: "window", since: attestNow.Add(time.Hour), until: attestNow.Add(3 * time.Hour), want: 2},
		{name: "limit", limit: 2, want: 2, truncated: true},
		{name: "empty", since: attestNow.Add(10 * time.Hour), want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, tr := h.Between(tc.since, tc.until, tc.limit)
			if len(got) != tc.want || tr != tc.truncated {
				t.Fatalf("got %d runs truncated=%v, want %d/%v", len(got), tr, tc.want, tc.truncated)
			}
		})
	}
}

func TestReportMarkdown(t *testing.T) {
	cfg := &config.Config{Compliance: config.ComplianceConfig{Frameworks: []string{"soc2-type2"}}}
	r := BuildReport(cfg, func(string) string { return "" })
	md := ReportMarkdown(r, "hive-1", attestNow)
	for _, want := range []string{"# Compliance control-mapping report", "Hive is not certified", "`hive-1`", "2026-10-08T12:00:00Z", "Framework: `soc2-type2`", "_not covered by Hive_", "| Framework | Control |"} {
		if !strings.Contains(md, want) {
			t.Fatalf("markdown missing %q:\n%s", want, md)
		}
	}
	empty := ReportMarkdown(BuildReport(nil, nil), "", attestNow)
	if !strings.Contains(empty, "(unset)") || !strings.Contains(empty, "none selected") || strings.Contains(empty, "| Framework |") {
		t.Fatalf("empty report markdown:\n%s", empty)
	}
	// A control with no settings that is still covered gets a placeholder row.
	bare := Report{Frameworks: []string{"x"}, Controls: []ControlStatus{{Framework: "x", ControlID: "C1", Title: "T", Status: StatusOff}}}
	if !strings.Contains(ReportMarkdown(bare, "h", attestNow), "| — | — | — |") {
		t.Fatal("bare control row missing")
	}
}

func TestPostureHistoryCSV(t *testing.T) {
	run := exportRun(attestNow, map[string]PostureStatus{PostureCatalogue()[0].ID: PostureFail})
	run.Results[0].Detail = "=HYPERLINK(\"x\")"
	run.Results[0].EvidenceRefs = []string{"a", "b"}
	rows, err := csv.NewReader(strings.NewReader(string(PostureHistoryCSV([]PostureRun{run})))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 || rows[0][0] != "run_at" {
		t.Fatalf("rows = %v", rows)
	}
	if rows[1][4] != "fail" || rows[1][5] != "false" || rows[1][8] != "a b" {
		t.Fatalf("row = %v", rows[1])
	}
	if !strings.HasPrefix(rows[1][7], "'=") {
		t.Fatalf("formula not neutralised: %q", rows[1][7])
	}
}

func TestAttestationsCSV(t *testing.T) {
	rows, err := csv.NewReader(strings.NewReader(string(AttestationsCSV([]Attestation{
		{Framework: "soc2-type2", ReviewedOn: "2026-10-08", By: "alice", At: attestNow, Note: "+1, fine"},
	})))).ReadAll()
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(rows) != 2 || rows[1][2] != "alice" || rows[1][4] != "'+1, fine" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestCSVSafe(t *testing.T) {
	cases := map[string]string{"": "", "ok": "ok", "=1": "'=1", "+1": "'+1", "-1": "'-1", "@x": "'@x", "\tx": "'\tx"}
	for in, want := range cases {
		if got := csvSafe(in); got != want {
			t.Fatalf("csvSafe(%q) = %q, want %q", in, got, want)
		}
	}
}
