package review

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// A PR another bot opened is that bot's branch: a workflow's promotion or
// release PR is force-pushed from its source branch on the next run, and a
// dependency bot rebases its own. A fix pushed there is overwritten (and, on
// a PR whose tree must equal its source, turns it red until then), so the
// fixer never pushes to it and spends no attempt on it.

func otherBotOpts() DispatchOptions {
	opts := fixGateOpts(false)
	opts.BotAuthorRoute = BotAuthorRoute{Authors: config.DefaultReviewBotAuthors}
	return opts
}

func otherBotPR(sha, login string) PullRequest {
	pr := dispatchPR(sha)
	pr.Author = login
	return pr
}

func TestOtherBotPRFixIsWithheld(t *testing.T) {
	for _, login := range []string{"github-actions[bot]", "renovate[bot]", "dependabot[bot]", "some-release-app[bot]"} {
		t.Run(login, func(t *testing.T) {
			pr := otherBotPR("sha1", login)
			plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, otherBotOpts())

			if len(plan.FixKicks) != 0 || len(plan.State.Fixes) != 0 {
				t.Fatalf("fix kicked onto another bot's branch: kicks=%+v fixes=%+v", plan.FixKicks, plan.State.Fixes)
			}
			if len(plan.State.FixAttempts) != 0 {
				t.Fatalf("a withheld fix spent an attempt: %+v", plan.State.FixAttempts)
			}
			if len(plan.State.Human) != 0 {
				t.Fatalf("withholding the push is not a human hold: %+v", plan.State.Human)
			}
			if len(plan.WithheldFixes) != 1 {
				t.Fatalf("withheld = %d, want 1", len(plan.WithheldFixes))
			}
			w := plan.WithheldFixes[0]
			if w.Author != login || w.Setting != WithheldFixBotSetting || !w.SettingEnabled || !strings.Contains(w.Reason, "another bot ("+login+")") {
				t.Fatalf("withheld entry = %+v, want the bot, setting %s and its reason", w, WithheldFixBotSetting)
			}
			if len(plan.FixExclusions) != 1 || plan.FixExclusions[0].Reason != FixDispatchExcludedOtherBotAuthor || plan.FixExclusions[0].Attempts != 0 {
				t.Fatalf("exclusions = %+v, want one %s with no attempts", plan.FixExclusions, FixDispatchExcludedOtherBotAuthor)
			}

			again := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, plan.State, otherBotOpts())
			if len(again.FixKicks) != 0 || len(again.WithheldFixes) != 0 {
				t.Fatalf("second cycle: kicks=%d withheld=%d, want 0 / 0", len(again.FixKicks), len(again.WithheldFixes))
			}
		})
	}
}

// The hive's own PRs keep their fixer, whichever of its logins opened them.
func TestOwnPRsAreNotAnotherBot(t *testing.T) {
	appOnly := otherBotOpts()
	appOnly.AIAuthor = "maintainer-login"
	appOnly.AppLogin = "hive-bot[bot]"
	plainAIAuthor := otherBotOpts()
	plainAIAuthor.AIAuthor = "hive-bot"
	attributed := otherBotPR("sha1", "release-app[bot]")
	attributed.AuthorAgent = "scanner"
	adopted := otherBotPR("sha1", "github-actions[bot]")
	adopted.Labels = []string{"stage/074-adopted-by-hive"}
	cases := []struct {
		name string
		pr   PullRequest
		opts DispatchOptions
	}{
		{"ai_author", dispatchPR("sha1"), otherBotOpts()},
		{"app login", dispatchPR("sha1"), appOnly},
		{"ai_author without the bot suffix", dispatchPR("sha1"), plainAIAuthor},
		{"agent attribution", attributed, otherBotOpts()},
		{"adopted", adopted, otherBotOpts()},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := PlanDispatch([]PullRequest{c.pr}, Artifact{Items: []Aggregate{changesRequestedFor(c.pr)}}, DispatchState{}, c.opts)
			if len(plan.FixKicks) != 1 || len(plan.WithheldFixes) != 0 {
				t.Fatalf("kicks=%d withheld=%+v, want the fixer dispatched", len(plan.FixKicks), plan.WithheldFixes)
			}
		})
	}
}

// review.bot_authors: [] names no other bots, so the bot-login rule alone
// decides, as before.
func TestOtherBotRuleOffWithNoBotAuthors(t *testing.T) {
	opts := otherBotOpts()
	opts.BotAuthorRoute.Authors = []string{}
	pr := otherBotPR("sha1", "github-actions[bot]")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{Items: []Aggregate{changesRequestedFor(pr)}}, DispatchState{}, opts)
	if len(plan.FixKicks) != 1 {
		t.Fatalf("kicks=%d, want 1 with review.bot_authors empty", len(plan.FixKicks))
	}
}

// The reviewer of another bot's PR is told nobody applies a fix on that
// branch, and that a finding the PR carries over belongs on the base branch.
func TestOtherBotPRReviewPromptRoutesFindingsToBase(t *testing.T) {
	pr := otherBotPR("sha1", "github-actions[bot]")
	plan := PlanDispatch([]PullRequest{pr}, Artifact{}, DispatchState{}, otherBotOpts())
	if len(plan.ReviewKicks) != 1 {
		t.Fatalf("review kicks = %d, want 1", len(plan.ReviewKicks))
	}
	msg := plan.ReviewKicks[0].Message
	for _, want := range []string{"FINDINGS GO TO THE BASE BRANCH, NOT THIS PR.", "opened by github-actions[bot]", "has to be fixed there", "Do not ask the PR's author for a change"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("review prompt lacks %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "PROPOSE FIXES, DO NOT PUSH THEM.") {
		t.Fatal("review prompt also carries the person-author instruction")
	}

	own := PlanDispatch([]PullRequest{dispatchPR("sha1")}, Artifact{}, DispatchState{}, otherBotOpts())
	if len(own.ReviewKicks) != 1 || strings.Contains(own.ReviewKicks[0].Message, "FINDINGS GO TO THE BASE BRANCH") {
		t.Fatal("the hive's own PR got the other-bot instruction")
	}
}
