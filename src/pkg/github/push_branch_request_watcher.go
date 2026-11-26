package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// PushBranchRequestDir is where agents drop push-branch requests. An agent that
// wants its local branch on GitHub writes a request file here INSTEAD of
// running `git push` itself (hivecommons/hive#9771): the hive performs the push
// with the App token, after the same file-UID authorizer, lane allowlist, repo
// pause and repo scope checks as every other relay operation, and audits it as
// agent_branch_pushed. This is the relay that makes sandbox write enforcement
// (#9772) possible — a lane whose direct pushes are refused still has an
// audited way to publish its branches.
const PushBranchRequestDir = "/var/run/hive-metrics/push-requests"

// pushBranchRequestDirForTest lets tests point the watcher at a temp dir.
// Empty means use PushBranchRequestDir. Not for production use.
var pushBranchRequestDirForTest string

func pushBranchRequestDir() string {
	if pushBranchRequestDirForTest != "" {
		return pushBranchRequestDirForTest
	}
	return PushBranchRequestDir
}

// pushBranchRequestPollInterval is how often the watcher scans
// PushBranchRequestDir. A push is latency-sensitive (the agent usually opens a
// PR right after), but the same rate-limit caution as the merge watcher
// applies, so keep it on the same modest tick.
var pushBranchRequestPollInterval = 10 * time.Second

// pushBranchMaxAttempts bounds retries on a failed push before the request is
// quarantined, same rationale as mergeRequestMaxAttempts: a push that is
// genuinely refused (non-fast-forward, protected branch, missing local branch)
// must not be retried forever.
const pushBranchMaxAttempts = 3

// PushBranchRequest is the JSON an agent writes to PushBranchRequestDir to ask
// the hive to push a local branch on its behalf. Repo may be "owner/repo" or a
// bare repo name. Dir is the agent's local checkout containing the branch.
type PushBranchRequest struct {
	Repo   string `json:"repo"`
	Branch string `json:"branch"`
	Dir    string `json:"dir"`
	Agent  string `json:"agent,omitempty"`
	// ForceWithLease, when true, pushes with --force-with-lease so a reworked
	// branch can replace its own previous head. Never a plain --force: the
	// lease fails the push if the remote moved under someone else's commit.
	ForceWithLease bool `json:"force_with_lease,omitempty"`
}

// PushBranchResponse is written back next to a consumed request (as
// <name>.result.json) so the agent — or an operator debugging — can see what
// happened.
type PushBranchResponse struct {
	OK       bool   `json:"ok"`
	Branch   string `json:"branch,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

// PushBranchRequestAuthorizer decides whether a push-branch request may
// proceed. Like PRRequestAuthorizer it receives the claimed agent NAME and the
// UID that OWNS the request file; the caller implements the same two checks
// (forge-resistance via the uid-map, and the ACMM write-gate — pushing a
// branch is the same CanPush tier as opening a PR). Returning nil authorizes.
// A nil authorizer DENIES everything (fail closed).
type PushBranchRequestAuthorizer func(agent string, fileUID int) error

// pushBranchExec runs the watcher's git commands. Package-level seam so tests
// exercise the watcher without a git binary or network.
type pushBranchExecFunc func(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)

var pushBranchExec pushBranchExecFunc = runPushBranchCommand

// pushBranchExecTimeout bounds one git command of the push watcher, so a
// checkout that makes git block (a FIFO behind include.path, a stalled
// remote) cannot stall every later request.
const pushBranchExecTimeout = 5 * time.Minute

func runPushBranchCommand(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, pushBranchExecTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return out.String(), err
}

func (c *Client) gitAuthHeader(ctx context.Context) (string, bool) {
	if c == nil {
		return "", false
	}
	if c.appAuth != nil {
		token, err := c.appAuth.Token(ctx)
		if err == nil && token != "" {
			return gitBasicAuthHeader(token), true
		}
		return "", false
	}
	if c.authToken != "" {
		return gitBasicAuthHeader(c.authToken), true
	}
	return "", false
}

// gitBasicAuthHeader encodes a GitHub token the way git-over-HTTPS expects it:
// Basic auth with the conventional x-access-token username.
func gitBasicAuthHeader(token string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+token))
}

func firstOutputLines(output string, n int) string {
	lines := strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n")
	var picked []string
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		picked = append(picked, trimmed)
		if len(picked) >= n {
			break
		}
	}
	return strings.Join(picked, "\n")
}

// pushBranchNamePattern is the shape of a branch name the relay accepts. It is
// deliberately narrower than git's own rules: a leading alphanumeric keeps a
// name from being read as a flag, and the charset keeps whitespace, colons
// (refspec separators) and control characters out of the refspec entirely.
var pushBranchNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]*$`)

