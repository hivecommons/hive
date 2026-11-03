package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
	"github.com/hivecommons/hive/pkg/evidence/complexity"
	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

func evSummaryBundle(repo string, n int, kind string, created time.Time, merged bool, verdicts []evidence.Verdict, sentinel int, ci bool) *evidence.Bundle {
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, Repo: repo, Number: n, HeadSHA: "abc1234",
		Author: evidence.Author{Login: "x", Kind: kind}, CreatedAt: created, UpdatedAt: created,
		Verdicts: verdicts,
	}
	if merged {
		b.Merge = &evidence.MergeEvent{Actor: "a", Method: "squash", SHA: "m"}
	}
	for i := 0; i < sentinel; i++ {
		b.Sentinel = append(b.Sentinel, evidence.SentinelFinding{Rule: "r"})
	}
	if ci {
		b.CI.Checks = []evidence.Check{{Name: "build", Conclusion: "success"}}
	}
	return b
}

func TestBuildEvidenceSummary(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	bundles := []*evidence.Bundle{
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent.Add(-time.Hour), false, nil, 0, true),
		// newer bundle for the same PR supersedes the one above
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, []evidence.Verdict{
			{Perspective: "security", Model: "m1", Verdict: "approve", Confidence: 0.95},
			{Perspective: "security", Model: "", Verdict: "changes_requested", Confidence: 0.3},
		}, 2, true),
		evSummaryBundle("a/b", 2, evidence.AuthorHuman, recent, false, []evidence.Verdict{
			{Perspective: "security", Model: "m1", Verdict: "approve", Confidence: 0.6},
		}, 0, false),
		evSummaryBundle("c/d", 3, evidence.AuthorBot, old, true, nil, 1, true),
	}

	got := buildEvidenceSummary(bundles, now, 24, "", "")
	if got.N != 2 {
		t.Fatalf("n = %d, want 2", got.N)
	}
	if o := got.Outcomes["agent"]; o.N != 1 || o.Merged != 1 || o.Open != 0 {
		t.Errorf("agent outcomes = %+v", o)
	}
	if o := got.Outcomes["human"]; o.N != 1 || o.Open != 0 || o.Unknown != 1 {
		t.Errorf("live-only bundle without a merge is an unknown outcome, not open: %+v", o)
	}
	if got.Missing.VerdictsModelUnknown != 1 || got.Missing.RecordsEmptyCI != 1 || got.Missing.PRsOutcomeUnknown != 1 {
		t.Errorf("missing = %+v", got.Missing)
	}
	if got.SentinelPerWeek.N != 2 || len(got.SentinelPerWeek.Weeks) != 1 || got.SentinelPerWeek.Weeks[0].Week != "2026-W42" {
		t.Errorf("sentinel = %+v", got.SentinelPerWeek)
	}
	if len(got.Calibration) != 1 || got.Calibration[0].N != 3 {
		t.Fatalf("calibration = %+v", got.Calibration)
	}
	bk := got.Calibration[0].Buckets
	if bk[0].N != 1 || bk[0].Blocks != 1 || bk[1].N != 1 || bk[1].Unknown != 1 || bk[4].N != 1 || bk[4].Merged != 1 || bk[4].Unknown != 0 {
		t.Errorf("buckets = %+v", bk)
	}
	if m1 := got.ReviewerModels[0]; m1.Model != "m1" || m1.ByAuthor["human"].Approvals != 1 || m1.ByAuthor["human"].ApprovedNotMerged != 0 || m1.ByAuthor["human"].ApprovedUnknown != 1 {
		t.Errorf("approval on an unknown-outcome PR is unknown, not a false approve: %+v", m1)
	}
	if len(got.ReviewerModels) != 2 {
		t.Fatalf("models = %+v", got.ReviewerModels)
	}
	unk := got.ReviewerModels[1]
	if unk.Model != "unknown" || unk.ByAuthor["agent"].MergedAfterBlock != 1 {
		t.Errorf("unknown model = %+v", unk)
	}
	if got.ShadowGate.WouldHaveHeld != 0 {
		t.Errorf("shadow gate = %+v", got.ShadowGate)
	}

	if all := buildEvidenceSummary(bundles, now, 0, "c/d", ""); all.N != 1 || !all.AllTime {
		t.Errorf("all-time repo filter: %+v", all)
	}
	if k := buildEvidenceSummary(bundles, now, 0, "", "human"); k.N != 1 || k.Outcomes["agent"].N != 0 {
		t.Errorf("author_kind filter: %+v", k)
	}
}

