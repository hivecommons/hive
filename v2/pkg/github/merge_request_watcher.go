package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// MergeRequestDir is where agents drop merge requests. An agent that wants a PR
// merged writes a request file here INSTEAD of calling the GitHub MCP
// merge_pull_request tool (which issues a GraphQL mergePullRequest mutation that
// GitHub rejects for App installation tokens with "Resource not accessible by
// integration"). The hive's watcher merges the PR over REST with the App token,
// so the merge is authored by the App bot and uses the code path that actually
// works. This mirrors the hive-open-pr authorship relay.
const MergeRequestDir = "/var/run/hive-metrics/merge-requests"

// mergeRequestDirForTest lets tests point the watcher at a temp dir. Empty means
// use MergeRequestDir. Not for production use.
var mergeRequestDirForTest string

func mergeRequestDir() string {
	if mergeRequestDirForTest != "" {
		return mergeRequestDirForTest
	}
	return MergeRequestDir
}

// mergeRequestPollInterval is how often the watcher scans MergeRequestDir.
// Merges are more latency-sensitive than opens (an eligible PR should land
// quickly), but a tight loop risks GitHub secondary-rate-limits when a batch is
// pending, so keep it modest.
const mergeRequestPollInterval = 10 * time.Second

// mergeRequestMaxAttempts bounds retries on a transient merge failure before the
// request is quarantined. A PR that is genuinely un-mergeable (failing required
// checks, true conflict) must not be retried forever — that would burn API
// budget every tick. Non-terminal failures (branch behind, 405 "not mergeable
// yet") get a few tries; after this many the request is set aside with its last
// error recorded for an operator to see.
const mergeRequestMaxAttempts = 3

// MergeRequest is the JSON an agent writes to MergeRequestDir to ask the hive to
// merge a PR on its behalf. Repo may be "owner/repo" or a bare repo name.
// Method defaults to "squash". ExpectSHA, when set, is passed as the required
// head SHA so a moved head fails the merge cleanly instead of merging an
// unexpected commit.
type MergeRequest struct {
	Repo      string `json:"repo"`
	Number    int    `json:"number"`
	Method    string `json:"method,omitempty"`
	ExpectSHA string `json:"expect_sha,omitempty"`
	Agent     string `json:"agent,omitempty"`
	// UpdateBranch, when true, syncs the PR branch with base before merging
	// (resolves the common "behind main" case). Default false: the caller
	// usually only sets this after a merge failed for "not mergeable".
	UpdateBranch bool `json:"update_branch,omitempty"`
}

