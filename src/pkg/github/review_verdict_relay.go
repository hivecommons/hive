package github

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/review"
)

// ReviewEventRecordVerdict is the relay event for "I judged this PR and have
// nothing to post about it". It performs no GitHub write.
const ReviewEventRecordVerdict = "record_verdict"

const reviewVerdictAuditLookback = 90 * 24 * time.Hour

var reviewVerdictAuditPath = "/data/audit.jsonl"

// recordReviewVerdict is the missing half of the reviewer's output path.
//
// The reviewer produces TWO artifacts: a PR comment (posted through this same
// relay) and a structured verdict that the routing chain consumes —
// review.Collect -> review-verdicts.json -> Aggregate -> dispatchFix ->
// State.Human -> the human-decision label.
//
// The comment half worked. The verdict half had no transport at all. Nothing in
// the tree ever wrote the review-report-*.json files review.Collect reads, and
// the agents could not have written them anyway: /var/run/hive-metrics is
// 0755 dev:node and every agent runs as its own non-dev uid, so an agent-side
// write is EACCES by construction. Measured on a bluefin spoke: 117
// agent_pr_reviewed audit entries, a populated review-links.json, and
// review-verdicts.json still {"items": []} with 38 orphaned pending_reviews and
// zero human-decision labels ever applied.
//
// The relay is the correct place to close that gap. The watcher already runs
// server-side as the dir's owner, so it can write where the agent cannot, and
// the verdict rides the same authorized, forge-resistant drop the comment does
// instead of needing a second privileged channel.
//
// Failures here are logged and swallowed. The review is already posted by the
// time this runs; losing the verdict must never turn a successful review into a
// retry that posts the comment a second time.
//
// It returns the reports actually recorded, which are what the review evidence
// bundle may claim (review_evidence.go).
func (c *Client) recordReviewVerdict(ctx context.Context, req ReviewRequest, dir string) []review.PerspectiveReport {
	raw := strings.TrimSpace(req.Report)
	if raw == "" {
		return nil
	}
	dir = review.ReportDir(dir)
	// Validate BEFORE the report lands where the collector reads. review.Collect
	// fails the whole collection on the first unparseable file, so one malformed
	// agent verdict would otherwise take down routing for every PR in the hive.
	//
	// A combined review judges every perspective in one session and delivers
	// them as one array, so this accepts either shape: a bare object is still
	// exactly what it was.
	reports, err := review.ValidateReportsFor([]byte(raw), c.perspectives)
	if err != nil {
		c.logger.Warn("review-request watcher: rejected malformed verdict",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("agent", req.Agent), slog.String("error", err.Error()))
		return nil
	}
	decisionRequested, decisionQuestion := reviewDecisionRequestFromBody(req.Body)
	decisionRiskHigh := reviewDecisionRiskHighFromBody(req.Body)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		c.logger.Warn("review-request watcher: could not create report dir",
			slog.String("dir", dir), slog.String("error", err.Error()))
		return nil
	}
	// The diff is fetched only when a human hold is actually in play: an
	// approve with no decision question has nothing for the carve-out to lift.
	tightening := false
	if decisionRequested || reviewReportsHoldForHuman(reports) {
		tightening = c.workflowPermissionsTighteningOnly(ctx, req)
	}
	prHead := c.reviewedPRHeadFunc(ctx, req)
	var recorded []review.PerspectiveReport
	for _, report := range reports {
		if decisionRequested {
			report.DecisionRequested = true
			report.DecisionQuestion = decisionQuestion
			report.DecisionRiskHigh = decisionRiskHigh
			report.Verdict = review.VerdictRequiresHuman
		}
		report.PermissionsTighteningOnly = tightening
		if written, ok := c.writeOneVerdict(ctx, req, report, dir, prHead); ok {
			recorded = append(recorded, written)
		}
	}
	return recorded
}

