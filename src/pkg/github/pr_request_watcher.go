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
var prRequestPollInterval = 10 * time.Second

// PRRequest is the JSON an agent writes to PRRequestDir to ask the hive to open
// a PR on its behalf. Repo may be "owner/repo" or a bare repo name. Base is
// optional and normally omitted: an empty Base means "resolve the target
// repository's default branch" (CreatePR does this from the repo's own
// metadata, and FAILS THE REQUEST rather than guessing "main" if it can't) —
// set it only to target a non-default branch on purpose
// (kubestellar/hive#4928).
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
	// DuplicateTree says the returned PR was reused because it already carries
	// this request's exact content, not because it shares the head branch
	// (#5111). It is the difference between "your retry landed on the PR you
	// already opened" and "this change is already proposed as #N" — the second
	// is the one an agent must act on, by commenting on that PR rather than
	// filing again, so it is reported rather than folded into AlreadyExisted.
	DuplicateTree bool `json:"duplicate_tree,omitempty"`
	// SelfAuthorized reports that the PR was held because its only tracked
	// rationale is an issue the hive filed and no human has acknowledged
	// (#5117). The PR was still opened; it just cannot merge until someone
	// signs off on the direction. Reported to the agent so it can go get that
	// sign-off rather than reading the hold as an unexplained failure.
	//
	// Never set together with DuplicateTree: see the precedence note in
	// handleOnePRRequest. A duplicate request opened no PR, so there is nothing
	// of this request's to have authorised.
	SelfAuthorized bool   `json:"self_authorized,omitempty"`
	Error          string `json:"error,omitempty"`
	At             string `json:"at"`
}

// PRRequestAuthorizer decides whether a PR-open request may proceed. It receives
// the agent NAME claimed in the request and the UID that OWNS the request file
// (from the file's stat), and returns nil to authorize or an error explaining
// the denial. This is where the two policy checks live, implemented by the
// caller (which has the manager + uid-map):
//
//  1. Forge-resistance: the fileUID must map to the claimed agent (an agent can
//     only speak for ITSELF — one agent, or any non-agent process, cannot drop a
//     request "as" another agent). The per-agent PR-request subdir is UID-owned,
//     so this is the same UID trust anchor the git credential helper uses.
//  2. ACMM write-gate: the agent must be push-capable at the hive's current ACMM
//     level (the same CanPush()/mode check that governs `gh pr create` today) —
//     an advisory-only agent must NOT be able to open a PR via this path.
//
// A nil authorizer DENIES everything (fail closed) — the watcher never opens a
// PR without an authorizer, so a wiring mistake cannot silently bypass policy.
type PRRequestAuthorizer func(agent string, fileUID int) error

// StartPRRequestWatcher runs a loop that opens PRs for request files dropped in
// PRRequestDir. It returns immediately; the loop runs until ctx is cancelled.
// A nil client (no GitHub creds) makes this a no-op: requests accumulate rather
// than being silently dropped, so the feature degrades to "nothing opens" not
// "opened as the wrong identity".
//
// authz enforces the per-agent ACMM write-gate + forge-resistance (see
// PRRequestAuthorizer). A nil authz fails closed (denies every request) — the
// watcher must never open a PR that hasn't been authorized against the same
// policy as the direct `gh pr create` path.
//
// holdLabel (F6) decides server-side, from the authoring agent and authoritative
// hive config (the ACMM level), whether a freshly-opened PR must carry the
// "hold" label. It is applied AFTER the PR is created, on the path that
// actually runs — unlike the gh-wrapper.sh tail, which was dead code (it sat
// after `exec hive-open-pr`), so hold-gated PRs were being opened unlabeled. A
// nil holdLabel means "never hold".
//
// nowFn is injectable for tests; pass nil for time.Now.
//
// The returned channel closes when the watcher goroutine has exited (or
// immediately when it never starts), so callers — tests above all — can JOIN
// the loop after cancelling instead of sleeping and hoping: an unjoined
// watcher outliving its test races the test's global-seam restores.
// PROpenedHook is notified when the watcher opens a NEW PR for an agent.
type PROpenedHook func(agent, repo string, number int, url string)

