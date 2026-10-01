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

func runPushBranchCommand(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
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
func pushBranchGitEnv(dir, remote, header string) []string {
	env := []string{"GIT_TERMINAL_PROMPT=0"}
	pairs := [][2]string{{"safe.directory", dir}}
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
	header, _ := c.gitAuthHeader(ctx)
	env := pushBranchGitEnv(req.Dir, remote, header)

	// Resolve the local head being pushed so the audit entry and result name
	// the exact commit. A branch that does not exist locally can never push.
	sha, err := pushBranchExec(ctx, req.Dir, env, "git", "rev-parse", "--verify", "refs/heads/"+branch)
	if err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("local branch %q not found in %s: %s", branch, req.Dir, firstOutputLines(sha, 3)), nowFn)
		return
	}
	sha = strings.TrimSpace(sha)

	args := []string{"push"}
	if req.ForceWithLease {
		args = append(args, "--force-with-lease")
	}
	args = append(args, remote, "refs/heads/"+branch+":refs/heads/"+branch)
	if out, err := pushBranchExec(ctx, req.Dir, env, "git", args...); err != nil {
		c.recordPushBranchFailure(path, req, attempts,
			fmt.Sprintf("git push failed: %s", firstOutputLines(out, 3)), nowFn)
		return
	}

	c.recordWriteAudit(AuditActionAgentBranchPushed, InvocationMeta{Agent: req.Agent},
		WriteTarget{Repo: req.Repo},
		"branch", branch,
		"sha", sha,
		"force_with_lease", strconv.FormatBool(req.ForceWithLease))
	c.writePushBranchResult(path, PushBranchResponse{OK: true, Branch: branch, SHA: sha, Attempts: attempts, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Remove(path)
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
