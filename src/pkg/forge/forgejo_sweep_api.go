package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// SweepAPI is the read and write surface the Forgejo automerge and
// close-on-merge sweeps (pkg/forgesweep) need beyond Forge and Merger. Only
// the Forgejo adapter implements it: the sweeps were built and tested against
// Forgejo's REST API, and Gitea and GitLab are not implemented, so neither
// sweep runs on them.
type SweepAPI interface {
	Forge
	Merger

	// PullFacts reads one pull request with the fields the automerge gate
	// judges.
	PullFacts(ctx context.Context, repo string, number int) (PullFacts, error)
	// CombinedStatus returns the combined commit status state at sha
	// ("success", "pending", "failure", "error" or "") and how many statuses
	// it combines.
	CombinedStatus(ctx context.Context, repo, sha string) (state string, total int, err error)
	// LabelLastAdded returns the timeline event that last added label to the
	// issue or pull request. ok is false when the latest event for that label
	// removed it, or no event names it.
	LabelLastAdded(ctx context.Context, repo string, number int, label string) (add LabelAdd, ok bool, err error)
	// Permission returns login's access to repo: "none", "read", "write",
	// "admin" or "owner", or PermissionAssignable when Forgejo refuses the
	// permission API and the login is on the repo's assignee list.
	Permission(ctx context.Context, repo, login string) (string, error)
	// RecentlyMergedPulls lists the most recently updated closed pull
	// requests that were merged, newest first, at most limit of them.
	RecentlyMergedPulls(ctx context.Context, repo string, limit int) ([]MergedPull, error)
	// IssueFacts reads one issue: its state, author, labels and creation
	// time, and whether the number is really a pull request.
	IssueFacts(ctx context.Context, repo string, number int) (IssueFacts, error)
	// IssueCommentBodies returns the bodies of an issue's comments.
	IssueCommentBodies(ctx context.Context, repo string, number int) ([]string, error)
	// IssueReopened reports whether the issue's timeline holds a reopen
	// event.
	IssueReopened(ctx context.Context, repo string, number int) (bool, error)
	// CloseIssue sets an issue's state to closed.
	CloseIssue(ctx context.Context, repo string, number int) error
	// Self returns the login the token authenticates as.
	Self(ctx context.Context) (string, error)
}

// PullFacts is one pull request as the automerge gate sees it.
type PullFacts struct {
	Number    int
	State     string
	Draft     bool
	Merged    bool
	Mergeable bool
	Author    string
	Labels    []string
	HeadSHA   string
	Base      string
}

// LabelAdd is the timeline event that last added a label.
type LabelAdd struct {
	// Login added the label.
	Login string
	// PushedAfter is true when the timeline records a push to the pull
	// request (a pull_push event, force pushes included) after the label
	// was added: the approval the label carries predates the current head.
	PushedAfter bool
}

// IssueFacts is one issue as the close-on-merge sweep sees it.
type IssueFacts struct {
	State     string
	IsPull    bool
	Author    string
	Labels    []string
	CreatedAt time.Time
}

// MergedPull is a merged pull request as the close-on-merge sweep sees it.
type MergedPull struct {
	Number   int
	Author   string
	Title    string
	Body     string
	Base     string
	MergedAt time.Time
}

// NewForgejoSweepAPI builds the Forgejo adapter typed as SweepAPI.
func NewForgejoSweepAPI(token string, opts Options) (SweepAPI, error) {
	return newForgejoForge(token, opts)
}

// fjPull is the subset of a Forgejo pull request the sweeps decode. It adds
// mergeable, body and merged_at to gtPull's fields.
type fjPull struct {
	Number    int        `json:"number"`
	Title     string     `json:"title"`
	Body      string     `json:"body"`
	State     string     `json:"state"`
	Draft     bool       `json:"draft"`
	Merged    bool       `json:"merged"`
	Mergeable bool       `json:"mergeable"`
	MergedAt  *time.Time `json:"merged_at"`
	User      gtUser     `json:"user"`
	Labels    []gtLabel  `json:"labels"`
	Head      gtBranch   `json:"head"`
	Base      gtBranch   `json:"base"`
}

func (f *forgejoForge) repoPath(repo string) string {
	owner, name := f.ownerRepo(repo)
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name)
}

