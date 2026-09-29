package github

import (
	"context"
	"fmt"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

const (
	DefaultSupersessionSweepMaxActions = 5
	supersessionSweepMarker            = "<!-- hive:pr-supersession-sweep -->"
)

type SupersessionSweepOptions struct {
	MaxActions       int
	ACMMLevelForRepo func(repo string) int
	Audit            func(SupersessionSweepEvent)
}

type SupersessionSweepEvent struct {
	Repo      string
	Number    int
	Author    string
	IssueRepo string
	Issue     int
	CloserPR  int
	Action    string
}

type SupersessionSweepResult struct {
	Closed    []SupersessionSweepEvent
	Commented []SupersessionSweepEvent
	Seen      int
	Skipped   int
}

type supersessionCloser struct {
	Repo   string
	Number int
	URL    string
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
		if len(result.Closed)+len(result.Commented) >= maxActions {
			break
		}
		owner, repoName := c.splitRepo(repo)
		prs, err := c.listOpenPRsForSupersessionSweep(ctx, owner, repoName)
		if err != nil {
			return result, err
		}
		for _, pr := range prs {
			if len(result.Closed)+len(result.Commented) >= maxActions {
				break
			}
			if pr == nil {
				continue
			}
			result.Seen++
			event, action, err := c.trySweepSupersededPR(ctx, repo, owner, repoName, pr, identity, now, opts.ACMMLevelForRepo)
			if err != nil {
				return result, err
			}
			switch action {
			case "closed":
				result.Closed = append(result.Closed, event)
			case "commented":
				result.Commented = append(result.Commented, event)
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

func (c *Client) trySweepSupersededPR(ctx context.Context, prRepo, prOwner, prRepoName string, pr *gh.PullRequest, identity HiveIdentity, now time.Time, levelForRepo func(string) int) (SupersessionSweepEvent, string, error) {
	claims := claimsFromPR(pr, prRepo, identity, now)
	if len(claims) == 0 {
		return SupersessionSweepEvent{}, "", nil
	}

	var candidate IssueClaim
	var closer supersessionCloser
	hasOpenClaim := false
	for _, claim := range claims {
		issueOwner, issueRepoName := c.splitRepo(claim.Repo)
		issue, _, err := c.client.Issues.Get(ctx, issueOwner, issueRepoName, claim.Issue)
		if err != nil {
			return SupersessionSweepEvent{}, "", fmt.Errorf("checking claimed issue %s#%d for PR %s#%d: %w", claim.Repo, claim.Issue, prRepo, pr.GetNumber(), err)
		}
		if !strings.EqualFold(issue.GetState(), "closed") {
			hasOpenClaim = true
			continue
		}
		resolved, ok, err := c.resolveIssueClosingPR(ctx, claim.Repo, issueOwner, issueRepoName, claim.Issue, pr.GetNumber())
		if err != nil {
			return SupersessionSweepEvent{}, "", err
		}
		if ok && candidate.Issue == 0 {
			candidate = claim
			closer = resolved
		}
	}
	if candidate.Issue == 0 {
		return SupersessionSweepEvent{}, "", nil
	}

	event := SupersessionSweepEvent{
		Repo:      prRepo,
		Number:    pr.GetNumber(),
		Author:    safeGetLogin(pr.GetUser()),
		IssueRepo: candidate.Repo,
		Issue:     candidate.Issue,
		CloserPR:  closer.Number,
	}

	reason := ""
	if hasOpenClaim {
		reason = "this PR also claims another issue that is still open"
	} else {
		subset, err := c.prFilesSubset(ctx, prOwner, prRepoName, pr.GetNumber(), closer)
		if err != nil {
			return SupersessionSweepEvent{}, "", err
		}
		if !subset {
			reason = "this PR touches files the merged PR did not"
		}
	}

	ownPR := identity.Matches(event.Author)
	if !ownPR {
		event.Action = "commented-contributor"
		body := renderSupersessionComment(event, closer, "A different merged PR closed the claimed issue. Leaving this contributor PR open for a human to review.")
		if err := c.ensureSupersessionComment(ctx, prOwner, prRepoName, pr.GetNumber(), body); err != nil {
			return event, "", err
		}
		return event, "commented", nil
	}

	level := 0
	if levelForRepo != nil {
		level = levelForRepo(prRepo)
	}
	if reason == "" && level >= acmmLevelFullyAutonomous {
		event.Action = "closed"
		body := renderSupersessionComment(event, closer, "Closing this hive-authored PR because the claimed issue was already closed by the merged PR below.")
		if err := c.ensureSupersessionComment(ctx, prOwner, prRepoName, pr.GetNumber(), body); err != nil {
			return event, "", err
		}
		closed := "closed"
		if _, _, err := c.client.Issues.Edit(ctx, prOwner, prRepoName, pr.GetNumber(), &gh.IssueRequest{State: &closed}); err != nil {
			return event, "", fmt.Errorf("closing superseded PR %s#%d: %w", prRepo, pr.GetNumber(), err)
		}
		return event, "closed", nil
	}

	if reason == "" {
		reason = "this hive is below ACMM L6, so the sweep will not close PRs automatically"
	}
	event.Action = "commented-needs-human"
	body := renderSupersessionComment(event, closer, reason+". A human should decide whether to close this PR.")
	if err := c.ensureSupersessionComment(ctx, prOwner, prRepoName, pr.GetNumber(), body); err != nil {
		return event, "", err
	}
	if _, _, err := c.client.Issues.AddLabelsToIssue(ctx, prOwner, prRepoName, pr.GetNumber(), []string{issueNeedsHumanLabel}); err != nil {
		return event, "", fmt.Errorf("labeling superseded PR %s#%d: %w", prRepo, pr.GetNumber(), err)
	}
	return event, "commented", nil
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
		return supersessionCloser{Repo: issueDisplayRepo, Number: pr.GetNumber(), URL: pr.GetHTMLURL()}, true, nil
	}
	return supersessionCloser{}, false, nil
}

func (c *Client) closedByPullRequestReference(ctx context.Context, owner, repo string, issue, currentPR int) (supersessionCloser, bool) {
	payload := map[string]any{
		"query": `query($owner:String!,$repo:String!,$issue:Int!){repository(owner:$owner,name:$repo){issue(number:$issue){closedByPullRequestsReferences(first:10){nodes{number url merged}}}}}`,
		"variables": map[string]any{
			"owner": owner,
			"repo":  repo,
			"issue": issue,
		},
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
						Nodes []struct {
							Number int    `json:"number"`
							URL    string `json:"url"`
							Merged bool   `json:"merged"`
						} `json:"nodes"`
					} `json:"closedByPullRequestsReferences"`
				} `json:"issue"`
			} `json:"repository"`
		} `json:"data"`
	}
	if _, err := c.client.Do(ctx, req, &resp); err != nil {
		return supersessionCloser{}, false
	}
	displayRepo := owner + "/" + repo
	for _, node := range resp.Data.Repository.Issue.ClosedByPullRequestsReferences.Nodes {
		if node.Number == currentPR || !node.Merged {
			continue
		}
		return supersessionCloser{Repo: displayRepo, Number: node.Number, URL: node.URL}, true
	}
	return supersessionCloser{}, false
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