func TestHandleEvidenceSummaryValidation(t *testing.T) {
	s := &Server{}
	for _, q := range []string{"hours=-1", "hours=x", "repo=nope", "author_kind=robot",
		"to=2026-10-15", "from=yesterday", "from=2026-10-15&to=2026-10-01", "from=2022-01-01&to=2026-10-15", "from=2026-10-01&to=bogus"} {
		w := httptest.NewRecorder()
		s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?"+q, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
}

func TestHandleEvidenceSummaryEmpty(t *testing.T) {
	reviewEvidenceTestEnv(t)
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.N != 0 || resp.Hours != 24 {
		t.Errorf("resp = %+v", resp)
	}
}

func TestLoadEvidenceWriteCounters(t *testing.T) {
	dir := t.TempDir()
	good := filepath.Join(dir, "wc.json")
	body := `{"totals":{"observe":{"attempted":5,"blocked":2,"blocked_by_reason":{"x":2}}},"weeks":{}}`
	if err := os.WriteFile(good, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got := loadEvidenceWriteCounters(good)
	if o := got["observe"]; o.Attempted != 5 || o.Blocked != 2 || o.BlockedByReason["x"] != 2 {
		t.Errorf("got %+v", got)
	}
	bad := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(bad, []byte("{nope"), 0o600); err != nil {
		t.Fatal(err)
	}
	if loadEvidenceWriteCounters(bad) != nil || loadEvidenceWriteCounters(filepath.Join(dir, "missing.json")) != nil {
		t.Error("unreadable or absent file must yield nil")
	}
}

func TestHandleEvidenceSummaryIntentCounters(t *testing.T) {
	reviewEvidenceTestEnv(t)
	dir := t.TempDir()
	origW, origI := writeCountersPath, intentCountersPath
	t.Cleanup(func() { writeCountersPath, intentCountersPath = origW, origI })
	writeCountersPath = filepath.Join(dir, "write-counters.json")
	intentCountersPath = filepath.Join(dir, "intent-counters.json")

	s := &Server{}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["intent_counters"]; ok {
		t.Error("intent_counters must be omitted when the file is absent")
	}

	body := `{"totals":{"tier1":{"attempted":7,"blocked":3,"blocked_by_reason":{"tier1_needs_issue":3}},"tier3":{"attempted":2,"blocked":2}},"weeks":{}}`
	if err := os.WriteFile(intentCountersPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	w = httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil))
	var resp EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if t1 := resp.IntentCounters["tier1"]; t1.Attempted != 7 || t1.Blocked != 3 || resp.IntentCounters["tier3"].Blocked != 2 {
		t.Errorf("intent_counters = %+v", resp.IntentCounters)
	}
	if resp.WriteCounters != nil {
		t.Errorf("write_counters should be absent, got %+v", resp.WriteCounters)
	}
}

func TestBuildEvidenceSummaryShadowGate(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	yes, no := true, false
	withShadow := func(b *evidence.Bundle, held, blocked *bool) *evidence.Bundle {
		b.Shadow = &evidence.Shadow{}
		if held != nil {
			b.Shadow.ReviewGate = &evidence.ShadowGate{WouldHaveHeld: held, Reason: "r", EvaluatedAt: recent}
		}
		if blocked != nil {
			b.Shadow.Intent = &evidence.ShadowGate{WouldHaveBlocked: blocked, Tier: "observe", EvaluatedAt: recent}
		}
		return b
	}
	reverted := withShadow(evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, nil, 0, true), &yes, &yes)
	reverted.RevertedBy = []string{"a/b#9@def"}
	bundles := []*evidence.Bundle{
		reverted,
		withShadow(evSummaryBundle("a/b", 2, evidence.AuthorAgent, recent, false, nil, 0, true), &yes, nil),
		withShadow(evSummaryBundle("a/b", 3, evidence.AuthorHuman, recent, true, nil, 0, true), &no, &no),
		// no shadow observation at all
		evSummaryBundle("a/b", 4, evidence.AuthorHuman, recent, true, nil, 0, true),
		// outside the 24h window
		withShadow(evSummaryBundle("a/b", 5, evidence.AuthorAgent, now.Add(-48*time.Hour), true, nil, 0, true), &yes, &yes),
		// another repo
		withShadow(evSummaryBundle("c/d", 6, evidence.AuthorAgent, recent, true, nil, 0, true), &yes, nil),
	}

	sg := buildEvidenceSummary(bundles, now, 24, "a/b", "").ShadowGate
	if sg.N != 3 || sg.WouldHaveHeld != 2 || sg.WouldHaveBlocked != 1 {
		t.Fatalf("shadow gate = %+v", sg)
	}
	// PR 2 is a live bundle without a merge record: its outcome is unknown,
	// not "open", so it is counted but never enters a rate.
	if want := (EvidenceShadowOutcome{N: 2, Merged: 1, Unknown: 1, Reverted: 1}); sg.ReviewGate.Held != want || sg.ReviewGate.N != 3 {
		t.Errorf("review gate = %+v", sg.ReviewGate)
	}
	if want := (EvidenceShadowOutcome{N: 1, Merged: 1}); sg.ReviewGate.Passed != want {
		t.Errorf("review gate passed = %+v", sg.ReviewGate.Passed)
	}
	if want := (EvidenceShadowOutcome{N: 1, Merged: 1, Reverted: 1}); sg.Intent.Held != want || sg.Intent.N != 2 || sg.Intent.Passed.N != 1 {
		t.Errorf("intent = %+v", sg.Intent)
	}

	if k := buildEvidenceSummary(bundles, now, 24, "a/b", "human").ShadowGate; k.N != 1 || k.WouldHaveHeld != 0 || k.ReviewGate.Passed.N != 1 {
		t.Errorf("author_kind filter: %+v", k)
	}
	if z := buildEvidenceSummary(bundles[3:4], now, 24, "", "").ShadowGate; z != (EvidenceShadowGate{}) {
		t.Errorf("no shadow bundles must yield zero shadow gate, got %+v", z)
	}
}

// TestBuildEvidenceSummaryClaimVsVerdict pins panel 5 (#12365): weekly rows
// of agent-authored PRs, CI disagreement only over records with captured
// checks, swarm disagreement over any non-approve verdict, and empty-CI
// records disclosed rather than counted as agreement.
func TestBuildEvidenceSummaryClaimVsVerdict(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC) // 2026-W42
	wk42 := now.Add(-2 * time.Hour)
	wk41 := now.Add(-8 * 24 * time.Hour)
	approve := []evidence.Verdict{{Perspective: "security", Model: "m", Verdict: "approve", Confidence: 0.9}}
	mixed := []evidence.Verdict{
		{Perspective: "security", Model: "m", Verdict: "approve", Confidence: 0.9},
		{Perspective: "quality", Model: "m", Verdict: "changes_requested", Confidence: 0.6},
	}
	redCI := func(b *evidence.Bundle) *evidence.Bundle {
		b.CI.Checks = append(b.CI.Checks, evidence.Check{Name: "lint", Conclusion: "failure"})
		return b
	}
	skippedCI := func(b *evidence.Bundle) *evidence.Bundle {
		b.CI.Checks = append(b.CI.Checks, evidence.Check{Name: "e2e", Conclusion: "skipped"}, evidence.Check{Name: "docs", Conclusion: "neutral"})
		return b
	}
	pendingCI := func(b *evidence.Bundle) *evidence.Bundle {
		b.CI.Checks = append(b.CI.Checks, evidence.Check{Name: "e2e", Conclusion: "in_progress"})
		return b
	}
	gateCI := func(b *evidence.Bundle, status string) *evidence.Bundle {
		b.CI.Status = status
		return skippedCI(b)
	}
	bundles := []*evidence.Bundle{
		// W42: CI green, swarm approve → claimed only
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, wk42, true, approve, 0, true),
		// W42: CI red, swarm approve → ci_not_green
		redCI(evSummaryBundle("a/b", 2, evidence.AuthorAgent, wk42, false, approve, 0, true)),
		// W42: CI red, swarm changes_requested → both
		redCI(evSummaryBundle("a/b", 3, evidence.AuthorAgent, wk42, false, mixed, 0, true)),
		// W42: empty CI, swarm changes_requested → verdict only; CI column excludes it
		evSummaryBundle("a/b", 4, evidence.AuthorAgent, wk42, false, mixed, 0, false),
		// W42: human-authored, CI red → not an agent claim
		redCI(evSummaryBundle("a/b", 5, evidence.AuthorHuman, wk42, false, mixed, 0, true)),
		// W41: no verdicts, CI green → claimed only
		evSummaryBundle("a/b", 6, evidence.AuthorAgent, wk41, true, nil, 0, true),
		// W41: superseded bundle for #6 with red CI must not count
		redCI(evSummaryBundle("a/b", 6, evidence.AuthorAgent, wk41.Add(-time.Hour), false, nil, 0, true)),
		// W41: a skipped job beside a passing one is green for the gate
		skippedCI(evSummaryBundle("a/b", 7, evidence.AuthorAgent, wk41, false, approve, 0, true)),
		// W41: a check still running when captured is pending: in neither column
		pendingCI(evSummaryBundle("a/b", 8, evidence.AuthorAgent, wk41, false, approve, 0, true)),
		// W41: the gate's own verdict wins over a skipped-looking check list
		gateCI(evSummaryBundle("a/b", 9, evidence.AuthorAgent, wk41, false, approve, 0, true), "failure"),
	}

	got := buildEvidenceSummary(bundles, now, 24*14, "", "")
	if len(got.ClaimVsVerdict) != 2 {
		t.Fatalf("claim_vs_verdict = %+v", got.ClaimVsVerdict)
	}
	if want := (EvidenceClaimWeek{Week: "2026-W41", N: 4, Claimed: 4, CICaptured: 3, CINotGreen: 1, CIPending: 1}); got.ClaimVsVerdict[0] != want {
		t.Errorf("W41 = %+v, want %+v (skipped/neutral are green, in_progress is pending, ci.status wins)", got.ClaimVsVerdict[0], want)
	}
	if got.Missing.RecordsCIPending != 1 {
		t.Errorf("records_ci_pending = %d, want 1", got.Missing.RecordsCIPending)
	}
	if want := (EvidenceClaimWeek{Week: "2026-W42", N: 4, Claimed: 4, CICaptured: 3, CINotGreen: 2, VerdictNotApprove: 2, Both: 1}); got.ClaimVsVerdict[1] != want {
		t.Errorf("W42 = %+v, want %+v", got.ClaimVsVerdict[1], want)
	}
	if got.Missing.RecordsEmptyCI != 1 {
		t.Errorf("records_empty_ci = %d, want 1", got.Missing.RecordsEmptyCI)
	}

	if h := buildEvidenceSummary(bundles, now, 0, "", "human").ClaimVsVerdict; len(h) != 0 {
		t.Errorf("author_kind=human must yield no agent claims, got %+v", h)
	}
	if e := buildEvidenceSummary(nil, now, 24, "", "").ClaimVsVerdict; e == nil || len(e) != 0 {
		t.Errorf("empty input must yield an empty (non-null) slice, got %#v", e)
	}
}

