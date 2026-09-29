package github

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// Follow-up signing (#9364).
//
// reauthorBranchSigned signs a branch once, when hive-open-pr opens its PR.
// Everything an agent pushes to that branch afterwards (a CI fix, review
// follow-ups, a rebase) is plain git again, and so is the branch of every
// open-time signed_skipped fallback. Under a required_signatures ruleset such
// a PR can never merge.
//
// This pass reconciles instead of hooking pushes. It runs from the PR-request
// watcher loop, and when github.app_signed_commits is on it walks every open
// PR the App bot authored. A PR needs signing when any of its commits is
// unverified, not only when the head is: a person can sign their own commit
// on top of an agent's unsigned one, which leaves the head Verified while an
// unsigned commit still sits underneath it. So the pass finds the first
// unverified commit (oldest to newest) and signs from there through the head,
// via the same signBranch the open path uses. The base is the commit right
// before it, if any: compare takes a sha, and the merge base of sha...head is
// the sha itself. So the result is one GitHub-signed commit on top of the
// last verified one, with the same tree and the range's messages and DCO
// trailers. The open path's guards all still apply: the moved-head check,
// the mode/size refusals, and the scratch ref.
//
// It never re-authors a person's work. Before signing, every commit in the
// tail must be the hive's own: author and committer both either the App bot
// or an address in the pane identity domain (HIVE_GIT_BOT_EMAIL_DOMAIN). A
// tail with anyone else's commit is skipped, and the PR gets one comment
// saying why.

// prSignedReconcileInterval is the minimum gap between two reconcile passes.
// The watcher ticks every few seconds, while a follow-up push only needs
// signing before a human gets to merging. A minute keeps the steady-state cost
// at one open-PR list per repo per minute, and those GETs are usually free
// 304s through the ETag transport.
var prSignedReconcileInterval = time.Minute

// signedReconcileMaxPRPages bounds the open-PR listing per repo (100 per page).
const signedReconcileMaxPRPages = 10

// signedReconcileMarker identifies the App's "can't sign this" comment so it
// is posted once per PR, including across hive restarts.
const signedReconcileMarker = "<!-- hive:signed-commits-blocked -->"

// signedReconcileState is the reconciler's memory. It is in memory only.
// After a restart each hive PR's commits are listed once more, and the
// marker comment keeps the note from repeating.
type signedReconcileState struct {
	mu sync.Mutex
	// lastRun is when the pass last started (throttle).
	lastRun time.Time
	// settled maps "owner/repo#N" to the head sha the pass has already
	// decided on (verified, signed, or skipped for good). A PR whose listed
	// head still matches it costs no further calls.
	settled map[string]string
	// noted holds the PRs that already carry the skip comment.
	noted map[string]bool
}

func (s *signedReconcileState) settledHead(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.settled[key]
}

func (s *signedReconcileState) settle(key, sha string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.settled == nil {
		s.settled = map[string]string{}
	}
	s.settled[key] = sha
}

func (s *signedReconcileState) isNoted(key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.noted[key]
}

func (s *signedReconcileState) markNoted(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.noted == nil {
		s.noted = map[string]bool{}
	}
	s.noted[key] = true
}

// prune forgets every PR of one repo that is no longer open.
func (s *signedReconcileState) prune(repoPrefix string, open map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.settled {
		if strings.HasPrefix(key, repoPrefix) && !open[key] {
			delete(s.settled, key)
		}
	}
	for key := range s.noted {
		if strings.HasPrefix(key, repoPrefix) && !open[key] {
			delete(s.noted, key)
		}
	}
}

// signedCommitsEnabled reports the live github.app_signed_commits toggle.
func (c *Client) signedCommitsEnabled() bool {
	return c != nil && c.prSignedCommits != nil && c.prSignedCommits()
}

// maybeReconcileSignedCommits runs reconcileSignedCommits when signing is on
// and prSignedReconcileInterval has passed since the last run. The PR-request
// watcher calls it on every tick.
func (c *Client) maybeReconcileSignedCommits(ctx context.Context, now time.Time) {
	if !c.signedCommitsEnabled() {
		return
	}
	s := &c.signedReconcile
	s.mu.Lock()
	due := s.lastRun.IsZero() || now.Sub(s.lastRun) >= prSignedReconcileInterval
	if due {
		s.lastRun = now
	}
	s.mu.Unlock()
	if due {
		c.reconcileSignedCommits(ctx)
	}
}

