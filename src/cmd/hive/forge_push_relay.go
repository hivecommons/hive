package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hivecommons/hive/crux/pushbroker"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/forge"
	"github.com/hivecommons/hive/pkg/github"
)

// The forge relay's push-branch, review and merge queues
// (hivecommons/hive#19916).
//
// On a GitHub hive these queues are served by *github.Client watchers, which
// never start without a usable GitHub App. A forge-only hive has none, so an
// agent outside a sandbox Job kick had no way to publish a branch (its
// FORGEJO_TOKEN is stripped from the pane, #19498) and the PR relay had no
// head to open a PR from.
//
//   - push-requests are served here. The checks are the GitHub watcher's that
//     do not read GitHub data: the manager's push authorizer (file owner UID
//     and the CanPush ACMM gate), repo managed / off GitHub / not paused / in
//     the agent's scope, a branch name that is safe in a refspec, a checkout
//     owned by the requesting agent, and never the repo's default branch. The
//     push goes to the repo's forgeRemoteResolver remote, the same host and
//     token minter a sandbox kick pushes with, at the contributor tier. Git
//     runs with the checkout's hooks, fsmonitor and credential helpers
//     neutralised (github.PushBranchGitEnv) and the token only as a header
//     scoped to that one remote URL.
//   - review-requests and merge-requests are NOT served on a forge-only hive
//     yet. Each is answered with a reason and renamed .rejected, so the agent
//     stops waiting instead of polling a queue no relay reads.

// forgePushRelayTier is the token tier a relayed push mints for. The push
// authorizer has already required CanPush, which is the contributor tier's
// capability; no relayed push needs more.
const forgePushRelayTier = pushbroker.DefaultTier

// forgePushOutputLines is how many lines of git's output a failed push quotes
// in its result.
const forgePushOutputLines = 3

// forgeRelayReviewUnsupported and forgeRelayMergeUnsupported are the reasons a
// forge-only hive gives for the request kinds it does not serve.
const (
	forgeRelayReviewUnsupported = "review requests are not served on a forge-only hive yet; post the review text as a comment through the issue relay (kind \"comment\") instead"
	forgeRelayMergeUnsupported  = "merge requests are not served on a forge-only hive yet; the forge's automerge sweep merges eligible pull requests, or a human merges them"
)

