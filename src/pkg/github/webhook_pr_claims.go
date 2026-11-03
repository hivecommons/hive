package github

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// PR→issue claim linking on the webhook path (hivecommons/hive#14814).
//
// The duplicate-PR claim ledger used to learn which issues an open PR
// claims only from FetchClaimScan, which runs on the reconcile pass — on
// hive's own spoke about an hour apart during the incident. A `pull_request`
// webhook's targeted re-read patched the PR row into the cached snapshot but
// never re-ran the claim link, so an owner PR opened with `Fixes #N` between
// passes left #N offerable: #14765 was handed to a contributor 9 minutes
// after #14772 (Fixes #14765) opened, #14789 13 minutes after #14791. The
// delivery already carries the PR's title, body, author and head branch —
// everything claimsFromPR reads — so the claims are parsed from the payload
// and recorded the moment the PR exists, closed or merged, at no API cost.

// PRClaimDelivery is what one `pull_request` webhook says about the issues
// the PR claims: the claims parsed from its payload with the ledger's own
// grammar, and the PR state that decides how the ledger applies them.
type PRClaimDelivery struct {
	// Repo is the PR's repository in the configured spelling.
	Repo string
	// Number is the PR number.
	Number int
	// Action is the webhook action (opened, edited, closed, …).
	Action string
	// State is the PR state as the payload reports it: PRStateOpen,
	// PRStateClosed (unmerged) or PRStateMerged.
	State string
	// MergedAt is when the PR merged; zero unless State is PRStateMerged.
	MergedAt time.Time
	// Claims are the issue claims the PR's title, body and head branch make,
	// exactly as the reconcile pass would parse them.
	Claims []IssueClaim
	// At is the delivery time.
	At time.Time
}

// ClaimedIssues lists the issue numbers the delivery claims in the PR's own
// repository, ascending, for the receiver's response and log line.
func (d PRClaimDelivery) ClaimedIssues() []int {
	seen := map[int]bool{}
	var out []int
	for _, c := range d.Claims {
		if c.Repo != d.Repo || c.Issue <= 0 || seen[c.Issue] {
			continue
		}
		seen[c.Issue] = true
		out = append(out, c.Issue)
	}
	sort.Ints(out)
	return out
}

// CoveringClaims lists the strong (closing-keyword or branch-name) claims an
// OPEN delivery makes on issues in the PR's own repository: the issues a
// contributor already dispatched on should stop working, because this PR
// now covers them. Non-closing references ("Refs #N") are not covers, and
// a closed or merged PR covers nothing here. One claim per issue, ascending.
func (d PRClaimDelivery) CoveringClaims() []IssueClaim {
	if d.State != PRStateOpen || !webhookPRCoverActions[d.Action] {
		return nil
	}
	seen := map[int]bool{}
	var out []IssueClaim
	for _, c := range d.Claims {
		if c.Repo != d.Repo || c.Issue <= 0 || c.Reference || seen[c.Issue] {
			continue
		}
		seen[c.Issue] = true
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Issue < out[j].Issue })
	return out
}

// PRClaimsHook is told about every `pull_request` delivery whose action can
// change which issues the PR claims or whether the PR is still open. The
// governor registers one that applies the delivery to the duplicate-PR claim
// ledger (ClaimLedger.ApplyPRDelivery) and filters the cached actionable
// snapshot, so the claim takes effect without waiting for the reconcile pass.
type PRClaimsHook func(PRClaimDelivery)

var (
	prClaimsHookMu sync.RWMutex
	prClaimsHookFn PRClaimsHook
)

// SetPRClaimsHook installs (or, with nil, removes) the process-wide
// PRClaimsHook.
func SetPRClaimsHook(fn PRClaimsHook) {
	prClaimsHookMu.Lock()
	prClaimsHookFn = fn
	prClaimsHookMu.Unlock()
}

func prClaimsHook() PRClaimsHook {
	prClaimsHookMu.RLock()
	defer prClaimsHookMu.RUnlock()
	return prClaimsHookFn
}