// reconcileSignedCommits is one pass over every active repo's open PRs.
func (c *Client) reconcileSignedCommits(ctx context.Context) {
	if !c.signedCommitsEnabled() || c.client == nil {
		return
	}
	bot := strings.TrimSpace(c.appBotLogin)
	if bot == "" {
		// Without the App's login there is no telling a hive PR from a
		// person's; fail closed.
		return
	}
	for _, repo := range c.activeRepos() {
		if ctx.Err() != nil {
			return
		}
		owner, name := c.splitRepo(repo)
		prs, err := c.listOpenPRsForSigning(ctx, owner, name)
		if err != nil {
			c.warn("signed-commit reconciler: listing open PRs failed", "repo", owner+"/"+name, "err", err)
			continue
		}
		prefix := owner + "/" + name + "#"
		open := map[string]bool{}
		for _, pr := range prs {
			if ctx.Err() != nil {
				return
			}
			if pr == nil || !strings.EqualFold(safeGetLogin(pr.GetUser()), bot) {
				continue
			}
			// Only a branch in the PR's own repository is the hive's to
			// rewrite (and the App's token can only write there).
			if !strings.EqualFold(pr.GetHead().GetRepo().GetFullName(), owner+"/"+name) {
				continue
			}
			key := fmt.Sprintf("%s%d", prefix, pr.GetNumber())
			open[key] = true
			headSHA := pr.GetHead().GetSHA()
			if headSHA == "" || c.signedReconcile.settledHead(key) == headSHA {
				continue
			}
			if settled := c.reconcileSignedPR(ctx, owner, name, pr); settled != "" {
				c.signedReconcile.settle(key, settled)
			}
		}
		c.signedReconcile.prune(prefix, open)
	}
}