// SetPROpenedHook installs (or with nil, removes) the PR-opened hook. Safe
// to call before or after the watcher starts.
func (c *Client) SetPROpenedHook(fn PROpenedHook) {
	if c == nil {
		return
	}
	if fn == nil {
		c.prOpenedHook.Store(nil)
		return
	}
	c.prOpenedHook.Store(&fn)
}

func (c *Client) StartPRRequestWatcher(ctx context.Context, authz PRRequestAuthorizer, holdLabel func(agent string) bool, nowFn func() time.Time) <-chan struct{} {
	done := make(chan struct{})
	if c == nil {
		close(done)
		return done
	}
	c.prAuthz = authz
	c.prHoldLabel = holdLabel
	if nowFn == nil {
		nowFn = time.Now
	}
	// Shared with PrepareRequestDirs, which creates this queue unconditionally
	// at boot so requests can accumulate even before a watcher runs.
	dir := prRequestDir()
	if !ensureRequestDir(c.logger, "pr", dir) {
		close(done)
		return done
	}
	// Capture the poll interval BEFORE spawning: the goroutine's first read of
	// the package-level interval races with a test's fastTick cleanup restoring
	// it (the race detector flagged exactly that on v4 CI). Reading it here is
	// sequenced with the caller, and the loop only ever needed it once anyway.
	interval := prRequestPollInterval
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
				c.processPRRequestsInDir(ctx, nowFn, dir)
			}
		}
	}()
	c.logger.Info("pr-request watcher started", slog.String("dir", dir))
	return done
}

// processPRRequests handles one scan of the request dir. Exported-in-spirit for
// tests via ProcessPRRequestsOnce.
func (c *Client) processPRRequests(ctx context.Context, nowFn func() time.Time) {
	c.processPRRequestsInDir(ctx, nowFn, prRequestDir())
}

