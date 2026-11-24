package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// Issue and change-request creation for the Gitea family (Gitea and Forgejo),
// hivecommons/hive#19670.
//
// Both APIs take label IDS on create (CreateIssueOption.labels and
// CreatePullRequestOption.labels are []int64), while the add-labels endpoint
// takes names. So an item is created bare and its labels are then added by
// name with AddLabels, the call the rest of the adapter already uses. A label
// failure after a successful create is reported as ErrLabelsNotApplied
// together with the created item's ref, so the caller can record the item and
// retry the labels instead of filing a second copy.

// ErrLabelsNotApplied is returned, wrapped, with a valid ref when an issue or
// change request was created but its labels could not be added.
var ErrLabelsNotApplied = errors.New("forge: created, but labels were not applied")

// ChangeRequestSpec describes a change request to open.
type ChangeRequestSpec struct {
	// Head is the source branch. It must exist on the target repo.
	Head string
	// Base is the target branch. Empty means the repo's default branch, read
	// from the forge. The create fails rather than guessing when it cannot be
	// read.
	Base   string
	Title  string
	Body   string
	Labels []string
}

// ChangeRequestRef is the result of a change-request create.
type ChangeRequestRef struct {
	Number int
	URL    string
	// AlreadyExisted is true when an open change request from Head already
	// existed and was returned instead of opening a second one.
	AlreadyExisted bool
}

// ChangeRequestCreator is the OPTIONAL forge-neutral seam for opening a change
// request. Only the Gitea family implements it today; the GitHub path opens
// pull requests through *github.Client, unchanged.
type ChangeRequestCreator interface {
	CreateChangeRequest(ctx context.Context, repo string, spec ChangeRequestSpec) (ChangeRequestRef, error)
}

// gtMarkerIssue is the subset of an issue the marker lookup reads.
type gtMarkerIssue struct {
	Number      int       `json:"number"`
	Body        string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	PullRequest *gtPRStub `json:"pull_request"`
}

// gtCreated is the subset of a created issue or pull request we read back.
type gtCreated struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// CreateIssue files an issue via POST /repos/{owner}/{repo}/issues and then
// adds its labels by name. Like the GitHub seam, every issue carries exactly
// one origin/<source> provenance label: the caller's, else origin/forge.
func (f *giteaForge) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (IssueRef, error) {
	owner, name := f.ownerRepo(repo)
	if strings.TrimSpace(title) == "" {
		return IssueRef{}, fmt.Errorf("%s: create issue in %s/%s: a title is required", f.label, owner, name)
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(name))
	var out gtCreated
	payload := map[string]string{"title": title, "body": body}
	if err := f.doWriteJSON(ctx, http.MethodPost, endpoint, payload, &out); err != nil {
		return IssueRef{}, fmt.Errorf("%s: create issue in %s/%s: %w", f.label, owner, name, err)
	}
	ref := IssueRef{Number: out.Number, URL: out.HTMLURL}
	labels = config.WithOriginLabel(labels, config.OriginSourceForge)
	if err := f.AddLabels(ctx, repo, ref.Number, labels); err != nil { // writeback:solicited labels of the issue the caller is creating
		return ref, fmt.Errorf("%w: issue %s/%s#%d: %v", ErrLabelsNotApplied, owner, name, ref.Number, err)
	}
	return ref, nil
}

