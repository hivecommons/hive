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
			if got := taskBaseBranch(tc.title, "hivecommons/hive", "v4"); got != tc.want {
				t.Errorf("taskBaseBranch(%q, own repo, \"v4\") = %q, want %q", tc.title, got, tc.want)
			}
		})
	}
}

// Regression coverage for hivecommons/hive#6081.
//
// #5729 (above) established that the prompt must CARRY the base branch. It
// named the wrong one: upstreamBranch() is the branch this hive's BINARY was
// built from, and it was passed through for every task regardless of which
// repository the issue was in. A self-hosted hive built from `v4` therefore
// stamped `v4` onto 100% of its tasks for four repositories whose default
// branch is `main`.
//
// The tests above could not catch it: every one of them dispatches for
// hivecommons/hive, where the hive's own branch happens to be the right
// answer. These dispatch for a foreign repo.
//
// Why it is worse than the observed transcript suggests: that hive
// self-corrected only because a MISSING branch is loud. A repository that HAS
// a `v4` which is not its default resolves, opens the PR against it, and
// satisfies the prompt's own "confirm the PR's base" step.

// promptForRepo renders an assignment prompt for a hive built from hubBranch,
// dispatching for an arbitrary repository.
func promptForRepo(t *testing.T, hubBranch, repoFull, title string) string {
	t.Helper()
	withGitBranch(t, hubBranch)
	return buildTaskPrompt(repoFull, 101, title)
}

// TestBuildTaskPrompt_ForeignRepoDoesNotInheritTheHiveBranch is the reported
// bug: a hive built from v4 dispatching for Danathar/arch-bootc (default
// branch main) must not tell the agent to base its work on v4.
func TestBuildTaskPrompt_ForeignRepoDoesNotInheritTheHiveBranch(t *testing.T) {
	prompt := promptForRepo(t, "v4", "Danathar/arch-bootc", "the installer drops a mount option")

	if strings.Contains(prompt, "--base v4") || strings.Contains(prompt, "'v4' branch") {
		t.Errorf("the hive's own build branch was named as the base for a foreign repo; got: %q", prompt)
	}
	if strings.Contains(prompt, "upstream/v4") {
		t.Errorf("the hive's own build branch was named as the head start point for a foreign repo; got: %q", prompt)
	}
	// Declining to name a branch is only correct because the fallback wording
	// tells the agent how to find the right one.
	if !strings.Contains(prompt, "defaultBranchRef") {
		t.Errorf("prompt must tell the agent to resolve the repository's own default branch; got: %q", prompt)
	}
	if !strings.Contains(prompt, "Danathar/arch-bootc") {
		t.Errorf("the fallback wording must name the repository to resolve; got: %q", prompt)
	}
}

// The load-bearing half of #5729 survives the fix. Whichever wording is used,
// the agent must be told not to trust the branch the checkout is sitting on --
// that is what the reused checkout makes dangerous, and it is the one clause
// both wordings share.
func TestBuildTaskPrompt_ForeignRepoStillForbidsInheritingTheCheckout(t *testing.T) {
	prompt := promptForRepo(t, "v4", "Danathar/arch-bootc", "the installer drops a mount option")

	if !strings.Contains(prompt, "Do not assume the branch the checkout is currently on") {
		t.Errorf("foreign-repo prompt dropped the do-not-inherit-the-checkout clause; got: %q", prompt)
	}
}

// The discriminating counterpart, and the reason this is a narrowing rather
// than a removal: dispatching for the hive's OWN repo must still name the
// branch explicitly, which is all of #5729's value.
func TestBuildTaskPrompt_OwnRepoStillNamesTheHiveBranch(t *testing.T) {
	prompt := promptForRepo(t, "v4", "hivecommons/hive", "the dashboard drops a websocket frame")

	if !strings.Contains(prompt, "gh pr create --base v4") {
		t.Errorf("the hive's own repo lost its explicit base branch; got: %q", prompt)
	}
	if !strings.Contains(prompt, "upstream/v4") {
		t.Errorf("the hive's own repo lost its explicit head start point; got: %q", prompt)
	}
}

// A release-line tag is orthogonal to whose repo it is: an issue titled
// "[v5] ..." is work for v5 in whatever repository it was filed in. The fix
// must not swallow that override on the way past.
func TestBuildTaskPrompt_ForeignRepoKeepsTheReleaseLineOverride(t *testing.T) {
	prompt := promptForRepo(t, "v4", "Danathar/arch-bootc", "[v5] backport the mount-option guard")

	if !strings.Contains(prompt, "gh pr create --base v5") {
		t.Errorf("a [v5] issue lost its release-line base in a foreign repo; got: %q", prompt)
	}
	if strings.Contains(prompt, "--base v4") {
		t.Errorf("the hive's own branch leaked past the release-line override; got: %q", prompt)
	}
}

// TestTaskBaseBranch_ForeignRepoDeclines pins the selection rule directly, so
// a failure names the rule rather than a prompt substring.
func TestTaskBaseBranch_ForeignRepoDeclines(t *testing.T) {
	cases := []struct {
		name  string
		repo  string
		title string
		want  string
	}{
		{"the hive's own repo inherits", "hivecommons/hive", "a plain title", "v4"},
		{"the pre-transfer org still inherits", "kubestellar/hive", "a plain title", "v4"},
		{"owner case does not matter", "HiveCommons/Hive", "a plain title", "v4"},
		{"a foreign repo declines", "Danathar/arch-bootc", "a plain title", ""},
		{"a foreign repo named hive-ish declines", "Danathar/hive-tools", "a plain title", ""},
		{"a fork of hive declines rather than guessing", "Danathar/hive", "a plain title", ""},
		{"an unqualified repo declines", "hive", "a plain title", ""},
		{"an empty repo declines", "", "a plain title", ""},
		// The release line is about the WORK, not the repository, so it wins in
		// either kind of repo.
		{"release line wins in the hive's own repo", "hivecommons/hive", "[v5] something", "v5"},
		{"release line wins in a foreign repo", "Danathar/arch-bootc", "[v5] something", "v5"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := taskBaseBranch(tc.title, tc.repo, "v4"); got != tc.want {
				t.Errorf("taskBaseBranch(%q, %q, \"v4\") = %q, want %q", tc.title, tc.repo, got, tc.want)
			}
		})
	}
}

// The fallback wording is no longer a rarely-reached edge: after #6081 every
// task for a repository other than this one uses it, so it has to carry the
// same operational detail the named-branch wording does. #5729's evidence was
// that an agent follows the instruction it was given, and an instruction with
// no verification step is one nothing checks.
func TestBuildTaskPrompt_FallbackWordingCarriesTheFullProcedure(t *testing.T) {
	prompt := promptForRepo(t, "v4", "Danathar/arch-bootc", "the installer drops a mount option")

	for _, want := range []string{
		"defaultBranchRef",
		"git fetch upstream",
		"git checkout -b <your-branch> upstream/<default-branch>",
		"confirm the PR's base",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("fallback wording is missing %q; got: %q", want, prompt)
		}
	}
}
