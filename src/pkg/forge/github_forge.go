package forge

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// githubReader is the subset of *github.Client the GitHub adapter needs for the
// read path. Depending on an interface (rather than the concrete client) lets
// tests inject a stub without real network calls, and keeps this adapter a thin
// delegator over the existing client — no logic is duplicated here.
type githubReader interface {
	GetRepo(ctx context.Context, owner, repo string) (*gh.Repository, *gh.Response, error)
	EnumerateActionable(ctx context.Context) (*github.ActionableResult, error)
}

// githubWriter is the subset of go-github's *gh.IssuesService the write path
// delegates to. Both issue and pull-request comments/labels flow through the
// Issues endpoints on GitHub, so this one service covers all write ops. Using an
// interface keeps the adapter thin (it forwards straight to go-github, no logic)
// and lets tests inject a stub without a real HTTP client.
type githubWriter interface {
	CreateComment(ctx context.Context, owner, repo string, number int, comment *gh.IssueComment) (*gh.IssueComment, *gh.Response, error)
	AddLabelsToIssue(ctx context.Context, owner, repo string, number int, labels []string) ([]*gh.Label, *gh.Response, error)
	RemoveLabelForIssue(ctx context.Context, owner, repo string, number int, label string) (*gh.Response, error)
	Create(ctx context.Context, owner, repo string, issue *gh.IssueRequest) (*gh.Issue, *gh.Response, error)
	ListByRepo(ctx context.Context, owner, repo string, opts *gh.IssueListByRepoOptions) ([]*gh.Issue, *gh.Response, error)
}

// markerLookupMaxPages bounds FindIssueByMarker to the most recent pages of
// open issues, matching the bounded dedupe scan pkg/github.CreateIssue uses:
// publisher-filed issues are recent by construction and an unbounded scan of
// a busy repo would spend API budget on every reconciliation.
const markerLookupMaxPages = 3

// markerLookupPerPage is the page size of the bounded marker scan.
const markerLookupPerPage = 100

// gitHubForge adapts the existing pkg/github client to the Forge interface.
//
// It is deliberately thin: read operations delegate to the underlying client
// and translate its already-neutralized types (github.Issue/PullRequest) into
// the forge-neutral types. The write path is not implemented here — callers that
// need writes continue to use *github.Client directly until the Forge write
// path (see TODOs on the Forge interface) lands.
type gitHubForge struct {
	client githubReader
	writer githubWriter
	org    string
}

// newGitHubForge builds a GitHub adapter backed by a real pkg/github client.
func newGitHubForge(token string, opts Options) *gitHubForge {
	// A single-repo enumeration is not what EnumerateActionable does, so the
	// underlying client is constructed with no fixed repo list; per-repo reads
	// go through the go-github client via GetRepo. The org is used to resolve
	// bare repo names.
	client := github.NewClient(token, opts.Org, nil, slog.Default(), opts.BaseURL)
	// Write ops delegate to go-github's Issues service directly (the pkg/github
	// client's own hold/comment helpers are private and log-and-swallow errors,
	// which the Forge write path must surface). GoGitHub() exposes the same
	// underlying client those helpers use, so no logic is duplicated.
	return &gitHubForge{client: client, writer: client.GoGitHub().Issues, org: opts.Org}
}

// newGitHubForgeWithReader is a test seam: it builds the adapter over an
// arbitrary githubReader (e.g. a stub), bypassing real client construction. The
// same stub may also satisfy githubWriter, so it is used for both seams.
func newGitHubForgeWithReader(r githubReader, org string) *gitHubForge {
	f := &gitHubForge{client: r, org: org}
	if w, ok := r.(githubWriter); ok {
		f.writer = w
	}
	return f
}

func (f *gitHubForge) Kind() Kind { return KindGitHub }

func (f *gitHubForge) GetRepo(ctx context.Context, repo string) (*Repo, error) {
	owner, name := splitRepo(repo, f.org)
	r, _, err := f.client.GetRepo(ctx, owner, name)
	if err != nil {
		return nil, fmt.Errorf("github: get repo %s/%s: %w", owner, name, err)
	}
	if r == nil {
		return nil, fmt.Errorf("github: get repo %s/%s: empty response", owner, name)
	}
	return &Repo{
		FullName:      r.GetFullName(),
		Owner:         owner,
		Name:          name,
		URL:           r.GetHTMLURL(),
		DefaultBranch: r.GetDefaultBranch(),
		Description:   r.GetDescription(),
	}, nil
}

// ListOpenIssues delegates to EnumerateActionable and filters to the requested
// repo, reusing the existing client's issue-neutralization (author, labels,
// assignees, PR exclusion) rather than re-implementing it here.
func (f *gitHubForge) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	res, err := f.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: list issues for %s: %w", repo, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]Issue, 0, len(res.Issues.Items))
	for _, it := range res.Issues.Items {
		if repo != "" && it.Repo != repo {
			continue
		}
		out = append(out, Issue{
			Repo:      it.Repo,
			Number:    it.Number,
			Title:     it.Title,
			Author:    it.Author,
			Labels:    it.Labels,
			Assignees: it.Assignees,
			State:     "open",
			CreatedAt: it.CreatedAt,
			URL:       it.URL,
		})
	}
	return out, nil
}

