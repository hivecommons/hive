package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/effects"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/logscrub"
)

// IssueRequestDir is where agents drop issue-create and comment requests. An
// agent's `gh issue create` (via the gh wrapper → hive-open-issue) writes a
// request file here INSTEAD of calling the GitHub API from the agent's shell.
// The hive's watcher performs the write with the App installation token —
// server-side, retried, and immune to the agent shell-tool timeouts, GHE
// secondary-rate-limit stalls, and multiline-command mangling that made direct
// `gh issue create` silently lose findings (root-caused live 2026-08-21: the
// sec-check agent's creates timed out mid-flight and the finding survived only
// as a bead).
const IssueRequestDir = "/var/run/hive-metrics/issue-requests"

// issueRequestDirForTest lets tests point the watcher at a temp dir.
var issueRequestDirForTest string

func issueRequestDir() string {
	if issueRequestDirForTest != "" {
		return issueRequestDirForTest
	}
	return IssueRequestDir
}

// issueRequestPollInterval mirrors prRequestPollInterval: issue creation is not
// latency-critical (the agent has already recorded the finding and moved on).
// A var (not const) so tests can drive the real ticker loop quickly.
var issueRequestPollInterval = 10 * time.Second

// issueRetryBase and issueRetryMax bound the per-request retry backoff: a
// persistently failing issue request backs off exponentially so a
// poisoned-but-parseable request cannot hammer the forge every 10 seconds for
// the life of the pod. The PR-open and review watchers apply the same policy
// via the shared retryTracker (request_retry.go).
const (
	issueRetryBase         = 30 * time.Second
	issueRetryMax          = 15 * time.Minute
	issueRetryRateLimitMax = time.Hour
)

// issueRequestMaxAge is the give-up horizon: a request that still has not
// succeeded after this long is quarantined (.failed) with its last error, so
// the queue cannot grow without bound. 24h spans any plausible GHE outage
// while keeping the directory finite.
const issueRequestMaxAge = 24 * time.Hour

// IssueRequest is the JSON an agent writes to IssueRequestDir. Kind selects the
// operation: "issue" (default) creates an issue; "comment" posts a comment on
// an existing issue or PR (Number required); "claim" marks an issue as taken by
// the agent (Number required) — it applies a `hive/claimed-by-<agent>` LABEL
// rather than a GitHub assignee, because the hive authors as an App bot and App
// bots are not valid GitHub assignees (Issues.AddAssignees silently drops them);
// "close" closes an issue only after the reporter-confirmation gate passes or
// OverrideReason records an explicit maintainer override; "label" adds and/or
// removes plain labels on an existing issue or PR (Number required), refusing
// the hive-controlled labels listed in issue_request_label.go;
// "request_review" asks users and/or teams to review an existing PR (Number
// required), validated in issue_request_review.go.
type IssueRequest struct {
	Kind   string   `json:"kind,omitempty"` // "issue" (default) | "comment" | "claim" | "close" | "label" | "request_review"
	Repo   string   `json:"repo"`
	Title  string   `json:"title,omitempty"` // issue only
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"` // issue: labels to create with; label: labels to add
	Number int      `json:"number,omitempty"` // comment/claim/close/label/request_review: issue/PR number
	Agent  string   `json:"agent,omitempty"`
	// RemoveLabels are the labels a "label" request takes OFF the item. It may
	// be combined with Labels in one request (an add and a remove of the same
	// state change), and is ignored by every other kind.
	RemoveLabels []string `json:"remove_labels,omitempty"` // label only
	// Reviewers and TeamReviewers are the user logins and team slugs a
	// "request_review" request asks to review the PR. At least one is
	// required; both are ignored by every other kind.
	Reviewers     []string `json:"reviewers,omitempty"`      // request_review only
	TeamReviewers []string `json:"team_reviewers,omitempty"` // request_review only
	// OverrideReason is required to deliberately close a human-filed bug-family
	// issue before reporter confirmation (duplicate, not-a-bug, reporter asked).
	OverrideReason string `json:"override_reason,omitempty"` // close only
	// Parent is an optional issue number in the same repo. When set on an
	// "issue" request, the watcher links the newly created issue to it as a
	// GitHub sub-issue (POST .../issues/{parent}/sub_issues) after creating it,
	// so a split-out child shows up on the parent's sub-issue list and
	// progress bar instead of only a "Part of #N" line in the body
	// (hivecommons/hive#9435). A failure to link does not fail the create.
	Parent int `json:"parent,omitempty"` // issue only
	// BlockedBy lists issue numbers in the same repo that must close before
	// this one is ready. On an "issue" request the watcher records each as a
	// GitHub "blocked by" dependency of the new issue after creating it
	// (hivecommons/hive#9839), so a split's order lives where enumeration can
	// read it instead of only in prose. A failed link does not fail the
	// create; linked and failed blockers are both reported in the result.
	BlockedBy []int `json:"blocked_by,omitempty"` // issue only
	// NeedsDecision parks the new issue on a maintainer decision
	// (hivecommons/hive#11215). On an "issue" request the watcher adds the
	// hive's configured needs-decision label (hard_suppress_labels.
	// needs_decision) to the create itself, so an issue that asks the
	// maintainer to choose gets the "What to reply" notice and stays out of
	// the actionable queue whatever labels the agent named.
	NeedsDecision bool `json:"needs_decision,omitempty"` // issue only
}

// IssueResponse is written next to a consumed request as <name>.result.json.
type IssueResponse struct {
	OK             bool   `json:"ok"`
	Number         int    `json:"number,omitempty"`
	URL            string `json:"url,omitempty"`
	AlreadyExisted bool   `json:"already_existed,omitempty"`
	// Consolidated reports that no issue was created because an OPEN
	// agent-filed issue already references exactly the same files (#9376):
	// the finding was posted there as a comment instead, so the two land in
	// one PR rather than two PRs that conflict. Number/URL name that issue.
	Consolidated bool `json:"consolidated,omitempty"`
	// RejectedDuplicate reports that no issue was created because a maintainer
	// recently closed an agent-filed issue covering the same file set as
	// not-planned or duplicate (#6463). Number/URL point at that closed issue
	// so the agent can read the rebuttal instead of re-litigating it.
	RejectedDuplicate bool   `json:"rejected_duplicate,omitempty"`
	Error             string `json:"error,omitempty"`
	At                string `json:"at"`
	// LabelsAdded and LabelsRemoved report what a "label" request changed, in
	// the order it was applied, so the agent can confirm the end state without
	// re-reading the item.
	LabelsAdded   []string `json:"labels_added,omitempty"`
	LabelsRemoved []string `json:"labels_removed,omitempty"`
	// ReviewersRequested and TeamReviewersRequested report who a
	// "request_review" request asked, after normalization.
	ReviewersRequested     []string `json:"reviewers_requested,omitempty"`
	TeamReviewersRequested []string `json:"team_reviewers_requested,omitempty"`
	// ParentLinked reports that the created issue was linked as a GitHub
	// sub-issue of req.Parent. ParentLinkError carries the (non-fatal) reason
	// when a requested link did not succeed; the issue is still created OK.
	ParentLinked    bool   `json:"parent_linked,omitempty"`
	ParentLinkError string `json:"parent_link_error,omitempty"`
	// BlockedByLinked lists the req.BlockedBy numbers that were recorded as
	// GitHub "blocked by" dependencies of the created issue; BlockedByErrors
	// names each one that could not be, with the reason (#9839).
	BlockedByLinked []int    `json:"blocked_by_linked,omitempty"`
	BlockedByErrors []string `json:"blocked_by_errors,omitempty"`
}

// IssueRequestAuthorizer mirrors PRRequestAuthorizer: it receives the claimed
// agent name, the request-file owning UID, and the request kind ("issue" or
// "comment"), and returns nil to authorize. A nil authorizer denies everything
// (fail closed).
type IssueRequestAuthorizer func(agent string, fileUID int, kind string) error

// issueRetryState tracks in-memory backoff per request path. Reset on pod
// restart — acceptable: a restart retries everything once, then backs off again.
type issueRetryState struct {
	attempts int
	nextTry  time.Time
	firstTry time.Time
}

// StartIssueRequestWatcher runs the loop that executes issue/comment requests
// dropped in IssueRequestDir. Same contract as StartPRRequestWatcher: returns
// immediately, runs until ctx cancel, nil client is a no-op (requests
// accumulate rather than silently dropping), nil authz fails closed.
//
// The returned channel closes when the watcher goroutine has exited (or
// immediately when it never starts), so callers — tests above all — can JOIN
// the loop after cancelling instead of sleeping and hoping: an unjoined
// watcher outliving its test races the test's global-seam restores.
func (c *Client) StartIssueRequestWatcher(ctx context.Context, authz IssueRequestAuthorizer, nowFn func() time.Time) <-chan struct{} {
	done := make(chan struct{})
	if c == nil {
		close(done)
		return done
	}
	c.issueAuthz = authz
	if nowFn == nil {
		nowFn = time.Now
	}
	// Shared with PrepareRequestDirs, which creates this queue unconditionally
	// at boot so findings can accumulate even before a watcher runs.
	dir := issueRequestDir()
	if !ensureRequestDir(c.logger, "issue", dir) {
		close(done)
		return done
	}
	// Capture the poll interval BEFORE spawning: the goroutine's first read of
	// the package-level interval races with a test's fastTick cleanup restoring
	// it (the race detector flagged exactly that on v4 CI). Reading it here is
	// sequenced with the caller, and the loop only ever needed it once anyway.
	interval := issueRequestPollInterval
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
				c.processIssueRequestsInDir(ctx, nowFn, dir)
			}
		}
	}()
	c.logger.Info("issue-request watcher started", slog.String("dir", dir))
	return done
}