// canonicalVerdictHead returns the head SHA a verdict is stored under. The
// lanes (contributor-lane verdict gate, merge-eligibility, evidence bundles)
// all look verdicts up by the 40-character head GitHub reports, so a verdict
// is stored under that spelling: a blank head takes the dispatch head; an
// abbreviated one ("98489e7", what reviewers copy from the PR page) is
// widened to the dispatch head it prefixes, or, when nothing dispatched it,
// to the PR's current head if it prefixes that. A short head that matches
// neither is kept as written: it names an older revision and must not be
// re-attributed to the current one.
func (c *Client) canonicalVerdictHead(req ReviewRequest, reported, dispatchHead string, prHead func() string) string {
	head := strings.TrimSpace(reported)
	dispatchHead = strings.TrimSpace(dispatchHead)
	if head == "" {
		return dispatchHead
	}
	if !review.IsAbbreviatedSHA(head) {
		return head
	}
	if widened := review.CanonicalHeadSHA(head, dispatchHead); widened != head {
		return widened
	}
	if prHead == nil {
		return head
	}
	current := prHead()
	if widened := review.CanonicalHeadSHA(head, current); widened != head {
		c.logger.Info("review-request watcher: abbreviated verdict head widened to the PR head",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("reported", head), slog.String("head", widened))
		return widened
	}
	if current != "" {
		c.logger.Warn("review-request watcher: abbreviated verdict head is not the PR's current head; recorded as written",
			slog.String("repo", req.Repo), slog.Int("number", req.Number),
			slog.String("reported", head), slog.String("current", current))
	}
	return head
}

// reviewedPRHeadFunc returns a memoized lookup of req's PR head SHA: one
// GitHub GET per relay pass at most, only when a verdict actually needs it,
// and "" when the client is unavailable or the fetch fails.
func (c *Client) reviewedPRHeadFunc(ctx context.Context, req ReviewRequest) func() string {
	var (
		head    string
		fetched bool
	)
	return func() string {
		if fetched {
			return head
		}
		fetched = true
		if c == nil || c.client == nil {
			return ""
		}
		owner, name := c.splitRepo(strings.TrimSpace(req.Repo))
		if owner == "" || name == "" || req.Number <= 0 {
			return ""
		}
		pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_verdict_relay"), owner, name, req.Number)
		if err != nil || pr == nil {
			c.logger.Warn("review-request watcher: could not fetch PR head to widen abbreviated verdict head",
				slog.String("repo", req.Repo), slog.Int("number", req.Number))
			return ""
		}
		head = strings.TrimSpace(pr.GetHead().GetSHA())
		return head
	}
}

func reviewReportsHoldForHuman(reports []review.PerspectiveReport) bool {
	for _, r := range reports {
		if r.Verdict == review.VerdictRequiresHuman || r.DecisionRequested {
			return true
		}
	}
	return false
}

// reviewVerdictFilesMaxPages bounds the files fetch: a permissions-only
// tightening is a handful of files, so a PR that pages past this is not one
// and the check fails closed without reading the rest.
const reviewVerdictFilesMaxPages = 3

// workflowPermissionsTighteningOnly fetches the reviewed PR's changed files
// and asks review.WorkflowPermissionsTighteningOnly whether the diff is a
// pure workflow permissions reduction (hivecommons/hive#13068). Every
// failure path -- no client, API error, truncated file list -- answers
// false, which leaves the verdict exactly as the reviewer recorded it.
func (c *Client) workflowPermissionsTighteningOnly(ctx context.Context, req ReviewRequest) bool {
	if c == nil || c.client == nil {
		return false
	}
	owner, name := c.splitRepo(strings.TrimSpace(req.Repo))
	if owner == "" || name == "" || req.Number <= 0 {
		return false
	}
	ctx = WithRESTCaller(ctx, "hive:review_verdict_relay")
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, req.Number)
	if err != nil || pr == nil {
		return false
	}
	var files []review.ChangedFile
	opts := &gh.ListOptions{PerPage: listPerPage}
	complete := false
	for page := 0; page < reviewVerdictFilesMaxPages; page++ {
		list, resp, err := c.client.PullRequests.ListFiles(ctx, owner, name, req.Number, opts)
		if err != nil {
			return false
		}
		for _, f := range list {
			if f == nil {
				continue
			}
			files = append(files, review.ChangedFile{Path: f.GetFilename(), Status: f.GetStatus(), Patch: f.GetPatch()})
		}
		if resp == nil || resp.NextPage == 0 {
			complete = true
			break
		}
		opts.Page = resp.NextPage
	}
	// GitHub caps the files endpoint on very large PRs; a short list could
	// hide the widening that disqualifies the carve-out.
	if !complete || pr.GetChangedFiles() > len(files) {
		return false
	}
	ok := review.WorkflowPermissionsTighteningOnly(files)
	if ok {
		c.logger.Info("review-request watcher: workflow diff is a pure permissions tightening",
			slog.String("repo", req.Repo), slog.Int("number", req.Number))
	}
	return ok
}

