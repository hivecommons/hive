// Package forge provides a source-forge abstraction so Hive is not hardcoded
// to GitHub. It defines a small, forge-neutral Forge interface and neutral data
// types (Repo, Issue, ChangeRequest), plus concrete adapters for GitHub (a thin
// wrapper over pkg/github) and GitLab (a stdlib net/http client for the REST v4
// API).
//
// The read path (enumerate open issues / merge requests, get a repo) and the
// core write path (comment on an issue/CR, add/remove labels, set a hold gate)
// are implemented and tested for GitHub, GitLab and Gitea/Forgejo. A full merge
// primitive is left as a clearly-marked TODO on the interface (Merge) rather
// than half-implemented, because merge semantics diverge sharply across forges
// (strategy names, gate checks, MR-vs-PR); callers that need to merge today
// continue to use the forge-specific client directly.
package forge

import (
	"context"
	"fmt"
	"time"
)

// Kind identifies a supported forge implementation.
type Kind string

const (
	// KindGitHub selects the GitHub adapter (wraps pkg/github).
	KindGitHub Kind = "github"
	// KindGitLab selects the GitLab adapter (REST API v4 over net/http).
	KindGitLab Kind = "gitlab"
	// KindGitea selects the Gitea/Forgejo adapter (REST API v1 over net/http).
	KindGitea Kind = "gitea"
)

const (
	// holdLabel is the forge-neutral label used by SetHold to gate a change from
	// merging. It matches the "hold" convention already used by the GitHub client.
	holdLabel = "hold"
)

// Repo is a forge-neutral description of a repository / project.
type Repo struct {
	// FullName is the human "owner/name" (GitHub) or "namespace/path" (GitLab)
	// slug that identifies the repo within its forge.
	FullName string `json:"full_name"`
	// Owner is the org/namespace portion of FullName.
	Owner string `json:"owner"`
	// Name is the repository/project name portion of FullName.
	Name string `json:"name"`
	// URL is the canonical web URL of the repo.
	URL string `json:"url"`
	// DefaultBranch is the repo's default branch (may be empty if unknown).
	DefaultBranch string `json:"default_branch,omitempty"`
	// Description is the repo's short description (may be empty).
	Description string `json:"description,omitempty"`
}

