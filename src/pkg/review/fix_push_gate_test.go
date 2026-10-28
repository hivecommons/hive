package review

import (
	"strings"
	"testing"
	"time"
)

// The fix gate (hivecommons/hive#8421): a review-fix kick tells an agent to
// push onto the PR branch, so it may only follow a changes_requested verdict
// on a PR the hive itself opened. all_authors widens what is REVIEWED and
// must never imply pushing; review.fix_human_prs no longer does either,
// because the hive-signed commit fails the squash sign-off gate
// (hivecommons/hive#12897).

func changesRequestedFor(pr PullRequest) Aggregate {
	return Aggregate{Repo: "acme/" + pr.Repo, Number: pr.Number, HeadSHA: pr.HeadSHA, Verdict: VerdictChangesRequested,
		Reasons: []string{"guard nil input"}}
}

func fixGateOpts(fixHumanPRs bool) DispatchOptions {
	return DispatchOptions{
		RequireApproval: true,
		FanOut:          true,
		AllAuthors:      true,
		FixHumanPRs:     fixHumanPRs,
		PostComments:    true,
		ProjectOrg:      "acme",
		AIAuthor:        "hive-bot[bot]",
		Agents: []AgentCapability{
			reviewer("r1"),
			{Name: DefaultFixerAgent, Enabled: true, UsesKick: true},
		},
	}
}

// TestHumanPRFixWithheldWhenFixHumanPRsOff is the incident: all_authors on,
// a person's PR, changes_requested. The review stands; nothing is pushed;
// the refusal is recorded once with the PR, the author and the setting.
func TestHumanPRFixWithheldWhenFixHumanPRsOff(t *testing.T) {
	pr := humanPR("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))

	if len(plan.FixKicks) != 0 {
		t.Fatalf("fix kick dispatched for a human-authored PR with fix_human_prs off: %+v", plan.FixKicks)
	}
	if len(plan.State.Human) != 0 {
		t.Fatalf("withholding a push is not a human hold: %+v", plan.State.Human)
	}
	if len(plan.WithheldFixes) != 1 || len(plan.State.Withheld) != 1 {
		t.Fatalf("withheld = plan %d / state %d, want 1 / 1", len(plan.WithheldFixes), len(plan.State.Withheld))
	}
	w := plan.WithheldFixes[0]
	if w.Repo != "acme/hive" || w.Number != 7 || w.HeadSHA != "sha1" {
		t.Fatalf("withheld entry names the wrong PR: %+v", w)
	}
	if w.Author != "clubanderson" {
		t.Fatalf("withheld entry must carry the PR author for the audit trail, got %q", w.Author)
	}
	if w.Setting != WithheldFixSetting {
		t.Fatalf("withheld entry must cite the setting that permits the push, got %q", w.Setting)
	}
	// Pin the literal: the audit trail and the docs name this key, so the
	// constant cannot drift away from it unnoticed.
	if WithheldFixSetting != "review.fix_human_prs" {
		t.Fatalf("WithheldFixSetting = %q, want review.fix_human_prs", WithheldFixSetting)
	}
	if w.Withheld.IsZero() {
		t.Fatal("withheld entry has no timestamp")
	}

	// Reported once per head: the next cycle finds the state entry and
	// produces nothing new to audit, while the state keeps the record.
	again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, plan.State, fixGateOpts(false))
	if len(again.WithheldFixes) != 0 {
		t.Fatalf("refusal re-reported on the next cycle: %+v", again.WithheldFixes)
	}
	if len(again.State.Withheld) != 1 || len(again.FixKicks) != 0 {
		t.Fatalf("second cycle: withheld=%d fixes=%d, want 1 / 0", len(again.State.Withheld), len(again.FixKicks))
	}
}

// TestHumanPRFixWithheldEvenWhenFixHumanPRsOn is hivecommons/hive#12897: a
// fix pushed onto a contributor's PR is signed off by the hive, and the
// squash sign-off gate rejects it as bot-signoff. So fix_human_prs on still
// withholds the push, and the audit entry records that the setting was on
// and why the fix went into the review instead.
func TestHumanPRFixWithheldEvenWhenFixHumanPRsOn(t *testing.T) {
	pr := humanPR("sha1")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(true))

	if len(plan.FixKicks) != 0 {
		t.Fatalf("fix kick dispatched for a human-authored PR with fix_human_prs on: %+v", plan.FixKicks)
	}
	if len(plan.WithheldFixes) != 1 || len(plan.State.Withheld) != 1 {
		t.Fatalf("withheld = plan %d / state %d, want 1 / 1", len(plan.WithheldFixes), len(plan.State.Withheld))
	}
	w := plan.WithheldFixes[0]
	if !w.SettingEnabled || w.Setting != WithheldFixSetting {
		t.Fatalf("withheld entry must record %s as on: %+v", WithheldFixSetting, w)
	}
	if !strings.Contains(w.Reason, "squash sign-off gate") {
		t.Fatalf("withheld reason must name the sign-off gate, got %q", w.Reason)
	}

	off := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))
	if len(off.WithheldFixes) != 1 || off.WithheldFixes[0].SettingEnabled {
		t.Fatalf("fix_human_prs off must be recorded as off: %+v", off.WithheldFixes)
	}
}