const (
	reviewNeedsDecisionMarker    = "<!-- hive-verdict: needs-decision -->"
	reviewDecisionQuestionMarker = "<!-- hive-decision-question:"
	// reviewRedundantOfMarker names the PR a review found already carries this
	// change (`<!-- hive-redundant-of: #334 -->`); see reviewer-queue.md.
	reviewRedundantOfMarker = "<!-- hive-redundant-of:"
)

// hiddenMarkerValue returns the payload of the first `<!-- prefix ... -->`
// line in body, or "" when absent. The prefix match is case-insensitive.
func hiddenMarkerValue(body, prefix string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(strings.ToLower(trimmed), prefix) && strings.HasSuffix(trimmed, "-->") {
			return strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(trimmed[len(prefix):]), "-->"))
		}
	}
	return ""
}

// decisionQuestionFromBody reads the `<!-- hive-decision-question: ... -->`
// marker, the single grammar shared by the verdict relay and the comment
// renderer.
func decisionQuestionFromBody(body string) string {
	return hiddenMarkerValue(body, reviewDecisionQuestionMarker)
}

func reviewDecisionRequestFromBody(body string) (bool, string) {
	var requested bool
	for _, line := range strings.Split(body, "\n") {
		if strings.ToLower(strings.TrimSpace(line)) == reviewNeedsDecisionMarker {
			requested = true
			break
		}
	}
	if requested {
		return true, decisionQuestionFromBody(body)
	}
	if fallback := maintainerCallQuestion(body); fallback != "" {
		return true, fallback
	}
	return false, ""
}

// reviewDecisionRiskHighFromBody reports whether the reviewer marked its
// decision question `<!-- hive-decision-risk: high -->`. The marker is the
// reviewer's escape hatch from the self-authored decision denylist: a
// Hive-authored PR carrying it parks at stage/085-needs-decision even when
// the question's wording matches no review.DecisionRiskClass.
func reviewDecisionRiskHighFromBody(body string) bool {
	const prefix = "<!-- hive-decision-risk:"
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.HasPrefix(lower, prefix) || !strings.HasSuffix(lower, "-->") {
			continue
		}
		level := strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(lower, prefix), "-->"))
		if level == "high" {
			return true
		}
	}
	return false
}

func maintainerCallQuestion(body string) string {
	for _, line := range strings.Split(body, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		if !strings.Contains(lower, "maintainer call") || !strings.Contains(trimmed, "@") {
			continue
		}
		if strings.Contains(lower, "not a maintainer call") ||
			strings.Contains(lower, "is not a maintainer call") ||
			strings.Contains(lower, "isn't a maintainer call") ||
			strings.Contains(lower, "no maintainer call") {
			continue
		}
		return trimmed
	}
	return ""
}

