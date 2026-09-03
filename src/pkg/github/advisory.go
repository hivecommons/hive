package github

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"unicode/utf8"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/logscrub"
)

const (
	advisoryTitle     = "🐝 Hive Advisory Report"
	advisoryLabelName = "hive/advisory"
	advisoryLabelDesc = "Pinned advisory report from Hive agents"
	advisoryLabelClr  = "0e8a16"
)

// IssuesDisabledError reports that the advisory issue cannot be created or
// resolved because the target repo has its Issues feature turned off
// (has_issues=false). GitHub disables Issues on forks by default, so this is
// the common failure mode when a hive is pointed at a fork (#4329). It is a
// distinct class from the 403 "Resource not accessible by integration"
// App-permission failure: no amount of App reconfiguration fixes it, only a
// repo-settings change (or repointing the hive) does, so the message names
// that remedy.
type IssuesDisabledError struct {
	// Repo is the owner/name of the repo with Issues disabled.
	Repo string
	// Fork records whether the repo is a GitHub fork, the usual reason
	// Issues are off.
	Fork bool
}

func (e *IssuesDisabledError) Error() string {
	why := "common on forks"
	if e.Fork {
		why = "it is a fork, and GitHub disables Issues on forks by default"
	}
	return fmt.Sprintf("Issues are disabled on %s (%s) — enable Issues in the repo's Settings > General > Features, or point the hive at the upstream repo", e.Repo, why)
}

// EnsureAdvisoryIssue finds or creates the pinned advisory issue for a repo.
// Returns the issue number.
func (c *Client) EnsureAdvisoryIssue(ctx context.Context, repo string) (int, error) {
	if c == nil {
		return 0, ErrNoGitHubClient
	}
	owner := c.org
	if parts := strings.SplitN(repo, "/", 2); len(parts) == 2 {
		owner = parts[0]
		repo = parts[1]
	}

	num, err := c.findAdvisoryIssue(ctx, owner, repo)
	if err != nil {
		return 0, fmt.Errorf("searching for advisory issue: %w", err)
	}
	if num > 0 {
		c.logger.Info("found existing advisory issue", slog.String("repo", repo), slog.Int("number", num))
		return num, nil
	}

	// Before attempting to create, check whether the repo can hold issues at
	// all. A fork (or any repo with the Issues feature off) has no Issues tab:
	// the create below would fail with a 410 that reads like an auth problem,
	// and the fleet alert would blame the App. Name the real cause instead
	// (#4329). A failed metadata probe fails OPEN — the create attempt then
	// produces its own, real error.
	if ghRepo, _, repoErr := c.client.Repositories.Get(ctx, owner, repo); repoErr == nil {
		if ghRepo != nil && !ghRepo.GetHasIssues() {
			return 0, &IssuesDisabledError{Repo: owner + "/" + repo, Fork: ghRepo.GetFork()}
		}
	} else {
		c.logger.Warn("could not check repo has_issues before advisory issue create",
			slog.String("repo", repo), slog.String("error", repoErr.Error()))
	}

	c.logger.Info("creating advisory issue", slog.String("repo", repo))

	_, _, labelErr := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(advisoryLabelName),
		Description: gh.Ptr(advisoryLabelDesc),
		Color:       gh.Ptr(advisoryLabelClr),
	})
	labelExists := labelErr == nil || strings.Contains(labelErr.Error(), "already_exists")
	if !labelExists {
		c.logger.Warn("could not create advisory label, issue will be created without it", slog.String("error", labelErr.Error()))
	}

	body := "This issue collects advisory findings from Hive agents.\n\n" +
		"At lower ACMM levels, some agents work in advisory mode — they analyze code and post findings " +
		"here but do not create issues or PRs. At higher levels, designated agents (e.g. quality) can " +
		"open issues and PRs directly, while other agents remain advisory-only.\n\n" +
		"The governor posts periodic digest comments summarizing what advisory agents found.\n\n" +
		"**Do not close this issue.** It is a living document."

	// Invocation-attribution trail (attribution.go): this issue is created by
	// the hive itself, so the "agent" is the governor flow — backend/model are
	// genuinely unknown here and are omitted rather than guessed. Trailer is
	// config-gated; the audit entry below is unconditional.
	meta := InvocationMeta{Agent: AttributionAgentGovernor}
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

	req := &gh.IssueRequest{
		Title: gh.Ptr(advisoryTitle),
		Body:  gh.Ptr(body),
	}
	if labelExists {
		req.Labels = &[]string{advisoryLabelName}
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repo, req)
	if err != nil {
		return 0, fmt.Errorf("creating advisory issue: %w", err)
	}
	c.recordCreationAudit(AuditActionHiveIssueCreated, meta,
		"repo", owner+"/"+repo,
		"number", strconv.Itoa(issue.GetNumber()),
		"author", issue.GetUser().GetLogin(),
		"url", issue.GetHTMLURL(),
		"flow", "advisory")

	c.logger.Info("created advisory issue — pin it manually for visibility", slog.String("repo", repo), slog.Int("number", issue.GetNumber()))
	return issue.GetNumber(), nil
}

