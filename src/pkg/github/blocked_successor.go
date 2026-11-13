package github

// Following a closed blocker to its successor.
//
// Two blocked sweeps used to release a hold the moment the thing it waited
// on closed, even when that thing had only been replaced:
//
//   - a PR held on `Depends-on: #A` (pr_blocked_depends_on.go) was released
//     when #A closed unmerged, although a maintainer had re-done #A as #B, so
//     the PR could merge before its real prerequisite;
//   - an issue blocked on another issue (`<!-- hive:blocked-on issue=… -->`,
//     reconcileBlockedOnIssue) was unblocked when that issue was closed as a
//     duplicate of, or replaced by, a new one.
//
// Both now look for a successor first and, when it is still open, move the
// hold to it. An issue blocked on a PR already did this
// (pullRequestSupersededBy in issue_blocked.go); that path is untouched and
// shares only ParseSupersededBy with this file.
//
// Who may declare a successor. The older path accepts a "Superseded by #N"
// line from any commenter. Holding a pull request's merge, or an issue's
// place in the queue, on such a line is a power a stranger must not have, so
// the lookups here read it only from:
//
//   - the closed item's own body;
//   - a comment by one of the hive's own identities (which is also where an
//     adoption marker, `hive:adopted … mode=supersede superseded_by=N`, comes
//     from);
//   - a comment by the closed item's author;
//   - a comment whose author_association is OWNER, MEMBER or COLLABORATOR.
//
// Anything else is ignored, which means the hold is released as before.
// GitHub's own duplicate closure (state_reason "duplicate") needs triage
// rights and is trusted as it is; the canonical issue is read from GraphQL
// `Issue.duplicateOf`, the one place GitHub exposes it (the REST issue and
// REST timeline carry no canonical issue).
//
// A "Superseded by #N" number is in the closed item's own repository. A
// chain (A replaced by B, B replaced by C) is followed for at most
// blockedSuccessorMaxHops hops per lookup. Only a successor that is already
// declared when the sweep sees the blocker closed is followed: a line
// written after the sweep released the hold changes nothing.
//
// A lookup must never turn "released at once" into "never released". A read
// GitHub definitely refuses (not found, no access, a GraphQL errors array
// that is not a rate limit) means "no successor", and the hold is released
// at once as before. A read that fails for a reason that may pass (a rate
// limit, a 5xx, the network) keeps the hold, but for at most
// blockedSuccessorLookupGrace from the first failure; after that the sweep
// gives up, says so once, and releases as before.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/writeback"
)

// blockedSuccessorMaxHops bounds one successor lookup: the answer is at most
// this many replacements away from the closed blocker. It is the limit
// followSupersededChain applies for an issue blocked on a PR.
const blockedSuccessorMaxHops = 3

// blockedSuccessorLookupGrace is how long a successor lookup that keeps
// failing for a reason that may pass can delay a release: longer than one
// GitHub rate-limit window, so a limit that resets is waited out, and short
// enough that a failure that never passes costs one hour, not the hold.
const blockedSuccessorLookupGrace = time.Hour

// blockedSuccessorNow is the clock the grace is measured on; tests override
// it.
var blockedSuccessorNow = time.Now

// blockedSuccessorLookupState remembers one held item's failing lookup.
type blockedSuccessorLookupState struct {
	// first is when the lookup first failed.
	first time.Time
	// gaveUp: the grace ran out and the give-up was logged.
	gaveUp bool
}

// blockedSuccessorLookups holds the lookups that are failing right now,
// keyed by the held item and the closed blocker
// (blockedSuccessorLookupKey). It is in memory only: a restart starts the
// grace again, which delays a release by at most one more grace.
var blockedSuccessorLookups = struct {
	sync.Mutex
	m map[string]blockedSuccessorLookupState
}{m: map[string]blockedSuccessorLookupState{}}

// blockedSuccessorLookupKey names the lookup for the held item repo#number
// and its closed blocker ("owner/repo#N").
func blockedSuccessorLookupKey(repo string, number int, blocker string) string {
	return blockedOnSeenKey(repo, number) + "|" + strings.ToLower(strings.TrimSpace(blocker))
}