// writeOneVerdict commits a single perspective's verdict where review.Collect
// will find it. One file per perspective, which is what the collector has
// always read — a combined review changes how verdicts arrive, not how they
// are stored, so nothing downstream needs to know the difference. It returns
// the report as stored and whether it was.
func (c *Client) writeOneVerdict(ctx context.Context, req ReviewRequest, report review.PerspectiveReport, dir string, prHead func() string) (review.PerspectiveReport, bool) {
	// The verdict must be about the PR that was actually reviewed. Without this
	// an agent authorized to comment on one PR could record a binding
	// requires_human/reject verdict against any other PR in the fleet.
	if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
		c.logger.Warn("review-request watcher: verdict target does not match reviewed PR, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("claimed", fmt.Sprintf("%s#%d", report.Repo, report.Number)),
			slog.String("agent", req.Agent))
		return report, false
	}
	// Bind to a real dispatch record. A combined review answers one kick with
	// several verdicts, so each element is checked on its own: the dispatch is
	// recorded per perspective, and a perspective nothing dispatched is refused
	// here exactly as it would be for a single-object verdict.
	ok, reason, dispatchHead, dispatchBodyDigest, hiveAuthored := c.verdictDispatchAuthorized(report, req)
	if !ok {
		c.logger.Warn("review-request watcher: verdict does not match a review dispatch, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("perspective", string(report.Perspective)),
			slog.String("agent", req.Agent),
			slog.String("reason", reason))
		return report, false
	}
	report.HeadSHA = c.canonicalVerdictHead(req, report.HeadSHA, dispatchHead, prHead)
	report.HiveAuthored = hiveAuthored
	if review.ConcreteRemediationText(req.Body) {
		report.ConcreteRemediation = true
	}
	if strings.TrimSpace(dispatchBodyDigest) != "" {
		if strings.TrimSpace(report.BodyDigest) == "" {
			c.logger.Warn("review-request watcher: verdict missing review body digest, discarded",
				slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
				slog.String("perspective", string(report.Perspective)),
				slog.String("agent", req.Agent))
			return report, false
		}
		if strings.TrimSpace(report.BodyDigest) != strings.TrimSpace(dispatchBodyDigest) {
			c.logger.Warn("review-request watcher: verdict body digest does not match review dispatch, discarded",
				slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
				slog.String("perspective", string(report.Perspective)),
				slog.String("agent", req.Agent))
			return report, false
		}
	}

	// Re-marshalled from the validated struct rather than written through from
	// the request. One array element is not a standalone document, and the
	// collector reads each file as one report; re-marshalling also drops the
	// descriptive extra keys models add, which ValidateReport tolerates but
	// nothing downstream reads, and carries the bound head SHA above.
	raw, err := json.Marshal(report)
	if err != nil {
		c.logger.Warn("review-request watcher: could not serialize verdict",
			slog.String("perspective", string(report.Perspective)), slog.String("error", err.Error()))
		return report, false
	}

	name := review.ReviewReportFilePrefix +
		verdictSlug(report.Repo) + "-" +
		strconv.Itoa(report.Number) + "-" +
		verdictSlug(string(report.Perspective)) +
		review.ReviewReportFileSuffix
	path := filepath.Join(dir, name)

	// Write-then-rename: review.Collect scans this dir on its own schedule and
	// must never observe a half-written report.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		c.logger.Warn("review-request watcher: could not write verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return report, false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		c.logger.Warn("review-request watcher: could not commit verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return report, false
	}
	c.logger.Info("review-request watcher: verdict recorded",
		slog.String("repo", report.Repo), slog.Int("number", report.Number),
		slog.String("perspective", string(report.Perspective)),
		slog.String("verdict", string(report.Verdict)),
		slog.String("path", path))
	return report, true
}

func (c *Client) verdictDispatchAuthorized(report review.PerspectiveReport, req ReviewRequest) (bool, string, string, string, bool) {
	state, err := review.LoadDispatchState("")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c.advisoryVerdictAuthorized(report, req)
		}
		return false, "dispatch_state_unavailable", "", "", false
	}
	now := time.Now().UTC()
	for _, p := range state.Pending {
		if !pendingReviewMatchesVerdict(p, report, req) {
			continue
		}
		if c.sameVerdictAuthor(verdictAuthorAgent(p.AuthorAgent, report), req.Agent) {
			return false, "author_self_approval", "", "", false
		}
		return true, "", p.HeadSHA, p.BodyDigest, strings.TrimSpace(p.AuthorAgent) != ""
	}
	cutoff := now.Add(-review.RecentDispatchTTL)
	for _, r := range state.Recent {
		if !recentReviewMatchesVerdict(r, report, req) {
			continue
		}
		if !r.Confirmed.IsZero() && r.Confirmed.Before(cutoff) {
			continue
		}
		if c.sameVerdictAuthor(verdictAuthorAgent(r.AuthorAgent, report), req.Agent) {
			return false, "author_self_approval", "", "", false
		}
		return true, "", r.HeadSHA, r.BodyDigest, strings.TrimSpace(r.AuthorAgent) != ""
	}
	// Queue-mode reviewer prompts can reach the relay through the ordinary
	// PR-list comment path rather than a review-swarm dispatch. That still is
	// an authorized hive-review request, and with an explicit head SHA it is
	// precise enough to record as an advisory verdict. Without this fallback,
	// contributor PRs that were actually reviewed stayed permanently
	// "awaiting review approval", and combined verdict arrays from the
	// reviewer recorded only the one perspective whose dispatch happened to be
	// assigned to that same agent.
	return c.advisoryVerdictAuthorized(report, req)
}

