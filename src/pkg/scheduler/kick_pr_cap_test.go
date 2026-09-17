package scheduler

// Tests for the kick-prompt PR list cap (hivecommons/hive#7368). The issue
// list has always been capped at maxIssuesPerKick; the PR list was not, so a
// spoke with 302 open PRs delivered a 69.5 KiB kick — the PR section alone
// ~36 KiB against a documented ~3.5 KiB. Every PR list a kick renders is now
// bounded by governor.kick_limits.max_prs (default maxPRsPerKick) and says so
// with an explicit "… and N more" line when cut.

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// manyPRs mirrors the reported backlog: 302 open PRs with realistic titles.
func manyPRs(n int) []github.PullRequest {
	prs := make([]github.PullRequest, 0, n)
	for i := 1; i <= n; i++ {
		prs = append(prs, makePR("projectbluefin/bluefin", 1000+i,
			fmt.Sprintf("chore(deps): bump some-dependency from 1.%d.0 to 1.%d.1 in /packages", i, i+1), "dependabot[bot]"))
	}
	return prs
}

func prLineCount(s string) int { return strings.Count(s, "projectbluefin/bluefin#") }

// TestFormatPRList_CappedWithOverflowMarker is the regression for #7368: the
// ${PR_LIST} formatter renders at most maxPRsPerKick PRs and tells the agent
// how many it left out. On the parent commit it rendered all 302.
func TestFormatPRList_CappedWithOverflowMarker(t *testing.T) {
	s := newScheduler()
	actionable := &github.ActionableResult{PRs: github.PRResult{Count: 302, Items: manyPRs(302)}}

	out := s.formatPRList(actionable)
	if got := prLineCount(out); got != maxPRsPerKick {
		t.Fatalf("PR lines = %d, want %d (cap)", got, maxPRsPerKick)
	}
	want := fmt.Sprintf("… and %d more open PRs not listed (cap %d per kick", 302-maxPRsPerKick, maxPRsPerKick)
	if !strings.Contains(out, want) {
		t.Errorf("overflow marker missing; want %q in:\n%s", want, out[len(out)-300:])
	}
	// The cap keeps the section near the documented budget (~120 B per line):
	// at 302 PRs the uncapped list measured ~36 KiB.
	if len(out) > 10*1024 {
		t.Errorf("PR section is %d bytes; the cap should hold it under 10 KiB", len(out))
	}

	// Exactly at the cap: no marker, nothing omitted.
	exact := &github.ActionableResult{PRs: github.PRResult{Count: maxPRsPerKick, Items: manyPRs(maxPRsPerKick)}}
	if out := s.formatPRList(exact); prLineCount(out) != maxPRsPerKick || strings.Contains(out, "more open PRs") {
		t.Errorf("a list exactly at the cap must render whole with no marker:\n%s", out)
	}
}

// TestScannerMessage_PRListCapped covers the hardcoded scanner fallback
// prompt, which had its own uncapped PR loop, and its stale-draft list.
func TestScannerMessage_PRListCapped(t *testing.T) {
	s := newScheduler()
	drafts := manyPRs(maxPRsPerKick + 7)
	actionable := &github.ActionableResult{PRs: github.PRResult{Count: 302, Items: manyPRs(302), StaleDrafts: drafts}}

	msg := s.buildScannerMessage(nil, actionable)
	if !strings.Contains(msg, "ACTIONABLE PRs (302)") {
		t.Errorf("the header must still carry the true count:\n%s", msg[:200])
	}
	// Actionable list (cap) + stale drafts (cap) — the drafts section shares
	// the same repo prefix, so count both markers instead.
	if got := prLineCount(msg); got != 2*maxPRsPerKick {
		t.Errorf("PR lines = %d, want %d (both lists capped)", got, 2*maxPRsPerKick)
	}
	if !strings.Contains(msg, fmt.Sprintf("… and %d more open PRs", 302-maxPRsPerKick)) {
		t.Error("actionable PR overflow marker missing")
	}
	if !strings.Contains(msg, "… and 7 more open PRs") {
		t.Error("stale-draft overflow marker missing")
	}
}

