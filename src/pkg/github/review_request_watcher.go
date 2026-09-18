package github

import (
	"context"
	"encoding/json"
	"errors"
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
// the review type: "approve" | "request_changes" | "comment" |
// "resolve_thread". Body is required for request_changes/comment (GitHub
// rejects an empty non-approve review) and optional for approve.
//
// ThreadID (hivecommons/hive#7360) names one inline review thread — the
// "PRRT_…" node id review-threads.json lists. With Event "comment" it turns
// the request into an in-thread REPLY instead of a PR-level review; with
// Event "resolve_thread" it names the thread to resolve. Both are guarded
// server-side: the thread's first comment must be from a configured
// classification.review_bots login and the thread must still be open, so an
// agent can never reply in or resolve a human's conversation by this path.
type ReviewRequest struct {
	Repo     string `json:"repo"`
	Number   int    `json:"number"`
	Event    string `json:"event"` // approve | request_changes | comment | resolve_thread
	Body     string `json:"body,omitempty"`
	Agent    string `json:"agent,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	// Report carries the reviewer's structured verdict alongside the comment it
	// is posting. The relay writes it server-side for review.Collect; see
	// recordReviewVerdict for why the agent cannot write it itself.
	Report string `json:"report,omitempty"`
}

// ReviewResponse is written next to a consumed request as <name>.result.json.
type ReviewResponse struct {
	OK       bool   `json:"ok"`
	Number   int    `json:"number,omitempty"`
	State    string `json:"state,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Error    string `json:"error,omitempty"`
	At       string `json:"at"`
}

// Review-thread states recorded in the audit detail (state=) for the two
// thread-scoped events. They share AuditActionPRReviewed with the PR-level
// reviews so the activity collector counts them as review output without a
// new action name.
const (
	reviewStateThreadReplied  = "thread_replied"
	reviewStateThreadResolved = "thread_resolved"
)

// reviewEventResolveThread is the Event value for resolving a thread; unlike
// the three review verbs it has no GitHub REST review event behind it.
const reviewEventResolveThread = "resolve_thread"

func isResolveThreadEvent(event string) bool {
	switch strings.TrimSpace(strings.ToLower(event)) {
	case reviewEventResolveThread, "resolve-thread", "resolve":
		return true
	}
	return false
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
	data, _, err := readUntrustedFile(path, requestMaxBytes)
	if err != nil {
		if errors.Is(err, errDropBoxFileRejected) {
			// A FIFO, symlink, or oversize drop can never become a valid
			// request; move it aside so it stops being scanned every tick.
			_ = os.Rename(path, path+".rejected")
			c.logger.Warn("review-request watcher: REJECTED (unsafe file)",
				slog.String("path", path), slog.String("reason", err.Error()))
		}
		return // vanished between ReadDir and here
	}
	var req ReviewRequest
	if err := json.Unmarshal(data, &req); err != nil {
		// A torn read is not a malformed request: leave it for the next tick
		// rather than destroying it. See quarantinable.
		if !quarantinable(path, nowFn()) {
			return
		}
		c.writeReviewResult(path, ReviewResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.reviewRetries.clear(path)
		c.logger.Warn("review-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	apiEvent, state, okEvent := reviewEventToAPI(req.Event)
	resolveThread := isResolveThreadEvent(req.Event)
	// record_verdict posts nothing. It exists because the verdict rides the
	// review relay, and a perspective that honestly has no comment to make must
	// still be able to record that it judged the PR — an unrecorded approve
	// reads downstream as "never reviewed", so the PR is dispatched again from
	// scratch, forever.
	recordOnly := strings.EqualFold(strings.TrimSpace(req.Event), ReviewEventRecordVerdict)
	threadID := strings.TrimSpace(req.ThreadID)
	// A "comment" that names a thread is an in-thread reply, not a PR review.
	threadReply := okEvent && apiEvent == "COMMENT" && threadID != ""
	// Validate shape BEFORE authorizing or touching the API — a hopeless request
	// must never retry.
	var shapeErr string
	switch {
	case strings.TrimSpace(req.Repo) == "" || req.Number <= 0:
		shapeErr = "review request requires repo and number"
	case resolveThread && threadID == "":
		shapeErr = "resolve_thread requires thread_id"
	case recordOnly && strings.TrimSpace(req.Report) == "":
		shapeErr = "record_verdict requires a verdict report"
	case !okEvent && !resolveThread && !recordOnly:
		shapeErr = "review request event must be approve|request_changes|comment|resolve_thread|record_verdict"
	case okEvent && threadID != "" && apiEvent != "COMMENT":
		shapeErr = "thread_id is only valid with event comment (reply) or resolve_thread"
	case okEvent && apiEvent != "APPROVE" && strings.TrimSpace(req.Body) == "":
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

	if resolveThread || threadReply {
		c.handleReviewThreadRequest(ctx, path, req, threadID, resolveThread, nowFn)
		return
	}

	// Authorized and well-formed, but nothing to post: record and finish
	// without touching the GitHub API.
	if recordOnly {
		c.recordReviewVerdict(req, "")
		c.writeReviewResult(path, ReviewResponse{OK: true, Number: req.Number, State: ReviewEventRecordVerdict, At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Remove(path)
		c.reviewRetries.clear(path)
		c.logger.Info("review-request watcher: verdict recorded without a comment",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent))
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
	var created *gh.PullRequestReview
	if leak, ok := c.scanCanaryText(req.Body, "hive-review:"+req.Repo); ok && c.canaryFailClosed {
		err = fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	} else {
		created, _, err = c.client.PullRequests.CreateReview(ctx, owner, repoName, req.Number, reviewReq)
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

	// Record where the review landed so the queue views can link to it. A
	// failure here is logged and swallowed: the review is already posted, and
	// losing its address must never turn a successful review into a retry
	// that posts it a second time.
	if created != nil {
		if err := RecordReviewLink("", req.Repo, req.Number, ReviewLink{
			URL:     created.GetHTMLURL(),
			State:   state,
			HeadSHA: reviewReq.GetCommitID(),
			At:      nowFn().UTC(),
		}); err != nil {
			c.logger.Warn("review-request watcher: could not record review link",
				slog.String("repo", req.Repo), slog.Int("number", req.Number),
				slog.String("error", err.Error()))
		}
	}

	// Persist the structured verdict now that the comment is posted. The two
	// artifacts are recorded together so a verdict can never be attributed to a
	// review that never actually landed.
	c.recordReviewVerdict(req, "")

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

// handleReviewThreadRequest is the thread-scoped tail of handleOneReviewRequest
// (hivecommons/hive#7360): an in-thread reply (Event comment + ThreadID) or a
// resolveReviewThread mutation (Event resolve_thread). The request has already
// passed shape validation and the per-agent authorizer.
//
// The guard lives HERE, not in the prompt. Before touching the thread the
// watcher re-fetches it with the App token and denies when:
//   - classification.review_bots names no login (feature off → nothing is
//     ever resolvable, whatever the kick said);
//   - the id resolves to nothing, or to a thread on a different PR than the
//     request names (a stale or copy-pasted id must not act elsewhere);
//   - the thread's FIRST comment is not from a configured bot — a human's
//     conversation is never replied in or resolved by this path;
//   - the thread is already resolved;
//   - (reply only) the hive has already replied max_attempts_per_thread
//     times — the "one more attempt, then stop" ceiling is enforced even if
//     the monitor's filter is bypassed.
//
// Denials quarantine the request as .denied exactly like an authorization
// failure: they are policy verdicts, not transient errors, so they never
// retry. Forge errors on the mutation itself go through the same backoff +
// give-up horizon as a failed review.
func (c *Client) handleReviewThreadRequest(ctx context.Context, path string, req ReviewRequest, threadID string, resolve bool, nowFn func() time.Time) {
	bots := c.getReviewBots()
	if !bots.Enabled() {
		c.denyReviewRequest(path, req, "classification.review_bots is not configured; thread replies and resolution are disabled", nowFn)
		return
	}
	node, err := c.fetchReviewThreadNode(ctx, threadID)
	if err != nil {
		c.retryOrQuarantineReview(path, req, err, nowFn)
		return
	}
	switch {
	case node == nil:
		c.denyReviewRequest(path, req, "thread "+threadID+" not found", nowFn)
		return
	case node.Number != req.Number || !sameRepoRef(node.Repo, req.Repo, c.org):
		c.denyReviewRequest(path, req, fmt.Sprintf("thread %s belongs to %s#%d, not the requested PR", threadID, node.Repo, node.Number), nowFn)
		return
	case !bots.IsBot(node.FirstAuthor):
		author := node.FirstAuthor
		if author == "" {
			author = "(unknown)"
		}
		c.denyReviewRequest(path, req, "thread "+threadID+" was opened by "+author+", which is not a configured review bot; human threads are never resolved by the hive", nowFn)
		return
	case node.IsResolved:
		c.denyReviewRequest(path, req, "thread "+threadID+" is already resolved", nowFn)
		return
	case !resolve && node.HiveReplies >= bots.MaxAttempts():
		c.denyReviewRequest(path, req, fmt.Sprintf("thread %s already has %d hive replies (max_attempts_per_thread=%d); leaving it for a human", threadID, node.HiveReplies, bots.MaxAttempts()), nowFn)
		return
	}

	meta := c.attributionMeta(req.Agent)
	state := reviewStateThreadResolved
	if resolve {
		err = c.resolveReviewThread(ctx, threadID)
	} else {
		state = reviewStateThreadReplied
		body := req.Body
		if c.attributionTrailerOn() {
			body = AppendTrailer(body, meta)
		}
		// Same canary contract as the PR-level review body: agent-supplied
		// text posted straight to the forge.
		if leak, ok := c.scanCanaryText(req.Body, "hive-review:"+req.Repo); ok && c.canaryFailClosed {
			err = fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		} else {
			err = c.replyToReviewThread(ctx, threadID, body)
		}
	}
	if err != nil {
		c.retryOrQuarantineReview(path, req, err, nowFn)
		return
	}

	c.recordCreationAudit(AuditActionPRReviewed, meta,
		"repo", req.Repo,
		"number", strconv.Itoa(req.Number),
		"state", state,
		"thread", threadID)
	c.writeReviewResult(path, ReviewResponse{OK: true, Number: req.Number, State: state, ThreadID: threadID, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Remove(path)
	c.reviewRetries.clear(path)
	c.logger.Info("review-request watcher: review thread "+strings.TrimPrefix(state, "thread_")+" by App bot",
		slog.String("repo", req.Repo), slog.Int("number", req.Number),
		slog.String("thread", threadID), slog.String("agent", req.Agent))
}

// retryOrQuarantineReview is the shared forge-failure path: record the error
// in the result file, back off, and quarantine as .failed past the horizon.
func (c *Client) retryOrQuarantineReview(path string, req ReviewRequest, err error, nowFn func() time.Time) {
	c.writeReviewResult(path, ReviewResponse{OK: false, Error: err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
	if c.reviewRetries.noteFailure(path, nowFn()) {
		_ = os.Rename(path, path+".failed")
		c.reviewRetries.clear(path)
		c.logger.Error("review-request watcher: request exceeded retry horizon, quarantined",
			slog.String("path", path), slog.String("repo", req.Repo),
			slog.Int("number", req.Number), slog.String("error", err.Error()))
		return
	}
	c.logger.Warn("review-request watcher: request failed, will retry with backoff",
		slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", err.Error()))
}

// sameRepoRef reports whether a thread's "owner/name" matches the request's
// repo, which may be bare ("name", resolved against org) or qualified.
func sameRepoRef(nameWithOwner, requested, org string) bool {
	nameWithOwner = strings.TrimSpace(nameWithOwner)
	requested = strings.TrimSpace(requested)
	if nameWithOwner == "" || requested == "" {
		return false
	}
	if !strings.Contains(requested, "/") {
		if org == "" {
			// No org to qualify with: match on the bare repo name only.
			_, name, _ := strings.Cut(nameWithOwner, "/")
			return strings.EqualFold(name, requested)
		}
		requested = org + "/" + requested
	}
	return strings.EqualFold(nameWithOwner, requested)
}

func (c *Client) writeReviewResult(reqPath string, resp ReviewResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = writeRequestFile(out, b)
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
	if err := writeRequestFile(path, b); err != nil {
		return "", err
	}
	return path, nil
}
