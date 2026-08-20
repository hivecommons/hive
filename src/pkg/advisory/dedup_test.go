package advisory

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
	"time"
)

// titles renders a finding slice for failure messages, so a bad collapse shows
// WHAT survived rather than just a count.
func titles(fs []Finding) string {
	var b strings.Builder
	for _, f := range fs {
		b.WriteString("  - " + f.Title + "\n")
	}
	return b.String()
}

// Near-duplicate collapse (#2364).
//
// The advisory issue is a living document that agents append to every cycle.
// Exact-title dedup does not hold, because an agent re-reporting a persistent
// problem rewords the title each time — so the digest accumulates one entry per
// report rather than one per problem. The live digest that motivated this had
// 92 findings of which 29 were one already-fixed CI workflow.
//
// The risk in fixing it is over-collapse: merging two findings that describe
// DIFFERENT problems hides a real defect, which is strictly worse than showing
// a duplicate. Most of these tests exist to hold that line.

func TestCollapseNearDuplicates_MergesRewordedRestatements(t *testing.T) {
	// Verbatim titles from the pinned digest of #2364 — the same broken
	// workflow reported five ways.
	findings := []Finding{
		{Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
			Title: "pr-verifier.yml failing on every PR — reusable workflow deleted from infra"},
		{Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
			Title: "pr-verifier.yml broken: reusable workflow missing from kubestellar/infra"},
		{Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
			Title: "pr-verifier.yml failing 100% — reusable workflow missing from kubestellar/infra"},
		{Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
			Title: "PR Verifier failing on all PRs — reusable-pr-verifier.yml deleted from kubestellar/infra"},
		{Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
			Title: "pr-verifier.yml: 100% failure rate — reusable workflow missing from kubestellar/infra"},
	}

	got := collapseNearDuplicates(findings)
	if len(got) != 1 {
		t.Fatalf("collapsed to %d findings, want 1:\n%s", len(got), titles(got))
	}
	if got[0].DuplicateCount != 4 {
		t.Errorf("DuplicateCount = %d, want 4 (5 reports, 1 representative)", got[0].DuplicateCount)
	}
}

// The tokenizer must see an org-qualified name and its bare form as the same
// subject. Before sub-word splitting, "kubestellar/infra" and "infra" shared no
// token and these two reports of one problem did not merge.
func TestFindingTokens_SplitsQualifiedNames(t *testing.T) {
	tokens := findingTokens("reusable workflow missing from kubestellar/infra")
	for _, want := range []string{"kubestellar", "infra", "reusable", "workflow"} {
		if !tokens[want] {
			t.Errorf("token %q missing from %v", want, keys(tokens))
		}
	}
	if tokens["kubestellar/infra"] {
		t.Error("qualified name was kept whole; it must split into its parts")
	}
}

// Numbers change on every re-report of the same problem and must not keep two
// reports apart.
func TestFindingTokens_DropsDigits(t *testing.T) {
	a := findingTokens("pr-verifier failing 100% — 2,943 runs")
	b := findingTokens("pr-verifier failing 19% — 47 runs")
	if jaccard(a, b) != 1.0 {
		t.Errorf("titles differing only in numbers scored %.2f, want 1.00\n a=%v\n b=%v",
			jaccard(a, b), keys(a), keys(b))
	}
}

// A merge must never cross agents or finding types. That structural guard is
// what stops a security finding being absorbed into a CI failure because their
// wording happened to overlap.
func TestCollapseNearDuplicates_NeverMergesAcrossAgentOrType(t *testing.T) {
	base := "database connection pool exhausted under load"
	findings := []Finding{
		{Agent: "scanner", Type: "security", Severity: "critical", Title: base},
		{Agent: "quality", Type: "security", Severity: "critical", Title: base},
		{Agent: "scanner", Type: "coverage-gap", Severity: "critical", Title: base},
	}

	got := collapseNearDuplicates(findings)
	if len(got) != 3 {
		t.Errorf("identical titles across agent/type collapsed to %d, want 3 kept:\n%s",
			len(got), titles(got))
	}
}

// The representative must carry the highest severity in its group. Keeping a
// medium representative would demote a critical report into a lower section of
// the digest, which is a quieter way of hiding it.
func TestCollapseNearDuplicates_KeepsHighestSeverity(t *testing.T) {
	// Deliberately a clear restatement pair: this test is about which severity
	// survives a merge, so the titles must be similar enough that the merge is
	// not itself in question.
	findings := []Finding{
		{Agent: "quality", Type: "coverage-gap", Severity: "medium",
			Title: "oauth.go session cookie minting has no test coverage"},
		{Agent: "quality", Type: "coverage-gap", Severity: "critical",
			Title: "oauth.go session cookie minting has zero test coverage"},
	}

	got := collapseNearDuplicates(findings)
	if len(got) != 1 {
		t.Fatalf("collapsed to %d, want 1:\n%s", len(got), titles(got))
	}
	if got[0].Severity != "critical" {
		t.Errorf("severity = %q, want critical — the more severe report must win the merge", got[0].Severity)
	}
}