func (c *Client) processPRRequestsInDir(ctx context.Context, nowFn func() time.Time, dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		// Only consume request files; skip our own .result.json outputs.
		if e.IsDir() || !strings.HasSuffix(name, ".json") || strings.HasSuffix(name, ".result.json") {
			continue
		}
		path := filepath.Join(dir, name)
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
	if !c.prRetries.allows(path, nowFn()) {
		return
	}
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
		c.prRetries.clear(path)
		c.logger.Warn("pr-request watcher: bad request file quarantined",
			slog.String("path", path), slog.String("error", err.Error()))
		return
	}

	// AUTHORIZE before opening — the watcher must enforce the SAME policy as the
	// direct `gh pr create` path: the request's agent must own the file (an agent
	// can only speak for itself) AND be push-capable at the hive's ACMM level.
	// The owning UID comes from the file's stat, which the requester cannot forge
	// without actually running as that UID. A nil authorizer fails closed.
	fileUID := statUID(data, path)
	if c.prAuthz == nil {
		c.denyPRRequest(path, req, "no authorizer configured (fail closed)", nowFn)
		return
	}
	if err := c.prAuthz(req.Agent, fileUID); err != nil {
		c.denyPRRequest(path, req, err.Error(), nowFn)
		return
	}

	// Validate claims while the request is still at the server-side choke point.
	// Agents cannot bypass this by invoking a different CLI: direct POST /pulls
	// is denied by the proxy, and every supported path arrives here.
	title, body, err := c.validatePRRequestClaims(ctx, req)
	if err != nil {
		if reason, policy := prRequestPolicyReason(err); policy {
			c.rejectPRRequest(path, req, "claim", reason, nowFn)
			return
		}
		c.failPRRequest(path, req, err, nowFn)
		return
	}

	// Scan the candidate diff at the same choke point (#5114). The gate above
	// checks what the request SAYS; this one checks what the branch would
	// PUBLISH — agent and run metadata committed into files, which no change to
	// the request can make safe. Both run because a request can be honest about
	// a branch that is still unpublishable.
	if err := c.validatePRRequestContent(ctx, req); err != nil {
		if reason, policy := prContentMetadataReason(err); policy {
			c.rejectPRRequest(path, req, "content", reason, nowFn)
			return
		}
		c.failPRRequest(path, req, err, nowFn)
		return
	}

	// Public outreach prose speaks for the project, so it gets a second gate at
	// the same choke point (#5115). The check above is about the request being
	// accurate; this one is about the project being able to stand behind what
	// the PR would publish — an unsupported capability claim with no evidence
	// record, or regulatory language.
	if reason, err := c.validateOutreachPRRequest(ctx, req); err != nil {
		c.failPRRequest(path, req, err, nowFn)
		return
	} else if reason != "" {
		c.rejectPRRequest(path, req, "outreach", reason, nowFn)
		return
	}

	// Invocation-attribution trail (attribution.go): resolve what the hive
	// invoked for this agent, append the visible trailer to the PR body when
	// the toggle is on, and — below, on success — record the audit entry
	// unconditionally. This choke point covers every agent regardless of CLI,
	// because the proxy hard-denies direct POST /pulls.
	meta := c.attributionMeta(req.Agent)
	if c.attributionTrailerOn() {
		body = AppendTrailer(body, meta)
	}

	res, err := c.CreatePR(ctx, req.Repo, req.Head, req.Base, title, body)
	resp := PRResponse{At: nowFn().UTC().Format(time.RFC3339)}
	if err != nil {
		c.failPRRequest(path, req, err, nowFn)
		return
	}
	resp.OK = true
	resp.Number = res.Number
	resp.URL = res.URL
	resp.AlreadyExisted = res.AlreadyExisted
	resp.DuplicateTree = res.DuplicateTree

	// F6: apply the ACMM "hold" label server-side, from authoritative config, on
	// the path that actually runs. At hold-gated levels (L3/L4/L5) every
	// agent-opened PR must be human-approved before merge; the label is what the
	// merge gate keys on. The old gh-wrapper.sh `args+=("--label" "hold")` was
	// unreachable (it followed `exec hive-open-pr`), so those PRs were opened
	// unlabeled and the gate was inert. AddLabels is additive + idempotent, so it
	// is safe to (re)apply even when we reused an already-open PR.
	//
	// #5117 adds a second, level-independent reason to hold: the PR's only
	// tracked rationale is an issue the hive filed that nobody has acknowledged.
	// It is evaluated only when the level is NOT already holding — at a
	// hold-gated level every PR already waits for the human checkpoint this gate
	// exists to create, so asking GitHub about the issue would buy nothing.
	//
	// PRECEDENCE, #5111 over #5117 (and over the level gate). The other two ask
	// "may this agent's change proceed to merge?", which is a property of the PR
	// THIS REQUEST CREATED. DuplicateTree says this request created nothing: res
	// names a PR that already existed, and it may belong to another agent or to
	// a human. Labelling or commenting there acts on somebody else's PR, and a
	// "hold" stamped on it blocks a merge that has nothing to do with this
	// request. So the duplicate path takes neither hold reason.
	//
	// No governance is skipped by that ordering. The PR being pointed at went
	// through the self-authorisation gate when IT was opened through this same
	// path; and where it is a human's PR, hive has no standing to gate it at
	// all. Evaluating the gate here would additionally spend GitHub calls to
	// compute a hold that must not be applied.
	holdByLevel := c.prHoldLabel != nil && c.prHoldLabel(req.Agent)
	var selfAuth SelfAuthorization
	if !res.DuplicateTree && !holdByLevel {
		selfAuth = c.EvaluateSelfAuthorization(ctx, req.Repo, title, body, req.IssueN)
		resp.SelfAuthorized = selfAuth.Held
	}
	if !res.DuplicateTree && (holdByLevel || selfAuth.Held) {
		if lerr := c.AddLabels(ctx, req.Repo, res.Number, []string{"hold"}); lerr != nil {
			// A missing hold label is a policy failure, not a cosmetic one. Keep
			// the request queued: the next bounded retry deduplicates the existing
			// PR and reapplies the label instead of silently declaring success.
			c.logger.Warn("pr-request watcher: PR opened but failed to apply hold label (hold-gated level)",
				slog.String("repo", req.Repo), slog.Int("number", res.Number), slog.String("error", lerr.Error()))
			c.failPRRequest(path, req, fmt.Errorf("PR #%d opened but required hold label could not be applied: %w", res.Number, lerr), nowFn)
			return
		} else if selfAuth.Held {
			c.logger.Info("pr-request watcher: held PR — its only tracked rationale is an issue the hive filed and nobody acknowledged",
				slog.String("repo", req.Repo), slog.Int("number", res.Number),
				slog.String("rationale_repo", selfAuth.Repo), slog.Int("rationale_issue", selfAuth.Issue),
				slog.String("reason", selfAuth.Reason), slog.String("agent", req.Agent))
		} else {
			c.logger.Info("pr-request watcher: applied hold label (hold-gated ACMM level)",
				slog.String("repo", req.Repo), slog.Int("number", res.Number))
		}
	}

	// Explain the hold on the PR itself, once, when we opened it. A "hold" with
	// no stated cause reads as a malfunction, and the person who has to clear it
	// needs to know what clears it. Best-effort: the label is the enforcement,
	// the comment is the courtesy, and a failed courtesy must not fail the
	// request and send it round the retry loop.
	if selfAuth.Held && !res.AlreadyExisted {
		if cerr := c.CreateIssueComment(ctx, req.Repo, res.Number, selfAuthorizationNotice(selfAuth)); cerr != nil {
			c.logger.Warn("pr-request watcher: held PR but could not post the explanation",
				slog.String("repo", req.Repo), slog.Int("number", res.Number), slog.String("error", cerr.Error()))
		}
	}

	// Audit the creation UNCONDITIONALLY (not gated by the trailer toggle) —
	// this is the durable answer to "which backend/model produced this PR?".
	// A reused PR is recorded too (reused=true): the watcher may be
	// re-processing a request that partially succeeded, and the invocation
	// that produced the branch is the same.
	if hook := c.prOpenedHook.Load(); hook != nil && *hook != nil && !res.AlreadyExisted {
		// Progress surfaces (the Linear session emitter) learn about the PR
		// here, on the path that actually opened it. Own goroutine: an HTTP
		// post to a tracker must never delay consuming the request file.
		go (*hook)(req.Agent, req.Repo, res.Number, res.URL)
	}
	c.recordCreationAudit(AuditActionAgentPRCreated, meta,
		"repo", req.Repo,
		"number", strconv.Itoa(res.Number),
		"author", res.Author,
		"url", res.URL,
		"reused", strconv.FormatBool(res.AlreadyExisted),
		"self_authorized", strconv.FormatBool(selfAuth.Held),
		"duplicate_tree", strconv.FormatBool(res.DuplicateTree))
	c.writePRResult(path, resp)
	// Success (or reuse of an existing PR) — consume the request so it isn't
	// reprocessed.
	_ = os.Remove(path)
	c.prRetries.clear(path)
	if res.DuplicateTree {
		// Logged on its own line, not folded into the line below: this is the
		// outcome the issue asked to be able to count ("a queue that looks like
		// five problems when it is one"), and it should be findable without
		// parsing a boolean out of a success message.
		c.logger.Info("pr-request watcher: request carried content an open PR already proposes; reused it instead of opening a duplicate",
			slog.String("repo", req.Repo), slog.String("head", req.Head),
			slog.Int("number", res.Number), slog.String("agent", req.Agent))
	}
	c.logger.Info("pr-request watcher: PR opened by App bot",
		slog.String("repo", req.Repo), slog.String("head", req.Head),
		slog.Int("number", res.Number), slog.Bool("reused", res.AlreadyExisted),
		slog.Bool("duplicate_tree", res.DuplicateTree),
		slog.String("agent", req.Agent))
}

