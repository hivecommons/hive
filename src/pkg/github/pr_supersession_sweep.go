package github

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/writeback"
)

const (
	DefaultSupersessionSweepMaxActions = 5
	supersessionSweepMarker            = "<!-- hive:pr-supersession-sweep -->"
)

// Supersession sweep audit actions the dashboard replays from the audit log
// (recorded as SupersessionSweepAuditPrefix + action), so producer and
// consumer share one name per action.
const (
	SupersessionSweepAuditPrefix           = "supersession-sweep-"
	SupersessionActionDetectedContributor  = "detected-contributor"
	SupersessionActionKeptOpen             = "kept-open"
	SupersessionActionCommentedContributor = "commented-contributor"
	// SupersessionActionSuppressedContributor records a detection the
	// write-back gate kept off GitHub (dashboard only).
	SupersessionActionSuppressedContributor = "suppressed-contributor"
	SupersessionActionCommentedGrace        = "commented-grace"
)

type SupersessionSweepOptions struct {
	MaxActions       int
	ACMMLevelForRepo func(repo string) int
	Audit            func(SupersessionSweepEvent)
	// CloseContributorPRs lets the sweep close a superseded human-authored PR
	// once the guards in DecideSupersessionAutoClose hold and GracePeriod has
	// elapsed since the notice with no human reply (hivecommons/hive#11418).
	// Off by default: without it contributor PRs get a notice and an
	// operator recommendation, but are never closed automatically.
	CloseContributorPRs bool
	// GracePeriod is how long a contributor PR stays open after the
	// supersession notice. Non-positive means DefaultSupersessionGracePeriod.
	GracePeriod time.Duration
	// AutoCloseSuperseded is governor.auto_close_superseded: a hive-authored
	// superseded PR whose head GitHub refuses to update (merge conflict) is
	// closed as superseded instead of waiting on the fix lane for a rebase
	// that no longer serves anything (hivecommons/hive#14965). nil means on.
	AutoCloseSuperseded func() bool
}

// autoCloseSupersededOn resolves AutoCloseSuperseded (nil → on).
func (o SupersessionSweepOptions) autoCloseSupersededOn() bool {
	return o.AutoCloseSuperseded == nil || o.AutoCloseSuperseded()
}

type SupersessionSweepEvent struct {
	Repo      string
	Number    int
	Author    string
	IssueRepo string
	Issue     int
	CloserPR  int
	Action    string
	Reason    string
}

type SupersessionSweepResult struct {
	Closed    []SupersessionSweepEvent
	Commented []SupersessionSweepEvent
	// Resolved are hive-authored PRs the sweep resolved itself instead of
	// asking a person: head updated onto base (SupersessionActionRebased),
	// body rewritten to a reference (SupersessionActionResolved), or the
	// update refused (SupersessionActionRebaseFailed).
	Resolved []SupersessionSweepEvent
	Seen     int
	Skipped  int
}

// Actions counts the write actions the sweep took, against MaxActions.
func (r *SupersessionSweepResult) Actions() int {
	if r == nil {
		return 0
	}
	return len(r.Closed) + len(r.Commented) + len(r.Resolved)
}

type supersessionCloser struct {
	Repo     string
	Number   int
	URL      string
	MergedAt time.Time
}

func (c *Client) SweepSupersededOpenPRs(ctx context.Context, opts SupersessionSweepOptions) (*SupersessionSweepResult, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	maxActions := opts.MaxActions
	if maxActions <= 0 {
		maxActions = DefaultSupersessionSweepMaxActions
	}
	result := &SupersessionSweepResult{}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	now := time.Now()

	for _, repo := range c.getRepos() {
		if result.Actions() >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if result.Actions() >= maxActions {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			event, action, err := c.trySweepSupersededPR(ctx, repo, owner, repoName, pr, identity, now, opts)
			if err != nil {
				return result, err
			}
			switch action {
			case "closed":
				result.Closed = append(result.Closed, event)
			case "commented":
				result.Commented = append(result.Commented, event)
			case "resolved":
				result.Resolved = append(result.Resolved, event)
			default:
				result.Skipped++
				continue
			}
			if opts.Audit != nil {
				opts.Audit(event)
			}
		}
	}
	return result, nil
}

