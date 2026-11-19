package stagelabels

// Agent sign-off rules shared by every kick that sends an agent to commit on
// a PR branch (hivecommons/hive#19267 and its follow-up audit).
//
// `Sign-off survives the squash` (.github/workflows/dco-squash-attribution.yml)
// fails a PR whose squash would land a commit under the PR author's name with
// a Signed-off-by that does not name that author. On a person's PR that is
// every commit an agent authors (its own sign-off) and every person's commit
// an agent re-signs. Co-authored-by does not count as a sign-off there, so
// the only compliant agent work on such a branch is a rebase that replays the
// person's commits with their own trailers, unless the PR carries
// MergeCommitLabel (a merge commit lands each commit as it is).

// OwnSignoffRebaseExec is the `git rebase --exec` step that amends with
// --signoff only a commit whose author email is the agent's own (git var
// GIT_AUTHOR_IDENT honours the lane's pinned GIT_AUTHOR_* env), so a
// person's commit keeps exactly the trailers its author wrote. A blanket
// `git rebase --signoff` appended the agent's sign-off to a human's commit on
// hivecommons/hive#19102.
const OwnSignoffRebaseExec = `'case "$(git var GIT_AUTHOR_IDENT)" in *"<$(git log -1 --format=%ae)>"*) git commit --amend --no-edit --signoff;; esac'`

// PersonPRAgentCommitRule is the instruction for an agent repairing a PR
// whose GitHub author is a person rather than the hive App or a bot. The
// bad commits on hivecommons/hive#19110, #19102 and #19063 were all agent
// follow-ups pushed to such a branch (citation refreshes and test fixes after
// a rebase, a coverage fix), plus one person's commit re-signed by the lane.
const PersonPRAgentCommitRule = "the squash lands every commit under that person's name, so a commit you author, or your Signed-off-by on a commit they wrote, fails `Sign-off survives the squash` and sends the PR back to them. " +
	"Unless the PR carries `" + MergeCommitLabel + "`, do NOT push a commit of your own to it and NEVER add your Signed-off-by to their commits " +
	"(never commit, amend or rebase their work with `-s`; a rebase replays their commits with only their own trailers). " +
	"Any change beyond resolving a conflict inside their commits (a test fix, refreshed citations, a changelog fragment) goes in one PR comment naming the exact edit for the author; then move on."
