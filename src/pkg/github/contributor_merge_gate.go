package github

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// Contributor-PR merge gate (hivecommons/hive#9624).
//
// A review-swarm verdict is advisory AI output. It must never be the thing
// that lets the hive merge a person's PR. The review relay already keeps the
// swarm's votes off contributor PRs (#9608), but the recorded verdict still fed
// merge eligibility, so a green, swarm-approved contributor PR landed in
// merge-eligible.json and became mergeable through the agent merge relay.
//
// The rule, shared by the governor's classifier and the merge relay:
//
//   - A PR opened by this hive (its App bot or project.ai_author) or by a bot
//     in auto_merge.trusted_bot_authors is gated exactly as before.
//   - Any other PR is a contributor PR. It is never merge-eligible unless the
//     operator opted in with auto_merge.contributor_prs, and even then only
//     once a person with write access has approved its current head on the
//     forge. The swarm verdict does not count toward that approval.
//
// "Any bot" is deliberately NOT trusted here, unlike the review relay's
// comment-only guard: anyone can register a GitHub App and open a fork PR as
// it, so for merging, only the bots the operator named are trusted.

// MergeAuthorClass is who opened a PR, as far as merging it is concerned.
type MergeAuthorClass string

const (
	// MergeAuthorHive is a PR opened by this hive: the App bot or
	// project.ai_author.
	MergeAuthorHive MergeAuthorClass = "hive"
	// MergeAuthorTrustedBot is a PR opened by a bot the operator listed in
	// auto_merge.trusted_bot_authors.
	MergeAuthorTrustedBot MergeAuthorClass = "trusted_bot"
	// MergeAuthorContributor is anyone else, including a PR whose author is
	// unknown (fail closed).
	MergeAuthorContributor MergeAuthorClass = "contributor"
)

// ContributorMergeReason is the leading text of every refusal the gate
// produces, in the dashboard verdict and in the merge relay's result file.
const ContributorMergeReason = "contributor PR: needs a maintainer's review"

const (
	// contributorMergeOptInOff explains a refusal on a hive that has not
	// opted in to merging contributor PRs.
	contributorMergeOptInOff = "this hive does not merge PRs it did not open (auto_merge.contributor_prs is off)"
	// contributorMergeNoApproval explains a refusal on a hive that has opted
	// in, for a PR no maintainer has approved at its current head.
	contributorMergeNoApproval = "no approving review of the current head from a person with write access (a review-swarm verdict does not count)"
)

// appAuthorPrefix is how some enumerators spell an App author ("app/<slug>").
const appAuthorPrefix = "app/"

// botLoginSuffix is the suffix GitHub reserves for App bot logins.
const botLoginSuffix = "[bot]"

// ContributorMergeRefusalReason is the full refusal text for a contributor PR:
// optIn reports whether auto_merge.contributor_prs is on, which decides what
// is missing (the opt-in itself, or a maintainer's approval).
func ContributorMergeRefusalReason(optIn bool) string {
	if !optIn {
		return ContributorMergeReason + "; " + contributorMergeOptInOff
	}
	return ContributorMergeReason + "; " + contributorMergeNoApproval
}

// ClassifyMergeAuthor decides whose PR this is for merge purposes. It reuses
// the hive's own identity (HiveIdentity: project.ai_author and the App bot
// login, the same resolver the duplicate-PR guard and the self-authorization
// gate use) plus the operator's trusted-bot set (lower-cased logins, as
// config.AutoMergeConfig.TrustedBotAuthorSet returns it). appAuthored is the
// enumerator's own "author is this client's App bot" finding.
//
// The body trailer (HiveAttributed / HiveAgent) is deliberately not consulted:
// anyone can type it into a PR description, so it cannot vouch for a merge.
func ClassifyMergeAuthor(author string, appAuthored bool, id HiveIdentity, trustedBots map[string]bool) MergeAuthorClass {
	if appAuthored {
		return MergeAuthorHive
	}
	login := normalizeMergeAuthorLogin(author)
	if login == "" {
		return MergeAuthorContributor
	}
	if id.Matches(login) {
		return MergeAuthorHive
	}
	if trustedBots[strings.ToLower(login)] {
		return MergeAuthorTrustedBot
	}
	return MergeAuthorContributor
}