// blockedSuccessorLookupFailed records that the lookup for key failed for a
// reason that may pass. giveUp is false while the first failure is younger
// than blockedSuccessorLookupGrace: the caller keeps the hold. After that it
// is true on every further failure, and the caller releases as if there
// were no successor; news is true only the first time, so the give-up is
// logged once. The record is kept until the lookup completes or the hold is
// released (blockedSuccessorLookupSettled), so a release that could not be
// written on the give-up pass does not start a new grace on the next one.
func blockedSuccessorLookupFailed(key string) (giveUp, news bool) {
	now := blockedSuccessorNow()
	blockedSuccessorLookups.Lock()
	defer blockedSuccessorLookups.Unlock()
	st, ok := blockedSuccessorLookups.m[key]
	if !ok {
		blockedSuccessorLookups.m[key] = blockedSuccessorLookupState{first: now}
		return false, false
	}
	if now.Sub(st.first) <= blockedSuccessorLookupGrace {
		return false, false
	}
	news = !st.gaveUp
	st.gaveUp = true
	blockedSuccessorLookups.m[key] = st
	return true, news
}

// blockedSuccessorLookupSettled forgets the failing lookup for key: it
// completed (with a successor or none), or the hold it delayed is gone.
func blockedSuccessorLookupSettled(key string) {
	blockedSuccessorLookups.Lock()
	defer blockedSuccessorLookups.Unlock()
	delete(blockedSuccessorLookups.m, key)
}

// blockedSuccessorRefused reports whether err is GitHub definitely refusing
// a read of a successor lookup, as opposed to failing in a way that may
// pass. Definite: a REST 404, 410, 422, 401 or a 403 that is not a rate
// limit (isDefiniteIssueRefusal), a repository reference GitHub could never
// accept, and a GraphQL errors array (FORBIDDEN, NOT_FOUND, …) none of whose
// entries says rate limit. Everything else is treated as passing: a rate
// limit in any form, a 5xx, a network failure, a deadline.
func blockedSuccessorRefused(err error) bool {
	if err == nil {
		return false
	}
	var ge *graphQLErrors
	if errors.As(err, &ge) {
		for _, e := range ge.Errors {
			if strings.Contains(strings.ToUpper(e.Type), "RATE_LIMIT") || strings.Contains(strings.ToLower(e.Message), "rate limit") {
				return false
			}
		}
		return true
	}
	var bad *invalidRepoRefError
	if errors.As(err, &bad) {
		return true
	}
	return isDefiniteIssueRefusal(err)
}

// blockedSuccessorMarkerRE reads the link the Depends-on sweep leaves beside
// a successor's blocked-on marker: the successor, then the target it
// replaces.
var blockedSuccessorMarkerRE = regexp.MustCompile(`<!--\s*hive:depends-on-successor\s+pr=([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)\s+replaces=([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)\s*-->`)

// blockedSuccessorLink says successor holds a PR in place of replaces.
type blockedSuccessorLink struct {
	successor blockedPRTarget
	replaces  blockedPRTarget
}

// blockedSuccessorMarker renders the link marker for one replacement.
func blockedSuccessorMarker(successor, replaces blockedPRTarget) string {
	return "<!-- hive:depends-on-successor pr=" + successor.ref() + " replaces=" + replaces.ref() + " -->"
}

// blockedPRSuccessorMarkerComment is the one comment that moves a Depends-on
// hold to a successor: the successor's blocked-on marker (what every reader
// of the hold already understands), the link marker, and a short note. The
// wording avoids the phrase ParseSupersededBy reads, so this comment never
// makes the PR that carries it look superseded itself.
func blockedPRSuccessorMarkerComment(successor, replaces blockedPRTarget) string {
	return BlockedOnMarker(successor.repo, successor.number) + "\n" +
		blockedSuccessorMarker(successor, replaces) + "\n" +
		fmt.Sprintf("Hive: %s, which this pull request lists under `Depends-on:`, was not merged. Its declared successor %s is still open, so this pull request now waits on %s and keeps the `%s` label. Update the `Depends-on:` line when convenient.", replaces.ref(), successor.ref(), successor.ref(), BlockedLabel)
}