const (
	advisoryDigestPrefix    = "## 🐝 Advisory Digest"
	githubCommentCharLimit  = 65536
	truncationFooterPadding = 200
	// advisoryDigestAuditInterval is how often (in digest posts) an
	// advisory_commented audit entry is emitted, in addition to the very first
	// post. The digest is refreshed ~once a minute; a value of 50 yields roughly
	// one audit pulse per ~50 minutes, enough to show the loop is alive without
	// flooding the dashboard audit log.
	advisoryDigestAuditInterval = 50
	// advisoryDigestWriteThroughInterval bounds how many consecutive
	// unchanged-digest cycles may be skipped before a real write is forced
	// (#4818). A skipped cycle proves a PRIOR successful write, not current
	// write permission, so without a periodic write-through a 403 regression
	// (App loses issues:write, repo dropped from the installation) would stay
	// invisible for as long as the digest stayed quiet. At ~one cycle a minute
	// this forces roughly one real write per hour — enough to keep the
	// App-banner logic honest while still eliminating ~98% of the no-op edits.
	advisoryDigestWriteThroughInterval = 60
)

// PostAdvisoryDigest updates the existing digest comment on the advisory issue,
// or creates one if none exists. This prevents duplicate comments on each eval cycle.
func (c *Client) PostAdvisoryDigest(ctx context.Context, repo string, issueNum int, digest string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)

	// Belt-and-suspenders enforcement: no advisory body may ever carry a raw
	// @mention. The digest comment is rewritten every update cycle, so a
	// single "@username" in any of its findings would re-notify that human on
	// every refresh. FormatDigestMarkdown already neutralizes mentions at
	// render time; NeutralizeMentions is idempotent, so applying it again here
	// guarantees the invariant for every caller of this post path. The secret
	// scrub (logscrub) runs after, so nothing it redacts can re-introduce a
	// mention and neither pass weakens the other.
	digest = advisory.NeutralizeMentions(digest)

	// Canary-gated like CreateIssue/CreatePR/CreateIssueComment
	// (kubestellar/hive#4960): the digest aggregates agent-sourced advisory
	// finding text, so it is a secondary exfiltration channel that must honor
	// the same fail-closed contract as the primary write paths.
	if leak, ok := c.scanCanaryText(digest, "hive-advisory-digest:"+repo); ok {
		if c.canaryFailClosed {
			return fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	digest = truncateDigest(logscrub.ScrubString(digest))

	// Skip-if-unchanged guard (#4818): the digest is re-rendered ~once a
	// minute, and at steady state the body is byte-identical cycle after
	// cycle — rewriting the pinned comment anyway burned ~1,440 no-op GitHub
	// writes/day. Hash the FINAL body (post-scrub, post-truncation — exactly
	// the bytes that would go over the wire) and skip the whole forge
	// round-trip when it matches the last body this process successfully
	// wrote. Returning nil is deliberate: an unchanged skip is a HEALTHY
	// cycle, so the caller's success path still advances the
	// advisory-staleness freshness record (RecordAdvisoryPost). Three cases
	// always write: the first post after process start (no hash yet), a
	// changed body, and the periodic write-through that re-proves write
	// permission (see advisoryDigestWriteThroughInterval).
	key := fmt.Sprintf("%s/%s#%d", owner, repoName, issueNum)
	digestHash := fmt.Sprintf("%x", sha256.Sum256([]byte(digest)))
	c.advisoryMu.Lock()
	if c.advisoryDigestSkips == nil {
		c.advisoryDigestSkips = make(map[string]int)
	}
	if c.advisoryDigestHashes[key] == digestHash &&
		c.advisoryDigestSkips[key] < advisoryDigestWriteThroughInterval-1 {
		c.advisoryDigestSkips[key]++
		skips := c.advisoryDigestSkips[key]
		c.advisoryMu.Unlock()
		c.logger.Debug("advisory digest unchanged — skipping forge write",
			slog.String("repo", repo), slog.Int("issue", issueNum),
			slog.Int("consecutive_skips", skips))
		return nil
	}
	c.advisoryMu.Unlock()

	commentID, err := c.findDigestComment(ctx, owner, repoName, issueNum)
	if err != nil {
		c.logger.Warn("could not search for existing digest comment, creating new", slog.String("error", err.Error()))
	}

	var author string
	if commentID > 0 {
		edited, _, err := c.client.Issues.EditComment(ctx, owner, repoName, int64(commentID), &gh.IssueComment{
			Body: gh.Ptr(digest),
		})
		if err != nil {
			return fmt.Errorf("updating advisory digest comment on %s#%d: %w", repo, issueNum, err)
		}
		author = edited.GetUser().GetLogin()
	} else {
		comment, _, err := c.client.Issues.CreateComment(ctx, owner, repoName, issueNum, &gh.IssueComment{
			Body: gh.Ptr(digest),
		})
		if err != nil {
			return fmt.Errorf("posting advisory digest to %s#%d: %w", repo, issueNum, err)
		}
		author = comment.GetUser().GetLogin()
	}

	// Audit the FIRST post and every advisoryDigestAuditInterval-th one, not the
	// ~once-a-minute refreshes in between: auditing every update would flood the
	// audit log (1000s/day) and evict the discrete create→merge events. A
	// periodic pulse still proves the advisory loop is alive on the dashboard.
	c.advisoryMu.Lock()
	if c.advisoryDigestPosts == nil {
		c.advisoryDigestPosts = make(map[string]int)
	}
	if c.advisoryDigestHashes == nil {
		c.advisoryDigestHashes = make(map[string]string)
	}
	// Record the hash only after a SUCCESSFUL write (errors returned above),
	// so a failed edit keeps being retried every cycle rather than skipped as
	// "already posted". Reset the skip streak: the write-through clock starts
	// over from any real write.
	c.advisoryDigestHashes[key] = digestHash
	c.advisoryDigestSkips[key] = 0
	c.advisoryDigestPosts[key]++
	count := c.advisoryDigestPosts[key]
	c.advisoryMu.Unlock()

	if count == 1 || count%advisoryDigestAuditInterval == 0 {
		c.recordCreationAudit(AuditActionAdvisoryCommented, InvocationMeta{Agent: AttributionAgentGovernor},
			"repo", owner+"/"+repoName,
			"number", strconv.Itoa(issueNum),
			"author", author,
			"post_count", strconv.Itoa(count),
			"flow", "advisory-digest")
	}
	return nil
}

// ProbeIssueWrite (#2353) verifies that the current credential can actually
// WRITE to repo by performing a benign, self-reverting edit of the advisory
// issue: it reads the issue's current body and writes that same body straight
// back. GitHub records no visible change, but the request still exercises the
// exact issues:write path (and repo scope) a real digest post needs.
//
// This closes the recheck false-positive: a READ (finding the advisory issue)
// succeeds even when the App cannot write, and an installation-permission check
// cannot see that the repo is absent from the App installation's selected
// repos. Only a real write proves write capability, so callers that must not
// clear the write-forbidden banner (#2353) probe with this before declaring the
// App healthy.
//
// Returns nil on a successful write; the underlying error (e.g. the 403
// "Resource not accessible by integration") otherwise, so the caller can
// classify it exactly as it would a failed digest post.
func (c *Client) ProbeIssueWrite(ctx context.Context, repo string, issueNum int) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, repoName, issueNum)
	if err != nil {
		return fmt.Errorf("reading advisory issue %s#%d for write probe: %w", repo, issueNum, err)
	}
	// Write the current body back unchanged — a no-op edit that still requires
	// (and thus proves) issues:write on THIS repo.
	_, _, err = c.client.Issues.Edit(ctx, owner, repoName, issueNum, &gh.IssueRequest{
		Body: gh.Ptr(issue.GetBody()),
	})
	if err != nil {
		return fmt.Errorf("write probe on advisory issue %s#%d: %w", repo, issueNum, err)
	}
	return nil
}