// validPushBranchName reports whether branch is a name the relay will put in a
// refspec. ".." is rejected explicitly (legal charset, illegal in a ref), as
// are ".lock" suffixes and a trailing "/" or ".".
func validPushBranchName(branch string) bool {
	if branch == "" || !pushBranchNamePattern.MatchString(branch) {
		return false
	}
	if strings.Contains(branch, "..") || strings.Contains(branch, "//") {
		return false
	}
	if strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") || strings.HasSuffix(branch, ".lock") {
		return false
	}
	return true
}

// validatePushBranchDir checks the checkout the request names. The dir is
// agent-supplied, so it gets the same forge posture as the request file
// itself: when per-agent UIDs are in play (fileUID > 0), the directory must be
// OWNED by the requesting agent's UID — otherwise one agent could push another
// agent's (or the hive's own) working tree under its own lane. When ownership
// is unverifiable (fileUID <= 0, shared-dev-UID mode), only the structural
// checks apply, the same fallback the authorizers take.
func validatePushBranchDir(dir string, fileUID int) error {
	if strings.TrimSpace(dir) == "" {
		return errors.New("the request names no checkout directory (dir)")
	}
	if !filepath.IsAbs(dir) || strings.Contains(dir, "..") {
		return fmt.Errorf("checkout dir %q must be an absolute path without \"..\"", dir)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return fmt.Errorf("checkout dir %q: %w", dir, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("checkout dir %q is not a directory", dir)
	}
	if fileUID > 0 {
		if owner := fileOwnerUID(fi); owner != fileUID {
			return fmt.Errorf("checkout dir %q is owned by uid %d, not by the requesting agent (uid %d) — an agent may only push its own working tree", dir, owner, fileUID)
		}
	}
	return nil
}

// pushBranchRemoteURL is the HTTPS remote the watcher pushes to. Always built
// from the validated owner/name pair, never taken from the request or the
// checkout's own remotes: the repo the allowlist, pause and scope checks
// approved is the repo the push reaches.
func pushBranchRemoteURL(owner, repo string) string {
	return "https://github.com/" + owner + "/" + repo + ".git"
}

// pushBranchGitEnv builds the environment for one git invocation: no terminal
// prompts, the checkout marked safe (it is owned by the agent's UID, not the
// hive's), and the App token as an extraHeader on the remote — a process-local
// Git config overlay, so the credential never touches
// the agent-owned working tree's config.
//
// Because the watcher runs git inside a tree owned by the requesting agent's
// UID, every repo-local channel that makes git execute a program must be
// neutralized: hooks (pre-push runs on every push), core.fsmonitor, and
// credential helpers are all configurable from the agent-writable .git
// directory and would otherwise run as the hive's UID with this environment —
// including the App token header — visible to them. GIT_CONFIG_* pairs have
// command-line precedence, so they override anything set in the checkout.
//
// The same holds for the keys that decide where the credentialed request goes
// (hivecommons/hive#19959): system and global config are turned off, askpass
// programs are unset in the environment and in config, TLS verification and
// git's default redirect policy are pinned, signing (which runs gpg.program)
// is off for push and log, submodules are never pushed, and git may only
// speak the remote's own scheme. Keys no command-scope value can neutralise
// (url.*.insteadOf, http.<url>.*, http.proxy and the TLS trust settings) are
// refused before the push by pushBranchConfigRefusal.
func pushBranchGitEnv(dir, remote, header string) []string {
	env := []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_SYSTEM=" + os.DevNull,
		"GIT_CONFIG_GLOBAL=" + os.DevNull,
		"GIT_ASKPASS=",
		"SSH_ASKPASS=",
		// A missing object in a partial clone would otherwise start a
		// fetch from the checkout's promisor remote, with its proxy.
		"GIT_NO_LAZY_FETCH=1",
	}
	if scheme := pushBranchRemoteScheme(remote); scheme != "" {
		env = append(env, "GIT_ALLOW_PROTOCOL="+scheme)
	}
	pairs := [][2]string{
		{"safe.directory", dir},
		{"core.hooksPath", os.DevNull},
		{"core.fsmonitor", "false"},
		{"credential.helper", ""},
		{"core.askPass", ""},
		{"http.sslVerify", "true"},
		{"http.followRedirects", "initial"},
		{"push.recurseSubmodules", "no"},
		{"push.gpgSign", "false"},
		{"log.showSignature", "false"},
	}
	if header != "" {
		pairs = append(pairs, [2]string{"http." + strings.TrimRight(remote, "/") + ".extraHeader", header})
	}
	env = append(env, "GIT_CONFIG_COUNT="+strconv.Itoa(len(pairs)))
	for i, p := range pairs {
		env = append(env,
			fmt.Sprintf("GIT_CONFIG_KEY_%d=%s", i, p[0]),
			fmt.Sprintf("GIT_CONFIG_VALUE_%d=%s", i, p[1]))
	}
	return env
}

