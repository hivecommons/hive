package github

import (
	"context"
	"encoding/json"
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
> Hive will automatically remove the `+"`hold`"+` label once current policy no longer requires a level hold for %q. If this is an outreach PR, a human must review it and remove the label.`,
		levelHoldMarker(agent), agent, agent)
}

func levelHoldMarker(agent string) string {
	body, _ := json.Marshal(levelHoldNoticeMetadata{Agent: strings.TrimSpace(agent)})
	return fmt.Sprintf("%s%s -->", levelHoldNoticePrefix, body)
}

func levelHoldAgentFromNotice(body string) (string, bool) {
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
			return agent, true
		}
	}
	return "", false
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
	if c == nil || pr == nil || !hasLabel(extractPRLabels(pr.Labels), "hold") {
		return false, "", nil
	}
	number := pr.GetNumber()
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return false, "level-hold-comment-check", err
	}
	agent := ""
	for _, comment := range comments {
		if !c.isTrustedLevelHoldNoticeAuthor(comment) {
			continue
		}
		if parsed, ok := levelHoldAgentFromNotice(comment.GetBody()); ok {
			agent = parsed
			break
		}
	}
	if agent == "" {
		return false, "hold", nil
	}
	if c.prHoldLabel == nil {
		return false, "level-hold-policy-unavailable", nil
	}
	if c.prHoldLabel(agent) {
		return false, "level-hold-still-required", nil
	}
	selfAuth := c.EvaluateSelfAuthorization(ctx, owner+"/"+repo, pr.GetTitle(), pr.GetBody(), nil)
	if selfAuth.Held {
		if !hasSelfAuthorizationNotice(comments, c.appBotLogin) {
			if _, _, err := c.client.Issues.CreateComment(ctx, owner, repo, number, &gh.IssueComment{Body: gh.Ptr(selfAuthorizationNotice(selfAuth))}); err != nil {
				return false, "self-authorization-notice", fmt.Errorf("commenting on self-authorization hold: %w", err)
			}
		}
		return false, "self-authorization-hold", nil
	}
	if ok, err := c.latestHoldLabelEventWasByApp(ctx, owner, repo, number); err != nil {
		return false, "level-hold-event-check", err
	} else if !ok {
		return false, "level-hold-not-app-labeled", nil
	}
	if _, err := c.client.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape("hold")); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return false, "level-hold-release", fmt.Errorf("removing level hold label: %w", err)
	}
	c.info("self-authored automerge sweep released level-applied hold", "repo", owner+"/"+repo, "pr", number, "agent", agent)
	return true, "level-hold-released", nil
}

func (c *Client) isTrustedLevelHoldNoticeAuthor(comment *gh.IssueComment) bool {
	if c == nil || comment == nil || strings.TrimSpace(c.appBotLogin) == "" {
		return false
	}
	return strings.EqualFold(safeGetLogin(comment.GetUser()), c.appBotLogin)
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
	if c == nil || strings.TrimSpace(c.appBotLogin) == "" {
		return false, nil
	}
	opts := &gh.ListOptions{PerPage: 100}
	latestEvent := ""
	latestActor := ""
	latestAtSet := false
	var latestAt gh.Timestamp
	for {
		events, resp, err := c.client.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing issue events for %s/%s#%d: %w", owner, repo, number, err)
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
	return latestEvent == "labeled" && strings.EqualFold(latestActor, c.appBotLogin), nil
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
