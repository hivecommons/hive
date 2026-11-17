package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/beads"
)

// The issue and topic kinds of the sandbox request spool
// (hivecommons/hive#18572, #18573). review_request_sandbox_spool.go verifies
// and stages every spooled request; this file hands the issue and topic
// kinds to their handlers. A spooled issue request runs through
// handleOneIssueRequestFrom, so the repo gate, the ACMM mode gate, the lane
// write allowlist, repo pause, repo scope, dedupe by title, the canary scan
// and the audit trail all apply exactly as they do in-pod. A spooled topic
// request is applied to the manifest agent's own bead store by the handler
// cmd/hive installs. Both are attributed to the manifest's agent only.

// Topic request operations, mirroring `bd topic`.
const (
	SandboxTopicOpClaim   = "claim"
	SandboxTopicOpGet     = "get"
	SandboxTopicOpList    = "list"
	SandboxTopicOpRelease = "release"
)

// SandboxTopicRequest is what hive-topic writes into the spool. Agent is
// ignored: a spooled request belongs to the manifest's agent. A claim with
// no PR is a pending claim for the PR hive opens from this kick.
type SandboxTopicRequest struct {
	Op     string `json:"op"`
	Key    string `json:"key,omitempty"`
	Repo   string `json:"repo,omitempty"`
	PR     int    `json:"pr,omitempty"`
	Branch string `json:"branch,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// SandboxTopicCall is a validated topic request with its attribution
// resolved. Pending marks a claim that binds to the PR hive opens from
// Branch once that PR exists.
type SandboxTopicCall struct {
	Agent   string
	Op      string
	Key     string
	Repo    string
	PR      int
	Branch  string
	Pending bool
}

// SandboxTopicResponse is the result file hive-topic reads back. Refused
// means the topic is held through a different PR (claim) or has no open
// claim (get); hive-topic exits 2 for it, as `bd topic` does.
type SandboxTopicResponse struct {
	OK       bool               `json:"ok"`
	Refused  bool               `json:"refused,omitempty"`
	Error    string             `json:"error,omitempty"`
	Claim    *beads.TopicClaim  `json:"claim,omitempty"`
	Claims   []beads.TopicClaim `json:"claims,omitempty"`
	Released bool               `json:"released,omitempty"`
	Pending  bool               `json:"pending,omitempty"`
	At       string             `json:"at"`
}

// SandboxTopicHandler applies one topic call to the agent's bead store.
type SandboxTopicHandler func(SandboxTopicCall) SandboxTopicResponse

// SetSandboxSpoolRelays installs the authorizer for spooled issue requests
// and the handler for spooled topic requests. Call it before the request
// watchers start. A nil authorizer or handler refuses its kind (fail
// closed): the request gets a result saying so and nothing is written.
func (c *Client) SetSandboxSpoolRelays(issueAuthz IssueRequestAuthorizer, topic SandboxTopicHandler) {
	if c == nil {
		return
	}
	c.sandboxSpoolMu.Lock()
	defer c.sandboxSpoolMu.Unlock()
	c.sandboxSpoolIssueAuthz = issueAuthz
	c.sandboxSpoolTopic = topic
}

func (c *Client) sandboxSpoolRelays() (IssueRequestAuthorizer, SandboxTopicHandler) {
	c.sandboxSpoolMu.Lock()
	defer c.sandboxSpoolMu.Unlock()
	return c.sandboxSpoolIssueAuthz, c.sandboxSpoolTopic
}

// spoolRequestPaths lists the requests waiting in a staging dir.
func spoolRequestPaths(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range entries {
		if isReviewRequestFile(e) {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	return out
}

// processSandboxIssueDir runs a spool's staged issue requests through the
// pod-local issue relay's handler.
func (c *Client) processSandboxIssueDir(ctx context.Context, dir string, src reviewRequestSource, nowFn func() time.Time) {
	for _, path := range spoolRequestPaths(dir) {
		if ctx.Err() != nil {
			return
		}
		c.handleOneIssueRequestFrom(ctx, path, src, nowFn)
	}
}

// processSandboxTopicDir applies a spool's staged topic requests.
func (c *Client) processSandboxTopicDir(ctx context.Context, dir string, src reviewRequestSource, nowFn func() time.Time) {
	for _, path := range spoolRequestPaths(dir) {
		if ctx.Err() != nil {
			return
		}
		c.handleOneSandboxTopicRequest(path, src, nowFn)
	}
}

// handleOneSandboxTopicRequest applies one staged topic request and writes
// its result. A topic write is local to the agent's bead store, so there is
// nothing to retry: every request is final on its first pass.
func (c *Client) handleOneSandboxTopicRequest(path string, src reviewRequestSource, nowFn func() time.Time) {
	data, _, err := readUntrustedFile(path, requestMaxBytes)
	if err != nil {
		if errors.Is(err, errDropBoxFileRejected) {
			_ = os.Rename(path, path+".rejected")
		}
		return
	}
	var req SandboxTopicRequest
	if err := json.Unmarshal(data, &req); err != nil {
		if !quarantinable(path, nowFn()) {
			return
		}
		c.finishSandboxTopicRequest(path, ".bad", SandboxTopicResponse{Error: "invalid JSON: " + err.Error()}, nowFn)
		return
	}
	call, reason := sandboxTopicCall(req, src)
	if reason != "" {
		c.finishSandboxTopicRequest(path, ".bad", SandboxTopicResponse{Error: reason}, nowFn)
		return
	}
	if call.Op == SandboxTopicOpClaim && !c.AgentServesRepo(call.Agent, call.Repo) {
		c.finishSandboxTopicRequest(path, ".denied", SandboxTopicResponse{Error: "authorization denied: " + AgentRepoScopeReason(call.Agent, call.Repo)}, nowFn)
		return
	}
	_, handler := c.sandboxSpoolRelays()
	if handler == nil {
		c.finishSandboxTopicRequest(path, ".denied", SandboxTopicResponse{Error: "authorization denied: no topic store configured (fail closed)"}, nowFn)
		return
	}
	resp := handler(call)
	suffix := ""
	if !resp.OK {
		suffix = ".denied"
	}
	c.logger.Info("review-request watcher: sandbox topic request applied",
		slog.String("agent", call.Agent), slog.String("op", call.Op), slog.String("topic", call.Key),
		slog.Bool("ok", resp.OK), slog.Bool("pending", call.Pending), slog.String("error", resp.Error))
	c.finishSandboxTopicRequest(path, suffix, resp, nowFn)
}

// finishSandboxTopicRequest writes resp next to the staged request, then
// removes the request (suffix "") or sets it aside under suffix.
func (c *Client) finishSandboxTopicRequest(path, suffix string, resp SandboxTopicResponse, nowFn func() time.Time) {
	if resp.At == "" {
		resp.At = nowFn().UTC().Format(time.RFC3339)
	}
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = writeRequestFile(strings.TrimSuffix(path, ".json")+".result.json", b)
	}
	if suffix == "" {
		_ = os.Remove(path)
		return
	}
	_ = os.Rename(path, path+suffix)
}

// sandboxTopicCall validates req and resolves it against the manifest: the
// agent is the manifest's, a claim's repo defaults to the kick's target, and
// a claim with no PR number becomes a pending claim on the kick's branch.
func sandboxTopicCall(req SandboxTopicRequest, src reviewRequestSource) (SandboxTopicCall, string) {
	call := SandboxTopicCall{
		Agent:  src.agent,
		Op:     strings.ToLower(strings.TrimSpace(req.Op)),
		Repo:   strings.TrimSpace(req.Repo),
		PR:     req.PR,
		Branch: strings.TrimSpace(req.Branch),
	}
	if call.Agent == "" {
		return call, "topic request has no manifest agent"
	}
	switch call.Op {
	case SandboxTopicOpList:
		return call, ""
	case SandboxTopicOpClaim, SandboxTopicOpGet, SandboxTopicOpRelease:
	default:
		return call, "unknown topic op " + `"` + req.Op + `"` + "; want claim, get, list or release"
	}
	key, err := beads.NormalizeTopicKey(req.Key)
	if err != nil {
		return call, err.Error()
	}
	call.Key = key
	if call.Op != SandboxTopicOpClaim {
		return call, ""
	}
	if call.Repo == "" {
		call.Repo = src.repo
	}
	if call.Repo == "" {
		return call, "topic claim needs --repo"
	}
	switch {
	case call.PR < 0:
		return call, "topic claim needs a positive --pr"
	case call.PR > 0:
		return call, ""
	}
	// No PR yet: the claim is for the PR hive opens from this kick.
	if src.branch == "" {
		return call, "this kick opens no pull request, so a topic claim needs --pr"
	}
	if !beads.SameTopicRepo(call.Repo, src.repo) {
		return call, "a claim without --pr binds to the pull request this kick opens in " + src.repo + ", not " + call.Repo
	}
	if call.Branch != "" && call.Branch != src.branch {
		return call, "a claim without --pr binds to this kick's branch " + src.branch + ", not " + call.Branch
	}
	call.Repo, call.Branch, call.Pending = src.repo, src.branch, true
	return call, ""
}