// TestKickLimits_ConfigOverridesBothCaps: governor.kick_limits tunes both
// lists per hive, and the issue cap now reads from the same block.
func TestKickLimits_ConfigOverridesBothCaps(t *testing.T) {
	s := newScheduler()
	s.cfg.Governor.KickLimits = config.KickLimitsConfig{MaxIssues: ptr(3), MaxPRs: ptr(5)}

	actionable := &github.ActionableResult{PRs: github.PRResult{Count: 20, Items: manyPRs(20)}}
	if out := s.formatPRList(actionable); prLineCount(out) != 5 || !strings.Contains(out, "… and 15 more open PRs not listed (cap 5 per kick") {
		t.Errorf("max_prs=5 not honoured:\n%s", out)
	}

	var issues []github.Issue
	for i := 1; i <= 10; i++ {
		issues = append(issues, makeIssue("org/repo", i, "issue title", "scanner", i, nil, false))
	}
	if out := s.formatIssueList(issues); strings.Count(out, "org/repo#") != 3 {
		t.Errorf("max_issues=3 not honoured:\n%s", out)
	}
	if msg := s.buildScannerMessage(issues, emptyActionable()); strings.Count(msg, "org/repo#") != 3 {
		t.Errorf("max_issues=3 not honoured by the scanner fallback:\n%s", msg)
	}
	if refs := issueRefsForAgent("scanner", issues, s.issueCap()); len(refs) != 3 {
		t.Errorf("issueRefsForAgent honoured %d, want 3", len(refs))
	}

	// A negative value falls back to the default; an explicit 0 is unlimited
	// (#7455), and an absent key stays at the default.
	s.cfg.Governor.KickLimits = config.KickLimitsConfig{MaxIssues: ptr(-1), MaxPRs: ptr(0)}
	if s.issueCap() != maxIssuesPerKick {
		t.Errorf("issueCap() = %d, want the default %d", s.issueCap(), maxIssuesPerKick)
	}
	if s.prCap() != config.KickListUnlimited {
		t.Errorf("prCap() = %d, want unlimited for an explicit max_prs: 0", s.prCap())
	}
	s.cfg.Governor.KickLimits = config.KickLimitsConfig{}
	if s.issueCap() != maxIssuesPerKick || s.prCap() != maxPRsPerKick {
		t.Errorf("absent caps = (%d, %d), want defaults (%d, %d)", s.issueCap(), s.prCap(), maxIssuesPerKick, maxPRsPerKick)
	}
	var nilSched *Scheduler
	if nilSched.prCap() != maxPRsPerKick || (&Scheduler{}).issueCap() != maxIssuesPerKick {
		t.Error("nil scheduler / nil config must yield the defaults")
	}
}

// TestMergeEligibleAndCIFailingLists_Capped: the two file-backed PR lists
// share the cap and the marker.
func TestMergeEligibleAndCIFailingLists_Capped(t *testing.T) {
	var eligible, failing []string
	for i := 1; i <= maxPRsPerKick+4; i++ {
		eligible = append(eligible, fmt.Sprintf(`{"number":%d,"repo":"o/r","title":"t%d","queued":false}`, i, i))
		failing = append(failing, fmt.Sprintf(`{"number":%d,"repo":"o/r","title":"t%d","author":"a","head_sha":"abc"}`, i, i))
	}
	out := formatMergeEligibleData([]byte(`{"merge_eligible":[`+strings.Join(eligible, ",")+`]}`), maxPRsPerKick)
	if strings.Count(out, " o/r") != maxPRsPerKick || !strings.Contains(out, "… and 4 more open PRs") {
		t.Errorf("merge-eligible list not capped:\n%s", out)
	}
	if out := formatMergeEligibleData([]byte(`{"merge_eligible":[`+strings.Join(eligible, ",")+`]}`), 0); strings.Count(out, " o/r") != maxPRsPerKick+4 {
		t.Errorf("limit 0 must mean uncapped for the raw formatter:\n%s", out)
	}

	dir := t.TempDir()
	orig := ciFailingPath
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { ciFailingPath = orig })
	if err := os.WriteFile(ciFailingPath, []byte(`{"ci_failing":[`+strings.Join(failing, ",")+`]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newScheduler()
	if out := s.buildCIFailingList(); strings.Count(out, " o/r by @a") != maxPRsPerKick || !strings.Contains(out, "… and 4 more open PRs") {
		t.Errorf("ci-failing list not capped:\n%s", out)
	}
}

func ptr(v int) *int { return &v }