// normalizeMergeAuthorLogin maps the "app/<slug>" spelling some enumerators
// use onto GitHub's "<slug>[bot]" login, so both compare against the same
// identity and trusted-bot set.
func normalizeMergeAuthorLogin(author string) string {
	login := strings.TrimSpace(author)
	if len(login) > len(appAuthorPrefix) && strings.EqualFold(login[:len(appAuthorPrefix)], appAuthorPrefix) {
		return login[len(appAuthorPrefix):] + botLoginSuffix
	}
	return login
}

// MaintainerApproval is one person's standing approval of a PR, as the forge
// reports it: their latest opinionated review is APPROVED, they are a person
// (not a bot, not this hive), and they have write access to the repository.
type MaintainerApproval struct {
	Login string `json:"login"`
	// CommitSHA is the commit the approval was given on. An approval of an
	// older head does not vouch for commits pushed after it.
	CommitSHA string `json:"commit_sha,omitempty"`
}

// MaintainerApprovalAt returns the login of a maintainer who approved the PR
// at head sha, from the facts the enumeration collected. ok is false when no
// such approval is known, including when the facts were never fetched.
func (pr PullRequest) MaintainerApprovalAt(sha string) (login string, ok bool) {
	sha = strings.TrimSpace(sha)
	if sha == "" || pr.Protection == nil {
		return "", false
	}
	for _, a := range pr.Protection.MaintainerApprovals {
		if strings.TrimSpace(a.Login) != "" && strings.EqualFold(strings.TrimSpace(a.CommitSHA), sha) {
			return a.Login, true
		}
	}
	return "", false
}

// graphQLActorTypeUser is the GraphQL __typename of a person's account. Bots,
// mannequins and organizations never count as a maintainer's approval.
const graphQLActorTypeUser = "User"

// isMaintainerApprover reports whether a GraphQL review author may vouch for a
// contributor PR: a person's account (not a bot, not this hive's App or
// project.ai_author, per isHumanAuthor) with push access to the repository.
func (c *Client) isMaintainerApprover(typename, login string, canPush bool) bool {
	if !canPush || typename != graphQLActorTypeUser {
		return false
	}
	return c.isHumanAuthor(&gh.User{Login: gh.Ptr(login), Type: gh.Ptr(typename)})
}

// ContributorMergePolicy is the operator configuration the merge relay's
// author gate reads on every request.
type ContributorMergePolicy struct {
	// AllowContributorPRs mirrors auto_merge.contributor_prs.
	AllowContributorPRs bool
	// TrustedBots is the lower-cased auto_merge.trusted_bot_authors set.
	TrustedBots map[string]bool
}

// SetContributorMergePolicy installs the merge relay's contributor-PR gate.
// fn is called per request so a config reload takes effect without a restart.
// Safe to call once at startup before the watcher goroutine runs; nil leaves
// the gate uninstalled (the F4 merge-eligible binding still applies the same
// rule through the governor's classifier).
func (c *Client) SetContributorMergePolicy(fn func() ContributorMergePolicy) {
	if c == nil {
		return
	}
	c.contributorMergePolicy = fn
}

// restCallerContributorMergeGuard labels the gate's REST calls for the budget
// view.
const restCallerContributorMergeGuard = "hive:merge_contributor_guard"

// contributorReviewPageSize and contributorReviewMaxPages bound the review
// listing: 100 reviews a page, at most 10 pages. A PR with more than 1000
// reviews is not one the hive should be merging on its own anyway, and a
// truncated listing can only miss an approval (fail closed), never invent one.
const (
	contributorReviewPageSize = 100
	contributorReviewMaxPages = 10
)

// Review states as the REST API spells them.
const (
	restReviewStateApproved  = "APPROVED"
	restReviewStateCommented = "COMMENTED"
	restReviewStatePending   = "PENDING"
)

// Repository permission levels that grant push access.
const (
	repoPermissionAdmin    = "admin"
	repoPermissionMaintain = "maintain"
	repoPermissionWrite    = "write"
)