func (c *Client) processIssueRequests(ctx context.Context, nowFn func() time.Time) {
	c.processIssueRequestsInDir(ctx, nowFn, issueRequestDir())
}

func (c *Client) processIssueRequestsInDir(ctx context.Context, nowFn func() time.Time, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		path := filepath.Join(dir, name)
		c.handleOneIssueRequest(ctx, path, nowFn)
	}
}

// ProcessIssueRequestsOnce runs a single scan+process pass. Test/CLI entry point.
func (c *Client) ProcessIssueRequestsOnce(ctx context.Context) {
	if c == nil {
		return
	}
	c.processIssueRequests(ctx, time.Now)
}

func (c *Client) issueBackoffAllows(path string, nowFn func() time.Time) bool {
	c.issueRetryMu.Lock()
	defer c.issueRetryMu.Unlock()
	if c.issueRetries == nil {
		c.issueRetries = map[string]*issueRetryState{}
	}
	st := c.issueRetries[path]
	if st == nil {
		return true
	}
	return !nowFn().Before(st.nextTry)
}

// issueNoteFailure records a failed attempt and returns true when the request
// has exceeded its give-up horizon and should be quarantined.
func (c *Client) issueNoteFailure(path string, nowFn func() time.Time, err error) bool {
	c.issueRetryMu.Lock()
	defer c.issueRetryMu.Unlock()
	if c.issueRetries == nil {
		c.issueRetries = map[string]*issueRetryState{}
	}
	now := nowFn()
	st := c.issueRetries[path]
	if st == nil {
		st = &issueRetryState{firstTry: now}
		c.issueRetries[path] = st
	}
	st.attempts++
	backoff := issueRetryBase << uint(min(st.attempts-1, 30))
	if backoff > issueRetryMax || backoff <= 0 {
		backoff = issueRetryMax
	}
	if isRateLimitErr(err) {
		backoff = retryableGitHubDelay(err, now, backoff, issueRetryMax, issueRetryRateLimitMax)
	}
	st.nextTry = now.Add(backoff)
	return now.Sub(st.firstTry) > issueRequestMaxAge
}

func (c *Client) issueClearRetry(path string) {
	c.issueRetryMu.Lock()
	defer c.issueRetryMu.Unlock()
	delete(c.issueRetries, path)
}

