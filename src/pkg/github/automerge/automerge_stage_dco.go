package automerge

import (
	"context"
	"fmt"
	"regexp"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/authorkind"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Hive-owned DCO signal (hivecommons/hive#12542).
//
// Prow's dco plugin wrote `dco-signoff: yes|no`; the operator wants one green
// `dco` pill (red `no-dco` when a commit is unsigned) and wants a missing
// sign-off to send the PR straight back to its author — no waiting for CI or
// a reviewer verdict. So the stage pass reads the PR's commits itself, once
// per head (cached in the stage store), and:
//   - sets Desired.DCO so Plan writes exactly one of `dco` / `no-dco`
//     (Prow's `dco-signoff: *` pair is foreign and left alone, #13125);
//   - on DCONo overrides the lane's stage with the rework stage (072, or 070
//     when self-authored) under LaneReasonDCOMissing, so the 072 nudge fires
//     with the exact rebase command on the first evaluation of the head.

// stageDCOMaxPages bounds the commits read: GitHub caps the PR commits
// endpoint at 250 commits, three pages of 100 covers it.
const stageDCOMaxPages = 3

// signedOffByRE matches one Signed-off-by trailer line.
var signedOffByRE = regexp.MustCompile(`(?mi)^Signed-off-by: .+`)

type dcoStats struct {
	total            int
	unsigned         int
	unsignedHive     int
	unsignedOther    int
	squashUnsafeHive int
}

// dcoCommitStats counts the non-merge commits and those without a trailer.
// Merge-from-base commits are skipped, as the DCO App and Prow's plugin do.
func dcoCommitStats(commits []*gh.RepositoryCommit) (total, unsigned int) {
	stats := dcoCommitStatsDetailed(commits, "")
	return stats.total, stats.unsigned
}

func dcoCommitStatsDetailed(commits []*gh.RepositoryCommit, appLogin string) dcoStats {
	return dcoCommitStatsDetailedForAuthor(commits, appLogin, "")
}

func dcoCommitStatsDetailedForAuthor(commits []*gh.RepositoryCommit, appLogin, prAuthor string) dcoStats {
	return dcoCommitStatsForSquashAuthor(commits, appLogin, prAuthor, botSquashAuthor(prAuthor))
}

// dcoCommitStatsForSquashAuthor is dcoCommitStatsDetailedForAuthor with the
// squash-attribution verdict on the PR author made explicit: selfSquash means
// a squash under prAuthor attributes the hive's commits to the hive itself
// (a bot author, or one of this hive's own identities: project.ai_author,
// the App bot), so no signed hive commit is squash-unsafe. Unsigned commits
// are still counted and rejected.
func dcoCommitStatsForSquashAuthor(commits []*gh.RepositoryCommit, appLogin, prAuthor string, selfSquash bool) dcoStats {
	return dcoCommitStatsForHive(commits, appLogin, prAuthor, selfSquash, nil)
}

// dcoCommitStatsForHive is dcoCommitStatsForSquashAuthor with the hive's own
// identities (isHive, StageLabelPolicy.HiveAuthor) counted as the hive's, both
// when splitting unsigned commits and when counting squash-unsafe signed ones
// (hivecommons/hive#16703); a nil isHive counts only the App bot.
func dcoCommitStatsForHive(commits []*gh.RepositoryCommit, appLogin, prAuthor string, selfSquash bool, isHive func(login string) bool) dcoStats {
	var stats dcoStats
	for _, rc := range commits {
		if rc == nil || len(rc.Parents) > 1 {
			continue
		}
		stats.total++
		msg := rc.GetCommit().GetMessage()
		if !signedOffByRE.MatchString(msg) {
			stats.unsigned++
			if dcoCommitOwnedByHive(rc, appLogin, isHive) {
				stats.unsignedHive++
			} else {
				stats.unsignedOther++
			}
		} else if !selfSquash && dcoCommitOwnedByHive(rc, appLogin, isHive) && !messageHasPRAuthorSignoff(msg, prAuthor) {
			stats.squashUnsafeHive++
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

// dcoCommitOwnedByHive is dcoCommitByHive that also counts a commit GitHub
// links (by author login) to one of the hive's own identities (isHive:
// project.ai_author, the login the hive's agents push as). Only GitHub's
// linkage counts here too, never the git author name, so a collaborator's
// unsigned commit under their own email stays "other" and is never signed by
// the App (hivecommons/hive#16685).
//
// Trust assumption (hivecommons/hive#16702): GitHub derives the login from
// the git author email and the hive's agents push plain, unsigned git
// commits (app_signed_commits re-authors only the PR's opening head), so
// neither Commit.Verification nor the committer can tell an agent's commit
// from one a pusher authored with project.ai_author's email. Requiring
// either would stop the hive re-signing its own fix commits. The in-place
// path that acts on this split needs a base-repo head the App can push
// (adoptionDCORepairMode), so only collaborators with push access can
// present such a commit, and they are trusted not to. commitByApp has the
// same property for the App bot login.
func dcoCommitOwnedByHive(commit *gh.RepositoryCommit, appLogin string, isHive func(login string) bool) bool {
	if dcoCommitByHive(commit, appLogin) {
		return true
	}
	if commit == nil || isHive == nil {
		return false
	}
	login := strings.TrimSpace(commit.GetAuthor().GetLogin())
	return login != "" && isHive(login)
}

// stageDCO returns the commit check for the observation's head, reading the
// commits only when the store has no result for this head (the caller
// caches the result, including how many unsigned commits are the App's own,
// with Store.NoteDCO once the record exists). A read
// failure leaves the state unknown (labels untouched, stage unchanged) and
// is reported, never fatal.
func (c *Engine) stageDCO(ctx context.Context, store *stagelabels.Store, key string, obs stageObservation, policy StageLabelPolicy) (dcoStats, error) {
	if obs.HeadSHA == "" {
		return dcoStats{}, nil
	}
	appLogin := c.transport.AppBotLogin()
	selfSquash := botSquashAuthor(obs.Author) || hiveAuthoredPR(obs, policy, appLogin)
	// A split cached before hive identities counted (hivecommons/hive#16701) filed
	// project.ai_author commits as "other"; read the commits once more so
	// a hive-authored head is re-signed rather than left waiting.
	if rec, ok := store.Get(key); ok && (policy.HiveAuthor == nil || rec.DCOSplitHiveIdentity) {
		if t, u, cached := rec.DCOFor(obs.HeadSHA); cached {
			// Records cached before the ownership split existed carry
			// none; an in-place adoption decides repair-vs-rehome on it,
			// so read the commits once more for that case only.
			if (u > 0 || rec.DCOSquashUnsafeKnown) && (u == 0 || rec.DCOSplitKnown(obs.HeadSHA) || !rec.AdoptedInPlace()) {
				unsafe := rec.DCOSquashUnsafeHive
				if selfSquash {
					unsafe = 0
				}
				return dcoStats{total: t, unsigned: u, unsignedHive: rec.DCOUnsignedHive, unsignedOther: u - rec.DCOUnsignedHive, squashUnsafeHive: unsafe}, nil
			}
		}
	}
	// A list cut off at GitHub's cap is judged as read, as before.
	all, _, err := hgithub.ListPRRepositoryCommits(ctx, c.gh, obs.Owner, obs.Repo, obs.Number)
	if err != nil {
		return dcoStats{}, err
	}
	return dcoCommitStatsForHive(all, appLogin, obs.Author, selfSquash, policy.HiveAuthor), nil
}

// hiveAuthoredPR reports whether the PR is the hive's own: author/hive by
// kind, observed under the self-authored lane, authored by the App bot, or
// authored by one of the identities the policy names as this hive's
// (project.ai_author — the clanker login the hive's agents open PRs as).
// Such a PR is never re-homed for sign-off attribution: a squash under its
// author attributes the hive's commits to the hive (hivecommons/hive#16619).
func hiveAuthoredPR(obs stageObservation, policy StageLabelPolicy, appLogin string) bool {
	if obs.AuthorKind == authorkind.Hive || obs.Lane == stageLaneSelf {
		return true
	}
	author := strings.TrimSpace(obs.Author)
	if author == "" {
		return false
	}
	if appLogin != "" && strings.EqualFold(author, appLogin) {
		return true
	}
	return policy.HiveAuthor != nil && policy.HiveAuthor(author)
}

// Matches the bot-PR policy in check-squash-signoff-attribution.sh: a squash
// remains bot-authored. Unsigned commits are still counted and rejected.
func botSquashAuthor(login string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(login)), "[bot]")
}

func messageHasPRAuthorSignoff(message, prAuthor string) bool {
	login := strings.ToLower(strings.TrimSpace(prAuthor))
	if login == "" {
		return false
	}
	for _, line := range strings.Split(message, "\n") {
		if !strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "signed-off-by:") {
			continue
		}
		email := signedOffEmail(line)
		if githubNoreplyMatchesLogin(email, login) {
			return true
		}
	}
	return false
}

func signedOffEmail(line string) string {
	start := strings.LastIndex(line, "<")
	end := strings.LastIndex(line, ">")
	if start < 0 || end <= start {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(line[start+1 : end]))
}

func githubNoreplyMatchesLogin(email, login string) bool {
	email = strings.ToLower(strings.TrimSpace(email))
	login = strings.ToLower(strings.TrimSpace(login))
	const suffix = "@users.noreply.github.com"
	if email == "" || login == "" || !strings.HasSuffix(email, suffix) {
		return false
	}
	local := strings.TrimSuffix(email, suffix)
	if local == login {
		return true
	}
	parts := strings.SplitN(local, "+", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] != login {
		return false
	}
	for _, r := range parts[0] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
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

// signoffResetCommand is the exact fix handed to the author whose commits
// carry a Signed-off-by naming the App: re-author and re-sign them in place
// under the author's own git identity and force-push the same branch.
func signoffResetCommand(total int) string {
	if total <= 1 {
		return "git commit --amend --reset-author -s && git push --force-with-lease"
	}
	return fmt.Sprintf("git rebase --exec 'git commit --amend --no-edit --reset-author -s' HEAD~%d && git push --force-with-lease", total)
}

// signoffAttributionAsk is the review ask rendered for
// LaneReasonHiveSignoffAttribution: the commits are signed, but as the hive
// App, so a squash merge would misattribute them and the attribution gate
// stays red. Only the branch owner can rewrite them.
func signoffAttributionAsk(d stageDetail) string {
	n := d.DCOSquashUnsafeHive
	if n <= 0 {
		n = 1
	}
	plural, pronoun := "s are", "them"
	if n == 1 {
		plural, pronoun = " is", "it"
	}
	return fmt.Sprintf("%d commit%s signed off as the hive App instead of you, so a squash merge would misattribute %s — set `git config user.email` to your own address, then run `%s`",
		n, plural, pronoun, signoffResetCommand(max(d.DCOTotal, n)))
}
