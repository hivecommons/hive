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
	issueRetryBase = 30 * time.Second
	issueRetryMax  = 15 * time.Minute
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
// bots are not valid GitHub assignees (Issues.AddAssignees silently drops them).
// The claim is filed by the agent when it starts work on an issue, giving a
// visible, auditable ownership signal the hive can observe.
type IssueRequest struct {
	Kind   string   `json:"kind,omitempty"` // "issue" (default) | "comment" | "claim"
	Repo   string   `json:"repo"`
	Title  string   `json:"title,omitempty"` // issue only
	Body   string   `json:"body,omitempty"`
	Labels []string `json:"labels,omitempty"` // issue only
	Number int      `json:"number,omitempty"` // comment/claim: issue/PR number
	Agent  string   `json:"agent,omitempty"`
}

// claimLabelPrefix is the label namespace applied for a "claim" request. The
// full label is claimLabelPrefix + <sanitized agent name>, e.g.
// "hive/claimed-by-scanner". Namespaced so it is easy to filter/sweep and never
// collides with a project's own labels.
const claimLabelPrefix = "hive/claimed-by-"

// IssueResponse is written next to a consumed request as <name>.result.json.
type IssueResponse struct {
	OK             bool   `json:"ok"`
	Number         int    `json:"number,omitempty"`
	URL            string `json:"url,omitempty"`
	AlreadyExisted bool   `json:"already_existed,omitempty"`
	Error          string `json:"error,omitempty"`
	At             string `json:"at"`
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
func (c *Client) issueNoteFailure(path string, nowFn func() time.Time) bool {
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
	data, err := os.ReadFile(path)
	if err != nil {
		return // vanished between ReadDir and here
	}
	var req IssueRequest
	if err := json.Unmarshal(data, &req); err != nil {
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
		}
	case "comment":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Body) == "" {
			shapeErr = "comment request requires repo, number, and body"
		}
	case "claim":
		if strings.TrimSpace(req.Repo) == "" || req.Number <= 0 || strings.TrimSpace(req.Agent) == "" {
			shapeErr = "claim request requires repo, number, and agent"
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

	meta := c.attributionMeta(req.Agent)
	body := req.Body
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

	resp := IssueResponse{At: nowFn().UTC().Format(time.RFC3339)}
	switch kind {
	case "claim":
		// Apply a namespaced ownership label (App bots can't be assignees).
		label := claimLabelPrefix + sanitizeAgentName(req.Agent)
		err = c.AddLabels(ctx, req.Repo, req.Number, []string{label})
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
		}
	case "comment":
		err = c.CreateIssueComment(ctx, req.Repo, req.Number, body)
		if err == nil {
			resp.OK = true
			resp.Number = req.Number
		}
	default: // "issue"
		var res CreateIssueResult
		res, err = c.CreateIssue(ctx, req.Repo, req.Title, body, req.Labels)
		if err == nil {
			resp.OK = true
			resp.Number = res.Number
			resp.URL = res.URL
			resp.AlreadyExisted = res.AlreadyExisted
		}
	}

	if err != nil {
		resp.OK = false
		resp.Error = err.Error()
		c.writeIssueResult(path, resp)
		gaveUp := c.issueNoteFailure(path, nowFn)
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

	action := AuditActionAgentIssueCreated
	switch kind {
	case "comment":
		action = AuditActionAgentCommentCreated
	case "claim":
		action = AuditActionIssueClaimed
	}
	c.recordCreationAudit(action, meta,
		"repo", req.Repo,
		"number", strconv.Itoa(resp.Number),
		"url", resp.URL,
		"reused", strconv.FormatBool(resp.AlreadyExisted))
	c.writeIssueResult(path, resp)
	_ = os.Remove(path)
	c.issueClearRetry(path)
	c.logger.Info("issue-request watcher: request completed",
		slog.String("repo", req.Repo), slog.String("kind", kind),
		slog.Int("number", resp.Number), slog.Bool("reused", resp.AlreadyExisted),
		slog.String("agent", req.Agent))
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
		_ = os.WriteFile(out, b, 0o644)
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
	if err := os.WriteFile(path, b, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// CreateIssueResult is what CreateIssue returns.
type CreateIssueResult struct {
	Number int
	URL    string
	// AlreadyExisted is true when an OPEN issue with the same exact title was
	// already present in the repo, so we returned it instead of creating a
	// duplicate. This makes the watcher's retry loop idempotent: a create that
	// succeeded server-side but crashed before the request file was consumed
	// (or an agent-side "timed out but actually created" ambiguity) never
	// yields a second issue.
	AlreadyExisted bool
}

// CreateIssue creates an issue as the hive's App bot, ensuring requested labels
// exist first and deduping by exact open-issue title. repo may be "owner/repo"
// or bare (owner defaults to the hive org).
func (c *Client) CreateIssue(ctx context.Context, repo, title, body string, labels []string) (CreateIssueResult, error) {
	if c == nil || c.client == nil {
		return CreateIssueResult{}, ErrNoGitHubClient
	}
	owner, repoName := c.splitRepo(repo)
	title = strings.TrimSpace(title)
	if title == "" {
		return CreateIssueResult{}, fmt.Errorf("CreateIssue: title is required")
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
	// index can lag minutes on GHE — useless against a 10s retry loop).
	if existing, err := c.findOpenIssueByTitle(ctx, owner, repoName, title); err != nil {
		c.logger.Warn("CreateIssue: dedupe lookup failed, proceeding to create",
			slog.String("repo", repoName), slog.String("error", err.Error()))
	} else if existing != nil {
		c.logger.Info("CreateIssue: open issue with identical title exists, reusing",
			slog.String("repo", repoName), slog.Int("number", existing.GetNumber()))
		return CreateIssueResult{Number: existing.GetNumber(), URL: existing.GetHTMLURL(), AlreadyExisted: true}, nil
	}

	// Ensure labels exist; a label that cannot be ensured is dropped (labels
	// are provenance metadata, not a gate — mirror the gh-wrapper posture).
	var usable []string
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if err := c.ensureLabel(ctx, owner, repoName, l); err != nil {
			c.logger.Warn("CreateIssue: could not ensure label, creating without it",
				slog.String("repo", repoName), slog.String("label", l), slog.String("error", err.Error()))
			continue
		}
		usable = append(usable, l)
	}

	req := &gh.IssueRequest{Title: gh.Ptr(title), Body: gh.Ptr(body)}
	if len(usable) > 0 {
		req.Labels = &usable
	}
	issue, _, err := c.client.Issues.Create(ctx, owner, repoName, req)
	if err != nil {
		return CreateIssueResult{}, fmt.Errorf("creating issue in %s/%s: %w", owner, repoName, err)
	}
	c.logger.Info("CreateIssue: issue created as the App bot",
		slog.String("repo", repoName), slog.Int("number", issue.GetNumber()))
	return CreateIssueResult{Number: issue.GetNumber(), URL: issue.GetHTMLURL()}, nil
}

// findOpenIssueByTitle scans up to the 3 most recent pages of open issues for
// an exact (whitespace-trimmed) title match. Bounded: agent-filed issues are
// recent by construction, and an unbounded scan of a busy repo would burn API
// budget on every create.
func (c *Client) findOpenIssueByTitle(ctx context.Context, owner, repo, title string) (*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Sort:        "created",
		Direction:   "desc",
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 1; page <= 3; page++ {
		opts.ListOptions.Page = page
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return nil, err
		}
		for _, is := range issues {
			if is.IsPullRequest() {
				continue
			}
			if strings.TrimSpace(is.GetTitle()) == title {
				return is, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
	}
	return nil, nil
}
