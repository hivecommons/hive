package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/evidence"
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
	if o := got.Outcomes["human"]; o.N != 1 || o.Open != 1 {
		t.Errorf("human outcomes = %+v", o)
	}
	if got.Missing.VerdictsModelUnknown != 1 || got.Missing.RecordsEmptyCI != 1 {
		t.Errorf("missing = %+v", got.Missing)
	}
	if got.SentinelPerWeek.N != 2 || len(got.SentinelPerWeek.Weeks) != 1 || got.SentinelPerWeek.Weeks[0].Week != "2026-W42" {
		t.Errorf("sentinel = %+v", got.SentinelPerWeek)
	}
	if len(got.Calibration) != 1 || got.Calibration[0].N != 3 {
		t.Fatalf("calibration = %+v", got.Calibration)
	}
	bk := got.Calibration[0].Buckets
	if bk[0].N != 1 || bk[0].Blocks != 1 || bk[1].N != 1 || bk[3].N != 1 || bk[3].Merged != 1 {
		t.Errorf("buckets = %+v", bk)
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
	for _, q := range []string{"hours=-1", "hours=x", "repo=nope", "author_kind=robot"} {
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
	if got["observe"] != (EvidenceWriteCount{Attempted: 5, Blocked: 2}) {
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
	if resp.IntentCounters["tier1"] != (EvidenceWriteCount{Attempted: 7, Blocked: 3}) || resp.IntentCounters["tier3"].Blocked != 2 {
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
	if want := (EvidenceShadowOutcome{N: 2, Merged: 1, Open: 1, Reverted: 1}); sg.ReviewGate.Held != want || sg.ReviewGate.N != 3 {
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
	}

	got := buildEvidenceSummary(bundles, now, 24*14, "", "")
	if len(got.ClaimVsVerdict) != 2 {
		t.Fatalf("claim_vs_verdict = %+v", got.ClaimVsVerdict)
	}
	if want := (EvidenceClaimWeek{Week: "2026-W41", N: 1, Claimed: 1, CICaptured: 1}); got.ClaimVsVerdict[0] != want {
		t.Errorf("W41 = %+v, want %+v", got.ClaimVsVerdict[0], want)
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