// blockedSuccessorLinks returns the link markers in comments the hive itself
// posted, oldest first. A link in anyone else's comment is ignored: it would
// let a stranger keep a merge held on a pull request of their choosing.
func (c *Client) blockedSuccessorLinks(comments []*gh.IssueComment) []blockedSuccessorLink {
	var out []blockedSuccessorLink
	for _, cm := range comments {
		if cm == nil {
			continue
		}
		login := safeGetLogin(cm.GetUser())
		if !c.isOwnBotLogin(login) && !c.IsHiveAuthorLogin(login) {
			continue
		}
		for _, m := range blockedSuccessorMarkerRE.FindAllStringSubmatch(cm.GetBody(), -1) {
			succ, errSucc := strconv.Atoi(m[2])
			old, errOld := strconv.Atoi(m[4])
			if errSucc != nil || errOld != nil || succ <= 0 || old <= 0 {
				continue
			}
			link := blockedSuccessorLink{
				successor: blockedPRTarget{repo: m[1], number: succ},
				replaces:  blockedPRTarget{repo: m[3], number: old},
			}
			if link.successor.key() == link.replaces.key() {
				continue
			}
			out = append(out, link)
		}
	}
	return out
}

// blockedSuccessorMarked is marked plus any linked successor missing from
// it, so a successor is settled even if only its link marker survived.
func blockedSuccessorMarked(marked []blockedPRTarget, links []blockedSuccessorLink) []blockedPRTarget {
	if len(links) == 0 {
		return marked
	}
	seen := make(map[string]bool, len(marked)+len(links))
	out := make([]blockedPRTarget, 0, len(marked)+len(links))
	for _, t := range marked {
		seen[t.key()] = true
		out = append(out, t)
	}
	for _, l := range links {
		if !seen[l.successor.key()] {
			seen[l.successor.key()] = true
			out = append(out, l.successor)
		}
	}
	return out
}

// blockedSuccessorIsSuccessor reports whether the hive marked t as standing
// in for another target.
func blockedSuccessorIsSuccessor(t blockedPRTarget, links []blockedSuccessorLink) bool {
	for _, l := range links {
		if l.successor.key() == t.key() {
			return true
		}
	}
	return false
}

// blockedSuccessorReplacedBy returns the successor the hive most recently
// marked for t.
func blockedSuccessorReplacedBy(t blockedPRTarget, links []blockedSuccessorLink) (blockedPRTarget, bool) {
	for i := len(links) - 1; i >= 0; i-- {
		if links[i].replaces.key() == t.key() {
			return links[i].successor, true
		}
	}
	return blockedPRTarget{}, false
}

// blockedSuccessorLive reports whether t may hold the PR: the body lists it
// under `Depends-on:`, or the hive marked it as the successor of a target
// that may hold the PR and is still closed without having merged. A
// successor therefore stops holding when the target it replaces is dropped
// from the body, and when that target is reopened or merges after all (the
// target then answers for itself). A target with no link costs no read.
func (c *Client) blockedSuccessorLive(ctx context.Context, t blockedPRTarget, listed map[string]bool, links []blockedSuccessorLink, cache map[string]blockedPRTargetState, visiting map[string]bool) bool {
	if listed[t.key()] {
		return true
	}
	if visiting[t.key()] {
		return false
	}
	visiting[t.key()] = true
	defer delete(visiting, t.key())
	for _, l := range links {
		if l.successor.key() != t.key() {
			continue
		}
		if !c.blockedSuccessorLive(ctx, l.replaces, listed, links, cache, visiting) {
			continue
		}
		if st := c.blockedPRTargetRead(ctx, l.replaces, cache); st.err == nil && !st.open && !st.merged {
			return true
		}
	}
	return false
}

