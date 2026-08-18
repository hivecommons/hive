package forge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	// giteaAPIPath is the REST API v1 path appended to the instance root.
	giteaAPIPath = "/api/v1"
	// giteaPerPage is the page size requested from list endpoints.
	giteaPerPage = 50
	// giteaMaxPages caps pagination so a misbehaving server cannot loop us
	// forever; 100 pages * 50 per page = 5k items, ample for a repo.
	giteaMaxPages = 100
	// giteaHTTPTimeout bounds a single HTTP request.
	giteaHTTPTimeout = 30 * time.Second
	// giteaAuthHeader is the HTTP header carrying the access token.
	giteaAuthHeader = "Authorization"
	// giteaTokenScheme is the auth scheme prefix Gitea/Forgejo expect on the
	// Authorization header value ("token <TOKEN>").
	giteaTokenScheme = "token "
	// giteaStateOpen is the query value selecting open issues / pull requests.
	giteaStateOpen = "open"
)

// giteaForge implements Forge against the Gitea/Forgejo REST API v1 using only
// the standard library. It covers BOTH the read path (get repo, list open
// issues, list open pull requests) and the write path (comment, add/remove
// labels, hold gate). Gitea and Forgejo share this API surface.
type giteaForge struct {
	baseURL string // instance root, no trailing slash, no /api/v1
	token   string
	org     string
	http    *http.Client
}

// newGiteaForge constructs the Gitea adapter. token authenticates as a
// "token <TOKEN>" Authorization header; it may be empty for unauthenticated
// reads of public repos but is required for private repos and all writes. The
// caller supplies it (never read from the environment here).
//
// Unlike GitHub/GitLab there is no single public Gitea SaaS host, so a BaseURL
// is required — an empty BaseURL is an error rather than a silent default.
func newGiteaForge(token string, opts Options) (*giteaForge, error) {
	base := strings.TrimRight(opts.BaseURL, "/")
	if base == "" {
		return nil, fmt.Errorf("gitea: BaseURL is required (no public Gitea default)")
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, fmt.Errorf("gitea: invalid base URL %q: %w", base, err)
	}
	if u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("gitea: base URL %q must be absolute (scheme + host)", base)
	}
	return &giteaForge{
		baseURL: base,
		token:   token,
		org:     opts.Org,
		http:    &http.Client{Timeout: giteaHTTPTimeout},
	}, nil
}

func (f *giteaForge) Kind() Kind { return KindGitea }

// gtRepo is the subset of a Gitea repository we map onto Repo.
type gtRepo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	HTMLURL       string `json:"html_url"`
	DefaultBranch string `json:"default_branch"`
	Description   string `json:"description"`
	Owner         struct {
		Login string `json:"login"`
	} `json:"owner"`
}

// gtUser is the author shape returned inline on issues and pull requests.
type gtUser struct {
	Login string `json:"login"`
}

// gtLabel is the label shape returned inline on issues and pull requests.
type gtLabel struct {
	Name string `json:"name"`
}

// gtIssue is the subset of a Gitea issue we map onto Issue. The /issues
// endpoint can also return pull requests; the PullRequest field is set on those
// so we can filter them out.
type gtIssue struct {
	Number      int       `json:"number"`
	Title       string    `json:"title"`
	State       string    `json:"state"`
	Labels      []gtLabel `json:"labels"`
	HTMLURL     string    `json:"html_url"`
	CreatedAt   time.Time `json:"created_at"`
	User        gtUser    `json:"user"`
	Assignees   []gtUser  `json:"assignees"`
	PullRequest *gtPRStub `json:"pull_request"`
}

// gtPRStub is the non-nil marker Gitea sets on an /issues entry that is really a
// pull request; its presence is what distinguishes a PR from a plain issue.
type gtPRStub struct {
	Merged bool `json:"merged"`
}

// gtPull is the subset of a Gitea pull request we map onto ChangeRequest.
type gtPull struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	State     string    `json:"state"`
	Labels    []gtLabel `json:"labels"`
	HTMLURL   string    `json:"html_url"`
	CreatedAt time.Time `json:"created_at"`
	Draft     bool      `json:"draft"`
	User      gtUser    `json:"user"`
	Head      gtBranch  `json:"head"`
	Base      gtBranch  `json:"base"`
}

// gtBranch is the head/base branch shape on a Gitea pull request.
type gtBranch struct {
	Ref string `json:"ref"`
	SHA string `json:"sha"`
}

// ownerRepo resolves a repo argument into Gitea owner + repo path segments. A
// bare name is prefixed with the default org; Gitea repo paths are always
// exactly "owner/repo" (no nesting), so only the first "/" is significant.
func (f *giteaForge) ownerRepo(repo string) (owner, name string) {
	if i := strings.Index(repo, "/"); i >= 0 {
		return repo[:i], repo[i+1:]
	}
	return f.org, repo
}

