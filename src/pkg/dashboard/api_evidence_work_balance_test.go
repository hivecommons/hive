package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/hivestate"
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
	build := func(entries []AuditEntry, win evidenceWindow, repo string) []EvidenceWorkBalanceWeek {
		out, _ := buildEvidenceWorkBalance(evidenceWorkBalanceInputs{Entries: entries}, win, repo)
		return out
	}
	got := build(entries, trailing(24*14), "o/r")
	if len(got) != 2 {
		t.Fatalf("work_balance = %+v", got)
	}
	if w := got[0]; w.Week != "2026-W41" || w.N != 1 || w.Closed != 1 || w.Opened["agent"] != 0 || w.Throttled || w.IssueSource != "audit" {
		t.Errorf("W41 = %+v", w)
	}
	w := got[1]
	if w.Week != "2026-W42" || w.N != 4 || w.Closed != 1 || w.Opened["agent"] != 2 || w.Opened["bot"] != 1 || w.Opened["human"] != 0 {
		t.Errorf("W42 = %+v", w)
	}
	if _, ok := w.Opened["human"]; !ok {
		t.Error("opened must carry every kind, including a zero human column")
	}

	if all := build(entries, trailing(0), ""); len(all) != 2 || all[1].Opened["agent"] != 3 {
		t.Errorf("all-time, all repos = %+v", all)
	}
	if day := build(entries, trailing(24), "o/r"); len(day) != 1 || day[0].Week != "2026-W42" {
		t.Errorf("24h window = %+v", day)
	}
	// custom from/to range: only W41 (the end bound is exclusive)
	custom := evidenceWindow{Hours: 7 * 24, From: wk41.Add(-24 * time.Hour), To: wk41.Add(24 * time.Hour), Custom: true}
	if c := build(entries, custom, "o/r"); len(c) != 1 || c[0].Week != "2026-W41" || c[0].Closed != 1 {
		t.Errorf("custom range = %+v", c)
	}
	if c := build(entries, evidenceWindow{Hours: 1, From: wk41.Add(-time.Hour), To: wk41, Custom: true}, "o/r"); len(c) != 0 {
		t.Errorf("custom range end must be exclusive, got %+v", c)
	}
	if e := build(nil, trailing(24), ""); e == nil || len(e) != 0 {
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

// TestBuildEvidenceWorkBalanceGitHubTruth pins the rework for #12517: issue
// opens and closes come from the GitHub rollup (every author) for weeks the
// rollup covers and from the audit log before that, PR opens/merges/closes
// come from the folded evidence bundles by author kind, and each week
// carries the governor's last queue-depth sample.
func TestBuildEvidenceWorkBalanceGitHubTruth(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) // Thu 2026-W42 (Mon 10-12)
	w42 := time.Date(2026, 10, 13, 9, 0, 0, 0, time.UTC)
	w41 := time.Date(2026, 10, 6, 9, 0, 0, 0, time.UTC)
	w40 := time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	ts := func(t time.Time) string { return t.Format(time.RFC3339) }
	rollup := IssueActivityRollup{
		Since:     time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), // covers W41 and W42 fully, not W40
		FetchedAt: now,
		Issues: map[string]ghpkg.IssueActivity{
			"o/r#1": {Repo: "o/r", Number: 1, Kind: "human", CreatedAt: w42},
			"o/r#2": {Repo: "o/r", Number: 2, Kind: "bot", CreatedAt: w41, ClosedAt: w42.Add(time.Hour)},
			// audited as an agent create, so agent even though GitHub says bot
			"o/r#3": {Repo: "o/r", Number: 3, Kind: "bot", CreatedAt: w42},
			// opened before coverage: its open is not counted, its close is
			"o/r#4": {Repo: "o/r", Number: 4, Kind: "human", CreatedAt: w40, ClosedAt: w41},
			"x/y#5": {Repo: "x/y", Number: 5, Kind: "human", CreatedAt: w42},
		},
	}
	entries := []AuditEntry{
		{Timestamp: ts(w42), Action: ghpkg.AuditActionAgentIssueCreated, Repo: "o/r", Target: 3},
		// audit-only week W40: counted from the audit log
		{Timestamp: ts(w40), Action: ghpkg.AuditActionAgentIssueCreated, Detail: "repo=o/r, number=9"},
		{Timestamp: ts(w40), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=9"},
		// covered week: the audit log must NOT double count
		{Timestamp: ts(w41), Action: ghpkg.AuditActionIssueClosed, Detail: "repo=o/r, number=4"},
	}
	bundle := func(n int, kind string, opened time.Time) *evidence.Bundle {
		return &evidence.Bundle{Repo: "o/r", Number: n, Author: evidence.Author{Login: "a", Kind: kind}, CreatedAt: opened, UpdatedAt: opened}
	}
	merged := bundle(10, evidence.AuthorAgent, w41)
	merged.Merge = &evidence.MergeEvent{At: w42, OpenedAt: w41}
	closed := bundle(11, evidence.AuthorHuman, w41)
	closed.Backfilled = true
	closed.Backfill = &evidence.Backfill{State: evidence.BackfillStateClosed, ClosedAt: w42}
	open := bundle(12, evidence.AuthorBot, w42)
	prs := evidence.FoldPRs([]*evidence.Bundle{merged, closed, open}, "", "")
	buckets := []hivestate.Bucket{
		{Start: w41, Last: w41.Add(time.Hour), Samples: 2, QueueIssues: []int{30, 35}, QueuePRs: []int{10, 12}},
		{Start: w41.Add(2 * 24 * time.Hour), Last: w41.Add(2*24*time.Hour + time.Hour), Samples: 1, QueueIssues: []int{38}, QueuePRs: []int{18}},
		{Start: w42, Last: w42.Add(time.Hour), Samples: 1, QueueIssues: []int{40}, QueuePRs: []int{20}},
	}
	win := evidenceWindow{Hours: 24 * 21, From: now.Add(-21 * 24 * time.Hour), To: now}
	got, src := buildEvidenceWorkBalance(evidenceWorkBalanceInputs{Entries: entries, Rollup: rollup, PRs: prs, Buckets: buckets}, win, "o/r")
	if len(got) != 3 {
		t.Fatalf("weeks = %+v", got)
	}
	if w := got[0]; w.Week != "2026-W40" || w.IssueSource != "audit" || w.Opened["agent"] != 1 || w.Closed != 1 {
		t.Errorf("W40 (audit-only) = %+v", w)
	}
	if w := got[1]; w.Week != "2026-W41" || w.IssueSource != "github" || w.Opened["bot"] != 1 || w.Opened["human"] != 0 || w.Closed != 1 ||
		w.PRsOpened["agent"] != 1 || w.PRsOpened["human"] != 1 || w.PRsMerged != 0 || w.QueueIssues == nil || *w.QueueIssues != 38 || *w.QueuePRs != 18 {
		t.Errorf("W41 = %+v", w)
	}
	w := got[2]
	if w.Week != "2026-W42" || w.IssueSource != "github" || w.Opened["human"] != 1 || w.Opened["agent"] != 1 || w.Opened["bot"] != 0 || w.Closed != 1 ||
		w.PRsOpened["bot"] != 1 || w.PRsMerged != 1 || w.PRsClosed != 1 || w.QueueIssues == nil || *w.QueueIssues != 40 || *w.QueuePRs != 20 {
		t.Errorf("W42 = %+v", w)
	}
	if w.N != 1+1+1+1+1+1 {
		t.Errorf("W42 n = %d, want every open/close/merge event counted once", w.N)
	}
	if src.IssuesGitHubSince != "2026-10-05T00:00:00Z" || src.AuditOnlyWeeks != 1 || src.QueueSamples != 4 || src.IssuesKnown != 5 {
		t.Errorf("sources = %+v", src)
	}
}

