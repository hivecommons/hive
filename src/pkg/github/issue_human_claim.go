package github

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/claims"
)

// claimedByHumanLabel is the label the hive itself writes for a person's (or
// contributor's) ledger claim (claims.ClaimedByLabel). A bot applying it is
// the ledger speaking; a person applying it by hand is a claim.
const claimedByHumanLabel = "hive/claimed-by-human"

// humanClaimCacheTTL bounds how long one issue's human-claim verdict is
// reused. The contributor claim's renewal and the release tick both ask, so
// the cache keeps a live lease from costing a timeline read per progress
// report.
const humanClaimCacheTTL = time.Minute

// maintainerPermissionTTL bounds how long a login's repository permission
// is reused.
const maintainerPermissionTTL = 10 * time.Minute

// Via phrases of claims.HumanClaimReason.
const (
	humanClaimViaAssignee = "assigned"
	humanClaimViaComment  = "/claim comment"
	humanClaimViaLabel    = claimedByHumanLabel + " label"
	humanClaimViaLedger   = "claimed in the ledger"
)

var (
	// humanClaimCommentRe is a person's claim comment: the `/claim` command,
	// or the plain "Claimed; fix in progress." an owner's dispatcher posts
	// beside `gh issue edit --add-assignee`.
	humanClaimCommentRe = regexp.MustCompile(`(?i)^\s*(?:/claim\b|claimed\b)`)
	// humanUnclaimCommentRe withdraws a person's earlier claim comment.
	humanUnclaimCommentRe = regexp.MustCompile(`(?i)^\s*/unclaim\b`)
)

type humanClaimCacheEntry struct {
	reason  string
	expires time.Time
}

type maintainerCacheEntry struct {
	ok      bool
	expires time.Time
}

// humanClaimState holds the client's human-claim caches.
type humanClaimState struct {
	mu         sync.Mutex
	verdicts   map[string]humanClaimCacheEntry
	maintainer map[string]maintainerCacheEntry
}

// IssueHumanClaimReason reports a claim an owner or maintainer holds on the
// issue OUTSIDE the ledger, as a claims.HumanClaimReason, or "" when there is
// none (hivecommons/hive#18899). The ledger already ranks a person over a
// contributor, but a claim that never went through it (the operator's
// dispatcher runs `gh issue edit --add-assignee` plus a plain "Claimed; fix in
// progress." comment) never displaced a live contributor lease: #18866's
// contributor opened #18870 eleven minutes after the owner claimed it.
//
// Three signals count, each only from a person with write access to the
// repository, never from a bot or the hive's own App:
//   - an assignee the person assigned (the assigner is a maintainer; an
//     assignee who is the contributor holder is the maintainer handing the
//     issue to that contributor, not a claim against them);
//   - a `/claim` (or plain "Claimed...") comment from an OWNER, MEMBER or
//     COLLABORATOR, not later withdrawn by `/unclaim`;
//   - `hive/claimed-by-human` applied by hand rather than by the hive.
//
// holder is the contributor login whose claim is being checked ("" for a
// fresh claim by anyone): that login's own assignment or comment never
// counts against it. An open issue only; a closed issue holds nothing.
// A lookup error is returned so the caller keeps the claim it has.
func (c *Client) IssueHumanClaimReason(ctx context.Context, repo string, number int, holder string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	key := strings.ToLower(repo) + "#" + fmt.Sprint(number) + "|" + strings.ToLower(strings.TrimSpace(holder))
	now := time.Now()
	c.humanClaims.mu.Lock()
	if e, ok := c.humanClaims.verdicts[key]; ok && now.Before(e.expires) {
		c.humanClaims.mu.Unlock()
		return e.reason, nil
	}
	c.humanClaims.mu.Unlock()

	reason, err := c.readHumanClaim(ctx, repo, number, holder)
	if err != nil {
		return "", err
	}
	c.humanClaims.mu.Lock()
	if c.humanClaims.verdicts == nil {
		c.humanClaims.verdicts = map[string]humanClaimCacheEntry{}
	}
	if len(c.humanClaims.verdicts) > humanClaimCacheMax {
		c.humanClaims.verdicts = map[string]humanClaimCacheEntry{}
	}
	c.humanClaims.verdicts[key] = humanClaimCacheEntry{reason: reason, expires: now.Add(humanClaimCacheTTL)}
	c.humanClaims.mu.Unlock()
	return reason, nil
}

// humanClaimCacheMax bounds the verdict cache.
const humanClaimCacheMax = 512

func (c *Client) readHumanClaim(ctx context.Context, repo string, number int, holder string) (string, error) {
	owner, name := c.splitRepo(repo)
	// A claim the ledger already holds by a person (dashboard, hivectl) or by
	// one of the hive's own agents counts for the readers that do not consult
	// the ledger themselves (adoption).
	if claim, ok := c.ledgerClaimFor(owner, name, number, time.Now()); ok && !strings.EqualFold(claim.Identity, strings.TrimSpace(holder)) {
		switch strings.ToLower(claim.Kind) {
		case string(claims.KindHuman):
			return claims.HumanClaimReason(humanClaimViaLedger, claim.Identity), nil
		case string(claims.KindAgent):
			return claims.ReasonHiveClaim + ": " + claim.Identity, nil
		}
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return "", fmt.Errorf("checking issue for a human claim: %w", err)
	}
	if issue.IsPullRequest() || strings.EqualFold(issue.GetState(), "closed") {
		return "", nil
	}
	holder = strings.TrimSpace(holder)
	assignees := map[string]bool{}
	for _, login := range extractAssignees(issue.Assignees) {
		if login = strings.TrimSpace(login); login != "" && !strings.EqualFold(login, holder) && !c.isBotLogin(login) {
			assignees[strings.ToLower(login)] = true
		}
	}
	hasLabel := graceHasLabel(extractLabels(issue.Labels), claimedByHumanLabel)

	assigner, labeler, err := c.timelineClaimActors(ctx, owner, name, number, assignees, hasLabel)
	if err != nil {
		return "", err
	}
	for login := range assignees {
		by := assigner[login]
		if by == "" || strings.EqualFold(by, holder) || c.isBotLogin(by) {
			continue
		}
		ok, err := c.isMaintainerLogin(ctx, owner, name, by)
		if err != nil {
			return "", err
		}
		if ok {
			return claims.HumanClaimReason(humanClaimViaAssignee, by), nil
		}
	}
	if labeler != "" && !strings.EqualFold(labeler, holder) && !c.isBotLogin(labeler) {
		ok, err := c.isMaintainerLogin(ctx, owner, name, labeler)
		if err != nil {
			return "", err
		}
		if ok {
			return claims.HumanClaimReason(humanClaimViaLabel, labeler), nil
		}
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return "", err
	}
	if who := c.humanClaimCommenter(comments, holder); who != "" {
		return claims.HumanClaimReason(humanClaimViaComment, who), nil
	}
	return "", nil
}