// advisoryVerdictAuthorized admits a verdict no dispatch row binds. The
// verdict feeds the same aggregate that gates merge eligibility and
// recommends closing a PR, so the submitting lane must be one this hive
// reviews with: any other lane that can reach the relay could otherwise
// approve or veto any PR it did not author.
func (c *Client) advisoryVerdictAuthorized(report review.PerspectiveReport, req ReviewRequest) (bool, string, string, string, bool) {
	head := strings.TrimSpace(report.HeadSHA)
	if head == "" {
		return false, "missing_head_sha_without_dispatch", "", "", false
	}
	if !c.isReviewerAgent(req.Agent) {
		return false, "non_reviewer_without_dispatch", "", "", false
	}
	if c.sameVerdictAuthor(verdictAuthorAgent("", report), req.Agent) {
		return false, "author_self_approval", "", "", false
	}
	return true, "", head, strings.TrimSpace(report.BodyDigest), false
}

func pendingReviewMatchesVerdict(p review.PendingReview, report review.PerspectiveReport, req ReviewRequest) bool {
	return reviewDispatchFieldsMatch(p.Repo, p.Number, p.HeadSHA, p.Perspective, p.Agent, report, req)
}

func recentReviewMatchesVerdict(r review.RecentReview, report review.PerspectiveReport, req ReviewRequest) bool {
	return reviewDispatchFieldsMatch(r.Repo, r.Number, r.HeadSHA, r.Perspective, r.Agent, report, req)
}

func reviewDispatchFieldsMatch(repo string, number int, headSHA string, perspective review.Perspective, agent string, report review.PerspectiveReport, req ReviewRequest) bool {
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(report.Repo)) {
		return false
	}
	if number != report.Number || perspective != report.Perspective || strings.TrimSpace(agent) != strings.TrimSpace(req.Agent) {
		return false
	}
	// An abbreviated head from the reviewer still binds to the dispatch that
	// named the full one (review.SameHeadSHA).
	return review.HeadSHAMatchesOrBlank(headSHA, report.HeadSHA)
}

// sameVerdictAuthor reports whether agent belongs to the authoring lane's
// family: the author itself, its base, or a sibling replica.
func (c *Client) sameVerdictAuthor(authorAgent, agent string) bool {
	authorAgent = strings.TrimSpace(authorAgent)
	agent = strings.TrimSpace(agent)
	if authorAgent == "" || agent == "" {
		return false
	}
	return authorAgent == agent || c.agentBaseName(authorAgent) == c.agentBaseName(agent)
}

func verdictAuthorAgent(stateAuthor string, report review.PerspectiveReport) string {
	if strings.TrimSpace(stateAuthor) != "" {
		return stateAuthor
	}
	return auditAuthorAgentForPR(report.Repo, report.Number, reviewVerdictAuditPath, time.Now().UTC().Add(-reviewVerdictAuditLookback))
}

type verdictAuditEntry struct {
	Timestamp string `json:"ts"`
	Action    string `json:"action"`
	Detail    string `json:"detail,omitempty"`
	Agent     string `json:"agent,omitempty"`
}

