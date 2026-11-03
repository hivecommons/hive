package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func historicalIssueWeek(start time.Time) evidence.IssueWeek {
	return evidence.IssueWeek{
		Week: evidenceWeekKey(start), CoveredFrom: start, CoveredTo: start.AddDate(0, 0, 7),
		Opened: map[string]int{"human": 1}, Closed: 1,
		Events: []evidence.IssueEvent{
			{At: start.Add(time.Hour), Action: evidence.IssueActionOpened, Kind: evidence.AuthorHuman},
			{At: start.Add(2 * time.Hour), Action: evidence.IssueActionClosed},
		},
	}
}

func TestEvidenceIssueSeriesCollectorBoundary(t *testing.T) {
	boundary := time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)
	old := boundary.AddDate(0, 0, -7)
	for _, tc := range []struct {
		name                                                  string
		since                                                 time.Time
		seriesWeeks, auditWeeks, boundaryHuman, boundaryAgent int
	}{
		{"exact Monday", boundary, 1, 0, 0, 1},
		// A partial collector week is not authoritative, even for events
		// later in that week. The issue series supplies the complete week.
		{"midweek", boundary.Add(24 * time.Hour), 2, 0, 1, 0},
		{"no collector", time.Time{}, 2, 0, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := evidenceWorkBalanceInputs{
				Series: []evidence.IssueSeriesWeek{
					{Repo: "o/r", IssueWeek: historicalIssueWeek(old)},
					{Repo: "o/r", IssueWeek: historicalIssueWeek(boundary)},
					{Repo: "x/y", IssueWeek: historicalIssueWeek(old)},
				},
				Rollup: IssueActivityRollup{Since: tc.since, Issues: map[string]ghpkg.IssueActivity{
					"o/r#1": {Repo: "o/r", Kind: "agent", CreatedAt: boundary.Add(time.Hour)},
				}},
				Entries: []AuditEntry{
					{Timestamp: old.Add(time.Hour).Format(time.RFC3339), Action: ghpkg.AuditActionAgentIssueCreated, Repo: "o/r", Target: 2},
					{Timestamp: boundary.Add(time.Hour).Format(time.RFC3339), Action: ghpkg.AuditActionIssueClosed, Repo: "o/r", Target: 1},
				},
			}
			got, src := buildEvidenceWorkBalance(in, evidenceWindow{To: boundary.AddDate(0, 0, 7)}, "O/R")
			if len(got) != 2 {
				t.Fatalf("weeks = %+v", got)
			}
			if w := got[0]; w.Opened["human"] != 1 || w.Opened["agent"] != 0 || w.Closed != 1 || w.N != 2 || w.IssueSource != "github" {
				t.Errorf("historical week = %+v", w)
			}
			if w := got[1]; w.Opened["human"] != tc.boundaryHuman || w.Opened["agent"] != tc.boundaryAgent || w.Closed != tc.boundaryHuman || w.N != 2*tc.boundaryHuman+tc.boundaryAgent || w.IssueSource != "github" {
				t.Errorf("boundary week = %+v", w)
			}
			if src.IssueSeriesWeeks != tc.seriesWeeks || src.AuditOnlyWeeks != tc.auditWeeks {
				t.Errorf("sources = %+v", src)
			}
		})
	}
}

