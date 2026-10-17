package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// evidenceReportNow is the fixed clock the golden reports are rendered at.
var evidenceReportNow = time.Date(2026, 10, 16, 15, 4, 0, 0, time.UTC)

// evidenceReportFixture is the fleet fixture of the drill-down UI tests with
// a name that needs escaping, so the same data backs modal and report.
func evidenceReportFixture(t *testing.T) []RegistryEntry {
	t.Helper()
	merged := func(v int) *int { return &v }
	return []RegistryEntry{
		{ID: "alpha", Name: "Alpha <b>", ACMMLevel: 4, PRsMerged90d: merged(40), Evidence: &HeartbeatEvidence{
			Verdicts90d: 25, FalseApprove90d: 3, FalseBlock90d: 1, Merged90d: 30, Reverted14d90d: 3,
			ReviewerFamilies: []string{"claude"}, AuthorFamilies: []string{"claude"},
			Calibration: [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 2}, {}, {}, {}, {}, {N: 13, Bad: 1}},
			OutcomesByAuthorKind: map[string]EvidenceOutcome{
				"agent": {N: 20, Merged: 16, Reverted: 2}, "human": {N: 12, Merged: 10, Reverted: 1},
			},
			WriteCounters: map[string]EvidenceWriteCount{"guarded": {Attempted: 40, Blocked: 4}, "sandbox": {Attempted: 3}},
			ShadowGate: &EvidenceShadow{ReviewGate: EvidenceShadowGate{
				Held: EvidenceOutcome{N: 10, Merged: 8, Reverted: 2}, Passed: EvidenceOutcome{N: 15, Merged: 12}}},
		}},
		{ID: "beta", ACMMLevel: 3, PRsMerged90d: merged(10), Evidence: &HeartbeatEvidence{
			Verdicts90d: 12, FalseApprove90d: 1, Merged90d: 9, Reverted14d90d: 1,
			ReviewerFamilies: []string{"gpt"}, AuthorFamilies: []string{"claude"},
			Calibration:          [EvidenceCalibrationBuckets]EvidenceBucket{{N: 12, Bad: 3}},
			OutcomesByAuthorKind: map[string]EvidenceOutcome{"agent": {N: 12, Merged: 9, Reverted: 1}},
			WriteCounters:        map[string]EvidenceWriteCount{"guarded": {Attempted: 10, Blocked: 1}},
		}},
		{ID: "old", ACMMLevel: 3, Evidence: &HeartbeatEvidence{Verdicts90d: 4, Merged90d: 2}},
		{ID: "gamma", ACMMLevel: 2},
	}
}

// checkGolden compares got with testdata/<name>.golden; UPDATE_GOLDEN=1
// rewrites the file.
func checkGolden(t *testing.T, name, got string) {
	t.Helper()
	path := filepath.Join("testdata", name+".golden")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden: %v (run with UPDATE_GOLDEN=1 to create it)", err)
	}
	if string(want) != got {
		t.Errorf("%s differs from golden (UPDATE_GOLDEN=1 to accept):\n%s", name, got)
	}
}

var evidenceIssueNumberRE = regexp.MustCompile(`#\d{4,}`)

// TestEvidenceReportGoldenFleet: the whole-fleet Markdown report is stable
// and carries no issue numbers in its prose (the footer link is the only URL).
func TestEvidenceReportGoldenFleet(t *testing.T) {
	view := buildEvidenceView(evidenceReportFixture(t))
	rep := buildEvidenceReport(view, "whole fleet", evidenceReportNow)
	md := renderEvidenceReportMarkdown(rep)
	checkGolden(t, "evidence_report_fleet", md)
	if evidenceIssueNumberRE.MatchString(md) {
		t.Errorf("report prose contains an issue number:\n%s", md)
	}
	for _, want := range []string{
		"# Hive evidence report — whole fleet",
		"- Generated: 2026-10-16 15:04 UTC",
		"## Summary",
		"- 3 of 4 selected hives report evidence for the last 90d.",
		"- Of the 50 merged PRs in the last 90 days across the selected hives, 39 (78.0%) have an evidence record — recording began 2026-10-08, so this will rise.",
		"| Very confident (0.83–1.0) | 13 | 7.7% of 13 (margin 1.4% – 33.3%) |",
		"| Not confident (0.0–0.5) | 24 | 20.8% of 24 (margin 9.2% – 40.5%) |",
		"## How to read these",
		"## What is not reported yet and why",
		"## Grade per section",
		"| Evidence coverage | Fair |",
		"| Boundary enforcement | Not graded |",
		"review-evidence.md#how-to-read-the-hub-evidence-view",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("fleet report is missing %q", want)
		}
	}
}