// TestParseEvidenceWindow pins the 24h/7d/30d/custom window contract
// (#12062): hours up to the throughput cap, and from/to as RFC3339 or
// YYYY-MM-DD with an inclusive date-only `to`.
func TestParseEvidenceWindow(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	q := func(s string) map[string][]string {
		u, err := url.ParseQuery(s)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	win, err := parseEvidenceWindow(q("hours=720"), now)
	if err != nil || win.Hours != 720 || win.Custom || !win.From.Equal(now.Add(-720*time.Hour)) {
		t.Fatalf("hours=720: %+v %v", win, err)
	}
	win, err = parseEvidenceWindow(q("from=2026-10-01&to=2026-10-15"), now)
	if err != nil || !win.Custom || win.Hours != 15*24 ||
		!win.From.Equal(time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)) ||
		!win.To.Equal(time.Date(2026, 10, 16, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("date range: %+v %v", win, err)
	}
	win, err = parseEvidenceWindow(q("from=2026-10-15T06:00:00Z"), now)
	if err != nil || !win.Custom || !win.To.Equal(now) || win.Hours != 30 {
		t.Fatalf("rfc3339 open-ended: %+v %v", win, err)
	}
	// Backfilled history reaches back years (#12371): a 21-month range is fine.
	if win, err := parseEvidenceWindow(q("from=2025-01-01&to=2026-10-15"), now); err != nil || !win.Custom {
		t.Errorf("from=2025-01-01: %+v %v", win, err)
	}
	if _, err := parseEvidenceWindow(q("from=2022-10-01&to=2026-10-15"), now); err == nil {
		t.Errorf("range over %d days must be rejected", evidenceSummaryMaxRangeDays)
	}
	if _, err := parseEvidenceWindow(q("from=2026-10-15&to=2026-10-01"), now); err == nil {
		t.Error("to before from must be rejected")
	}
}

func TestBuildEvidenceSummaryCustomRange(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	bundles := []*evidence.Bundle{
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, time.Date(2026, 10, 2, 9, 0, 0, 0, time.UTC), true, nil, 0, true),
		evSummaryBundle("a/b", 2, evidence.AuthorHuman, time.Date(2026, 10, 9, 9, 0, 0, 0, time.UTC), false, nil, 0, true),
		evSummaryBundle("a/b", 3, evidence.AuthorHuman, time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC), false, nil, 0, true),
	}
	win := evidenceWindow{Custom: true, Hours: 7 * 24,
		From: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 12, 0, 0, 0, 0, time.UTC)}
	got := buildEvidenceSummaryWindow(bundles, win, "", "")
	if got.N != 1 || got.Outcomes["human"].N != 1 || got.AllTime {
		t.Fatalf("custom range: %+v", got)
	}
	if got.From != "2026-10-05T00:00:00Z" || got.To != "2026-10-12T00:00:00Z" {
		t.Errorf("from/to echo = %q %q", got.From, got.To)
	}
	// All-time still has no bounds.
	if all := buildEvidenceSummary(bundles, now, 0, "", ""); all.N != 3 || !all.AllTime || all.From != "" {
		t.Errorf("all time: %+v", all)
	}
}

func TestHandleEvidenceSummaryComplexityByRepo(t *testing.T) {
	reviewEvidenceTestEnv(t)
	root := ghpkg.ReviewEvidenceRoot
	// A bundle plus two monthly snapshots; the snapshots must neither count
	// as unreadable bundles nor be dropped from the summary.
	seedReviewEvidenceBundle(t, "abc1234", "", time.Now().UTC(), true)
	for i, m := range []string{"2026-03", "2026-04"} {
		snap := complexity.Snapshot{
			SchemaVersion: complexity.SchemaVersion, Repo: "acme/widget", Month: m, Ref: "main",
			Functions: 10 + i, MeanCyclo: 2.5, MedianCyclo: 2, P90Cyclo: 7 + i, MaxCyclo: 12,
			OverThreshold: i, Threshold: complexity.DefaultThreshold,
			Warnings: 3, WarningsByTool: map[string]int{"go vet": 3},
			Hotspots: []complexity.Function{{File: "a.go", Name: "F", Cyclo: 12}},
		}
		if _, err := complexity.Write(root, snap); err != nil {
			t.Fatal(err)
		}
	}
	s := &Server{}
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.N != 1 || resp.Missing.RecordsUnreadable != 0 {
		t.Errorf("snapshots must not be counted as bundles: n=%d unreadable=%d", resp.N, resp.Missing.RecordsUnreadable)
	}
	if len(resp.ComplexityByRepo) != 1 || resp.ComplexityByRepo[0].Repo != "acme/widget" {
		t.Fatalf("complexity_by_repo = %+v", resp.ComplexityByRepo)
	}
	months := resp.ComplexityByRepo[0].Months
	if len(months) != 2 || months[0].Month != "2026-03" || months[1].Month != "2026-04" {
		t.Fatalf("months = %+v", months)
	}
	last := months[1]
	if last.Functions != 11 || last.P90Cyclomatic != 8 || last.FunctionsOverThreshold != 1 || last.StaticWarningsByTool["go vet"] != 3 || last.Ref != "main" {
		t.Errorf("month = %+v", last)
	}
	if strings.Contains(w.Body.String(), `"hotspots"`) {
		t.Error("summary must not carry hotspot lists")
	}
}

