package github

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/remoteitems"
)

// RemoteItemsFetcher reads the hive's own items in a remote repo under the
// hive owner's token (hivecommons/hive#18199): the token source is consulted
// on every call so a connect or disconnect takes effect at once, and a
// missing token is an error rather than an unauthenticated read (a public
// listing would work, but the poller must never run on a disconnected
// identity: there is nothing to attribute to).
//
// TokenFor, when set, picks the token per repository instead
// (hivecommons/hive#18516): a repo that writes as the App bot is read under
// that repo's own minted bot token, so a hive whose owner never connected an
// account still follows up on what its bot opened. When set it is the only
// source; Token serves callers that do not set it.
type RemoteItemsFetcher struct {
	Token    OwnerTokenSource
	TokenFor func(repo string) (string, bool)
	APIURL   string
}

// remoteItemsPageSize is GitHub's largest page.
const remoteItemsPageSize = 100

// remoteItemsMaxPages bounds one listing: the hive opens a handful of items
// in a remote repo, not hundreds.
const remoteItemsMaxPages = 3

var _ remoteitems.Fetcher = (*RemoteItemsFetcher)(nil)

func (f *RemoteItemsFetcher) client(repo string) (*gh.Client, error) {
	if f == nil {
		return nil, fmt.Errorf("remote items: no token source")
	}
	if f.TokenFor != nil {
		// Authoritative when set: a repo that writes as the bot is never
		// read under the owner's token instead (bot_only never uses it).
		tok, ok := f.TokenFor(repo)
		if !ok || tok == "" {
			return nil, fmt.Errorf("remote items: no token for %s", repo)
		}
		return newTokenClient(tok, f.APIURL), nil
	}
	if f.Token == nil {
		return nil, fmt.Errorf("remote items: no token for %s", repo)
	}
	tok, ok := f.Token()
	if !ok {
		return nil, fmt.Errorf("remote items: no usable token for %s", repo)
	}
	return newTokenClient(tok, f.APIURL), nil
}

func splitSlug(repo string) (owner, name string, err error) {
	owner, name, ok := strings.Cut(strings.TrimSpace(repo), "/")
	if !ok || owner == "" || name == "" {
		return "", "", fmt.Errorf("remote items: %q is not owner/name", repo)
	}
	return owner, name, nil
}

// ListOwnOpenItems lists the open issues and PRs in repo created by login.
// GitHub's issues listing returns both; a PR is told apart by its
// pull_request field.
func (f *RemoteItemsFetcher) ListOwnOpenItems(ctx context.Context, repo, login string) ([]remoteitems.RemoteItem, error) {
	client, err := f.client(repo)
	if err != nil {
		return nil, err
	}
	owner, name, err := splitSlug(repo)
	if err != nil {
		return nil, err
	}
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Creator:     login,
		Sort:        "created",
		Direction:   "asc",
		ListOptions: gh.ListOptions{PerPage: remoteItemsPageSize},
	}
	var out []remoteitems.RemoteItem
	for page := 0; page < remoteItemsMaxPages; page++ {
		issues, resp, err := client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %s items by %s: %w", repo, login, err)
		}
		for _, is := range issues {
			if is == nil {
				continue
			}
			it := remoteitems.RemoteItem{
				Repo:   owner + "/" + name,
				Number: is.GetNumber(),
				IsPR:   is.IsPullRequest(),
				Title:  is.GetTitle(),
				Body:   is.GetBody(),
				URL:    is.GetHTMLURL(),
				Author: is.GetUser().GetLogin(),
			}
			if is.CreatedAt != nil {
				it.CreatedAt = is.CreatedAt.Time
			}
			out = append(out, it)
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// CommentsSince lists the issue comments on repo#number created after since
// and, for a PR, its review comments and reviews too, oldest first.
func (f *RemoteItemsFetcher) CommentsSince(ctx context.Context, repo string, number int, isPR bool, since time.Time) ([]remoteitems.Comment, error) {
	client, err := f.client(repo)
	if err != nil {
		return nil, err
	}
	owner, name, err := splitSlug(repo)
	if err != nil {
		return nil, err
	}
	var out []remoteitems.Comment
	iopts := &gh.IssueListCommentsOptions{Sort: gh.Ptr("created"), Direction: gh.Ptr("asc"), ListOptions: gh.ListOptions{PerPage: remoteItemsPageSize}}
	if !since.IsZero() {
		iopts.Since = gh.Ptr(since)
	}
	comments, _, err := client.Issues.ListComments(ctx, owner, name, number, iopts)
	if err != nil {
		return nil, fmt.Errorf("listing %s#%d comments: %w", repo, number, err)
	}
	for _, c := range comments {
		if c == nil || c.CreatedAt == nil {
			continue
		}
		out = append(out, remoteitems.Comment{Kind: "comment", Author: c.GetUser().GetLogin(), Body: c.GetBody(), URL: c.GetHTMLURL(), CreatedAt: c.CreatedAt.Time})
	}
	if isPR {
		ropts := &gh.PullRequestListCommentsOptions{Sort: "created", Direction: "asc", ListOptions: gh.ListOptions{PerPage: remoteItemsPageSize}}
		if !since.IsZero() {
			ropts.Since = since
		}
		rcs, _, err := client.PullRequests.ListComments(ctx, owner, name, number, ropts)
		if err != nil {
			return nil, fmt.Errorf("listing %s#%d review comments: %w", repo, number, err)
		}
		for _, c := range rcs {
			if c == nil || c.CreatedAt == nil {
				continue
			}
			out = append(out, remoteitems.Comment{Kind: "review_comment", Author: c.GetUser().GetLogin(), Body: c.GetBody(), URL: c.GetHTMLURL(), CreatedAt: c.CreatedAt.Time})
		}
		reviews, _, err := client.PullRequests.ListReviews(ctx, owner, name, number, &gh.ListOptions{PerPage: remoteItemsPageSize})
		if err != nil {
			return nil, fmt.Errorf("listing %s#%d reviews: %w", repo, number, err)
		}
		for _, r := range reviews {
			if r == nil || r.SubmittedAt == nil || !r.SubmittedAt.Time.After(since) {
				continue
			}
			body := strings.TrimSpace(r.GetBody())
			state := strings.ToLower(r.GetState())
			if body == "" {
				if state == "commented" {
					continue // an inline-only review; its comments arrived above
				}
				body = "(" + state + ")"
			} else if state != "" {
				body = "[" + state + "] " + body
			}
			out = append(out, remoteitems.Comment{Kind: "review", Author: r.GetUser().GetLogin(), Body: body, URL: r.GetHTMLURL(), CreatedAt: r.SubmittedAt.Time})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}
