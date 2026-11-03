package scheduler

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Kick-size measurement for governor.kick_limits (hivecommons/hive#12133).
//
// These tests render the SHIPPED scanner-automerge policy — the largest
// template and the one every automerge spoke's scanner types — at
// representative backlog sizes and report the bytes each list contributes.
// The numbers are what docs/operator-reference.md ("Kick size and the list
// caps") quotes; `go test -run TestKickSizeMeasurement -v ./pkg/scheduler/`
// re-derives them. TestKickSizeDefaultsStayInsideBudget pins the
// default-cap worst case so a template or formatter change that blows the
// budget documented in pkg/dashboard/prompt_history.go fails CI instead of
// being discovered as a slow kick on a live spoke.

const kickSizeIssueTitle = "Dashboard: repo cost widget shows stale totals after the nightly rollup finishes"
const kickSizePRTitle = "🐛 dashboard: refresh repo cost widget after nightly rollup (#4711)"

func kickSizeScheduler(t *testing.T, maxIssues, maxPRs *int, mergeEligible, ciFailing int) *Scheduler {
	t.Helper()
	redirectPolicySeams(t)
	dir := t.TempDir()

	origME, origCI := mergeEligiblePath, ciFailingPath
	mergeEligiblePath = filepath.Join(dir, "merge-eligible.json")
	ciFailingPath = filepath.Join(dir, "ci-failing.json")
	t.Cleanup(func() { mergeEligiblePath, ciFailingPath = origME, origCI })

	type meRow struct {
		Number int    `json:"number"`
		Repo   string `json:"repo"`
		Title  string `json:"title"`
	}
	me := struct {
		Items []meRow `json:"merge_eligible"`
	}{}
	for i := 0; i < mergeEligible; i++ {
		me.Items = append(me.Items, meRow{Number: 5000 + i, Repo: "acme/console", Title: kickSizePRTitle})
	}
	meData, _ := json.Marshal(me)
	if err := os.WriteFile(mergeEligiblePath, meData, 0o644); err != nil {
		t.Fatal(err)
	}
	type ciRow struct {
		Number        int      `json:"number"`
		Repo          string   `json:"repo"`
		Title         string   `json:"title"`
		Agent         string   `json:"agent"`
		FailingChecks []string `json:"failing_checks"`
		Excerpt       string   `json:"excerpt"`
	}
	ci := struct {
		Items []ciRow `json:"ci_failing"`
	}{}
	for i := 0; i < ciFailing; i++ {
		ci.Items = append(ci.Items, ciRow{
			Number: 6000 + i, Repo: "acme/console", Title: kickSizePRTitle, Agent: "quality",
			FailingChecks: []string{"build-gate", "Test (chromium, shard 1)"},
			Excerpt:       "src/hooks/__tests__/a.test.tsx:1:8 — 'React' is defined but never used.",
		})
	}
	ciData, _ := json.Marshal(ci)
	if err := os.WriteFile(ciFailingPath, ciData, 0o644); err != nil {
		t.Fatal(err)
	}

	level := 6
	cfg := &config.Config{
		ACMMLevel: &level,
		Project:   config.ProjectConfig{Org: "acme", PrimaryRepo: "console", Repos: []string{"acme/console", "acme/docs", "acme/hive"}},
		AgentRuntimeConfig: config.AgentRuntimeConfig{
			Agents: map[string]config.AgentConfig{
				"scanner": {Mode: "ISSUES_PRS_MERGE", KickTemplate: "scanner-automerge.md"},
			},
		},
	}
	cfg.Governor.KickLimits = config.KickLimitsConfig{MaxIssues: maxIssues, MaxPRs: maxPRs}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	return New(cfg, logger)
}

func kickSizeActionable(issues, prs int) *github.ActionableResult {
	repos := []string{"acme/console", "acme/docs", "acme/hive"}
	a := &github.ActionableResult{}
	for i := 0; i < issues; i++ {
		a.Issues.Items = append(a.Issues.Items, github.Issue{
			Repo: repos[i%len(repos)], Number: 1000 + i, Title: kickSizeIssueTitle,
			Lane: "scanner", AgeMinutes: 60 * (i + 1), Labels: []string{"bug", "area/dashboard"},
			AuthorIsHuman: i%3 == 0,
		})
	}
	for i := 0; i < prs; i++ {
		a.PRs.Items = append(a.PRs.Items, github.PullRequest{
			Repo: repos[i%len(repos)], Number: 3000 + i, Title: kickSizePRTitle, Author: "octocat",
			MergeableState: "clean",
		})
	}
	a.Issues.Count = issues
	a.PRs.Count = prs
	return a
}

type kickSizeSample struct {
	issues, prs, mergeEligible, ciFailing int
	total, issueList, prList, mergeList   int
}

func measureKick(t *testing.T, maxIssues, maxPRs *int, issues, prs, mergeEligible, ciFailing int) kickSizeSample {
	t.Helper()
	s := kickSizeScheduler(t, maxIssues, maxPRs, mergeEligible, ciFailing)
	a := kickSizeActionable(issues, prs)
	msgs := s.BuildKickMessages(a, []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if !strings.Contains(msgs[0].Message, "Starvation guard") {
		t.Fatalf("shipped scanner-automerge.md did not render:\n%s", msgs[0].Message[:min(len(msgs[0].Message), 400)])
	}
	issueList, _ := s.formatIssueListWithPolicy(a.Issues.Items)
	prList, _ := s.formatPRListWithPolicyForAgent(a, "scanner")
	return kickSizeSample{
		issues: issues, prs: prs, mergeEligible: mergeEligible, ciFailing: ciFailing,
		total:     len(msgs[0].Message),
		issueList: len(issueList),
		prList:    len(prList),
		mergeList: len(s.buildMergeEligibleListFor(nil, false)),
	}
}

// TestKickSizeMeasurement prints the measurement table. It never fails on
// size; TestKickSizeDefaultsStayInsideBudget does that.
func TestKickSizeMeasurement(t *testing.T) {
	if testing.Short() {
		t.Skip("measurement table only; the guard is TestKickSizeDefaultsStayInsideBudget")
	}
	cases := []struct{ issues, prs, me, ci int }{
		{0, 0, 0, 0},
		{10, 5, 2, 1},
		{25, 10, 5, 2},
		{40, 20, 10, 5},
		{60, 30, 15, 5},
		{100, 50, 25, 5},
		{150, 80, 50, 10},
		{302, 302, 100, 20},
	}
	var b strings.Builder
	fmt.Fprintf(&b, "\nscanner-automerge.md, default caps (max_issues=%d, max_prs=%d)\n", config.DefaultMaxIssuesPerKick, config.DefaultMaxPRsPerKick)
	fmt.Fprintf(&b, "%-8s %-6s %-8s %-6s | %-10s %-10s %-10s %-10s\n", "issues", "prs", "merge-el", "ci", "total", "issuelist", "prlist", "mergelist")
	for _, c := range cases {
		m := measureKick(t, nil, nil, c.issues, c.prs, c.me, c.ci)
		fmt.Fprintf(&b, "%-8d %-6d %-8d %-6d | %-10s %-10s %-10s %-10s\n", c.issues, c.prs, c.me, c.ci, kib(m.total), kib(m.issueList), kib(m.prList), kib(m.mergeList))
	}
	forty, twenty := 40, 20
	fmt.Fprintf(&b, "\nscanner-automerge.md, Bob's proposal (max_issues=40, max_prs=20)\n")
	for _, c := range cases[3:] {
		m := measureKick(t, &forty, &twenty, c.issues, c.prs, c.me, c.ci)
		fmt.Fprintf(&b, "%-8d %-6d %-8d %-6d | %-10s %-10s %-10s %-10s\n", c.issues, c.prs, c.me, c.ci, kib(m.total), kib(m.issueList), kib(m.prList), kib(m.mergeList))
	}
	empty := measureKick(t, nil, nil, 0, 0, 0, 0)
	fmt.Fprintf(&b, "\nfixed cost (policy + seams + repos, empty lists): %s\n", kib(empty.total))
	t.Log(b.String())
}

func kib(n int) string { return fmt.Sprintf("%.1f KiB", float64(n)/1024) }

// The lists' contribution at the shipped default caps — every list full on
// the largest shipped template, against the 302-PR backlog that produced the
// 69.5 KiB kick (kick_pr_cap_test.go) — must stay bounded. The guard is on the
// DELTA over an empty-list kick, not the total, so a policy-text edit cannot
// trip it but a formatter or cap regression (the 36 KiB PR list) does.
func TestKickSizeDefaultsStayInsideBudget(t *testing.T) {
	empty := measureKick(t, nil, nil, 0, 0, 0, 0)
	full := measureKick(t, nil, nil, 302, 302, 100, 20)
	lists := full.total - empty.total
	if lists > maxDefaultCapListBytes {
		t.Fatalf("lists at default caps add %s to the kick, over the %s budget (issue list %s, PR list %s, merge list %s)",
			kib(lists), kib(maxDefaultCapListBytes), kib(full.issueList), kib(full.prList), kib(full.mergeList))
	}
	t.Logf("default caps: fixed cost %s, lists add %s (issue list %s, PR list %s, merge-eligible list %s), total %s",
		kib(empty.total), kib(lists), kib(full.issueList), kib(full.prList), kib(full.mergeList), kib(full.total))
}

// maxDefaultCapListBytes bounds what the capped lists may add to a kick.
// Measured 30.5 KiB at the shipped defaults (100 issues ≈ 11.5 KiB, 50 PRs ≈
// 6.9 KiB, 50 merge-eligible ≈ 4.8 KiB, 20 CI-failing with excerpts ≈ 7 KiB);
// the headroom is for longer titles and labels, not for a new list.
const maxDefaultCapListBytes = 40 * 1024

// Every rendered kick logs its byte size next to the list counts and caps
// that produced it (#12133), so operators can tune governor.kick_limits from
// the hive log instead of the 32 KiB-truncated prompt history.
func TestKickSizeLogLine(t *testing.T) {
	ten := 10
	s := kickSizeScheduler(t, &ten, &ten, 0, 0)
	var buf bytes.Buffer
	s.logger = slog.New(slog.NewTextHandler(&buf, nil))
	a := kickSizeActionable(25, 12)
	msgs := s.BuildKickMessages(a, []string{"scanner"})
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	var line string
	for _, l := range strings.Split(buf.String(), "\n") {
		if strings.Contains(l, `msg="kick rendered"`) {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("no 'kick rendered' line in log:\n%s", buf.String())
	}
	for _, want := range []string{
		"agent=scanner",
		fmt.Sprintf("kick_bytes=%d", len(msgs[0].Message)),
		"issues_total=25", "issues_listed=10",
		"prs_total=12", "prs_listed=10",
		"max_issues=10", "max_prs=10",
	} {
		if !strings.Contains(line, want) {
			t.Errorf("kick rendered line missing %q:\n%s", want, line)
		}
	}
}