// TestEvidenceReportGoldenHive: a single-hive report drops the per-hive
// coverage duplicate row and names the hive in the title.
func TestEvidenceReportGoldenHive(t *testing.T) {
	entries := filterEvidenceRegistry(evidenceReportFixture(t), map[string][]string{"hive": {"beta"}})
	if len(entries) != 1 || entries[0].ID != "beta" {
		t.Fatalf("filter = %+v", entries)
	}
	view := buildEvidenceView(entries)
	md := renderEvidenceReportMarkdown(buildEvidenceReport(view, "hive beta", evidenceReportNow))
	checkGolden(t, "evidence_report_hive", md)
	for _, want := range []string{
		"# Hive evidence report — hive beta",
		"- beta reports evidence for the last 90d.",
		"| All selected hives | 10 | 90.0% of 10 (margin 59.6% – 98.2%) |",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("hive report is missing %q", want)
		}
	}
	if strings.Contains(md, "| beta | 10 |") {
		t.Error("single-hive report repeats the hive's coverage row under the selection row")
	}
}

// TestEvidenceReportHTMLEscapes: hive names are operator text.
func TestEvidenceReportHTMLEscapes(t *testing.T) {
	view := buildEvidenceView(evidenceReportFixture(t))
	page := renderEvidenceReportHTML(buildEvidenceReport(view, "hive Alpha <b>", evidenceReportNow))
	if strings.Contains(page, "Alpha <b>") {
		t.Error("hive name not escaped in the HTML report")
	}
	for _, want := range []string{"<!DOCTYPE html>", "<title>Hive evidence report — hive Alpha &lt;b&gt;</title>", "<style>", "<table><thead><tr><th>Reviewer confidence</th>", "<footer>"} {
		if !strings.Contains(page, want) {
			t.Errorf("HTML report is missing %q", want)
		}
	}
	if strings.Contains(page, "<link") || strings.Contains(page, "<script") {
		t.Error("HTML report must be self-contained (no external assets or scripts)")
	}
}

// TestEvidenceReportEndpoint: admin-only, both formats, inline vs download.
func TestEvidenceReportEndpoint(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.registry.Hives = evidenceReportFixture(t)

	handler := srv.requireAdmin(srv.handleEvidenceReport)
	req := httptest.NewRequest("GET", evidenceReportPath, nil)
	req.AddCookie(&http.Cookie{Name: "hive_hub_user", Value: "regularuser"})
	w := httptest.NewRecorder()
	handler(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("non-admin status = %d, want 403", w.Code)
	}

	w = httptest.NewRecorder()
	srv.handleEvidenceReport(w, httptest.NewRequest("GET", evidenceReportPath+"?hive=alpha&hours=168", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/markdown; charset=utf-8" {
		t.Fatalf("md status = %d content-type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `inline; filename="hive-evidence-alpha-`) || !strings.HasSuffix(cd, `.md"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}
	body := w.Body.String()
	if !strings.Contains(body, "# Hive evidence report — hive Alpha \\<b>") || !strings.Contains(body, "Window: the last 7d") {
		t.Errorf("md report body:\n%s", body)
	}
	if strings.Contains(body, "| beta |") {
		t.Error("hive filter leaked another hive into the report")
	}

	w = httptest.NewRecorder()
	srv.handleEvidenceReport(w, httptest.NewRequest("GET", evidenceReportPath+"?format=html&download=1", nil))
	if w.Code != http.StatusOK || w.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("html status = %d content-type = %q", w.Code, w.Header().Get("Content-Type"))
	}
	if cd := w.Header().Get("Content-Disposition"); !strings.HasPrefix(cd, `attachment; filename="hive-evidence-fleet-`) || !strings.HasSuffix(cd, `.html"`) {
		t.Errorf("Content-Disposition = %q", cd)
	}

	for _, bad := range []string{"?format=pdf", "?from=2026-10-10&to=2026-10-01"} {
		w = httptest.NewRecorder()
		srv.handleEvidenceReport(w, httptest.NewRequest("GET", evidenceReportPath+bad, nil))
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s status = %d, want 400", bad, w.Code)
		}
	}
}

// TestEvidenceEndpointFilters: hive= and level= narrow GET /api/hub/evidence
// the way the chips do, and the body carries the findings block.
func TestEvidenceEndpointFilters(t *testing.T) {
	srv := newHubServerForTest(t)
	srv.registry.Hives = evidenceReportFixture(t)
	for _, tc := range []struct {
		query  string
		spokes int
	}{{"", 4}, {"?hive=alpha,beta", 2}, {"?hive=alpha&hive=gamma", 2}, {"?level=3", 2}, {"?hive=alpha&level=3", 0}, {"?hive=nope", 0}} {
		w := httptest.NewRecorder()
		srv.handleEvidence(w, httptest.NewRequest("GET", evidencePath+tc.query, nil))
		var got EvidenceView
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatalf("%s: %v", tc.query, err)
		}
		if got.Spokes != tc.spokes {
			t.Errorf("%s spokes = %d, want %d", tc.query, got.Spokes, tc.spokes)
		}
		if len(got.Findings) < 3 || len(got.Findings) > 6 {
			t.Errorf("%s findings = %d sentences, want 3–6: %q", tc.query, len(got.Findings), got.Findings)
		}
		for _, s := range got.Findings {
			if evidenceIssueNumberRE.MatchString(s) {
				t.Errorf("finding contains an issue number: %q", s)
			}
		}
	}
}

