package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

const levelHoldNoticePrefix = "<!-- hive:level-hold "

type levelHoldNoticeMetadata struct {
	Agent string `json:"agent"`
}

func levelHoldNotice(agent string) string {
	agent = strings.TrimSpace(agent)
	return fmt.Sprintf(`%s
> [!IMPORTANT]
> **Held for human review by the hive's ACMM level gate.**
>
> This PR was opened by the %q agent while Hive policy required a human checkpoint for that agent. Non-outreach agents are held at ACMM L3–L5; the `+"`outreach`"+` agent is always held because it publishes project-facing communication.
>
> Hive will keep the `+"`hold`"+` label until a human removes it. Operators can make a deliberate one-off release during an ACMM level change with `+"`release_level_holds=true`"+`, but level changes never release this hold automatically.`,
		levelHoldMarker(agent), agent)
}

func levelHoldMarker(agent string) string {
	body, _ := json.Marshal(levelHoldNoticeMetadata{Agent: strings.TrimSpace(agent)})
	return fmt.Sprintf("%s%s -->", levelHoldNoticePrefix, body)
}

func levelHoldAgentFromNotice(body string) (string, bool) {
	meta, ok := levelHoldMetadataFromNotice(body)
	return meta.Agent, ok
}

func levelHoldMetadataFromNotice(body string) (levelHoldNoticeMetadata, bool) {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, levelHoldNoticePrefix) || !strings.HasSuffix(line, " -->") {
			continue
		}
		raw := strings.TrimSuffix(strings.TrimPrefix(line, levelHoldNoticePrefix), " -->")
		var meta levelHoldNoticeMetadata
		if err := json.Unmarshal([]byte(raw), &meta); err != nil {
			continue
		}
		if agent := strings.TrimSpace(meta.Agent); agent != "" {
			meta.Agent = agent
			return meta, true
		}
	}
	return levelHoldNoticeMetadata{}, false
}

func (c *Client) ensureLevelHoldNotice(ctx context.Context, repo string, number int, agent string) error {
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return err
	}
	for _, comment := range comments {
		if !c.isTrustedLevelHoldNoticeAuthor(comment) {
			continue
		}
		if _, ok := levelHoldAgentFromNotice(comment.GetBody()); ok {
			return nil
		}
	}
	if _, _, err := c.client.Issues.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(levelHoldNotice(agent))}); err != nil {
		return fmt.Errorf("commenting on level hold: %w", err)
	}
	return nil
}

func (c *Client) releaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error) {
	if c == nil || pr == nil || !HasHoldLabel(extractPRLabels(pr.Labels)) {
		return false, "", nil
	}
	return false, "hold", nil
}

func (c *Client) isTrustedLevelHoldNoticeAuthor(comment *gh.IssueComment) bool {
	return c.isTrustedAppBotCommentAuthor(comment)
}

