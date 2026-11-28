package forge

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// ReadAPI is the read surface an agent's `hive-gh pr view` / `issue view` /
// `pr checks` is served from on a Forgejo repo (hivecommons/hive#20230). Only
// the Forgejo adapter implements it.
type ReadAPI interface {
	// PullView reads one pull request with the fields an agent works from.
	PullView(ctx context.Context, repo string, number int) (PullView, error)
	// IssueView reads one issue (or the issue side of a pull request).
	IssueView(ctx context.Context, repo string, number int) (IssueView, error)
	// CommitStatuses returns the combined commit status state at sha and
	// each status it combines.
	CommitStatuses(ctx context.Context, repo, sha string) (state string, statuses []CommitStatus, err error)
}

// PullView is one pull request as an agent reads it.
type PullView struct {
	Number    int
	Title     string
	Body      string
	State     string
	Draft     bool
	Merged    bool
	Mergeable bool
	Author    string
	Labels    []string
	HeadRef   string
	HeadSHA   string
	Base      string
	URL       string
	UpdatedAt time.Time
}

// IssueView is one issue as an agent reads it.
type IssueView struct {
	Number    int
	Title     string
	Body      string
	State     string
	Author    string
	Labels    []string
	Assignees []string
	URL       string
	IsPull    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CommitStatus is one commit status (a CI check) on a commit.
type CommitStatus struct {
	Context     string
	State       string
	Description string
	TargetURL   string
}

var _ ReadAPI = (*forgejoForge)(nil)

func (f *forgejoForge) PullView(ctx context.Context, repo string, number int) (PullView, error) {
	var pr struct {
		fjPull
		HTMLURL   string    `json:"html_url"`
		UpdatedAt time.Time `json:"updated_at"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf("%s/pulls/%d", f.repoPath(repo), number), nil, &pr); err != nil {
		return PullView{}, fmt.Errorf("%s: read pull request %s#%d: %w", f.label, repo, number, err)
	}
	return PullView{
		Number:    pr.Number,
		Title:     pr.Title,
		Body:      pr.Body,
		State:     pr.State,
		Draft:     pr.Draft,
		Merged:    pr.Merged,
		Mergeable: pr.Mergeable,
		Author:    pr.User.Login,
		Labels:    labelNames(pr.Labels),
		HeadRef:   pr.Head.Ref,
		HeadSHA:   pr.Head.SHA,
		Base:      pr.Base.Ref,
		URL:       pr.HTMLURL,
		UpdatedAt: pr.UpdatedAt,
	}, nil
}

func (f *forgejoForge) IssueView(ctx context.Context, repo string, number int) (IssueView, error) {
	var is gtIssue
	if err := f.getJSON(ctx, fmt.Sprintf("%s/issues/%d", f.repoPath(repo), number), nil, &is); err != nil {
		return IssueView{}, fmt.Errorf("%s: read issue %s#%d: %w", f.label, repo, number, err)
	}
	return IssueView{
		Number:    is.Number,
		Title:     is.Title,
		Body:      is.Body,
		State:     is.State,
		Author:    is.User.Login,
		Labels:    labelNames(is.Labels),
		Assignees: loginNames(is.Assignees),
		URL:       is.HTMLURL,
		IsPull:    is.PullRequest != nil,
		CreatedAt: is.CreatedAt,
		UpdatedAt: is.UpdatedAt,
	}, nil
}

func (f *forgejoForge) CommitStatuses(ctx context.Context, repo, sha string) (string, []CommitStatus, error) {
	if strings.TrimSpace(sha) == "" {
		return "", nil, fmt.Errorf("%s: commit statuses: sha is required", f.label)
	}
	var out struct {
		State    string `json:"state"`
		Statuses []struct {
			Context     string `json:"context"`
			Status      string `json:"status"`
			Description string `json:"description"`
			TargetURL   string `json:"target_url"`
		} `json:"statuses"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf("%s/commits/%s/status", f.repoPath(repo), url.PathEscape(sha)), nil, &out); err != nil {
		return "", nil, fmt.Errorf("%s: commit statuses %s@%s: %w", f.label, repo, sha, err)
	}
	statuses := make([]CommitStatus, 0, len(out.Statuses))
	for _, s := range out.Statuses {
		statuses = append(statuses, CommitStatus{Context: s.Context, State: s.Status, Description: s.Description, TargetURL: s.TargetURL})
	}
	return out.State, statuses, nil
}
