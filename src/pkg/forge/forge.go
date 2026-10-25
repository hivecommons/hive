// Package forge provides a source-forge abstraction so Hive is not hardcoded
// to GitHub. It defines a small, forge-neutral Forge interface and neutral data
// types (Repo, Issue, ChangeRequest), plus concrete adapters for GitHub (a thin
// wrapper over pkg/github) and GitLab (a stdlib net/http client for the REST v4
// API).
//
// The read path (enumerate open issues / merge requests, get a repo) and the
// core write path (comment on an issue/CR, add/remove labels, set a hold gate)
// are implemented and tested for GitHub, GitLab and Gitea/Forgejo. The merge
// primitive is the OPTIONAL Merger extension: MergeOptions neutralizes the
// parts that diverge sharply across forges (strategy names, the expected head
// commit, the label gate), and every adapter implementing it must pass the
// shared contract in merge_contract_test.go. GitHub and Gitea implement
// Merger today; GitLab does not yet, and a caller type-asserts for Merger
// rather than assuming it.
package forge

import (
	"context"
	"errors"
	"fmt"
	"strings"
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

// MergeStrategy is the forge-neutral name of HOW a change request is
// integrated. Adapters map these onto their own forge's vocabulary (GitHub's
// merge_method, GitLab's squash/ff flags, Gitea's merge style) and refuse a
// value they cannot express with ErrMergeStrategyUnsupported, so a caller
// never silently gets a different history shape than it asked for.
type MergeStrategy string

const (
	// MergeStrategyDefault leaves the shape to the adapter's forge default.
	MergeStrategyDefault MergeStrategy = ""
	// MergeStrategySquash collapses the change into a single commit.
	MergeStrategySquash MergeStrategy = "squash"
	// MergeStrategyMerge records a merge commit, keeping the change's own
	// history — what a forward-merge between release lines requires.
	MergeStrategyMerge MergeStrategy = "merge"
	// MergeStrategyRebase replays the change's commits onto the target branch.
	MergeStrategyRebase MergeStrategy = "rebase"
)

// Merge refusals every adapter reports with the SAME sentinel, so a caller
// distinguishes "the gate says no" from "the head moved" from a transport
// failure without parsing forge-specific messages. Each is wrapped with the
// concrete repo/number/label detail; match with errors.Is.
var (
	// ErrMergeGated means a gate label on the change refuses the merge: the
	// hold label SetHold writes, or one of MergeOptions.BlockingLabels.
	ErrMergeGated = errors.New("forge: merge refused by a gate label")
	// ErrMergeHeadMismatch means the change's head is not the commit the
	// caller decided on — either seen before the merge or reported by the
	// forge when the head moved underneath it.
	ErrMergeHeadMismatch = errors.New("forge: change head is not the expected commit")
	// ErrMergeStrategyUnsupported means Strategy is not a MergeStrategy this
	// forge can express.
	ErrMergeStrategyUnsupported = errors.New("forge: merge strategy unsupported by this forge")
	// ErrMergeConflict means the change request cannot be integrated due to
	// merge conflicts.
	ErrMergeConflict = errors.New("forge: change request has merge conflicts")
	// ErrMergeNotMergeable means the forge refuses to merge because the change
	// request is not in a mergeable state (e.g. conflicts or failing checks).
	ErrMergeNotMergeable = errors.New("forge: change request is not mergeable")
)

// MergeOptions is the forge-neutral input to Merger.Merge: what shape to
// integrate in, which commit the caller decided on, and which labels gate it.
// A zero value is valid — forge-default strategy, no head pin, and the hold
// gate still enforced.
type MergeOptions struct {
	// Strategy is the forge-neutral merge strategy; empty uses the forge
	// default. An unknown value is refused, never silently defaulted.
	Strategy MergeStrategy
	// ExpectedHeadSHA, when non-empty, pins the commit the caller's decision
	// was made against. The adapter refuses with ErrMergeHeadMismatch when the
	// change's head has moved, and passes the pin to forges that enforce it
	// server-side, so a push racing the merge can never land unreviewed work.
	ExpectedHeadSHA string
	// BlockingLabels are EXTRA gate labels that refuse the merge, on top of
	// the hold label SetHold writes (which is always enforced — a forge where
	// hold did not mean hold would not be forge-neutral). Matching is
	// case-insensitive and whitespace-trimmed; an empty entry is ignored.
	BlockingLabels []string
	// DeleteBranch, when true, requests that the head/source branch be deleted
	// after a successful merge on forges that support it.
	DeleteBranch bool
}

// MergeResult is the forge-neutral outcome of a merge.
type MergeResult struct {
	// Merged reports that the change is merged on the forge after this call,
	// whether this call performed the merge or found it already done.
	Merged bool
	// SHA is the resulting merge commit, when the forge reports one.
	SHA string
	// AlreadyMerged reports that the change was ALREADY merged, so this call
	// performed no merge. The merge intent still holds, so it is success:
	// a retried merge request must not fail on its second pass.
	AlreadyMerged bool
}

// gateLabel returns the first label on a change that refuses the merge, or ""
// when none does. Every adapter evaluates the gate here rather than in its own
// merge method, so "hold means hold" cannot drift between forges.
func (o MergeOptions) gateLabel(labels []string) string {
	for _, have := range labels {
		have = strings.TrimSpace(have)
		if have == "" {
			continue
		}
		if strings.EqualFold(have, holdLabel) {
			return have
		}
		for _, blocked := range o.BlockingLabels {
			if blocked = strings.TrimSpace(blocked); blocked != "" && strings.EqualFold(have, blocked) {
				return have
			}
		}
	}
	return ""
}

// IssueWriter is the narrow write seam production callers are typed against.
//
// It is the "post the evidence, then label it" pair — the only forge writes on
// Hive's scheduler/governor path — split out of Forge so a caller can depend on
// the abstraction WITHOUT forcing every hive through an adapter. *github.Client
// already carries these two methods with exactly these signatures (its own doc
// comments say they mirror this interface for precisely this swap), so a GitHub
// hive keeps running on the concrete client it always used — same code path,
// same behavior — while a GitLab or Gitea hive gets the matching adapter at the
// same call site.
//
// Every Forge is an IssueWriter: Forge embeds this interface.
type IssueWriter interface {
	// CreateIssueComment posts a comment on the issue or change request numbered
	// `number` in `repo`. GitHub, GitLab and Gitea all model issue and PR/MR
	// comments through the same endpoint, so a single method covers both.
	CreateIssueComment(ctx context.Context, repo string, number int, body string) error

	// AddLabels adds the given labels to an issue or change request. It is a
	// no-op (returning nil) when labels is empty. Labels that already exist are
	// left as-is by every forge.
	AddLabels(ctx context.Context, repo string, number int, labels []string) error
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
//   - CreateIssueComment (comment on an issue or change request) — via IssueWriter
//   - AddLabels — via IssueWriter
//   - RemoveLabel
//   - SetHold (add/remove the hold gate label — a forge-neutral merge gate)
//
// Merge is deliberately NOT on this interface: it is the OPTIONAL Merger
// extension, so an adapter that can only read and label still satisfies Forge.
// See Merger, MergeOptions and MergeResult below.
type Forge interface {
	// The forge-neutral write seam (CreateIssueComment, AddLabels). Embedded so
	// any Forge can be handed to a caller typed against IssueWriter alone.
	IssueWriter

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

// IssueRef is the bounded provenance of a created or found issue: enough to
// link and audit, never the issue content.
type IssueRef struct {
	Number int
	URL    string
}

// IssueCreator is the OPTIONAL forge-neutral issue-create seam the audit
// campaign's authorized publisher (#8353) files through. It is kept off the
// core Forge interface because only the GitHub adapter implements it today;
// a caller that needs it type-asserts and refuses cleanly when absent.
type IssueCreator interface {
	// CreateIssue files a new issue in repo and returns its reference. The
	// publisher's mutation journal and marker lookup are its idempotency
	// guards; the GitHub seam additionally reuses an open issue with the same
	// subject or file set rather than twinning it (#11578).
	CreateIssue(ctx context.Context, repo, title, body string, labels []string) (IssueRef, error)
}

// IssueFinder is the OPTIONAL lookup seam the publisher uses to reconcile an
// uncertain create and to dedupe against issues already carrying a finding
// marker, so a crash between intent and acknowledgment never files twice.
type IssueFinder interface {
	// FindIssueByMarker returns the open issue in repo whose body contains the
	// exact marker text, or ok=false when none does. It is bounded to recent
	// issues; an error means the observation was not authoritative.
	FindIssueByMarker(ctx context.Context, repo, marker string) (ref IssueRef, ok bool, err error)
}

// IssueSeam is the pair the publisher is typed against.
type IssueSeam interface {
	IssueCreator
	IssueFinder
}

// Merger is the OPTIONAL extension interface for the merge primitive. It is
// kept off the core Forge interface because not every adapter implements it:
// a caller type-asserts (`m, ok := f.(Merger)`) and refuses cleanly when the
// forge it was handed cannot merge.
//
// Implemented by: GitHub, Gitea. Pending: GitLab.
//
// Every implementation MUST pass the shared contract in merge_contract_test.go
// — one table of forge-neutral cases run against an httptest fake per forge —
// so the behaviour below is identical on every forge rather than per-adapter
// folklore:
//
//   - the strategy map is honoured, and an unknown MergeStrategy is refused
//     with ErrMergeStrategyUnsupported rather than silently defaulted;
//   - ExpectedHeadSHA is enforced: a moved head refuses with
//     ErrMergeHeadMismatch and merges nothing;
//   - a gate label (hold, or MergeOptions.BlockingLabels) refuses with
//     ErrMergeGated and merges nothing;
//   - an already-merged change is idempotent success (AlreadyMerged), not an
//     error, so a retried merge request never fails on its second pass.
type Merger interface {
	Merge(ctx context.Context, repo string, number int, opts MergeOptions) (MergeResult, error)
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