func (c *Client) prFilesSubset(ctx context.Context, owner, repo string, openPR int, closer supersessionCloser) (bool, error) {
	openFiles, err := c.prFileSet(ctx, owner, repo, openPR)
	if err != nil {
		return false, err
	}
	closerOwner, closerRepoName := c.splitRepo(closer.Repo)
	closerFiles, err := c.prFileSet(ctx, closerOwner, closerRepoName, closer.Number)
	if err != nil {
		return false, err
	}
	for file := range openFiles {
		if !closerFiles[file] {
			return false, nil
		}
	}
	return true, nil
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

func renderSupersessionComment(event SupersessionSweepEvent, closer supersessionCloser, reason string) string {
	var b strings.Builder
	fmt.Fprintln(&b, supersessionSweepMarker)
	fmt.Fprintln(&b, "Supersession sweep found that this PR's claimed issue is already closed by another merged PR.")
	fmt.Fprintln(&b)
	fmt.Fprintf(&b, "- Claimed issue: %s#%d\n", event.IssueRepo, event.Issue)
	if closer.URL != "" {
		fmt.Fprintf(&b, "- Closing PR: [#%d](%s)\n", closer.Number, closer.URL)
	} else {
		fmt.Fprintf(&b, "- Closing PR: %s#%d\n", closer.Repo, closer.Number)
	}
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, reason)
	fmt.Fprintln(&b)
	fmt.Fprintln(&b, "_This comment is edited in place by the supersession sweep; it is not duplicated._")
	return b.String()
}

func (c *Client) ensureSupersessionComment(ctx context.Context, owner, repo string, number int, desiredBody string) error {
	comments, _, err := c.client.Issues.ListComments(ctx, owner, repo, number, &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	})
	if err != nil {
		return fmt.Errorf("listing supersession comments on %s/%s#%d: %w", owner, repo, number, err)
	}
	for _, cm := range comments {
		if cm == nil || !strings.Contains(cm.GetBody(), supersessionSweepMarker) {
			continue
		}
		if cm.GetBody() == desiredBody {
			return nil
		}
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: &desiredBody}); err != nil {
			return fmt.Errorf("editing supersession comment on %s/%s#%d: %w", owner, repo, number, err)
		}
		return nil
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: &desiredBody}); err != nil {
		return fmt.Errorf("creating supersession comment on %s/%s#%d: %w", owner, repo, number, err)
	}
	return nil
}