// FindIssueByMarker scans the most recent open issues for one whose body
// carries the exact marker text, bounded like the GitHub seam.
func (f *giteaForge) FindIssueByMarker(ctx context.Context, repo, marker string) (IssueRef, bool, error) {
	if marker == "" {
		return IssueRef{}, false, fmt.Errorf("%s: a marker lookup requires a marker", f.label)
	}
	owner, name := f.ownerRepo(repo)
	endpoint := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(name))
	q := url.Values{}
	q.Set("state", giteaStateOpen)
	q.Set("type", "issues")
	q.Set("limit", strconv.Itoa(markerLookupPerPage))
	for page := 1; page <= markerLookupMaxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		var issues []gtMarkerIssue
		if err := f.getJSON(ctx, endpoint, q, &issues); err != nil {
			return IssueRef{}, false, fmt.Errorf("%s: list issues in %s/%s: %w", f.label, owner, name, err)
		}
		for _, is := range issues {
			if is.PullRequest != nil {
				continue
			}
			if strings.Contains(is.Body, marker) {
				return IssueRef{Number: is.Number, URL: is.HTMLURL}, true, nil
			}
		}
		if len(issues) < markerLookupPerPage {
			break
		}
	}
	return IssueRef{}, false, nil
}

// CreateChangeRequest opens a pull request via POST /repos/{owner}/{repo}/pulls.
// An open pull request from the same head is returned with AlreadyExisted
// rather than duplicated, so a retried request is idempotent. An empty Base
// resolves the repo's default branch.
func (f *giteaForge) CreateChangeRequest(ctx context.Context, repo string, spec ChangeRequestSpec) (ChangeRequestRef, error) {
	owner, name := f.ownerRepo(repo)
	slug := owner + "/" + name
	if strings.TrimSpace(spec.Head) == "" || strings.TrimSpace(spec.Title) == "" {
		return ChangeRequestRef{}, fmt.Errorf("%s: open pull request in %s: head and title are required", f.label, slug)
	}
	open, err := f.ListOpenChangeRequests(ctx, repo)
	if err != nil {
		return ChangeRequestRef{}, fmt.Errorf("%s: open pull request in %s: checking for an existing one: %w", f.label, slug, err)
	}
	for _, cr := range open {
		if cr.SourceBranch == spec.Head && (spec.Base == "" || cr.TargetBranch == spec.Base) {
			return ChangeRequestRef{Number: cr.Number, URL: cr.URL, AlreadyExisted: true}, nil
		}
	}
	base := spec.Base
	if base == "" {
		r, err := f.GetRepo(ctx, repo)
		if err != nil {
			return ChangeRequestRef{}, fmt.Errorf("%s: open pull request in %s: resolving the default branch: %w", f.label, slug, err)
		}
		if r.DefaultBranch == "" {
			return ChangeRequestRef{}, fmt.Errorf("%s: open pull request in %s: the repo reports no default branch; set a base", f.label, slug)
		}
		base = r.DefaultBranch
	}
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(name))
	payload := map[string]string{"head": spec.Head, "base": base, "title": spec.Title, "body": spec.Body}
	var out gtCreated
	if err := f.doWriteJSON(ctx, http.MethodPost, endpoint, payload, &out); err != nil {
		return ChangeRequestRef{}, fmt.Errorf("%s: open pull request in %s: %w", f.label, slug, err)
	}
	ref := ChangeRequestRef{Number: out.Number, URL: out.HTMLURL}
	if err := f.AddLabels(ctx, repo, ref.Number, spec.Labels); err != nil { // writeback:solicited labels of the pull request the caller is opening
		return ref, fmt.Errorf("%w: pull request %s#%d: %v", ErrLabelsNotApplied, slug, ref.Number, err)
	}
	return ref, nil
}

// doWriteJSON is doWrite that decodes a successful response body into out.
func (f *giteaForge) doWriteJSON(ctx context.Context, method, endpoint string, payload, out any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, method, f.baseURL+giteaAPIPath+endpoint, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	f.setAuth(req)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("%s API %s returned status %d: %s", f.label, endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	if readErr != nil {
		return fmt.Errorf("reading body from %s: %w", endpoint, readErr)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

var (
	_ IssueSeam            = (*giteaForge)(nil)
	_ ChangeRequestCreator = (*giteaForge)(nil)
	_ IssueSeam            = (*forgejoForge)(nil)
	_ ChangeRequestCreator = (*forgejoForge)(nil)
)