// Distinct problems that merely share vocabulary must survive. These pairs are
// drawn from the same live digest and are genuinely different findings.
func TestCollapseNearDuplicates_KeepsDistinctFindings(t *testing.T) {
	cases := []struct{ name, a, b string }{
		{
			name: "different files, same finding type",
			a:    "saas_provision.go: 101 uncovered stmts in hive provisioning mutation paths",
			b:    "heartbeat.go (1658 lines, 28 functions) has zero dedicated unit tests",
		},
		{
			name: "summary vs specific test regression",
			a:    "v2 Tests: 9 consecutive failures all day 2026-08-06, regression on commit 1d89011",
			b:    "TestAlertAcksPersistRoundTrip regression in pkg/hub — 2 consecutive failures",
		},
		{
			name: "different workflows",
			a:    "pr-verifier.yml failing on every PR — reusable workflow deleted from infra",
			b:    "Coverage Hourly 'Check per-package coverage' failing on PR branch",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := collapseNearDuplicates([]Finding{
				{Agent: "a", Type: "t", Severity: "high", Title: tc.a},
				{Agent: "a", Type: "t", Severity: "high", Title: tc.b},
			})
			if len(got) != 2 {
				t.Errorf("distinct findings were merged (kept %d, want 2):\n  %q\n  %q", len(got), tc.a, tc.b)
			}
		})
	}
}

func TestCollapseNearDuplicates_PreservesOrderAndShortInputs(t *testing.T) {
	if got := collapseNearDuplicates(nil); got != nil {
		t.Errorf("nil input returned %v, want nil", got)
	}
	one := []Finding{{Agent: "a", Type: "t", Title: "only one"}}
	if got := collapseNearDuplicates(one); len(got) != 1 {
		t.Errorf("single finding collapsed to %d", len(got))
	}

	// Order is the digest's reading order; collapsing must not shuffle it.
	in := []Finding{
		{Agent: "a", Type: "t", Title: "alpha subject one"},
		{Agent: "a", Type: "t", Title: "beta subject two"},
		{Agent: "a", Type: "t", Title: "gamma subject three"},
	}
	got := collapseNearDuplicates(in)
	if len(got) != 3 {
		t.Fatalf("kept %d, want 3", len(got))
	}
	for i := range in {
		if got[i].Title != in[i].Title {
			t.Errorf("position %d = %q, want %q — order changed", i, got[i].Title, in[i].Title)
		}
	}
}

// --- the real corpus --------------------------------------------------------

type liveFinding struct {
	Agent    string `json:"agent"`
	Type     string `json:"type"`
	Severity string `json:"severity"`
	Title    string `json:"title"`
}

