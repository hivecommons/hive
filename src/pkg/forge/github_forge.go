package forge

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/crux/logscrub"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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

// githubIssueConsolidator is the consolidating create of *github.Client: it
// reuses an open issue with the same subject and folds a finding into an open
// twin over the same files instead of filing a second issue (#11578).
type githubIssueConsolidator interface {
	CreateIssue(ctx context.Context, req github.CreateIssueRequest) (github.CreateIssueResult, error)
}

// githubPullReader is the subset of go-github's *gh.PullRequestsService the
// merge path reads through. One PR fetch answers all three gate questions at
// once — already merged?, gate label?, still the expected head? — so the
// adapter spends a single extra call before a merge.
type githubPullReader interface {
	Get(ctx context.Context, owner, repo string, number int) (*gh.PullRequest, *gh.Response, error)
}

// githubMerger is the merge primitive of *github.Client. The adapter delegates
// to MergePR rather than calling the REST endpoint itself, so a forge merge
// inherits exactly what the hive's own merge relay has: the mutation boundary
// and its reconciliation, the forward-merge guard, and the merge audit record.
type githubMerger interface {
	MergePR(ctx context.Context, repo string, number int, mergeMethod, expectSHA string) (github.MergePRResult, error)
}

// githubRefDeleter is the subset of go-github's *gh.GitService the merge path
// uses to honour MergeOptions.DeleteBranch. GitHub has no "delete the source
// branch" flag on its merge call — unlike GitLab's
// should_remove_source_branch — so the adapter deletes the ref itself.
type githubRefDeleter interface {
	DeleteRef(ctx context.Context, owner, repo, ref string) (*gh.Response, error)
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
// the forge-neutral types; write and merge operations delegate to go-github's
// services and to *github.Client.MergePR. No forge logic is duplicated here.
type gitHubForge struct {
	client githubReader
	writer githubWriter
	pulls  githubPullReader
	merger githubMerger
	refs   githubRefDeleter
	org    string

	// consolidator, when set, routes CreateIssue through the consolidating
	// create so publisher filings fold into open twins like the agent lane.
	consolidator githubIssueConsolidator
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
	return &gitHubForge{
		client: client,
		writer: client.GoGitHub().Issues,
		pulls:  client.GoGitHub().PullRequests,
		merger: client,
		refs:   client.GoGitHub().Git,
		org:    opts.Org,
	}
}

// newGitHubForgeWithReader is a test seam: it builds the adapter over an
// arbitrary githubReader (e.g. a stub), bypassing real client construction. The
// same stub may also satisfy githubWriter, githubPullReader or githubMerger, so
// it is used for every seam it covers.
func newGitHubForgeWithReader(r githubReader, org string) *gitHubForge {
	f := &gitHubForge{client: r, org: org}
	if w, ok := r.(githubWriter); ok {
		f.writer = w
	}
	if p, ok := r.(githubPullReader); ok {
		f.pulls = p
	}
	if m, ok := r.(githubMerger); ok {
		f.merger = m
	}
	if d, ok := r.(githubRefDeleter); ok {
		f.refs = d
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
// without a second token. Creates go through
// *github.Client.CreateIssue with ConsolidateOpen, the same consolidating create the
// agent lane uses, so a convergence finding folds into an open issue over the
// same files instead of twinning it under a different marker (#11578); the
// publisher's mutation journal and marker lookup remain its own idempotency
// guards.
func NewGitHubIssueSeam(client *github.Client, org string) IssueSeam {
	if client == nil || client.GoGitHub() == nil {
		return nil
	}
	return &gitHubForge{client: client, writer: client.GoGitHub().Issues, org: org, consolidator: client}
}

// CreateIssue files a new issue: through the consolidating create when the
// seam was built over a *github.Client, else via go-github Issues.Create.
// Every issue carries exactly one origin/<source> provenance label: the one
// the caller passed in labels, else origin/forge.
func (f *gitHubForge) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (IssueRef, error) {
	owner, name := splitRepo(repo, f.org)
	labels = config.WithOriginLabel(labels, config.OriginSourceForge)
	if f.consolidator != nil {
		res, err := f.consolidator.CreateIssue(ctx, github.CreateIssueRequest{Repo: owner + "/" + name, Title: title, Body: body, Labels: labels, Origin: config.OriginSourceForge, ConsolidateOpen: true})
		if err != nil {
			return IssueRef{}, fmt.Errorf("forge: create issue in %s/%s: %w", owner, name, err)
		}
		return IssueRef{Number: res.Number, URL: res.URL}, nil
	}
	if f.writer == nil {
		return IssueRef{}, fmt.Errorf("forge: github write path unavailable")
	}
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

// gitHubMergeMethods maps the forge-neutral strategies onto GitHub's
// merge_method. It is also the membership test: a strategy absent from this
// map is refused, never defaulted into a different history shape. The empty
// strategy leaves the method to pkg/github.MergePR, whose default is squash.
var gitHubMergeMethods = map[string]string{
	"":                          "",
	string(MergeStrategySquash): "squash",
	string(MergeStrategyMerge):  "merge",
	string(MergeStrategyRebase): "rebase",
}

// Merge integrates a pull request under the shared Merger contract
// (merge_contract_test.go): an already-merged change reports ErrAlreadyMerged,
// the hold gate refuses with ErrHoldActive and a head that is not the commit
// the caller pinned refuses with ErrConflict — both WITHOUT calling the merge
// endpoint — and GitHub's own refusals map onto the same sentinels every
// adapter reports.
//
// The merge itself delegates to *github.Client.MergePR rather than calling the
// REST endpoint here, so a forge merge inherits what the hive's merge relay
// has: the mutation boundary and its reconciliation, the forward-merge guard,
// and the merge audit record.
func (f *gitHubForge) Merge(ctx context.Context, repo string, number int, opts MergeOptions) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("github: merge: repo is required")
	}
	if number <= 0 {
		return fmt.Errorf("github: merge: change number must be > 0 (got %d)", number)
	}
	method, ok := gitHubMergeMethods[strings.ToLower(strings.TrimSpace(opts.Strategy))]
	if !ok {
		return fmt.Errorf("github: unsupported merge strategy %q", opts.Strategy)
	}
	if f.pulls == nil || f.merger == nil {
		return fmt.Errorf("github: merge %s#%d: adapter was built without a merge client", repo, number)
	}
	owner, name := splitRepo(repo, f.org)
	slug := owner + "/" + name

	// One read answers all three gates at once: already merged?, held?, still
	// the commit the caller decided on?
	pr, _, err := f.pulls.Get(ctx, owner, name, number)
	if err != nil {
		return fmt.Errorf("github: check pull request %s#%d: %w", slug, number, err)
	}
	if pr.GetMerged() {
		return fmt.Errorf("%w: %s#%d", ErrAlreadyMerged, slug, number)
	}
	for _, label := range pr.Labels {
		if strings.EqualFold(strings.TrimSpace(label.GetName()), holdLabel) {
			return fmt.Errorf("%w: %s#%d carries %q", ErrHoldActive, slug, number, label.GetName())
		}
	}
	expect := strings.TrimSpace(opts.HeadSHA)
	if have := pr.GetHead().GetSHA(); expect != "" && have != "" && !strings.EqualFold(have, expect) {
		return fmt.Errorf("%w: %s#%d head is %s, caller decided on %s", ErrConflict, slug, number, have, expect)
	}

	// expect is passed through so GitHub enforces the pin server-side too: a
	// push landing between the read above and the merge is refused by the
	// forge (409) instead of merging work nobody reviewed.
	if _, err := f.merger.MergePR(ctx, slug, number, method, expect); err != nil {
		return gitHubMergeRefusal(slug, number, err)
	}
	if opts.DeleteBranch {
		if err := f.deleteHeadBranch(ctx, owner, name, pr.GetHead().GetRef()); err != nil {
			return fmt.Errorf("github: merged %s#%d but deleting its head branch: %w", slug, number, err)
		}
	}
	return nil
}

// gitHubMergeRefusal maps GitHub's merge refusals onto the forge-neutral
// sentinels: 405 is "not in a mergeable state" (conflicts, draft, required
// checks), 422 is a merge GitHub will not perform for this change, and 409 is
// the head having moved past the SHA the caller pinned.
func gitHubMergeRefusal(slug string, number int, err error) error {
	var ghErr *gh.ErrorResponse
	if errors.As(err, &ghErr) && ghErr.Response != nil {
		switch ghErr.Response.StatusCode {
		case http.StatusMethodNotAllowed, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s#%d: %w", ErrNotMergeable, slug, number, err)
		case http.StatusConflict:
			return fmt.Errorf("%w: %s#%d: %w", ErrConflict, slug, number, err)
		}
	}
	return fmt.Errorf("github: merge %s#%d: %w", slug, number, err)
}

// deleteHeadBranch removes the merged change's source branch. A branch that is
// already gone — the repo's own auto-delete setting usually wins the race — is
// the desired end state, not a failure.
func (f *gitHubForge) deleteHeadBranch(ctx context.Context, owner, name, ref string) error {
	if f.refs == nil {
		return fmt.Errorf("adapter was built without a git ref client")
	}
	if ref = strings.TrimSpace(ref); ref == "" {
		return fmt.Errorf("the pull request reports no head branch")
	}
	resp, err := f.refs.DeleteRef(ctx, owner, name, "heads/"+ref)
	if err != nil {
		if resp != nil && (resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusUnprocessableEntity) {
			return nil
		}
		return err
	}
	return nil
}

// Compile-time assertion that the adapter satisfies the interface.
var _ Forge = (*gitHubForge)(nil)
var _ IssueSeam = (*gitHubForge)(nil)
var _ Merger = (*gitHubForge)(nil)