func (c *Client) handleOneIssueRequest(ctx context.Context, path string, nowFn func() time.Time) {
	if !c.issueBackoffAllows(path, nowFn) {
		return
	}
	data, _, err := readUntrustedFile(path, requestMaxBytes)
	if err != nil {
		if errors.Is(err, errDropBoxFileRejected) {
			// A FIFO, symlink, or oversize drop can never become a valid
			// request; move it aside so it stops being scanned every tick.
			_ = os.Rename(path, path+".rejected")
			c.logger.Warn("issue-request watcher: REJECTED (unsafe file)",
				slog.String("path", path), slog.String("reason", err.Error()))
		}
		return // vanished between ReadDir and here
	}
	var req IssueRequest
	if err := json.Unmarshal(data, &req); err != nil {
		// A torn read is not a malformed request: leave it for the next tick
		// rather than destroying it. See quarantinable.
		if !quarantinable(path, nowFn()) {
			return
		}
		c.writeIssueResult(path, IssueResponse{OK: false, Error: "invalid JSON: " + err.Error(), At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.issueClearRetry(path)
		c.logger.Warn("issue-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}
	kind := strings.TrimSpace(strings.ToLower(req.Kind))
	if kind == "" {
		kind = "issue"
	}

	// Validate the request shape BEFORE authorizing or touching the API — a
	// structurally hopeless request can never succeed and must not retry.
	var shapeErr string
	switch kind {
	case "issue":
		if strings.TrimSpace(req.Repo) == "" || strings.TrimSpace(req.Title) == "" {
			shapeErr = "issue request requires repo and title"
		} else if placeholder, ok := issueUnsubstitutedTemplatePlaceholder(req.Title); ok {
			shapeErr = "issue request title contains unsubstituted template placeholder " + strconv.Quote(placeholder)
		} else if placeholder, ok := issueUnsubstitutedTemplatePlaceholder(req.Body); ok {
			shapeErr = "issue request body contains unsubstituted template placeholder " + strconv.Quote(placeholder)
		} else if list, ok := issueshape.UnchosenOptionList(req.Body); ok {
			shapeErr = "issue request body leaves the template's option list unchosen " + strconv.Quote(list) + "; pick one value"
		} else if issueBodyHasMisEscapedNewlines(req.Body) {
			shapeErr = "issue request body contains literal newline escape sequences; use real newlines or --body-file"
		}
	case "comment":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Body) == "" {
			shapeErr = "comment request requires repo, number, and body"
		} else if placeholder, ok := issueUnsubstitutedTemplatePlaceholder(req.Body); ok {
			shapeErr = "comment request body contains unsubstituted template placeholder " + strconv.Quote(placeholder)
		} else if issueBodyHasMisEscapedNewlines(req.Body) {
			shapeErr = "comment request body contains literal newline escape sequences; use real newlines or --body-file"
		}
	case "claim", "close":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
			shapeErr = kind + " request requires repo, number, and agent"
		}
	case "label":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
			shapeErr = "label request requires repo, number, and agent"
		} else if len(normalizeLabelList(req.Labels)) == 0 && len(normalizeLabelList(req.RemoveLabels)) == 0 {
			shapeErr = "label request requires at least one label in labels or remove_labels"
		}
	case "request_review":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
			shapeErr = "request_review request requires repo, number, and agent"
		} else {
			shapeErr = reviewRequestShapeError(normalizeReviewers(req.Reviewers), normalizeTeamReviewers(req.TeamReviewers))
		}
	default:
		shapeErr = "unknown kind " + strconv.Quote(kind)
	}
	if shapeErr != "" {
		c.writeIssueResult(path, IssueResponse{OK: false, Error: shapeErr, At: nowFn().UTC().Format(time.RFC3339)})
		_ = os.Rename(path, path+".bad")
		c.issueClearRetry(path)
		c.logger.Warn("issue-request watcher: malformed request quarantined",
			slog.String("path", path), slog.String("reason", shapeErr))
		return
	}

	// Authorize with the same UID forge-resistance + agent-mode gate as the PR
	// watcher. A nil authorizer fails closed. Denials quarantine (.denied) —
	// retrying can never change policy.
	fileUID := statUID(data, path)
	if c.issueAuthz == nil {
		c.denyIssueRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.issueAuthz(req.Agent, fileUID, kind); err != nil {
		c.denyIssueRequest(path, req, err.Error(), nowFn)
		return
	}
	// Lane write allowlist (#9587), keyed on the now-authorized agent name.
	if reason, refused := c.refuseWrite(req.Agent, issueRequestWriteOp(kind), req.Repo, req.Number); refused {
		c.denyIssueRequest(path, req, reason, nowFn)
		return
	}

	// The hive fulfils issues, comments and claims with its own credentials,
	// bypassing the agent proxy. Enforce pause before any of those API calls.
	if c.RepoIsPaused(req.Repo) {
		c.denyIssueRequest(path, req, RepoPausedReason(req.Repo), nowFn)
		return
	}

	// Per-repo agent scope (#6204). Issue creation is not hard-denied at the
	// proxy the way PR-create and merge are, so the proxy DOES gate a direct
	// `gh issue create` — but a request fulfilled here is performed by the
	// hive, whose traffic is exempt from the forced-egress redirect. Gating
	// both paths is what makes the scope a scope rather than a suggestion, and
	// issue noise on repos that never wanted the agent is the specific cost
	// #6204 is about.
	if !c.AgentServesRepo(req.Agent, req.Repo) {
		c.denyIssueRequest(path, req, AgentRepoScopeReason(req.Agent, req.Repo), nowFn)
		return
	}

	// Content gate for the label relay: hive-controlled labels are refused in
	// both directions, before any GitHub call (#9587).
	addLabels := normalizeLabelList(req.Labels)
	removeLabels := normalizeLabelList(req.RemoveLabels)
	if kind == "label" {
		if reason, refused := c.reservedLabelRefusal(addLabels, removeLabels); refused {
			c.denyIssueRequest(path, req, reason, nowFn)
			return
		}
	}

	meta := c.attributionMeta(req.Agent)
	body := c.relayBody(req.Agent, req.Body)
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

	resp := IssueResponse{At: nowFn().UTC().Format(time.RFC3339)}
	closedPR := false
	switch kind {
	case "claim":
		var reason string
		reason, err = c.IssueClaimBlockReason(ctx, req.Repo, req.Number)
		if err != nil {
			break // Retry a failed lookup; never claim on unknown labels.
		}
		if reason != "" {
			c.denyIssueRequest(path, req, "issue claim refused: "+reason, nowFn)
			return
		}
		// A claim right after a hive verification with nothing in between is
		// the re-verify loop, not new work (#12733). Deny it with the reason;
		// a lookup failure falls through to the claim.
		if verified, lookupErr := c.issueLatestCommentIsRecentVerification(ctx, req.Repo, req.Number, nowFn()); lookupErr != nil {
			c.logger.Warn("issue-request watcher: verification lookup before claim failed, claiming anyway",
				slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", lookupErr.Error()))
		} else if verified {
			c.denyIssueRequest(path, req, "issue claim refused: a hive agent verified this issue less than "+
				verificationClaimCooldown.String()+" ago and nothing has changed since; do not re-verify it, and open a PR without a claim if you intend to fix it", nowFn)
			return
		}
		// Apply a namespaced ownership label (App bots can't be assignees)
		// and, when enabled, stage/030-pr-in-progress — the timestamped "an
		// agent started a PR for this" signal (see issue_inprogress.go). The
		// stage goes through the one enforcer and both writes are journaled
		// against a fresh read, so a second claim on an issue whose earlier
		// labels lapsed is written, not replayed (hivecommons/hive#12512).
		err = c.AddLabelsFresh(ctx, req.Repo, req.Number, c.claimStage(), c.inProgressLabelsForClaim(req.Agent)...)
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
			c.markInProgressStarted(ctx, req.Repo, req.Number, req.Agent, nowFn())
		}
	case "comment":
		// A verification marker already on the issue means this comment was
		// posted on an earlier kick: acknowledge it, do not post again
		// (#12733). A failed lookup posts — suppression only on evidence.
		if marker := verificationMarker(req.Body); marker != "" {
			found, lookupErr := c.IssueCommentsContain(ctx, req.Repo, req.Number, marker)
			if lookupErr != nil {
				c.logger.Warn("issue-request watcher: verification-marker lookup failed, posting anyway",
					slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("error", lookupErr.Error()))
			} else if found {
				c.logger.Info("issue-request watcher: verification marker already on issue, not re-posting",
					slog.String("repo", req.Repo), slog.Int("number", req.Number), slog.String("agent", req.Agent), slog.String("marker", marker))
				resp.OK, resp.Number, resp.AlreadyExisted = true, req.Number, true
				break
			}
		}
		err = c.CreateIssueCommentBudgeted(ctx, req.Repo, req.Number, body)
		if errors.Is(err, ErrIssueCommentBudget) {
			c.denyIssueRequest(path, req, "comment refused: "+err.Error()+"; this issue already received its daily quota of hive comments, nothing more is needed on it today", nowFn)
			return
		}
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
		}
	case "label":
		// A re-park on a decision a maintainer already gave is refused; a
		// failed lookup takes the retry path like the claim gate.
		var reason string
		var refused bool
		reason, refused, err = c.reparkDecisionRefusal(ctx, req.Repo, req.Number, addLabels)
		if err != nil {
			break
		}
		if refused {
			c.denyIssueRequest(path, req, reason, nowFn)
			return
		}
		// Adds first, then removes: a request that both adds and removes ends
		// in the state it asked for even when the same automation reads the
		// item in between. A failure leaves the request on the retry path; the
		// GitHub calls are idempotent (adding a present label and removing an
		// absent one are both no-ops), so a retry converges.
		err = c.AddLabels(ctx, req.Repo, req.Number, addLabels)
		if err == nil {
			resp.LabelsAdded = addLabels
			for _, label := range removeLabels {
				if err = c.RemoveLabel(ctx, req.Repo, req.Number, label); err != nil {
					break
				}
				resp.LabelsRemoved = append(resp.LabelsRemoved, label)
			}
		}
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
		}
	case "request_review":
		reviewers := normalizeReviewers(req.Reviewers)
		teams := normalizeTeamReviewers(req.TeamReviewers)
		err = c.RequestReviewers(ctx, req.Repo, req.Number, reviewers, teams)
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
			resp.ReviewersRequested = reviewers
			resp.TeamReviewersRequested = teams
		}
	case "close":
		closedPR, err = c.closeIssue(ctx, req.Repo, req.Number, IssueCloseOptions{OverrideReason: req.OverrideReason})
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
		}
	default: // "issue"
		var res CreateIssueResult
		createLabels := req.Labels
		if req.NeedsDecision {
			createLabels = appendLabelIfMissing(createLabels, c.needsDecisionLabel())
		}
		res, err = c.createIssue(ctx, CreateIssueRequest{Repo: req.Repo, Title: req.Title, Body: body, Labels: createLabels, Origin: req.Agent, ConsolidateOpen: true})
		if err == nil && res.RejectedTwin {
			// Terminal, not retried: a maintainer already rejected this
			// finding, and no amount of retrying changes their verdict
			// (#6463). The result names the closed issue so the agent reads
			// the rebuttal instead of rephrasing the finding next kick.
			resp.OK = false
			resp.RejectedDuplicate = true
			resp.Number = res.Number
			resp.URL = res.URL
			resp.Error = fmt.Sprintf("not filed: a maintainer closed issue #%d (%s) as %q over the same file references within the last 30 days; do not re-file this finding — read the closure rationale and record the rejection instead", res.Number, res.URL, res.RejectedReason)
			c.recordWriteAudit(AuditActionAgentIssueRejectedDuplicate, meta,
				WriteTarget{Repo: req.Repo, Number: res.Number},
				"url", res.URL)
			c.writeIssueResult(path, resp)
			_ = os.Remove(path)
			c.issueClearRetry(path)
			c.logger.Info("issue-request watcher: create refused, maintainer-rejected twin exists",
				slog.String("repo", req.Repo), slog.Int("closed_issue", res.Number),
				slog.String("state_reason", res.RejectedReason), slog.String("agent", req.Agent))
			return
		}
		if err == nil {
			resp.OK = true
			resp.Number = res.Number
			resp.URL = res.URL
			resp.AlreadyExisted = res.AlreadyExisted
			resp.Consolidated = res.Consolidated
			// Best-effort GitHub sub-issue link (#9435): the child issue is
			// already created at this point, so a link failure (parent
			// missing, already at GitHub's sub-issue cap, transient API
			// error, …) is logged and surfaced in the result but never turns
			// a successful create into a failure or a retry.
			if req.Parent > 0 && !res.Consolidated && res.ID > 0 {
				if linkErr := c.AddSubIssue(ctx, req.Repo, req.Parent, res.ID); linkErr != nil {
					resp.ParentLinkError = linkErr.Error()
					c.logger.Warn("issue-request watcher: sub-issue link failed, issue still created",
						slog.String("repo", req.Repo), slog.Int("number", res.Number),
						slog.Int("parent", req.Parent), slog.String("error", linkErr.Error()))
				} else {
					resp.ParentLinked = true
					// #9840: remember that the relay made this link, so the
					// child may inherit the parent's acknowledgement. Only
					// relay-made links are recorded; a sub-issue link anyone
					// adds later in the GitHub UI confers nothing.
					linkOwner, linkRepo := c.splitRepo(req.Repo)
					if recErr := RecordSplitParent("", linkOwner+"/"+linkRepo, res.Number, req.Parent, req.Agent); recErr != nil {
						c.logger.Warn("issue-request watcher: sub-issue linked but split-parent ledger write failed; child will not inherit acknowledgement",
							slog.String("repo", req.Repo), slog.Int("number", res.Number),
							slog.Int("parent", req.Parent), slog.String("error", recErr.Error()))
					}
				}
			}
			// Best-effort GitHub "blocked by" links (#9839), same contract as
			// the parent link: never a failure of the create.
			if len(req.BlockedBy) > 0 && !res.Consolidated && res.Number > 0 {
				resp.BlockedByLinked, resp.BlockedByErrors = c.linkBlockedBy(ctx, req.Repo, res.Number, req.BlockedBy)
				for _, e := range resp.BlockedByErrors {
					c.logger.Warn("issue-request watcher: blocked-by link failed, issue still created",
						slog.String("repo", req.Repo), slog.Int("number", res.Number), slog.String("blocker", e))
				}
			}
		}
	}

	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
		c.writeIssueResult(path, resp)
		gaveUp := c.issueNoteFailure(path, nowFn, err)
		if gaveUp {
			_ = os.Rename(path, path+".failed")
			c.issueClearRetry(path)
			c.logger.Error("issue-request watcher: request exceeded retry horizon, quarantined",
				slog.String("path", path), slog.String("repo", req.Repo), slog.String("error", err.Error()))
			return
		}
		c.logger.Warn("issue-request watcher: request failed, will retry with backoff",
			slog.String("repo", req.Repo), slog.String("kind", kind), slog.String("error", err.Error()))
		return
	}

	if signal := issueRequestStartSignal(kind, req.Body, req.Labels); signal != "" {
		c.notifyAgentStart(req.Agent, req.Repo, req.Number, signal)
	}

	action := AuditActionAgentIssueCreated
	switch kind {
	case "comment":
		action = AuditActionAgentCommentCreated
	case "claim":
		action = AuditActionIssueClaimed
	case "close":
		action = AuditActionIssueClosed
	case "label":
		action = AuditActionAgentLabelApplied
	case "request_review":
		action = AuditActionAgentReviewRequested
	}
	extra := []string{
		"url", resp.URL,
		"reused", strconv.FormatBool(resp.AlreadyExisted),
		"consolidated", strconv.FormatBool(resp.Consolidated),
		"override_reason", req.OverrideReason,
	}
	if kind == "label" {
		extra = []string{
			"added", strings.Join(resp.LabelsAdded, " "),
			"removed", strings.Join(resp.LabelsRemoved, " "),
		}
	}
	if kind == "request_review" {
		extra = []string{
			"reviewers", strings.Join(resp.ReviewersRequested, " "),
			"team_reviewers", strings.Join(resp.TeamReviewersRequested, " "),
		}
	}
	if closedPR {
		// The number was a pull request: this is the hive closing a PR
		// without merging it, counted apart from issue closes.
		action = AuditActionPRClosed
		extra = append(extra, "path", PRAuditPathRelay)
	}
	c.recordWriteAudit(action, meta, WriteTarget{Repo: req.Repo, Number: resp.Number}, extra...)
	c.writeIssueResult(path, resp)
	_ = os.Remove(path)
	c.issueClearRetry(path)
	c.logger.Info("issue-request watcher: request completed",
		slog.String("repo", req.Repo), slog.String("kind", kind),
		slog.Int("number", resp.Number), slog.Bool("reused", resp.AlreadyExisted),
		slog.Bool("consolidated", resp.Consolidated),
		slog.String("agent", req.Agent))
}