// pushBranchRemoteScheme is the lowercased scheme of an http(s) remote URL,
// or "" for anything else (then GIT_ALLOW_PROTOCOL is left to git's default).
func pushBranchRemoteScheme(remote string) string {
	scheme, _, ok := strings.Cut(remote, "://")
	if !ok {
		return ""
	}
	switch scheme = strings.ToLower(scheme); scheme {
	case "https", "http":
		return scheme
	}
	return ""
}

// StartPushBranchRequestWatcher runs a loop that pushes branches for request
// files dropped in PushBranchRequestDir. Same contract as
// StartMergeRequestWatcher: it returns immediately, the loop runs until ctx is
// cancelled, a nil client is a no-op, a nil authz fails closed, nowFn is
// injectable for tests (nil for time.Now), and the returned channel closes
// when the watcher goroutine has exited so callers can JOIN the loop.
func (c *Client) StartPushBranchRequestWatcher(ctx context.Context, authz PushBranchRequestAuthorizer, nowFn func() time.Time) <-chan struct{} {
	done := make(chan struct{})
	if c == nil {
		close(done)
		return done
	}
	c.pushBranchAuthz = authz
	if nowFn == nil {
		nowFn = time.Now
	}
	// Agents must be able to DROP request files here (hive-push-branch runs AS
	// the agent) — same group-write + setgid + sticky posture as every other
	// request queue; the forge check still reads each file's OWNING UID.
	if !ensureRequestDir(c.logger, "push", pushBranchRequestDir()) {
		close(done)
		return done
	}
	// Captured before spawning for the same race-with-test-cleanup reason as
	// the merge watcher.
	interval := pushBranchRequestPollInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				if ctx.Err() != nil {
					return
				}
				c.processPushBranchRequests(ctx, nowFn)
			}
		}
	}()
	c.logger.Info("push-branch-request watcher started", slog.String("dir", pushBranchRequestDir()))
	return done
}