func TestLoadEvidenceComplexityCapsMonths(t *testing.T) {
	root := t.TempDir()
	for i := 1; i <= evidenceComplexityMonths+2; i++ {
		snap := complexity.Snapshot{SchemaVersion: complexity.SchemaVersion, Repo: "o/r", Month: fmt.Sprintf("2024-%02d", i%12+1), Functions: i}
		if i > 12 {
			snap.Month = fmt.Sprintf("2025-%02d", i-12)
		}
		if _, err := complexity.Write(root, snap); err != nil {
			t.Fatal(err)
		}
	}
	got := loadEvidenceComplexity(root)
	if len(got) != 1 || len(got[0].Months) != evidenceComplexityMonths {
		t.Fatalf("got %+v", got)
	}
	if got[0].Months[evidenceComplexityMonths-1].Month != "2025-02" {
		t.Errorf("latest month = %s", got[0].Months[evidenceComplexityMonths-1].Month)
	}
	if loadEvidenceComplexity(filepath.Join(root, "absent")) != nil {
		t.Error("missing root must yield nil")
	}
}

// TestBuildEvidenceSummaryBackfilledWindows pins how a PR with both a live
// bundle (written when review started, no merge event because the hive did
// not merge) and a backfilled bundle (#12371: PR open time, forge outcome)
// is counted. Windows select on PR open time, never on when a record was
// written; the outcome comes from whichever record knows it; a backfilled
// close-without-merge is Closed, not Open; and available_windows reports
// how many PRs each preset window would hold.
func TestBuildEvidenceSummaryBackfilledWindows(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	opened := time.Date(2026, 9, 20, 9, 0, 0, 0, time.UTC)
	mergedAt := time.Date(2026, 9, 25, 9, 0, 0, 0, time.UTC)
	backfilled := func(b *evidence.Bundle, state string) *evidence.Bundle {
		b.Backfilled = true
		b.Backfill = &evidence.Backfill{Source: "github-api", At: now.Add(-time.Hour), State: state}
		b.UpdatedAt = now.Add(-time.Hour)
		return b
	}
	bf1 := backfilled(evSummaryBundle("a/b", 1, evidence.AuthorAgent, opened, true, nil, 0, false), evidence.BackfillStateMerged)
	bf1.Merge.At = mergedAt
	bf1.Merge.OpenedAt = opened
	bf1.RevertedBy = []string{"a/b#9"}
	// Live record for the same PR, written yesterday when the swarm reviewed
	// it: carries the verdict, knows nothing about the merge.
	live1 := evSummaryBundle("a/b", 1, evidence.AuthorAgent, now.Add(-22*time.Hour), false, []evidence.Verdict{
		{Perspective: "security", Model: "m1", Verdict: "approve", Confidence: 0.95},
	}, 1, false)
	live1.HeadSHA = "def5678"
	held := true
	live1.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &held}}
	closed2 := backfilled(evSummaryBundle("a/b", 2, evidence.AuthorHuman, time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC), false, nil, 0, false), evidence.BackfillStateClosed)
	closed2.Backfill.ClosedAt = time.Date(2026, 9, 29, 9, 0, 0, 0, time.UTC)
	old3 := backfilled(evSummaryBundle("a/b", 3, evidence.AuthorBot, time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC), true, nil, 0, false), evidence.BackfillStateMerged)
	bundles := []*evidence.Bundle{live1, bf1, closed2, old3}

	// 24h: the live record was written 22h ago but the PR opened in
	// September, so nothing is in the window.
	if got := buildEvidenceSummary(bundles, now, 24, "", ""); got.N != 0 {
		t.Fatalf("24h n = %d, want 0 (window must use PR open time, not record write time): %+v", got.N, got.Outcomes)
	}
	got := buildEvidenceSummary(bundles, now, 720, "", "")
	if got.N != 2 {
		t.Fatalf("30d n = %d, want 2", got.N)
	}
	if o := got.Outcomes["agent"]; o.N != 1 || o.Merged != 1 || o.Open != 0 {
		t.Errorf("agent outcome must take the merge from the backfilled record: %+v", o)
	}
	if o := got.Outcomes["human"]; o.N != 1 || o.Closed != 1 || o.Open != 0 {
		t.Errorf("backfilled close-without-merge must count as closed: %+v", o)
	}
	if len(got.ReviewerModels) != 1 || got.ReviewerModels[0].ByAuthor["agent"].ApprovedNotMerged != 0 || got.ReviewerModels[0].ByAuthor["agent"].ApprovedUnknown != 0 || got.ReviewerModels[0].ByAuthor["agent"].Approvals != 1 {
		t.Errorf("approval on a PR the forge says merged is not a false approve: %+v", got.ReviewerModels)
	}
	if got.Missing.PRsOutcomeUnknown != 0 || got.Outcomes["agent"].Unknown != 0 || got.Outcomes["human"].Unknown != 0 {
		t.Errorf("every PR here has a recorded outcome: %+v", got.Outcomes)
	}
	if len(got.Calibration) != 1 || got.Calibration[0].Buckets[4].Merged != 1 {
		t.Errorf("calibration must see the merge: %+v", got.Calibration)
	}
	if sg := got.ShadowGate; sg.N != 1 || sg.ReviewGate.Held.Merged != 1 || sg.ReviewGate.Held.Reverted != 1 || sg.ReviewGate.Held.Open != 0 {
		t.Errorf("shadow gate must see the merge and revert: %+v", sg)
	}
	if len(got.ClaimVsVerdict) != 1 || got.ClaimVsVerdict[0].Week != "2026-W38" {
		t.Errorf("claim week must follow PR open time: %+v", got.ClaimVsVerdict)
	}
	if got.SentinelPerWeek.N != 1 || got.SentinelPerWeek.Weeks[0].Week != "2026-W38" {
		t.Errorf("sentinel week must follow PR open time: %+v", got.SentinelPerWeek)
	}
	if all := buildEvidenceSummary(bundles, now, 0, "", ""); all.N != 3 || all.Outcomes["bot"].Merged != 1 {
		t.Errorf("all time: %+v", all)
	}
	win, err := parseEvidenceWindow(map[string][]string{"from": {"2025-01-01"}, "to": {"2026-10-16"}}, now)
	if err != nil {
		t.Fatal(err)
	}
	if custom := buildEvidenceSummaryWindow(bundles, win, "", ""); custom.N != 3 {
		t.Errorf("custom since 2025-01-01: n = %d, want 3", custom.N)
	}
	avail := evidenceAvailableWindows(bundles, now, "", "")
	if avail["24"] != 0 || avail["168"] != 0 || avail["720"] != 2 || avail["all"] != 3 {
		t.Errorf("available_windows = %v", avail)
	}
	if avail := evidenceAvailableWindows(bundles, now, "", evidence.AuthorBot); avail["720"] != 0 || avail["all"] != 1 {
		t.Errorf("available_windows with author_kind = %v", avail)
	}
}

