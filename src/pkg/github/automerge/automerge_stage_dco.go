package automerge

import (
	"context"
	"fmt"
	"regexp"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Hive-owned DCO signal (hivecommons/hive#12542).
//
// Prow's dco plugin wrote `dco-signoff: yes|no`; the operator wants one green
// `dco` pill (red `no-dco` when a commit is unsigned) and wants a missing
// sign-off to send the PR straight back to its author — no waiting for CI or
// a reviewer verdict. So the stage pass reads the PR's commits itself, once
// per head (cached in the stage store), and:
//   - sets Desired.DCO so Plan writes exactly one of `dco` / `no-dco` and
//     strips the legacy Prow pair;
//   - on DCONo overrides the lane's stage with the rework stage (072, or 070
//     when self-authored) under LaneReasonDCOMissing, so the 072 nudge fires
//     with the exact rebase command on the first evaluation of the head.

// stageDCOMaxPages bounds the commits read: GitHub caps the PR commits
// endpoint at 250 commits, three pages of 100 covers it.
const stageDCOMaxPages = 3

// signedOffByRE matches one Signed-off-by trailer line.
var signedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by: .+`)

type dcoStats struct {
	total         int
	unsigned      int
	unsignedHive  int
	unsignedOther int
}

// dcoCommitStats counts the non-merge commits and those without a trailer.
// Merge-from-base commits are skipped, as the DCO App and Prow's plugin do.
func dcoCommitStats(commits []*gh.RepositoryCommit) (total, unsigned int) {
	stats := dcoCommitStatsDetailed(commits, "")
	return stats.total, stats.unsigned
}

func dcoCommitStatsDetailed(commits []*gh.RepositoryCommit, appLogin string) dcoStats {
	var stats dcoStats
	for _, rc := range commits {
		if rc == nil || len(rc.Parents) > 1 {
			continue
		}
		stats.total++
		if !signedOffByRE.MatchString(rc.GetCommit().GetMessage()) {
			stats.unsigned++
			if dcoCommitByHive(rc, appLogin) {
				stats.unsignedHive++
			} else {
				stats.unsignedOther++
			}
		}
	}
	return stats
}

// dcoCommitByHive reports whether an unsigned commit is the hive's own to
// rewrite with sign-off. Only GitHub's linkage of the commit to the App bot
// counts (commitByApp): git author/committer names and emails are
// contributor-controlled, so a lane-looking address never makes a commit
// hive-owned. Anything else is "other" and re-homes.
func dcoCommitByHive(commit *gh.RepositoryCommit, appLogin string) bool {
	return commitByApp(commit, appLogin)
}

// stageDCO returns the commit check for the observation's head, reading the
// commits only when the store has no result for this head (the caller
// caches the result, including how many unsigned commits are the App's own,
// with Store.NoteDCO once the record exists). A read
// failure leaves the state unknown (labels untouched, stage unchanged) and
// is reported, never fatal.
func (c *Engine) stageDCO(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation) (dcoStats, error) {
	if obs.HeadSHA == "" {
		return dcoStats{}, nil
	}
	if rec, ok := store.Get(key); ok {
		if t, u, cached := rec.DCOFor(obs.HeadSHA); cached {
			// Records cached before the ownership split existed carry
			// none; an in-place adoption decides repair-vs-rehome on it,
			// so read the commits once more for that case only.
			if u == 0 || rec.DCOSplitKnown(obs.HeadSHA) || !rec.AdoptedInPlace() {
				return dcoStats{total: t, unsigned: u, unsignedHive: rec.DCOUnsignedHive, unsignedOther: u - rec.DCOUnsignedHive}, nil
			}
		}
	}
	opts := &gh.ListOptions{PerPage: 100}
	var all []*gh.RepositoryCommit
	for page := 0; page < stageDCOMaxPages; page++ {
		commits, resp, err := c.gh.PullRequests.ListCommits(ctx, obs.Owner, obs.Repo, obs.Number, opts)
		if err != nil {
			return dcoStats{}, fmt.Errorf("listing commits for %s#%d: %w", obs.Display, obs.Number, err)
		}
		all = append(all, commits...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return dcoCommitStatsDetailed(all, c.transport.AppBotLogin()), nil
}

// dcoState folds the commit counts into the label state. A PR with no
// non-merge commits (nothing to sign) is unknown, not green.
func dcoState(total, unsigned int) stagelabels.DCOState {
	switch {
	case total == 0:
		return stagelabels.DCOUnknown
	case unsigned > 0:
		return stagelabels.DCONo
	}
	return stagelabels.DCOYes
}

// dcoRebaseCommand is the exact fix handed to the author: sign every commit
// on the branch in place and force-push the same branch.
func dcoRebaseCommand(total int) string {
	if total <= 0 {
		total = 1
	}
	return fmt.Sprintf("git rebase --signoff HEAD~%d && git push --force-with-lease", total)
}

// dcoAsk is the review ask rendered in the 072 nudge for a missing sign-off.
func dcoAsk(d stageDetail) string {
	n := d.DCOUnsigned
	plural := "s are"
	if n == 1 {
		plural = " is"
	}
	if n <= 0 {
		return "a commit is missing its `Signed-off-by` trailer (DCO) — run `" + dcoRebaseCommand(d.DCOTotal) + "`"
	}
	return fmt.Sprintf("%d commit%s missing the `Signed-off-by` trailer (DCO) — run `%s`", n, plural, dcoRebaseCommand(d.DCOTotal))
}