// TestEvidenceConfidenceRows: the six heartbeat buckets regroup into the four
// spoke-card ranges with pooled counts and the real range in the label.
func TestEvidenceConfidenceRows(t *testing.T) {
	in := []EvidenceCalibrationBucket{
		{Bucket: 0, Bad: 1, EvidenceRate: EvidenceRate{N: 5}}, {Bucket: 1, Bad: 2, EvidenceRate: EvidenceRate{N: 6}},
		{Bucket: 2, Bad: 0, EvidenceRate: EvidenceRate{N: 4}}, {Bucket: 3, Bad: 1, EvidenceRate: EvidenceRate{N: 3}},
		{Bucket: 4, Bad: 0, EvidenceRate: EvidenceRate{N: 0}}, {Bucket: 5, Bad: 0, EvidenceRate: EvidenceRate{N: 20}},
	}
	rows := evidenceConfidenceRows(in)
	if len(rows) != 4 {
		t.Fatalf("rows = %d", len(rows))
	}
	if rows[0].Label() != "Not confident (0.0–0.5)" || rows[0].N != 15 || rows[0].Bad != 3 || rows[0].Rate == nil {
		t.Errorf("row0 = %+v", rows[0])
	}
	if rows[1].Label() != "Somewhat confident (0.5–0.67)" || rows[1].N != 3 || rows[1].Rate != nil {
		t.Errorf("row1 = %+v", rows[1])
	}
	if rows[3].Label() != "Very confident (0.83–1.0)" || rows[3].N != 20 || *rows[3].Rate != 0 {
		t.Errorf("row3 = %+v", rows[3])
	}
}

// TestEvidenceFindingsFallbacks: every sentence has a "not enough data" form
// and nothing is invented for an empty registry.
func TestEvidenceFindingsFallbacks(t *testing.T) {
	empty := buildEvidenceView(nil)
	f := evidenceFindingsByKey(empty)
	for k, want := range map[string]string{
		"perhive":     "No hives are selected.",
		"coverage":    "No merged PRs are counted yet, so we cannot say how much of the work has an evidence record.",
		"acmm":        "No selected hive reports merges for the last 90d yet.",
		"calibration": "No confidence range has 10 verdicts yet, so we cannot tell whether the reviewer's confidence means anything.",
		"family":      "No family mix has 10 verdicts yet, so same- vs cross-family review cannot be compared.",
		"outcomes":    "", "writes": "", "shadow": "",
	} {
		if f[k] != want {
			t.Errorf("%s = %q, want %q", k, f[k], want)
		}
	}
	if got := evidenceFindings(empty); len(got) != 4 {
		t.Errorf("empty findings = %d sentences: %q", len(got), got)
	}
	waiting := evidenceWaiting(empty)
	if len(waiting) != 6 || !strings.Contains(waiting[2], "shadow mode is off on every selected hive") {
		t.Errorf("waiting = %q", waiting)
	}
	for _, s := range waiting {
		if evidenceIssueNumberRE.MatchString(s) {
			t.Errorf("waiting item contains an issue number: %q", s)
		}
	}
}