// webhookPRClaimActions are the `pull_request` actions after which the PR's
// claims may differ from what the ledger holds: the PR appeared, its text
// changed, its head moved (a branch rename arrives as synchronize), it left
// or re-entered draft, or it closed. Label and review-request actions carry
// the same PR text and are skipped so they do not re-save the ledger.
var webhookPRClaimActions = map[string]bool{
	"opened":             true,
	"reopened":           true,
	"edited":             true,
	"synchronize":        true,
	"ready_for_review":   true,
	"converted_to_draft": true,
	"closed":             true,
}

// webhookPRClaimDelivery builds the PRClaimDelivery for a `pull_request`
// event; ok is false for every other event, for an action that cannot
// change the claims, or for a payload with no PR object.
func (c *Client) webhookPRClaimDelivery(event, configured string, p webhookEventPayload, now time.Time) (PRClaimDelivery, bool) {
	if event != "pull_request" || p.PullRequest == nil || p.PullRequest.Number <= 0 || !webhookPRClaimActions[p.Action] {
		return PRClaimDelivery{}, false
	}
	wp := p.PullRequest
	pr := &gh.PullRequest{
		Number:  gh.Ptr(wp.Number),
		Title:   gh.Ptr(wp.Title),
		Body:    gh.Ptr(wp.Body),
		HTMLURL: gh.Ptr(wp.HTMLURL),
		State:   gh.Ptr(wp.State),
		Merged:  gh.Ptr(wp.Merged),
		Head:    &gh.PullRequestBranch{Ref: gh.Ptr(wp.Head.Ref), SHA: gh.Ptr(wp.Head.SHA)},
	}
	if wp.User != nil {
		pr.User = &gh.User{Login: gh.Ptr(wp.User.Login)}
	}
	if wp.MergedAt != nil {
		pr.MergedAt = &gh.Timestamp{Time: *wp.MergedAt}
	}
	if wp.ClosedAt != nil {
		pr.ClosedAt = &gh.Timestamp{Time: *wp.ClosedAt}
	}
	if wp.CreatedAt != nil {
		pr.CreatedAt = &gh.Timestamp{Time: *wp.CreatedAt}
	}
	d := PRClaimDelivery{Repo: configured, Number: wp.Number, Action: p.Action, State: observedPRState(pr), At: now}
	switch {
	case wp.Merged || (wp.MergedAt != nil && !wp.MergedAt.IsZero()):
		d.State = PRStateMerged
		if wp.MergedAt != nil {
			d.MergedAt = *wp.MergedAt
		}
		if d.MergedAt.IsZero() {
			d.MergedAt = now
		}
	case p.Action == "closed", !strings.EqualFold(d.State, PRStateOpen):
		// GitHub reports a merged PR as state "closed" too; merged is
		// decided above, so what is left here closed without merging.
		d.State = PRStateClosed
	}
	identity := c.getHiveIdentity()
	if identity.AppLogin == "" {
		identity.AppLogin = c.appBotLogin
	}
	d.Claims = claimsFromPR(pr, configured, identity, now)
	for i := range d.Claims {
		d.Claims[i].PRState = d.State
	}
	return d, true
}