// blockedSuccessorTrustedComment reports whether cm may declare a successor
// for an item itemAuthor opened (see the file comment).
func (c *Client) blockedSuccessorTrustedComment(cm *gh.IssueComment, itemAuthor string) bool {
	if cm == nil {
		return false
	}
	if login := strings.TrimSpace(safeGetLogin(cm.GetUser())); login != "" {
		if c.isOwnBotLogin(login) || c.IsHiveAuthorLogin(login) {
			return true
		}
		if author := strings.TrimSpace(itemAuthor); author != "" && strings.EqualFold(login, author) {
			return true
		}
	}
	return trustedCommentAssociations[strings.ToUpper(strings.TrimSpace(cm.GetAuthorAssociation()))]
}

// blockedSuccessorStatement reads the successor a comment declares for the
// item it sits on. A comment carrying one of the blocked sweeps' own markers
// is bookkeeping about what that item waits on ("Re-targeted: blocker #A was
// superseded by #B"), not a statement about the item, and is skipped.
func blockedSuccessorStatement(text string) (int, bool) {
	if strings.Contains(text, blockedUnblockedMarker) || blockedOnMarkerRE.MatchString(text) ||
		blockedOnIssueMarkerRE.MatchString(text) || blockedSuccessorMarkerRE.MatchString(text) {
		return 0, false
	}
	return ParseSupersededBy(text)
}

// blockedSuccessorDeclared returns the number a trusted statement names as
// the successor of the closed item repo#number: its own body first, then its
// comments, newest first (the order pullRequestSupersededBy reads them in).
// A comment list GitHub definitely refuses names no successor. A read that
// fails for a reason that may pass is returned, and the caller keeps the
// hold within the grace (blockedSuccessorLookupFailed).
func (c *Client) blockedSuccessorDeclared(ctx context.Context, repo string, number int, body, author string) (int, bool, error) {
	if n, ok := ParseSupersededBy(body); ok && n != number {
		return n, true, nil
	}
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		if blockedSuccessorRefused(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if !c.blockedSuccessorTrustedComment(comments[i], author) {
			continue
		}
		if n, ok := blockedSuccessorStatement(comments[i].GetBody()); ok && n != number {
			return n, true, nil
		}
	}
	return 0, false, nil
}

// blockedPRDeclaredSuccessor is blockedSuccessorDeclared for a Depends-on
// target, answered once per pass however many PRs depend on it.
func (c *Client) blockedPRDeclaredSuccessor(ctx context.Context, t blockedPRTarget, cache map[string]blockedPRTargetState) (int, bool, error) {
	st := c.blockedPRTargetRead(ctx, t, cache)
	if st.err != nil {
		if blockedSuccessorRefused(st.err) {
			return 0, false, nil
		}
		return 0, false, st.err
	}
	if !st.succDone {
		st.succ, st.succOK, st.succErr = c.blockedSuccessorDeclared(ctx, t.repo, t.number, st.body, st.author)
		st.succDone = true
		cache[t.key()] = st
	}
	return st.succ, st.succOK, st.succErr
}

// blockedPRSuccessorOf follows t, a Depends-on target that closed without
// merging, to the open pull request that replaced it. It reports false when
// no trusted statement names a successor, when the chain ends on something
// that is not an open pull request (merged, closed with no successor of its
// own, an issue, an item the App cannot read), when it loops, or when it
// leads back to self, the PR being held. In each of those cases the caller
// releases the hold as it always did. An error means a read failed for a
// reason that may pass (rate limit, 5xx, network): the caller keeps the hold
// within the grace and the next pass looks again.
func (c *Client) blockedPRSuccessorOf(ctx context.Context, self, t blockedPRTarget, cache map[string]blockedPRTargetState) (blockedPRTarget, bool, error) {
	seen := map[string]bool{t.key(): true, self.key(): true}
	cur := t
	for hop := 0; hop < blockedSuccessorMaxHops; hop++ {
		n, ok, err := c.blockedPRDeclaredSuccessor(ctx, cur, cache)
		if err != nil {
			return blockedPRTarget{}, false, fmt.Errorf("reading what replaced %s: %w", cur.ref(), err)
		}
		if !ok {
			return blockedPRTarget{}, false, nil
		}
		next := blockedPRTarget{repo: cur.repo, number: n}
		if seen[next.key()] {
			return blockedPRTarget{}, false, nil
		}
		seen[next.key()] = true
		st := c.blockedPRTargetRead(ctx, next, cache)
		switch {
		case st.err != nil && blockedSuccessorRefused(st.err):
			return blockedPRTarget{}, false, nil
		case st.err != nil:
			return blockedPRTarget{}, false, fmt.Errorf("reading %s, named as the successor of %s: %w", next.ref(), cur.ref(), st.err)
		case !st.isPR:
			return blockedPRTarget{}, false, nil
		case st.open:
			return next, true, nil
		case st.merged:
			return blockedPRTarget{}, false, nil
		}
		cur = next
	}
	return blockedPRTarget{}, false, nil
}