// TestHandleEvidenceSummaryAvailableWindows pins that the handler surfaces
// available_windows alongside n.
func TestHandleEvidenceSummaryAvailableWindows(t *testing.T) {
	root := filepath.Join(reviewEvidenceTestEnv(t), "evidence")
	b := evSummaryBundle("a/b", 1, evidence.AuthorHuman, time.Now().UTC().Add(-20*24*time.Hour), true, nil, 0, true)
	b.Backfilled = true
	b.Backfill = &evidence.Backfill{Source: "github-api", At: time.Now().UTC(), State: evidence.BackfillStateMerged}
	dir := filepath.Join(root, "a", "b", "1")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(b)
	if err := os.WriteFile(filepath.Join(dir, "abc1234.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	s := &Server{}
	rec := httptest.NewRecorder()
	s.handleEvidenceSummary(rec, httptest.NewRequest(http.MethodGet, "/api/evidence/summary?hours=24", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var got EvidenceSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.N != 0 || got.AvailableWindows["24"] != 0 || got.AvailableWindows["720"] != 1 || got.AvailableWindows["all"] != 1 {
		t.Errorf("n=%d available_windows=%v", got.N, got.AvailableWindows)
	}
}

// TestBuildEvidenceSummaryUnknownOutcome pins the denominator policy behind
// every outcome rate the Evidence card draws (hivecommons/hive#12514
// follow-up). A PR that has only live bundles and no merge event has an
// unknown outcome: the hive records a merge only when it merged itself and
// never records a close, so such a PR may be open, merged by someone else
// or closed by hand. It must be reported as unknown — in
// outcomes_by_author_kind, per calibration bucket, per reviewer model and in
// missing.prs_outcome_unknown — and never counted as "not merged" against a
// reviewer or as "open". A backfilled record that says the PR was still open
// is a known outcome and stays Open.
func TestBuildEvidenceSummaryUnknownOutcome(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-3 * time.Hour)
	approve := []evidence.Verdict{{Perspective: "security", Model: "m", Verdict: "approve", Confidence: 0.95}}
	block := []evidence.Verdict{{Perspective: "security", Model: "m", Verdict: "changes_requested", Confidence: 0.95}}
	held := true
	shadow := func(b *evidence.Bundle) *evidence.Bundle {
		b.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &held}}
		return b
	}
	// Live-only, reviewed, no merge event: outcome unknown.
	unknownApproved := shadow(evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, false, approve, 0, false))
	unknownBlocked := evSummaryBundle("a/b", 2, evidence.AuthorAgent, recent, false, block, 0, false)
	// Live, merged by this hive: known.
	mergedApproved := evSummaryBundle("a/b", 3, evidence.AuthorAgent, recent, true, approve, 0, true)
	// Live review plus a backfilled record that says closed without merge:
	// the one genuine false approve.
	closedLive := evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent, false, approve, 0, false)
	closedBF := evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent.Add(-time.Hour), false, nil, 0, false)
	closedBF.HeadSHA = "bf"
	closedBF.Backfilled = true
	closedBF.Backfill = &evidence.Backfill{Source: "github-api", At: now, State: evidence.BackfillStateClosed}
	// Backfilled record that found the PR still open: known, Open.
	openBF := evSummaryBundle("a/b", 5, evidence.AuthorHuman, recent, false, nil, 0, false)
	openBF.Backfilled = true
	openBF.Backfill = &evidence.Backfill{Source: "github-api", At: now, State: evidence.BackfillStateOpen}

	got := buildEvidenceSummary([]*evidence.Bundle{unknownApproved, unknownBlocked, mergedApproved, closedLive, closedBF, openBF}, now, 24, "", "")
	if got.N != 5 {
		t.Fatalf("n = %d, want 5", got.N)
	}
	if o := outcomeCounts(got.Outcomes["agent"]); o != (EvidenceOutcomes{N: 4, Merged: 1, Closed: 1, Open: 0, Unknown: 2}) {
		t.Errorf("agent outcomes = %+v, want merged 1 / closed 1 / unknown 2", o)
	}
	if o := outcomeCounts(got.Outcomes["human"]); o != (EvidenceOutcomes{N: 1, Open: 1}) {
		t.Errorf("a backfilled still-open record is a known Open, got %+v", o)
	}
	if got.Missing.PRsOutcomeUnknown != 2 {
		t.Errorf("missing.prs_outcome_unknown = %d, want 2", got.Missing.PRsOutcomeUnknown)
	}

	if len(got.ReviewerModels) != 1 {
		t.Fatalf("models = %+v", got.ReviewerModels)
	}
	st := got.ReviewerModels[0].ByAuthor["agent"]
	want := EvidenceModelStat{N: 4, Approvals: 3, Blocks: 1, ApprovedNotMerged: 1, ApprovedUnknown: 1, BlockedUnknown: 1, AdvisoryBlocks: 1, AdvisoryBlockedUnknown: 1}
	if st != want {
		t.Errorf("model stat = %+v, want %+v", st, want)
	}
	// The false-approve denominator the card uses is approvals with a known
	// outcome: 1 false approve over 2, not 2 over 3.
	if known := st.Approvals - st.ApprovedUnknown; known != 2 || st.ApprovedNotMerged != 1 {
		t.Errorf("false-approve = %d/%d, want 1/2", st.ApprovedNotMerged, known)
	}
	if known := st.Blocks - st.BlockedUnknown; known != 0 || st.MergedAfterBlock != 0 {
		t.Errorf("false-block must have no denominator when every block is on an unknown-outcome PR: %+v", st)
	}

	if len(got.Calibration) != 1 {
		t.Fatalf("calibration = %+v", got.Calibration)
	}
	top := got.Calibration[0].Buckets[4]
	if top.N != 4 || top.Merged != 1 || top.Unknown != 2 {
		t.Errorf("top bucket = %+v, want n 4 / merged 1 / unknown 2 (merged rate 1 of 2 known, not 1 of 4)", top)
	}

	if h := got.ShadowGate.ReviewGate.Held; h.N != 1 || h.Open != 0 || h.Unknown != 1 {
		t.Errorf("shadow held outcome must be unknown, not open: %+v", h)
	}
}