func (f *forgejoForge) PullFacts(ctx context.Context, repo string, number int) (PullFacts, error) {
	var pr fjPull
	if err := f.getJSON(ctx, fmt.Sprintf("%s/pulls/%d", f.repoPath(repo), number), nil, &pr); err != nil {
		return PullFacts{}, fmt.Errorf("%s: read pull request %s#%d: %w", f.label, repo, number, err)
	}
	return PullFacts{
		Number:    pr.Number,
		State:     pr.State,
		Draft:     pr.Draft,
		Merged:    pr.Merged,
		Mergeable: pr.Mergeable,
		Author:    pr.User.Login,
		Labels:    labelNames(pr.Labels),
		HeadSHA:   pr.Head.SHA,
		Base:      pr.Base.Ref,
	}, nil
}

func (f *forgejoForge) CombinedStatus(ctx context.Context, repo, sha string) (string, int, error) {
	if strings.TrimSpace(sha) == "" {
		return "", 0, fmt.Errorf("%s: combined status: sha is required", f.label)
	}
	var out struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	if err := f.getJSON(ctx, fmt.Sprintf("%s/commits/%s/status", f.repoPath(repo), url.PathEscape(sha)), nil, &out); err != nil {
		return "", 0, fmt.Errorf("%s: combined status %s@%s: %w", f.label, repo, sha, err)
	}
	return out.State, out.TotalCount, nil
}

// fjTimelineEvent is one issue timeline entry. A "label" event's body is "1"
// when the label was added and empty when it was removed.
type fjTimelineEvent struct {
	Type  string   `json:"type"`
	Body  string   `json:"body"`
	User  gtUser   `json:"user"`
	Label *gtLabel `json:"label"`
}

// timelineLabelAdded is the body Forgejo writes on a label-added event.
const timelineLabelAdded = "1"

// timelinePullPush is the timeline event type Forgejo records when commits
// are pushed (or force-pushed) to a pull request's head branch.
const timelinePullPush = "pull_push"

// LabelLastAdded reads the timeline in order (Forgejo returns it oldest
// first), so the last label event wins and a pull_push after it marks the
// addition as predating the head.
func (f *forgejoForge) LabelLastAdded(ctx context.Context, repo string, number int, label string) (LabelAdd, bool, error) {
	var last LabelAdd
	var added, seen bool
	err := f.paginate(ctx, fmt.Sprintf("%s/issues/%d/timeline", f.repoPath(repo), number), nil, func(body []byte) (int, error) {
		var page []fjTimelineEvent
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding timeline: %w", err)
		}
		for _, ev := range page {
			if ev.Type == timelinePullPush {
				last.PushedAfter = true
				continue
			}
			if ev.Type != "label" || ev.Label == nil || !strings.EqualFold(ev.Label.Name, label) {
				continue
			}
			seen = true
			added = ev.Body == timelineLabelAdded
			last = LabelAdd{Login: ev.User.Login}
		}
		return len(page), nil
	})
	if err != nil {
		return LabelAdd{}, false, fmt.Errorf("%s: timeline %s#%d: %w", f.label, repo, number, err)
	}
	if !seen || !added || last.Login == "" {
		return LabelAdd{}, false, nil
	}
	return last, true, nil
}

func (f *forgejoForge) IssueReopened(ctx context.Context, repo string, number int) (bool, error) {
	reopened := false
	err := f.paginate(ctx, fmt.Sprintf("%s/issues/%d/timeline", f.repoPath(repo), number), nil, func(body []byte) (int, error) {
		var page []fjTimelineEvent
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding timeline: %w", err)
		}
		for _, ev := range page {
			if ev.Type == "reopen" {
				reopened = true
			}
		}
		return len(page), nil
	})
	if err != nil {
		return false, fmt.Errorf("%s: timeline %s#%d: %w", f.label, repo, number, err)
	}
	return reopened, nil
}

// PermissionAssignable is what Permission returns, instead of "write", for a
// login on the repo's assignee list when Forgejo refuses the permission API
// (#19914). Forgejo's list also holds members of a team with write on any
// one unit (the wiki or issues only, say) or with read on pull requests, so
// it is weaker than "write": a caller decides whether it is enough.
const PermissionAssignable = "assignable"

// permissionNone is what Permission returns for a login missing from the
// assignee list.
const permissionNone = "none"