// issueRequestWriteOp maps a validated issue-request kind to its write-surface
// operation name (#9587).
func issueRequestWriteOp(kind string) string {
	switch kind {
	case "comment":
		return WriteOpComment
	case "claim":
		return WriteOpClaim
	case "close":
		return WriteOpCloseIssue
	case "label":
		return WriteOpLabel
	case "request_review":
		return WriteOpRequestReview
	default:
		return WriteOpCreateIssue
	}
}

func issueUnsubstitutedTemplatePlaceholder(text string) (string, bool) {
	return issueshape.UnsubstitutedTemplatePlaceholder(text)
}

func issueBodyHasMisEscapedNewlines(body string) bool {
	return issueshape.BodyHasMisEscapedNewlines(body)
}

func (c *Client) denyIssueRequest(path string, req IssueRequest, reason string, nowFn func() time.Time) {
	c.writeIssueResult(path, IssueResponse{OK: false, Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.issueClearRetry(path)
	c.logger.Warn("issue-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo), slog.String("reason", reason))
}

func (c *Client) writeIssueResult(reqPath string, resp IssueResponse) {
	out := strings.TrimSuffix(reqPath, ".json") + ".result.json"
	if b, err := json.MarshalIndent(resp, "", "  "); err == nil {
		_ = writeRequestFile(out, b)
	}
}

// WriteIssueRequest drops a well-formed request file into dir (tests and
// in-process callers).
func WriteIssueRequest(dir string, req IssueRequest) (string, error) {
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

// CreateIssueResult is what CreateIssue returns.
type CreateIssueResult struct {
	Number int
	URL    string
	// ID is the issue's numeric database ID (distinct from Number, its
	// repo-scoped issue number). The sub-issues API takes this as
	// sub_issue_id, so callers linking a fresh child under a parent need it.
	ID int64
	// AlreadyExisted is true when an OPEN issue with the same exact title was
	// already present in the repo, so we returned it instead of creating a
	// duplicate. This makes the watcher's retry loop idempotent: a create that
	// succeeded server-side but crashed before the request file was consumed
	// (or an agent-side "timed out but actually created" ambiguity) never
	// yields a second issue.
	AlreadyExisted bool
	// RejectedTwin is true when no issue was created because a maintainer
	// recently closed an App-bot-filed issue with the same file-reference set
	// as not-planned/duplicate (#6463). Number/URL identify that closed issue
	// and RejectedReason carries its state_reason.
	RejectedTwin   bool
	RejectedReason string
	// Consolidated is true when no issue was created because an OPEN
	// App-bot-filed issue has the same file-reference set (#9376); the
	// finding was appended to it as a comment. AlreadyExisted is also true.
	Consolidated bool
}

// CreateIssueRequest is what every hive issue-create path hands the central
// helper. Origin is required: it names the provenance label the issue carries:
// agent/<lane> for an agent lane, origin/<source> for a non-lane source. A
// label already in Labels that belongs to the non-lane origin family wins over
// Origin, and further origin/* entries are dropped.
type CreateIssueRequest struct {
	// Repo may be "owner/repo" or bare (owner defaults to the hive org).
	Repo   string
	Title  string
	Body   string
	Labels []string
	// Origin is the origin/<source> provenance source (without the prefix).
	// Agent-filed issues pass the agent name; it is reduced to its lane.
	Origin string
	// ConsolidateOpen turns on open-twin consolidation: a finding over the
	// same files as an open hive-filed issue is folded into it (#9376).
	ConsolidateOpen bool
}

// OriginLabel returns the provenance label this request resolves to: an
// existing origin/* label, else agent/<lane> for a known lane or
// origin/<Origin> for a non-lane source.
func (r CreateIssueRequest) OriginLabel() string {
	if label, ok := config.FirstOriginLabel(r.Labels); ok {
		return label
	}
	return config.OriginLabelForAgent(r.Origin)
}

// ErrOriginRequired is returned when a create carries no provenance label and
// names no Origin source.
var ErrOriginRequired = errors.New("CreateIssue: provenance is required (set Origin or pass an origin/* label)")

// CreateIssue creates an issue as the hive's App bot, ensuring requested labels
// exist first and deduping by exact open-issue title. Every issue gets
// exactly one provenance label (see CreateIssueRequest.Origin).
func (c *Client) CreateIssue(ctx context.Context, req CreateIssueRequest) (CreateIssueResult, error) {
	return c.createIssue(ctx, req)
}

// createIssue is the one create path. The agent issue-request watcher turns
// ConsolidateOpen on: agents file findings one at a time and cannot see each
// other's, so two findings about the same files become two issues, two PRs,
// and a merge conflict (#9376). Hive-internal callers (fleet report, review
// backlog, the convergence publisher through the forge seam, and the
// dashboard's ACMM gap issues) also turn it on.
func (c *Client) createIssue(ctx context.Context, req CreateIssueRequest) (CreateIssueResult, error) {
	repo, title, body, consolidateOpen := req.Repo, req.Title, req.Body, req.ConsolidateOpen
	originLabel := req.OriginLabel()
	if originLabel == "" {
		return CreateIssueResult{}, ErrOriginRequired
	}
	labels := config.WithIssueOriginLabel(req.Labels, req.Origin)
	if c == nil || c.client == nil {
		return CreateIssueResult{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	// Before any API call: a structurally impossible repo (an unsubstituted
	// prompt placeholder like "<org>/<target-repo>") would otherwise cost a
	// dedupe lookup, one call per label, and a create — all 404 — and then be
	// retried on the watcher's backoff for up to 24h.
	if err := validateRepoRef(owner, repoName); err != nil {
		return CreateIssueResult{}, fmt.Errorf("CreateIssue: %w", err)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return CreateIssueResult{}, fmt.Errorf("CreateIssue: title is required")
	}
	// Same posture and same placement as validateRepoRef above: a request that
	// is still carrying its template cannot become a useful issue, and unlike a
	// bad repo ref it would SUCCEED, leaving a human to triage an empty report
	// (#7153). Checked before the dedupe lookup so a malformed title never
	// becomes the thing future filings dedupe against.
	if err := validateIssueTemplateFilled(title, body); err != nil {
		return CreateIssueResult{}, fmt.Errorf("CreateIssue: %w", err)
	}
	if leak, ok := c.scanCanaryText(title+"\n"+body, "hive-open-issue:"+repo); ok {
		if c.canaryFailClosed {
			return CreateIssueResult{}, fmt.Errorf("ioscan canary leak detected: agent=%s source=%s", leak.Agent, leak.Source)
		}
	}
	title = logscrub.ScrubString(title)
	body = logscrub.ScrubString(body)

	// Idempotency: list recent open issues and reuse an exact-title match.
	// Issues.ListByRepo is strongly consistent (unlike the Search API, whose
	// index can lag minutes on GHE — useless against a 10s retry loop). The
	// same scan also looks for an open App-bot twin over the same file set,
	// so consolidation costs no extra API calls.
	var wantFiles map[string]bool
	if consolidateOpen {
		if len(c.hiveBotLogins()) > 0 {
			wantFiles = issueFileRefSet(title + "\n" + body)
		} else {
			// No App-bot identity (PAT auth, or app_slug unset): hive-filed
			// issues cannot be told from human ones, so twin, fold and
			// rejected-twin checks stay off rather than risk folding a
			// maintainer's issue. Say so instead of disabling silently.
			c.noBotIdentityWarned.Do(func() {
				c.logger.Warn("CreateIssue: no App-bot login configured, open-twin, component-fold and rejected-twin dedupe are disabled; set github.additional_bot_logins or use a GitHub App")
			})
		}
	}
	var fileSetTwin, componentTwin *gh.Issue
	var componentPath string
	var componentCount, componentClosed int
	var openComponent map[string]int
	var openComponentOldest map[string]*gh.Issue
	if found, err := c.scanOpenIssues(ctx, owner, repoName, title, body, wantFiles); err != nil {
		if isRetryableGitHubError(err) {
			return CreateIssueResult{}, fmt.Errorf("CreateIssue: dedupe lookup failed with retryable error in %s/%s: %w", owner, repoName, err)
		}
		c.logger.Warn("CreateIssue: dedupe lookup failed with terminal error, creating without dedupe",
			slog.String("repo", repoName), slog.String("reason_class", "terminal"), slog.String("error", err.Error()))
	} else if existing := found.subject; existing != nil {
		// A human-filed twin was not created by this hive, so the finding's
		// detail would otherwise vanish: leave it as a comment (#11576).
		if consolidateOpen && len(c.hiveBotLogins()) > 0 && !isHiveBotLogin(c.hiveBotLogins(), existing.GetUser().GetLogin()) {
			if err := c.commentOnHumanFiledTwin(ctx, owner, repoName, existing,
				subjectMatchFindingComment(title, body)); err != nil {
				return CreateIssueResult{}, err
			}
			return CreateIssueResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), ID: existing.GetID(), AlreadyExisted: true, Consolidated: true}, nil
		}
		c.logger.Info("CreateIssue: open issue with the same subject exists, reusing",
			slog.String("repo", repoName), slog.Int("number", existing.GetNumber()),
			slog.String("existing_title", existing.GetTitle()))
		return CreateIssueResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), ID: existing.GetID(), AlreadyExisted: true}, nil
	} else {
		fileSetTwin = found.fileSet
		componentTwin, componentPath, componentCount = found.component, found.componentPath, found.componentCount
		openComponent, openComponentOldest = found.componentCounts, found.componentOldest
	}

	// Rejected-finding gate (#6463): a maintainer who recently closed an
	// agent-filed issue over the same file set as not-planned/duplicate has
	// already answered this finding, however it is reworded this time. A
	// failed lookup files anyway — suppression only on positive evidence.
	if twin, err := c.findRejectedTwin(ctx, owner, repoName, title, body); err != nil {
		c.logger.Warn("CreateIssue: rejected-twin lookup failed, proceeding to create",
			slog.String("repo", repoName), slog.String("error", err.Error()))
	} else if twin != nil {
		c.logger.Info("CreateIssue: refusing create, maintainer rejected an identical-file-set finding",
			slog.String("repo", repoName), slog.Int("closed_issue", twin.GetNumber()),
			slog.String("state_reason", twin.GetStateReason()))
		return CreateIssueResult{
			Number:         twin.GetNumber(),
			URL:            twin.GetHTMLURL(),
			RejectedTwin:   true,
			RejectedReason: twin.GetStateReason(),
		}, nil
	}

	// Open-twin consolidation (#9376): an open agent-filed issue over exactly
	// the same files already owns this work. A second issue would be picked up
	// by a second agent and become a second PR editing the same files, and
	// whichever merges first forces the other to rebase. Fold the finding into
	// the existing issue so one PR handles both.
	if fileSetTwin != nil {
		comment := consolidatedFindingComment(title, body, wantFiles)
		if !isHiveBotLogin(c.hiveBotLogins(), fileSetTwin.GetUser().GetLogin()) {
			if err := c.commentOnHumanFiledTwin(ctx, owner, repoName, fileSetTwin, comment); err != nil {
				return CreateIssueResult{}, err
			}
		} else if err := c.CreateIssueComment(ctx, owner+"/"+repoName, fileSetTwin.GetNumber(), comment); err != nil {
			return CreateIssueResult{}, fmt.Errorf("CreateIssue: consolidating into open issue #%d in %s/%s: %w", fileSetTwin.GetNumber(), owner, repoName, err)
		}
		c.logger.Info("CreateIssue: consolidated finding into open issue over the same file set",
			slog.String("repo", repoName), slog.Int("number", fileSetTwin.GetNumber()),
			slog.String("existing_title", fileSetTwin.GetTitle()))
		return CreateIssueResult{Number: fileSetTwin.GetNumber(), URL: fileSetTwin.GetHTMLURL(), AlreadyExisted: true, Consolidated: true}, nil
	}

	// Same-component fold (#11239): exact file-set equality misses a stream
	// of variants against one file — each bypass cites the shared file plus
	// its own fixture or helper, so every set differs and every variant became
	// its own issue and PR. Once enough open agent-filed issues already cite a
	// path this finding also cites, the pattern is clear: fold the finding
	// into the oldest of them, which acts as the component's tracker.
	//
	// Open issues alone rarely reach the threshold in steady state: each
	// variant's PR merges and closes its issue before the next one is filed.
	// So when some open sibling exists but the open count falls short, App-bot
	// issues closed as completed within sameComponentClosedSiblingWindow count
	// too. The fold target is still the oldest OPEN sibling — a finding is
	// never folded into a closed issue, where nobody would act on it. With no
	// open sibling the finding files normally (no extra API call is made).
	// not_planned/duplicate closures never count: they belong to the
	// rejected-twin gate above.
	if componentTwin == nil && len(openComponent) > 0 {
		closed, err := c.recentlyCompletedComponentCounts(ctx, owner, repoName, wantFiles)
		if err != nil {
			c.logger.Warn("CreateIssue: recently-closed sibling lookup failed, folding on open issues only",
				slog.String("repo", repoName), slog.String("error", err.Error()))
		} else {
			componentTwin, componentPath, componentCount, componentClosed = pickFoldComponent(openComponent, closed, openComponentOldest)
		}
	}
	if componentTwin != nil {
		if err := c.appendFoldedFinding(ctx, owner, repoName, componentTwin.GetNumber(),
			foldedComponentFindingComment(title, body, componentPath, componentCount, componentClosed)); err != nil {
			return CreateIssueResult{}, fmt.Errorf("CreateIssue: folding into open issue #%d in %s/%s: %w", componentTwin.GetNumber(), owner, repoName, err)
		}
		c.foldedTrackers.add(owner+"/"+repoName, componentTwin.GetNumber())
		c.logger.Info("CreateIssue: folded finding into the oldest open issue on the same component",
			slog.String("repo", repoName), slog.Int("number", componentTwin.GetNumber()),
			slog.String("path", componentPath), slog.Int("open_siblings", componentCount),
			slog.Int("recently_closed_siblings", componentClosed))
		return CreateIssueResult{Number: componentTwin.GetNumber(), URL: componentTwin.GetHTMLURL(), AlreadyExisted: true, Consolidated: true}, nil
	}

	// Ensure labels exist. Retryable failures keep the whole request queued
	// because routing labels (agent/*) drive hive filtering, author-kind and
	// duplicate guards. Terminal label failures still degrade to unlabeled,
	// and so does any failure on a non-lane origin/* label: those origins are
	// never gates, so a label-API blip must not block the filing itself.
	var usable []string
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if err := c.ensureCreateIssueLabel(ctx, owner, repoName, l); err != nil {
			if isRetryableGitHubError(err) && !IsOriginLabel(l) {
				return CreateIssueResult{}, fmt.Errorf("CreateIssue: could not ensure label %q in %s/%s due to retryable error: %w", l, owner, repoName, err)
			}
			reasonClass := "terminal"
			if isRetryableGitHubError(err) {
				reasonClass = "retryable"
			}
			c.logger.Warn("CreateIssue: could not ensure label, creating without it",
				slog.String("repo", repoName), slog.String("label", l), slog.String("reason_class", reasonClass), slog.String("error", err.Error()))
			continue
		}
		usable = append(usable, l)
	}

	ghReq := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body)}
	if len(usable) > 0 {
		ghReq.Labels = &usable
	}
	var issue *gh.Issue
	claim := issueCreateClaim(owner+"/"+repoName, title, body, issueCreateNow())
	res, found := c.priorBucketIssueCreate(ctx, claim)
	if !found {
		var err error
		res, err = effects.Execute(ctx, c.mutationBoundary(), claim,
			func(ctx context.Context) (effects.Result, error) {
				var apiErr error
				issue, _, apiErr = c.client.Issues.Create(ctx, owner, repoName, ghReq)
				if apiErr != nil {
					return effects.Result{}, apiErr
				}
				return effects.Result{Provenance: issue.GetHTMLURL()}, nil
			})
		if err != nil {
			return CreateIssueResult{}, fmt.Errorf("creating issue in %s/%s: %w", owner, repoName, err)
		}
	}
	if issue == nil {
		// The mutation boundary replayed this create as already applied
		// (#11579): another filing of the same finding — same canonical
		// subject and file set, however the title was reworded — landed
		// inside the idempotency window (this bucket or the previous one),
		// so the effect never ran and there is no API response. Hand back
		// the journaled issue rather than a zero number with a nil error; an
		// unparsable provenance is an error, never a fabricated result.
		number, parseErr := parseIssueNumberFromURL(res.Provenance)
		if parseErr != nil {
			return CreateIssueResult{}, fmt.Errorf("creating issue in %s/%s: replayed as already applied but recorded provenance %q is not an issue URL: %w", owner, repoName, res.Provenance, parseErr)
		}
		c.logger.Info("CreateIssue: create replayed as already applied, reusing the journaled issue",
			slog.String("repo", repoName), slog.Int("number", number))
		return CreateIssueResult{Number: number, URL: res.Provenance, AlreadyExisted: true}, nil
	}
	c.logger.Info("CreateIssue: issue created as the App bot",
		slog.String("repo", repoName), slog.Int("number", issue.GetNumber()))
	return CreateIssueResult{Number: issue.GetNumber(), URL: issue.GetHTMLURL(), ID: issue.GetID()}, nil
}