func (c *Client) processPushBranchRequests(ctx context.Context, nowFn func() time.Time) {
	entries, err := os.ReadDir(pushBranchRequestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		path := filepath.Join(pushBranchRequestDir(), name)
		c.handleOnePushBranchRequest(ctx, path, nowFn)
	}
}

// ProcessPushBranchRequestsOnce runs a single scan+process pass. Test/CLI entry point.
func (c *Client) ProcessPushBranchRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processPushBranchRequests(ctx, time.Now)
}

func (c *Client) handleOnePushBranchRequest(ctx context.Context, path string, nowFn func() time.Time) {
	data, _, err := readUntrustedFile(path, requestMaxBytes)
	if err != nil {
		if errors.Is(err, errDropBoxFileRejected) {
			_ = os.Rename(path, path+".rejected")
			c.logger.Warn("push-branch-request watcher: REJECTED (unsafe file)",
				slog.String("path", path), slog.String("reason", err.Error()))
		}
		return // vanished between ReadDir and here — fine
	}
	var req PushBranchRequest
	if err := json.Unmarshal(data, &req); err != nil {
		// A torn read is not a malformed request: leave it for the next tick
		// rather than destroying it. See quarantinable.
		if !quarantinable(path, nowFn()) {
			return
		}
		c.writePushBranchResult(path, PushBranchResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.logger.Warn("push-branch-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	// AUTHORIZE before pushing — the same forge-resistance + CanPush ACMM gate
	// a direct push would need. A nil authorizer fails closed.
	fileUID := statUID(data, path)
	if c.pushBranchAuthz == nil {
		c.denyPushBranchRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.pushBranchAuthz(req.Agent, fileUID); err != nil {
		c.denyPushBranchRequest(path, req, err.Error(), nowFn)
		return
	}
	// Lane write allowlist (#9587), keyed on the now-authorized agent name. A
	// branch push has no issue or PR number, so the refusal's typed target is
	// zero, like a refused open_pr.
	if reason, refused := c.refuseWrite(req.Agent, WriteOpPushBranch, req.Repo, 0); refused {
		c.denyPushBranchRequest(path, req, reason, nowFn)
		return
	}

	// Per-repo pause (#6203) and per-repo agent scope (#6204), on the same
	// footing as the other relays: for an enforced lane (#9772) this watcher
	// is the only agent-reachable push path, so it is the only place a pause
	// or scope can stop one.
	if c.RepoIsPaused(req.Repo) {
		c.denyPushBranchRequest(path, req, RepoPausedReason(req.Repo), nowFn)
		return
	}
	if !c.AgentServesRepo(req.Agent, req.Repo) {
		c.denyPushBranchRequest(path, req, AgentRepoScopeReason(req.Agent, req.Repo), nowFn)
		return
	}

	// Content gates. Each of these is permanent for this request — no retry
	// can make a malformed branch name valid or move a checkout's owner.
	owner, repoName := c.splitRepo(req.Repo)
	if err := validateRepoRef(owner, repoName); err != nil {
		c.denyPushBranchRequest(path, req, err.Error(), nowFn)
		return
	}
	branch := strings.TrimPrefix(strings.TrimSpace(req.Branch), "refs/heads/")
	if !validPushBranchName(branch) {
		c.denyPushBranchRequest(path, req, fmt.Sprintf("branch %q is not a valid branch name for the push relay", req.Branch), nowFn)
		return
	}
	if err := validatePushBranchDir(req.Dir, fileUID); err != nil {
		c.denyPushBranchRequest(path, req, err.Error(), nowFn)
		return
	}

	attempts := priorPushBranchAttempts(path) + 1

	// The default branch is refused whatever the allowlist says: agents
	// propose changes through PRs, and a relay that pushed straight to the
	// default branch would bypass review, branch protection audit trails and
	// the merge relay's CI gate in one move. A failed lookup fails the attempt
	// (retryable — the answer may exist on the next tick), never open.
	defBranch, err := c.DefaultBranch(ctx, owner, repoName)
	if err != nil {
		c.recordPushBranchFailure(path, req, attempts, err.Error(), nowFn)
		return
	}
	if strings.EqualFold(branch, defBranch) {
		c.denyPushBranchRequest(path, req, fmt.Sprintf("branch %q is the default branch of %s — the push relay only publishes topic branches; changes to the default branch go through a PR and the merge relay", branch, req.Repo), nowFn)
		return
	}

	remote := pushBranchRemoteURL(owner, repoName)
	// Commands that read the agent's checkout run without the auth header:
	// only the push, from a hive-owned GIT_DIR, carries it (#19959).
	env := pushBranchGitEnv(req.Dir, remote, "")

	// Resolve the local head being pushed so the audit entry and result name
	// the exact commit. A branch that does not exist locally can never push.
	sha, err := pushBranchExec(ctx, req.Dir, env, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("local branch %q not found in %s: %s", branch, req.Dir, firstOutputLines(sha, 3)), nowFn)
		return
	}
	sha = strings.TrimSpace(sha)

	// Every commit a hive identity authors must carry its sign-off before it
	// leaves the checkout. An unsigned hive commit on a published branch is
	// what later makes the automerge DCO stage re-author the PR
	// (hivecommons/hive#16619); refusing it here keeps the fix at the source.
	// Hooks are neutralised by pushBranchGitEnv, so the relay checks itself.
	if log, err := pushBranchExec(ctx, req.Dir, env, "git", UnsignedHiveCommitLogArgs(sha)...); err == nil {
		if reason := HiveCommitSignoffRefusal(log, HiveCommitIdentities{Logins: c.pushHiveIdentities()}); reason != "" {
			c.denyPushBranchRequest(path, req, reason, nowFn)
			return
		}
	}

	// Refuse a checkout whose own config could redirect the push
	// (hivecommons/hive#19959). The push below never reads that config; the
	// refusal tells the agent its checkout is set up to redirect pushes.
	listing, err := pushBranchExec(ctx, req.Dir, env, "git", pushBranchConfigListArgs...)
	if err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("reading the git config of %s: %s", req.Dir, firstOutputLines(listing, 3)), nowFn)
		return
	}
	if reason := pushBranchConfigRefusal(listing); reason != "" {
		c.denyPushBranchRequest(path, req, reason, nowFn)
		return
	}

	repoInfo, err := pushBranchExec(ctx, req.Dir, env, "git", pushBranchRepoInfoArgs...)
	if err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("locating the git dir of %s: %s", req.Dir, firstOutputLines(repoInfo, 3)), nowFn)
		return
	}
	gitDir, isolation, cleanup, err := pushBranchIsolatedGitDir(repoInfo, branch, sha, fileUID)
	if err != nil {
		c.denyPushBranchRequest(path, req, err.Error(), nowFn)
		return
	}
	defer cleanup()
	header, _ := c.gitAuthHeader(ctx)
	pushEnv := append(pushBranchGitEnv(gitDir, remote, header), isolation...)

	args := []string{"push"}
	if req.ForceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remote, "refs/heads/"+branch+":refs/heads/"+branch)
	if out, err := pushBranchExec(ctx, gitDir, pushEnv, "git", args...); err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("git push failed: %s", firstOutputLines(out, 3)), nowFn)
		return
	}

	c.deliverAuditRecord(AgentBranchPushedAuditRecord(req.Agent, req.Repo, branch, sha, req.ForceWithLease))
	c.writePushBranchResult(path, PushBranchResponse{OK: true, Branch: branch, SHA: sha, Attempts: attempts, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Remove(path)
	c.notifyAgentAction(AgentAction{Agent: req.Agent, Kind: AgentActionPush, Repo: req.Repo, Branch: branch, SHA: sha, At: nowFn()})
	c.logger.Info("push-branch-request watcher: branch pushed",
		slog.String("repo", req.Repo), slog.String("branch", branch),
		slog.String("sha", sha), slog.String("agent", req.Agent))
}

// recordPushBranchFailure writes the failed attempt's result and applies the
// retry / exhaust policy, mirroring recordMergeFailure without the
// re-engagement hook: a failed push has no PR for a fix loop to own yet.
func (c *Client) recordPushBranchFailure(path string, req PushBranchRequest, attempts int, errMsg string, nowFn func() time.Time) {
	c.writePushBranchResult(path, PushBranchResponse{OK: false, Branch: req.Branch, Attempts: attempts, Error: errMsg, At: nowFn().UTC().Format(time.RFC3339)})
	if attempts >= pushBranchMaxAttempts {
		_ = os.Rename(path, path+".exhausted")
		c.logger.Warn("push-branch-request watcher: push failed, giving up after max attempts",
			slog.String("repo", req.Repo), slog.String("branch", req.Branch),
			slog.Int("attempts", attempts), slog.String("error", errMsg))
		return
	}
	c.logger.Info("push-branch-request watcher: push failed, will retry",
		slog.String("repo", req.Repo), slog.String("branch", req.Branch),
		slog.Int("attempts", attempts), slog.String("error", errMsg))
}

func (c *Client) denyPushBranchRequest(path string, req PushBranchRequest, reason string, nowFn func() time.Time) {
	c.writePushBranchResult(path, PushBranchResponse{OK: false, Branch: req.Branch, Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.logger.Warn("push-branch-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo),
		slog.String("branch", req.Branch), slog.String("reason", reason))
}

// priorPushBranchAttempts reads the attempts count from a previously-written
// result file for this request, so retries accumulate across ticks. Returns 0
// when no prior result exists or it can't be read.
func priorPushBranchAttempts(reqPath string) int {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	b, _, err := readUntrustedFile(out, requestMaxBytes)
	if err != nil {
		return 0
	}
	var prev PushBranchResponse
	if json.Unmarshal(b, &prev) != nil {
		return 0
	}
	return prev.Attempts
}

func (c *Client) writePushBranchResult(reqPath string, resp PushBranchResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = writeRequestFile(out, b)
	}
}

// WritePushBranchRequest is a helper (used by tests and any in-process caller)
// to drop a well-formed push-branch request file into dir.
func WritePushBranchRequest(dir string, req PushBranchRequest) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.json", sanitizeAgentName(req.Agent), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	if err := writeRequestFile(path, b); err != nil {
		return "", err
	}
	return path, nil
}

