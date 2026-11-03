package automerge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
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
// history. For a role/trusted PR, or any PR whose current head a trusted
// human approved, the lane now makes that choice itself:
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
// Where the election is refused and the hive caused the shape (its own
// commits next to the contributor's, or a head it pushed), the PR parks at
// stage/085 with a maintainer note: the contributor did not make those
// commits and is never asked to rewrite them (projectbluefin/testsuite#991,
// where a maintainer merging with a merge commit was the real resolution).
// A head made only of App-signed commits the hive never pushed is a
// person's own commit made with the App's identity (hivecommons/pluk#315)
// and still goes back to its author with the commands to re-sign it.

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
// author. Requires: role/trusted, or a trusted human's approval of exactly
// this head (approvedHead), so the hive will merge it at all; every commit
// signed (a missing sign-off is the DCO lane's problem, not a merge
// method's); contributor commits alongside the hive's (mixed authorship);
// and a repo whose policy allows merge commits. repo is "owner/repo"
// (stageRepoFullName), the form auto_merge.squash_only lists.
func mergeCommitElection(stats dcoStats, role stagelabels.Role, approvedHead bool, policy StageLabelPolicy, repo string) mergeCommitDecision {
	if stats.squashUnsafeHive <= 0 {
		return mergeCommitDecision{}
	}
	// The role check comes last: Refused names the blocker the maintainer
	// note asks about, and an approval cannot clear the other three.
	switch {
	case stats.unsigned > 0:
		return mergeCommitDecision{Refused: mergeCommitRefusedUnsigned}
	case stats.total <= stats.squashUnsafeHive:
		return mergeCommitDecision{Refused: mergeCommitRefusedHiveOnly}
	case policy.SquashOnly != nil && policy.SquashOnly(repo):
		return mergeCommitDecision{Refused: mergeCommitRefusedSquashOnly}
	case role != stagelabels.RoleTrusted && !approvedHead:
		return mergeCommitDecision{Refused: mergeCommitRefusedRole}
	}
	return mergeCommitDecision{Elected: true}
}

// signoffHiveCaused reports whether the hive, not the PR author, put the
// squash-unsafe commits on the head: the contributor's own commits sit next
// to the App-signed ones (mixed authorship), or the hive recorded pushing
// this head or adopted the PR in place. Such a head is a maintainer's to
// land (merge commit) and never goes back to the contributor.
func signoffHiveCaused(stats dcoStats, rec stagelabels.Record, hadPrev bool, head string) bool {
	if stats.squashUnsafeHive <= 0 {
		return false
	}
	if stats.total > stats.squashUnsafeHive {
		return true
	}
	return hadPrev && (rec.HivePushedHead(head) || rec.AdoptedInPlace())
}

// MergeCommitComment is the one note left when the lane elects a merge
// commit for a head.
func MergeCommitComment(obs stageObservation, stats dcoStats) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", MergeCommitMarkerPrefix, obs.HeadSHA)
	fmt.Fprintf(&b, "%d of %d commits on `%s` are the hive's, signed off as the hive App, and the rest are @%s's with their own sign-off. ", stats.squashUnsafeHive, stats.total, shortSHA(obs.HeadSHA), nonEmpty(obs.Author, "the contributor"))
	b.WriteString("A squash would land the hive's commits under @")
	b.WriteString(nonEmpty(obs.Author, "the contributor"))
	b.WriteString("'s name with the App's sign-off, so this PR merges with a **merge commit** instead (`")
	b.WriteString(stagelabels.MergeCommitLabel)
	b.WriteString("`): every commit lands as it is, each with its own author's sign-off. Nothing is rewritten and nobody needs to re-sign anything. ")
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

// squashSignoffCheck is the check-run name of
// .github/workflows/dco-squash-attribution.yml.
const squashSignoffCheck = "Sign-off survives the squash"

func isSquashSignoffCheck(name string) bool {
	return strings.EqualFold(strings.TrimSpace(name), squashSignoffCheck)
}

// squashSignoffGateNote is the sign-off attribution notes' mention of that
// check, or "" when the head's required and reported checks do not include
// it (noteStageDCOGate): the workflow lives in the repositories that carry
// it, not in every repository the hive manages, and the hive's own commit
// check is what holds the head everywhere else.
func squashSignoffGateNote(obs stageObservation) string {
	d := obs.Detail
	if !d.SquashSignoffGate || (d.DCOGateHead != "" && obs.HeadSHA != "" && d.DCOGateHead != obs.HeadSHA) {
		return ""
	}
	return " (the `" + squashSignoffCheck + "` check stays red)"
}

// SignoffAttributionDecisionMarkerPrefix opens the one stage/085 note per
// head for a hive-caused sign-off attribution the lane could not resolve
// with a merge commit by itself.
const SignoffAttributionDecisionMarkerPrefix = "<!-- hive:maintainer-signoff-attribution"

// SignoffAttributionDecisionComment is that note: addressed to maintainers,
// it says the hive's commits caused the block, that the contributor has
// nothing to fix, and what clears it (mergeCommitRefusalText).
func SignoffAttributionDecisionComment(obs stageObservation) string {
	n, total := max(obs.Detail.DCOSquashUnsafeHive, 1), obs.Detail.DCOTotal
	if total < n {
		total = n
	}
	author := nonEmpty(obs.Author, "the contributor")
	var b strings.Builder
	fmt.Fprintf(&b, "%s head=%s -->\n", SignoffAttributionDecisionMarkerPrefix, obs.HeadSHA)
	fmt.Fprintf(&b, "Held at `%s` for a maintainer. %d of %d commits on `%s` are the hive's own, signed off as the hive App, so a squash merge would land them as @%s with the App's sign-off, and the hive will not squash-merge this head%s. ",
		stagelabels.NeedsDecision.Label(), n, total, shortSHA(obs.HeadSHA), author, squashSignoffGateNote(obs))
	fmt.Fprintf(&b, "@%s did not make those commits and has nothing to fix here.\n\n", author)
	b.WriteString(mergeCommitRefusalText(obs))
	return b.String()
}

// noteSignoffAttributionDecision posts SignoffAttributionDecisionComment
// once per head.
func (c *Engine) noteSignoffAttributionDecision(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation) (int, error) {
	if obs.HeadSHA == "" {
		return 0, nil
	}
	if rec, ok := store.Get(key); ok && rec.SignoffAttributionNoted(obs.HeadSHA) {
		return 0, nil
	}
	obs = c.withMergeAskFacts(ctx, key, obs)
	body := SignoffAttributionDecisionComment(obs)
	if d := c.writebackAllowed(obs.Owner+"/"+obs.Repo, writebackItemForObservation(obs), writeback.KindNeedsDecisionComment, body); !d.Allowed {
		c.info("stage labels: sign-off attribution note withheld by write-back gate", "repo", obs.Display, "pr", obs.Number, "reason", d.Reason)
		return 0, nil
	}
	if _, _, err := c.gh.Issues.CreateComment(ctx, obs.Owner, obs.Repo, obs.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil { // writeback:gated
		c.warn("stage labels: sign-off attribution 085 note failed", "repo", obs.Display, "pr", obs.Number, "error", err)
		return 0, err
	}
	store.MarkSignoffAttributionNoted(key, obs.HeadSHA)
	c.info("stage labels: hive-caused sign-off attribution parked for a maintainer", "repo", obs.Display, "pr", obs.Number, "head", obs.HeadSHA, "author", obs.Author, "refused", obs.Detail.MergeCommitRefused)
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
