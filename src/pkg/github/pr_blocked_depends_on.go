package github

// Depends-on holds for pull requests.
//
// A PR body's `Depends-on: #N` / `owner/repo#N` lines (ParseDependsOn) are
// the author's word that the PR must land after another one — a change
// whose tests need an upstream pull request in another repository, say.
// Before this sweep only the dashboard's critical path read them; nothing
// stopped a merge lane from landing the PR first, so a person had to hold
// it by hand and remember to release it.
//
// SweepBlockedPRDependencies puts the existing `blocked` signal on such a
// PR: the `blocked` label plus one `<!-- hive:blocked-on pr=owner/repo#N -->`
// marker comment per open pull request it depends on (postBlockedOnPRMarker,
// the same writer a relay's blocked verdict uses). Every merge lane refuses
// a `blocked` PR. The same sweep lifts the label, with the existing
// `<!-- hive:unblocked -->` note, once every marked target has merged or
// closed, or is no longer listed in the body, so a stale marker never holds
// the PR for ever.
//
// A target that closed WITHOUT merging may have been replaced: a maintainer
// re-did a contributor's pull request on another branch. Before releasing on
// such a target the sweep looks for the successor a trusted statement names
// (blocked_successor.go: who may declare one, and the hop limit). When that
// successor is an open pull request the PR stays `blocked`, and ONE comment
// carries the successor's blocked-on marker, a
// `<!-- hive:depends-on-successor pr=owner/repo#B replaces=owner/repo#A -->`
// marker and a short note. Later passes read that second marker with the
// PR's comments: a successor counts as listed while the target it replaces
// is listed and still closed unmerged, so it holds without the body being
// edited and stops holding with the target it stands in for. Only a
// successor already declared when the sweep sees the target closed is
// followed; with none, the hold is released as before.
//
// People win:
//
//   - a `blocked` whose newest label event is not the App's (a person put it
//     on) is never placed over and never lifted;
//   - a person who removes the hive's `blocked` releases the PR for the
//     dependencies already marked on it: the sweep does not put the label
//     back until the body names an open pull request it has not marked yet;
//   - drafts are left alone, and a target the App cannot read (another
//     organisation, a typo, a 404) places nothing and is logged once.
//
// A target that is an issue rather than a pull request stays display-only
// (the dashboard's critical path); this sweep places nothing for it.

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// blockedPRSweepMaxPages bounds the open-PR listing per repository.
const blockedPRSweepMaxPages = 5

// BlockedPRSweepResult summarises one SweepBlockedPRDependencies pass.
type BlockedPRSweepResult struct {
	// PRs counts open PRs wearing `blocked`, or naming an open pull request
	// in a Depends-on line.
	PRs int
	// Placed counts PRs that got the hive's `blocked` label this pass.
	Placed int
	// Marked counts blocked-on marker comments posted.
	Marked int
	// Released counts PRs left unblocked because a person removed the
	// hive's `blocked` for the same dependencies.
	Released int
	// Unblocked counts PRs whose hive `blocked` was lifted.
	Unblocked int
	// Suppressed counts writes the write-back gate held.
	Suppressed int
	Errors     int
}

// blockedPRTarget is one pull request a PR depends on.
type blockedPRTarget struct {
	repo   string
	number int
}

func (t blockedPRTarget) ref() string { return t.repo + "#" + strconv.Itoa(t.number) }

func (t blockedPRTarget) key() string { return strings.ToLower(t.ref()) }

// blockedPRTargetState is one read of a Depends-on target, cached per pass.
type blockedPRTargetState struct {
	err    error
	isPR   bool
	open   bool
	merged bool
	// body and author are the target's own text and login: the successor
	// lookup reads them when the target closed without merging.
	body   string
	author string
	// succDone: the declared-successor lookup ran this pass; succ, succOK
	// and succErr are its answer (blockedPRDeclaredSuccessor).
	succDone bool
	succ     int
	succOK   bool
	succErr  error
}

// blockedPRUnreadable remembers which PR+ref pairs were already logged as
// unreadable, so a target the App cannot see is logged once, not every pass.
var blockedPRUnreadable = struct {
	sync.Mutex
	m map[string]bool
}{m: map[string]bool{}}