// forgePushExec runs one git command for the push relay and returns its
// combined output. Inherited forge and GitHub credentials are stripped from
// the environment (pushbroker.PushEnv) before env is appended. It is a
// variable so tests run without a git binary or network.
var forgePushExec = func(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = dir
	cmd.Env = append(pushbroker.PushEnv(os.Environ()), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

// forgeRepoDefaultBranch returns repo's default branch on its forge. It is a
// variable so tests can substitute a fake.
var forgeRepoDefaultBranch = func(ctx context.Context, cfg *config.Config, repo string) (string, error) {
	l, err := forgeListerForRepo(cfg, repo)
	if err != nil {
		return "", err
	}
	g, ok := l.(interface {
		GetRepo(ctx context.Context, repo string) (*forge.Repo, error)
	})
	if !ok {
		return "", fmt.Errorf("the %s adapter cannot read repos", cfg.ForgeKindForRepo(repo))
	}
	r, err := g.GetRepo(ctx, repo)
	if err != nil {
		return "", err
	}
	if r == nil || strings.TrimSpace(r.DefaultBranch) == "" {
		return "", fmt.Errorf("the forge reported no default branch for %s", repo)
	}
	return r.DefaultBranch, nil
}

// withPushRelay turns on the push-branch queue with authz, the manager's push
// authorizer. A relay without it still answers push requests, and denies
// every one (fail closed).
func (r *forgeRequestRelay) withPushRelay(authz github.PushBranchRequestAuthorizer) *forgeRequestRelay {
	r.pushAuthz = authz
	return r
}

// ensureQueueDirs creates the push, review and merge queues once, the first
// time the relay finds a forge-only config, so an agent's request lands and
// is answered instead of failing to write. The PR and issue queues are
// created at boot (github.PrepareRequestDirs).
func (r *forgeRequestRelay) ensureQueueDirs() {
	if r.queuesReady {
		return
	}
	ready := true
	for kind, dir := range map[string]string{"push": r.pushDir, "review": r.reviewDir, "merge": r.mergeDir} {
		if dir != "" && !github.EnsureRequestDir(r.logger, kind, dir) {
			ready = false
		}
	}
	r.queuesReady = ready
}

func (r *forgeRequestRelay) handlePushBranch(ctx context.Context, path string) {
	var req github.PushBranchRequest
	result := func(resp github.PushBranchResponse) {
		resp.Branch, resp.At = req.Branch, r.stamp()
		writeRelayResult(path, resp)
	}
	fail := func(msg string) { result(github.PushBranchResponse{Error: msg}) }
	if !r.readRequest(path, &req, fail) {
		return
	}
	deny := func(reason string) {
		fail("authorization denied: " + reason)
		r.settle(path, ".denied", false)
		r.logger.Warn("forge request relay: push DENIED (policy)", "agent", req.Agent, "repo", req.Repo, "branch", req.Branch, "reason", reason)
	}
	reject := func(reason string) {
		fail("push rejected: " + reason)
		r.settle(path, ".rejected", false)
		r.logger.Warn("forge request relay: push REJECTED", "agent", req.Agent, "repo", req.Repo, "branch", req.Branch, "reason", reason)
	}
	if r.pushAuthz == nil {
		deny("no authorizer configured (fail closed)")
		return
	}
	fileUID := github.RequestFileOwnerUID(path)
	if err := r.pushAuthz(req.Agent, fileUID); err != nil {
		deny(err.Error())
		return
	}
	cfg := r.cfg()
	if reason := repoRefusal(cfg, req.Agent, req.Repo); reason != "" {
		reject(reason)
		return
	}
	branch := strings.TrimPrefix(strings.TrimSpace(req.Branch), "refs/heads/")
	if !github.ValidPushBranchName(branch) {
		reject(fmt.Sprintf("branch %q is not a valid branch name for the push relay", req.Branch))
		return
	}
	if err := github.ValidatePushBranchDir(req.Dir, fileUID); err != nil {
		reject(err.Error())
		return
	}
	// The "owner/name" shape forge URLs are built from.
	repo := fullRepoName(strings.TrimSpace(req.Repo), cfg.Project.Org)
	if r.pushRemote == nil {
		reject("no forge remote resolver configured (fail closed)")
		return
	}
	remote, ok, err := r.pushRemote(repo)
	if err != nil || !ok {
		reason := fmt.Sprintf("repo %s has no forge remote this hive can push to", repo)
		if err != nil {
			reason = err.Error()
		}
		reject(reason)
		return
	}
	remoteURL := pushbroker.ForgeRepoURL(remote.BaseURL, repo)
	if remoteURL == "" {
		reject(fmt.Sprintf("repo %s has no usable push URL on %s", repo, remote.BaseURL))
		return
	}
	attempt := r.attempts[path] + 1
	retry := func(msg string) {
		result(github.PushBranchResponse{Attempts: attempt, Error: msg})
		r.settle(path, "", true)
		r.logger.Warn("forge request relay: push failed, will retry", "repo", repo, "branch", branch, "attempt", attempt, "error", msg)
	}

	// Never the default branch: agents propose changes through PRs. A failed
	// lookup is retried, never treated as "not the default branch".
	def, err := forgeRepoDefaultBranch(ctx, cfg, req.Repo)
	if err != nil {
		retry("reading the default branch: " + err.Error())
		return
	}
	if strings.EqualFold(branch, def) {
		reject(fmt.Sprintf("branch %q is the default branch of %s; the push relay only publishes topic branches", branch, repo))
		return
	}

	token, err := pushbroker.WithTier(remote.Minter, forgePushRelayTier).MintPushToken(ctx, repo)
	if err != nil {
		retry("minting push token: " + err.Error())
		return
	}
	env := github.PushBranchGitEnv(req.Dir, remoteURL, token)
	sha, err := forgePushExec(ctx, req.Dir, env, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		retry(fmt.Sprintf("local branch %q not found in %s: %s", branch, req.Dir, github.FirstOutputLines(sha, forgePushOutputLines)))
		return
	}
	sha = strings.TrimSpace(sha)
	args := []string{"push"}
	if req.ForceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remoteURL, "refs/heads/"+branch+":refs/heads/"+branch)
	if out, err := forgePushExec(ctx, req.Dir, env, args...); err != nil {
		retry("git push failed: " + github.FirstOutputLines(out, forgePushOutputLines))
		return
	}
	result(github.PushBranchResponse{OK: true, SHA: sha, Attempts: attempt})
	r.settle(path, "", false)
	r.logger.Info("forge request relay: branch pushed", "agent", req.Agent, "repo", repo, "branch", branch, "sha", sha, "force_with_lease", req.ForceWithLease)
}

// handleReviewRequest answers a review request a forge-only hive does not
// serve, so the agent stops waiting on it.
func (r *forgeRequestRelay) handleReviewRequest(path string) {
	var req github.ReviewRequest
	fail := func(msg string) {
		writeRelayResult(path, github.ReviewResponse{OK: false, Number: req.Number, Error: msg, At: r.stamp()})
	}
	if !r.readRequest(path, &req, fail) {
		return
	}
	fail(forgeRelayReviewUnsupported)
	r.settle(path, ".rejected", false)
	r.logger.Warn("forge request relay: review request REJECTED (not served on a forge-only hive)", "agent", req.Agent, "repo", req.Repo, "number", req.Number)
}

// handleMergeRequest answers a merge request a forge-only hive does not
// serve, so the agent stops waiting on it.
func (r *forgeRequestRelay) handleMergeRequest(path string) {
	var req github.MergeRequest
	fail := func(msg string) {
		writeRelayResult(path, github.MergeResponse{OK: false, Number: req.Number, Error: msg, At: r.stamp()})
	}
	if !r.readRequest(path, &req, fail) {
		return
	}
	fail(forgeRelayMergeUnsupported)
	r.settle(path, ".rejected", false)
	r.logger.Warn("forge request relay: merge request REJECTED (not served on a forge-only hive)", "agent", req.Agent, "repo", req.Repo, "number", req.Number)
}
