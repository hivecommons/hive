package automerge

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
)

const (
	defaultSelfAuthorizationReleaseLimit = 10
	selfAuthorizationReleaseComment      = "hold released: self-authorization hold disabled by config (#5117)"
)

func (c *Engine) selfAuthorizationReleaseBudget() int {
	if c == nil || c.selfAuthorizationHoldEnabled == nil {
		return 0
	}
	if c.selfAuthorizationHoldReleaseLimit > 0 {
		return c.selfAuthorizationHoldReleaseLimit
	}
	return defaultSelfAuthorizationReleaseLimit
}

func (c *Engine) selfAuthorizationHoldActive(repo string) bool {
	if c == nil || c.selfAuthorizationHoldEnabled == nil {
		return true
	}
	return c.selfAuthorizationHoldEnabled(repo)
}

func (c *Engine) releaseSelfAuthorizationHoldIfEligible(ctx context.Context, displayRepo, owner, repo string, number int) (bool, error) {
	if c == nil || c.gh == nil || c.transport == nil {
		return false, nil
	}
	noticeAt, ok, err := c.latestSelfAuthorizationNoticeTime(ctx, owner, repo, number)
	if err != nil {
		return false, err
	}
	if !ok {
		return false, nil
	}
	currentSelfAuthHold, err := c.currentHoldWasAppliedBySelfAuthorization(ctx, owner, repo, number, noticeAt)
	if err != nil {
		return false, err
	}
	if !currentSelfAuthHold {
		return false, nil
	}
	if _, err := c.gh.Issues.RemoveLabelForIssue(ctx, owner, repo, number, url.PathEscape("hold")); err != nil && !isGitHubStatus(err, http.StatusNotFound) {
		return false, fmt.Errorf("removing hold label: %w", err)
	}
	comment := &gh.IssueComment{Body: gh.Ptr(selfAuthorizationReleaseComment)}
	if _, _, err := c.gh.Issues.CreateComment(ctx, owner, repo, number, comment); err != nil {
		return false, fmt.Errorf("posting release comment: %w", err)
	}
	c.info("self-authorization hold released because it is disabled by config", "repo", displayRepo, "pr", number)
	return true, nil
}

func (c *Engine) latestSelfAuthorizationNoticeTime(ctx context.Context, owner, repo string, number int) (time.Time, bool, error) {
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	var latest time.Time
	opts := &gh.IssueListCommentsOptions{
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for {
		comments, resp, err := c.gh.Issues.ListComments(ctx, owner, repo, number, opts)
		if err != nil {
			return time.Time{}, false, fmt.Errorf("listing comments: %w", err)
		}
		for _, comment := range comments {
			if comment == nil || !strings.EqualFold(hgithub.SafeGetLogin(comment.GetUser()), appBot) {
				continue
			}
			if hgithub.IsSelfAuthorizationHoldNotice(comment.GetBody()) {
				if at := comment.GetCreatedAt().Time; at.After(latest) {
					latest = at
				}
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if latest.IsZero() {
		return time.Time{}, false, nil
	}
	return latest, true, nil
}

func (c *Engine) currentHoldWasAppliedBySelfAuthorization(ctx context.Context, owner, repo string, number int, noticeAt time.Time) (bool, error) {
	appBot := strings.TrimSpace(c.transport.AppBotLogin())
	var latest *gh.IssueEvent
	opts := &gh.ListOptions{PerPage: 100}
	for {
		events, resp, err := c.gh.Issues.ListIssueEvents(ctx, owner, repo, number, opts)
		if err != nil {
			return false, fmt.Errorf("listing issue events: %w", err)
		}
		for _, event := range events {
			if event == nil || event.GetLabel().GetName() == "" || !strings.EqualFold(event.GetLabel().GetName(), "hold") {
				continue
			}
			if event.GetEvent() != "labeled" && event.GetEvent() != "unlabeled" {
				continue
			}
			if event.GetEvent() == "unlabeled" && event.GetCreatedAt().Time.After(noticeAt) {
				return false, nil
			}
			if latest == nil || event.GetCreatedAt().Time.After(latest.GetCreatedAt().Time) {
				latest = event
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.Page = resp.NextPage
	}
	if latest == nil || latest.GetEvent() != "labeled" {
		return false, nil
	}
	return strings.EqualFold(hgithub.SafeGetLogin(latest.GetActor()), appBot), nil
}