func auditAuthorAgentForPR(repo string, number int, path string, since time.Time) string {
	return auditAuthorMetaForPR(repo, number, path, since).Agent
}

// auditAuthorMetaForPR is the launch metadata (agent, backend, model) of the
// agent_pr_created audit entry for repo#number: the same InvocationMeta the
// PR's attribution trailer was rendered from. Agent is empty when no hive
// agent opened the PR.
func auditAuthorMetaForPR(repo string, number int, path string, since time.Time) InvocationMeta {
	var author InvocationMeta
	exactMatch := false
	for _, auditFile := range verdictAuditLogFiles(path) {
		data, err := readVerdictAuditLogFile(auditFile)
		if err != nil {
			continue
		}
		for _, line := range bytes.Split(data, []byte("\n")) {
			line = bytes.TrimSpace(line)
			if len(line) == 0 {
				continue
			}
			var entry verdictAuditEntry
			if json.Unmarshal(line, &entry) != nil || entry.Action != AuditActionAgentPRCreated || strings.TrimSpace(entry.Agent) == "" {
				continue
			}
			if !since.IsZero() {
				ts, err := time.Parse(time.RFC3339, entry.Timestamp)
				if err != nil || ts.Before(since) {
					continue
				}
			}
			entryRepo, entryNumber := auditPRDetailTarget(entry.Detail)
			if entryNumber != number {
				continue
			}
			switch verdictRepoMatchRank(entryRepo, repo) {
			case 2:
				author = auditPRDetailMeta(entry)
				exactMatch = true
			case 1:
				if !exactMatch {
					author = auditPRDetailMeta(entry)
				}
			}
		}
	}
	return author
}

// auditPRDetailMeta reads the backend and model pairs InvocationMeta.AuditDetail
// wrote into an agent_pr_created entry's detail.
func auditPRDetailMeta(entry verdictAuditEntry) InvocationMeta {
	m := InvocationMeta{Agent: strings.TrimSpace(entry.Agent)}
	for _, part := range strings.Split(entry.Detail, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "backend":
			m.Backend = strings.TrimSpace(v)
		case "model":
			m.Model = strings.TrimSpace(v)
		}
	}
	return m
}

func verdictAuditLogFiles(filePath string) []string {
	dir := filepath.Dir(filePath)
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	prefix := strings.TrimSuffix(base, ext)
	patterns := []string{
		filePath,
		filePath + ".*",
		filepath.Join(dir, prefix+"-*"+ext),
		filepath.Join(dir, prefix+"-*"+ext+".gz"),
	}
	seen := map[string]bool{}
	var files []string
	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, p := range matches {
			if seen[p] {
				continue
			}
			seen[p] = true
			files = append(files, p)
		}
	}
	sort.Strings(files)
	return files
}

const maxVerdictAuditFileReadBytes = 64 << 20

func readVerdictAuditLogFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var r io.Reader = f
	if strings.HasSuffix(path, ".gz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer func() { _ = gz.Close() }()
		r = gz
	}
	return io.ReadAll(io.LimitReader(r, maxVerdictAuditFileReadBytes))
}

func auditPRDetailTarget(detail string) (string, int) {
	var repo string
	var number int
	for _, part := range strings.Split(detail, ",") {
		k, v, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		switch k {
		case "repo":
			repo = strings.TrimSpace(v)
		case "number":
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err == nil {
				number = n
			}
		}
	}
	return repo, number
}

func verdictRepoMatchRank(a, b string) int {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if strings.EqualFold(a, b) {
		return 2
	}
	if !strings.Contains(a, "/") && strings.EqualFold(a, verdictBareRepo(b)) {
		return 1
	}
	return 0
}

func verdictBareRepo(repo string) string {
	if i := strings.LastIndex(repo, "/"); i >= 0 {
		return repo[i+1:]
	}
	return repo
}

// verdictSlug reduces an identifier to a filename-safe token. report.Repo is
// agent-supplied, so "../../etc/cron.d/x" must not become a path: everything
// outside the allowlist collapses to "-", which leaves no separators and no
// dot-dot for the join to walk.
func verdictSlug(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "unknown"
	}
	return out
}
