package dashboard

import (
	"strings"
	"testing"
)

// Regression coverage for kubestellar/hive#5729.
//
// A contributor relay works one issue at a time out of a single PERSISTENT
// checkout, and nothing resets that checkout between tasks. The task prompt
// told the agent to fork, clone, commit, push and open a PR but never said
// which branch to base it on, so the base was whatever the previous task left
// checked out. On 2026-09-02 one `[v5]`-titled issue (#5617) put the checkout
// on `v5` and the four PRs after it — #5688, #5700, #5705, #5711, three of them
// fixes for defects live on the deployed `v4` — were opened against `v5` too.
// Branch ancestry confirmed inheritance rather than choice: each was 1–2
// commits ahead of `v5` and 64–67 ahead of `v4`, with no `base_ref_changed`
// event on any of them.
//
// The invariant these tests hold: every assignment prompt names the branch its
// work belongs on, and a task following a branch-specific one is told this
// hive's branch rather than the previous task's.

// promptFor renders an assignment prompt for a hive built from hubBranch.
func promptFor(t *testing.T, hubBranch, title string) string {
	t.Helper()
	withGitBranch(t, hubBranch)
	return buildTaskPrompt("hivecommons/hive", 101, title)
}

// TestBuildTaskPrompt_NamesTheBaseBranch is the core assertion: the prompt has
// to CARRY the base. Fixing only the workspace is measurably not enough — a
// working branch reset from `v5` onto `v4` mid-task, holding zero commits and a
// clean tree, was put back on `v5` by the agent, because the plan it had
// already formed said `v5`.
func TestBuildTaskPrompt_NamesTheBaseBranch(t *testing.T) {
	prompt := promptFor(t, "v4", "the dashboard drops a websocket frame")

	if !strings.Contains(prompt, "'v4' branch") {
		t.Errorf("prompt does not name the branch to base the work on; got: %q", prompt)
	}
	if !strings.Contains(prompt, "gh pr create --base v4") {
		t.Errorf("prompt does not tell the agent which base to open the PR against; got: %q", prompt)
	}
	// The head side matters as much as the base side: a branch cut from the
	// leftover checkout carries the previous line's commits even when --base is
	// right, which is what put 64–67 unrelated commits under those five PRs.
	if !strings.Contains(prompt, "upstream/v4") {
		t.Errorf("prompt does not tell the agent to start its work branch from the base; got: %q", prompt)
	}
}

// TestBuildTaskPrompt_TaskAfterBranchSpecificOneGetsTheHiveBranch is the
// session-level regression the bug actually is. #5617 was legitimately `[v5]`
// work; #5681 immediately after it was not, and inherited `v5` anyway. Rendering
// both in order pins that the second prompt is not influenced by the first.
func TestBuildTaskPrompt_TaskAfterBranchSpecificOneGetsTheHiveBranch(t *testing.T) {
	branchSpecific := promptFor(t, "v4", "[v5] reviewer lane follow-ups from the adjudication run")
	if !strings.Contains(branchSpecific, "gh pr create --base v5") {
		t.Fatalf("a [v5] issue must be based on v5; got: %q", branchSpecific)
	}

	next := promptFor(t, "v4", "contributor task lease is not released on restart")
	if !strings.Contains(next, "gh pr create --base v4") {
		t.Errorf("the task after a branch-specific one must be based on the hive's own branch; got: %q", next)
	}
	if strings.Contains(next, "v5") {
		t.Errorf("the previous task's branch leaked into the next prompt; got: %q", next)
	}
}

// TestBuildTaskPrompt_BaseFollowsTheHiveBranch pins that the base is derived,
// not re-hardcoded to v4: a hive built from another branch bases its work
// there. This is the same reasoning #3990 recorded for the onboarding page's
// clone command, and the two answers must agree — a contributor who cloned the
// branch that page names should not then be told to base work elsewhere.
func TestBuildTaskPrompt_BaseFollowsTheHiveBranch(t *testing.T) {
	prompt := promptFor(t, "v5", "the dashboard drops a websocket frame")

	if !strings.Contains(prompt, "gh pr create --base v5") {
		t.Errorf("base did not follow the hive's own build branch; got: %q", prompt)
	}
	if strings.Contains(prompt, "--base v4") {
		t.Error("base is still pinned to v4 rather than derived")
	}
}

// TestBuildTaskPrompt_UnknownBuildBranchFallsBack: a build with no injected
// branch (local `go run`, where versionBranch stays "unknown") must not emit
// "unknown" as a base, and must still forbid inheriting the checkout's branch.
func TestBuildTaskPrompt_UnknownBuildBranchFallsBack(t *testing.T) {
	for _, branch := range []string{"", "unknown"} {
		t.Run("branch="+branch, func(t *testing.T) {
			prompt := promptFor(t, branch, "the dashboard drops a websocket frame")

			if strings.Contains(prompt, "--base unknown") {
				t.Fatal("a build with no injected branch emitted an unusable base")
			}
			if !strings.Contains(prompt, "gh pr create --base "+defaultUpstreamBranch) {
				t.Errorf("fallback did not use defaultUpstreamBranch (%q); got: %q", defaultUpstreamBranch, prompt)
			}
		})
	}
}

// TestBuildTaskPromptBody_UnresolvedBaseStillForbidsInheritance: an unresolved
// base is the one case where the prompt cannot name a branch. It must still not
// leave the agent to inherit one — a silent wrong base is precisely the failure
// this change exists to remove — so it names the substitute instead.
func TestBuildTaskPromptBody_UnresolvedBaseStillForbidsInheritance(t *testing.T) {
	prompt := buildTaskPromptBody("hivecommons/hive", "hivecommons/hive#101", "a title", "", "")

	if !strings.Contains(prompt, "Do not assume the branch the checkout is currently on") {
		t.Errorf("prompt with no resolvable base must still forbid inheriting one; got: %q", prompt)
	}
	if !strings.Contains(prompt, "defaultBranchRef") {
		t.Errorf("prompt must name how to resolve the repository's default branch; got: %q", prompt)
	}
	if strings.Contains(prompt, "--base ''") || strings.Contains(prompt, "'' branch") {
		t.Errorf("an empty base leaked into the prompt as a literal; got: %q", prompt)
	}
}

// TestTaskBaseBranch pins the selection rule itself, including the shapes that
// must NOT be read as a branch. The classifier already routes on "[quality]"
// and friends (pkg/classify), so a rule that grabbed any bracketed prefix would
// send every lane-tagged issue to a branch that does not exist.
func TestTaskBaseBranch(t *testing.T) {
	cases := []struct {
		name  string
		title string
		want  string
	}{
		{"plain title uses the hive branch", "reviewer lane follow-ups", "v4"},
		{"release-line tag wins", "[v5] reviewer lane follow-ups", "v5"},
		{"tag is case-insensitive", "[V5] reviewer lane follow-ups", "v5"},
		{"multi-digit release lines", "[v12] something", "v12"},
		{"leading whitespace is tolerated", "  [v5] something", "v5"},
		{"lane prefixes are not branches", "[quality] tighten the gate", "v4"},
		{"a tag that is not a release line", "[vNext] something", "v4"},
		{"a bare v is not a release line", "[v] something", "v4"},
		{"a tag with trailing words", "[v5 reviewer] something", "v4"},
		{"an unterminated bracket", "[v5 something", "v4"},
		{"a tag that is not leading", "reviewer lane [v5] follow-ups", "v4"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskBaseBranch(tc.title, "v4"); got != tc.want {
				t.Errorf("taskBaseBranch(%q, \"v4\") = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}