func (f *giteaForge) GetRepo(ctx context.Context, repo string) (*Repo, error) {
	owner, name := f.ownerRepo(repo)
	endpoint := fmt.Sprintf("/repos/%s/%s", url.PathEscape(owner), url.PathEscape(name))
	var r gtRepo
	if err := f.getJSON(ctx, endpoint, nil, &r); err != nil {
		return nil, fmt.Errorf("gitea: get repo %s/%s: %w", owner, name, err)
	}
	full := r.FullName
	if full == "" {
		full = owner + "/" + name
	}
	outOwner := r.Owner.Login
	if outOwner == "" {
		outOwner = owner
	}
	outName := r.Name
	if outName == "" {
		outName = name
	}
	return &Repo{
		FullName:      full,
		Owner:         outOwner,
		Name:          outName,
		URL:           r.HTMLURL,
		DefaultBranch: r.DefaultBranch,
		Description:   r.Description,
	}, nil
}

func (f *giteaForge) ListOpenIssues(ctx context.Context, repo string) ([]Issue, error) {
	owner, name := f.ownerRepo(repo)
	slug := owner + "/" + name
	endpoint := fmt.Sprintf("/repos/%s/%s/issues", url.PathEscape(owner), url.PathEscape(name))
	q := url.Values{}
	q.Set("state", giteaStateOpen)
	// type=issues excludes pull requests server-side where supported; we also
	// filter client-side via the pull_request marker as a belt-and-braces guard.
	q.Set("type", "issues")

	var out []Issue
	err := f.paginate(ctx, endpoint, q, func(body []byte) (int, error) {
		var page []gtIssue
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding issues page: %w", err)
		}
		for _, it := range page {
			if it.PullRequest != nil {
				continue // a PR masquerading as an issue; skip it
			}
			out = append(out, Issue{
				Repo:      slug,
				Number:    it.Number,
				Title:     it.Title,
				Author:    it.User.Login,
				Labels:    labelNames(it.Labels),
				Assignees: loginNames(it.Assignees),
				State:     it.State,
				CreatedAt: it.CreatedAt,
				URL:       it.HTMLURL,
			})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitea: list issues for %q: %w", slug, err)
	}
	return out, nil
}

func (f *giteaForge) ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error) {
	owner, name := f.ownerRepo(repo)
	slug := owner + "/" + name
	endpoint := fmt.Sprintf("/repos/%s/%s/pulls", url.PathEscape(owner), url.PathEscape(name))
	q := url.Values{}
	q.Set("state", giteaStateOpen)

	var out []ChangeRequest
	err := f.paginate(ctx, endpoint, q, func(body []byte) (int, error) {
		var page []gtPull
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding pulls page: %w", err)
		}
		for _, pr := range page {
			out = append(out, ChangeRequest{
				Repo:         slug,
				Number:       pr.Number,
				Title:        pr.Title,
				Author:       pr.User.Login,
				Labels:       labelNames(pr.Labels),
				Draft:        pr.Draft,
				State:        pr.State,
				CreatedAt:    pr.CreatedAt,
				URL:          pr.HTMLURL,
				SourceBranch: pr.Head.Ref,
				TargetBranch: pr.Base.Ref,
				HeadSHA:      pr.Head.SHA,
			})
		}
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitea: list pull requests for %q: %w", slug, err)
	}
	return out, nil
}

// --- Write path ---