// LevelHoldPR identifies an open PR carrying a Hive App level-hold notice.
type LevelHoldPR struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Title  string `json:"title,omitempty"`
	URL    string `json:"url,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// PendingLevelHolds lists open PRs across active repositories whose hold label
// is attributable to the ACMM level gate and whose latest hold-label event was
// the Hive App applying that hold. Human re-holds are not considered pending
// level holds; they are a human's to remove.
func (c *Client) PendingLevelHolds(ctx context.Context) ([]LevelHoldPR, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	var pending []LevelHoldPR
	for _, repoRef := range c.activeRepos() {
		owner, repo := c.splitRepo(repoRef)
		opts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: 100}}
		for {
			prs, resp, err := c.client.PullRequests.List(ctx, owner, repo, opts)
			if err != nil {
				return nil, fmt.Errorf("listing open pull requests for %s/%s: %w", owner, repo, err)
			}
			for _, pr := range prs {
				if pr == nil || !HasHoldLabel(extractPRLabels(pr.Labels)) {
					continue
				}
				meta, ok, comments, err := c.levelHoldMetadataAndCommentsForPR(ctx, owner, repo, pr.GetNumber())
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				blocked, err := c.levelHoldReleaseBlockedByOtherPolicy(ctx, owner, repo, pr, meta, comments)
				if err != nil {
					return nil, err
				}
				if blocked {
					continue
				}
				appHeld, err := c.latestHoldLabelEventWasByApp(ctx, owner, repo, pr.GetNumber())
				if err != nil {
					return nil, err
				}
				if appHeld {
					pending = append(pending, LevelHoldPR{Repo: owner + "/" + repo, Number: pr.GetNumber(), Title: pr.GetTitle(), URL: pr.GetHTMLURL(), Agent: meta.Agent})
				}
			}
			if resp.NextPage == 0 {
				break
			}
			opts.Page = resp.NextPage
		}
	}
	return pending, nil
}

// ReleaseLevelHoldsOnce removes the literal hold label from the level-held PRs
// currently pending across active repositories and comments with the operator
// and target level that authorized this deliberate one-off release.
func (c *Client) ReleaseLevelHoldsOnce(ctx context.Context, level int, actor string) ([]LevelHoldPR, error) {
	if c == nil {
		return nil, ErrNoGitHubClient
	}
	pending, err := c.PendingLevelHolds(ctx)
	if err != nil {
		return nil, err
	}
	actor = strings.TrimSpace(actor)
	if actor == "" {
		actor = "unknown operator"
	}
	for _, pr := range pending {
		owner, repo := c.splitRepo(pr.Repo)
		if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, pr.Number, url.PathEscape("hold")); err != nil && !githubStatusError(err, http.StatusNotFound) {
			return pending, fmt.Errorf("removing level hold label on %s#%d: %w", pr.Repo, pr.Number, err)
		}
		body := fmt.Sprintf("hold released by operator %s when raising to L%d", actor, level)
		if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, pr.Number, &gh.IssueComment{Body: gh.Ptr(body)}); err != nil {
			return pending, fmt.Errorf("commenting on level hold release for %s#%d: %w", pr.Repo, pr.Number, err)
		}
		if c.logger != nil {
			c.logger.Info("level-applied hold released by operator during ACMM level change", "repo", pr.Repo, "number", pr.Number, "agent", pr.Agent, "operator", actor, "level", level)
		}
	}
	return pending, nil
}

func (c *Client) levelHoldMetadataAndCommentsForPR(ctx context.Context, owner, repo string, number int) (levelHoldNoticeMetadata, bool, []*gh.IssueComment, error) {
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return levelHoldNoticeMetadata{}, false, nil, err
	}
	if hasReporterTrustNotice(comments, c.appBotLogin) || hasSelfAuthorizationNotice(comments, c.appBotLogin) {
		return levelHoldNoticeMetadata{}, false, comments, nil
	}
	for _, comment := range comments {
		if !c.isTrustedLevelHoldNoticeAuthor(comment) {
			continue
		}
		if meta, ok := levelHoldMetadataFromNotice(comment.GetBody()); ok {
			return meta, true, comments, nil
		}
	}
	return levelHoldNoticeMetadata{}, false, comments, nil
}

func (c *Client) levelHoldReleaseBlockedByOtherPolicy(ctx context.Context, owner, repo string, pr *gh.PullRequest, _ levelHoldNoticeMetadata, comments []*gh.IssueComment) (bool, error) {
	if c == nil || pr == nil {
		return true, nil
	}
	number := pr.GetNumber()
	if comments == nil {
		var err error
		comments, err = c.listIssueComments(ctx, owner, repo, number)
		if err != nil {
			return true, err
		}
	}
	repoRef := owner + "/" + repo
	if hasReporterTrustNotice(comments, c.appBotLogin) {
		return true, nil
	}
	if c.reporterTrustHoldActive(repoRef) {
		reporter := c.EvaluateReporterTrust(ctx, repoRef, pr.GetTitle(), pr.GetBody(), nil)
		if reporter.Held {
			if err := c.AddLabels(ctx, repoRef, number, []string{issueNeedsHumanLabel}); err != nil {
				return true, fmt.Errorf("applying needs-human for reporter-trust hold on %s#%d: %w", repoRef, number, err)
			}
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(reporterTrustNotice(reporter))}); err != nil {
				return true, fmt.Errorf("commenting on reporter-trust hold for %s#%d: %w", repoRef, number, err)
			}
			return true, nil
		}
	}
	if hasSelfAuthorizationNotice(comments, c.appBotLogin) {
		return true, nil
	}
	if c.selfAuthorizationHoldActive(repoRef) {
		selfAuth := c.EvaluateSelfAuthorization(ctx, repoRef, pr.GetTitle(), pr.GetBody(), nil)
		if selfAuth.Held {
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(selfAuthorizationNotice(selfAuth))}); err != nil {
				return true, fmt.Errorf("commenting on self-authorization hold for %s#%d: %w", repoRef, number, err)
			}
			return true, nil
		}
	}
	return false, nil
}

func hasSelfAuthorizationNotice(comments []*gh.IssueComment, appBotLogin string) bool {
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		if strings.TrimSpace(appBotLogin) != "" && !strings.EqualFold(safeGetLogin(comment.GetUser()), appBotLogin) {
			continue
		}
		if strings.Contains(comment.GetBody(), "hivecommons/hive#5117") {
			return true
		}
	}
	return false
}

func (c *Client) latestHoldLabelEventWasByApp(ctx context.Context, owner, repo string, number int) (bool, error) {
	ok, _, err := c.latestHoldLabelEventByApp(ctx, owner, repo, number)
	return ok, err
}

// latestHoldLabelEventByApp reports whether the newest `hold` label event on
// the issue or PR is a `labeled` by the App bot, and when that event happened
// so a caller can pair it with the notice the App posted alongside it. A
// blank bot login fails closed.
func (c *Client) latestHoldLabelEventByApp(ctx context.Context, owner, repo string, number int) (bool, gh.Timestamp, error) {
	if c == nil || strings.TrimSpace(c.appBotLogin) == "" {
		return false, gh.Timestamp{}, nil
	}
	opts := &gh.ListOptions{PerPage: 100}
	latestEvent := ""
	latestActor := ""
	latestAtSet := false
	var latestAt gh.Timestamp
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return false, gh.Timestamp{}, fmt.Errorf("listing issue events for %s/%s#%d: %w", owner, repo, number, err)
		}
		for _, event := range events {
			if event == nil || !strings.EqualFold(event.GetLabel().GetName(), "hold") {
				continue
			}
			kind := event.GetEvent()
			if kind != "labeled" && kind != "unlabeled" {
				continue
			}
			if !latestAtSet || event.GetCreatedAt().After(latestAt.Time) {
				latestAtSet = true
				latestAt = event.GetCreatedAt()
				latestEvent = kind
				latestActor = safeGetLogin(event.GetActor())
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	return latestEvent == "labeled" && strings.EqualFold(latestActor, c.appBotLogin), latestAt, nil
}

func (c *Client) listIssueComments(ctx context.Context, owner, repo string, number int) ([]*gh.IssueComment, error) {
	opts := &gh.IssueListCommentsOptions{ListOptions: gh.ListOptions{PerPage: 100}}
	var all []*gh.IssueComment
	for {
		comments, resp, err := c.client.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return nil, fmt.Errorf("listing comments for %s/%s#%d: %w", owner, repo, number, err)
		}
		all = append(all, comments...)
		if resp.NextPage == 0 {
			return all, nil
		}
		opts.Page = resp.NextPage
	}
}

func (c *Client) ReleaseLevelHoldIfEligible(ctx context.Context, owner, repo string, pr *gh.PullRequest) (bool, string, error) {
	return c.releaseLevelHoldIfEligible(ctx, owner, repo, pr)
}

func githubStatusError(err error, status int) bool {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		return ghErr.Response.StatusCode == status
	}
	return false
}