func truncateDigest(digest string) string {
	if len(digest) <= githubCommentCharLimit {
		return digest
	}
	cutoff := githubCommentCharLimit - truncationFooterPadding
	lastNewline := strings.LastIndex(digest[:cutoff], "\n")
	if lastNewline > 0 {
		cutoff = lastNewline
	}
	// Ensure cutoff doesn't split a UTF-8 character
	for cutoff > 0 && !utf8.RuneStart(digest[cutoff]) {
		cutoff--
	}
	return digest[:cutoff] + fmt.Sprintf("\n\n---\n⚠️ *Digest truncated: %d → %d characters (GitHub limit: %d)*\n", len(digest), cutoff, githubCommentCharLimit)
}

func (c *Client) ensureAdvisoryLabel(ctx context.Context, owner, repo string, issueNum int) {
	_, _, labelErr := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(advisoryLabelName),
		Description: gh.Ptr(advisoryLabelDesc),
		Color:       gh.Ptr(advisoryLabelClr),
	})
	if labelErr != nil && !strings.Contains(labelErr.Error(), "already_exists") {
		return
	}
	_, _, _ = c.client.Issues.AddLabelsToIssue(ctx, owner, repo, issueNum, []string{advisoryLabelName})
}

func (c *Client) findDigestComment(ctx context.Context, owner, repo string, issueNum int) (int, error) {
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 50},
	}
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, issueNum, opts)
	if err != nil {
		return 0, err
	}
	var botAuthored int64
	for _, comment := range comments {
		if !strings.HasPrefix(comment.GetBody(), advisoryDigestPrefix) {
			continue
		}
		if c.appAuth == nil {
			// Token (PAT) client: historical prefix-only match. The credential
			// may legitimately be the human who authored the comment, and
			// authorship cannot be verified without an extra /user round trip.
			return int(comment.GetID()), nil
		}
		login := comment.GetUser().GetLogin()
		if c.appBotLogin != "" && login == c.appBotLogin {
			// Exactly our own bot comment — the one credential-safe choice.
			return int(comment.GetID()), nil
		}
		if strings.HasSuffix(login, "[bot]") || comment.GetUser().GetType() == "Bot" {
			// Bot-authored but not provably ours (bot login unknown, or a slug
			// mismatch between config and the real App). Remember the first as
			// a fallback rather than skipping it: refusing our own comment on
			// a misconfigured slug would create a duplicate every cycle.
			if botAuthored == 0 {
				botAuthored = comment.GetID()
			}
			continue
		}
		// A digest comment this App can never edit — e.g. one left behind by
		// the removed user-token fallback (#1927), authored by a human. GitHub
		// hard-forbids an App from editing a foreign-authored comment (403
		// "Resource not accessible by integration") no matter what the
		// installation grants, so adopting it wedges the digest forever
		// (kalantar-msb/soft-reflective#1). Skip it; if no bot-authored digest
		// comment exists a fresh App-authored one is created and every later
		// cycle edits THAT one, so nothing is duplicated per cycle.
		c.logger.Warn("skipping advisory digest comment not authored by this App — an installation token can never edit it",
			slog.String("repo", owner+"/"+repo),
			slog.Int("issue", issueNum),
			slog.Int64("comment_id", comment.GetID()),
			slog.String("author", login))
	}
	return int(botAuthored), nil
}