// ListOpenChangeRequests delegates to EnumerateActionable and filters to the
// requested repo.
func (f *gitHubForge) ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error) {
	res, err := f.client.EnumerateActionable(ctx)
	if err != nil {
		return nil, fmt.Errorf("github: list change requests for %s: %w", repo, err)
	}
	if res == nil {
		return nil, nil
	}
	out := make([]ChangeRequest, 0, len(res.PRs.Items))
	for _, pr := range res.PRs.Items {
		if repo != "" && pr.Repo != repo {
			continue
		}
		out = append(out, ChangeRequest{
			Repo:      pr.Repo,
			Number:    pr.Number,
			Title:     pr.Title,
			Author:    pr.Author,
			Labels:    pr.Labels,
			Draft:     pr.Draft,
			State:     "open",
			CreatedAt: pr.CreatedAt,
			URL:       pr.URL,
			HeadSHA:   pr.HeadSHA,
		})
	}
	return out, nil
}

// --- Write path ---
//
// All four write ops delegate straight to go-github's Issues service. GitHub
// models pull-request comments and labels through the Issues endpoints, so a
// single service covers issues and change requests alike.

// CreateIssueComment posts a comment on an issue or pull request.
func (f *gitHubForge) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	owner, name := splitRepo(repo, f.org)
	body = logscrub.ScrubString(body)
	_, _, err := f.writer.CreateComment(ctx, owner, name, number, &gh.IssueComment{Body: gh.Ptr(body)})
	if err != nil {
		return fmt.Errorf("github: comment on %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// AddLabels adds labels to an issue or pull request. Empty label sets are a
// no-op so callers need not guard the call site.
func (f *gitHubForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	owner, name := splitRepo(repo, f.org)
	_, _, err := f.writer.AddLabelsToIssue(ctx, owner, name, number, labels)
	if err != nil {
		return fmt.Errorf("github: add labels to %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// RemoveLabel removes a single label from an issue or pull request.
func (f *gitHubForge) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	owner, name := splitRepo(repo, f.org)
	_, err := f.writer.RemoveLabelForIssue(ctx, owner, name, number, label)
	if err != nil {
		return fmt.Errorf("github: remove label from %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// SetHold applies or clears the hold gate label via AddLabels/RemoveLabel.
func (f *gitHubForge) SetHold(ctx context.Context, repo string, number int, hold bool) error {
	if hold {
		return f.AddLabels(ctx, repo, number, []string{holdLabel})
	}
	return f.RemoveLabel(ctx, repo, number, holdLabel)
}

// NewGitHubIssueSeam exposes the issue-create and marker-lookup seam over an
// already-authenticated pkg/github client (the App-authenticated client a
// GitHub hive boots with), so the publisher files as the hive's App identity
// without a second token. Writes go straight to go-github's Issues service:
// the publisher's own mutation journal is the idempotency guard, and routing
// through *github.Client.CreateIssue would journal the same effect twice
// under two different logical IDs.
func NewGitHubIssueSeam(client *github.Client, org string) IssueSeam {
	if client == nil || client.GoGitHub() == nil {
		return nil
	}
	return &gitHubForge{client: client, writer: client.GoGitHub().Issues, org: org}
}

// CreateIssue files a new issue via go-github Issues.Create.
func (f *gitHubForge) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (IssueRef, error) {
	if f.writer == nil {
		return IssueRef{}, fmt.Errorf("forge: github write path unavailable")
	}
	owner, name := splitRepo(repo, f.org)
	req := &gh.IssueRequest{Title: gh.Ptr(logscrub.ScrubString(title)), Body: gh.Ptr(logscrub.ScrubString(body))}
	if len(labels) > 0 {
		req.Labels = &labels
	}
	issue, _, err := f.writer.Create(ctx, owner, name, req)
	if err != nil {
		return IssueRef{}, fmt.Errorf("forge: create issue in %s/%s: %w", owner, name, err)
	}
	return IssueRef{Number: issue.GetNumber(), URL: issue.GetHTMLURL()}, nil
}

// FindIssueByMarker scans the most recent open issues for one whose body
// carries the exact marker text.
func (f *gitHubForge) FindIssueByMarker(ctx context.Context, repo, marker string) (IssueRef, bool, error) {
	if f.writer == nil {
		return IssueRef{}, false, fmt.Errorf("forge: github read path unavailable")
	}
	if marker == "" {
		return IssueRef{}, false, fmt.Errorf("forge: a marker lookup requires a marker")
	}
	owner, name := splitRepo(repo, f.org)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: markerLookupPerPage},
	}
	for page := 1; page <= markerLookupMaxPages; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := f.writer.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return IssueRef{}, false, fmt.Errorf("forge: list issues in %s/%s: %w", owner, name, err)
		}
		for _, is := range issues {
			if is.IsPullRequest() {
				continue
			}
			if strings.Contains(is.GetBody(), marker) {
				return IssueRef{Number: is.GetNumber(), URL: is.GetHTMLURL()}, true, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return IssueRef{}, false, nil
}

// Compile-time assertion that the adapter satisfies the interface.
var _ Forge = (*gitHubForge)(nil)
var _ IssueSeam = (*gitHubForge)(nil)