func TestEvidenceIssueSeriesWindowAndFallback(t *testing.T) {
	start := time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC)
	w := historicalIssueWeek(start)
	in := evidenceWorkBalanceInputs{
		Series:  []evidence.IssueSeriesWeek{{Repo: "o/r", IssueWeek: w}},
		Entries: []AuditEntry{{Timestamp: start.Add(time.Hour).Format(time.RFC3339), Action: ghpkg.AuditActionAgentIssueCreated, Repo: "o/r", Target: 1}},
	}
	win := evidenceWindow{Custom: true, From: start.Add(time.Hour), To: start.Add(2 * time.Hour)}
	got, _ := buildEvidenceWorkBalance(in, win, "o/r")
	if len(got) != 1 || got[0].Opened["human"] != 1 || got[0].Closed != 0 || got[0].N != 1 {
		t.Fatalf("custom window must exclude its end: %+v", got)
	}
	// A partial backfill can satisfy a partial request, but must not replace
	// a full week's audit counts with an incomplete history.
	in.Series[0].CoveredFrom = win.From
	in.Series[0].CoveredTo = win.To
	got, src := buildEvidenceWorkBalance(in, win, "o/r")
	if len(got) != 1 || got[0].Opened["human"] != 1 || src.IssueSeriesWeeks != 1 {
		t.Fatalf("covered partial window = %+v, %+v", got, src)
	}
	got, src = buildEvidenceWorkBalance(in, evidenceWindow{To: w.CoveredTo}, "o/r")
	if len(got) != 1 || got[0].Opened["agent"] != 1 || got[0].Opened["human"] != 0 || src.AuditOnlyWeeks != 1 || src.IssueSeriesWeeks != 0 {
		t.Fatalf("insufficient coverage must retain audit: %+v, %+v", got, src)
	}
	// Empty observed weeks stay visible and suppress duplicate audit data.
	w.Events = []evidence.IssueEvent{}
	w.Opened, w.Closed = map[string]int{}, 0
	in.Series[0].IssueWeek = w
	got, src = buildEvidenceWorkBalance(in, evidenceWindow{To: w.CoveredTo}, "o/r")
	if len(got) != 1 || got[0].N != 0 || got[0].IssueSource != "github" || src.AuditOnlyWeeks != 0 {
		t.Fatalf("observed zero = %+v, %+v", got, src)
	}
	// In the all-repo view only the covered repo's audit entries are removed.
	in.Entries = append(in.Entries, AuditEntry{Timestamp: start.Add(time.Hour).Format(time.RFC3339), Action: ghpkg.AuditActionIssueClosed, Repo: "x/y", Target: 2})
	got, src = buildEvidenceWorkBalance(in, evidenceWindow{To: w.CoveredTo}, "")
	if len(got) != 1 || got[0].N != 1 || got[0].Closed != 1 || got[0].IssueSource != "audit" || src.AuditOnlyWeeks != 1 {
		t.Fatalf("mixed repo coverage = %+v, %+v", got, src)
	}
}

func TestHandleEvidenceSummaryHistoricalIssueSeries(t *testing.T) {
	reviewEvidenceTestEnv(t)
	previousRollup := issueActivityPath
	issueActivityPath = t.TempDir() + "/missing.json"
	t.Cleanup(func() { issueActivityPath = previousRollup })
	start := time.Date(2026, 1, 26, 0, 0, 0, 0, time.UTC)
	w := historicalIssueWeek(start)
	for _, repo := range []string{"o/r", "x/y"} {
		if err := evidence.WriteIssueWeek(ghpkg.ReviewEvidenceRoot, repo, w); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{}
	response := httptest.NewRecorder()
	s.handleEvidenceSummary(response, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?hours=0&repo=o/r", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status %d: %s", response.Code, response.Body)
	}
	var dto EvidenceSummary
	if err := json.Unmarshal(response.Body.Bytes(), &dto); err != nil {
		t.Fatal(err)
	}
	if len(dto.WorkBalance) != 1 || dto.WorkBalance[0].N != 2 || dto.WorkBalanceSources.IssueSeriesWeeks != 1 || dto.Missing.IssuesHumanOpensUnobserved {
		t.Fatalf("historical summary = %+v", dto)
	}
	if dto.Missing.RecordsUnreadable != 0 {
		t.Fatalf("issue series must not be counted as unreadable review bundles: %+v", dto.Missing)
	}
	path, _ := evidence.IssueWeekPath(ghpkg.ReviewEvidenceRoot, "o/r", w.Week)
	if err := os.WriteFile(path, []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded := evidence.LoadIssueSeries(ghpkg.ReviewEvidenceRoot)
	if len(loaded) != 1 || loaded[0].Repo != "x/y" {
		t.Fatalf("malformed file must not hide other repos: %+v", loaded)
	}
}
