package dashboard

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
	"github.com/hivecommons/hive/pkg/github"
)

// Scored live test of 2026-11-17, "which agents are running?": she said
// two were paused against one on the dashboard and named nobody. The
// brief classifies agents the way the sidebar dots do, counts each replica
// as one agent, names the agents that are working and never lists a paused
// one as working.
func TestHiveStatusAgentsMatchDashboard(t *testing.T) {
	agents := []FrontendAgent{
		{Name: "coder", State: "running", Busy: "working", Doing: "fixing #1"},
		{Name: "reviewer", State: "running", Busy: "working", ReplicaBase: "reviewer", ReplicaIndex: 1, ReplicaCount: 3},
		{Name: "reviewer-2", State: "running", Busy: "idle", ReplicaBase: "reviewer", ReplicaIndex: 2, ReplicaCount: 3},
		{Name: "reviewer-3", State: "running", Busy: "working", Paused: true, ReplicaBase: "reviewer", ReplicaIndex: 3, ReplicaCount: 3},
		{Name: "helper", State: "stopped", Busy: "idle", OnDemand: true, Paused: true},
		{Name: "scanner", State: "running", Busy: "idle", OffByCadence: true},
		{Name: "fixer", State: "failed", Busy: "idle"},
	}
	var b strings.Builder
	hiveStatusAgents(&b, agents)
	got := b.String()
	for _, want := range []string{
		"Agents: 7 (each replica counts as one agent, 2 are replicas), 2 working, 1 paused, 0 blocked, 1 idle, 1 off, 1 down, 1 on demand.",
		"Working right now: coder (fixing #1); reviewer.",
		"Paused, not running: reviewer-3.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("agents lack %q:\n%s", want, got)
		}
	}
	working := got[strings.Index(got, "Working right now"):strings.Index(got, "Paused, not running")]
	if strings.Contains(working, "reviewer-3") || strings.Contains(working, "helper") {
		t.Fatalf("a paused or on-demand agent is named as working: %s", working)
	}
	if class := hiveAgentClassOf(agents[4]); class != hiveAgentOnDemand {
		t.Fatalf("a paused on-demand agent is %q on the dashboard, not paused", class)
	}

	// The same counts come out of the brief the attendee reads.
	s := &Server{}
	s.status = &StatusPayload{HiveID: "hive-test", Agents: agents}
	brief, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, "Agents: 7 (each replica counts as one agent, 2 are replicas), 2 working, 1 paused") {
		t.Fatalf("brief agents: %s", brief)
	}

	// More working agents than the cap are counted, not all named.
	var many []FrontendAgent
	for i := 0; i < hiveStatusMaxListed+2; i++ {
		many = append(many, FrontendAgent{Name: fmt.Sprintf("worker-%d", i), State: "running", Busy: "working"})
	}
	b.Reset()
	hiveStatusAgents(&b, many)
	if got := b.String(); !strings.Contains(got, "worker-4 (and 2 more).") || strings.Contains(got, "worker-5") {
		t.Fatalf("working list is capped: %s", got)
	}

	b.Reset()
	hiveStatusAgents(&b, []FrontendAgent{{Name: "fixer", State: "stopped", Busy: "working", Paused: true}})
	if got := b.String(); !strings.Contains(got, "Agents: 1, 0 working, 1 paused, 0 blocked.") || !strings.Contains(got, "No agent is working right now.") || strings.Contains(got, "Working right now") {
		t.Fatalf("a paused agent is never working: %s", got)
	}
}

// roundTwoRepos is n configured repos; the first holds the open items.
func roundTwoRepos(n int) []FrontendRepo {
	repos := make([]FrontendRepo, 0, n)
	for i := 0; i < n; i++ {
		repos = append(repos, FrontendRepo{Name: fmt.Sprintf("r%02d", i), Full: fmt.Sprintf("org/r%02d", i)})
	}
	repos[0].Issues, repos[0].PRs = 40, 10
	return repos
}