// ApplyPRDelivery applies one `pull_request` webhook to the ledger and
// reports how many claims it recorded and how many it released. It makes
// the ledger say what the next FetchClaimScan would say about this PR:
//
//   - every unsettled claim the PR held is dropped first, so a PR edited to
//     stop naming an issue, or closed without merging, releases it;
//   - an open PR's claims are recorded as the scan records open claims, the
//     rank rule in insertLocked still deciding a collision with another PR's
//     claim on the same issue;
//   - a merged PR's claims are recorded as MergedPR claims anchored at the
//     merge, as the merged-PR scan does (#6867).
//
// FirstObservedAt carries forward when the same PR already claimed the same
// issue, so the weak-claim deferral window is not reset by the delivery.
// Callers persist with Save. A nil ledger or an empty delivery is a no-op.
func (l *ClaimLedger) ApplyPRDelivery(d PRClaimDelivery) (recorded, released int) {
	if l == nil || d.Number <= 0 || d.Repo == "" {
		return 0, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := d.At
	if now.IsZero() {
		now = l.now()
	}
	prev := make(map[string]IssueClaim, len(l.claims))
	dropped := map[string]bool{}
	for key, c := range l.claims {
		prev[key] = c
		if c.PRNumber == d.Number && c.PRRepo == d.Repo && !c.MergedPR {
			delete(l.claims, key)
			dropped[key] = true
		}
	}
	if d.State == PRStateClosed {
		return 0, len(dropped)
	}
	for _, c := range d.Claims {
		if c.Issue <= 0 || c.Repo == "" {
			continue
		}
		c.ObservedAt = now
		c.PRNumber, c.PRRepo = d.Number, d.Repo
		if d.State == PRStateMerged {
			c.MergedPR = true
			c.MergedAt = d.MergedAt
			if c.MergedAt.IsZero() {
				c.MergedAt = now
			}
			c.PRState = PRStateMerged
			c.FirstObservedAt = c.MergedAt
		} else {
			c.PRState = PRStateOpen
		}
		c = l.anchorFirstObservedLocked(prev, c)
		l.insertLocked(c)
		if held := l.claims[c.Key()]; held.PRNumber == d.Number && held.PRRepo == d.Repo {
			recorded++
			delete(dropped, c.Key())
		}
	}
	return recorded, len(dropped)
}

// webhookPRCoverActions are the `pull_request` actions after which an open
// PR may newly cover an issue: it appeared, or its text changed. synchronize
// carries the same text and is skipped so a push does not re-read every
// covered issue.
var webhookPRCoverActions = map[string]bool{
	"opened":           true,
	"reopened":         true,
	"edited":           true,
	"ready_for_review": true,
}

// webhookPRCoverTimeout bounds the background CoveredBy submissions one
// delivery spawns (one issue read and label write each).
const webhookPRCoverTimeout = 2 * time.Minute

// webhookPRCoverIssues lists the same-repo issues an open PR delivery covers
// for the stage engine: the strong, hive-authored closing claims. External
// PRs and non-closing references stay the sweep's call (the issue is kept
// visible with the PR as context, hivecommons/hive#8876); a closed or merged
// PR covers nothing (close-on-merge and Uncovered own those). Ascending,
// deduplicated.
func webhookPRCoverIssues(d PRClaimDelivery) []int {
	if d.State != PRStateOpen || !webhookPRCoverActions[d.Action] {
		return nil
	}
	seen := map[int]bool{}
	var out []int
	for _, c := range d.Claims {
		if c.Repo != d.Repo || c.Issue <= 0 || c.ExternalAuthor || c.Reference || seen[c.Issue] {
			continue
		}
		seen[c.Issue] = true
		out = append(out, c.Issue)
	}
	sort.Ints(out)
	return out
}

// coverIssuesForWebhookPR stages every issue an open hive PR delivery
// closes as 040-covered-by-pr, off the webhook response path. The ledger
// hook above already keeps the issues out of the offerable set; this is the
// stage side of the same fact, which until now only the hive-open-pr relay
// emitted — and only for the issue the request declared, so the siblings a
// PR also closed stayed 010-actionable with the scanner as their actor
// until the next sweep (hivecommons/hive#14910, #14911 under #14921). Gated
// on the issue-stage lane: with it off nothing writes stage labels.
func (c *Client) coverIssuesForWebhookPR(repo string, d PRClaimDelivery) {
	if c == nil || !c.issueStagePolicy().Enabled {
		return
	}
	issues := webhookPRCoverIssues(d)
	if len(issues) == 0 {
		return
	}
	c.stageFacts.background.Add(1)
	go func() {
		defer c.stageFacts.background.Done()
		ctx, cancel := context.WithTimeout(context.Background(), webhookPRCoverTimeout)
		defer cancel()
		c.coverIssuesByPR(ctx, repo, d.Number, issues)
		if c.logger != nil {
			c.logger.Info("webhook PR cover: issues staged as covered by the PR",
				"repo", repo, "pr", d.Number, "action", d.Action, "issues", issues, "source", "webhook")
		}
	}()
}