// markBlockedPRSuccessor posts the comment that moves pr's hold from
// l.replaces to l.successor, once: later passes find the link marker in the
// PR's comments and never reach this. The write goes through the write-back
// gate as the same kind as every other write of this sweep; a refusal is
// counted and the hold stays on for the pass regardless, so the next pass
// asks again.
func (c *Client) markBlockedPRSuccessor(ctx context.Context, repo string, pr *gh.PullRequest, labels []string, l blockedSuccessorLink, res *BlockedPRSweepResult) {
	number := pr.GetNumber()
	if !c.blockedPRWriteAllowed(repo, pr, labels, "blocked on "+l.successor.ref()+", the successor of "+l.replaces.ref(), res) {
		return
	}
	if err := c.CreateIssueComment(ctx, repo, number, blockedPRSuccessorMarkerComment(l.successor, l.replaces)); err != nil { // writeback:gated
		res.Errors++
		c.logger.Warn("blocked PR sweep: successor marker not posted", "repo", repo, "pr", number, "target", l.replaces.ref(), "successor", l.successor.ref(), "error", err)
		return
	}
	res.Marked++
	c.logger.Info("blocked PR sweep: Depends-on target was replaced; PR now held on its successor", "repo", repo, "pr", number, "target", l.replaces.ref(), "successor", l.successor.ref())
}

// blockedSuccessorIssue is the open issue a closed blocker issue led to.
type blockedSuccessorIssue struct {
	repo   string
	number int
	// duplicate: GitHub's duplicate closure of the blocker named it directly.
	duplicate bool
}

func (s blockedSuccessorIssue) ref() string { return s.repo + "#" + strconv.Itoa(s.number) }

// blockedSuccessorDuplicateOfQuery reads the canonical issue of a duplicate
// closure by the duplicate's node id, so it also answers for an issue that
// was transferred to another repository.
const blockedSuccessorDuplicateOfQuery = `query($id:ID!){node(id:$id){... on Issue{duplicateOf{number repository{nameWithOwner}}}}}`

// blockedSuccessorDuplicateOf returns the issue GitHub recorded nodeID as a
// duplicate of. The canonical issue may be in another repository. No node id,
// an issue with no canonical and a lookup GitHub definitely refuses (an
// errors array such as NOT_FOUND or FORBIDDEN, a REST-style refusal of the
// request) all report false; a failure that may pass is returned.
func (c *Client) blockedSuccessorDuplicateOf(ctx context.Context, nodeID string) (repo string, number int, ok bool, err error) {
	nodeID = strings.TrimSpace(nodeID)
	if nodeID == "" {
		return "", 0, false, nil
	}
	var out struct {
		Node *struct {
			DuplicateOf *struct {
				Number     int `json:"number"`
				Repository struct {
					NameWithOwner string `json:"nameWithOwner"`
				} `json:"repository"`
			} `json:"duplicateOf"`
		} `json:"node"`
	}
	if err := c.graphQL(ctx, blockedSuccessorDuplicateOfQuery, map[string]any{"id": nodeID}, &out); err != nil {
		if blockedSuccessorRefused(err) {
			return "", 0, false, nil
		}
		return "", 0, false, err
	}
	if out.Node == nil || out.Node.DuplicateOf == nil || out.Node.DuplicateOf.Number <= 0 {
		return "", 0, false, nil
	}
	owner, name, found := strings.Cut(strings.TrimSpace(out.Node.DuplicateOf.Repository.NameWithOwner), "/")
	if !found || validateRepoRef(owner, name) != nil {
		return "", 0, false, nil
	}
	return owner + "/" + name, out.Node.DuplicateOf.Number, true, nil
}