// TestAgentPRFixUnaffectedByFixHumanPRs guards the hive's own PRs: the gate
// is about other people's branches, and must not touch agent-authored work
// in either position of the toggle.
func TestAgentPRFixUnaffectedByFixHumanPRs(t *testing.T) {
	for _, on := range []bool{false, true} {
		pr := dispatchPR("sha1")
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(on))
		if len(plan.FixKicks) != 1 {
			t.Fatalf("fix_human_prs=%v: agent-authored PR got %d fix kicks, want 1", on, len(plan.FixKicks))
		}
		if len(plan.WithheldFixes) != 0 {
			t.Fatalf("fix_human_prs=%v: agent-authored PR was withheld: %+v", on, plan.WithheldFixes)
		}
	}
}

// TestHiveMediatedPRUnderPersonLoginIsWithheld: a PR an agent opened on a
// person's credentials shows the person as author, and the squash is
// attributed to that login. The attribution trailer and the audit-trail agent
// still make it the hive's own for review, but a hive-signed fix would fail
// the squash sign-off gate as bot-signoff, so the push is withheld and the
// fix is proposed in the review (hivecommons/hive#13124).
func TestHiveMediatedPRUnderPersonLoginIsWithheld(t *testing.T) {
	attributed := humanPR("sha1")
	attributed.HiveAttributed = true
	audited := humanPR("sha1")
	audited.AuthorAgent = "scanner"
	for name, pr := range map[string]PullRequest{"hive_attributed": attributed, "author_agent": audited} {
		plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))
		if len(plan.FixKicks) != 0 || len(plan.WithheldFixes) != 1 || len(plan.State.Human) != 0 {
			t.Fatalf("%s: fixes=%d withheld=%d human=%d, want 0 / 1 / 0", name, len(plan.FixKicks), len(plan.WithheldFixes), len(plan.State.Human))
		}
		if got := plan.WithheldFixes[0].Author; got != pr.Author {
			t.Fatalf("%s: withheld entry author = %q, want %q", name, got, pr.Author)
		}
		review := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, fixGateOpts(false))
		if len(review.ReviewKicks) != 1 || !strings.Contains(review.ReviewKicks[0].Message, "PROPOSE FIXES, DO NOT PUSH THEM.") {
			t.Fatalf("%s: review prompt lacks the propose-only instruction", name)
		}
	}
}

func TestAdoptedContributorPRIsHiveOwnedForFixes(t *testing.T) {
	pr := humanPR("sha1")
	pr.Labels = []string{"stage/074-adopted-by-hive", "author/clanker"}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, fixGateOpts(false))

	if len(plan.FixKicks) != 1 || len(plan.WithheldFixes) != 0 {
		t.Fatalf("adopted contributor PR fixes=%d withheld=%d, want 1 / 0", len(plan.FixKicks), len(plan.WithheldFixes))
	}
}

func TestAdoptedPendingFixIsRekickedEveryCycle(t *testing.T) {
	pr := humanPR("sha1")
	pr.Labels = []string{"stage/074-adopted-by-hive", "author/clanker"}
	state := DispatchState{
		Fixes:       []PendingFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 3, Dispatched: time.Now().Add(-4 * time.Hour)}},
		FixAttempts: []FixAttempt{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 3, Dispatched: time.Now().Add(-4 * time.Hour)}},
	}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, state, fixGateOpts(false))

	if len(plan.FixKicks) != 1 {
		t.Fatalf("adopted PR got %d fix kicks, want 1", len(plan.FixKicks))
	}
	if len(plan.State.Fixes) != 1 || plan.State.Fixes[0].Attempts != 4 {
		t.Fatalf("pending fixes = %+v, want a fourth attempt", plan.State.Fixes)
	}
}

func TestNonAdoptedPendingFixIsNotRekicked(t *testing.T) {
	pr := humanPR("sha1")
	pr.HiveAttributed = true
	state := DispatchState{Fixes: []PendingFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 1, Dispatched: time.Now()}}}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, state, fixGateOpts(false))

	if len(plan.FixKicks) != 0 || len(plan.State.Fixes) != 1 || plan.State.Fixes[0].Attempts != 1 {
		t.Fatalf("ordinary pending fix changed: fixes=%d state=%+v", len(plan.FixKicks), plan.State.Fixes)
	}
	// A hive-attributed PR under a person login never reaches the fix path
	// (hivecommons/hive#13124): the push is withheld, so the exclusion is
	// not-hive-authored rather than pending. Either way it is not re-kicked.
	if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedNotHiveAuthored {
		t.Fatalf("fix exclusions = %+v, want not-hive-authored exclusion", plan.FixExclusions)
	}
	if len(plan.WithheldFixes) != 1 {
		t.Fatalf("withheld fixes = %+v, want the hive-attributed person-login PR withheld", plan.WithheldFixes)
	}
}

