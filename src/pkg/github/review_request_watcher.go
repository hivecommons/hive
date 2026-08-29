package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// ReviewRequestDir is where agents drop PR-review requests. An agent that wants
// to review a PR writes a request file here INSTEAD of running `gh pr review`
// from its own shell. The hive's watcher submits the review with the App token,
// so the review is authored by the App bot AND — unlike a direct agent-CLI
// review, which the hive never observes — it is recorded on the audit/activity
// trail. This is the review analogue of the PR-request watcher.
const ReviewRequestDir = "/var/run/hive-metrics/review-requests"

// reviewRequestDirForTest lets tests point the watcher at a temp dir.
var reviewRequestDirForTest string

func reviewRequestDir() string {
	if reviewRequestDirForTest != "" {
		return reviewRequestDirForTest
	}
	return ReviewRequestDir
}

// reviewRequestPollInterval mirrors prRequestPollInterval — reviews are not
// latency-critical. A var so tests can drive the ticker quickly.
var reviewRequestPollInterval = 10 * time.Second

// ReviewRequest is the JSON an agent writes to ReviewRequestDir. Event selects
// the review type: "approve" | "request_changes" | "comment". Body is required
// for request_changes/comment (GitHub rejects an empty non-approve review) and
// optional for approve.
type ReviewRequest struct {
	Repo   string `json:"repo"`
	Number int    `json:"number"`
	Event  string `json:"event"` // approve | request_changes | comment
	Body   string `json:"body,omitempty"`
	Agent  string `json:"agent,omitempty"`
}

// ReviewResponse is written next to a consumed request as <name>.result.json.
type ReviewResponse struct {
	OK     bool   `json:"ok"`
	Number int    `json:"number,omitempty"`
	State  string `json:"state,omitempty"`
	Error  string `json:"error,omitempty"`
	At     string `json:"at"`
}

// ReviewRequestAuthorizer mirrors PRRequestAuthorizer: it receives the claimed
// agent name and the request-file owning UID and returns nil to authorize.
// A nil authorizer denies everything (fail closed). Reviewing is a PR-write, so
// the caller gates it with the same push-capability check as opening a PR.
type ReviewRequestAuthorizer func(agent string, fileUID int) error

// reviewEventToAPI maps our lowercase Event to the GitHub review Event verb and
// the state string we record in the audit trail. ok=false for an unknown event.
func reviewEventToAPI(event string) (apiEvent, state string, ok bool) {
	switch strings.TrimSpace(strings.ToLower(event)) {
	case "approve", "approved":
		return "APPROVE", "approved", true
	case "request_changes", "changes_requested", "request-changes":
		return "REQUEST_CHANGES", "changes_requested", true
	case "comment", "commented":
		return "COMMENT", "commented", true
	default:
		return "", "", false
	}
}

// StartReviewRequestWatcher runs the loop that submits PR reviews for request
// files dropped in ReviewRequestDir. Same contract as StartPRRequestWatcher:
// returns immediately, runs until ctx cancel, nil client is a no-op (requests
// accumulate rather than silently dropping), nil authz fails closed.
//
// The returned channel closes when the watcher goroutine has exited (or
// immediately when it never starts), so callers — tests above all — can JOIN
// the loop after cancelling instead of sleeping and hoping: an unjoined
// watcher outliving its test races the test's global-seam restores.
func (c *Client) StartReviewRequestWatcher(ctx context.Context, authz ReviewRequestAuthorizer, nowFn func() time.Time) <-chan struct{} {
	done := make(chan struct{})
	if c == nil {
		close(done)
		return done
	}
	c.reviewAuthz = authz
	if nowFn == nil {
		nowFn = time.Now
	}
	// Agents (UID >= 2001, shared node group) must be able to DROP request files;
	// MkdirAll is umask-masked, so force group-write + setgid (same as the PR
	// watcher). The forge-check still holds via the file's owning UID.
	if !ensureRequestDir(c.logger, "review", reviewRequestDir()) {
		close(done)
		return done
	}
	// Capture the poll interval BEFORE spawning: the goroutine's first read of
	// the package-level interval races with a test's fastTick cleanup restoring
	// it (the race detector flagged exactly that on v4 CI). Reading it here is
	// sequenced with the caller, and the loop only ever needed it once anyway.
	interval := reviewRequestPollInterval
	go func() {
		defer close(done)
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// A cancelled ctx can lose the select to an already-ready tick
				// (select picks randomly among ready cases), letting the loop
				// process one more scan after cancellation. Fail the tick
				// closed so cancel means no further processing.
				if ctx.Err() != nil {
					return
				}
				c.processReviewRequests(ctx, nowFn)
			}
		}
	}()
	c.logger.Info("review-request watcher started", slog.String("dir", reviewRequestDir()))
	return done
}

func (c *Client) processReviewRequests(ctx context.Context, nowFn func() time.Time) {
	entries, err := os.ReadDir(reviewRequestDir())
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		c.handleOneReviewRequest(ctx, filepath.Join(reviewRequestDir(), name), nowFn)
	}
}

