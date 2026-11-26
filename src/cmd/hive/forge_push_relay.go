package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/gitidentity"
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
//     scoped to that one remote URL. As on GitHub, a push is refused when a
//     commit in the pushed range is authored by a hive identity without a
//     Signed-off-by trailer (forgeHiveCommitIdentities), and a published
//     branch is audited as agent_branch_pushed (#19960). A checkout whose
//     config could redirect the push (url.*.insteadOf, http.proxy,
//     http.<url>.* and the like) is refused
//     (github.PushBranchConfigRefusal, hivecommons/hive#19959), and the push
//     itself runs from a hive-owned GIT_DIR over the checkout's objects
//     (github.PushBranchIsolatedGitDir), so it never reads that config.
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

// forgePushExecTimeout bounds one git command of the push relay. Push
// requests are served on the same goroutine as the PR and issue queues, so a
// git that never returns (a stalled remote, or a proxy the agent set in its
// checkout's git config) must not stall every queue.
const forgePushExecTimeout = 5 * time.Minute

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
	ctx, cancel := context.WithTimeout(ctx, forgePushExecTimeout)
	defer cancel()
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

// forgeHiveCommitIdentities is the hive's own commit identity on a forge,
// for the push relay's sign-off check (#19960). The GitHub watcher matches
// GitHub logins; on a forge the hive is the configured forge user
// (project.ai_author, or the effective author), matched by author name or by
// the local part of its commit email (Forgejo's "<user>@noreply.<host>"), and
// every lane, which commits as "<agent>@<bot email domain>"
// (gitidentity.AgentIdentity). A hive that still has a usable GitHub App
// gives its lanes the App bot's noreply address instead
// (gitidentity.AppLinkedAgentIdentity), so the bot login is included too.
func forgeHiveCommitIdentities(cfg *config.Config) github.HiveCommitIdentities {
	ids := github.HiveCommitIdentities{EmailDomains: []string{gitidentity.BotEmailDomain()}}
	for _, login := range []string{cfg.Project.AIAuthor, cfg.EffectiveAIAuthor(), cfg.GitHub.BotLogin()} {
		if login = strings.TrimSpace(login); login != "" {
			ids.Logins = append(ids.Logins, login)
		}
	}
	return ids
}

// withPushAudit sets where a published branch's agent_branch_pushed entry
// goes: the same typed audit sink the GitHub client records to. Without one
// the entry is not recorded.
func (r *forgeRequestRelay) withPushAudit(audit func(github.AuditRecord)) *forgeRequestRelay {
	r.pushAudit = audit
	return r
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
		if resp.Branch == "" {
			resp.Branch = req.Branch
		}
		resp.At = r.stamp()
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
	// The lane write allowlist (write_surface.allowlist) applies here as on
	// the GitHub push watcher: it needs no GitHub data.
	if !cfg.AgentMayWrite(req.Agent, github.WriteOpPushBranch) {
		deny(github.WriteAllowlistReason(req.Agent, github.WriteOpPushBranch))
		return
	}
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
	// Commands that read the agent's checkout run without the token; only
	// the push, from a hive-owned GIT_DIR, carries it (#19959).
	env := github.PushBranchGitEnv(req.Dir, remoteURL, "")
	sha, err := forgePushExec(ctx, req.Dir, env, "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		retry(fmt.Sprintf("local branch %q not found in %s: %s", branch, req.Dir, github.FirstOutputLines(sha, forgePushOutputLines)))
		return
	}
	sha = strings.TrimSpace(sha)

	// Every commit a hive identity authors must carry its sign-off before it
	// leaves the checkout, the same refusal as the GitHub push watcher. Hooks
	// are neutralised by PushBranchGitEnv, so the relay checks itself. A
	// failed listing is retried, never treated as "nothing to check".
	log, err := forgePushExec(ctx, req.Dir, env, github.UnsignedHiveCommitLogArgs(sha)...)
	if err != nil {
		retry("listing the commits to push: " + github.FirstOutputLines(log, forgePushOutputLines))
		return
	}
	if reason := github.HiveCommitSignoffRefusal(log, forgeHiveCommitIdentities(cfg)); reason != "" {
		deny(reason)
		return
	}

	// Refuse a checkout whose own config could redirect the push
	// (hivecommons/hive#19959). The push below never reads that config.
	listing, err := forgePushExec(ctx, req.Dir, env, github.PushBranchConfigListArgs()...)
	if err != nil {
		retry(fmt.Sprintf("reading the git config of %s: %s", req.Dir, github.FirstOutputLines(listing, forgePushOutputLines)))
		return
	}
	if reason := github.PushBranchConfigRefusal(listing); reason != "" {
		deny(reason)
		return
	}
	repoInfo, err := forgePushExec(ctx, req.Dir, env, github.PushBranchRepoInfoArgs()...)
	if err != nil {
		retry(fmt.Sprintf("locating the git dir of %s: %s", req.Dir, github.FirstOutputLines(repoInfo, forgePushOutputLines)))
		return
	}
	gitDir, isolation, cleanup, err := github.PushBranchIsolatedGitDir(repoInfo, branch, sha, fileUID)
	if err != nil {
		deny(err.Error())
		return
	}
	defer cleanup()
	pushEnv := append(github.PushBranchGitEnv(gitDir, remoteURL, token), isolation...)
	args := []string{"push"}
	if req.ForceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remoteURL, "refs/heads/"+branch+":refs/heads/"+branch)
	if out, err := forgePushExec(ctx, gitDir, pushEnv, args...); err != nil {
		retry("git push failed: " + github.FirstOutputLines(out, forgePushOutputLines))
		return
	}
	if r.pushAudit != nil {
		r.pushAudit(github.AgentBranchPushedAuditRecord(req.Agent, req.Repo, branch, sha, req.ForceWithLease))
	}
	result(github.PushBranchResponse{OK: true, Branch: branch, SHA: sha, Attempts: attempt})
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