func loadLiveDigest(t *testing.T) []Finding {
	t.Helper()
	raw, err := os.ReadFile("testdata/live_digest_2364.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var rows []liveFinding
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	out := make([]Finding, 0, len(rows))
	for _, r := range rows {
		out = append(out, Finding{Agent: r.Agent, Type: r.Type, Severity: r.Severity, Title: r.Title})
	}
	return out
}

// End-to-end against the actual findings from the pinned comment of #2364.
// This is the case the change exists for, so it is measured, not assumed.
func TestCollapseNearDuplicates_LiveDigestCorpus(t *testing.T) {
	findings := loadLiveDigest(t)
	if len(findings) != 92 {
		t.Fatalf("fixture has %d findings, expected the captured 92", len(findings))
	}

	byAgent := map[string][]Finding{}
	for _, f := range findings {
		byAgent[f.Agent] = append(byAgent[f.Agent], f)
	}
	kept := 0
	var kase []Finding
	for _, fs := range byAgent {
		c := collapseNearDuplicates(fs)
		kept += len(c)
		kase = append(kase, c...)
	}

	if kept >= len(findings) {
		t.Fatalf("collapse removed nothing: %d -> %d", len(findings), kept)
	}
	// Measured at 92 -> 60 when written. Assert a floor rather than the exact
	// number so wording drift in the fixture does not make this brittle, but
	// keep it tight enough that a regression to no-op dedup fails.
	if reduction := len(findings) - kept; reduction < 30 {
		t.Errorf("collapsed only %d of %d findings; expected >=25 given 29 pr-verifier restatements",
			reduction, len(findings))
	}

	// The pr-verifier cluster is the specific thing that made the digest
	// unreadable: 29 reports of one problem. It must end up as a handful.
	prv := 0
	for _, f := range kase {
		if strings.Contains(strings.ToLower(f.Title), "pr-verifier") ||
			strings.Contains(strings.ToLower(f.Title), "pr verifier") {
			prv++
		}
	}
	if prv > 6 {
		t.Errorf("pr-verifier findings still %d after collapse (was 29); expected <=6", prv)
	}
	if prv == 0 {
		t.Error("pr-verifier findings vanished entirely — the problem must still be reported once")
	}
}

// The safety property, measured on the real corpus: no merge may join findings
// about different subjects. This is what fixes the threshold at 0.5 — merges
// across subjects first appear at 0.35.
func TestCollapseNearDuplicates_NoCrossSubjectMergeOnLiveCorpus(t *testing.T) {
	subject := func(title string) string {
		tl := strings.ToLower(title)
		for _, s := range []string{"pr-verifier", "pr verifier", "coverage hourly", "v2 tests", "v2 ci"} {
			if strings.Contains(tl, s) {
				if s == "pr verifier" {
					return "pr-verifier"
				}
				return s
			}
		}
		return ""
	}

	findings := loadLiveDigest(t)
	byAgent := map[string][]Finding{}
	for _, f := range findings {
		byAgent[f.Agent] = append(byAgent[f.Agent], f)
	}

	for _, fs := range byAgent {
		// Re-run the matching loop so each merge decision can be inspected.
		var kept []Finding
		var keptTokens []map[string]bool
		for _, f := range fs {
			tokens := findingTokens(f.Title)
			for i := range kept {
				if kept[i].Agent != f.Agent || kept[i].Type != f.Type {
					continue
				}
				if jaccard(tokens, keptTokens[i]) < nearDuplicateThreshold {
					continue
				}
				sa, sb := subject(f.Title), subject(kept[i].Title)
				if sa != sb {
					t.Errorf("cross-subject merge (%q vs %q):\n  %q\n  merged into %q",
						sa, sb, f.Title, kept[i].Title)
				}
				goto next
			}
			kept = append(kept, f)
			keptTokens = append(keptTokens, tokens)
		next:
		}
	}
}

// The threshold must stay clear of the empirical over-collapse boundary. If
// someone lowers it chasing compression, this fails before the digest starts
// hiding findings.
func TestNearDuplicateThreshold_HasSafetyMargin(t *testing.T) {
	const observedOverCollapseBoundary = 0.30
	if nearDuplicateThreshold <= observedOverCollapseBoundary {
		t.Fatalf("threshold %.2f is at or below the measured over-collapse boundary %.2f — "+
			"distinct findings get merged there", nearDuplicateThreshold, observedOverCollapseBoundary)
	}
	if margin := nearDuplicateThreshold - observedOverCollapseBoundary; margin < 0.1 {
		t.Errorf("threshold margin %.2f is too thin; keep >=0.10 above %.2f",
			margin, observedOverCollapseBoundary)
	}
}

// --- rendering --------------------------------------------------------------

// Collapsing must not make a recurring problem look like a one-off: the count
// carries the "this keeps happening" signal the removed entries used to.
func TestFormatDigestMarkdown_ShowsRepeatCount(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		TotalCount:  1,
		ByAgent: map[string][]Finding{
			"ci-maintainer": {{
				Agent: "ci-maintainer", Type: "ci-failure", Severity: "high",
				Title: "pr-verifier.yml failing on every PR", DuplicateCount: 28,
			}},
		},
	}
	out := FormatDigestMarkdown(d, DigestOptions{Org: "kubestellar", PrimaryRepo: "hive"})
	if !strings.Contains(out, "_(reported 29×)_") {
		t.Errorf("digest does not surface the repeat count:\n%s", out)
	}
}

func TestFormatDigestMarkdown_NoRepeatMarkerForSingleReport(t *testing.T) {
	d := &Digest{
		GeneratedAt: time.Now(),
		TotalCount:  1,
		ByAgent: map[string][]Finding{
			"quality": {{Agent: "quality", Type: "coverage-gap", Severity: "low", Title: "one-off finding"}},
		},
	}
	out := FormatDigestMarkdown(d, DigestOptions{Org: "kubestellar", PrimaryRepo: "hive"})
	if strings.Contains(out, "reported") {
		t.Errorf("single-report finding was annotated with a count:\n%s", out)
	}
}

// The header count and the rendered list must agree — a header claiming 92
// above 60 entries is its own bug.
func TestBuildDigest_TotalCountMatchesCollapsedFindings(t *testing.T) {
	findings := loadLiveDigest(t)
	d := BuildDigest(findings, "busy")

	listed := 0
	for _, fs := range d.ByAgent {
		listed += len(fs)
	}
	if d.TotalCount != listed {
		t.Errorf("TotalCount = %d but %d findings are listed", d.TotalCount, listed)
	}
	if d.TotalCount >= len(findings) {
		t.Errorf("TotalCount = %d, expected fewer than the %d input findings", d.TotalCount, len(findings))
	}
}

// keys renders a token set deterministically for failure messages.
func keys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