// Permission reads login's access through
// GET /repos/{owner}/{repo}/collaborators/{login}/permission. Forgejo answers
// that call about another user only for a site admin or a repo admin, and
// returns 403 to a caller with write access (#19914). On that 403 the
// answer comes from GET /repos/{owner}/{repo}/assignees, which any repo
// reader may call: a login on it is PermissionAssignable, one missing from
// it "none". The list never says "admin" or "owner", and it leaves out a
// site admin with no access of their own to the repo.
func (f *forgejoForge) Permission(ctx context.Context, repo, login string) (string, error) {
	var out struct {
		Permission string `json:"permission"`
	}
	err := f.getJSON(ctx, fmt.Sprintf("%s/collaborators/%s/permission", f.repoPath(repo), url.PathEscape(login)), nil, &out)
	if err == nil {
		return strings.ToLower(strings.TrimSpace(out.Permission)), nil
	}
	if !isGiteaStatus(err, http.StatusForbidden) {
		return "", fmt.Errorf("%s: permission of %s on %s: %w", f.label, login, repo, err)
	}
	var assignees []gtUser
	if aerr := f.getJSON(ctx, f.repoPath(repo)+"/assignees", nil, &assignees); aerr != nil {
		return "", fmt.Errorf("%s: permission of %s on %s: %v; reading the assignee list instead: %w", f.label, login, repo, err, aerr)
	}
	for _, u := range assignees {
		if u.Login != "" && strings.EqualFold(u.Login, login) {
			return PermissionAssignable, nil
		}
	}
	return permissionNone, nil
}

func (f *forgejoForge) RecentlyMergedPulls(ctx context.Context, repo string, limit int) ([]MergedPull, error) {
	if limit <= 0 || limit > giteaPerPage {
		limit = giteaPerPage
	}
	q := url.Values{"state": {"closed"}, "sort": {"recentupdate"}, "limit": {strconv.Itoa(limit)}, "page": {"1"}}
	var pulls []fjPull
	if err := f.getJSON(ctx, f.repoPath(repo)+"/pulls", q, &pulls); err != nil {
		return nil, fmt.Errorf("%s: list closed pull requests %s: %w", f.label, repo, err)
	}
	out := make([]MergedPull, 0, len(pulls))
	for _, pr := range pulls {
		if !pr.Merged || pr.MergedAt == nil {
			continue
		}
		out = append(out, MergedPull{
			Number:   pr.Number,
			Author:   pr.User.Login,
			Title:    pr.Title,
			Body:     pr.Body,
			Base:     pr.Base.Ref,
			MergedAt: *pr.MergedAt,
		})
	}
	return out, nil
}

func (f *forgejoForge) IssueFacts(ctx context.Context, repo string, number int) (IssueFacts, error) {
	var is gtIssue
	if err := f.getJSON(ctx, fmt.Sprintf("%s/issues/%d", f.repoPath(repo), number), nil, &is); err != nil {
		return IssueFacts{}, fmt.Errorf("%s: read issue %s#%d: %w", f.label, repo, number, err)
	}
	return IssueFacts{
		State:     is.State,
		IsPull:    is.PullRequest != nil,
		Author:    is.User.Login,
		Labels:    labelNames(is.Labels),
		CreatedAt: is.CreatedAt,
	}, nil
}

func (f *forgejoForge) IssueCommentBodies(ctx context.Context, repo string, number int) ([]string, error) {
	var bodies []string
	err := f.paginate(ctx, fmt.Sprintf("%s/issues/%d/comments", f.repoPath(repo), number), nil, func(body []byte) (int, error) {
		var page []struct {
			Body string `json:"body"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return 0, fmt.Errorf("decoding comments: %w", err)
		}
		for _, c := range page {
			bodies = append(bodies, c.Body)
		}
		return len(page), nil
	})
	if err != nil {
		return nil, fmt.Errorf("%s: comments %s#%d: %w", f.label, repo, number, err)
	}
	return bodies, nil
}

func (f *forgejoForge) CloseIssue(ctx context.Context, repo string, number int) error {
	if err := f.doWrite(ctx, http.MethodPatch, fmt.Sprintf("%s/issues/%d", f.repoPath(repo), number), map[string]string{"state": "closed"}, nil); err != nil {
		return fmt.Errorf("%s: close issue %s#%d: %w", f.label, repo, number, err)
	}
	return nil
}

func (f *forgejoForge) Self(ctx context.Context) (string, error) {
	// A repository-limited token cannot read /user (#19994). When the operator
	// names the hive's own login in config, trust it and skip the lookup; the
	// sweeps only compare the login against PR and issue authors.
	if f.login != "" {
		return f.login, nil
	}
	var u gtUser
	if err := f.getJSON(ctx, "/user", nil, &u); err != nil {
		return "", fmt.Errorf("%s: read token user: %w", f.label, err)
	}
	if u.Login == "" {
		return "", fmt.Errorf("%s: token user has no login", f.label)
	}
	return u.Login, nil
}

var _ SweepAPI = (*forgejoForge)(nil)