func (c *Client) listOpenPRsForSupersessionSweep(ctx context.Context, owner, repo string) ([]*gh.PullRequest, error) {
	opts := &gh.PullRequestListOptions{
		State:       "open",
		ListOptions: gh.ListOptions{PerPage: claimSearchPerPage},
	}
	var all []*gh.PullRequest
	for page := 0; page < claimSearchMaxPages; page++ {
		prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
		if err != nil {
			return nil, fmt.Errorf("listing open PRs for supersession sweep in %s/%s: %w", owner, repo, err)
		}
		all = append(all, prs...)
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return all, nil
}

func (c *Client) trySweepSupersededPR(ctx context.Context, prRepo, prOwner, prRepoName string, pr *gh.PullRequest, identity HiveIdentity, now time.Time, opts SupersessionSweepOptions) (SupersessionSweepEvent, string, error) {
	// Holds gate writes, not detection: a sentinel must not hide supersession
	// from the tracker. No comments, labels or closes happen while held.
	labels := labelNames(pr.Labels)
	held := c.isHeld(labels) || c.isExempt(labels)
	candidate, closer, detected, err := c.detectSupersession(ctx, prRepo, pr, identity, now)
	if err != nil {
		return SupersessionSweepEvent{}, "", err
	}
	if !detected {
		if held {
			return SupersessionSweepEvent{}, "", nil
		}
		// Nothing supersedes this PR (any more). A park an earlier pass
		// put on it for a cause that has since gone — the claimed issue was
		// reopened, the claim was rewritten — clears with that cause
		// (hivecommons/hive#14155); a person's needs-human stays.
		return c.releaseStaleSupersessionPark(ctx, prRepo, prOwner, prRepoName, pr)
	}

	event := SupersessionSweepEvent{
		Repo:      prRepo,
		Number:    pr.GetNumber(),
		Author:    safeGetLogin(pr.GetUser()),
		IssueRepo: candidate.Repo,
		Issue:     candidate.Issue,
		CloserPR:  closer.Number,
	}

	if held {
		if opts.Audit != nil && !identity.Matches(event.Author) {
			event.Action = SupersessionActionDetectedContributor
			event.Reason = "held; awaiting an operator decision"
			opts.Audit(event)
		}
		return event, "", nil
	}
	reason := ""
	ownPR := identity.Matches(event.Author)
	if !ownPR {
		// Write-back gate: every contributor path below (notice, park,
		// auto-close, park release) writes on a PR the hive did not open.
		// Below the level supersession_notice needs, or without the opt-in
		// label, the detection is audited and nothing else happens.
		// This is a policy pre-check (no body yet): the content-aware
		// check runs below, once the notice is rendered.
		item := c.sentinelWritebackItem(pr)
		if d := c.WritebackGate().Allowed(prRepo, item, writeback.KindSupersessionNotice); !d.Allowed {
			if opts.Audit != nil {
				event.Action = SupersessionActionSuppressedContributor
				event.Reason = "write-back gate: " + d.Reason
				opts.Audit(event)
			}
			return event, "", nil
		}
	}
	openFiles, closerFiles, err := c.prFileSets(ctx, prOwner, prRepoName, pr.GetNumber(), closer)
	if err != nil {
		return SupersessionSweepEvent{}, "", err
	}
	extra := supersessionExtraFiles(openFiles, closerFiles)
	subset := len(extra) == 0
	if !subset {
		reason = supersessionReasonExtraFiles
	}

	level := 0
	if opts.ACMMLevelForRepo != nil {
		level = opts.ACMMLevelForRepo(prRepo)
	}
	if ownPR {
		return c.resolveOwnSupersededPR(ctx, prRepo, prOwner, prRepoName, pr, event, closer, closerFiles, subset, level, opts.autoCloseSupersededOn())
	}

	blockingExtra := supersessionBlockingExtraFiles(openFiles, closerFiles)
	if len(blockingExtra) == 0 {
		reason = ""
	}
	event.Reason = reason
	if reason == "" {
		_, event.Reason = supersessionSizeAllowsAutoClose(supersessionSizeLabel(pr), -1)
	}
	// Record every detection, even when the notice is already present or
	// automatic closure is disabled. This does not consume a write action.
	if opts.Audit != nil {
		detected := event
		detected.Action = SupersessionActionDetectedContributor
		opts.Audit(detected)
	}
	if supersessionHasLabel(pr, SupersessionKeepOpenLabel) {
		if opts.Audit != nil {
			event.Action = SupersessionActionKeptOpen
			opts.Audit(event)
		}
		if supersessionHasLabel(pr, issueNeedsHumanLabel) {
			return c.releaseStaleSupersessionPark(ctx, prRepo, prOwner, prRepoName, pr)
		}
		reason = supersessionReasonKeptOpen
	}
	if reason == "" && opts.CloseContributorPRs && level >= acmmLevelAutonomous &&
		c.WritebackAllowed(prRepo, c.sentinelWritebackItem(pr), writeback.KindSupersessionClose, "close superseded PR").Allowed {
		return c.trySupersessionAutoCloseContributorPR(ctx, prOwner, prRepoName, pr, event, closer, identity, now, opts.GracePeriod, candidate.Closing)
	}
	notice := supersessionDecisionNotice(prOwner, closer, reason)
	if reason == supersessionReasonExtraFiles {
		notice += "\n\nFiles the merged PR did not touch: " + supersessionFilesSummary(extra) + "."
		if candidate.Closing {
			// A contributor's PR that adds files the merged PR did not: the
			// hive cannot rebase or rewrite someone else's PR, so a person
			// decides — parked at 085 with the files named, never a bare label.
			return c.escalateSupersededPR(ctx, prRepo, prOwner, prRepoName, pr, event, closer, notice)
		}
		// The PR only references the issue (`Refs #N`, `Part of #N`, a
		// branch name): it never claimed to deliver it, so the issue's
		// closer cannot have superseded it. Say so, never park — #13959
		// and #14090 (Part of / Refs a closed tracker) sat at 085 for
		// this until a maintainer removed the label (hivecommons/hive#14155).
	}
	if candidate.Closing && reason != supersessionReasonKeptOpen {
		return c.escalateSupersededPR(ctx, prRepo, prOwner, prRepoName, pr, event, closer, notice)
	}
	event.Action = SupersessionActionCommentedContributor
	body := renderSupersessionComment(event, closer, notice)
	if !ownPR {
		// Content-aware gate on the real body: an identical notice already
		// on this PR, or the same notice just sent to another PR, is held
		// exactly like a notice that already stands.
		if d := c.WritebackAllowed(prRepo, c.sentinelWritebackItem(pr), writeback.KindSupersessionNotice, body); !d.Allowed {
			return c.releaseStaleSupersessionPark(ctx, prRepo, prOwner, prRepoName, pr)
		}
	}
	changed, err := c.ensureSupersessionComment(ctx, prOwner, prRepoName, pr.GetNumber(), body)
	if err != nil {
		return event, "", err
	}
	if !changed {
		// The notice stands from an earlier pass. If that pass parked the
		// PR for a reason this one no longer escalates, the park goes.
		return c.releaseStaleSupersessionPark(ctx, prRepo, prOwner, prRepoName, pr)
	}
	return event, "commented", nil
}

// detectSupersession finds the claim whose issue another merged PR closed.
// detected is false when the PR is not superseded: it claims nothing, one
// of its closing claims is still open, no closer resolves, or an earlier
// pass already rewrote the claim into a reference.
func (c *Client) detectSupersession(ctx context.Context, prRepo string, pr *gh.PullRequest, identity HiveIdentity, now time.Time) (candidate IssueClaim, closer supersessionCloser, detected bool, err error) {
	det, err := c.detectSupersessionDetail(ctx, prRepo, pr, identity, now)
	return det.candidate, det.closer, det.detected, err
}

// supersessionDetail is detectSupersessionDetail's answer: the merged-closer
// supersession (candidate/closer/detected) plus what the hive-authored
// auto-close rule reads when no merged closer resolves — whether every
// candidate claim is closed (allClosingClosed) and the first closed claim
// (closedClaim), which names the issue in the close comment.
type supersessionDetail struct {
	candidate        IssueClaim
	closer           supersessionCloser
	detected         bool
	allClosingClosed bool
	closedClaim      IssueClaim
	// closingClaims: the PR states what it delivers (`Fixes #N`); a
	// bare-reference PR never reads as "nothing left to deliver".
	closingClaims bool
	// allNotPlanned: every candidate issue was closed as not planned
	// (wontfix) — the one no-closer case where nothing is left to deliver
	// for certain; a "completed" close whose PR did not resolve may be a
	// transient lookup failure.
	allNotPlanned bool
}

func (c *Client) detectSupersessionDetail(ctx context.Context, prRepo string, pr *gh.PullRequest, identity HiveIdentity, now time.Time) (supersessionDetail, error) {
	var det supersessionDetail
	claims := supersessionCandidateClaims(claimsFromPR(pr, prRepo, identity, now))
	if len(claims) == 0 {
		return det, nil
	}
	det.allNotPlanned = true
	for _, claim := range claims {
		issueOwner, issueRepoName := c.splitRepo(claim.Repo)
		issue, _, err := c.client.Issues.Get(ctx, issueOwner, issueRepoName, claim.Issue)
		if err != nil {
			return supersessionDetail{}, fmt.Errorf("checking claimed issue %s#%d for PR %s#%d: %w", claim.Repo, claim.Issue, prRepo, pr.GetNumber(), err)
		}
		if !strings.EqualFold(issue.GetState(), "closed") {
			// An issue this PR claims to close is still open, so the PR is
			// still live work whatever happened to its other claims. Before
			// hivecommons/hive#12349 this escalated to needs-human as "also
			// claims another issue that is still open", which flagged #12322
			// (Fixes open #12117, Part of closed #12080) as superseded.
			return supersessionDetail{}, nil
		}
		if det.closedClaim.Issue == 0 {
			det.closedClaim = claim
		}
		if !strings.EqualFold(issue.GetStateReason(), issueStateReasonNotPlanned) {
			det.allNotPlanned = false
		}
		resolved, ok, err := c.resolveIssueClosingPR(ctx, claim.Repo, issueOwner, issueRepoName, claim.Issue, pr.GetNumber())
		if err != nil {
			return supersessionDetail{}, err
		}
		if ok && det.candidate.Issue == 0 {
			det.candidate = claim
			det.closer = resolved
		}
	}
	det.allClosingClosed = true
	det.closingClaims = claims[0].Closing
	if det.candidate.Issue == 0 {
		return det, nil
	}
	if supersessionResolvedInBody(pr.GetBody(), det.candidate.Issue, det.closer.Number) {
		// An earlier pass already turned this claim into a reference; the
		// issue stays closed, so without this check the rewrite would read
		// as a fresh supersession every tick.
		return supersessionDetail{closedClaim: det.closedClaim, allClosingClosed: true, closingClaims: det.closingClaims, allNotPlanned: det.allNotPlanned}, nil
	}
	det.detected = true
	return det, nil
}

// supersessionCandidateClaims picks the claims whose closure can supersede
// the PR. A closing-keyword claim (`Fixes #N`) is the PR's statement of what
// it delivers, so when any exist only they count: a bare `Part of #N` /
// `Refs #N` reference is context, and a closed parent must not mark a PR
// superseded while the issue it actually fixes is open (#12322 vs #12080).
// With no closing claim at all the weaker tiers (branch-name heuristic,
// bare references) stand in as before.
func supersessionCandidateClaims(claims []IssueClaim) []IssueClaim {
	var closing []IssueClaim
	for _, claim := range claims {
		if claim.Closing {
			closing = append(closing, claim)
		}
	}
	if len(closing) > 0 {
		return closing
	}
	return claims
}

func (c *Client) resolveIssueClosingPR(ctx context.Context, issueDisplayRepo, owner, repo string, issue, currentPR int) (supersessionCloser, bool, error) {
	if closer, ok := c.closedByPullRequestReference(ctx, owner, repo, issue, currentPR); ok {
		return closer, true, nil
	}
	sha, err := c.issueClosingCommit(ctx, owner, repo, issue)
	if err != nil || sha == "" {
		return supersessionCloser{}, false, err
	}
	prs, _, err := c.client.PullRequests.ListPullRequestsWithCommit(ctx, owner, repo, sha, nil)
	if err != nil {
		return supersessionCloser{}, false, fmt.Errorf("resolving closing commit %s for %s#%d: %w", sha, issueDisplayRepo, issue, err)
	}
	for _, pr := range prs {
		if pr == nil || pr.GetNumber() == currentPR || pr.GetMergedAt().Time.IsZero() {
			continue
		}
		return supersessionCloser{Repo: issueDisplayRepo, Number: pr.GetNumber(), URL: pr.GetHTMLURL(), MergedAt: pr.GetMergedAt().Time}, true, nil
	}
	return supersessionCloser{}, false, nil
}

// closedByReferencePageCap bounds closedByPullRequestsReferences pagination
// (10 pages of 10 nodes) so a pathological issue cannot loop the sweep.
const closedByReferencePageCap = 10

func (c *Client) closedByPullRequestReference(ctx context.Context, owner, repo string, issue, currentPR int) (supersessionCloser, bool) {
	displayRepo := owner + "/" + repo
	var latest supersessionCloser
	found := false
	after := ""
	for page := 0; page < closedByReferencePageCap; page++ {
		variables := map[string]any{
			"owner": owner,
			"repo":  repo,
			"issue": issue,
			"after": nil,
		}
		if after != "" {
			variables["after"] = after
		}
		payload := map[string]any{
			"query":     `query($owner:String!,$repo:String!,$issue:Int!,$after:String){repository(owner:$owner,name:$repo){issue(number:$issue){closedByPullRequestsReferences(first:10,after:$after){pageInfo{hasNextPage endCursor}nodes{number url merged mergedAt}}}}}`,
			"variables": variables,
		}
		req, err := c.client.NewRequest("POST", "graphql", payload)
		if err != nil {
			return supersessionCloser{}, false
		}
		var resp struct {
			Data struct {
				Repository struct {
					Issue struct {
						ClosedByPullRequestsReferences struct {
							PageInfo struct {
								HasNextPage bool   `json:"hasNextPage"`
								EndCursor   string `json:"endCursor"`
							} `json:"pageInfo"`
							Nodes []struct {
								Number   int       `json:"number"`
								URL      string    `json:"url"`
								Merged   bool      `json:"merged"`
								MergedAt time.Time `json:"mergedAt"`
							} `json:"nodes"`
						} `json:"closedByPullRequestsReferences"`
					} `json:"issue"`
				} `json:"repository"`
			} `json:"data"`
		}
		// Fail closed: a partial read could surface a stale closer, which is
		// the very bug paging fixes. Callers fall back to the closing commit.
		if _, err := c.client.Do(ctx, req, &resp); err != nil {
			return supersessionCloser{}, false
		}
		refs := resp.Data.Repository.Issue.ClosedByPullRequestsReferences
		for _, node := range refs.Nodes {
			if node.Number == currentPR || !node.Merged {
				continue
			}
			if !sameRepoPullURL(node.URL, owner, repo, node.Number) {
				continue
			}
			if !found || node.MergedAt.After(latest.MergedAt) {
				latest = supersessionCloser{Repo: displayRepo, Number: node.Number, URL: node.URL, MergedAt: node.MergedAt}
				found = true
			}
		}
		if !refs.PageInfo.HasNextPage || refs.PageInfo.EndCursor == "" {
			break
		}
		after = refs.PageInfo.EndCursor
	}
	return latest, found
}

// sameRepoPullURL reports whether a PR URL from closedByPullRequestsReferences
// names pull request number in owner/repo. GitHub can list a merged PR from
// another repository there, so only a same-repo PR may be named by number.
// The host is ignored so GitHub Enterprise URLs compare the same way.
func sameRepoPullURL(rawURL, owner, repo string, number int) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	want := "/" + owner + "/" + repo + "/pull/" + strconv.Itoa(number)
	return strings.EqualFold(strings.TrimSuffix(u.Path, "/"), want)
}