// failPRRequest records a transient validation or PR-creation failure. The
// request remains queued and is retried with the same bounded backoff used for
// all other forge failures.
func (c *Client) failPRRequest(path string, req PRRequest, err error, nowFn func() time.Time) {
	resp := PRResponse{OK: false, Error: err.Error(), At: nowFn().UTC().Format(time.RFC3339)}
	c.writePRResult(path, resp)

	// #5343: an unpushed head ref is the one failure here whose visible symptom
	// points away from its cause. Log it at ERROR on its own line — this is
	// work an agent already completed and cannot publish, and it is invisible
	// in the fleet view because the agent's session ended healthy.
	if reason, ok := missingHeadReason(err); ok {
		c.logger.Error("pr-request watcher: head branch is not on the remote — the agent's push did not authenticate",
			slog.String("repo", req.Repo), slog.String("head", req.Head),
			slog.String("agent", req.Agent), slog.String("diagnosis", reason))
	}
	if c.prRetries.noteFailure(path, nowFn()) {
		_ = os.Rename(path, path+".failed")
		c.prRetries.clear(path)
		c.logger.Error("pr-request watcher: request exceeded retry horizon, quarantined",
			slog.String("path", path), slog.String("repo", req.Repo),
			slog.String("head", req.Head), slog.String("error", err.Error()))
		return
	}
	c.logger.Warn("pr-request watcher: request failed, will retry with backoff",
		slog.String("repo", req.Repo), slog.String("head", req.Head), slog.String("error", err.Error()))
}