// blockedSuccessorIssueRef is the repository and number an issue payload
// says it lives at. They differ from the ones it was asked for by when the
// issue was transferred: GitHub redirects the old URL to the new one.
func blockedSuccessorIssueRef(issue *gh.Issue, repo string, number int) (string, int) {
	if n := issue.GetNumber(); n > 0 {
		number = n
	}
	if u := issue.GetRepositoryURL(); u != "" {
		if i := strings.LastIndex(u, "/repos/"); i >= 0 {
			owner, name, found := strings.Cut(strings.Trim(u[i+len("/repos/"):], "/"), "/")
			if found && validateRepoRef(owner, name) == nil {
				repo = owner + "/" + name
			}
		}
	}
	return repo, number
}

// blockedIssueSuccessorOf follows the closed blocker issue repo#number of the
// blocked issue t to the open issue that replaced it: the canonical issue of
// a duplicate closure, else the issue a trusted "Superseded by #N" names in
// the blocker's own repository. It reports false when nothing names a
// successor, when the chain ends on something that is not an open issue the
// App can read, when it loops, or when it leads back to t itself; the caller
// then unblocks as it always did. An error means a read failed for a reason
// that may pass and the caller keeps the block within the grace.
func (c *Client) blockedIssueSuccessorOf(ctx context.Context, t blockedIssueTarget, repo string, number int) (blockedSuccessorIssue, bool, error) {
	none := blockedSuccessorIssue{}
	norm := func(r string) string {
		owner, name := c.splitRepo(r)
		return owner + "/" + name
	}
	curRepo, curNumber := norm(repo), number
	selfKey := blockedOnSeenKey(norm(t.repo), t.number)
	seen := map[string]bool{blockedOnSeenKey(curRepo, curNumber): true, selfKey: true}
	cur, err := c.blockedSuccessorReadIssue(ctx, curRepo, curNumber)
	if err != nil {
		if blockedSuccessorRefused(err) {
			return none, false, nil
		}
		return none, false, fmt.Errorf("reading closed blocker %s#%d: %w", curRepo, curNumber, err)
	}
	curRepo, curNumber = blockedSuccessorIssueRef(cur, curRepo, curNumber)
	seen[blockedOnSeenKey(curRepo, curNumber)] = true
	for hop := 0; hop < blockedSuccessorMaxHops; hop++ {
		if cur.IsPullRequest() {
			return none, false, nil
		}
		nextRepo, nextNumber, duplicate, ok, err := c.blockedIssueDeclaredSuccessor(ctx, curRepo, curNumber, cur)
		if err != nil {
			return none, false, err
		}
		if !ok {
			return none, false, nil
		}
		nextRepo = norm(nextRepo)
		if nextOwner, nextName := c.splitRepo(nextRepo); validateRepoRef(nextOwner, nextName) != nil || nextNumber <= 0 {
			return none, false, nil
		}
		if seen[blockedOnSeenKey(nextRepo, nextNumber)] {
			return none, false, nil
		}
		seen[blockedOnSeenKey(nextRepo, nextNumber)] = true
		next, err := c.blockedSuccessorReadIssue(ctx, nextRepo, nextNumber)
		if err != nil {
			if blockedSuccessorRefused(err) {
				return none, false, nil
			}
			return none, false, fmt.Errorf("reading %s#%d, named as the successor of %s#%d: %w", nextRepo, nextNumber, curRepo, curNumber, err)
		}
		if next.IsPullRequest() {
			return none, false, nil
		}
		nextRepo, nextNumber = blockedSuccessorIssueRef(next, nextRepo, nextNumber)
		if strings.EqualFold(next.GetState(), "open") {
			if blockedOnSeenKey(nextRepo, nextNumber) == selfKey {
				return none, false, nil
			}
			return blockedSuccessorIssue{repo: nextRepo, number: nextNumber, duplicate: duplicate && hop == 0}, true, nil
		}
		seen[blockedOnSeenKey(nextRepo, nextNumber)] = true
		cur, curRepo, curNumber = next, nextRepo, nextNumber
	}
	return none, false, nil
}

