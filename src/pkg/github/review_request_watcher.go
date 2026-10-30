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
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/review"
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

// reviewRequestPollInterval mirrors prRequestPollInterval. Since #13378 it
// is the cadence only while webhooks are degraded or the request dir cannot
// be watched; see ReviewRequestWebhookFallbackInterval for the healthy
// cadence. A var so tests can drive the ticker quickly.
var reviewRequestPollInterval = 10 * time.Second

// reviewRequestTickBudget is how long one watcher tick may wait for its
// first pacing slot before the tick is shed to the next one.
const reviewRequestTickBudget = 5 * time.Second

// ReviewRequest is the JSON an agent writes to ReviewRequestDir. Event selects
// the review type: "approve" | "request_changes" | "comment" |
// "resolve_thread". Body is required for request_changes/comment (GitHub
// rejects an empty non-approve review) and optional for approve.
//
// On a pull request this hive did not open, "approve" and "request_changes"
// are rewritten to a COMMENT before they reach GitHub
// (review_contributor_guard.go, hivecommons/hive#9590).
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
	// Revise asks the relay to update the hive's existing review on this PR
	// instead of adding another one. Only honored for repos in
	// Review.ReviseRepos; see review_revise.go.
	Revise bool `json:"revise,omitempty"`
}

// ReviewResponse is written next to a consumed request as <name>.result.json.
type ReviewResponse struct {
	OK       bool   `json:"ok"`
	Number   int    `json:"number,omitempty"`
	State    string `json:"state,omitempty"`
	ThreadID string `json:"thread_id,omitempty"`
	Error    string `json:"error,omitempty"`
	// Note explains a successful result that did less than asked — the
	// per-head cap recording a verdict but suppressing its comment.
	Note string `json:"note,omitempty"`
	At   string `json:"at"`
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
	// Capture the intervals BEFORE spawning: the goroutine's first read of
	// the package-level vars races with a test's fastTick cleanup restoring
	// them (the race detector flagged exactly that on v4 CI). Reading them
	// here is sequenced with the caller.
	poll, fallback := reviewRequestPollInterval, reviewRequestFallbackInterval
	dir, spools := reviewRequestDir(), sandboxReviewSpoolsDir()
	// Event-driven since #13378: a request-file drop (fsnotify) or a
	// review-shaped webhook delivery (reviewRequestWakeEvent) wakes the
	// loop; the timer is a reconcile that runs every fallback while webhooks
	// are healthy and every poll while they are degraded or the dir cannot be
	// watched. The wake channel is per client, so a delivery handled by
	// HandleWebhookEvent reaches this loop without a package-level seam.
	wake := c.reviewWake.c()
	dirNotify, stopNotify := watchReviewRequestDir(dir, c.logger)
	go func() {
		defer close(done)
		defer stopNotify()
		t := time.NewTimer(reviewRequestScanInterval(dirNotify != nil && !hasSandboxReviewSpools(spools), poll, fallback))
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			case <-wake:
			case <-dirNotify:
			}
			// A cancelled ctx can lose the select to an already-ready tick
			// (select picks randomly among ready cases), letting the loop
			// process one more scan after cancellation. Fail the tick
			// closed so cancel means no further processing.
			if ctx.Err() != nil {
				return
			}
			c.reviewRequestTick(ctx, nowFn)
			if !t.Stop() {
				select {
				case <-t.C:
				default:
				}
			}
			// A registered sandbox spool keeps the poll cadence: its writes
			// land on a shared volume, often from another node, where no
			// change notification reaches this pod.
			t.Reset(reviewRequestScanInterval(dirNotify != nil && !hasSandboxReviewSpools(spools), poll, fallback))
		}
	}()
	c.logger.Info("review-request watcher started", slog.String("dir", dir),
		slog.Bool("dir_notify", dirNotify != nil),
		slog.Duration("webhook_fallback", fallback), slog.Duration("degraded_poll", poll))
	return done
}