// TestMergeIssueActivity pins the durable rollup: newer records win, Since is
// first fetch minus the lookback and never moves later, and only closed
// issues age out.
func TestMergeIssueActivity(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	r := mergeIssueActivity(IssueActivityRollup{}, []ghpkg.IssueActivity{
		{Repo: "o/r", Number: 1, Kind: "human", CreatedAt: now.Add(-time.Hour)},
		{Repo: "o/r", Number: 2, Kind: "bot", CreatedAt: now.Add(-400 * 24 * time.Hour)},
		{Repo: "o/r", Number: 3, Kind: "bot", CreatedAt: now.Add(-400 * 24 * time.Hour), ClosedAt: now.Add(-200 * 24 * time.Hour)},
	}, []string{"o/r"}, now)
	if !r.Since.Equal(now.Add(-issueActivityLookback)) || len(r.Issues) != 2 || r.Issues["o/r#2"].Number != 2 {
		t.Errorf("first merge = %+v", r)
	}
	later := now.Add(2 * time.Hour)
	r = mergeIssueActivity(r, []ghpkg.IssueActivity{{Repo: "o/r", Number: 1, Kind: "human", CreatedAt: now.Add(-time.Hour), ClosedAt: later}}, []string{"o/r", "x/y"}, later)
	if !r.Since.Equal(now.Add(-issueActivityLookback)) || r.Issues["o/r#1"].ClosedAt.IsZero() || len(r.Repos) != 2 || !r.FetchedAt.Equal(later) {
		t.Errorf("second merge = %+v", r)
	}
	path := filepath.Join(t.TempDir(), "sub", "issue-activity.json")
	if err := saveIssueActivityRollup(path, r); err != nil {
		t.Fatal(err)
	}
	if back := loadIssueActivityRollup(path); len(back.Issues) != 2 || !back.Since.Equal(r.Since) {
		t.Errorf("round trip = %+v", back)
	}
	if !issueActivityDue(path, later.Add(issueActivityRefresh)) || issueActivityDue(path, later.Add(time.Minute)) {
		t.Error("refresh cadence not honoured")
	}
	if missing := loadIssueActivityRollup(filepath.Join(t.TempDir(), "none.json")); missing.Issues == nil || !missing.Since.IsZero() {
		t.Errorf("missing file = %+v", missing)
	}
}

// TestIssueActivityAuthorKind pins the issue author classification.
func TestIssueActivityAuthorKind(t *testing.T) {
	cases := []struct{ login, typ, body, want string }{
		{"alice", "User", "plain", "human"},
		{"dependabot[bot]", "Bot", "bump", "bot"},
		{"hive-app[bot]", "Bot", "x\n\n---\n🐝 **Hive Agent**: `scanner` | **SHA:** `abc`", "agent"},
	}
	for _, c := range cases {
		if got := ghpkg.IssueActivityAuthorKind(c.login, c.typ, c.body); got != c.want {
			t.Errorf("%s/%s = %q, want %q", c.login, c.typ, got, c.want)
		}
	}
}