// Just after a restart the hive had counted 5 of 13 repos. A partial count
// is never worded as a total.
func TestHiveStatusBriefPartialCountNeverATotal(t *testing.T) {
	const configured, scanned = 13, 5
	repos := roundTwoRepos(configured)
	for i := scanned; i < configured; i++ {
		repos[i].CountsStale = true
		repos[i].CountsStaleSince = "2026-11-17T15:00:00Z"
	}
	s := &Server{}
	s.status = &StatusPayload{HiveID: "hive-test", Repos: repos,
		RestartRefresh: &FrontendRestartRefresh{RestoredAt: "2026-11-17T15:20:00Z", ReposScanned: scanned, ReposTotal: configured}}
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open now: at least 40 issues and at least 10 PRs, counted in 5 of 13 repos so far (a partial count, not a total; still counting org/r05, org/r06, org/r07 and 5 more)",
		"Board pressure (BP): at least 40/10, that is at least 40 open issues and at least 10 open PRs on the hive's board, counted in 5 of 13 repos so far.",
		"say \"at least\" and \"counted in 5 of 13 repos so far\", never a total.",
		"The hive restarted and has re-read 5 of 13 repos so far.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "across 13 repos") || strings.Contains(got, "Open now: 40 issues") {
		t.Fatalf("a partial count is worded as a total: %s", got)
	}

	// One repo's issue list still pending: the issue count is a lower
	// bound, the PR count is complete, and the repo prints no zero.
	repos = roundTwoRepos(configured)
	repos[12].PRs = 2
	repos[12].IssuesSyncing = true
	s.status = &StatusPayload{HiveID: "hive-test", Repos: repos}
	got, _ = s.HiveStatusBrief(context.Background())
	for _, want := range []string{
		"Open now: at least 40 issues, counted in 12 of 13 repos so far (a partial count, not a total; issue list still syncing for org/r12), and 12 PRs across 13 repos",
		"org/r12 issues still syncing, 2 PRs",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "org/r12 0 issues") {
		t.Fatalf("a pending issue list prints a zero: %s", got)
	}
}

// Every count carries its scope, worded the same way each time.
func TestHiveStatusBriefScopeWording(t *testing.T) {
	const configured = 13
	s := &Server{}
	s.status = &StatusPayload{HiveID: "hive-test", Repos: roundTwoRepos(configured)}
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"Open now: 40 issues and 10 PRs across 13 repos (org/r00 40 issues, 10 PRs)",
		" PRs across 13 repos. Held: ",
		" PRs across 13 repos. Blocked: ",
		"that is 40 open issues and 10 open PRs on the hive's board across 13 repos.",
		"Scope of every count here: across 13 repos; say \"across 13 repos\" with each count, or for a question about one repo give that repo's own count and name the repo.",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("brief lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "at least") || strings.Contains(got, "These counts were read") {
		t.Fatalf("a fresh, complete count carries no caveat: %s", got)
	}

	// One configured repo: the scope is that repo, by name.
	s.status = &StatusPayload{HiveID: "hive-test", Repos: roundTwoRepos(1)}
	got, _ = s.HiveStatusBrief(context.Background())
	if !strings.Contains(got, "Open now: 40 issues and 10 PRs in the org/r00 repo") || strings.Contains(got, "across") {
		t.Fatalf("single repo scope: %s", got)
	}

	sc := hiveCountScope{Repos: configured, IssuesCounted: configured, PRsCounted: configured}
	if sc.Scope() != " across 13 repos" || sc.Partial() {
		t.Fatalf("scope %q", sc.Scope())
	}
}

// A snapshot known to be stale carries its age; a fresh one says nothing.
func TestHiveStatusFreshness(t *testing.T) {
	now := time.Date(2026, 11, 17, 16, 0, 0, 0, time.UTC)
	full := hiveCountScope{Repos: 2, IssuesCounted: 2, PRsCounted: 2}
	fresh := &StatusPayload{Timestamp: now.Add(-time.Minute).Format(time.RFC3339)}
	if got := hiveStatusFreshness(fresh, full, now); got != "" {
		t.Fatalf("fresh snapshot: %q", got)
	}
	old := &StatusPayload{Timestamp: now.Add(-10 * time.Minute).Format(time.RFC3339)}
	if got := hiveStatusFreshness(old, full, now); !strings.Contains(got, "These counts were read 10 minutes ago.") || !strings.Contains(got, "one short clause") {
		t.Fatalf("old snapshot: %q", got)
	}
	partial := hiveCountScope{Repos: 2, IssuesCounted: 1, PRsCounted: 2}
	if got := hiveStatusFreshness(fresh, partial, now); !strings.Contains(got, "These counts were read 1 minute ago.") {
		t.Fatalf("partial snapshot: %q", got)
	}
	stale := &StatusPayload{Timestamp: now.Format(time.RFC3339), StaleSince: now.Add(-2 * time.Hour).Format(time.RFC3339)}
	if got := hiveStatusFreshness(stale, full, now); !strings.Contains(got, "last-good figures from 2026-11-17T14:00:00Z (2 hours old).") {
		t.Fatalf("stale snapshot: %q", got)
	}
	for d, want := range map[time.Duration]string{
		20 * time.Second: "under a minute", 4 * time.Minute: "4 minutes", time.Hour: "1 hour", 72 * time.Hour: "3 days",
	} {
		if got := hiveStatusAge(d); got != want {
			t.Errorf("hiveStatusAge(%v) = %q, want %q", d, got, want)
		}
	}
}