// ProcessReviewRequestsOnce runs a single scan+process pass. Test/CLI entry.
func (c *Client) ProcessReviewRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processReviewRequests(ctx, time.Now)
}

func (c *Client) handleOneReviewRequest(ctx context.Context, path string, nowFn func() time.Time) {
	if !c.reviewRetries.allows(path, nowFn()) {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return // vanished between ReadDir and here
	}
	var req ReviewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		c.writeReviewResult(path, ReviewResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.reviewRetries.clear(path)
		c.logger.Warn("review-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	apiEvent, state, okEvent := reviewEventToAPI(req.Event)
	// Validate shape BEFORE authorizing or touching the API — a hopeless request
	// must never retry.
	var shapeErr string
	switch {
	case strings.TrimSpace(req.Repo) == "" || req.Number <= 0:
		shapeErr = "review request requires repo and number"
	case !okEvent:
		shapeErr = "review request event must be approve|request_changes|comment"
	case apiEvent != "APPROVE" && strings.TrimSpace(req.Body) == "":
		shapeErr = "review request body is required for request_changes/comment"
	}
	if shapeErr != "" {
		c.writeReviewResult(path, ReviewResponse{OK: false, Error: shapeErr, At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.reviewRetries.clear(path)
		c.logger.Warn("review-request watcher: malformed request quarantined",
			slog.String("path", path), slog.String("reason", shapeErr))
		return
	}

	// Authorize: forge-resistance (file UID must be the claimed agent) + the same
	// push-capability gate as opening a PR. A nil authorizer fails closed.
	fileUID := statUID(data, path)
	if c.reviewAuthz == nil {
		c.denyReviewRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.reviewAuthz(req.Agent, fileUID); err != nil {
		c.denyReviewRequest(path, req, err.Error(), nowFn)
		return
	}

	meta := c.attributionMeta(req.Agent)
	body := req.Body
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

	owner, repoName := c.splitRepo(req.Repo)
	reviewReq := &gh.PullRequestReviewRequest{Event: gh.Ptr(apiEvent)}
	if strings.TrimSpace(body) != "" {
		reviewReq.Body = gh.Ptr(body)
	}
	resp := ReviewResponse{At: nowFn().UTC().Format(time.RFC3339)}
	// Canary-gated like CreateIssue/CreatePR/CreateIssueComment
	// (kubestellar/hive#4960): a PR review body is agent-supplied text posted
	// straight to GitHub, the same exfiltration shape as an issue or comment,
	// so it must honor the same fail-closed contract and flow through the same
	// error/retry handling as a real CreateReview failure below.
	if leak, ok := c.scanCanaryText(req.Body, "hive-review:"+req.Repo); ok && c.canaryFailClosed {
		err = fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	} else {
		_, _, err = c.client.PullRequests.CreateReview(ctx, owner, repoName, req.Number, reviewReq)
	}
	if err != nil {
		// Retry with exponential backoff and quarantine at the give-up horizon
		// (request_retry.go) — an every-tick retry loop on a poisoned request
		// burns secondary-rate-limit budget for the whole App installation.
		resp.OK = false
		resp.Error = err.Error()
		c.writeReviewResult(path, resp)
		if c.reviewRetries.noteFailure(path, nowFn()) {
			_ = os.Rename(path, path+".failed")
			c.reviewRetries.clear(path)
			c.logger.Error("review-request watcher: request exceeded retry horizon, quarantined",
				slog.String("path", path), slog.String("repo", req.Repo),
				slog.Int("number", req.Number), slog.String("error", err.Error()))
			return
		}
		c.logger.Warn("review-request watcher: review failed, will retry with backoff",
			slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
		return
	}
	resp.OK = true
	resp.Number = req.Number
	resp.State = state

	c.recordCreationAudit(AuditActionPRReviewed, meta,
		"repo", req.Repo,
		"number", strconv.Itoa(req.Number),
		"state", state)
	c.writeReviewResult(path, resp)
	_ = os.Remove(path)
	c.reviewRetries.clear(path)
	c.logger.Info("review-request watcher: review submitted by App bot",
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.String("state", state), slog.String("agent", req.Agent))
}

func (c *Client) denyReviewRequest(path string, req ReviewRequest, reason string, nowFn func() time.Time) {
	c.writeReviewResult(path, ReviewResponse{OK: false, Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.reviewRetries.clear(path)
	c.logger.Warn("review-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo),
		slog.Int("number", req.Number), slog.String("reason", reason))
}

func (c *Client) writeReviewResult(reqPath string, resp ReviewResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = os.WriteFile(out, b, 0o644)
	}
}

// WriteReviewRequest is a helper (tests / in-process callers) to drop a
// well-formed review-request file into ReviewRequestDir.
func WriteReviewRequest(dir string, req ReviewRequest) (string, error) {
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