// issueCreateIdempotencyWindow bounds how long the mutation journal treats a
// second filing of the same finding as a replay of the first (#11579). The
// window exists to close the scan→create race — two agents whose dedupe scans
// both passed seconds apart — not to suppress a finding for good: a finding
// fixed, closed and genuinely re-introduced months later must file again,
// and the journal otherwise remembers an applied create forever. Within the
// window the open-issue scan and this key agree; past it the scan (open
// twins, rejected twins, same-component streams) is the only guard, as it
// was before.
const issueCreateIdempotencyWindow = 24 * time.Hour

// issueCreateNow is the clock the issue-create claims bucket on; tests pin
// it to place two filings on either side of a bucket boundary.
var issueCreateNow = time.Now

// scanOpenIssuesMaxPages bounds scanOpenIssues to the newest
// scanOpenIssuesMaxPages × 100 open issues (#11579, B.4). Agent-filed issues
// are recent by construction, and an unbounded scan of a busy repo would burn
// API budget on every create. The cost of the bound is known: in a repo with
// more open issues than the window, a tracker older than the window is
// invisible to the scan and a new variant files fresh. The guards that do not
// depend on the window are the rejected-twin gate (its own creator-filtered
// closed-issue query), the same-component fold (recently-closed siblings are
// queried separately) and the create claim (issueCreateClaim), which covers
// the scan→create race regardless of repo size.
const scanOpenIssuesMaxPages = 3

