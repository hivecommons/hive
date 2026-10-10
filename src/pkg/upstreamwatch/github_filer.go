package upstreamwatch

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// stateReasonNotPlanned is the GitHub state_reason of an issue closed as
// "not planned".
const stateReasonNotPlanned = "not_planned"

// stateReasonCompleted is the GitHub state_reason of an issue closed as
// completed.
const stateReasonCompleted = "completed"

// markerSearchPerPage bounds the marker search: a marker is unique per
// upstream item, so a handful of results is plenty.
const markerSearchPerPage = 10

// Rate-limit handling for the marker search: GitHub allows 30 searches a
// minute, so a backlog pass waits for the reset and retries instead of
// ending the pass. A wait longer than maxRateLimitWait is not worth holding
// the eval goroutine for and is returned as the error.
const (
	maxRateLimitWait     = 2 * time.Minute
	maxRateLimitAttempts = 5
)

// GitHubFiler implements Filer over the GitHub REST API against the fork. It
// opens issues only, never PRs.
type GitHubFiler struct {
	client *gh.Client
	owner  string
	repo   string
	// wait blocks for d or until ctx is done; tests replace it.
	wait func(ctx context.Context, d time.Duration) error
}

// NewGitHubFiler returns a Filer for the fork at owner/repo.
func NewGitHubFiler(client *gh.Client, owner, repo string) *GitHubFiler {
	return &GitHubFiler{client: client, owner: owner, repo: repo}
}

// FindMarker implements Filer. Search is fuzzy, so every hit is confirmed by
// checking its body actually carries the marker.
func (g *GitHubFiler) FindMarker(ctx context.Context, marker string) (Existing, bool, error) {
	text := strings.TrimSuffix(strings.TrimPrefix(marker, "<!-- "), " -->")
	query := fmt.Sprintf(`repo:%s/%s is:issue in:body %q`, g.owner, g.repo, text)
	var result *gh.IssuesSearchResult
	var err error
	for attempt := 1; ; attempt++ {
		result, _, err = g.client.Search.Issues(ctx, query, &gh.SearchOptions{
			ListOptions: gh.ListOptions{PerPage: markerSearchPerPage},
		})
		d, limited := rateLimitDelay(err)
		if !limited || attempt >= maxRateLimitAttempts || d > maxRateLimitWait {
			break
		}
		if werr := g.waitFor(ctx, d); werr != nil {
			err = werr
			break
		}
	}
	if err != nil {
		return Existing{}, false, fmt.Errorf("search %s/%s for %q: %w", g.owner, g.repo, text, err)
	}
	for _, issue := range result.Issues {
		if issue.IsPullRequest() || !strings.Contains(issue.GetBody(), marker) {
			continue
		}
		return Existing{Number: issue.GetNumber(), Dismissed: issueDismissed(issue)}, true, nil
	}
	return Existing{}, false, nil
}

// rateLimitDelay reports whether err is a GitHub rate-limit error and how
// long until the limit resets.
func rateLimitDelay(err error) (time.Duration, bool) {
	var rl *gh.RateLimitError
	if errors.As(err, &rl) {
		return max(time.Until(rl.Rate.Reset.Time), 0) + time.Second, true
	}
	var abuse *gh.AbuseRateLimitError
	if errors.As(err, &abuse) {
		if abuse.RetryAfter != nil {
			return *abuse.RetryAfter + time.Second, true
		}
		return time.Minute, true
	}
	return 0, false
}

func (g *GitHubFiler) waitFor(ctx context.Context, d time.Duration) error {
	if g.wait != nil {
		return g.wait(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// File implements Filer.
func (g *GitHubFiler) File(ctx context.Context, issue Issue) (int, error) {
	req := &gh.IssueRequest{Title: gh.Ptr(issue.Title), Body: gh.Ptr(issue.Body)}
	if len(issue.Labels) > 0 {
		labels := append([]string(nil), issue.Labels...)
		req.Labels = &labels
	}
	created, _, err := g.client.Issues.Create(ctx, g.owner, g.repo, req)
	if err != nil {
		return 0, fmt.Errorf("create issue on %s/%s: %w", g.owner, g.repo, err)
	}
	return created.GetNumber(), nil
}

// GetIssue implements Filer. It is the reconciliation pass's read of one
// previously filed fork issue's current state.
func (g *GitHubFiler) GetIssue(ctx context.Context, number int) (IssueOutcome, bool, error) {
	issue, resp, err := g.client.Issues.Get(ctx, g.owner, g.repo, number)
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusNotFound {
			return IssueOutcome{}, false, nil
		}
		return IssueOutcome{}, false, fmt.Errorf("get issue %s/%s#%d: %w", g.owner, g.repo, number, err)
	}
	if issue.GetState() != "closed" {
		return IssueOutcome{Open: true}, true, nil
	}
	out := IssueOutcome{}
	switch {
	case issueDismissed(issue):
		out.Dismissed = true
	case issue.GetStateReason() == stateReasonCompleted:
		out.Ported = true
	}
	return out, true, nil
}

// issueDismissed reports whether a fork issue was closed as "not planned" or
// carries DismissedLabel.
func issueDismissed(issue *gh.Issue) bool {
	if issue.GetState() == "closed" && issue.GetStateReason() == stateReasonNotPlanned {
		return true
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(l.GetName(), DismissedLabel) {
			return true
		}
	}
	return false
}
