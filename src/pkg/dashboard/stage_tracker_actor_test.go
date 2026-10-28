package dashboard

import (
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// TestStageTrackerStageActorKinds: the payload names who holds the next
// move on every row with an actor, by kind, so the SPA never guesses from
// a login string. hive's hive drew docs#500 (github-actions[bot], 070)
// as "Fixing" with no icon while hive PRs at 070 wore the honeycomb.
func TestStageTrackerStageActorKinds(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	since := now.Add(-2 * time.Hour)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	clock = since
	for _, n := range []int{500, 501, 502, 503, 504, 505} {
		store.Observe(stagelabels.Key("acme/docs", n), stagelabels.Fixing, "label:needs-rebase")
	}
	store.Observe(stagelabels.Key("acme/docs", 506), stagelabels.ReadyForHumanMerge, "requires_human:human-approved@pat")
	store.Observe(stagelabels.Key("acme/docs", 507), stagelabels.ReviewPending, "contributor-lane:no-verdict")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Agents = []FrontendAgent{
		{Name: "fixer", Busy: "working", LastAction: &FrontendAgentLastAction{Repo: "acme/docs", Number: 503, Action: "agent_comment_created", At: now.Add(-3 * time.Minute).Format(time.RFC3339), Current: true}},
		{Name: "idle", Busy: "idle", LastAction: &FrontendAgentLastAction{Repo: "acme/docs", Number: 501, Action: "agent_pr_created", Current: true}},
	}
	status.Repos = []FrontendRepo{{
		Name: "docs", Full: "acme/docs",
		OpenPrs: []any{
			// A foreign bot's conflicting PR the lane filed under 070.
			FrontendPR{PullRequest: github.PullRequest{Number: 500, Title: "docs: resync", Author: "github-actions[bot]", Labels: []string{"stage/070-fixing", "needs-rebase", "stage/stalled"}}},
			// Hive PR at 070, no agent on it right now.
			FrontendPR{PullRequest: github.PullRequest{Number: 501, Title: "Hive fix", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/070-fixing", "author/hive"}}},
			// Clanker's PR.
			FrontendPR{PullRequest: github.PullRequest{Number: 502, Title: "Clanker fix", Author: "arteeh", HiveAttributed: true, Labels: []string{"stage/070-fixing", "author/clanker"}}},
			// Hive PR with an agent on it this moment.
			FrontendPR{PullRequest: github.PullRequest{Number: 503, Title: "Busy fix", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/070-fixing"}}},
			// A person's PR.
			FrontendPR{PullRequest: github.PullRequest{Number: 504, Title: "Human fix", Author: "pat", Labels: []string{"stage/070-fixing", "author/human"}}},
			// Held: nobody's move.
			FrontendPR{PullRequest: github.PullRequest{Number: 505, Title: "Held", Author: "pat", Labels: []string{"stage/070-fixing", "hold"}}},
			// 090 with a human approver.
			FrontendPR{PullRequest: github.PullRequest{Number: 506, Title: "Ready", Author: "pat", Labels: []string{"stage/090-ready-for-human-merge"}}},
			// 050: the review swarm's.
			FrontendPR{PullRequest: github.PullRequest{Number: 507, Title: "Reviewing", Author: "pat", Labels: []string{"stage/050-review-pending"}}},
			// No stage at all.
			FrontendPR{PullRequest: github.PullRequest{Number: 508, Title: "Fresh", Author: "pat"}},
		},
	}}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	resp := s.buildStageTracker(status, store, now)

	actor := func(n int) StageTrackerActor {
		t.Helper()
		it := trackerItem(t, resp, "pr", n)
		if it.StageActor == nil {
			t.Fatalf("pr %d: stage_actor missing", n)
		}
		return *it.StageActor
	}

	bot := actor(500)
	if bot.Kind != StageActorBot || bot.Login != "github-actions[bot]" || bot.Since == nil || !bot.Since.Equal(since) || !strings.Contains(bot.Why, "the hive does not fix bot PRs") {
		t.Fatalf("bot PR actor = %+v", bot)
	}
	pr500 := trackerItem(t, resp, "pr", 500)
	if !strings.HasPrefix(pr500.Caption, "Conflicting with the base branch; bot-authored PR (@github-actions[bot]) — the hive does not fix bot PRs") || pr500.Actor != StageActorBot {
		t.Fatalf("bot PR caption = %q actor = %q: a 070 bot PR must not claim a hive agent is fixing it", pr500.Caption, pr500.Actor)
	}

	if a := actor(501); a.Kind != StageActorHive || a.Login != "hive-app[bot]" || !strings.Contains(a.Why, "no agent is on it this moment") {
		t.Fatalf("hive PR actor = %+v", a)
	}
	if a := actor(502); a.Kind != StageActorClanker || a.Login != "arteeh" || !strings.Contains(a.Why, "contributor agent") {
		t.Fatalf("clanker PR actor = %+v", a)
	}
	if a := actor(503); a.Kind != StageActorHive || a.Login != "fixer" || a.Why != "fixer commented · 3m ago" {
		t.Fatalf("busy hive PR actor = %+v", a)
	}
	if a := actor(504); a.Kind != StageActorHuman || a.Login != "pat" || !strings.Contains(a.Why, "@pat must push a fix") {
		t.Fatalf("human PR actor = %+v", a)
	}
	if a := actor(505); a.Kind != StageActorNone || a.Login != "" || !strings.HasPrefix(a.Why, "on hold") {
		t.Fatalf("held PR actor = %+v", a)
	}
	if a := actor(506); a.Kind != StageActorHuman || a.Login != "pat" {
		t.Fatalf("090 actor = %+v", a)
	}
	if a := actor(507); a.Kind != StageActorHive || a.Login != "" || !strings.HasPrefix(a.Why, "review swarm") {
		t.Fatalf("050 actor = %+v", a)
	}
	if a := actor(508); a.Kind != StageActorNone || !strings.HasPrefix(a.Why, "not in any stage yet") {
		t.Fatalf("unstaged PR actor = %+v", a)
	}
}

func TestStageTrackerStageActorUnits(t *testing.T) {
	for _, tc := range []struct{ author, kind, want string }{
		{"github-actions[bot]", "", StageActorBot},
		{"dependabot[bot]", "", StageActorBot},
		{"hive-app[bot]", authorKindHive, StageActorHive},
		{"arteeh", authorKindClanker, StageActorClanker},
		{"pat", authorKindHuman, StageActorHuman},
		{"", "", StageActorNone},
		{"someone", "", StageActorNone},
	} {
		if got := stageTrackerAuthorActorKind(tc.author, tc.kind); got != tc.want {
			t.Errorf("author %q kind %q → %q, want %q", tc.author, tc.kind, got, tc.want)
		}
	}
	workers := map[string]stageTrackerWorker{"hive#30": {Agent: "scout"}}
	if w, ok := stageTrackerWorkerOn(workers, "hivecommons/hive", 30); !ok || w.Agent != "scout" {
		t.Fatalf("a bare repo name in the audit must match on its tail: %+v %v", w, ok)
	}
	if _, ok := stageTrackerWorkerOn(workers, "hivecommons/hive", 31); ok {
		t.Fatal("other number must not match")
	}
	for _, tc := range []struct {
		d    time.Duration
		want string
	}{{30 * time.Second, "30s"}, {5 * time.Minute, "5m"}, {3 * time.Hour, "3h"}, {72 * time.Hour, "3d"}} {
		if got := stageTrackerShortAgoText(tc.d); got != tc.want {
			t.Errorf("%v → %q, want %q", tc.d, got, tc.want)
		}
	}
}

// TestStageTrackerLinkedPRDrawnUnder reproduces hivecommons/hive#14921:
// one PR whose body says `Closes #14910` and `Closes #14911`, both open
// and listed. Each issue lists the PR once (never twice), the PR nests
// under the lowest-numbered issue, and every listing names that issue as
// drawn_under so the SPA draws the PR's strip exactly once and a compact
// reference on the other parent. Issues riding the PR take its actor.
func TestStageTrackerLinkedPRDrawnUnder(t *testing.T) {
	now := time.Date(2026, 10, 28, 18, 0, 0, 0, time.UTC)
	s := newTestServer()
	clock := now
	store := stagelabels.NewStore("", func() time.Time { return clock })
	s.stageStore = store
	clock = now.Add(-2 * time.Hour)
	store.Observe(stagelabels.Key("acme/hive", 14921), stagelabels.Fixing, "contributor-lane:decision-answered")
	store.Observe(stagelabels.Key("acme/hive", 14910), stagelabels.CoveredByPR, "open PR #14921 covers it (PR at 070-fixing)")
	store.Observe(stagelabels.Key("acme/hive", 14911), stagelabels.CoveredByPR, "open PR #14921 covers it (PR at 070-fixing)")
	clock = now

	status := minimalPayload()
	status.HiveID = "hv-test"
	status.Repos = []FrontendRepo{{
		Name: "hive", Full: "acme/hive",
		ActionableIssues: []any{
			// Both the stage reason and GitHub's linked-PR list name the PR:
			// two sources, one listing.
			github.Issue{Number: 14910, Title: "fetch the linked Linear issue", Author: "hive-app[bot]", Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 14921, State: "open"}}},
			github.Issue{Number: 14911, Title: "render the inlined Linear issue", Author: "hive-app[bot]", Labels: []string{"stage/040-covered-by-pr"},
				LinkedPRs: []github.IssueLinkedPR{{Number: 14921, State: "open"}}},
		},
		OpenPrs: []any{
			FrontendPR{PullRequest: github.PullRequest{Number: 14921, Title: "inline the linked Linear issue", Author: "hive-app[bot]", AppAuthored: true, Labels: []string{"stage/070-fixing", "author/hive"},
				LinkedIssues: []github.PRLinkedIssue{{Number: 14906, State: "closed"}, {Number: 14910}, {Number: 14911}}}},
		},
	}}
	s.statusMu.Lock()
	s.status = status
	s.statusMu.Unlock()
	resp := s.buildStageTracker(status, store, now)

	pr := trackerItem(t, resp, "pr", 14921)
	if pr.NestedUnder != 14910 {
		t.Fatalf("nested_under = %d, want the lowest-numbered open issue 14910", pr.NestedUnder)
	}
	for _, n := range []int{14910, 14911} {
		issue := trackerItem(t, resp, "issue", n)
		if len(issue.LinkedPRs) != 1 {
			t.Fatalf("issue %d linked_prs = %+v, want exactly one listing of #14921", n, issue.LinkedPRs)
		}
		lp := issue.LinkedPRs[0]
		if lp.Number != 14921 || lp.DrawnUnder != 14910 {
			t.Fatalf("issue %d linked PR = number %d drawn_under %d, want 14921 under 14910", n, lp.Number, lp.DrawnUnder)
		}
		if lp.StageActor == nil || lp.StageActor.Kind != StageActorHive {
			t.Fatalf("issue %d linked PR stage_actor = %+v, want the PR's hive actor", n, lp.StageActor)
		}
		if issue.StageActor == nil || issue.StageActor.Kind != StageActorHive {
			t.Fatalf("issue %d riding its PR should take the PR's actor, got %+v", n, issue.StageActor)
		}
	}
}