// reviewRequestTick is one scheduled or woken pass of the watcher.
// Housekeeping cadence against the pacer (#13382): a pass whose requests
// would queue behind the governor's enumeration during a caution window is
// shed; the next wake or timer retries it.
func (c *Client) reviewRequestTick(ctx context.Context, nowFn func() time.Time) {
	if ShedLowPriorityTick("review-request-watcher", reviewRequestTickBudget) {
		return
	}
	c.processReviewRequests(WithPacingClass(ctx, PacingLow), nowFn)
}

// processReviewRequests drains the pod-local drop box, then every sandbox
// review spool (review_request_sandbox_spool.go).
func (c *Client) processReviewRequests(ctx context.Context, nowFn func() time.Time) {
	c.processReviewRequestDir(ctx, reviewRequestDir(), reviewRequestSource{}, nowFn)
	c.processSandboxReviewSpools(ctx, nowFn)
}

func (c *Client) processReviewRequestDir(ctx context.Context, dir string, src reviewRequestSource, nowFn func() time.Time) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !isReviewRequestFile(e) {
			continue
		}
		c.handleOneReviewRequest(ctx, filepath.Join(dir, e.Name()), src, nowFn)
	}
}

// ProcessReviewRequestsOnce runs a single scan+process pass. Test/CLI entry.
func (c *Client) ProcessReviewRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processReviewRequests(ctx, time.Now)
}