// blockedSuccessorReadIssue reads one issue of a successor chain.
func (c *Client) blockedSuccessorReadIssue(ctx context.Context, repo string, number int) (*gh.Issue, error) {
	owner, name := c.splitRepo(repo)
	if err := validateRepoRef(owner, name); err != nil {
		return nil, err
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return nil, err
	}
	if issue == nil {
		return nil, fmt.Errorf("reading %s#%d: empty response", repo, number)
	}
	return issue, nil
}

// blockedIssueDeclaredSuccessor names what replaced the closed issue
// repo#number: the canonical issue when GitHub closed it as a duplicate
// (which may be in another repository), else the issue a trusted
// "Superseded by #N" names in the same repository.
func (c *Client) blockedIssueDeclaredSuccessor(ctx context.Context, repo string, number int, issue *gh.Issue) (succRepo string, succNumber int, duplicate, ok bool, err error) {
	if strings.EqualFold(strings.TrimSpace(issue.GetStateReason()), "duplicate") {
		dupRepo, dupNumber, found, dupErr := c.blockedSuccessorDuplicateOf(ctx, issue.GetNodeID())
		if dupErr != nil {
			return "", 0, false, false, fmt.Errorf("reading what %s#%d duplicates: %w", repo, number, dupErr)
		}
		if found {
			return dupRepo, dupNumber, true, true, nil
		}
	}
	n, found, declErr := c.blockedSuccessorDeclared(ctx, repo, number, issue.GetBody(), safeGetLogin(issue.GetUser()))
	if declErr != nil {
		return "", 0, false, false, fmt.Errorf("reading what replaced %s#%d: %w", repo, number, declErr)
	}
	if !found {
		return "", 0, false, false, nil
	}
	return repo, n, false, true, nil
}

// blockedOnIssueMarker renders the issue form of the blocked-on marker.
func blockedOnIssueMarker(repo string, issue int) string {
	return "<!-- hive:blocked-on issue=" + strings.TrimSpace(repo) + "#" + strconv.Itoa(issue) + " -->"
}

// blockedSuccessorIssueRetargetNote is the standalone comment that moves an
// issue's block from the closed blocker (ref, "owner/repo#N") to succ. Like
// the PR re-target it stays a comment of its own: the sweep reads the newest
// blocked-on marker by comment order.
func blockedSuccessorIssueRetargetNote(succ blockedSuccessorIssue, ref string) string {
	how := "was replaced by"
	if succ.duplicate {
		how = "is a duplicate of"
	}
	return blockedOnIssueMarker(succ.repo, succ.number) + "\n" +
		fmt.Sprintf("Re-targeted: blocker %s %s %s, which is still open.", ref, how, succ.ref())
}