// BlockedOnSeenReason is the "blocked on owner/repo#N" reason the blocked
// sweeps last read for repo#number from its blocked-on markers, or "" when
// they recorded none. The dashboard reads it for a `blocked` PR row, whose
// marker lives in a comment the enumeration never reads.
func BlockedOnSeenReason(repo string, number int) string {
	blockedOnSeen.Lock()
	defer blockedOnSeen.Unlock()
	if ref := blockedOnSeen.m[blockedOnSeenKey(repo, number)]; ref != "" {
		return blockedFilterReasonPrefix + " on " + ref
	}
	return ""
}

// NoteBlockedOnSeenForTest records what BlockedOnSeenReason answers for
// repo#number and returns the cleanup. Tests only.
func NoteBlockedOnSeenForTest(repo string, number int, prRepo string, pr int) func() {
	noteBlockedOnSeen(repo, number, prRepo, pr)
	return func() { forgetBlockedOnSeen(repo, number) }
}

// SweepBlockedPRDependencies places and lifts the hive's `blocked` on open
// PRs in opts.Repos (default: the configured repositories) from their
// `Depends-on:` lines. See the file comment. It needs the App bot login to
// tell its own label from a person's; without one it does nothing.
func (c *Client) SweepBlockedPRDependencies(ctx context.Context, opts BlockedSweepOptions) (BlockedPRSweepResult, error) {
	var res BlockedPRSweepResult
	if c == nil || c.client == nil {
		return res, ErrNoGitHubClient
	}
	if strings.TrimSpace(c.appBotLogin) == "" {
		return res, nil
	}
	repos := append([]string(nil), opts.Repos...)
	if len(repos) == 0 {
		repos = c.getRepos()
	}
	sort.Strings(repos)
	cache := map[string]blockedPRTargetState{}
	var firstErr error
	for _, repoRef := range repos {
		owner, name := c.splitRepo(repoRef)
		if validateRepoRef(owner, name) != nil {
			continue
		}
		prs, err := c.listOpenPRsForBlockedSweep(ctx, owner, name)
		if err != nil {
			res.Errors++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for _, pr := range prs {
			if ctx.Err() != nil {
				return res, firstErr
			}
			c.reconcileBlockedPR(ctx, owner+"/"+name, pr, cache, &res)
		}
	}
	return res, firstErr
}

func (c *Client) listOpenPRsForBlockedSweep(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
	var out []*gh.PullRequest
	for page := 0; page < blockedPRSweepMaxPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return out, fmt.Errorf("listing open pull requests for %s/%s: %w", owner, repo, err)
		}
		out = append(out, prs...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// reconcileBlockedPR places or lifts the hive's `blocked` on one open PR. A
// PR without the label costs nothing beyond its (per-pass cached) target
// reads unless one of them is an open pull request.
func (c *Client) reconcileBlockedPR(ctx context.Context, repo string, pr *gh.PullRequest, cache map[string]blockedPRTargetState, res *BlockedPRSweepResult) {
	if pr == nil || pr.GetNumber() <= 0 {
		return
	}
	number := pr.GetNumber()
	labels := extractPRLabels(pr.Labels)
	deps := blockedPRDependsOn(pr.GetBody(), repo, number)
	blocked := hasBlockedLabel(labels)
	var open []blockedPRTarget
	if !blocked {
		if len(deps) == 0 || pr.GetDraft() {
			return
		}
		if open = c.openBlockedPRTargets(ctx, repo, number, deps, cache); len(open) == 0 {
			return
		}
	}
	res.PRs++
	owner, name := c.splitRepo(repo)
	history, err := c.labelEventHistory(ctx, owner, name, number, BlockedLabel)
	if err != nil {
		res.Errors++
		c.logger.Warn("blocked PR sweep: label events unreadable; left alone", "repo", repo, "pr", number, "error", err)
		return
	}
	appIsLatest := strings.EqualFold(history.latestActor, c.appBotLogin)
	if blocked && !(history.latestKind == "labeled" && appIsLatest) {
		// A person's `blocked` (or one no labeled event explains) is theirs.
		return
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		res.Errors++
		c.logger.Warn("blocked PR sweep: comments unreadable; left alone", "repo", repo, "pr", number, "error", err)
		return
	}
	marked := blockedPRMarkedTargets(comments)
	markedSet := make(map[string]bool, len(marked))
	for _, t := range marked {
		markedSet[t.key()] = true
	}
	if blocked {
		var unmarked []blockedPRTarget
		for _, d := range deps {
			if !markedSet[d.key()] {
				unmarked = append(unmarked, d)
			}
		}
		fresh := c.openBlockedPRTargets(ctx, repo, number, unmarked, cache)
		c.liftOrKeepBlockedPR(ctx, repo, pr, labels, deps, marked, fresh, comments, cache, res)
		return
	}
	// Placement. An open target already marked counts too: the hive lifted
	// its own label once (the target was dropped from the body) and the
	// body lists it again.
	var fresh []blockedPRTarget
	for _, t := range open {
		if !markedSet[t.key()] {
			fresh = append(fresh, t)
		}
	}
	released := history.appLabeled && history.latestKind == "unlabeled" && !appIsLatest
	if released && len(fresh) == 0 {
		// A person removed the hive's `blocked` and the body names no open
		// pull request the hive has not already held this PR for.
		res.Released++
		return
	}
	if !c.blockedPRWriteAllowed(repo, pr, labels, "blocked on "+blockedPRRefs(open), res) {
		return
	}
	if err := c.AddLabels(ctx, repo, number, []string{BlockedLabel}); err != nil { // writeback:gated
		res.Errors++
		c.logger.Warn("blocked PR sweep: label not added", "repo", repo, "pr", number, "error", err)
		return
	}
	res.Placed++
	c.markBlockedPRTargets(ctx, repo, number, fresh, res)
	noteBlockedOnSeen(repo, number, open[0].repo, open[0].number)
	notifyItemRefresh(repo, number, true)
	c.logger.Info("blocked PR sweep: PR held on its Depends-on targets", "repo", repo, "pr", number, "targets", blockedPRRefs(open))
}

// liftOrKeepBlockedPR settles a PR wearing the hive's own `blocked`: it
// keeps the label while any marked target is open and still listed (or a
// newly listed target is open, which it marks), and otherwise lifts it with
// one unblocked note. A PR the hive blocked without any marker (not this
// sweep's placement) is left alone. A target read error keeps the block.
//
// A listed target that closed without merging is followed to its declared
// successor (see the file comment): one the hive already marked holds like
// any listed target, and one found on this pass is marked and holds from
// this pass on. A successor lookup GitHub definitely refuses releases at
// once, as before; one that fails for a reason that may pass keeps the block
// for at most blockedSuccessorLookupGrace, then releases as before.
func (c *Client) liftOrKeepBlockedPR(ctx context.Context, repo string, pr *gh.PullRequest, labels []string, deps, marked, fresh []blockedPRTarget, comments []*gh.IssueComment, cache map[string]blockedPRTargetState, res *BlockedPRSweepResult) {
	number := pr.GetNumber()
	if len(marked) == 0 {
		return
	}
	listed := make(map[string]bool, len(deps))
	for _, d := range deps {
		listed[d.key()] = true
	}
	links := c.blockedSuccessorLinks(comments)
	marked = blockedSuccessorMarked(marked, links)
	self := blockedPRTarget{repo: repo, number: number}
	var open []blockedPRTarget
	var found []blockedSuccessorLink
	var clauses []string
	for _, t := range marked {
		if !c.blockedSuccessorLive(ctx, t, listed, links, cache, map[string]bool{}) {
			if blockedSuccessorIsSuccessor(t, links) {
				clauses = append(clauses, t.ref()+" no longer stands in for a listed dependency")
			} else {
				clauses = append(clauses, t.ref()+" is no longer listed under `Depends-on:`")
			}
			continue
		}
		st := c.blockedPRTargetRead(ctx, t, cache)
		switch {
		case st.err != nil:
			res.Errors++
			c.logger.Warn("blocked PR sweep: tracked PR lookup failed; block kept", "repo", repo, "pr", number, "target", t.ref(), "error", st.err)
			open = append(open, t)
		case st.open:
			open = append(open, t)
		case st.merged:
			clauses = append(clauses, t.ref()+" merged")
		default:
			if succ, ok := blockedSuccessorReplacedBy(t, links); ok {
				// Already followed on an earlier pass: the successor is
				// marked and settles on its own turn in this loop.
				clauses = append(clauses, t.ref()+" was not merged and was replaced by "+succ.ref())
				continue
			}
			succ, ok, err := c.blockedPRSuccessorOf(ctx, self, t, cache)
			lookup := blockedSuccessorLookupKey(repo, number, t.ref())
			if err == nil {
				blockedSuccessorLookupSettled(lookup)
			} else if giveUp, news := blockedSuccessorLookupFailed(lookup); giveUp {
				// The lookup kept failing past the grace: release as if
				// nothing named a successor, which is what happened before.
				if news {
					c.logger.Warn("blocked PR sweep: successor lookup for a closed blocker kept failing for "+blockedSuccessorLookupGrace.String()+"; releasing as before", "repo", repo, "pr", number, "blocker", t.ref(), "error", err)
				}
				ok, err = false, nil
			}
			switch {
			case err != nil:
				res.Errors++
				c.logger.Warn("blocked PR sweep: successor lookup failed; block kept", "repo", repo, "pr", number, "target", t.ref(), "error", err)
				open = append(open, t)
			case ok:
				found = append(found, blockedSuccessorLink{successor: succ, replaces: t})
				open = append(open, succ)
			default:
				clauses = append(clauses, t.ref()+" closed without merging, so check that this PR still makes sense without it")
			}
		}
	}
	if len(open)+len(fresh) > 0 {
		if len(fresh) > 0 && c.blockedPRWriteAllowed(repo, pr, labels, "blocked on "+blockedPRRefs(fresh), res) {
			c.markBlockedPRTargets(ctx, repo, number, fresh, res)
		}
		for _, l := range found {
			c.markBlockedPRSuccessor(ctx, repo, pr, labels, l, res)
		}
		first := append(append([]blockedPRTarget(nil), open...), fresh...)[0]
		noteBlockedOnSeen(repo, number, first.repo, first.number)
		return
	}
	note := blockedPRDependsOnLiftNote(clauses)
	if !c.blockedPRWriteAllowed(repo, pr, labels, note, res) {
		return
	}
	if err := c.unblockItem(ctx, repo, number, true); err != nil {
		res.Errors++
		c.logger.Warn("blocked PR sweep: unblock failed", "repo", repo, "pr", number, "error", err)
		return
	}
	for _, t := range marked {
		blockedSuccessorLookupSettled(blockedSuccessorLookupKey(repo, number, t.ref()))
	}
	c.postUnblockedNote(ctx, repo, number, note, c.logger)
	res.Unblocked++
	c.logger.Info("blocked PR sweep: PR unblocked", "repo", repo, "pr", number, "targets", blockedPRRefs(marked))
}

// blockedPRWriteAllowed asks the write-back gate for one sweep write on pr
// (the label and its marker comments, or the lift and its note). The hold
// is the same kind a relay's blocked verdict and the marker-less blocked ask
// use, KindGateComment; a refusal is counted and logged.
func (c *Client) blockedPRWriteAllowed(repo string, pr *gh.PullRequest, labels []string, preview string, res *BlockedPRSweepResult) bool {
	_, attributed := ParseAttributionTrailer(pr.GetBody())
	item := c.WritebackItemForPR(PullRequest{
		Number: pr.GetNumber(), Author: safeGetLogin(pr.GetUser()), HiveAttributed: attributed,
		Labels: labels, CreatedAt: pr.GetCreatedAt().Time,
	})
	d := c.WritebackAllowed(repo, item, writeback.KindGateComment, preview)
	if !d.Allowed {
		res.Suppressed++
		c.logger.Info("blocked PR sweep: write held by writeback gate", "repo", repo, "pr", pr.GetNumber(), "reason", d.Reason, "detail", d.Detail)
	}
	return d.Allowed
}

// markBlockedPRTargets posts one blocked-on marker comment per target.
func (c *Client) markBlockedPRTargets(ctx context.Context, repo string, number int, targets []blockedPRTarget, res *BlockedPRSweepResult) {
	for _, t := range targets {
		posted, err := c.postBlockedOnPRMarker(ctx, repo, number, t.repo, t.number, blockedPRDependsOnMarkerComment(t.repo, t.number))
		if err != nil {
			res.Errors++
			c.logger.Warn("blocked PR sweep: blocked-on marker not posted", "repo", repo, "pr", number, "target", t.ref(), "error", err)
			continue
		}
		if posted {
			res.Marked++
		}
	}
}

// openBlockedPRTargets returns the targets that are open pull requests.
// An unreadable target places nothing and is logged once per PR and ref; an
// issue or a closed PR is skipped.
func (c *Client) openBlockedPRTargets(ctx context.Context, repo string, number int, targets []blockedPRTarget, cache map[string]blockedPRTargetState) []blockedPRTarget {
	var out []blockedPRTarget
	for _, t := range targets {
		st := c.blockedPRTargetRead(ctx, t, cache)
		if st.err != nil {
			key := strings.ToLower(repo) + "#" + strconv.Itoa(number) + "|" + t.key()
			blockedPRUnreadable.Lock()
			seen := blockedPRUnreadable.m[key]
			blockedPRUnreadable.m[key] = true
			blockedPRUnreadable.Unlock()
			if !seen {
				c.logger.Warn("blocked PR sweep: Depends-on target unreadable; not held on it", "repo", repo, "pr", number, "target", t.ref(), "error", st.err)
			}
			continue
		}
		if st.isPR && st.open {
			out = append(out, t)
		}
	}
	return out
}

// blockedPRTargetRead reads a target once per pass. The issue payload says
// whether it is a pull request, open, and (pull_request.merged_at) merged.
func (c *Client) blockedPRTargetRead(ctx context.Context, t blockedPRTarget, cache map[string]blockedPRTargetState) blockedPRTargetState {
	if st, ok := cache[t.key()]; ok {
		return st
	}
	var st blockedPRTargetState
	owner, name := c.splitRepo(t.repo)
	if err := validateRepoRef(owner, name); err != nil {
		st.err = err
	} else if issue, _, err := c.client.Issues.Get(ctx, owner, name, t.number); err != nil {
		st.err = err
	} else {
		st.isPR = issue.IsPullRequest()
		st.open = strings.EqualFold(issue.GetState(), "open")
		st.merged = !issue.GetPullRequestLinks().GetMergedAt().IsZero()
		st.body = issue.GetBody()
		st.author = safeGetLogin(issue.GetUser())
	}
	cache[t.key()] = st
	return st
}

// blockedPRDependsOn is ParseDependsOn as targets, without a self-reference.
func blockedPRDependsOn(body, repo string, number int) []blockedPRTarget {
	var out []blockedPRTarget
	for _, d := range ParseDependsOn(body, repo) {
		if d.Number == number && strings.EqualFold(d.Repo, repo) {
			continue
		}
		out = append(out, blockedPRTarget{repo: d.Repo, number: d.Number})
	}
	return out
}

// blockedPRMarkedTargets returns every distinct PR named by a blocked-on
// marker in comments, oldest first.
func blockedPRMarkedTargets(comments []*gh.IssueComment) []blockedPRTarget {
	seen := map[string]bool{}
	var out []blockedPRTarget
	for _, cm := range comments {
		for _, m := range blockedOnMarkerRE.FindAllStringSubmatch(cm.GetBody(), -1) {
			n, err := strconv.Atoi(m[2])
			if err != nil || n <= 0 {
				continue
			}
			t := blockedPRTarget{repo: m[1], number: n}
			if !seen[t.key()] {
				seen[t.key()] = true
				out = append(out, t)
			}
		}
	}
	return out
}

func blockedPRRefs(targets []blockedPRTarget) string {
	refs := make([]string, 0, len(targets))
	for _, t := range targets {
		refs = append(refs, t.ref())
	}
	return strings.Join(refs, ", ")
}

// blockedPRDependsOnMarkerComment is the marker comment for one open pull
// request a PR's Depends-on line names.
func blockedPRDependsOnMarkerComment(prRepo string, pr int) string {
	return BlockedOnMarker(prRepo, pr) + "\n" +
		fmt.Sprintf("Hive: this pull request says `Depends-on: %s#%d`, which is still open, so it carries the `%s` label and no Hive merge lane will merge it. Hive lifts the label once every pull request it depends on has merged or closed, or is dropped from the `Depends-on:` lines. Removing the label releases it; Hive puts it back only for a newly listed dependency.", prRepo, pr, BlockedLabel)
}

// blockedPRDependsOnLiftNote is the unblocked note for a PR whose every
// marked dependency settled. It names the first one right after
// "Unblocked: " so unblockAlreadyNoted recognises a repeat.
func blockedPRDependsOnLiftNote(clauses []string) string {
	return blockedUnblockedMarker + "\nUnblocked: " + strings.Join(clauses, "; ") + ". Nothing this pull request depends on is still open, so the merge lanes take it again."
}