// fakeForgeItems is the forge cache's open-items snapshot per repo.
type fakeForgeItems map[string]forgestate.Snapshot

func (f fakeForgeItems) Snapshot(repo string) (forgestate.Snapshot, bool) {
	snap, ok := f[repo]
	return snap, ok
}

// Scored live test of 2026-11-17, "what is pull request 18741 about?": she
// gave the title and author and said she had no description or CI status.
// The lookup carries a summary of the description, the state, review and
// CI status and what is blocking it, all from memory.
func TestHiveItemBriefCarriesSummaryStateReviewCI(t *testing.T) {
	const number = 18741
	body := "<!-- template -->\n## Summary\n\nCuts release v5.301.0 from v5. It bumps the version and compiles the changelog. Third sentence that is left out.\n\n- [x] DCO\n\nFixes #18700\nSigned-off-by: Someone <s@example.com>\n"
	pr := github.PullRequest{Repo: "hivecommons/hive", Number: number, Title: "release: v5.301.0", Author: "clubanderson",
		Labels: []string{"stage/060-in-review", "size/M", "dco-signoff: yes", "lgtm"}, CIStatus: "failure", FailingChecks: []string{"go-test", "lint", "docs", "e2e"},
		MergeableState: "blocked", ReviewState: "changes_requested", RequestedReviewers: []string{"alice"}, BaseRef: "v5",
		CreatedAt: time.Now().Add(-3 * time.Hour)}
	verdict := &github.MergeVerdict{State: github.MergeVerdictBlocked, Reason: "required check go-test is failing"}
	mc := &MetricsCollector{}
	mc.forgeCounts = forgestate.NewCounts(forgestate.CountsOptions{Open: fakeForgeItems{
		"hivecommons/hive": {Repo: "hivecommons/hive", Ready: true, PRs: map[int]forgestate.Item{number: {Number: number, Body: body}},
			Issues: map[int]forgestate.Item{12: {Number: 12, Body: "Held issue body from the cache."}}},
	}})
	s := &Server{deps: &Dependencies{MetricsCollector: mc}}
	s.status = &StatusPayload{HiveID: "hive-test", Repos: []FrontendRepo{{
		Name: "hive", Full: "hivecommons/hive",
		OpenPrs:          []any{FrontendPR{PullRequest: pr, MergeVerdict: verdict}},
		ActionableIssues: []any{github.Issue{Repo: "hivecommons/hive", Number: 7, Title: "bug", Author: "bob", Body: "The scanner crashes on empty repos.", Labels: []string{"kind/bug"}}},
		HeldIssues:       []any{github.HoldItem{Repo: "hivecommons/hive", Number: 12, Title: "parked", Labels: []string{"hold"}}},
		NonActionablePrs: []any{github.RepoWorkPR{Repo: "hivecommons/hive", Number: 30, Title: "wip", Author: "carol", Draft: true, Bucket: "draft", Reason: "draft PR"}},
	}}}
	got, ok := s.HiveItemBrief(context.Background(), number)
	if !ok {
		t.Fatal("PR not found")
	}
	for _, want := range []string{
		`PR hivecommons/hive#18741 "release: v5.301.0" by clubanderson.`,
		"About: Cuts release v5.301.0 from v5. It bumps the version and compiles the changelog.",
		"State: open, opened 3 hours ago.",
		"Review: changes requested, has the lgtm label, waiting on a review from alice.",
		"CI: failure, failing go-test, lint, docs and 1 more.",
		"Blocking it: CI is failing; GitHub reports the merge blocked by required checks or reviews; a reviewer requested changes; required check go-test is failing.",
		"Stage: stage/060-in-review.",
		"Answer in at most three short sentences",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("item brief lacks %q:\n%s", want, got)
		}
	}
	for _, not := range []string{"Third sentence", "Signed-off-by", "template", "Fixes #18700", "[x]"} {
		if strings.Contains(got, not) {
			t.Errorf("item brief carries %q:\n%s", not, got)
		}
	}
	if len(got) > hiveItemMaxChars+len(hiveItemAnswerGuide) {
		t.Fatalf("item brief is %d bytes", len(got))
	}

	issue, ok := s.HiveItemBrief(context.Background(), 7)
	if !ok || !strings.Contains(issue, `Issue hivecommons/hive#7 "bug" by bob. About: The scanner crashes on empty repos.`) ||
		!strings.Contains(issue, "State: open") || !strings.Contains(issue, "Blocking it: nothing recorded.") {
		t.Fatalf("issue brief: %s", issue)
	}
	held, ok := s.HiveItemBrief(context.Background(), 12)
	if !ok || !strings.Contains(held, "About: Held issue body from the cache.") || !strings.Contains(held, "Blocking it: on hold (label hold).") {
		t.Fatalf("held issue brief: %s", held)
	}
	draft, ok := s.HiveItemBrief(context.Background(), 30)
	if !ok || !strings.Contains(draft, "State: open, still a draft.") || !strings.Contains(draft, "the hive holds no description for it") ||
		!strings.Contains(draft, "Blocking it: it is still a draft; draft PR.") {
		t.Fatalf("sidelined draft brief: %s", draft)
	}
	if _, ok := s.HiveItemBrief(context.Background(), 99999); ok {
		t.Fatal("an unknown number is not found")
	}
}