// MergeResponse is written back next to a consumed request (as
// <name>.result.json) so the agent — or an operator debugging — can see what
// happened.
type MergeResponse struct {
	OK       bool   `json:"ok"`
	Number   int    `json:"number,omitempty"`
	SHA      string `json:"sha,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

// MergeRequestAuthorizer decides whether a merge request may proceed. Like
// PRRequestAuthorizer it receives the claimed agent NAME and the UID that OWNS
// the request file, PLUS the merge TARGET (repo, number, expectSHA) so the
// authorization can be bound to the specific PR being landed. Returning nil
// authorizes. The caller implements the checks:
//
//  1. forge-resistance — the file UID must map to the claimed agent;
//  2. the ACMM merge-gate — the agent must be merge-capable (CanMerge) at the
//     hive's current level;
//  3. target-binding (F4, CWE-863) — expectSHA must be non-empty (an empty
//     expected SHA is a TOCTOU hole: "merge whatever HEAD is now"), and the
//     (repo, number) pair must currently appear in the governor's
//     merge-eligible list. Without (3) an injected L6 scanner could request
//     landing ANY reachable PR whose required checks pass, including its own.
//
// A nil authorizer DENIES everything (fail closed).
type MergeRequestAuthorizer func(agent string, fileUID int, repo string, number int, expectSHA string) error

// StartMergeRequestWatcher runs a loop that merges PRs for request files dropped
// in MergeRequestDir. It returns immediately; the loop runs until ctx is
// cancelled. A nil client (no GitHub creds) makes this a no-op. authz enforces
// the per-agent ACMM merge-gate + forge-resistance AND the F4 target-binding
// (pinned SHA + merge-eligible membership); a nil authz fails closed. nowFn is
// injectable for tests; pass nil for time.Now.
func (c *Client) StartMergeRequestWatcher(ctx context.Context, authz MergeRequestAuthorizer, nowFn func() time.Time) {
	if c == nil {
		return
	}
	c.mergeAuthz = authz
	if nowFn == nil {
		nowFn = time.Now
	}
	if err := os.MkdirAll(mergeRequestDir(), 0o777); err != nil {
		c.logger.Warn("merge-request watcher: cannot create request dir; disabled",
			slog.String("dir", mergeRequestDir()), slog.String("error", err.Error()))
		return
	}
	// Agents must be able to DROP request files here (hive-merge runs AS the
	// agent). Force group-write + setgid so agent-written files inherit the node
	// group — same rationale as the pr-request dir. The forge check still holds:
	// the watcher reads each file's OWNING UID.
	if err := os.Chmod(mergeRequestDir(), 0o2775); err != nil {
		c.logger.Warn("merge-request watcher: could not set group-writable perms on request dir; agents may be unable to request merges",
			slog.String("dir", mergeRequestDir()), slog.String("error", err.Error()))
	}
	go func() {
		t := time.NewTicker(mergeRequestPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.processMergeRequests(ctx, nowFn)
			}
		}
	}()
	c.logger.Info("merge-request watcher started", slog.String("dir", mergeRequestDir()))
}

func (c *Client) processMergeRequests(ctx context.Context, nowFn func() time.Time) {
	entries, err := os.ReadDir(mergeRequestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		path := filepath.Join(mergeRequestDir(), name)
		c.handleOneMergeRequest(ctx, path, nowFn)
	}
}

// ProcessMergeRequestsOnce runs a single scan+process pass. Test/CLI entry point.
func (c *Client) ProcessMergeRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processMergeRequests(ctx, time.Now)
}

func (c *Client) handleOneMergeRequest(ctx context.Context, path string, nowFn func() time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // vanished between ReadDir and here — fine
	}
	var req MergeRequest
	if err := json.Unmarshal(data, &req); err != nil {
		c.writeMergeResult(path, MergeResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.logger.Warn("merge-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	// AUTHORIZE before merging — same policy as a direct merge would need: the
	// request's agent must own the file AND be merge-capable at the hive's ACMM
	// level. A nil authorizer fails closed.
	fileUID := statUID(data, path)
	if c.mergeAuthz == nil {
		c.denyMergeRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.mergeAuthz(req.Agent, fileUID, req.Repo, req.Number, req.ExpectSHA); err != nil {
		c.denyMergeRequest(path, req, err.Error(), nowFn)
		return
	}

	// Optional branch-update-first (resolves "behind main"). A failure here is
	// not fatal — the merge attempt below will surface the real blocker.
	if req.UpdateBranch {
		if err := c.UpdateBranch(ctx, req.Repo, req.Number); err != nil {
			c.logger.Info("merge-request watcher: update-branch failed (continuing to merge attempt)",
				slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
		}
	}

	// Track attempts across ticks by reading the prior result (if any).
	attempts := priorMergeAttempts(path) + 1

	res, err := c.MergePR(ctx, req.Repo, req.Number, req.Method, req.ExpectSHA)
	resp := MergeResponse{Number: req.Number, Attempts: attempts, At: nowFn().UTC().Format(time.RFC3339)}
	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
		c.writeMergeResult(path, resp)
		if attempts >= mergeRequestMaxAttempts {
			// Terminal: a PR that still won't merge after N tries is blocked by
			// something a retry can't fix (failing required check, true conflict,
			// permission). Quarantine so it stops burning API budget; the result
			// file records why, and the next kick can re-request if state changes.
			_ = os.Rename(path, path+".exhausted")
			c.logger.Warn("merge-request watcher: merge failed, giving up after max attempts",
				slog.String("repo", req.Repo), slog.Int("number", req.Number),
				slog.Int("attempts", attempts), slog.String("error", err.Error()))
			return
		}
		c.logger.Info("merge-request watcher: merge failed, will retry",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.Int("attempts", attempts), slog.String("error", err.Error()))
		return
	}
	resp.OK = true
	resp.SHA = res.SHA
	c.writeMergeResult(path, resp)
	_ = os.Remove(path)
	c.logger.Info("merge-request watcher: PR merged by App bot",
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.String("sha", res.SHA), slog.String("agent", req.Agent))
}

func (c *Client) denyMergeRequest(path string, req MergeRequest, reason string, nowFn func() time.Time) {
	c.writeMergeResult(path, MergeResponse{OK: false, Number: req.Number, Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.logger.Warn("merge-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo),
		slog.Int("number", req.Number), slog.String("reason", reason))
}

// priorMergeAttempts reads the attempts count from a previously-written result
// file for this request, so retries accumulate across ticks. Returns 0 when no
// prior result exists or it can't be read.
func priorMergeAttempts(reqPath string) int {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	b, err := os.ReadFile(out)
	if err != nil {
		return 0
	}
	var prev MergeResponse
	if json.Unmarshal(b, &prev) != nil {
		return 0
	}
	return prev.Attempts
}

func (c *Client) writeMergeResult(reqPath string, resp MergeResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = os.WriteFile(out, b, 0o644)
	}
}

// WriteMergeRequest is a helper (used by tests and any in-process caller) to drop
// a well-formed merge request file into MergeRequestDir.
func WriteMergeRequest(dir string, req MergeRequest) (string, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(req, "", "  ")
	if err != nil {
		return "", err
	}
	name := fmt.Sprintf("%s-%d.json", sanitizeAgentName(req.Agent), time.Now().UnixNano())
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}