// contributorMergeGuard is the merge relay's author gate. It returns a
// non-empty refusal when req names a contributor PR that may not be merged,
// and the PR author's login for the audit entry. err is a lookup failure: the
// caller treats it as a failed (retryable) attempt, never as an allow. With no
// policy installed it allows without a lookup.
func (c *Client) contributorMergeGuard(ctx context.Context, req MergeRequest) (refusal, author string, err error) {
	if c.contributorMergePolicy == nil {
		return "", "", nil
	}
	policy := c.contributorMergePolicy()
	owner, name := c.splitRepo(req.Repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, restCallerContributorMergeGuard), owner, name, req.Number)
	if err != nil {
		return "", "", fmt.Errorf("contributor gate: reading PR %s/%s#%d author: %w", owner, name, req.Number, err)
	}
	author = safeGetLogin(pr.GetUser())
	appAuthored := c.appBotLogin != "" && strings.EqualFold(author, c.appBotLogin)
	if ClassifyMergeAuthor(author, appAuthored, c.getHiveIdentity(), policy.TrustedBots) != MergeAuthorContributor {
		return "", author, nil
	}
	if !policy.AllowContributorPRs {
		return ContributorMergeRefusalReason(false), author, nil
	}
	approver, err := c.maintainerApprovalViaREST(ctx, owner, name, req.Number, req.ExpectSHA)
	if err != nil {
		return "", author, err
	}
	if approver == "" {
		return ContributorMergeRefusalReason(true), author, nil
	}
	return "", author, nil
}

// maintainerApprovalViaREST returns the login of a person with write access
// whose latest opinionated review of the PR is an APPROVE of commit sha, or ""
// when there is none. It reads the reviews API directly rather than trusting
// the governor's snapshot, because the relay is the last step before a merge.
func (c *Client) maintainerApprovalViaREST(ctx context.Context, owner, name string, number int, sha string) (string, error) {
	sha = strings.TrimSpace(sha)
	if sha == "" {
		return "", nil
	}
	ctx = WithRESTCaller(ctx, restCallerContributorMergeGuard)
	// Reviews arrive oldest first, so the last opinionated review per person
	// is their standing position; a DISMISSED review replaces an approval.
	latest := make(map[string]*gh.PullRequestReview)
	opts := &gh.ListOptions{PerPage: contributorReviewPageSize}
	for page := 0; page < contributorReviewMaxPages; page++ {
		reviews, resp, err := c.client.PullRequests.ListReviews(ctx, owner, name, number, opts)
		if err != nil {
			return "", fmt.Errorf("contributor gate: listing reviews of %s/%s#%d: %w", owner, name, number, err)
		}
		for _, r := range reviews {
			state := strings.ToUpper(strings.TrimSpace(r.GetState()))
			if state == restReviewStateCommented || state == restReviewStatePending {
				continue
			}
			login := strings.ToLower(strings.TrimSpace(r.GetUser().GetLogin()))
			if login == "" {
				continue
			}
			latest[login] = r
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	logins := make([]string, 0, len(latest))
	for login := range latest {
		logins = append(logins, login)
	}
	sort.Strings(logins)
	for _, login := range logins {
		r := latest[login]
		if !strings.EqualFold(strings.TrimSpace(r.GetState()), restReviewStateApproved) {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(r.GetCommitID()), sha) {
			continue
		}
		if !c.isHumanAuthor(r.GetUser()) {
			continue
		}
		perm, _, err := c.client.Repositories.GetPermissionLevel(ctx, owner, name, r.GetUser().GetLogin())
		if err != nil {
			return "", fmt.Errorf("contributor gate: reading %s's permission on %s/%s: %w", r.GetUser().GetLogin(), owner, name, err)
		}
		switch strings.ToLower(strings.TrimSpace(perm.GetPermission())) {
		case repoPermissionAdmin, repoPermissionMaintain, repoPermissionWrite:
			return r.GetUser().GetLogin(), nil
		}
	}
	return "", nil
}

// AuditActionMergeRequestRefused is recorded when the merge relay refuses a
// request on policy that the operator should see on the audit trail, today
// the contributor-PR gate (hivecommons/hive#9624). Detail carries repo=,
// number=, author= and reason=.
const AuditActionMergeRequestRefused = "merge_request_refused"

// auditReasonContributorPR is the reason= value of a contributor-gate refusal.
const auditReasonContributorPR = "contributor_pr"

// recordContributorMergeRefusal audits a contributor-gate refusal. The author
// login goes to the local audit log only; nothing here is posted to the forge.
func (c *Client) recordContributorMergeRefusal(req MergeRequest, author string) {
	c.recordCreationAudit(AuditActionMergeRequestRefused, InvocationMeta{Agent: req.Agent},
		"repo", req.Repo, "number", strconv.Itoa(req.Number),
		"author", author, "reason", auditReasonContributorPR)
}