// unsignedHiveCommitFormat is the git log format unsignedHiveCommits parses:
// one record per commit, fields split by US (0x1f), records by RS (0x1e).
const unsignedHiveCommitFormat = "%H%x1f%an%x1f%ae%x1f%B%x1e"

// unsignedHiveCommit names a commit the push relay refuses.
type unsignedHiveCommit struct {
	SHA    string
	Author string
}

// HiveCommitIdentities names the hive's own commit identities: the commits a
// push relay refuses to publish without a Signed-off-by trailer. The GitHub
// push watcher fills Logins with GitHub logins; the forge push relay fills it
// with the configured forge user and EmailDomains with the lanes' commit
// email domain (hivecommons/hive#19960).
type HiveCommitIdentities struct {
	// Logins match a commit by author name, or by the local part of its
	// author email: a GitHub noreply or App-slug address, or a forge's
	// "<login>@noreply.<host>" address.
	Logins []string
	// EmailDomains match a commit whose author email is at one of these
	// domains, case-insensitively.
	EmailDomains []string
}

// UnsignedHiveCommitLogArgs is the git argv (without "git" itself) that
// lists the non-merge commits a push of sha would publish, those no
// remote-tracking ref of the checkout already has, in the format
// HiveCommitSignoffRefusal reads.
func UnsignedHiveCommitLogArgs(sha string) []string {
	return []string{"log", "--no-merges", "--format=" + unsignedHiveCommitFormat, sha, "--not", "--remotes"}
}