func (c *Client) findAdvisoryIssue(ctx context.Context, owner, repo string) (int, error) {
	// Use search API to find the advisory issue across all open issues,
	// regardless of how many issues the repo has.
	query := fmt.Sprintf(`repo:%s/%s is:issue is:open in:title "%s"`, owner, repo, advisoryTitle)
	result, _, err := c.client.Search.Issues(ctx, query, &gh.SearchOptions{
		ListOptions: gh.ListOptions{PerPage: 5},
	})
	if err == nil {
		for _, issue := range result.Issues {
			if issue.GetTitle() == advisoryTitle {
				return issue.GetNumber(), nil
			}
		}
	} else {
		c.logger.Warn("search API failed, falling back to label filter", slog.String("error", err.Error()))
	}

	// Fallback 1: list by label (works when search API is unavailable)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{advisoryLabelName},
		ListOptions: gh.ListOptions{PerPage: 10},
	}
	issues, _, listErr := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if listErr == nil {
		for _, issue := range issues {
			if issue.GetTitle() == advisoryTitle {
				return issue.GetNumber(), nil
			}
		}
	}

	// Fallback 2: scan recent open issues by title (handles the case where
	// the label was never applied — prevents duplicate advisory issues)
	const maxPagesToScan = 3
	listOpts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 50, Page: 1},
	}
	for page := 0; page < maxPagesToScan; page++ {
		allIssues, _, scanErr := c.client.Issues.ListByRepo(ctx, owner, repo, listOpts)
		if scanErr != nil {
			break
		}
		for _, issue := range allIssues {
			if issue.IsPullRequest() {
				continue
			}
			if issue.GetTitle() == advisoryTitle {
				c.logger.Info("found advisory issue via title scan (missing label)", slog.Int("number", issue.GetNumber()))
				c.ensureAdvisoryLabel(ctx, owner, repo, issue.GetNumber())
				return issue.GetNumber(), nil
			}
		}
		if len(allIssues) < 50 {
			break
		}
		listOpts.ListOptions.Page++
	}

	// Fallback 3: a CLOSED advisory issue is REUSED, not replaced (#4167).
	// The issue says "do not close this issue", but people close it anyway —
	// and creating a fresh one then splits the digest: the hive writes to the
	// new issue while everyone who subscribed to the old one watches a comment
	// that never updates again, which reads exactly like a wedged digest. So
	// reopen the most recent closed one and keep posting to the URL people
	// already follow. Only after this finds nothing does the caller create.
	if num, ok := c.findClosedAdvisoryIssue(ctx, owner, repo); ok {
		if _, _, err := c.client.Issues.Edit(ctx, owner, repo, num, &gh.IssueRequest{State: gh.Ptr("open")}); err != nil {
			// Cannot reopen (permissions, locked). Report NOT-FOUND rather than
			// the closed number: posting a digest to a closed issue would be
			// invisible, and the caller's create path at least yields a live one.
			c.logger.Warn("found closed advisory issue but could not reopen it",
				slog.Int("number", num), slog.String("error", err.Error()))
			return 0, nil
		}
		c.logger.Info("reopened closed advisory issue instead of creating a duplicate",
			slog.String("repo", repo), slog.Int("number", num))
		c.ensureAdvisoryLabel(ctx, owner, repo, num)
		return num, nil
	}
	return 0, nil
}

// findClosedAdvisoryIssue returns the most recently updated CLOSED advisory
// issue for a repo, if one exists. Split out from findAdvisoryIssue so the
// "reuse the issue people already subscribe to" rule is testable on its own.
func (c *Client) findClosedAdvisoryIssue(ctx context.Context, owner, repo string) (int, bool) {
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 50, Page: 1},
	}
	issues, _, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
	if err != nil {
		return 0, false
	}
	for _, issue := range issues {
		if issue.IsPullRequest() {
			continue
		}
		if issue.GetTitle() == advisoryTitle {
			return issue.GetNumber(), true
		}
	}
	return 0, false
}