// Blocks split by the bundle's policy.require_approval (#12595): only a block
// an enforcing gate read can be a false block; an advisory block on a merged
// PR is dissent before merge. No model row is dropped.
func TestBuildEvidenceSummarySplitsEnforcedAndAdvisoryBlocks(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	block := func(model string) []evidence.Verdict {
		return []evidence.Verdict{{Perspective: "style", Model: model, Verdict: "changes_requested", Confidence: 0.8}}
	}
	withPolicy := func(b *evidence.Bundle, enforced bool) *evidence.Bundle {
		b.Policy.RequireApproval = enforced
		return b
	}
	bundles := []*evidence.Bundle{
		withPolicy(evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, block("m1"), 0, true), true),
		withPolicy(evSummaryBundle("a/b", 2, evidence.AuthorAgent, recent, false, block("m1"), 0, true), true),
		withPolicy(evSummaryBundle("a/b", 3, evidence.AuthorAgent, recent, true, block("m1"), 0, true), false),
		withPolicy(evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent, true, block("m1"), 0, true), false),
		withPolicy(evSummaryBundle("a/b", 5, evidence.AuthorAgent, recent, false, block("m1"), 0, true), false),
		withPolicy(evSummaryBundle("a/b", 6, evidence.AuthorAgent, recent, true, block("m2"), 0, true), false),
	}
	tests := []struct {
		name  string
		model string
		want  EvidenceModelStat
	}{
		{
			name: "mixed enforced and advisory", model: "m1",
			want: EvidenceModelStat{
				N: 5, Blocks: 5, MergedAfterBlock: 3, BlockedUnknown: 2,
				EnforcedBlocks: 2, MergedAfterEnforcedBlock: 1, EnforcedBlockedUnknown: 1,
				AdvisoryBlocks: 3, DissentBeforeMerge: 2, AdvisoryBlockedUnknown: 1,
			},
		},
		{
			name: "all advisory keeps its row", model: "m2",
			want: EvidenceModelStat{N: 1, Blocks: 1, MergedAfterBlock: 1, AdvisoryBlocks: 1, DissentBeforeMerge: 1},
		},
	}
	got := buildEvidenceSummary(bundles, now, 24, "", "")
	if len(got.ReviewerModels) != 2 {
		t.Fatalf("models = %+v, want both rows", got.ReviewerModels)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var st EvidenceModelStat
			for _, m := range got.ReviewerModels {
				if m.Model == tt.model {
					st = m.ByAuthor[evidence.AuthorAgent]
				}
			}
			if st != tt.want {
				t.Fatalf("stat = %+v, want %+v", st, tt.want)
			}
			if st.EnforcedBlocks+st.AdvisoryBlocks != st.Blocks {
				t.Fatalf("enforced + advisory != blocks: %+v", st)
			}
		})
	}
	data, err := json.Marshal(got.ReviewerModels[0].ByAuthor[evidence.AuthorAgent])
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"advisory_blocks":3`, `"enforced_blocks":2`, `"dissent_before_merge":2`, `"merged_after_enforced_block":1`} {
		if !strings.Contains(string(data), key) {
			t.Errorf("JSON %s missing %s", data, key)
		}
	}
}

// The review gate's shadow observations are split by the lane that recorded
// them (#12592): the merge-eligible lane or the self-merge ReviewGate.
func TestBuildEvidenceSummaryShadowReviewGateByLane(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	held, passed := true, false
	shadow := func(n int, merged bool, lane string, flag *bool) *evidence.Bundle {
		b := evSummaryBundle("a/b", n, evidence.AuthorAgent, recent, merged, nil, 0, true)
		b.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: flag, Lane: lane, Reason: "no-review-bundle", Confidence: 1, EvaluatedAt: recent}}
		return b
	}
	got := buildEvidenceSummary([]*evidence.Bundle{
		shadow(1, true, "self-merge", &held),
		shadow(2, true, "self-merge", &passed),
		shadow(3, false, "", &held),
	}, now, 24, "", "").ShadowGate
	tests := []struct {
		name string
		got  EvidenceShadowGateSide
		want EvidenceShadowGateSide
	}{
		{name: "self-merge", got: got.ReviewGateByLane.SelfMerge, want: EvidenceShadowGateSide{N: 2, Held: EvidenceShadowOutcome{N: 1, Merged: 1}, Passed: EvidenceShadowOutcome{N: 1, Merged: 1}}},
		{name: "merge-eligible", got: got.ReviewGateByLane.MergeEligible, want: EvidenceShadowGateSide{N: 1, Held: EvidenceShadowOutcome{N: 1, Unknown: 1}}},
		{name: "total", got: got.ReviewGate, want: EvidenceShadowGateSide{N: 3, Held: EvidenceShadowOutcome{N: 2, Merged: 1, Unknown: 1}, Passed: EvidenceShadowOutcome{N: 1, Merged: 1}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Fatalf("%s = %+v, want %+v", tt.name, tt.got, tt.want)
			}
		})
	}
	if got.WouldHaveHeld != 2 || got.N != 3 {
		t.Fatalf("shadow gate = %+v", got)
	}
}

// TestBuildEvidenceSummaryLedgerOutcomes pins how the review-outcome ledger
// resolves PRs a live bundle cannot: an Outcome stamped on the bundle (merged
// by someone else, or closed) counts as merged_elsewhere / closed, a ledger
// hint does the same at read time for a PR whose bundle has no stamp, an
// open hint is a known Open, a hive-performed merge is never merged
// elsewhere, and a PR the ledger never saw stays unknown.
func TestBuildEvidenceSummaryLedgerOutcomes(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-3 * time.Hour)
	approve := []evidence.Verdict{{Perspective: "security", Model: "m", Verdict: "approve", Confidence: 0.95}}

	// Stamped on disk by the ledger pass: merged by a human, and closed.
	stampedMerged := evSummaryBundle("a/b", 1, evidence.AuthorHuman, recent, false, approve, 0, false)
	stampedMerged.Outcome = &evidence.Outcome{Source: evidence.OutcomeSourceLedger, State: evidence.BackfillStateMerged, At: now, MergedBy: "alice", MergedAt: recent.Add(time.Hour)}
	stampedMerged.Merge = evidence.OutcomeMergeEvent(stampedMerged.Outcome, stampedMerged.HeadSHA)
	stampedClosed := evSummaryBundle("a/b", 2, evidence.AuthorHuman, recent, false, approve, 0, false)
	stampedClosed.Outcome = &evidence.Outcome{Source: evidence.OutcomeSourceLedger, State: evidence.BackfillStateClosed, At: now, ClosedAt: recent.Add(time.Hour)}
	// Merged by this hive: plain merged.
	hiveMerged := evSummaryBundle("a/b", 3, evidence.AuthorAgent, recent, true, approve, 0, true)
	// No stamp; resolved only by the ledger hints below.
	hintMerged := evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent, false, approve, 0, false)
	hintClosed := evSummaryBundle("a/b", 5, evidence.AuthorAgent, recent, false, approve, 0, false)
	hintOpen := evSummaryBundle("a/b", 6, evidence.AuthorHuman, recent, false, nil, 0, false)
	// Never seen by the ledger: still unknown.
	unknown := evSummaryBundle("a/b", 7, evidence.AuthorHuman, recent, false, approve, 0, false)

	ledger := &review.OutcomeLedger{GeneratedAt: now, Items: map[string]*review.PROutcome{
		"a/b#4": {Repo: "a/b", Number: 4, Outcome: review.OutcomeMerged, OutcomeAt: recent.Add(time.Hour)},
		"a/b#5": {Repo: "a/b", Number: 5, Outcome: review.OutcomeClosed, OutcomeAt: recent.Add(time.Hour)},
		"a/b#6": {Repo: "a/b", Number: 6, Outcome: review.OutcomeOpen},
		// A ledger row never overrides what the bundle already states.
		"a/b#3": {Repo: "a/b", Number: 3, Outcome: review.OutcomeClosed, OutcomeAt: now},
		"a/b#9": {Repo: "a/b", Number: 9, Outcome: "weird"},
	}}
	hints := evidenceOutcomeHints(ledger)
	if len(hints) != 4 {
		t.Fatalf("hints = %+v, want 4 (the unknown state is dropped)", hints)
	}

	bundles := []*evidence.Bundle{stampedMerged, stampedClosed, hiveMerged, hintMerged, hintClosed, hintOpen, unknown}
	win := evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}
	got := buildEvidenceSummaryWindowHints(bundles, hints, win, "", "")
	if got.N != 7 {
		t.Fatalf("n = %d, want 7", got.N)
	}
	if o := outcomeCounts(got.Outcomes["human"]); o != (EvidenceOutcomes{N: 4, Merged: 1, MergedElsewhere: 1, Closed: 1, Open: 1, Unknown: 1}) {
		t.Errorf("human outcomes = %+v, want merged 1 (elsewhere 1) / closed 1 / open 1 / unknown 1", o)
	}
	if o := outcomeCounts(got.Outcomes["agent"]); o != (EvidenceOutcomes{N: 3, Merged: 2, MergedElsewhere: 1, Closed: 1}) {
		t.Errorf("agent outcomes = %+v, want merged 2 (elsewhere 1) / closed 1", o)
	}
	if got.Missing.PRsOutcomeUnknown != 1 || got.Missing.PRsOutcomeFromLedger != 5 {
		t.Errorf("missing = unknown %d / from_ledger %d, want 1 / 5", got.Missing.PRsOutcomeUnknown, got.Missing.PRsOutcomeFromLedger)
	}
	// The reviewer-accuracy denominators see the ledger outcomes too: the two
	// closed approvals are false approves over five approvals with a known
	// outcome; only the never-seen PR's approval is unknown.
	if len(got.ReviewerModels) != 1 {
		t.Fatalf("models = %+v", got.ReviewerModels)
	}
	var st EvidenceModelStat
	for _, s := range got.ReviewerModels[0].ByAuthor {
		st.Approvals += s.Approvals
		st.ApprovedNotMerged += s.ApprovedNotMerged
		st.ApprovedUnknown += s.ApprovedUnknown
	}
	if st.Approvals != 6 || st.ApprovedNotMerged != 2 || st.ApprovedUnknown != 1 {
		t.Errorf("approvals = %+v, want 6 approvals / 2 not merged / 1 unknown", st)
	}

	// Without the ledger the same bundles leave the hinted PRs unknown.
	plain := buildEvidenceSummaryWindow(bundles, win, "", "")
	if plain.Missing.PRsOutcomeUnknown != 4 || plain.Missing.PRsOutcomeFromLedger != 2 {
		t.Errorf("without hints: unknown %d / from_ledger %d, want 4 / 2", plain.Missing.PRsOutcomeUnknown, plain.Missing.PRsOutcomeFromLedger)
	}
}

// outcomeCounts strips the pointer-valued change-failure fields so a test can
// compare the plain counts by value.
func outcomeCounts(o EvidenceOutcomes) EvidenceOutcomes {
	o.RevertedWithin14d = nil
	o.ChangeFailureRate = nil
	return o
}

// hivecommons/hive#13241: reverts and issue re-opens are counted per author
// kind as change failures, with rate = (reverts + re-opens) ÷ merged.
func TestBuildEvidenceSummaryChangeFailures(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	mergedAt := now.Add(-time.Hour)
	withMerge := func(b *evidence.Bundle) *evidence.Bundle {
		b.Merge.At = mergedAt
		return b
	}
	reverted := withMerge(evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, nil, 0, true))
	reverted.RevertedBy = []string{"a/b#9@merged"}
	reopened := withMerge(evSummaryBundle("a/b", 2, evidence.AuthorAgent, recent, true, nil, 0, true))
	reopened.IssueReopens = []evidence.IssueReopen{{Issue: 7, At: mergedAt.Add(30 * time.Minute), Source: evidence.IssueReopenSourceClaimLedger}}
	// A reopen outside the 14-day window is not a change failure.
	lateReopen := withMerge(evSummaryBundle("a/b", 3, evidence.AuthorHuman, recent, true, nil, 0, true))
	lateReopen.IssueReopens = []evidence.IssueReopen{{Issue: 8, At: mergedAt.Add(evidence.LinkWindow + time.Hour), Source: evidence.IssueReopenSourceClaimLedger}}
	both := withMerge(evSummaryBundle("a/b", 4, evidence.AuthorHuman, recent, true, nil, 0, true))
	both.RevertedBy = []string{"a/b#10@merged"}
	both.IssueReopens = []evidence.IssueReopen{{Issue: 9, At: mergedAt.Add(time.Hour), Source: evidence.IssueReopenSourceClaimLedger}}
	// An unmerged PR with links never counts.
	unmerged := evSummaryBundle("a/b", 5, evidence.AuthorHuman, recent, false, nil, 0, true)
	unmerged.IssueReopens = []evidence.IssueReopen{{Issue: 10, At: mergedAt, Source: evidence.IssueReopenSourceClaimLedger}}
	bundles := []*evidence.Bundle{
		reverted, reopened, lateReopen, both, unmerged,
		withMerge(evSummaryBundle("a/b", 6, evidence.AuthorAgent, recent, true, nil, 0, true)),
		withMerge(evSummaryBundle("a/b", 7, evidence.AuthorAgent, recent, true, nil, 0, true)),
	}

	got := buildEvidenceSummary(bundles, now, 24, "", "")
	agent := got.Outcomes["agent"]
	if agent.Merged != 4 || agent.RevertedWithin14d == nil || *agent.RevertedWithin14d != 1 || agent.IssueReopenedWithin14d != 1 || agent.ChangeFailures != 2 {
		t.Errorf("agent = %+v", agent)
	}
	if agent.ChangeFailureRate == nil || *agent.ChangeFailureRate != 0.5 {
		t.Errorf("agent change_failure_rate = %v, want 0.5", agent.ChangeFailureRate)
	}
	human := got.Outcomes["human"]
	if human.Merged != 2 || human.RevertedWithin14d == nil || *human.RevertedWithin14d != 1 || human.IssueReopenedWithin14d != 1 || human.ChangeFailures != 2 {
		t.Errorf("human = %+v", human)
	}
	if human.ChangeFailureRate == nil || *human.ChangeFailureRate != 1 {
		t.Errorf("human change_failure_rate = %v, want 1", human.ChangeFailureRate)
	}
	if human.Unknown != 1 {
		t.Errorf("unmerged live PR must be unknown, got %+v", human)
	}
	// A kind with no merges carries zero counts and no rate.
	none := buildEvidenceSummary(bundles[4:5], now, 24, "", "").Outcomes["human"]
	if none.RevertedWithin14d == nil || *none.RevertedWithin14d != 0 || none.ChangeFailureRate != nil || none.ChangeFailures != 0 {
		t.Errorf("no-merge kind = %+v", none)
	}

	raw, err := json.Marshal(agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{`"reverted_14d":1`, `"issue_reopened_14d":1`, `"change_failures":2`, `"change_failure_rate":0.5`} {
		if !strings.Contains(string(raw), key) {
			t.Errorf("JSON %s lacks %s", raw, key)
		}
	}

	cards := evidenceCards(&got, evidenceWindow{Hours: 24, From: now.Add(-24 * time.Hour), To: now}, now)
	card := cards[evidenceCardOutcome]
	// Both kinds have failures and an unknown trails, so the summary falls back
	// to the compact "undone" form to stay inside the 20-word budget.
	if want := "Agents: 4 merged, 2 undone (50%). Humans: 2 merged, 2 undone (100%). 1 outcome not recorded yet."; card.PlainSummary != want {
		t.Errorf("plain summary = %q, want prefix %q", card.PlainSummary, want)
	}
}

func TestCalibrationFiveBinsAndDerivedCoverage(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	var verdicts []evidence.Verdict
	for _, confidence := range []float64{.55, .65, .75, .85, .95} {
		raw, derived := confidence, confidence
		verdicts = append(verdicts, evidence.Verdict{Perspective: "security", Model: "m", Verdict: "approve", Confidence: confidence, ConfidenceRaw: &raw, ConfidenceDerived: &derived})
	}
	// Historical verdicts remain in self-report/fallback only.
	verdicts = append(verdicts, evidence.Verdict{Perspective: "security", Model: "m", Verdict: "changes_requested", Confidence: .6})
	merged := evSummaryBundle("o/r", 1, evidence.AuthorAgent, now.Add(-time.Hour), true, verdicts, 0, false)
	derived := .85
	unknown := evSummaryBundle("o/r", 2, evidence.AuthorHuman, now.Add(-time.Hour), false, []evidence.Verdict{{Perspective: "security", Model: "m", Verdict: "approve", Confidence: .95, ConfidenceDerived: &derived}}, 0, false)
	got := buildEvidenceSummary([]*evidence.Bundle{merged, unknown}, now, 24, "", "")
	if len(got.Calibration) != 1 || len(got.CalibrationDerived) != 1 {
		t.Fatalf("missing series: %+v", got)
	}
	self, other := got.Calibration[0], got.CalibrationDerived[0]
	if self.N != 7 || other.N != 6 || got.CalibrationLegacy != 2 {
		t.Fatalf("coverage self=%d derived=%d legacy=%d", self.N, other.N, got.CalibrationLegacy)
	}
	if len(other.Buckets) != 5 || len(self.Buckets) != 5 {
		t.Fatal("not five bins")
	}
	for i, b := range other.Buckets {
		if b.Merged != 1 {
			t.Errorf("bin %d missing merge: %+v", i, b)
		}
	}
	if other.Buckets[3].Unknown != 1 || self.Buckets[4].Unknown != 1 || self.Buckets[1].Blocks != 1 {
		t.Fatal("unknown/legacy bucket accounting wrong")
	}
	legacyOnly := buildEvidenceSummary([]*evidence.Bundle{evSummaryBundle("o/r", 3, evidence.AuthorHuman, now.Add(-time.Hour), true, []evidence.Verdict{{Perspective: "security", Verdict: "approve", Confidence: 1}}, 0, false)}, now, 24, "", "")
	if len(legacyOnly.CalibrationDerived) != 0 {
		t.Fatal("invented derived confidence for old bundles")
	}
}

func TestLoadEvidenceBundlesDoesNotFallBackToPackageRoot(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "o", "r", "1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "o", "r", "1", "abc1234.json"), []byte("not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := ghpkg.ReviewEvidenceRoot
	ghpkg.ReviewEvidenceRoot = dir
	t.Cleanup(func() { ghpkg.ReviewEvidenceRoot = prev })

	bundles, unreadable, err := loadEvidenceBundles("", ghpkg.ReviewEvidenceVerifier{})
	if err != nil || len(bundles) != 0 || unreadable != 0 {
		t.Fatalf("empty root must not read the package default: bundles=%d unreadable=%d err=%v", len(bundles), unreadable, err)
	}
}
