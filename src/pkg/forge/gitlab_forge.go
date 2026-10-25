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
	"time"
)

const (
	// defaultGitLabBaseURL is the public GitLab SaaS instance root.
	defaultGitLabBaseURL = "https://gitlab.com"
	// gitLabAPIPath is the REST API v4 path appended to the instance root.
	gitLabAPIPath = "/api/v4"
	// gitLabPerPage is the page size requested from list endpoints.
	gitLabPerPage = 100
	// gitLabMaxPages caps pagination so a misbehaving server cannot loop us
	// forever; 100 pages * 100 per page = 10k items, ample for a repo.
	gitLabMaxPages = 100
	// gitLabHTTPTimeout bounds a single HTTP request.
	gitLabHTTPTimeout = 30 * time.Second
	// gitLabTokenHeader is the GitLab personal/project access token header.
	gitLabTokenHeader = "PRIVATE-TOKEN"
)

// gitLabForge implements Forge and Merger against the GitLab REST API v4 using
// only the standard library. It covers the read path (get project, list open
// issues, list open merge requests), write path (comments, labels, hold gate),
// and merge operations.
type gitLabForge struct {
	baseURL string // instance root, no trailing slash, no /api/v4
	token   string
	org     string
	http    *http.Client
}

// newGitLabForge constructs the GitLab adapter. token authenticates as a
// PRIVATE-TOKEN header; it may be empty for unauthenticated reads of public
// projects but is required for private ones. The caller supplies it (never read
// from the environment here).
func newGitLabForge(token string, opts Options) (*gitLabForge, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		base = defaultGitLabBaseURL
	}
	if _, err := url.Parse(base); err != nil {
		return nil, fmt.Errorf("gitlab: invalid base URL %q: %w", base, err)
	}
	return &gitLabForge{
		baseURL: base,
		token:   token,
		org:     opts.Org,
		http:    &http.Client{Timeout: gitLabHTTPTimeout},
	}, nil
}

func (f *gitLabForge) Kind() Kind { return KindGitLab }

// glProject is the subset of a GitLab project we map onto Repo.
type glProject struct {
	ID                int    `json:"id"`
	PathWithNamespace string `json:"path_with_namespace"`
	Path              string `json:"path"`
	WebURL            string `json:"web_url"`
	DefaultBranch     string `json:"default_branch"`
	Description       string `json:"description"`
	Namespace         struct {
		FullPath string `json:"full_path"`
		Path     string `json:"path"`
	} `json:"namespace"`
}

// glUser is the author/assignee shape returned inline on issues and MRs.
type glUser struct {
	Username string `json:"username"`
}