// CreateIssueComment posts a comment via
// POST /repos/{owner}/{repo}/issues/{index}/comments. Gitea routes both issue
// and pull-request comments through the /issues/{index} tree, so one method
// covers issues and change requests alike.
func (f *giteaForge) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	owner, name := f.ownerRepo(repo)
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/comments", url.PathEscape(owner), url.PathEscape(name), number)
	payload := map[string]string{"body": body}
	if err := f.doWrite(ctx, http.MethodPost, endpoint, payload, nil); err != nil {
		return fmt.Errorf("gitea: comment on %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// AddLabels adds labels to an issue/PR via
// POST /repos/{owner}/{repo}/issues/{index}/labels. Modern Gitea/Forgejo accept
// label names in the "labels" array. It is a no-op when labels is empty.
func (f *giteaForge) AddLabels(ctx context.Context, repo string, number int, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	owner, name := f.ownerRepo(repo)
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/labels", url.PathEscape(owner), url.PathEscape(name), number)
	payload := map[string]any{"labels": labels}
	if err := f.doWrite(ctx, http.MethodPost, endpoint, payload, nil); err != nil {
		return fmt.Errorf("gitea: add labels to %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// RemoveLabel removes a single label via
// DELETE /repos/{owner}/{repo}/issues/{index}/labels/{label}. Modern
// Gitea/Forgejo accept the label name in the path. A 404 (label not present) is
// treated as success to satisfy the interface's idempotent-remove contract.
func (f *giteaForge) RemoveLabel(ctx context.Context, repo string, number int, label string) error {
	owner, name := f.ownerRepo(repo)
	endpoint := fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s",
		url.PathEscape(owner), url.PathEscape(name), number, url.PathEscape(label))
	okNotFound := func(status int) bool { return status == http.StatusNotFound }
	if err := f.doWrite(ctx, http.MethodDelete, endpoint, nil, okNotFound); err != nil {
		return fmt.Errorf("gitea: remove label from %s/%s#%d: %w", owner, name, number, err)
	}
	return nil
}

// SetHold applies or clears the hold gate label via AddLabels/RemoveLabel.
func (f *giteaForge) SetHold(ctx context.Context, repo string, number int, hold bool) error {
	if hold {
		return f.AddLabels(ctx, repo, number, []string{holdLabel})
	}
	return f.RemoveLabel(ctx, repo, number, holdLabel)
}

// giteaPageFunc processes one page body and returns the number of items it
// contained (used to detect the last page) and an error.
type giteaPageFunc func(body []byte) (count int, err error)

// paginate walks Gitea page/limit pagination. Gitea does not always send a
// reliable next-page header, so we page until a short (< limit) page arrives.
func (f *giteaForge) paginate(ctx context.Context, endpoint string, q url.Values, fn giteaPageFunc) error {
	if q == nil {
		q = url.Values{}
	}
	q.Set("limit", strconv.Itoa(giteaPerPage))
	for page := 1; page <= giteaMaxPages; page++ {
		q.Set("page", strconv.Itoa(page))
		body, err := f.getRaw(ctx, endpoint, q)
		if err != nil {
			return err
		}
		count, err := fn(body)
		if err != nil {
			return err
		}
		if count < giteaPerPage {
			return nil // short page => last page
		}
	}
	return nil
}

// getJSON issues a GET and decodes the response body into out.
func (f *giteaForge) getJSON(ctx context.Context, endpoint string, q url.Values, out any) error {
	body, err := f.getRaw(ctx, endpoint, q)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decoding response: %w", err)
	}
	return nil
}

// getRaw issues a GET against the API and returns the response body.
func (f *giteaForge) getRaw(ctx context.Context, endpoint string, q url.Values) ([]byte, error) {
	u := f.baseURL + giteaAPIPath + endpoint
	if enc := q.Encode(); enc != "" {
		u += "?" + enc
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, fmt.Errorf("building request: %w", err)
	}
	f.setAuth(req)
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, readErr := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("gitea API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
	}
	if readErr != nil {
		return nil, fmt.Errorf("reading body from %s: %w", endpoint, readErr)
	}
	return data, nil
}

// doWrite issues a POST/PUT/DELETE with an optional JSON body and discards a
// successful response. okStatus, when non-nil, marks additional non-2xx statuses
// (e.g. a 404 on an idempotent DELETE) as success.
func (f *giteaForge) doWrite(ctx context.Context, method, endpoint string, payload any, okStatus func(int) bool) error {
	var bodyReader io.Reader
	if payload != nil {
		buf, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		bodyReader = bytes.NewReader(buf)
	}
	u := f.baseURL + giteaAPIPath + endpoint
	req, err := http.NewRequestWithContext(ctx, method, u, bodyReader)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	f.setAuth(req)
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")

	resp, err := f.http.Do(req)
	if err != nil {
		return fmt.Errorf("request to %s: %w", endpoint, err)
	}
	defer func() { _ = resp.Body.Close() }()

	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	if okStatus != nil && okStatus(resp.StatusCode) {
		return nil
	}
	return fmt.Errorf("gitea API %s returned status %d: %s", endpoint, resp.StatusCode, truncate(string(data), 200))
}

// setAuth sets the token Authorization header when a token is configured.
func (f *giteaForge) setAuth(req *http.Request) {
	if f.token != "" {
		req.Header.Set(giteaAuthHeader, giteaTokenScheme+f.token)
	}
}

// labelNames extracts label names, guaranteeing a non-nil slice so downstream
// range/join calls are always safe.
func labelNames(labels []gtLabel) []string {
	out := make([]string, 0, len(labels))
	for _, l := range labels {
		if l.Name != "" {
			out = append(out, l.Name)
		}
	}
	return out
}

// loginNames extracts non-empty login names from a slice of Gitea users.
func loginNames(users []gtUser) []string {
	out := make([]string, 0, len(users))
	for _, u := range users {
		if u.Login != "" {
			out = append(out, u.Login)
		}
	}
	return out
}

// Compile-time assertion that the adapter satisfies the interface.
var _ Forge = (*giteaForge)(nil)