// rejectPRRequest records a permanent policy mismatch and quarantines it. A
// changed request or branch is required to make it valid, so retrying the same
// file would only consume API quota and hide the actionable feedback.
//
// More than one gate shares this path, so gate names which one refused.
// Without it a quarantined request says only that something was rejected, and
// the gates have different remedies: correct the request, or change what the
// branch would publish.
func (c *Client) rejectPRRequest(path string, req PRRequest, gate, reason string, nowFn func() time.Time) {
	c.writePRResult(path, PRResponse{OK: false, Error: "PR " + gate + " rejected: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".rejected")
	c.prRetries.clear(path)
	c.logger.Warn("pr-request watcher: REJECTED (policy)",
		slog.String("gate", gate),
		slog.String("agent", req.Agent), slog.String("repo", req.Repo),
		slog.String("head", req.Head), slog.String("reason", reason))
}

// denyPRRequest records an authorization failure and quarantines the request
// (renamed .denied) so it is not retried forever. A denied request is a policy
// event, not a transient error — retrying can never make an advisory agent
// push-capable or change who owns the file.
func (c *Client) denyPRRequest(path string, req PRRequest, reason string, nowFn func() time.Time) {
	c.writePRResult(path, PRResponse{OK: false, Error: "authorization denied: " + reason, At: nowFn().UTC().Format(time.RFC3339)})
	_ = os.Rename(path, path+".denied")
	c.prRetries.clear(path)
	c.logger.Warn("pr-request watcher: DENIED (policy)",
		slog.String("agent", req.Agent), slog.String("repo", req.Repo),
		slog.String("head", req.Head), slog.String("reason", reason))
}

// statUID returns the UID that owns the request file. On the (Linux) container
// this is a real UID that a forging process cannot fake without running as it.
// data is unused but kept in the signature so a future non-stat proof (e.g. an
// embedded signed token) can slot in without touching call sites.
func statUID(_ []byte, path string) int {
	fi, err := os.Stat(path)
	if err != nil {
		return -1
	}
	return fileOwnerUID(fi)
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