// glIssue is the subset of a GitLab issue we map onto Issue.
type glIssue struct {
	IID       int       `json:"iid"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Labels    []string  `json:"labels"`
	WebURL    string    `json:"web_url"`
	CreatedAt time.Time `json:"created_at"`
	Author    glUser    `json:"author"`
	Assignees []glUser  `json:"assignees"`
}

// glMergeRequest is the subset of a GitLab merge request we map onto
// ChangeRequest.
type glMergeRequest struct {
	IID          int       `json:"iid"`
	Title        string    `json:"title"`
	State        string    `json:"state"`
	Labels       []string  `json:"labels"`
	WebURL       string    `json:"web_url"`
	CreatedAt    time.Time `json:"created_at"`
	Draft        bool      `json:"draft"`
	WorkInProg   bool      `json:"work_in_progress"`
	SourceBranch string    `json:"source_branch"`
	TargetBranch string    `json:"target_branch"`
	SHA          string    `json:"sha"`
	Author       glUser    `json:"author"`
}

// projectSlug resolves a repo argument into a GitLab project path. GitLab
// namespaces can be nested ("group/subgroup/project"), so a bare name is only
// prefixed with the default org when it contains no "/".
func (f *gitLabForge) projectSlug(repo string) string {
	if strings.Contains(repo, "/") {
		return repo
	}
	if f.org != "" {
		return f.org + "/" + repo
	}
	return repo
}

func (f *gitLabForge) GetRepo(ctx context.Context, repo string) (*Repo, error) {
	slug := f.projectSlug(repo)
	// GitLab requires the project path to be URL-encoded as a single path segment.
	endpoint := fmt.Sprintf("/projects/%s", url.PathEscape(slug))
	var p glProject
	if err := f.getJSON(ctx, endpoint, nil, &p); err != nil {
		return nil, fmt.Errorf("gitlab: get project %q: %w", slug, err)
	}
	full := p.PathWithNamespace
	if full == "" {
		full = slug
	}
	name := p.Path
	if name == "" {
		_, name = splitRepoLast(full)
	}
	owner := p.Namespace.FullPath
	if owner == "" {
		owner, _ = splitOwnerLast(full)
	}
	return &Repo{
		FullName:      full,
		Owner:         owner,
		Name:          name,
		URL:           p.WebURL,
		DefaultBranch: p.DefaultBranch,
		Description:   p.Description,
	}, nil
}

func (f *gitLabForge) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	slug := f.projectSlug(repo)
	endpoint := fmt.Sprintf("/projects/%s/issues", url.PathEscape(slug))
	q := url.Values{}
	q.Set("state", "opened")

	var out []Issue
	err := f.paginate(ctx, endpoint, q, func(body []byte) error {
		var page []glIssue
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("decoding issues page: %w", err)
		}
		for _, it := range page {
			out = append(out, Issue{
				Repo:      slug,
				Number:    it.IID,
				Title:     it.Title,
				Author:    it.Author.Username,
				Labels:    normalizeLabels(it.Labels),
				Assignees: usernames(it.Assignees),
				State:     it.State,
				CreatedAt: it.CreatedAt,
				URL:       it.WebURL,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: list issues for %q: %w", slug, err)
	}
	return out, nil
}

func (f *gitLabForge) ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error) {
	slug := f.projectSlug(repo)
	endpoint := fmt.Sprintf("/projects/%s/merge_requests", url.PathEscape(slug))
	q := url.Values{}
	q.Set("state", "opened")

	var out []ChangeRequest
	err := f.paginate(ctx, endpoint, q, func(body []byte) error {
		var page []glMergeRequest
		if err := json.Unmarshal(body, &page); err != nil {
			return fmt.Errorf("decoding merge_requests page: %w", err)
		}
		for _, mr := range page {
			out = append(out, ChangeRequest{
				Repo:         slug,
				Number:       mr.IID,
				Title:        mr.Title,
				Author:       mr.Author.Username,
				Labels:       normalizeLabels(mr.Labels),
				Draft:        mr.Draft || mr.WorkInProg,
				State:        mr.State,
				CreatedAt:    mr.CreatedAt,
				URL:          mr.WebURL,
				SourceBranch: mr.SourceBranch,
				TargetBranch: mr.TargetBranch,
				HeadSHA:      mr.SHA,
			})
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitlab: list merge requests for %q: %w", slug, err)
	}
	return out, nil
}

// --- Write path ---
//
// The GitLab write endpoints below target the ISSUE resource. GitLab models
// issue notes and issue labels via /projects/:id/issues/:iid/...; merge-request
// writes live under a parallel /merge_requests/:iid tree. Hive's hold gate and
// triage comments operate on issues, so the issue endpoints are what the neutral
// interface needs first. Comment/label semantics are identical on both trees, so
// extending to MRs later is a mechanical endpoint swap.

// CreateIssueComment posts a note on an issue via
// POST /projects/:id/issues/:iid/notes.
func (f *gitLabForge) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	slug := f.projectSlug(repo)
	endpoint := fmt.Sprintf("/projects/%s/issues/%d/notes", url.PathEscape(slug), number)
	payload := map[string]string{"body": body}
	if err := f.doWrite(ctx, http.MethodPost, endpoint, payload); err != nil {
		return fmt.Errorf("gitlab: comment on %q#%d: %w", slug, number, err)
	}
	return nil
}

// AddLabels adds labels to an issue via PUT /projects/:id/issues/:iid with the
// add_labels field (a comma-separated list). It is a no-op when labels is empty.
func (f *gitLabForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	slug := f.projectSlug(repo)
	endpoint := fmt.Sprintf("/projects/%s/issues/%d", url.PathEscape(slug), number)
	payload := map[string]string{"add_labels": strings.Join(labels, ",")}
	if err := f.doWrite(ctx, http.MethodPut, endpoint, payload); err != nil {
		return fmt.Errorf("gitlab: add labels to %q#%d: %w", slug, number, err)
	}
	return nil
}

// RemoveLabel removes a single label from an issue via PUT with remove_labels.
// GitLab treats removing an absent label as success, matching the interface
// contract.
func (f *gitLabForge) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	slug := f.projectSlug(repo)
	endpoint := fmt.Sprintf("/projects/%s/issues/%d", url.PathEscape(slug), number)
	payload := map[string]string{"remove_labels": label}
	if err := f.doWrite(ctx, http.MethodPut, endpoint, payload); err != nil {
		return fmt.Errorf("gitlab: remove label from %q#%d: %w", slug, number, err)
	}
	return nil
}

// SetHold applies or clears the hold gate label using AddLabels/RemoveLabel.
func (f *gitLabForge) SetHold(ctx context.Context, repo string, number int, hold bool) error {
	if hold {
		return f.AddLabels(ctx, repo, number, []string{holdLabel})
	}
	return f.RemoveLabel(ctx, repo, number, holdLabel)
}

// --- Merge path ---

// glMergePayload is the body sent to PUT /projects/:id/merge_requests/:iid/merge.
type glMergePayload struct {
	SHA                      string `json:"sha,omitempty"`
	Squash                   *bool  `json:"squash,omitempty"`
	ShouldRemoveSourceBranch bool   `json:"should_remove_source_branch,omitempty"`
}

// Merge accepts a merge request via PUT /projects/:id/merge_requests/:iid/merge.
//
// Strategy mapping:
//   - "squash": sets squash=true in the merge payload.
//   - "merge": sets squash=false in the merge payload.
//   - "rebase": executes rebase-then-merge: calls PUT .../rebase first, then PUT .../merge.
//   - empty (""): leaves squash unset to preserve project/MR default behavior.
//   - DeleteBranch: sets should_remove_source_branch=true.
//   - HeadSHA: passes sha=<HeadSHA> for the head-SHA guard.
//
// Error mapping:
//   - If the MR carries the "hold" label: returns ErrHoldActive.
//   - HTTP 405 (Method Not Allowed): returns ErrNotMergeable (e.g. conflicts, draft, CI blocked).
//   - A closed (not merged) MR: returns ErrNotMergeable before any write.
//   - HTTP 406 (Not Acceptable): GitLab sends it for both "already merged" and
//     "closed", so the MR state is re-read: merged returns ErrAlreadyMerged,
//     anything else ErrNotMergeable. A closed MR must never be reported as
//     merged (the relay records ErrAlreadyMerged as success).
//   - HTTP 409 (Conflict): returns ErrConflict (head SHA mismatch or concurrent merge).
func (f *gitLabForge) Merge(ctx context.Context, repo string, number int, opts MergeOptions) error {
	if strings.TrimSpace(repo) == "" {
		return fmt.Errorf("gitlab: merge: repo is required")
	}
	if number <= 0 {
		return fmt.Errorf("gitlab: merge: merge request number must be > 0 (got %d)", number)
	}

	strategy := strings.ToLower(strings.TrimSpace(opts.Strategy))
	switch strategy {
	case "", string(MergeStrategyMerge), string(MergeStrategySquash), string(MergeStrategyRebase):
		// valid neutral strategy
	default:
		return fmt.Errorf("gitlab: unsupported merge strategy %q", opts.Strategy)
	}

	slug := f.projectSlug(repo)
	mrEndpoint := fmt.Sprintf("/projects/%s/merge_requests/%d", url.PathEscape(slug), number)

	// Step 1: Honor the hold label and inspect MR state.
	var mr glMergeRequest
	if err := f.getJSON(ctx, mrEndpoint, nil, &mr); err != nil {
		return fmt.Errorf("gitlab: check merge request %q#%d: %w", slug, number, err)
	}
	for _, l := range mr.Labels {
		if strings.EqualFold(strings.TrimSpace(l), holdLabel) {
			return ErrHoldActive
		}
	}
	if strings.EqualFold(mr.State, "merged") {
		return ErrAlreadyMerged
	}
	if strings.EqualFold(mr.State, "closed") {
		return fmt.Errorf("%w: gitlab merge request %q#%d is closed", ErrNotMergeable, slug, number)
	}

	// Step 2: Handle rebase-then-merge if strategy is "rebase".
	if strategy == string(MergeStrategyRebase) {
		rebaseEndpoint := fmt.Sprintf("/projects/%s/merge_requests/%d/rebase", url.PathEscape(slug), number)
		if err := f.doRebase(ctx, rebaseEndpoint); err != nil {
			return fmt.Errorf("gitlab: rebase %q#%d: %w", slug, number, err)
		}
	}

	// Step 3: Accept the merge request.
	mergeEndpoint := fmt.Sprintf("/projects/%s/merge_requests/%d/merge", url.PathEscape(slug), number)
	payload := glMergePayload{
		SHA:                      strings.TrimSpace(opts.HeadSHA),
		ShouldRemoveSourceBranch: opts.DeleteBranch,
	}
	if strategy == string(MergeStrategySquash) {
		payload.Squash = new(true)
	} else if strategy == string(MergeStrategyMerge) {
		payload.Squash = new(false)
	}

	err := f.doAcceptMerge(ctx, mergeEndpoint, payload)
	if !errors.Is(err, errGitLabNotAcceptable) {
		return err
	}
	// 406 is ambiguous (merged or closed): only a re-read that says "merged"
	// may surface as ErrAlreadyMerged.
	var after glMergeRequest
	if gerr := f.getJSON(ctx, mrEndpoint, nil, &after); gerr != nil {
		return fmt.Errorf("%w: gitlab 406 and re-check failed: %v", ErrNotMergeable, gerr)
	}
	if strings.EqualFold(after.State, "merged") {
		return fmt.Errorf("%w: %v", ErrAlreadyMerged, err)
	}
	return fmt.Errorf("%w: gitlab merge request %q#%d is %s: %v", ErrNotMergeable, slug, number, after.State, err)
}

// errGitLabNotAcceptable marks a 406 from the accept endpoint; Merge resolves
// it against the MR state because GitLab uses 406 for merged and closed alike.
var errGitLabNotAcceptable = errors.New("gitlab 406")

func (f *gitLabForge) doRebase(ctx context.Context, endpoint string) error {
	u := f.baseURL + gitLabAPIPath + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, nil)
	if err != nil {
		return fmt.Errorf("building rebase request: %w", err)
	}
	if f.token != "" {
		req.Header.Set(gitLabTokenHeader, f.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusCreated:
		return nil
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: gitlab 405: %s", ErrNotMergeable, truncate(string(data), 200))
	case http.StatusConflict:
		return fmt.Errorf("%w: gitlab 409: %s", ErrConflict, truncate(string(data), 200))
	default:
		return fmt.Errorf("gitlab API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
}

func (f *gitLabForge) doAcceptMerge(ctx context.Context, endpoint string, payload glMergePayload) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding merge request body: %w", err)
	}
	u := f.baseURL + gitLabAPIPath + endpoint
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, u, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("building merge request: %w", err)
	}
	if f.token != "" {
		req.Header.Set(gitLabTokenHeader, f.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	switch resp.StatusCode {
	case http.StatusOK, http.StatusAccepted, http.StatusCreated:
		return nil
	case http.StatusMethodNotAllowed:
		return fmt.Errorf("%w: gitlab 405: %s", ErrNotMergeable, truncate(string(data), 200))
	case http.StatusNotAcceptable:
		return fmt.Errorf("%w: %s", errGitLabNotAcceptable, truncate(string(data), 200))
	case http.StatusConflict:
		return fmt.Errorf("%w: gitlab 409: %s", ErrConflict, truncate(string(data), 200))
	default:
		return fmt.Errorf("gitlab API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
}

// doWrite issues a POST/PUT with a JSON body and discards a successful response.
func (f *gitLabForge) doWrite(ctx context.Context, method, endpoint string, payload any) error {
	buf, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("encoding request: %w", err)
	}
	u := f.baseURL + gitLabAPIPath + endpoint
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(buf))
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	if f.token != "" {
		req.Header.Set(gitLabTokenHeader, f.token)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	return nil
}

// pageFunc processes one page body and returns an error. The paginate loop
// separately reads the X-Next-Page header to decide whether to continue.
type pageFunc func(body []byte) error

// paginate walks GitLab keyset/offset pagination via the X-Next-Page response
// header, invoking fn for each page body.
func (f *gitLabForge) paginate(ctx context.Context, endpoint string, q url.Values, fn pageFunc) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("per_page", strconv.Itoa(gitLabPerPage))
	page := 1
	for i := 0; i < gitLabMaxPages; i++ {
		q.Set("page", strconv.Itoa(page))
		body, nextPage, err := f.getRaw(ctx, endpoint, q)
		if err != nil {
			return err
		}
		if err := fn(body); err != nil {
			return err
		}
		if nextPage == 0 {
			return nil
		}
		page = nextPage
	}
	return nil
}

// getJSON issues a GET and decodes the response body into out.
func (f *gitLabForge) getJSON(ctx context.Context, endpoint string, q url.Values, out any) error {
	body, _, err := f.getRaw(ctx, endpoint, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// getRaw issues a GET against the API and returns the body plus the parsed
// X-Next-Page header (0 when absent/last page).
func (f *gitLabForge) getRaw(ctx context.Context, endpoint string, q url.Values) (body []byte, nextPage int, err error) {
	u := f.baseURL + gitLabAPIPath + endpoint
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, 0, fmt.Errorf("building request: %w", err)
	}
	if f.token != "" {
		req.Header.Set(gitLabTokenHeader, f.token)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, 0, fmt.Errorf("gitlab API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	if readErr != nil {
		return nil, 0, fmt.Errorf("reading body from %s: %w", endpoint, readErr)
	}

	if np := resp.Header.Get("X-Next-Page"); np != "" {
		if parsed, convErr := strconv.Atoi(np); convErr == nil {
			nextPage = parsed
		}
	}
	return data, nextPage, nil
}

// normalizeLabels guards against a nil labels slice so downstream range/join
// calls are always safe.
func normalizeLabels(labels []string) []string {
	if labels == nil {
		return []string{}
	}
	return labels
}

// usernames extracts non-empty usernames from a slice of GitLab users.
func usernames(users []glUser) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u.Username != "" {
			out = append(out, u.Username)
		}
	}
	return out
}

// splitRepoLast returns the owner and last path segment of a slug.
func splitRepoLast(full string) (owner, name string) {
	if i := strings.LastIndex(full, "/"); i >= 0 {
		return full[:i], full[i+1:]
	}
	return "", full
}

// splitOwnerLast returns everything before the last "/" as owner.
func splitOwnerLast(full string) (owner, name string) {
	return splitRepoLast(full)
}

// truncate shortens s to at most n runes for error messages.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Compile-time assertions that the adapter satisfies the interfaces.
var _ Forge = (*gitLabForge)(nil)
var _ Merger = (*gitLabForge)(nil)
