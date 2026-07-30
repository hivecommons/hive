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

// PRRequestDir is where agents drop PR-open requests. An agent pushes its branch
// (the credential helper uses the App token, so the push is already the App
// identity) and then writes a request file here INSTEAD of running `gh pr
// create`. The hive's watcher opens the PR with the App token, so the PR is
// authored by the App bot — never the Copilot login user. This keeps PR
// authorship deterministic without touching Copilot's auth/entitlement.
const PRRequestDir = "/var/run/hive-metrics/pr-requests"

// prRequestDirForTest lets tests point the watcher at a temp dir. Empty means
// use PRRequestDir. Not for production use.
var prRequestDirForTest string

func prRequestDir() string {
	if prRequestDirForTest != "" {
		return prRequestDirForTest
	}
	return PRRequestDir
}

// prRequestPollInterval is how often the watcher scans PRRequestDir. PR opens
// are not latency-critical (the agent has already pushed and moved on), so a
// modest interval keeps the API/log noise low.
const prRequestPollInterval = 10 * time.Second

// PRRequest is the JSON an agent writes to PRRequestDir to ask the hive to open
// a PR on its behalf. Repo may be "owner/repo" or a bare repo name; Base
// defaults to "main".
type PRRequest struct {
	Repo   string `json:"repo"`
	Head   string `json:"head"`
	Base   string `json:"base,omitempty"`
	Title  string `json:"title"`
	Body   string `json:"body,omitempty"`
	Agent  string `json:"agent,omitempty"`
	IssueN []int  `json:"issues,omitempty"` // informational; the body already carries "Fixes #N"
}

// PRResponse is written back next to a consumed request (as <name>.result.json)
// so the agent — or an operator debugging — can see what happened.
type PRResponse struct {
	OK             bool   `json:"ok"`
	Number         int    `json:"number,omitempty"`
	URL            string `json:"url,omitempty"`
	AlreadyExisted bool   `json:"already_existed,omitempty"`
	Error          string `json:"error,omitempty"`
	At             string `json:"at"`
}

// StartPRRequestWatcher runs a loop that opens PRs for request files dropped in
// PRRequestDir. It returns immediately; the loop runs until ctx is cancelled.
// A nil client (no GitHub creds) makes this a no-op: requests accumulate rather
// than being silently dropped, so the feature degrades to "nothing opens" not
// "opened as the wrong identity".
//
// nowFn is injectable for tests; pass nil for time.Now.
func (c *Client) StartPRRequestWatcher(ctx context.Context, nowFn func() time.Time) {
	if c == nil {
		return
	}
	if nowFn == nil {
		nowFn = time.Now
	}
	if err := os.MkdirAll(prRequestDir(), 0o777); err != nil {
		c.logger.Warn("pr-request watcher: cannot create request dir; disabled",
			slog.String("dir", prRequestDir()), slog.String("error", err.Error()))
		return
	}
	go func() {
		t := time.NewTicker(prRequestPollInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c.processPRRequests(ctx, nowFn)
			}
		}
	}()
	c.logger.Info("pr-request watcher started", slog.String("dir", prRequestDir()))
}

// processPRRequests handles one scan of the request dir. Exported-in-spirit for
// tests via ProcessPRRequestsOnce.
func (c *Client) processPRRequests(ctx context.Context, nowFn func() time.Time) {
	entries, err := os.ReadDir(prRequestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Only consume request files; skip our own .result.json outputs.
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		path := filepath.Join(prRequestDir(), name)
		c.handleOnePRRequest(ctx, path, nowFn)
	}
}

// ProcessPRRequestsOnce runs a single scan+process pass. Test/CLI entry point.
func (c *Client) ProcessPRRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processPRRequests(ctx, time.Now)
}

func (c *Client) handleOnePRRequest(ctx context.Context, path string, nowFn func() time.Time) {
	data, err := os.ReadFile(path)
	if err != nil {
		return // vanished between ReadDir and here — fine
	}
	var req PRRequest
	if err := json.Unmarshal(data, &req); err != nil {
		// A malformed request can never succeed; move it aside so it stops being
		// retried every tick, and leave a result explaining why.
		c.writePRResult(path, PRResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.logger.Warn("pr-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	res, err := c.CreatePR(ctx, req.Repo, req.Head, req.Base, req.Title, req.Body)
	resp := PRResponse{At: nowFn().UTC().Format(time.RFC3339)}
	if err != nil {
		// Leave the request in place so the next tick retries (transient API
		// errors, main not yet pushed, etc.), but record the last error.
		resp.OK = false
		resp.Error = err.Error()
		c.writePRResult(path, resp)
		c.logger.Warn("pr-request watcher: open failed, will retry",
			slog.String("repo", req.Repo), slog.String("head", req.Head), slog.String("error", err.Error()))
		return
	}
	resp.OK = true
	resp.Number = res.Number
	resp.URL = res.URL
	resp.AlreadyExisted = res.AlreadyExisted
	c.writePRResult(path, resp)
	// Success (or reuse of an existing PR) — consume the request so it isn't
	// reprocessed.
	_ = os.Remove(path)
	c.logger.Info("pr-request watcher: PR opened by App bot",
		slog.String("repo", req.Repo), slog.String("head", req.Head),
		slog.Int("number", res.Number), slog.Bool("reused", res.AlreadyExisted),
		slog.String("agent", req.Agent))
}

func (c *Client) writePRResult(reqPath string, resp PRResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = os.WriteFile(out, b, 0o644)
	}
}

// WritePRRequest is a helper (used by tests and any in-process caller) to drop a
// well-formed request file into PRRequestDir.
func WritePRRequest(dir string, req PRRequest) (string, error) {
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

func sanitizeAgentName(s string) string {
	if s == "" {
		return "agent"
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "agent"
	}
	return b.String()
}