// Issue is a forge-neutral issue. It maps GitHub issues and GitLab issues onto
// one shape. Fields absent on a given forge are left at their zero value.
type Issue struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Assignees []string  `json:"assignees"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
}

// ChangeRequest is a forge-neutral pull request (GitHub) / merge request
// (GitLab). The name is deliberately forge-neutral because "pull request" is a
// GitHub term.
type ChangeRequest struct {
	Repo      string    `json:"repo"`
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Author    string    `json:"author"`
	Labels    []string  `json:"labels"`
	Draft     bool      `json:"draft"`
	State     string    `json:"state"`
	CreatedAt time.Time `json:"created_at"`
	URL       string    `json:"url"`
	// SourceBranch / TargetBranch describe the change's direction. GitHub calls
	// these head/base; GitLab calls them source/target. Empty when unknown.
	SourceBranch string `json:"source_branch,omitempty"`
	TargetBranch string `json:"target_branch,omitempty"`
	// HeadSHA is the tip commit of the change (GitHub head SHA / GitLab sha).
	HeadSHA string `json:"head_sha,omitempty"`
}

// MergeOptions is a placeholder for the (not-yet-implemented) Merge primitive.
// Merge semantics diverge across forges — GitHub squash/rebase/merge, GitLab MR
// merge with fast-forward/semi-linear options, Gitea merge styles — so the
// concrete shape is intentionally left for the caller that first needs it.
type MergeOptions struct {
	// Strategy is the forge-specific merge strategy (e.g. "squash", "merge",
	// "rebase"). Empty means "use the forge default".
	Strategy string
}

// Forge is the forge-neutral operation set Hive depends on.
//
// Read path — implemented by the GitHub, GitLab and Gitea adapters:
//   - Kind
//   - GetRepo
//   - ListOpenIssues
//   - ListOpenChangeRequests
//
// Write path — implemented by all three adapters:
//   - CreateIssueComment (comment on an issue or change request)
//   - AddLabels
//   - RemoveLabel
//   - SetHold (add/remove the hold gate label — a forge-neutral merge gate)
//
// A full Merge primitive is deliberately NOT on this interface yet: its options
// (strategy, gate checks) diverge too much across forges to neutralize cleanly.
// See MergeOptions and the TODO on Merger below.
type Forge interface {
	// Kind reports which forge implementation this is.
	Kind() Kind

	// GetRepo fetches a single repo by its forge slug ("owner/name" for GitHub,
	// "namespace/path" for GitLab, "owner/repo" for Gitea).
	GetRepo(ctx context.Context, repo string) (*Repo, error)

	// ListOpenIssues returns the open issues for a repo. On GitHub this excludes
	// pull requests (which the REST API models as issues); on GitLab and Gitea
	// issues and change requests are already separate resources.
	ListOpenIssues(ctx context.Context, repo string) ([]Issue, error)

	// ListOpenChangeRequests returns the open pull requests (GitHub / Gitea) /
	// merge requests (GitLab) for a repo.
	ListOpenChangeRequests(ctx context.Context, repo string) ([]ChangeRequest, error)

	// CreateIssueComment posts a comment on the issue or change request numbered
	// `number` in `repo`. GitHub, GitLab and Gitea all model issue and PR/MR
	// comments through the same endpoint, so a single method covers both.
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error

	// AddLabels adds the given labels to an issue or change request. It is a
	// no-op (returning nil) when labels is empty. Labels that already exist are
	// left as-is by every forge.
	AddLabels(ctx context.Context, repo string, number int, labels []string) error

	// RemoveLabel removes a single label from an issue or change request. Removing
	// a label that is not present is treated as success (the desired end state is
	// reached), so callers need not pre-check.
	RemoveLabel(ctx context.Context, repo string, number int, label string) error

	// SetHold applies (hold=true) or clears (hold=false) the forge-neutral hold
	// gate on a change request. It is the merge-gate primitive: a held change must
	// not be merged. It is implemented on top of AddLabels/RemoveLabel using the
	// conventional "hold" label, so it works uniformly across forges.
	SetHold(ctx context.Context, repo string, number int, hold bool) error
}

// Merger is an OPTIONAL extension interface for the merge primitive. It is kept
// off the core Forge interface because merge options are not yet forge-neutral.
// No adapter implements it today; it exists to pin the intended shape.
//
//	// TODO(forge merge path): implement Merge on each adapter once MergeOptions
//	// stabilizes across GitHub/GitLab/Gitea.
type Merger interface {
	Merge(ctx context.Context, repo string, number int, opts MergeOptions) error
}

// Options configures a forge at construction time. All fields are optional; a
// zero Options yields sensible defaults (public forge endpoints).
type Options struct {
	// BaseURL overrides the forge API base URL. For GitHub this is the REST API
	// URL (e.g. GHE "https://github.example.com/api/v3"); empty uses the public
	// default. For GitLab this is the instance root (e.g.
	// "https://gitlab.example.com"); empty uses "https://gitlab.com". The GitLab
	// adapter appends the "/api/v4" path itself. For Gitea this is the instance
	// root (e.g. "https://gitea.example.com" or "https://codeberg.org"); it has
	// no public default and MUST be supplied. The Gitea adapter appends the
	// "/api/v1" path itself.
	BaseURL string
	// Org is the default owner/namespace used when a repo slug has no "/" prefix.
	Org string
}

// NewForge constructs a Forge of the given kind.
//
// token authenticates against the forge. It must be supplied by the caller
// (typically from an environment variable such as GITHUB_TOKEN / GITLAB_TOKEN);
// this package never reads or defaults secrets itself.
//
// For KindGitHub this builds a thin adapter over pkg/github.NewClient. For
// KindGitLab this builds the stdlib REST v4 adapter.
func NewForge(kind Kind, token string, opts Options) (Forge, error) {
	switch kind {
	case KindGitHub, Kind(""):
		// Empty kind defaults to GitHub so existing GitHub-only configs keep
		// working without naming a forge.
		return newGitHubForge(token, opts), nil
	case KindGitLab:
		return newGitLabForge(token, opts)
	case KindGitea:
		return newGiteaForge(token, opts)
	default:
		return nil, fmt.Errorf("unknown forge kind %q (want %q, %q or %q)", kind, KindGitHub, KindGitLab, KindGitea)
	}
}

// splitRepo splits an "owner/name" slug into its parts, falling back to
// defaultOwner when no "/" is present. It is shared by the adapters.
func splitRepo(repo, defaultOwner string) (owner, name string) {
	for i := 0; i < len(repo); i++ {
		if repo[i] == '/' {
			return repo[:i], repo[i+1:]
		}
	}
	return defaultOwner, repo
}