func (c *Client) handleOneReviewRequest(ctx context.Context, path string, src reviewRequestSource, nowFn func() time.Time) {
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
	// A sandbox spool's requests belong to the agent its manifest names,
	// whatever the file says (hivecommons/hive#14986).
	if src.agent != "" {
		req.Agent = src.agent
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
	case !recordOnly && !resolveThread && strings.TrimSpace(req.Body) != "":
		// A finding whose evidence is text a secret scrubber already masked
		// is wrong by construction (hivecommons/hive#8067). Refuse it here
		// rather than publish it: the quoted line is not what the file says,
		// and a maintainer cannot tell that from the comment. See
		// review_redaction_guard.go.
		shapeErr = redactedQuoteRefusal(req.Body)
	}
	// A verdict that will not validate is refused BEFORE anything is posted.
	// Recording it after the comment went out used to log a warning the agent
	// never saw and drop the verdict: the PR then read as never reviewed, was
	// handed back on the next kick, and collected the same comment again —
	// six near-identical duplicate notices on one PR in two hours. Failing
	// the whole request instead keeps the two artifacts atomic and puts the
	// validator's message where the agent reads it, so it can fix the JSON
	// and resubmit inside the same kick.
	if shapeErr == "" && strings.TrimSpace(req.Report) != "" {
		if _, err := review.ValidateReportsFor([]byte(strings.TrimSpace(req.Report)), c.perspectives); err != nil {
			shapeErr = malformedVerdictRefusal(err)
		}
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
	if src.agent != "" {
		// A staged spool file is owned by hive, not the agent: its owning
		// UID proves nothing. The proving was done at ingest instead: the
		// manifest names the agent, and the request's MAC under the run's
		// spool key shows that agent's sandbox wrote it
		// (ingestSandboxSpool). 0 is the authorizer's "no owning UID" value.
		fileUID = 0
	}
	if c.reviewAuthz == nil {
		c.denyReviewRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.reviewAuthz(req.Agent, fileUID); err != nil {
		c.denyReviewRequest(path, req, err.Error(), nowFn)
		return
	}
	// Lane write allowlist (#9587), keyed on the now-authorized agent name.
	reviewOp := WriteOpReview
	if resolveThread {
		reviewOp = WriteOpResolveThread
	}
	if reason, refused := c.refuseWrite(req.Agent, reviewOp, req.Repo, req.Number); refused {
		c.denyReviewRequest(path, req, reason, nowFn)
		return
	}

	// Reviews use the hive's credentials and bypass the agent proxy, just like
	// the other write relays. Pause applies to every review event.
	if c.RepoIsPaused(req.Repo) {
		c.denyReviewRequest(path, req, RepoPausedReason(req.Repo), nowFn)
		return
	}
	// review.require_labels (PR approval allow-list): the server-side half of
	// the gate. The kick lists never offer an unlabelled PR, but a reviewer
	// that found one anyway — or was told to by a prompt — is refused here,
	// whatever the event: a review, a thread reply, a resolution or a bare
	// record_verdict are all "the hive reviewed this PR". Labels are read
	// fresh with the App token; a fetch failure is retried, not waved through.
	if admitted, err := c.reviewLabelGateAdmits(ctx, req.Repo, req.Number); err != nil {
		c.retryOrQuarantineReview(path, req, fmt.Errorf("review.require_labels: could not read PR labels: %w", err), nowFn)
		return
	} else if !admitted {
		c.recordWriteAudit(AuditActionReviewLabelGateRefused, c.attributionMeta(req.Agent),
			WriteTarget{Repo: req.Repo, Number: req.Number},
			"event", strings.TrimSpace(req.Event), "require_labels", strings.Join(c.reviewRequireLabels(), ","))
		c.denyReviewRequest(path, req, config.ReviewRequireLabelsSkipReason+" ("+strings.Join(c.reviewRequireLabels(), ", ")+")", nowFn)
		return
	}
	if resolveThread || threadReply {
		c.handleReviewThreadRequest(ctx, path, req, threadID, resolveThread, nowFn)
		return
	}

	sentinelBlockNote := ""
	if apiEvent == "APPROVE" {
		if blocked, err := c.sentinelReviewBlockReason(ctx, req.Repo, req.Number); err != nil {
			c.denyReviewRequest(path, req, "sentinel approval block could not read PR labels: "+err.Error(), nowFn)
			return
		} else if blocked != "" {
			meta := c.attributionMeta(req.Agent)
			c.recordWriteAudit(AuditActionSentinelBlockedApproval, meta,
				WriteTarget{Repo: req.Repo, Number: req.Number},
				"state", "approved", "label", blocked)
			if strings.TrimSpace(req.Body) == "" {
				c.recordReviewEvidence(ctx, req, nil, nil, nowFn())
				c.writeReviewResult(path, ReviewResponse{
					OK:     true,
					Number: req.Number,
					State:  ReviewEventRecordVerdict,
					Note:   fmt.Sprintf("approval suppressed: %s label requires human review", blocked),
					At:     nowFn().UTC().Format(time.RFC3339),
				})
				_ = os.Remove(path)
				c.reviewRetries.clear(path)
				c.logger.Warn("review-request watcher: approval suppressed by sentinel label",
					slog.String("agent", req.Agent), slog.String("repo", req.Repo),
					slog.Int("number", req.Number), slog.String("label", blocked))
				return
			}
			apiEvent, state, _ = reviewEventToAPI("comment")
			req.Report = ""
			sentinelBlockNote = fmt.Sprintf("approval downgraded to COMMENT: %s label requires human review", blocked)
			c.logger.Warn("review-request watcher: approval downgraded by sentinel label",
				slog.String("agent", req.Agent), slog.String("repo", req.Repo),
				slog.Int("number", req.Number), slog.String("label", blocked))
		}
	}

	// COMMENT-only on contributor PRs (hivecommons/hive#9590). The hive
	// reviews everyone's work but adjudicates only its own: an APPROVE or
	// REQUEST_CHANGES aimed at a PR this hive did not open becomes a COMMENT
	// here, whatever the reviewer asked for. See review_contributor_guard.go.
	commentOnlyNote := ""
	apiEvent, state, commentOnlyNote = c.enforceCommentOnlyForContributorPR(ctx, req, apiEvent, state)
	if commentOnlyNote != "" {
		// COMMENT is the one review event GitHub refuses with an empty body,
		// and a downgraded APPROVE may have had none.
		req.Body = contributorCommentBody(req.Body)
	}

	// Per-head backstop. Everything above depends on the agent doing the
	// right thing; this does not. A head that already carries as many hive
	// reviews as it can legitimately receive (one in combined mode, one per
	// perspective otherwise) gets no further top-level review comment from
	// an agent unless it is revising its own (--revise) or replying in a
	// thread. This is the guard that would have stopped 21 reviews landing
	// on one head of actions#548 in a day whatever the reviewer's prompt said.
	//
	// The COMMENT is what is suppressed, not the judgement. The verdict is
	// still recorded (it may be the dispatch lane's, binding this head's
	// pending entries); dropping it with the comment would leave the PR
	// pending forever with nothing left to post. The result is ok:true with
	// state record_verdict and a note, so the agent's own loop is done.
	if !recordOnly && !req.Revise && apiEvent != "APPROVE" {
		if reason := c.perHeadReviewRefusal(ctx, req); reason != "" {
			c.recordReviewEvidence(ctx, req, c.recordReviewVerdict(ctx, req, ""), nil, nowFn())
			if err := c.fileOutOfScopeReviewBacklog(ctx, req, nowFn().UTC()); err != nil {
				c.logger.Warn("review-request watcher: could not file out-of-scope backlog",
					slog.String("repo", req.Repo), slog.Int("number", req.Number),
					slog.String("error", err.Error()))
			}
			c.writeReviewResult(path, ReviewResponse{OK: true, Number: req.Number, State: ReviewEventRecordVerdict, Note: "comment suppressed: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
			_ = os.Remove(path)
			c.reviewRetries.clear(path)
			c.logger.Warn("review-request watcher: comment suppressed by per-head cap, verdict recorded",
				slog.String("agent", req.Agent), slog.String("repo", req.Repo),
				slog.Int("number", req.Number), slog.String("reason", reason))
			return
		}
	}

	// Authorized and well-formed, but nothing to post: record and finish
	// without touching the GitHub API.
	if recordOnly {
		c.recordReviewEvidence(ctx, req, c.recordReviewVerdict(ctx, req, ""), nil, nowFn())
		if err := c.fileOutOfScopeReviewBacklog(ctx, req, nowFn().UTC()); err != nil {
			c.logger.Warn("review-request watcher: could not file out-of-scope backlog",
				slog.String("repo", req.Repo), slog.Int("number", req.Number),
				slog.String("error", err.Error()))
		}
		c.writeReviewResult(path, ReviewResponse{OK: true, Number: req.Number, State: ReviewEventRecordVerdict, At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Remove(path)
		c.reviewRetries.clear(path)
		c.logger.Info("review-request watcher: verdict recorded without a comment",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent))
		return
	}

	meta := c.attributionMeta(req.Agent)
	body := leadWithDecisionQuestion(c.relayBody(req.Agent, req.Body))
	if c.confidenceScoreOn() {
		body = appendConfidenceLine(body, req.Report, c.perspectives)
	}
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
	decisionDeduped := false
	ownPRNote := ""
	if leak, ok := c.scanCanaryText(req.Body, "hive-review:"+req.Repo); ok && c.canaryFailClosed {
		err = fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
	} else if req.Revise {
		// Correct the record rather than shouting over it. A revision that
		// finds no prior review of its own falls through to posting
		// normally: a first review is not a revision.
		var revised bool
		created, revised, err = c.reviseReview(ctx, req, body)
		if err == nil && created == nil && !revised {
			created, _, err = c.client.PullRequests.CreateReview(ctx, owner, repoName, req.Number, reviewReq)
		}
	} else if existing := c.findOwnOpenDecisionReview(ctx, owner, repoName, req.Number, req.Body); existing != nil && apiEvent == "COMMENT" {
		// The same decision question is already standing on this PR from
		// an earlier head (hivecommons/hive#15499): refresh that review in
		// place rather than asking the person again. The verdict for the
		// new head is still recorded below.
		created, err = c.refreshDecisionReview(ctx, owner, repoName, req.Number, existing, body)
		if err == nil {
			decisionDeduped = true
		}
	} else {
		provenance := owner + "/" + repoName + "#" + strconv.Itoa(req.Number)
		_, err = effects.ExecuteReconciling(ctx, c.mutationBoundary(), effects.Claim{
			Repo:   owner + "/" + repoName,
			Kind:   effects.KindReviewSubmit,
			Target: strconv.Itoa(req.Number),
			Actor:  req.Agent,
			// The request file is the unit of intent: retries of one file must
			// dedupe, but two files carrying byte-identical text (a second
			// "no findings" pass on a newer head) are two reviews, not a
			// replay. Without this the journal refused the second one as
			// already applied and the request retried until quarantine.
			Inputs: map[string]string{"event": apiEvent, "body": effects.StableDigest(body), "request": filepath.Base(path)},
		}, func(ctx context.Context) (effects.ExternalState, error) {
			return c.observeReviewSubmit(ctx, owner, repoName, req.Number, body, "", provenance)
		}, effects.DefaultUnresolvedTTL, func(ctx context.Context) (effects.Result, error) {
			var apiErr error
			for {
				created, _, apiErr = c.client.PullRequests.CreateReview(ctx, owner, repoName, req.Number, reviewReq)
				if !IsOwnPullRequestReviewRefusal(apiErr) || ownPRNote != "" {
					return effects.Result{Provenance: provenance}, apiErr
				}
				// GitHub never lets the App approve or request changes on
				// its own pull request (422), and no retry changes that:
				// post the same text once more as a COMMENT review so the
				// verdict still lands on the PR (hivecommons/hive#15666).
				// The verdict itself is recorded below as asked.
				ownPRNote = fmt.Sprintf("%s downgraded to COMMENT: GitHub refuses a review on the App's own pull request", apiEvent)
				commentEvent, _, _ := reviewEventToAPI("comment")
				reviewReq.Event = gh.Ptr(commentEvent)
				if reviewReq.Body == nil {
					reviewReq.Body = gh.Ptr(contributorCommentBody(""))
				}
			}
		})
	}
	if errors.Is(err, errReviseNotAllowlisted) {
		// Policy, not a transient fault (#13252): the repo is not in
		// review.revise_repos, and no retry changes that. Deny once and move
		// the file aside so it is not re-attempted on every tick.
		c.denyReviewRequest(path, req, err.Error(), nowFn)
		return
	}
	if err == nil && ownPRNote != "" {
		_, state, _ = reviewEventToAPI("comment")
		c.logger.Warn("review-request watcher: own-PR review refused by GitHub, posted as COMMENT",
			slog.String("agent", req.Agent), slog.String("repo", req.Repo), slog.Int("number", req.Number))
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
	if commentOnlyNote != "" {
		resp.Note = commentOnlyNote
	}
	if ownPRNote != "" {
		resp.Note = ownPRNote
	}
	if sentinelBlockNote != "" {
		resp.Note = sentinelBlockNote
	}
	if decisionDeduped {
		resp.Note = decisionDedupedNote
	}

	// Record where the review landed so the queue views can link to it. A
	// failure here is logged and swallowed: the review is already posted, and
	// losing its address must never turn a successful review into a retry
	// that posts it a second time.
	if created != nil {
		if err := RecordReviewLink("", req.Repo, req.Number, ReviewLink{
			URL:   created.GetHTMLURL(),
			State: state,
			// The CREATED review's commit_id, not the request's: the request
			// never sets one, so the ledger recorded every review with an
			// empty head and could not tell a re-review of the same head from
			// a review of a new one.
			HeadSHA: created.GetCommitID(),
			At:      nowFn().UTC(),
		}); err != nil {
			c.logger.Warn("review-request watcher: could not record review link",
				slog.String("repo", req.Repo), slog.Int("number", req.Number),
				slog.String("error", err.Error()))
		}
	}

	// Persist the structured verdict now that the comment is posted. The two
	// artifacts are recorded together so a verdict can never be attributed to a
	// review that never actually landed. The evidence bundle for the head
	// records the same two things (review_evidence.go).
	c.recordReviewEvidence(ctx, req, c.recordReviewVerdict(ctx, req, ""), created, nowFn())
	if err := c.fileReviewBacklog(ctx, req, nowFn().UTC(), created); err != nil {
		c.logger.Warn("review-request watcher: could not file out-of-scope backlog",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("error", err.Error()))
	}

	c.recordWriteAudit(AuditActionPRReviewed, meta,
		WriteTarget{Repo: req.Repo, Number: req.Number},
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
		body := c.relayBody(req.Agent, req.Body)
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

	c.recordWriteAudit(AuditActionPRReviewed, meta,
		WriteTarget{Repo: req.Repo, Number: req.Number},
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

// appendConfidenceLine adds the derived mergeability score to a review comment
// (hivecommons/hive#8182). It is computed from the verdict file the agent
// hands over with the comment — the same reports the collector routes on — so
// the number a maintainer reads and the one in review-verdicts.json can never
// disagree. A request without a parseable verdict gets no line: the score is
// a function of the verdict, and a guess would be worse than silence. The
// line goes above the attribution trailer so it reads as part of the review.
func appendConfidenceLine(body, rawReport string, set review.PerspectiveSet) string {
	raw := strings.TrimSpace(rawReport)
	if raw == "" {
		return body
	}
	reports, err := review.ValidateReportsFor([]byte(raw), set)
	if err != nil || len(reports) == 0 {
		return body
	}
	conf := review.ScoreConfidence(review.ScopeEffectiveReports(reports), set.Len())
	line := conf.Render()
	// A redundant PR is not bad code: name the reason instead of a 0/5.
	if of := redundantOfFromBody(body); of != "" && conf.Score < review.ConfidenceSafeFloor {
		line = "**no-op merge: duplicate of " + of + "**"
	}
	body = strings.TrimRight(body, "\n")
	if body == "" {
		return line
	}
	return body + "\n\n" + line
}

// redundantOfFromBody reads the PR a review marked as already containing this
// change (`<!-- hive-redundant-of: #334 -->`), or "" when it did not.
func redundantOfFromBody(body string) string {
	return hiddenMarkerValue(body, reviewRedundantOfMarker)
}

// leadWithDecisionQuestion renders the question carried by the
// `<!-- hive-decision-question: ... -->` marker as visible text at the top of
// the comment (hivecommons/hive#13116). GitHub hides the marker, which stays
// untouched for machine parsing.
func leadWithDecisionQuestion(body string) string {
	question := decisionQuestionFromBody(body)
	if question == "" || strings.Contains(body, "**Decision needed:** "+question) {
		return body
	}
	return "**Decision needed:** " + question + "\n\n" + body
}

// malformedVerdictRefusal is the result an agent reads when its --verdict-file
// does not validate. It carries the validator's exact complaint and a complete
// example object, because the agent that produced the bad shape is the one
// that has to fix it, in this kick, from this message alone.
func malformedVerdictRefusal(err error) string {
	return "verdict rejected, nothing posted: " + err.Error() +
		". Fix the JSON and resubmit the same comment with it. One object per perspective, or an array of them, shaped exactly like: " +
		review.VerdictSchemaExample
}

// perHeadReviewRefusal returns a denial reason when the PR's current head has
// already received the maximum number of hive reviews, or "" to proceed. It
// reads the head from GitHub (one GET) rather than trusting the verdict's
// head_sha, which the agent writes. A failed GET or an unreadable ledger
// proceeds: this is a backstop against a loop, not the primary gate, and a
// GitHub blip must not turn into a lost review.
func (c *Client) perHeadReviewRefusal(ctx context.Context, req ReviewRequest) string {
	limit := c.maxReviewsPerHead()
	if limit <= 0 {
		return ""
	}
	links, err := LoadReviewLinks("")
	if err != nil {
		return ""
	}
	link, ok := links[ReviewLinkKey(req.Repo, req.Number)]
	if !ok || link.HeadSHA == "" || link.HeadCount < limit {
		return ""
	}
	owner, repoName := c.splitRepo(req.Repo)
	pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_request_watcher"), owner, repoName, req.Number)
	if err != nil || pr.GetHead().GetSHA() == "" {
		return ""
	}
	if !strings.EqualFold(pr.GetHead().GetSHA(), link.HeadSHA) {
		return ""
	}
	return fmt.Sprintf("head %s of %s#%d already carries %d hive review(s) (limit %d): %s — pass --revise to update the existing review, --thread to reply in it, or wait for the author to push",
		link.HeadSHA[:min(7, len(link.HeadSHA))], req.Repo, req.Number, link.HeadCount, limit, link.URL)
}

// maxReviewsPerHead is how many top-level hive reviews one head may receive:
// the operator's review.max_reviews_per_head when set, else 1 in combined
// mode (one session, one comment) and one per perspective otherwise.
func (c *Client) maxReviewsPerHead() int {
	if c.maxReviewsPerHeadFn != nil {
		if n := c.maxReviewsPerHeadFn(); n != 0 {
			return n
		}
	}
	if c.combinedPerspectivesFn != nil && c.combinedPerspectivesFn() {
		return 1
	}
	return c.perspectives.Len()
}