func TestHiveItemSummary(t *testing.T) {
	for body, want := range map[string]string{
		"": "",
		"## What\n\n| a | b |\n\n```go\nx := 1\n```\n":                                             "",
		"See [the RFC](https://example.com/rfc) for **details**! More here: https://example.com/x": "See the RFC for details! More here.",
		"- adds a cache\n- removes the poll loop\n- third item":                                    "adds a cache. removes the poll loop.",
	} {
		if got := hiveItemSummary(body); got != want {
			t.Errorf("hiveItemSummary(%q) = %q, want %q", body, got, want)
		}
	}
	long := hiveItemSummary(strings.Repeat("word ", hiveItemSummaryMaxChars))
	if n := len([]rune(long)); n > hiveItemSummaryMaxChars || !strings.HasSuffix(long, "…") {
		t.Fatalf("long summary is %d runes: %q", n, long)
	}
}

// No repo confirmed since the restart, yet figures exist: the brief says
// they are last known figures, never "counted in 0 of 13 repos".
func TestHiveStatusBriefNoRepoConfirmedYet(t *testing.T) {
	const configured = 13
	repos := roundTwoRepos(configured)
	for i := range repos {
		repos[i].CountsStale = true
		repos[i].CountsStaleSince = "2026-11-17T15:00:00Z"
	}
	s := &Server{}
	s.status = &StatusPayload{HiveID: "hive-test", Repos: repos,
		RestartRefresh: &FrontendRestartRefresh{RestoredAt: "2026-11-17T15:20:00Z", ReposScanned: 0, ReposTotal: configured}}
	got, err := s.HiveStatusBrief(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "at least 40 issues and at least 10 PRs, last known figures, not yet confirmed in any of the 13 repos") {
		t.Errorf("brief lacks the honest scope:\n%s", got)
	}
	if strings.Contains(got, "in 0 of 13") || strings.Contains(got, "across 13 repos") {
		t.Fatalf("contradictory or total wording: %s", got)
	}
	// As repos are re-read the count fills in.
	for i := 0; i < 4; i++ {
		repos[i].CountsStale, repos[i].CountsStaleSince = false, ""
	}
	s.status = &StatusPayload{HiveID: "hive-test", Repos: repos,
		RestartRefresh: &FrontendRestartRefresh{RestoredAt: "2026-11-17T15:20:00Z", ReposScanned: 4, ReposTotal: configured}}
	got, _ = s.HiveStatusBrief(context.Background())
	if !strings.Contains(got, "counted in 4 of 13 repos so far") {
		t.Errorf("a partial count does not fill in:\n%s", got)
	}
}
