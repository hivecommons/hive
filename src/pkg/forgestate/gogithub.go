package forgestate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// GoGitHubFetcher is the production Fetcher. Pass it the hive's client
// (github.Client.GoGitHub()): that client's transport already carries the
// slow-start pacing after secondary rate limits and the shared ETag cache,
// so every list page is sent conditionally and a 304 is replayed for free
// against the primary quota.
type GoGitHubFetcher struct {
	client   *gh.Client
	perPage  int
	maxPages int
}

// ErrNoClient is returned by a GoGitHubFetcher with no go-github client.
var ErrNoClient = errors.New("forgestate: no github client")

// NewGoGitHubFetcher returns a Fetcher over c.
func NewGoGitHubFetcher(c *gh.Client) *GoGitHubFetcher {
	return &GoGitHubFetcher{client: c, perPage: 100, maxPages: 50}
}

func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("forgestate: repo %q is not owner/name", repo)
	}
	return owner, name, nil
}

func (f *GoGitHubFetcher) ready(repo string) (string, string, error) {
	if f == nil || f.client == nil {
		return "", "", ErrNoClient
	}
	return splitRepo(repo)
}

// List implements Fetcher. Conditional requests happen in the client's
// transport, so the etag argument is not re-sent here and NotModified is
// never reported; an incomplete pagination is an error, never a short list.
func (f *GoGitHubFetcher) List(ctx context.Context, repo, _ string) (ListResult, error) {
	owner, name, err := f.ready(repo)
	if err != nil {
		return ListResult{}, err
	}
	var out ListResult
	iopts := &gh.IssueListByRepoOptions{State: "open", ListOptions: gh.ListOptions{PerPage: f.perPage}}
	for page := 0; ; page++ {
		if page >= f.maxPages {
			return ListResult{}, fmt.Errorf("forgestate: %s issues exceed %d pages", repo, f.maxPages)
		}
		issues, resp, err := f.client.Issues.ListByRepo(ctx, owner, name, iopts)
		if err != nil {
			return ListResult{}, err
		}
		if page == 0 && resp != nil && resp.Response != nil {
			out.ETag = resp.Header.Get("ETag")
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() {
				continue
			}
			out.Issues = append(out.Issues, issueItem(is))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		iopts.ListOptions.Page = resp.NextPage
	}
	popts := &gh.PullRequestListOptions{State: "open", ListOptions: gh.ListOptions{PerPage: f.perPage}}
	for page := 0; ; page++ {
		if page >= f.maxPages {
			return ListResult{}, fmt.Errorf("forgestate: %s pull requests exceed %d pages", repo, f.maxPages)
		}
		prs, resp, err := f.client.PullRequests.List(ctx, owner, name, popts)
		if err != nil {
			return ListResult{}, err
		}
		for _, pr := range prs {
			if pr == nil {
				continue
			}
			out.PRs = append(out.PRs, prItem(pr))
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		popts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// Issue implements Fetcher.
func (f *GoGitHubFetcher) Issue(ctx context.Context, repo string, number int) (Item, error) {
	owner, name, err := f.ready(repo)
	if err != nil {
		return Item{}, err
	}
	is, resp, err := f.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return Item{}, notFound(resp, err)
	}
	return issueItem(is), nil
}

// PR implements Fetcher.
func (f *GoGitHubFetcher) PR(ctx context.Context, repo string, number int) (Item, error) {
	owner, name, err := f.ready(repo)
	if err != nil {
		return Item{}, err
	}
	pr, resp, err := f.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return Item{}, notFound(resp, err)
	}
	return prItem(pr), nil
}

// notFound wraps ErrNotFound around a 404/410 so the Store drops the item.
func notFound(resp *gh.Response, err error) error {
	if resp != nil && resp.Response != nil &&
		(resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone) {
		return fmt.Errorf("%w: %w", ErrNotFound, err)
	}
	return err
}

func issueItem(is *gh.Issue) Item {
	return Item{
		Kind:      KindIssue,
		Number:    is.GetNumber(),
		State:     is.GetState(),
		Labels:    labelNames(is.Labels),
		Assignees: logins(is.Assignees),
		UpdatedAt: is.GetUpdatedAt().Time,
		Draft:     is.GetDraft(),
		Closed:    strings.EqualFold(is.GetState(), "closed"),
	}
}

func prItem(pr *gh.PullRequest) Item {
	return Item{
		Kind:           KindPR,
		Number:         pr.GetNumber(),
		State:          pr.GetState(),
		Labels:         labelNames(pr.Labels),
		Assignees:      logins(pr.Assignees),
		UpdatedAt:      pr.GetUpdatedAt().Time,
		Draft:          pr.GetDraft(),
		Closed:         strings.EqualFold(pr.GetState(), "closed"),
		HeadSHA:        pr.GetHead().GetSHA(),
		Merged:         pr.GetMerged() || !pr.GetMergedAt().Time.IsZero(),
		MergeableState: pr.GetMergeableState(),
	}
}

func labelNames(ls []*gh.Label) []string {
	var out []string
	for _, l := range ls {
		if n := l.GetName(); n != "" {
			out = append(out, n)
		}
	}
	return out
}

func logins(us []*gh.User) []string {
	var out []string
	for _, u := range us {
		if l := u.GetLogin(); l != "" {
			out = append(out, l)
		}
	}
	return out
}