// issueCreateClaim is the effects claim guarding one issue create. It is
// keyed on the finding's canonical subject and file-reference set, not its
// exact title (#11579): a model-authored title varies between two agents
// filing the same finding seconds apart, so an exact-title key let both
// pass. The ledger claim (Target) serialises concurrent creators of one
// finding; the journal inputs make the second one a replay of the first.
//
// Body and labels are deliberately NOT inputs: the body is regenerated on
// every filing and the labels name the filing agent, and either would mint a
// fresh logical operation for what is the same finding. A title with no
// trustworthy canonical subject (see canonicalIssueSubject) and no file
// references falls back to the exact title, which is the pre-#11579 key.
func issueCreateClaim(repo, title, body string, now time.Time) effects.Claim {
	subject := canonicalIssueSubject(title)
	if subject == "" {
		subject = strings.TrimSpace(title)
	}
	files := make([]string, 0)
	for f := range issueFileRefSet(title + "\n" + body) {
		files = append(files, f)
	}
	sort.Strings(files)
	fileSet := strings.Join(files, "\n")
	return effects.Claim{
		Repo:   repo,
		Kind:   effects.KindIssueCreate,
		Target: effects.StableDigest(subject, fileSet),
		Inputs: map[string]string{
			"subject": effects.StableDigest(subject),
			"files":   effects.StableDigest(fileSet),
			"window":  issueCreateIdempotencyBucket(now),
		},
	}
}

// issueCreateIdempotencyBucket is the issueCreateIdempotencyWindow-sized
// bucket now falls in, as a journal input: creates in the same bucket share a
// logical operation, creates in different buckets do not. Buckets are aligned
// to the Unix epoch (00:00Z for the 24h window), so two filings seconds apart
// can straddle a boundary; priorBucketIssueCreate covers that pair.
func issueCreateIdempotencyBucket(now time.Time) string {
	return strconv.FormatInt(now.UTC().Unix()/int64(issueCreateIdempotencyWindow/time.Second), 10)
}

// priorBucketIssueCreate reports the issue the previous idempotency bucket
// already journaled for claim's finding, if any (#11579). The bucket is
// epoch-aligned, so a filing at 23:59:59Z and its twin at 00:00:01Z are
// distinct logical operations and the journal's exact-ID replay alone would
// let the second one create. Probing the previous bucket's key — a read, no
// claim is taken and nothing runs — closes that straddle: any two filings
// less than issueCreateIdempotencyWindow apart are in the same bucket or in
// adjacent ones. A boundary that does not journal never reports a hit, so
// the create proceeds exactly as before.
func (c *Client) priorBucketIssueCreate(ctx context.Context, claim effects.Claim) (effects.Result, bool) {
	prior := claim
	prior.Inputs = make(map[string]string, len(claim.Inputs))
	for k, v := range claim.Inputs {
		prior.Inputs[k] = v
	}
	bucket, err := strconv.ParseInt(claim.Inputs["window"], 10, 64)
	if err != nil {
		return effects.Result{}, false
	}
	prior.Inputs["window"] = strconv.FormatInt(bucket-1, 10)
	res, ok := effects.LookupApplied(ctx, c.mutationBoundary(), prior)
	if !ok || strings.TrimSpace(res.Provenance) == "" {
		return effects.Result{}, false
	}
	return res, true
}

// issueURLPattern matches the "…/{owner}/{repo}/issues/{number}" tail of a
// GitHub issue URL, host-agnostic like prURLPattern.
var issueURLPattern = regexp.MustCompile(`[^/\s]+/[^/\s]+/issues/(\d+)`)

// parseIssueNumberFromURL extracts the issue number from a GitHub issue URL,
// the provenance the issue-create effect journals.
func parseIssueNumberFromURL(issueURL string) (int, error) {
	m := issueURLPattern.FindStringSubmatch(strings.TrimSpace(issueURL))
	if m == nil {
		return 0, fmt.Errorf("not a recognizable GitHub issue URL: %q", issueURL)
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("invalid issue number in URL %q", issueURL)
	}
	return n, nil
}

// openIssueMatches is what one scan of the recent open issues found.
type openIssueMatches struct {
	// subject is an exact-title match, else the oldest canonical-subject
	// match (see canonicalIssueSubject).
	subject *gh.Issue
	// fileSet is the oldest open issue (any author) whose file-reference set
	// equals the requested one. Only looked for when wantFiles is non-empty.
	fileSet *gh.Issue

	// component is the oldest App-bot-filed open issue citing componentPath,
	// a path the request also cites and that at least
	// sameComponentFoldThreshold such open issues cite (#11239).
	// componentCount is how many do.
	component      *gh.Issue
	componentPath  string
	componentCount int

	// componentCounts is, per non-ubiquitous path the request cites, how many
	// open App-bot-filed issues cite it; componentOldest is the oldest of
	// them. Kept so recently-closed siblings can top the counts up (#11239).
	componentCounts map[string]int
	componentOldest map[string]*gh.Issue
}

// sameComponentFoldThreshold is how many open App-bot-filed issues must
// already cite one path before a new finding citing it is folded into the
// oldest of them instead of filed (#11239). Below it, overlapping findings
// file normally: two issues touching one file is ordinary, a third is a
// stream.
const sameComponentFoldThreshold = 3

// highConfidenceFoldThreshold is the same threshold counted including the
// incoming finding (so one open sibling) for a high-confidence pair: the
// primary cited path is shared and the canonical subject tokens overlap
// heavily. Path-only overlap keeps sameComponentFoldThreshold.
const highConfidenceFoldThreshold = 2

// subjectOverlapMin is the Jaccard similarity of canonical subject tokens at
// which two titles count as the same finding.
const subjectOverlapMin = 0.6

// sameComponentClosedSiblingWindow is how far back an App-bot issue closed as
// completed still counts toward sameComponentFoldThreshold (#11239). Without
// it the count resets every time a variant's PR merges, and a one-bypass,
// one-issue, one-PR cadence never reaches the threshold.
const sameComponentClosedSiblingWindow = 14 * 24 * time.Hour

// sameComponentClosedMaxPages bounds the recently-closed sibling scan.
const sameComponentClosedMaxPages = 2