func TestAdoptedFixAttemptsExhaustedIsVisibleHumanHold(t *testing.T) {
	pr := humanPR("sha1")
	pr.Labels = []string{"stage/074-adopted-by-hive", "author/clanker"}
	state := DispatchState{
		Fixes:       []PendingFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 6, Dispatched: time.Now()}},
		FixAttempts: []FixAttempt{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Agent: DefaultFixerAgent, Attempts: 6, Dispatched: time.Now()}},
	}

	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, state, fixGateOpts(false))

	if len(plan.FixKicks) != 0 {
		t.Fatalf("exhausted adopted PR got %d fix kicks, want 0", len(plan.FixKicks))
	}
	if len(plan.State.Human) != 1 || plan.State.Human[0].Reason != FixAttemptsExhaustedHumanDecisionReason {
		t.Fatalf("human hold = %+v, want %q", plan.State.Human, FixAttemptsExhaustedHumanDecisionReason)
	}
	if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedAttemptsExhausted || plan.FixExclusions[0].Attempts != 6 {
		t.Fatalf("fix exclusions = %+v, want exhausted attempts=6", plan.FixExclusions)
	}
}

// TestWithheldFixPrunedWhenHeadMoves: a new push is a new decision. The
// record for the old head goes with it, like pending reviews and holds.
func TestWithheldFixPrunedWhenHeadMoves(t *testing.T) {
	state := DispatchState{Withheld: []WithheldFix{{Repo: "acme/hive", Number: 7, HeadSHA: "sha1", Setting: WithheldFixSetting, Withheld: time.Now().UTC()}}}
	pr := humanPR("sha2")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, state, fixGateOpts(false))
	if len(plan.State.Withheld) != 0 {
		t.Fatalf("stale withheld entry survived a head change: %+v", plan.State.Withheld)
	}
}

// TestHumanPRReviewPromptProposesFixesInComment: with the push withheld, the
// reviewer is told to carry the fix in the comment as a suggestion or patch,
// and not to touch the branch — with fix_human_prs on too
// (hivecommons/hive#12897). The hive's own PRs get the ordinary prompt.
func TestHumanPRReviewPromptProposesFixesInComment(t *testing.T) {
	const marker = "PROPOSE FIXES, DO NOT PUSH THEM."
	cases := []struct {
		name     string
		pr       PullRequest
		on       bool
		combined bool
		want     bool
	}{
		{name: "human_off", pr: humanPR("sha1"), want: true},
		{name: "human_off_combined", pr: humanPR("sha1"), combined: true, want: true},
		{name: "human_on", pr: humanPR("sha1"), on: true, want: true},
		{name: "human_on_combined", pr: humanPR("sha1"), on: true, combined: true, want: true},
		{name: "agent_off", pr: dispatchPR("sha1")},
		{name: "agent_on", pr: dispatchPR("sha1"), on: true},
	}
	for _, tc := range cases {
		opts := fixGateOpts(tc.on)
		opts.CombinedPerspectives = tc.combined
		plan := PlanDispatch([]PullRequest{tc.pr}, Artifact{}, DispatchState{}, opts)
		if len(plan.ReviewKicks) != 1 {
			t.Fatalf("%s: got %d review kicks, want 1", tc.name, len(plan.ReviewKicks))
		}
		msg := plan.ReviewKicks[0].Message
		if got := strings.Contains(msg, marker); got != tc.want {
			t.Fatalf("%s: propose-only instruction present=%v, want %v:\n%s", tc.name, got, tc.want, msg)
		}
		if tc.want && (!strings.Contains(msg, "```suggestion") || !strings.Contains(msg, "```diff") || !strings.Contains(msg, "review.fix_human_prs")) {
			t.Fatalf("%s: propose-only instruction lacks the suggestion/patch guidance or the setting name:\n%s", tc.name, msg)
		}
	}
}

func TestWithheldFixExclusionReportedOncePerHead(t *testing.T) {
	pr := humanPR("sha1")
	pr.HiveAttributed = true
	arts := Artifact{Items: []Aggregate{changesRequestedFor(pr)}}

	first := PlanDispatch([]PullRequest{pr}, arts, DispatchState{}, fixGateOpts(false))
	if len(first.FixExclusions) != 1 || len(first.WithheldFixes) != 1 {
		t.Fatalf("first cycle: exclusions=%+v withheld=%+v, want one of each", first.FixExclusions, first.WithheldFixes)
	}

	second := PlanDispatch([]PullRequest{pr}, arts, first.State, fixGateOpts(false))
	if len(second.FixExclusions) != 0 || len(second.WithheldFixes) != 0 {
		t.Fatalf("second cycle re-reported the same head: exclusions=%+v withheld=%+v", second.FixExclusions, second.WithheldFixes)
	}
	if len(second.State.Withheld) != 1 {
		t.Fatalf("withheld state = %+v, want the single entry kept", second.State.Withheld)
	}
}