// HiveCommitSignoffRefusal reads the output of UnsignedHiveCommitLogArgs and
// returns why the push is refused when a commit authored by one of ids has no
// Signed-off-by trailer, or "" when every hive commit is signed off. Commits
// by anyone else are not checked; the DCO check on the PR handles those.
func HiveCommitSignoffRefusal(log string, ids HiveCommitIdentities) string {
	bad := unsignedHiveCommits(log, ids)
	if len(bad) == 0 {
		return ""
	}
	return fmt.Sprintf("commit %s is authored by hive identity %q without a Signed-off-by trailer; hive-authored commits must be signed off at creation (git commit -s)", bad[0].SHA, bad[0].Author)
}

// pushHiveIdentities lists the logins whose commits must be signed off: the
// App bot and the configured hive identity (ai_author + App login).
func (c *Client) pushHiveIdentities() []string {
	var ids []string
	if c.appBotLogin != "" {
		ids = append(ids, c.appBotLogin)
	}
	id := c.getHiveIdentity()
	ids = append(ids, id.AIAuthor, id.AppLogin)
	return ids
}

// unsignedHiveCommits parses `git log --format=unsignedHiveCommitFormat`
// output and returns the commits authored by one of ids whose message has no
// Signed-off-by trailer. Commits by anyone else are not the relay's concern;
// the DCO check on the PR handles those.
func unsignedHiveCommits(log string, ids HiveCommitIdentities) []unsignedHiveCommit {
	var bad []unsignedHiveCommit
	for _, rec := range strings.Split(log, "\x1e") {
		rec = strings.TrimSpace(rec)
		if rec == "" {
			continue
		}
		f := strings.SplitN(rec, "\x1f", 4)
		if len(f) < 4 {
			continue
		}
		sha, name, email, msg := strings.TrimSpace(f[0]), strings.TrimSpace(f[1]), strings.TrimSpace(f[2]), f[3]
		login := hiveIdentityForCommitter(name, email, ids.Logins)
		if login == "" {
			login = hiveEmailDomainIdentity(email, ids.EmailDomains)
		}
		if login == "" || signedTrailerLineRE.MatchString(msg) {
			continue
		}
		bad = append(bad, unsignedHiveCommit{SHA: sha, Author: login})
	}
	return bad
}