// blockedSuccessorRetargetIssue runs when the issue t waits on (repo#number)
// is closed. It reports true when the block must stay for this pass: the
// blocker was replaced by an open issue and the block now names it (or
// already did, or the note could not be written yet), or a read failed for
// a reason that may pass and the grace has not run out. It reports false
// when the caller should unblock exactly as before: no successor, one that
// is closed or unreadable, a read GitHub definitely refuses, a read that
// kept failing past the grace, or a write-back policy that does not let the
// hive comment on this issue.
//
// scanned says the caller read t's comments this pass. When it did not (the
// per-pass comment budget ran out), the body may still name a blocker an
// earlier pass already moved off, so the comments are read here before
// anything is posted, as the PR re-target does.
func blockedSuccessorRetargetIssue(ctx context.Context, c *Client, t blockedIssueTarget, repo string, number int, scanned bool, res *BlockedIssueResult, logger *slog.Logger) bool {
	ref := repo + "#" + strconv.Itoa(number)
	key := blockedSuccessorLookupKey(t.repo, t.number, ref)
	// failed settles one failed read of this step: true keeps the block for
	// the pass, false releases it as before (a refusal, or the grace ran out).
	failed := func(err error, what string) bool {
		if blockedSuccessorRefused(err) {
			blockedSuccessorLookupSettled(key)
			return false
		}
		giveUp, news := blockedSuccessorLookupFailed(key)
		if !giveUp {
			res.Errors++
			logger.Warn("blocked sweep: "+what+" failed; block kept", "repo", t.repo, "issue", t.number, "blocker", ref, "error", err)
			return true
		}
		if news {
			logger.Warn("blocked sweep: successor lookup for a closed blocker kept failing for "+blockedSuccessorLookupGrace.String()+"; releasing as before", "repo", t.repo, "issue", t.number, "blocker", ref, "error", err)
		}
		return false
	}
	succ, ok, err := c.blockedIssueSuccessorOf(ctx, t, repo, number)
	if err != nil {
		return failed(err, "successor lookup for a closed blocker")
	}
	if !ok {
		blockedSuccessorLookupSettled(key)
		return false
	}
	if !scanned {
		scan, scanErr := c.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
		if scanErr != nil {
			return failed(scanErr, "comment lookup before re-target")
		}
		if scan.ok && (!scan.issue || !strings.EqualFold(scan.prRepo, repo) || scan.pr != number) {
			blockedSuccessorLookupSettled(key)
			stripBlockedHelpWanted(ctx, c, t, res, logger)
			return true
		}
	}
	item, err := c.WritebackItemForNumber(ctx, t.repo, t.number)
	if err != nil {
		return failed(err, "item lookup before re-target")
	}
	blockedSuccessorLookupSettled(key)
	note := blockedSuccessorIssueRetargetNote(succ, ref)
	gateRepo := c.blockedRepairRepo(t.repo)
	if d := c.WritebackAllowed(gateRepo, item, writeback.KindGateComment, note); !d.Allowed {
		if !writeback.IsContentReason(d.Reason) {
			// The policy does not let the hive comment here (a pause, the
			// repo mode, the level, the opt-in label). Without the note
			// nothing would say what the issue now waits on, so it is
			// unblocked as it always was. That ends the block, so this is
			// logged once.
			logger.Info("blocked sweep: write-back policy does not allow a re-target note; unblocking as before", "repo", t.repo, "issue", t.number, "blocker", ref, "successor", succ.ref(), "reason", d.Reason, "detail", d.Detail)
			return false
		}
		// The policy allows the note; the gate only declined this instance
		// (burst, duplicate, hourly cap). Keep the block and ask again on a
		// later pass, logging the hold once per issue and reason.
		level := slog.LevelDebug
		if c.WritebackGate().NoteHeld(gateRepo, t.number, writeback.KindGateComment, d.Reason) {
			level = slog.LevelInfo
		}
		logger.Log(ctx, level, "blocked sweep: re-target on a successor held by writeback gate; block kept", "repo", t.repo, "issue", t.number, "blocker", ref, "successor", succ.ref(), "reason", d.Reason, "detail", d.Detail)
		stripBlockedHelpWanted(ctx, c, t, res, logger)
		return true
	}
	c.WritebackGate().ForgetHeld(gateRepo, t.number, writeback.KindGateComment)
	// The note carries the issue form of the blocked-on marker. The next
	// pass reads the newest marker as the live blocker, finds the open
	// successor there and never comes back here, so it is posted once.
	if err := c.CreateIssueCommentBudgeted(ctx, t.repo, t.number, note); err != nil { // writeback:gated
		logger.Warn("blocked sweep: re-target note not posted; block kept", "repo", t.repo, "issue", t.number, "blocker", ref, "successor", succ.ref(), "error", err)
		return true
	}
	stripBlockedHelpWanted(ctx, c, t, res, logger)
	res.Retargeted++
	logger.Info("blocked sweep: issue re-targeted on its closed blocker's successor", "repo", t.repo, "issue", t.number, "old_blocker", ref, "new_blocker", succ.ref())
	return true
}
