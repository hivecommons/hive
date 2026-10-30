package automerge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Merge commit for mixed authorship (follow-up to hivecommons/hive#13623 and
// hivecommons/hive#13636).
//
// The `Sign-off survives the squash` gate exists so a squash cannot launder
// authorship: GitHub writes the landing commit as the PR author while
// keeping the branch commit's Signed-off-by, so a hive commit signed as the
// App would land as the contributor with a bot's sign-off. The hive used to
// create exactly that shape on a PR it adopted in place (its fix commit
// carries the App's sign-off), see it, and park the PR at stage/085 asking
// a maintainer to merge with a merge commit. Every commit on such a head is
// signed off by its own author, so a merge commit lands them all unchanged
// and the invariant holds without anyone rewriting the contributor's
// history. For a role/trusted PR the lane now makes that choice itself:
//   - the stage pass elects a merge commit when the head mixes contributor
//     commits with hive commits, every commit is signed, and the repo is
//     not squash-only (auto_merge.squash_only); it writes
//     `merge-method/merge`, leaves one note per head once that write
//     landed (a write the write-back gate suppressed refuses the election
//     on the next pass instead), and does not route
//     the PR to 071/072 or 085 on hive-signoff-attribution;
//   - the attribution check (.github/workflows/dco-squash-attribution.yml)
//     reads the label and accepts a bot's self-signed commit under it;
//   - the merge sweeps honour the label with merge_method=merge and record
//     the reason in the merge ledger.
// Where the election is refused the previous behaviour stands, and an
// adopted head's stage/085 note names why.

// mergeMethodReasonMixedAuthorship is the merge-ledger reason recorded when
// the label selects the method.
const mergeMethodReasonMixedAuthorship = "merge commit: mixed authorship, each commit self-signed"

// MergeCommitMarkerPrefix opens the one note per head explaining the label.
const MergeCommitMarkerPrefix = "<!-- hive:merge-commit"

// Why mergeCommitElection refused (stageDetail.MergeCommitRefused).
const (
	mergeCommitRefusedRole       = "role-not-trusted"
	mergeCommitRefusedSquashOnly = "squash-only"
	mergeCommitRefusedUnsigned   = "unsigned-commits"
	mergeCommitRefusedHiveOnly   = "no-contributor-commits"
	// mergeCommitRefusedLabelsSuppressed: the write-back gate kept the
	// hive's labels off the PR, so `merge-method/merge` cannot land
	// (hivecommons/hive#15710).
	mergeCommitRefusedLabelsSuppressed = "labels-suppressed"
)

type mergeCommitDecision struct {
	Elected bool
	Refused string
}

// mergeCommitElection decides whether a head with squash-unsafe hive
// commits is merged with a merge commit instead of being sent back to its
// author. Requires: role/trusted (the hive will merge it at all), every
// commit signed (a missing sign-off is the DCO lane's problem, not a merge
// method's), contributor commits alongside the hive's (mixed authorship),
// and a repo whose policy allows merge commits.
func mergeCommitElection(stats dcoStats, role stagelabels.Role, policy StageLabelPolicy, display string) mergeCommitDecision {
	if stats.squashUnsafeHive <= 0 {
		return mergeCommitDecision{}
	}
	switch {
	case role != stagelabels.RoleTrusted:
		return mergeCommitDecision{Refused: mergeCommitRefusedRole}
	case stats.unsigned > 0:
		return mergeCommitDecision{Refused: mergeCommitRefusedUnsigned}
	case stats.total <= stats.squashUnsafeHive:
		return mergeCommitDecision{Refused: mergeCommitRefusedHiveOnly}
	case policy.SquashOnly != nil && policy.SquashOnly(display):
		return mergeCommitDecision{Refused: mergeCommitRefusedSquashOnly}
	}
	return mergeCommitDecision{Elected: true}
}

// MergeCommitComment is the one note left when the lane elects a merge
// commit for a head.
func MergeCommitComment(obs stageObservation, stats dcoStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", MergeCommitMarkerPrefix, obs.HeadSHA)
	fmt.Fprintf(&b, "%d of %d commits on `%s` are the hive's, signed off as the hive App, and the rest are @%s's with their own sign-off. ", stats.squashUnsafeHive, stats.total, shortSHA(obs.HeadSHA), nonEmpty(obs.Author, "the contributor"))
	b.WriteString("A squash would land the hive's commits under @")
	b.WriteString(nonEmpty(obs.Author, "the contributor"))
	b.WriteString("'s name with the App's sign-off, so the hive will merge this PR with a **merge commit** instead (`")
	b.WriteString(stagelabels.MergeCommitLabel)
	b.WriteString("`): every commit lands as it is, each with its own author's sign-off. Nothing is rewritten and nobody needs to act. ")
	b.WriteString("Maintainers merging by hand: use `gh pr merge --merge`, not squash.")
	return b.String()
}

// noteMergeCommit posts MergeCommitComment once per head.
func (c *Engine) noteMergeCommit(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, stats dcoStats) (int, error) {
	if obs.HeadSHA == "" {
		return 0, nil
	}
	if rec, ok := store.Get(key); ok && rec.MergeCommitNoted(obs.HeadSHA) {
		return 0, nil
	}
	if stagelabels.HasMergeCommitLabel(obs.Labels) {
		// Labelled before this process remembered it (restart, or a
		// maintainer did it): the label is the note.
		store.MarkMergeCommitNoted(key, obs.HeadSHA)
		return 0, nil
	}
	c.info("stage labels: merge commit elected for mixed authorship", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "author", obs.Author, "hive_commits", stats.squashUnsafeHive, "total", stats.total)
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(MergeCommitComment(obs, stats))}); err != nil {
		c.warn("stage labels: merge-commit note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkMergeCommitNoted(key, obs.HeadSHA)
	return 1, nil
}

// mergeMethodWithReason is mergeMethodFor plus the ledger reason when
// something other than the repository's convention chose the method. The
// `merge-method/merge` label wins over the squash convention and is never
// downgraded: a repo that rejects merge commits fails the merge call
// loudly rather than squashing a head the label says must not be squashed.
func (c *Engine) mergeMethodWithReason(ctx context.Context, owner, repo string, pr *gh.PullRequest) (method, reason string) {
	var client *gh.Client
	var logger *slog.Logger
	if c != nil {
		client, logger = c.gh, c.logger
	}
	if pr != nil && stagelabels.HasMergeCommitLabel(labelNames(pr.Labels)) {
		return "merge", mergeMethodReasonMixedAuthorship
	}
	return hgithub.ResolveForwardMergeMethod(hgithub.WithRESTCaller(ctx, "hive:automerge_sweep"), client, logger, owner, repo, pr, "squash"), ""
}

// mergeClaimInputs is the merge effect's recorded inputs, with the method
// reason when one applies.
func mergeClaimInputs(method, reason, expectSHA, lane string) map[string]string {
	in := map[string]string{"method": method, "expect_sha": expectSHA, "lane": lane}
	if reason != "" {
		in["method_reason"] = reason
	}
	return in
}