func (c *Client) issueClosingCommit(ctx context.Context, owner, repo string, issue int) (string, error) {
	opts := &gh.ListOptions{PerPage: 100}
	latest := ""
	for page := 0; page < claimSearchMaxPages; page++ {
		events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, repo, issue, opts)
		if err != nil {
			return "", fmt.Errorf("listing issue timeline for %s/%s#%d: %w", owner, repo, issue, err)
		}
		for _, ev := range events {
			if ev == nil || ev.GetEvent() != "closed" || ev.GetCommitID() == "" {
				continue
			}
			latest = ev.GetCommitID()
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latest, nil
}

// prFileSets reads the open PR's files and the closer's.
func (c *Client) prFileSets(ctx context.Context, owner, repo string, openPR int, closer supersessionCloser) (openFiles, closerFiles map[string]bool, err error) {
	openFiles, err = c.prFileSet(ctx, owner, repo, openPR)
	if err != nil {
		return nil, nil, err
	}
	closerOwner, closerRepoName := c.splitRepo(closer.Repo)
	closerFiles, err = c.prFileSet(ctx, closerOwner, closerRepoName, closer.Number)
	if err != nil {
		return nil, nil, err
	}
	return openFiles, closerFiles, nil
}

func (c *Client) prFileSet(ctx context.Context, owner, repo string, number int) (map[string]bool, error) {
	opts := &gh.ListOptions{PerPage: 100}
	out := map[string]bool{}
	for page := 0; page < claimSearchMaxPages; page++ {
		files, resp, err := c.client.PullRequests.ListFiles(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing files for PR %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, file := range files {
			if file == nil || file.GetFilename() == "" {
				continue
			}
			out[file.GetFilename()] = true
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return out, nil
}

// supersessionDecisionNotice leads a contributor PR's notice with the one call
// the repo operator has to make, not a request for code review
// (hivecommons/hive#13116). operator is the repository owner, who can merge or
// close; the PR author is deliberately not mentioned. note is the keep reason
// from DecideSupersessionAutoClose or the sweep, "" when the sweep had none.
func supersessionDecisionNotice(operator string, closer supersessionCloser, note string) string {
	addressee := "maintainers"
	if operator = strings.TrimSpace(operator); operator != "" {
		addressee = "@" + operator
	}
	switch note {
	case supersessionReasonKeptOpen:
		// The decision is already recorded on the PR; do not reopen it.
		return fmt.Sprintf("An operator chose to keep this PR open (label `%s`), so the sweep will not close it even though #%d already merged and closed the claimed issue. Remove the label to let the sweep decide again.", SupersessionKeepOpenLabel, closer.Number)
	case supersessionReasonExtraFiles:
		return fmt.Sprintf("**Decision needed (%s):** #%d already merged and closed the claimed issue, but %s. Recommended: **keep open** until the extra changes are reviewed, or close as superseded if they are not wanted. Another option: merge anyway.", addressee, closer.Number, note)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "**Decision needed (%s):** this PR appears to duplicate #%d, which already merged: its implementation files overlap the merged PR (additional tests, docs and changelog fragments do not block this recommendation; the sweep compares file names, not contents). Recommended: **close as superseded**. Other options: keep open, or merge anyway.", addressee, closer.Number)
	if note != "" {
		fmt.Fprintf(&b, " The sweep did not close it automatically: %s.", note)
	}
	return b.String()
}

func renderSupersessionComment(event SupersessionSweepEvent, closer supersessionCloser, reason string) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionSweepMarker)
	fmt.Fprintln(&b, "Supersession sweep found that this PR's claimed issue is already closed by another merged PR.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Claimed issue: %s#%d\n", event.IssueRepo, event.Issue)
	switch {
	case closer.Number <= 0:
		fmt.Fprintln(&b, "- Closing PR: none (the issue was closed without a merged PR)")
	case closer.URL != "":
		fmt.Fprintf(&b, "- Closing PR: [#%d](%s)\n", closer.Number, closer.URL)
	default:
		fmt.Fprintf(&b, "- Closing PR: %s#%d\n", closer.Repo, closer.Number)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, reason)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the supersession sweep; it is not duplicated._")
	return b.String()
}

// findSupersessionComment returns the sweep's marker comment on the PR, or
// nil when none has been posted.
func (c *Client) findSupersessionComment(ctx context.Context, owner, repo string, number int) (*gh.IssueComment, error) {
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, number, &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return nil, fmt.Errorf("listing supersession comments on %s/%s#%d: %w", owner, repo, number, err)
	}
	for _, cm := range comments {
		if cm != nil && strings.Contains(cm.GetBody(), supersessionSweepMarker) {
			return cm, nil
		}
	}
	return nil, nil
}

// ensureSupersessionComment posts the sweep's marker comment or edits the
// existing one in place. changed is false when an identical comment was
// already there, which tells the caller this escalation was delivered on an
// earlier cycle.
func (c *Client) ensureSupersessionComment(ctx context.Context, owner, repo string, number int, desiredBody string) (changed bool, err error) {
	existing, err := c.findSupersessionComment(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if existing != nil {
		if existing.GetBody() == desiredBody {
			return false, nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, existing.GetID(), &gh.IssueComment{Body: &desiredBody}); err != nil {
			return false, fmt.Errorf("editing supersession comment on %s/%s#%d: %w", owner, repo, number, err)
		}
		return true, nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: &desiredBody}); err != nil {
		return false, fmt.Errorf("creating supersession comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return true, nil
}