func (c *Client) listOpenPRsForSigning(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	var all []*gh.PullRequest
	for page := 0; page < signedReconcileMaxPRPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		all = append(all, prs...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

// reconcileSignedPR brings one PR's head to a Verified commit if it can. It
// returns the head sha to remember as settled, or "" to look again on the
// next pass (a transient failure, or a head that moved under us).
func (c *Client) reconcileSignedPR(ctx context.Context, owner, repo string, pr *gh.PullRequest) string {
	number := pr.GetNumber()
	head := pr.GetHead().GetRef()
	headSHA := pr.GetHead().GetSHA()
	logArgs := []any{"repo", owner + "/" + repo, "pr", number, "head", head}

	commits, err := c.listPRRepositoryCommits(ctx, owner, repo, number)
	if err != nil {
		c.warn("signed-commit reconciler: listing PR commits failed", append(logArgs, "err", err)...)
		return ""
	}
	if len(commits) == 0 {
		return headSHA
	}
	if len(commits) >= maxPRCommitPages*100 {
		// GitHub stops listing PR commits at 250, so the tail cannot be seen.
		reason := fmt.Sprintf("the PR has %d or more commits, past what GitHub lists, so its unsigned tail cannot be checked", len(commits))
		c.warn("signed-commit reconciler: skipped", append(logArgs, "reason", reason)...)
		c.noteSignedSkip(ctx, owner, repo, number, reason)
		return headSHA
	}
	if commits[len(commits)-1].GetSHA() != headSHA {
		// Pushed between the PR list and the commit list; next pass.
		return ""
	}

	// A verified head is not enough: a person can sign their own commit on
	// top of an agent's unsigned one, leaving the head Verified while an
	// unsigned commit still sits underneath it. Find the first unverified
	// commit, oldest to newest; everything from there through the head is
	// the range that must be re-authored, or refused as a whole.
	firstUnverified := -1
	for i, rc := range commits {
		if !commitVerified(rc) {
			firstUnverified = i
			break
		}
	}
	if firstUnverified < 0 {
		// Every commit, including the head, is Verified.
		return headSHA
	}

	base := pr.GetBase().GetRef()
	if firstUnverified > 0 {
		base = commits[firstUnverified-1].GetSHA()
	}
	tail := commits[firstUnverified:]

	if reason := c.signedTailBlocker(tail); reason != "" {
		reason = fmt.Sprintf("commit %s is unverified; %s", shortSHA(commits[firstUnverified].GetSHA()), reason)
		c.warn("signed-commit reconciler: skipped; the PR head stays unsigned", append(logArgs, "reason", reason)...)
		c.noteSignedSkip(ctx, owner, repo, number, reason)
		return headSHA
	}

	oid, replaced, err := c.signBranch(ctx, owner, repo, base, head)
	if err != nil {
		var moved signedHeadMovedError
		c.warn("signed-commit reconciler: could not re-author the unsigned tail as a GitHub-signed commit", append(logArgs, "base", base, "reason", err.Error())...)
		if errors.As(err, &moved) || !errors.Is(err, errSignedCommitSkip) {
			return "" // transient: retry next pass
		}
		c.noteSignedSkip(ctx, owner, repo, number, err.Error())
		return headSHA
	}
	c.info("signed-commit reconciler: unsigned tail re-authored as one GitHub-signed commit by the App bot",
		append(logArgs, "base", base, "commit", oid, "replaced_commits", replaced)...)
	return oid
}

func commitVerified(rc *gh.RepositoryCommit) bool {
	return rc.GetCommit().GetVerification().GetVerified()
}

// signedTailBlocker returns why the tail must not be re-authored, or "" when
// every commit in it is the hive's own linear work.
func (c *Client) signedTailBlocker(tail []*gh.RepositoryCommit) string {
	for _, rc := range tail {
		sha := shortSHA(rc.GetSHA())
		if len(rc.Parents) > 1 {
			// createCommitOnBranch makes single-parent commits. Flattening a
			// merge would pull the merged-in changes into the PR's diff.
			return fmt.Sprintf("commit %s is a merge commit; the signed rewrite only handles a linear history (rebase instead of merging)", sha)
		}
		author := rc.GetCommit().GetAuthor()
		if !c.hiveCommitIdentity(safeGetLogin(rc.GetAuthor()), author.GetEmail()) {
			return fmt.Sprintf("commit %s is authored by %s <%s>, not a hive agent; the hive never re-authors a person's work", sha, author.GetName(), author.GetEmail())
		}
		committer := rc.GetCommit().GetCommitter()
		if !c.hiveCommitIdentity(safeGetLogin(rc.GetCommitter()), committer.GetEmail()) {
			return fmt.Sprintf("commit %s is committed by %s <%s>, not a hive agent; the hive never re-authors a person's work", sha, committer.GetName(), committer.GetEmail())
		}
	}
	return ""
}

// hiveCommitIdentity reports whether a commit identity is the hive's own: the
// App bot (by GitHub login or its noreply address) or an address in the pane
// identity domain. A login GitHub resolved to any other account belongs to a
// person, whatever email the commit carries.
func (c *Client) hiveCommitIdentity(login, email string) bool {
	bot := strings.TrimSpace(c.appBotLogin)
	if login != "" {
		return bot != "" && strings.EqualFold(login, bot)
	}
	local, domain, ok := strings.Cut(strings.TrimSpace(email), "@")
	if !ok || local == "" {
		return false
	}
	if strings.EqualFold(domain, signedTrailerDomain()) {
		return true
	}
	if strings.EqualFold(domain, signedTrailerNoreplyDomain) && bot != "" {
		if _, rest, hasID := strings.Cut(local, "+"); hasID {
			local = rest
		}
		return strings.EqualFold(local, bot)
	}
	return false
}

// noteSignedSkip leaves one comment on the PR explaining why its head can't be
// signed. The in-memory set spares the comment listing on later heads; the
// marker covers restarts.
func (c *Client) noteSignedSkip(ctx context.Context, owner, repo string, number int, reason string) {
	key := fmt.Sprintf("%s/%s#%d", owner, repo, number)
	if c.signedReconcile.isNoted(key) {
		return
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		c.warn("signed-commit reconciler: listing PR comments failed", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	for _, cm := range comments {
		if c.isTrustedAppBotCommentAuthor(cm) && strings.Contains(cm.GetBody(), signedReconcileMarker) {
			c.signedReconcile.markNoted(key)
			return
		}
	}
	body := signedReconcileMarker + "\n" +
		"**Signed commits:** the hive could not re-sign the newest commits on this branch, so the PR head is not Verified. " +
		"If the base branch requires signed commits, this PR cannot merge until that is resolved.\n\n" +
		"Reason: " + reason + "\n\n" +
		"The hive re-signs only its own agents' commits (`github.app_signed_commits`). To unblock it, sign the commits yourself, " +
		"or drop them from the branch so the hive can re-sign the agent's work on a later pass. This note is posted once per PR."
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
		c.warn("signed-commit reconciler: posting the skip note failed", "repo", owner+"/"+repo, "pr", number, "err", err)
		return
	}
	c.signedReconcile.markNoted(key)
	c.info("signed-commit reconciler: noted on the PR why it can't be signed", "repo", owner+"/"+repo, "pr", number)
}