// ubiquitousFileRefs are paths so many unrelated findings mention in passing
// that sharing one says nothing about sharing a root cause.
//
// The list is static and ecosystem-level by design (#11579, B.5): it names
// files every repo has, not a given repo's own hot spots (a monolithic
// Containerfile, a renovate.json, a top-level config.yml). A repo-specific
// hot file therefore counts toward the same-component fold, so unrelated
// findings that each cite it alongside their real subject can be folded
// together once sameComponentFoldThreshold of them are open at once. The
// threshold is the mitigation: one or two shared citations never fold. A
// per-repo exclusion list is a config surface, deliberately out of scope
// here.
var ubiquitousFileRefs = map[string]bool{
	"README.md": true, "CHANGELOG.md": true, "CONTRIBUTING.md": true,
	"go.mod": true, "go.sum": true, "package.json": true, "package-lock.json": true,
	"pnpm-lock.yaml": true, "yarn.lock": true, "Cargo.toml": true, "Cargo.lock": true,
	"pyproject.toml": true, "requirements.txt": true,
}

func isUbiquitousFileRef(p string) bool {
	return ubiquitousFileRefs[p[strings.LastIndex(p, "/")+1:]]
}

// scanOpenIssues scans up to the 3 most recent pages of open issues for an
// exact (whitespace-trimmed) title match, falling back to a canonical subject
// match so a finding re-filed with a different model-authored qualifier is
// recognised as the same finding. Exact matches win; otherwise the OLDEST
// canonical match is returned so repeats consolidate onto the original issue
// rather than the newest copy. When wantFiles is non-empty the same pass also
// records the oldest open issue (bot- or human-filed) whose
// file-reference set equals wantFiles (#9376). Bounded: agent-filed issues
// are recent by construction, and an unbounded scan of a busy repo would burn
// API budget on every create.
func (c *Client) scanOpenIssues(ctx context.Context, owner, repo, title, body string, wantFiles map[string]bool) (openIssueMatches, error) {
	wantCanonical := canonicalIssueSubject(title)
	botLogins := c.hiveBotLogins()
	wantPrimary, wantTokens := "", map[string]bool(nil)
	if len(wantFiles) > 0 {
		wantPrimary = primaryFileRef(title+"\n"+body, wantFiles)
		wantTokens = issueSubjectTokens(title)
	}
	var pairOldest *gh.Issue
	var found openIssueMatches
	componentCounts := make(map[string]int)
	componentOldest := make(map[string]*gh.Issue)
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; page <= scanOpenIssuesMaxPages; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return openIssueMatches{}, err
		}
		for _, is := range issues {
			if is.IsPullRequest() {
				continue
			}
			candidate := strings.TrimSpace(is.GetTitle())
			if candidate == title {
				return openIssueMatches{subject: is}, nil
			}
			// Pages arrive newest-first, so overwriting leaves the oldest
			// match in hand once the scan finishes.
			if wantCanonical != "" && canonicalIssueSubject(candidate) == wantCanonical {
				found.subject = is
			}
			if len(wantFiles) > 0 {
				have := issueFileRefSet(candidate + "\n" + is.GetBody())
				// Human-filed issues are fold targets for an exact file-set
				// match (#11576), but never count toward the high-confidence
				// pair or the component stream threshold, which measure
				// bot-filed variants.
				if equalFileRefSets(wantFiles, have) {
					found.fileSet = is
				}
				if !isHiveBotLogin(botLogins, is.GetUser().GetLogin()) {
					continue
				}
				if wantPrimary != "" && have[wantPrimary] && primaryFileRef(candidate+"\n"+is.GetBody(), have) == wantPrimary &&
					subjectTokensOverlap(wantTokens, issueSubjectTokens(candidate)) {
					pairOldest = is
				}
				for p := range have {
					if wantFiles[p] && !isUbiquitousFileRef(p) {
						componentCounts[p]++
						componentOldest[p] = is
					}
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	found.component, found.componentPath, found.componentCount, _ = pickFoldComponent(componentCounts, nil, componentOldest)
	if found.component == nil && pairOldest != nil {
		// High-confidence pair: same primary path and near-identical subject.
		found.component, found.componentPath, found.componentCount = pairOldest, wantPrimary, highConfidenceFoldThreshold-1
	}
	found.componentCounts, found.componentOldest = componentCounts, componentOldest
	return found, nil
}

// pickFoldComponent picks the shared path to fold into: among paths cited by
// at least one open sibling whose open plus recently-closed count reaches
// sameComponentFoldThreshold, the most-cited wins; ties go to the lexically
// smallest so the choice is deterministic. The target is always the oldest
// open sibling on that path.
func pickFoldComponent(open, closed map[string]int, oldest map[string]*gh.Issue) (target *gh.Issue, path string, openCount, closedCount int) {
	best := 0
	for p, n := range open {
		if n == 0 || oldest[p] == nil {
			continue
		}
		total := n + closed[p]
		if total < sameComponentFoldThreshold {
			continue
		}
		if total > best || (total == best && p < path) {
			target, path, openCount, closedCount, best = oldest[p], p, n, closed[p], total
		}
	}
	return target, path, openCount, closedCount
}

// recentlyCompletedComponentCounts counts, per non-ubiquitous path in
// wantFiles, the App-bot-filed issues closed as completed within
// sameComponentClosedSiblingWindow that cite it (#11239). Bounded to
// sameComponentClosedMaxPages pages of 100, filtered server-side by creator
// and since. not_planned/duplicate closures are skipped: a maintainer
// rejection is the rejected-twin gate's evidence, not a sign of a stream.
func (c *Client) recentlyCompletedComponentCounts(ctx context.Context, owner, repo string, wantFiles map[string]bool) (map[string]int, error) {
	botLogins := c.hiveBotLogins()
	counts := make(map[string]int)
	if len(botLogins) == 0 || len(wantFiles) == 0 {
		return counts, nil
	}
	cutoff := time.Now().Add(-sameComponentClosedSiblingWindow)
	for _, botLogin := range botLogins {
		if err := c.countClosedSiblingsBy(ctx, owner, repo, botLogin, cutoff, wantFiles, counts); err != nil {
			return nil, err
		}
	}
	return counts, nil
}

func (c *Client) countClosedSiblingsBy(ctx context.Context, owner, repo, botLogin string, cutoff time.Time, wantFiles map[string]bool, counts map[string]int) error {
	opts := &gh.IssueListByRepoOptions{
		State:       "closed",
		Creator:     botLogin,
		Sort:        "updated",
		Direction:   "desc",
		Since:       cutoff,
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; page <= sameComponentClosedMaxPages; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return err
		}
		for _, is := range issues {
			if is.IsPullRequest() || is.GetUser().GetLogin() != botLogin {
				continue
			}
			if is.GetStateReason() != "completed" || is.GetClosedAt().Time.Before(cutoff) {
				continue
			}
			for p := range issueFileRefSet(is.GetTitle() + "\n" + is.GetBody()) {
				if wantFiles[p] && !isUbiquitousFileRef(p) {
					counts[p]++
				}
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return nil
}

// hiveFindingFoldedMarker tags every comment that folds a finding into an
// existing issue, so a human-filed twin is commented on at most once.
const hiveFindingFoldedMarker = "<!-- hive-finding-folded -->"

// commentOnHumanFiledTwin folds a finding into a human-filed issue at most
// once. Scanner re-files are reworded across sweeps, so the body-digest claim
// cannot dedupe them; a marker already on the issue means the finding was
// recorded and this re-file reuses the issue silently. A failed lookup
// comments anyway — suppression only on positive evidence.
func (c *Client) commentOnHumanFiledTwin(ctx context.Context, owner, repo string, twin *gh.Issue, comment string) error {
	found, err := c.IssueCommentsContain(ctx, owner+"/"+repo, twin.GetNumber(), hiveFindingFoldedMarker)
	if err != nil {
		c.logger.Warn("CreateIssue: folded-comment lookup on human-filed issue failed, commenting anyway",
			slog.String("repo", repo), slog.Int("number", twin.GetNumber()), slog.String("error", err.Error()))
	} else if found {
		c.logger.Info("CreateIssue: human-filed issue already carries a folded finding, reusing without another comment",
			slog.String("repo", repo), slog.Int("number", twin.GetNumber()))
		return nil
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+repo, twin.GetNumber(), comment); err != nil {
		return fmt.Errorf("CreateIssue: commenting on human-filed issue #%d in %s/%s: %w", twin.GetNumber(), owner, repo, err)
	}
	return nil
}

// subjectMatchFindingComment renders a finding folded into a human-filed issue
// with the same canonical subject (#11576). File sets were never compared on
// this path, so the header claims only a subject match.
func subjectMatchFindingComment(title, body string) string {
	var b strings.Builder
	b.WriteString(hiveFindingFoldedMarker + "\n")
	b.WriteString("**Consolidated finding.** This was filed as a separate issue; it has the same subject as this one, so it was folded in here instead. Address it together with this issue.\n\n### ")
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(body)
	return b.String()
}

// consolidatedFindingComment renders a finding folded into an open issue that
// covers the same files (#9376). The header says why it is a comment rather
// than its own issue, so whoever picks the issue up handles both in one PR.
func consolidatedFindingComment(title, body string, files map[string]bool) string {
	paths := make([]string, 0, len(files))
	for f := range files {
		paths = append(paths, "`"+f+"`")
	}
	sort.Strings(paths)
	var b strings.Builder
	b.WriteString(hiveFindingFoldedMarker + "\n")
	b.WriteString("**Consolidated finding.** This was filed as a separate issue; it references exactly the same files as this one (")
	b.WriteString(strings.Join(paths, ", "))
	b.WriteString("), so it was folded in here instead. Address it in the same PR as this issue — two PRs editing the same files would conflict.\n\n### ")
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(body)
	return b.String()
}

// foldedComponentFindingComment renders a finding folded into the oldest open
// issue on a component that already has a stream of open findings (#11239).
// The marker lets tooling count folded variants; the header tells whoever
// works the tracker to fix the shared root cause rather than one more variant.
func foldedComponentFindingComment(title, body, path string, openSiblings, closedSiblings int) string {
	var b strings.Builder
	b.WriteString(hiveFindingFoldedMarker + "\n")
	b.WriteString("**Folded finding.** This was filed as a separate issue, but ")
	b.WriteString(strconv.Itoa(openSiblings))
	b.WriteString(" open agent-filed issue")
	if openSiblings != 1 {
		b.WriteString("s")
	}
	if closedSiblings > 0 {
		b.WriteString(" (plus ")
		b.WriteString(strconv.Itoa(closedSiblings))
		b.WriteString(" closed as completed in the last ")
		b.WriteString(strconv.Itoa(int(sameComponentClosedSiblingWindow / (24 * time.Hour))))
		b.WriteString(" days)")
	}
	b.WriteString(" already cite `")
	b.WriteString(path)
	b.WriteString("`, so it was folded into the oldest of them instead of opening another. Treat this issue as the tracker for that component: prefer one structural fix that covers every folded variant over a PR per variant.\n\n### ")
	b.WriteString(title)
	b.WriteString("\n\n")
	b.WriteString(body)
	return b.String()
}

// foldedCommentMaxChars is how large one fold-trail comment may grow before a
// new one is started; GitHub rejects comment bodies over 65536 characters.
const foldedCommentMaxChars = 60000

// appendFoldedFinding records a same-component fold on the tracker issue
// without notifying its subscribers again (#12733): the newest existing
// <!-- hive-finding-folded --> comment that still has room is EDITED to carry
// the new finding as another section. GitHub does not notify on an edit, so
// a tracker that collects dozens of variants costs one notification, not one
// per variant. Only when no fold comment exists (or every one is full) is a
// new comment posted, through the per-issue budget. A failed comment list
// falls back to posting; a failed edit or post is returned so the request
// retries rather than dropping the finding.
func (c *Client) appendFoldedFinding(ctx context.Context, owner, repo string, number int, comment string) error {
	section := strings.TrimPrefix(comment, hiveFindingFoldedMarker+"\n")
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		c.logger.Warn("CreateIssue: fold-trail lookup failed, posting a new fold comment",
			slog.String("repo", repo), slog.Int("number", number), slog.String("error", err.Error()))
		return c.CreateIssueCommentBudgeted(ctx, owner+"/"+repo, number, comment)
	}
	for i := len(comments) - 1; i >= 0; i-- {
		cm := comments[i]
		if cm == nil || !strings.Contains(cm.GetBody(), hiveFindingFoldedMarker) {
			continue
		}
		if len(cm.GetBody())+len(section) > foldedCommentMaxChars {
			break
		}
		merged := cm.GetBody() + "\n\n---\n\n" + section
		if _, _, err := c.client.Issues.EditComment(ctx, owner, repo, cm.GetID(), &gh.IssueComment{Body: gh.Ptr(merged)}); err != nil {
			return fmt.Errorf("appending to fold comment %d: %w", cm.GetID(), err)
		}
		c.logger.Info("CreateIssue: appended folded finding to the existing fold comment",
			slog.String("repo", repo), slog.Int("number", number), slog.Int64("comment_id", cm.GetID()))
		return nil
	}
	err = c.CreateIssueCommentBudgeted(ctx, owner+"/"+repo, number, comment)
	if errors.Is(err, ErrIssueCommentBudget) {
		// The finding is folded either way; the forge trail resumes tomorrow.
		c.logger.Warn("CreateIssue: fold comment suppressed by the per-issue budget, finding folded silently",
			slog.String("repo", repo), slog.Int("number", number))
		return nil
	}
	return err
}

// verificationMarkerRE matches the agent-authored verification markers that
// follow the `<!-- hive-* -->` convention: `<!-- hive-466-verified -->`,
// `<!-- hive-verified -->`, `<!-- hive-verification: head=abc123 -->`. The
// whole marker, including any head SHA it carries, is the idempotence key, so
// a verification against a new head still posts.
var verificationMarkerRE = regexp.MustCompile(`<!--\s*hive-(?:[A-Za-z0-9_]+-)*verif(?:ied|ication)\b[^>]*-->`)

// verificationMarker returns the first verification marker in body, or "".
func verificationMarker(body string) string {
	return verificationMarkerRE.FindString(body)
}

// verificationClaimCooldown is how long after a hive verification comment a
// `claim` on the same issue is refused when nothing else happened since.
const verificationClaimCooldown = 24 * time.Hour

// issueLatestCommentIsRecentVerification reports whether the newest comment
// on the issue is a hive-bot verification posted within
// verificationClaimCooldown of now. Any later comment — a human reply, a
// fold, a PR link — is a state change and lets the claim through.
func (c *Client) issueLatestCommentIsRecentVerification(ctx context.Context, repo string, number int, now time.Time) (bool, error) {
	owner, repoName := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, repoName, number)
	if err != nil {
		return false, err
	}
	if len(comments) == 0 {
		return false, nil
	}
	last := comments[len(comments)-1]
	if last == nil || verificationMarker(last.GetBody()) == "" {
		return false, nil
	}
	if login := last.GetUser().GetLogin(); login != "" && !isHiveBotLogin(c.hiveBotLogins(), login) {
		return false, nil
	}
	if at := last.GetCreatedAt().Time; !at.IsZero() && now.Sub(at) > verificationClaimCooldown {
		return false, nil
	}
	return true, nil
}

func (c *Client) ensureCreateIssueLabel(ctx context.Context, owner, repo, name string) error {
	if IsOriginLabel(name) {
		return c.ensureOriginLabel(ctx, owner, repo, name)
	}
	if _, _, err := c.client.Issues.GetLabel(ctx, owner, repo, name); err == nil {
		return nil
	} else if ghErr, ok := err.(*gh.ErrorResponse); !ok || ghErr.Response == nil || ghErr.Response.StatusCode != http.StatusNotFound {
		return err
	}
	color := "8250df"
	description := "Created by Hive for agent-filed issue provenance"
	if def, ok := escalationIssueLabelDefinition(name); ok {
		color = def.color
		description = def.description
	}
	_, _, err := c.client.Issues.CreateLabel(ctx, owner, repo, &gh.Label{
		Name:        gh.Ptr(name),
		Color:       gh.Ptr(color),
		Description: gh.Ptr(description),
	})
	if ghErr, ok := err.(*gh.ErrorResponse); ok && ghErr.Response != nil && ghErr.Response.StatusCode == http.StatusUnprocessableEntity {
		if _, _, getErr := c.client.Issues.GetLabel(ctx, owner, repo, name); getErr == nil {
			return nil
		} else if isRetryableGitHubError(getErr) {
			return getErr
		}
	}
	return err
}

var subjectTokenSplitRE = regexp.MustCompile(`[^a-z0-9]+`)
var subjectTagRE = regexp.MustCompile(`^\s*\[[^\]]*\]\s*`)
var subjectParenRE = regexp.MustCompile(`\([^)]*\)`)

// issueSubjectTokens returns the distinctive lowercase words of a title's
// canonical subject: role prefix, parentheticals and numbers dropped.
func issueSubjectTokens(title string) map[string]bool {
	t := subjectTagRE.ReplaceAllString(title, "")
	t = subjectParenRE.ReplaceAllString(t, " ")
	if canon := canonicalIssueSubject(t); canon != "" {
		t = canon
	}
	out := make(map[string]bool)
	for _, w := range subjectTokenSplitRE.Split(strings.ToLower(t), -1) {
		if len(w) < 3 || strings.Trim(w, "0123456789") == "" {
			continue
		}
		out[w] = true
	}
	return out
}

// subjectTokensOverlap reports whether two token sets overlap heavily enough
// (Jaccard >= subjectOverlapMin, at least 2 shared tokens) to be one finding.
func subjectTokensOverlap(a, b map[string]bool) bool {
	shared := 0
	for w := range a {
		if b[w] {
			shared++
		}
	}
	union := len(a) + len(b) - shared
	return shared >= 2 && union > 0 && float64(shared)/float64(union) >= subjectOverlapMin
}

// primaryFileRef is the first non-ubiquitous path, in text order, that is in
// set: the file a finding is mainly about, as opposed to incidental mentions.
func primaryFileRef(text string, set map[string]bool) string {
	for _, tok := range fileRefTokenPattern.FindAllString(urlPattern.ReplaceAllString(text, " "), -1) {
		tok = lineSuffixPattern.ReplaceAllString(tok, "")
		if set[tok] && !isUbiquitousFileRef(tok) {
			return tok
		}
	}
	return ""
}
