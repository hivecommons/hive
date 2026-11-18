package github

import (
	"context"
	"errors"
)

// PRGateSubject is the live state of a pull request as the PR gate reads it.
type PRGateSubject struct {
	HeadSHA string
	Title   string
	// State is GitHub's "open" or "closed" (a merged PR reads closed).
	State string
	Draft bool
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
	return PRGateSubject{HeadSHA: pr.GetHead().GetSHA(), Title: pr.GetTitle(), State: pr.GetState(), Draft: pr.GetDraft()}, nil
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
	owner, repoName := c.splitRepo(repo)
	err := c.createCommitStatus(ctx, owner, repoName, number, sha, statusContext, state, description, prGateStatusCaller)
	if err != nil && isStatusesPermissionDenied(err) {
		return errors.Join(ErrPRGateStatusForbidden, err)
	}
	return err
}