// timelineClaimActors reads the issue timeline once and returns, per current
// assignee (lower-cased login), who last assigned them, and who last applied
// `hive/claimed-by-human` when that label is still on the issue. It reads
// nothing when there is no assignee and no such label.
func (c *Client) timelineClaimActors(ctx context.Context, owner, repo string, number int, assignees map[string]bool, hasLabel bool) (assigner map[string]string, labeler string, err error) {
	assigner = map[string]string{}
	if len(assignees) == 0 && !hasLabel {
		return assigner, "", nil
	}
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, lerr := c.client.Issues.ListIssueTimeline(ctx, owner, repo, number, opts)
		if lerr != nil {
			return nil, "", fmt.Errorf("listing timeline for %s/%s#%d: %w", owner, repo, number, lerr)
		}
		for _, ev := range events {
			if ev == nil {
				continue
			}
			actor := strings.TrimSpace(ev.GetActor().GetLogin())
			switch strings.ToLower(ev.GetEvent()) {
			case "assigned":
				if login := strings.ToLower(strings.TrimSpace(ev.GetAssignee().GetLogin())); assignees[login] {
					assigner[login] = actor
				}
			case "unassigned":
				delete(assigner, strings.ToLower(strings.TrimSpace(ev.GetAssignee().GetLogin())))
			case "labeled":
				if strings.EqualFold(ev.GetLabel().GetName(), claimedByHumanLabel) {
					labeler = actor
				}
			case "unlabeled":
				if strings.EqualFold(ev.GetLabel().GetName(), claimedByHumanLabel) {
					labeler = ""
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			return assigner, labeler, nil
		}
		opts.Page = resp.NextPage
	}
}

// humanClaimCommenter returns the login of an OWNER/MEMBER/COLLABORATOR whose
// latest claim comment is standing (not followed by their own `/unclaim`),
// skipping bots and the contributor holder; "" when there is none.
func (c *Client) humanClaimCommenter(comments []*gh.IssueComment, holder string) string {
	standing := map[string]bool{}
	var order []string
	for _, cm := range comments {
		if cm == nil || !ownerGraceAssociations[strings.ToUpper(cm.GetAuthorAssociation())] {
			continue
		}
		login := strings.TrimSpace(cm.GetUser().GetLogin())
		if login == "" || strings.EqualFold(login, holder) || c.isBotLogin(login) {
			continue
		}
		key := strings.ToLower(login)
		switch {
		case humanUnclaimCommentRe.MatchString(cm.GetBody()):
			standing[key] = false
		case humanClaimCommentRe.MatchString(cm.GetBody()):
			if _, seen := standing[key]; !seen {
				order = append(order, login)
			}
			standing[key] = true
		}
	}
	for _, login := range order {
		if standing[strings.ToLower(login)] {
			return login
		}
	}
	return ""
}

// isBotLogin reports whether login is an App/bot account: the "[bot]" suffix
// GitHub puts on every App, or one of this hive's own configured logins.
func (c *Client) isBotLogin(login string) bool {
	login = strings.TrimSpace(login)
	return strings.HasSuffix(login, botLoginSuffix) || c.isHiveBotAccount(login)
}

// isMaintainerLogin reports whether login has write access (write, maintain
// or admin) to the repository. A lookup error is returned, never read as
// "not a maintainer": the caller keeps the claim it holds.
func (c *Client) isMaintainerLogin(ctx context.Context, owner, repo, login string) (bool, error) {
	key := strings.ToLower(owner + "/" + repo + "|" + login)
	now := time.Now()
	c.humanClaims.mu.Lock()
	if e, ok := c.humanClaims.maintainer[key]; ok && now.Before(e.expires) {
		c.humanClaims.mu.Unlock()
		return e.ok, nil
	}
	c.humanClaims.mu.Unlock()
	level, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, repo, login)
	if err != nil {
		return false, fmt.Errorf("permission lookup for %s on %s/%s: %w", login, owner, repo, err)
	}
	role := strings.ToLower(strings.TrimSpace(level.GetRoleName()))
	perm := strings.ToLower(strings.TrimSpace(level.GetPermission()))
	ok := unparkPermissionLevels[perm] || unparkPermissionLevels[role]
	c.humanClaims.mu.Lock()
	if c.humanClaims.maintainer == nil {
		c.humanClaims.maintainer = map[string]maintainerCacheEntry{}
	}
	c.humanClaims.maintainer[key] = maintainerCacheEntry{ok: ok, expires: now.Add(maintainerPermissionTTL)}
	c.humanClaims.mu.Unlock()
	return ok, nil
}