// hiveIdentityForCommitter returns the login in logins that name/email
// belongs to, or "" when the committer is not a hive identity. A login
// matches its own name, "<login>@users.noreply.github.com",
// "<id>+<login>@users.noreply.github.com", and (for "<slug>[bot]") the
// bracket-free "<slug>@…" address signedTrailerEmail derives.
func hiveIdentityForCommitter(name, email string, logins []string) string {
	email = strings.ToLower(email)
	local := email
	if i := strings.IndexByte(email, '@'); i >= 0 {
		local = email[:i]
	}
	if i := strings.IndexByte(local, '+'); i >= 0 {
		local = local[i+1:]
	}
	for _, login := range logins {
		login = strings.TrimSpace(login)
		if login == "" {
			continue
		}
		slug := strings.TrimSuffix(login, "[bot]")
		if strings.EqualFold(name, login) || strings.EqualFold(name, slug) ||
			strings.EqualFold(local, login) || strings.EqualFold(local, slug) {
			return login
		}
	}
	return ""
}

// hiveEmailDomainIdentity returns email when it is an address at one of
// domains, or "" otherwise.
func hiveEmailDomainIdentity(email string, domains []string) string {
	i := strings.LastIndexByte(email, '@')
	if i < 0 {
		return ""
	}
	host := email[i+1:]
	for _, d := range domains {
		d = strings.TrimSpace(d)
		if d != "" && strings.EqualFold(host, d) {
			return email
		}
	}
	return ""
}
