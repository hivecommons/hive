package github

import (
	"context"
	"errors"

	"github.com/hivecommons/hive/pkg/prgate"
)

// PermissionStatuses is the installation permission name for Commit statuses.
const PermissionStatuses = "statuses"

// PRGateSubject is the live state of a pull request as the PR gate reads it.
type PRGateSubject struct {
	HeadSHA string
	Title   string
	// State is GitHub's "open" or "closed" (a merged PR reads closed).
	State string
	Draft bool
	// HeadRepo is the head repository's owner/name ("" when the fork was
	// deleted); Author is the PR author's login. The fork-head canary reads
	// them to tell same-repo, fork and Dependabot heads apart.
	HeadRepo string
	Author   string
}

// prGateStatusCaller names the PR gate's status writes for REST accounting.
const prGateStatusCaller = "hive:pr_gate_status"

// ReadPRGateSubject re-reads a PR straight from GitHub. It never answers from
// the PR-detail cache: the gate posts on the head it reads here, and a stale
// head would put a verdict on a commit that is no longer the PR's.
func (c *Client) ReadPRGateSubject(ctx context.Context, repo string, number int) (PRGateSubject, error) {
	if c == nil {
		return PRGateSubject{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:pr_gate_read"), owner, repoName, number)
	if err != nil {
		return PRGateSubject{}, err
	}
	return PRGateSubject{HeadSHA: pr.GetHead().GetSHA(), Title: pr.GetTitle(), State: pr.GetState(), Draft: pr.GetDraft(),
		HeadRepo: pr.GetHead().GetRepo().GetFullName(), Author: pr.GetUser().GetLogin()}, nil
}

// ErrPRGateStatusForbidden marks a gate status write that GitHub refused with
// 403: the installation lacks Commit statuses read and write.
var ErrPRGateStatusForbidden = errors.New("commit status write forbidden")

// PostPRGateStatus writes one gate commit status through the shared status
// writer (the one behind the prow/lgtm binding). It does NOT choose the
// context: the caller (pkg/prgate) enforces the context allowlist. A 403 is
// returned wrapped in ErrPRGateStatusForbidden; nothing is retried here.
func (c *Client) PostPRGateStatus(ctx context.Context, repo string, number int, sha, statusContext, state, description string) error {
	if c == nil {
		return ErrNoGitHubClient
	}
	// Defense in depth: the gate already refuses any context off the
	// allowlist, and so does the writer that all of them go through.
	if err := prgate.PostableContext(statusContext); err != nil {
		return err
	}
	owner, repoName := c.splitRepo(repo)
	err := c.createCommitStatus(ctx, owner, repoName, number, sha, statusContext, state, description, prGateStatusCaller)
	if err != nil && isStatusesPermissionDenied(err) {
		return errors.Join(ErrPRGateStatusForbidden, err)
	}
	return err
}

// PRGateGrant is the installation's Commit statuses grant as the PR gate's
// grant check needs it.
type PRGateGrant struct {
	// Applicable is false when the client holds no GitHub App credential (a
	// personal access token has no installation grants to read).
	Applicable   bool
	Installation string
	// Level is the granted Commit statuses permission: "read", "write",
	// "admin" or "" when not granted.
	Level string
}

// ReadPRGateGrant reads the Commit statuses permission the Hive App
// installation was actually granted. An owner must accept a permission
// change before an installation holds it, so the App's requested permissions
// say nothing: this reads the installation itself.
func (c *Client) ReadPRGateGrant(ctx context.Context) (PRGateGrant, error) {
	if c == nil {
		return PRGateGrant{}, ErrNoGitHubClient
	}
	if c.appAuth == nil || !c.appAuth.HasKey() {
		return PRGateGrant{}, nil
	}
	info, err := c.appAuth.VerifyInstallation(ctx)
	if err != nil {
		return PRGateGrant{}, err
	}
	return PRGateGrant{Applicable: true, Installation: info.Account, Level: info.Permissions[PermissionStatuses]}, nil
}
