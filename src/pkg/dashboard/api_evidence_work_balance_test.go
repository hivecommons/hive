package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// TestBuildEvidenceWorkBalance pins panel 6 (#12366): audited issue opens are
// split by actor kind per ISO week against audited closes, the window and
// repo filters follow the Throughput contract, and non-issue audit actions
// are ignored.
func TestBuildEvidenceWorkBalance(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) // 2026-W42
	wk42 := now.Add(-2 * time.Hour)
	wk41 := now.Add(-8 * 24 * time.Hour)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	entries := []AuditEntry{
		{Timestamp: ts(wk42), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=o/r, number=1"},
		{Timestamp: ts(wk42.Add(-time.Hour)), Action: ghpkg.AuditActionAgentIssueCreated, Repo: "o/r", Target: 2},
		{Timestamp: ts(wk42), Action: ghpkg.AuditActionHiveIssueCreated, Detail: "repo=o/r, number=3"},
		{Timestamp: ts(wk42), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=1, actor_class=human"},
		// a PR create is not issue work
		{Timestamp: ts(wk42), Action: ghpkg.AuditActionAgentPRCreated, Detail: "repo=o/r, number=4"},
		// another repo
		{Timestamp: ts(wk42), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=x/y, number=5"},
		// previous week
		{Timestamp: ts(wk41), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=6"},
		// unparseable timestamp and future entry are dropped
		{Timestamp: "nope", Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=7"},
		{Timestamp: ts(now.Add(time.Hour)), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=8"},
	}

	trailing := func(hours int) evidenceWindow {
		w := evidenceWindow{Hours: hours, To: now}
		if hours > 0 {
			w.From = now.Add(-time.Duration(hours) * time.Hour)
		}
		return w
	}
	got := buildEvidenceWorkBalance(entries, trailing(24*14), "o/r")
	if len(got) != 2 {
		t.Fatalf("work_balance = %+v", got)
	}
	if w := got[0]; w.Week != "2026-W41" || w.N != 1 || w.Closed != 1 || w.Opened["agent"] != 0 || w.Throttled {
		t.Errorf("W41 = %+v", w)
	}
	w := got[1]
	if w.Week != "2026-W42" || w.N != 4 || w.Closed != 1 || w.Opened["agent"] != 2 || w.Opened["bot"] != 1 || w.Opened["human"] != 0 {
		t.Errorf("W42 = %+v", w)
	}
	if _, ok := w.Opened["human"]; !ok {
		t.Error("opened must carry every kind, including a zero human column")
	}

	if all := buildEvidenceWorkBalance(entries, trailing(0), ""); len(all) != 2 || all[1].Opened["agent"] != 3 {
		t.Errorf("all-time, all repos = %+v", all)
	}
	if day := buildEvidenceWorkBalance(entries, trailing(24), "o/r"); len(day) != 1 || day[0].Week != "2026-W42" {
		t.Errorf("24h window = %+v", day)
	}
	// custom from/to range: only W41 (the end bound is exclusive)
	custom := evidenceWindow{Hours: 7 * 24, From: wk41.Add(-24 * time.Hour), To: wk41.Add(24 * time.Hour), Custom: true}
	if c := buildEvidenceWorkBalance(entries, custom, "o/r"); len(c) != 1 || c[0].Week != "2026-W41" || c[0].Closed != 1 {
		t.Errorf("custom range = %+v", c)
	}
	if c := buildEvidenceWorkBalance(entries, evidenceWindow{Hours: 1, From: wk41.Add(-time.Hour), To: wk41, Custom: true}, "o/r"); len(c) != 0 {
		t.Errorf("custom range end must be exclusive, got %+v", c)
	}
	if e := buildEvidenceWorkBalance(nil, trailing(24), ""); e == nil || len(e) != 0 {
		t.Errorf("empty input must yield an empty (non-null) slice, got %#v", e)
	}
}

// TestHandleEvidenceSummaryWorkBalanceFromRing checks the summary handler
// reads work_balance from the dashboard audit ring and discloses the
// unobserved human opens and missing throttle windows.
func TestHandleEvidenceSummaryWorkBalanceFromRing(t *testing.T) {
	reviewEvidenceTestEnv(t)
	audit := &AuditLog{}
	audit.Log("system", ghpkg.AuditActionAgentIssueCreated, "repo=o/r, number=1", "")
	audit.Log("system", ghpkg.AuditActionIssueClosed, "repo=o/r, number=1", "")
	s := &Server{audit: audit}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?hours=24&repo=o/r", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp struct {
		WorkBalance []EvidenceWorkBalanceWeek  `json:"work_balance"`
		Missing     map[string]json.RawMessage `json:"missing"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.WorkBalance) != 1 || resp.WorkBalance[0].Opened["agent"] != 1 || resp.WorkBalance[0].Closed != 1 {
		t.Errorf("work_balance = %+v", resp.WorkBalance)
	}
	for _, key := range []string{"issues_human_opens_unobserved", "throttle_windows_unavailable"} {
		if string(resp.Missing[key]) != "true" {
			t.Errorf("missing.%s = %s, want true", key, resp.Missing[key])
		}
	}
}
