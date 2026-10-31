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
	redundantOf := reviewRedundantOfNumber(req.Body)
	if redundantOf == 0 && decisionRequested {
		// A reviewer that wrote "already landed via #N" but forgot the
		// marker still named the PR; carry it so the hive's own
		// already-landed close can act (hivecommons/hive#15647).
		redundantOf = review.AlreadyLandedPR(0, decisionQuestion)
	}
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
	prSnapshot := c.reviewedPRSnapshotFunc(ctx, req)
	prHead := func() string { return prSnapshot().Head }
	var recorded []review.PerspectiveReport
	wake := false
	for _, report := range reports {
		if decisionRequested {
			report.DecisionRequested = true
			report.DecisionQuestion = decisionQuestion
			report.DecisionRiskHigh = decisionRiskHigh
			report.Verdict = review.VerdictRequiresHuman
		}
		if redundantOf > 0 {
			report.RedundantOf = redundantOf
		}
		report.PermissionsTighteningOnly = tightening
		if written, ok, dispatched := c.writeOneVerdict(ctx, req, report, dir, prHead, prSnapshot); ok {
			recorded = append(recorded, written)
			wake = wake || dispatched
		}
	}
	// One wake per request, however many perspectives a combined review
	// answered: the head that wrote them is free, and the PR may be ready
	// for its next head or its decision kick. An advisory verdict freed no
	// dispatch slot, so it wakes nothing (hivecommons/hive#14705).
	if wake {
		c.notifyReviewVerdict(req.Repo, req.Number)
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

// reviewedPRSnapshot is what the relay reads from the PR itself: its head
// SHA, and the digest of its current title and body.
type reviewedPRSnapshot struct {
	Head       string
	BodyDigest string
	// Author is the PR's login author, for the hive-authored fallback.
	Author string
	// Base is the PR's base branch, for the same-patch test across a head
	// move (hivecommons/hive#15823).
	Base string
	// Supersedes is the PR the body declares this one supersedes (0 when
	// none): with hive authorship it marks the hive's superseding copy,
	// whose reviewer findings route to the fixer rather than a person.
	Supersedes int
}

// reviewedPRSnapshotFunc returns a memoized lookup of req's PR head SHA and
// body digest: one REST call per relayed request at most, and none when
// nothing asks. A fetch failure is logged once and yields an empty snapshot.
func (c *Client) reviewedPRSnapshotFunc(ctx context.Context, req ReviewRequest) func() reviewedPRSnapshot {
	var (
		snap    reviewedPRSnapshot
		fetched bool
	)
	return func() reviewedPRSnapshot {
		if fetched {
			return snap
		}
		fetched = true
		if c == nil || c.client == nil {
			return snap
		}
		owner, name := c.splitRepo(strings.TrimSpace(req.Repo))
		if owner == "" || name == "" || req.Number <= 0 {
			return snap
		}
		pr, _, err := c.client.PullRequests.Get(WithRESTCaller(ctx, "hive:review_verdict_relay"), owner, name, req.Number)
		if err != nil || pr == nil {
			c.logger.Warn("review-request watcher: could not fetch PR to bind verdict head and body digest",
				slog.String("repo", req.Repo), slog.Int("number", req.Number))
			return snap
		}
		snap = reviewedPRSnapshot{
			Head:       strings.TrimSpace(pr.GetHead().GetSHA()),
			BodyDigest: review.PullRequestBodyDigest(pr.GetTitle(), pr.GetBody()),
			Author:     safeGetLogin(pr.GetUser()),
			Base:       strings.TrimSpace(pr.GetBase().GetRef()),
			Supersedes: prSupersedesNumberOrZero(pr.GetBody()),
		}
		return snap
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

// reviewRedundantOfNumber reads the PR number from the
// `<!-- hive-redundant-of: #N -->` marker, 0 when absent or unparseable.
func reviewRedundantOfNumber(body string) int {
	v := strings.TrimSpace(hiddenMarkerValue(body, reviewRedundantOfMarker))
	v = strings.TrimPrefix(v, "#")
	if i := strings.IndexAny(v, " \t,;)"); i >= 0 {
		v = v[:i]
	}
	// owner/repo#N and .../pull/N both end in the number.
	if i := strings.LastIndexAny(v, "/#"); i >= 0 {
		v = v[i+1:]
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return 0
	}
	return n
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
// the report as stored, whether it was, and whether it was bound to a review
// dispatch rather than admitted as advisory.
func (c *Client) writeOneVerdict(ctx context.Context, req ReviewRequest, report review.PerspectiveReport, dir string, prHead func() string, prSnapshot func() reviewedPRSnapshot) (review.PerspectiveReport, bool, bool) {
	// The verdict must be about the PR that was actually reviewed. Without this
	// an agent authorized to comment on one PR could record a binding
	// requires_human/reject verdict against any other PR in the fleet.
	if !strings.EqualFold(strings.TrimSpace(report.Repo), strings.TrimSpace(req.Repo)) || report.Number != req.Number {
		c.logger.Warn("review-request watcher: verdict target does not match reviewed PR, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("claimed", fmt.Sprintf("%s#%d", report.Repo, report.Number)),
			slog.String("agent", req.Agent))
		return report, false, false
	}
	// Bind to a real dispatch record. A combined review answers one kick with
	// several verdicts, so each element is checked on its own: the dispatch is
	// recorded per perspective, and a perspective nothing dispatched is refused
	// here exactly as it would be for a single-object verdict.
	auth := c.verdictDispatchAuthorizedAt(ctx, report, req, prSnapshot)
	if !auth.OK {
		c.logger.Warn("review-request watcher: verdict does not match a review dispatch, discarded",
			slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
			slog.String("perspective", string(report.Perspective)),
			slog.String("agent", req.Agent),
			slog.String("reason", auth.Reason))
		return report, false, false
	}
	// A dispatched verdict is stored under the head its dispatch record names,
	// whatever the reviewer wrote: the match above already proved the two
	// agree (or that the reviewer's value names no revision at all), and a
	// garbled head would orphan the report under a key nothing looks up
	// (hivecommons/hive#14361). An advisory verdict keeps its own head.
	if dispatchHead := strings.TrimSpace(auth.HeadSHA); auth.Dispatched && dispatchHead != "" {
		if strings.TrimSpace(report.HeadSHA) != dispatchHead {
			c.logger.Debug("review-request watcher: verdict head_sha replaced by the dispatch head",
				slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
				slog.String("perspective", string(report.Perspective)),
				slog.String("agent", req.Agent))
		}
		report.HeadSHA = dispatchHead
	} else {
		report.HeadSHA = c.canonicalVerdictHead(req, report.HeadSHA, auth.HeadSHA, prHead)
	}
	report.HiveAuthored = auth.HiveAuthored
	if prSnapshot != nil && (report.DecisionRequested || report.Verdict == review.VerdictRequiresHuman) {
		// Only a verdict whose routing depends on authorship pays for the
		// PR fetch; an approve or changes_requested never does.
		snap := prSnapshot()
		if !report.HiveAuthored && c.IsHiveAuthorLogin(snap.Author) {
			// The App's own PR under a dispatch record that did not carry
			// authorship (an adoption with no agent attribution, or a record
			// written before hive_authored existed): the login settles it, so
			// the aggregate can self-resolve its decision questions and the
			// stage engine routes it as the hive's (hivecommons/hive#15499).
			report.HiveAuthored = true
		}
		// The hive's superseding copy of another PR ("Supersedes #N" under
		// the App's login): its requires_human findings — security-sensitive
		// included — go to the fixer, never to an operator decision
		// (owner rule, hivecommons/hive: hive superseder auto-merge).
		report.HiveSuperseding = report.HiveAuthored && snap.Supersedes > 0 && c.IsHiveAuthorLogin(snap.Author)
	}
	// The review head, and whether this is the decision step's reconciled
	// report, come from the dispatch record this verdict bound to, never the
	// reviewer's own JSON: a head cannot claim to be another head or to have
	// decided, and an advisory admission carries neither
	// (hivecommons/hive#13542). Only a decided report may carry dispositions.
	report.Head = auth.ReviewHead
	report.Decided = auth.Decided
	if !report.Decided {
		report.Dispositions = nil
	}
	if review.ConcreteRemediationText(req.Body) {
		report.ConcreteRemediation = true
	}
	// The body digest a verdict is grouped under comes from the dispatch
	// record, never the reviewer's JSON. The dispatch recorded the digest of
	// the exact title and body the reviewer was given; asking a model to copy
	// a 64-character hash back proved unreliable (prose summaries, a
	// "sha256:" prefix, a hash of something else), and every miscopy left the
	// report orphaned in its own aggregate, so its pending entry never cleared
	// and the swarm deadlocked (hivecommons/hive#14361). A dispatch record
	// with no digest stamps an empty one, so every head's verdict for that
	// PR and head SHA shares one aggregate instead of each landing under
	// whatever the model wrote (hivecommons/hive#14594). An advisory verdict
	// has no dispatch record and keeps the digest it reported; when it
	// reported none, the digest of the PR's current title and body is
	// stamped, so the planner can tell a later body edit from an unknown
	// one (hivecommons/hive#14665).
	if dispatchBodyDigest := strings.TrimSpace(auth.BodyDigest); auth.Dispatched {
		if strings.TrimSpace(report.BodyDigest) != dispatchBodyDigest {
			c.logger.Debug("review-request watcher: verdict body digest replaced by the dispatch digest",
				slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
				slog.String("perspective", string(report.Perspective)),
				slog.String("agent", req.Agent))
		}
		report.BodyDigest = dispatchBodyDigest
	} else if strings.TrimSpace(report.BodyDigest) == "" && prSnapshot != nil {
		report.BodyDigest = prSnapshot().BodyDigest
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
		return report, false, false
	}

	// One file per perspective, and per review head when the perspective was
	// dispatched to several heads, so each head's verdict is kept rather than
	// the last one overwriting the others.
	name := review.ReviewReportFilePrefix +
		verdictSlug(report.Repo) + "-" +
		strconv.Itoa(report.Number) + "-" +
		verdictSlug(string(report.Perspective))
	if report.Head != "" {
		name += "-" + verdictSlug(report.Head)
	}
	if report.Decided {
		name += "-decided"
	}
	name += review.ReviewReportFileSuffix
	path := filepath.Join(dir, name)

	// Write-then-rename: review.Collect scans this dir on its own schedule and
	// must never observe a half-written report.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		c.logger.Warn("review-request watcher: could not write verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return report, false, false
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		c.logger.Warn("review-request watcher: could not commit verdict",
			slog.String("path", path), slog.String("error", err.Error()))
		return report, false, false
	}
	c.logger.Info("review-request watcher: verdict recorded",
		slog.String("repo", report.Repo), slog.Int("number", report.Number),
		slog.String("perspective", string(report.Perspective)),
		slog.String("verdict", string(report.Verdict)),
		slog.String("path", path))
	return report, true, auth.Dispatched
}

// verdictAuthorization is what admitting a verdict establishes about it:
// whether it binds (and if not, why), and what the dispatch record it bound
// to said about the PR head, the review body digest, whether the PR is hive-
// authored, and the review head the perspective was dispatched to. An
// advisory admission (no dispatch row) carries only what the report itself
// claimed, and no review head.
type verdictAuthorization struct {
	OK           bool
	Reason       string
	HeadSHA      string
	BodyDigest   string
	HiveAuthored bool
	ReviewHead   string
	// Dispatched marks a verdict bound to a dispatch record (pending or
	// recent) rather than admitted as advisory. Only a dispatched verdict
	// takes its head SHA and body digest from the record.
	Dispatched bool
	// Decided marks a verdict bound to the review-heads decision step's
	// dispatch: the reconciled report for its perspective (#13542).
	Decided bool
	// HeadMove is set when the verdict bound to a dispatch row for an
	// earlier head (hivecommons/hive#15823); HeadSHA is then the head it
	// was accepted for.
	HeadMove *verdictHeadMove
}

// dispatchRecordHiveAuthored is the authorship a dispatch record vouches
// for: the planner's hive_authored flag (hivecommons/hive#15499) or, for
// records written before it existed, an agent attribution.
func dispatchRecordHiveAuthored(authorAgent string, hiveAuthored bool) bool {
	return hiveAuthored || strings.TrimSpace(authorAgent) != ""
}

func verdictRefused(reason string) verdictAuthorization {
	return verdictAuthorization{Reason: reason}
}

// verdictDispatchAuthorized binds a verdict with no PR lookup available:
// the backlog filer's path, where a head move cannot be resolved and the
// dispatch rows are matched as written.
func (c *Client) verdictDispatchAuthorized(report review.PerspectiveReport, req ReviewRequest) verdictAuthorization {
	return c.verdictDispatchAuthorizedAt(context.Background(), report, req, nil)
}

// verdictDispatchAuthorizedAt binds a verdict to a dispatch row. Rows are
// ranked, best first: the row the verdict names exactly (its agent and head);
// the same head dispatched to another reviewer-role agent (a verdict from a
// sibling reviewer answers the perspective just as well, hivecommons/hive#15823);
// and a row for an earlier head when the verdict names the PR's current head
// or a head carrying the same patch (verdictHeadMove). prSnapshot may be nil,
// in which case no head move is resolved.
func (c *Client) verdictDispatchAuthorizedAt(ctx context.Context, report review.PerspectiveReport, req ReviewRequest, prSnapshot func() reviewedPRSnapshot) verdictAuthorization {
	state, err := review.LoadDispatchState("")
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return c.advisoryVerdictAuthorized(report, req)
		}
		return verdictRefused("dispatch_state_unavailable")
	}
	now := time.Now().UTC()
	reviewerRole := c.isReviewerAgent(req.Agent)
	moves := c.verdictHeadMover(ctx, req, report, prSnapshot)
	bestTier := -1
	var best verdictAuthorization
	consider := func(tier int, auth verdictAuthorization) {
		if tier < 0 || (bestTier >= 0 && tier >= bestTier) {
			return
		}
		bestTier, best = tier, auth
	}
	for _, p := range state.Pending {
		tier, move := reviewDispatchRowTier(p.Repo, p.Number, p.HeadSHA, p.Perspective, p.Agent, report, req, reviewerRole, moves)
		if tier < 0 {
			continue
		}
		if c.sameVerdictAuthor(verdictAuthorAgent(p.AuthorAgent, report), req.Agent) {
			return verdictRefused("author_self_approval")
		}
		consider(tier, verdictAuthorization{OK: true, Dispatched: true, HeadSHA: p.HeadSHA, BodyDigest: p.BodyDigest, HiveAuthored: dispatchRecordHiveAuthored(p.AuthorAgent, p.HiveAuthored), ReviewHead: strings.TrimSpace(p.Head), Decided: p.Decision, HeadMove: move})
	}
	if bestTier < 0 {
		cutoff := now.Add(-review.RecentDispatchTTL)
		for _, r := range state.Recent {
			tier, move := reviewDispatchRowTier(r.Repo, r.Number, r.HeadSHA, r.Perspective, r.Agent, report, req, reviewerRole, moves)
			if tier < 0 {
				continue
			}
			if !r.Confirmed.IsZero() && r.Confirmed.Before(cutoff) {
				continue
			}
			if c.sameVerdictAuthor(verdictAuthorAgent(r.AuthorAgent, report), req.Agent) {
				return verdictRefused("author_self_approval")
			}
			consider(tier, verdictAuthorization{OK: true, Dispatched: true, HeadSHA: r.HeadSHA, BodyDigest: r.BodyDigest, HiveAuthored: dispatchRecordHiveAuthored(r.AuthorAgent, r.HiveAuthored), ReviewHead: strings.TrimSpace(r.Head), Decided: r.Decision, HeadMove: move})
		}
	}
	if bestTier >= 0 {
		if best.HeadMove != nil {
			best = c.acceptVerdictAcrossHeadMove(best, state, report, req)
		}
		return best
	}
	// Queue-mode reviewer prompts can reach the relay through the ordinary
	// PR-list comment path rather than a review-swarm dispatch. That still is
	// an authorized hive-review request, and with an explicit head SHA it is
	// precise enough to record as an advisory verdict. Without this fallback,
	// contributor PRs that were actually reviewed stayed permanently
	// "awaiting review approval", and combined verdict arrays from the
	// reviewer recorded only the one perspective whose dispatch happened to be
	// assigned to that same agent.
	c.logAdvisoryFallbackCandidates(state, report, req)
	return c.advisoryVerdictAuthorized(report, req)
}

// verdictHeadMove is how a verdict bound to a dispatch row for an earlier
// head: the row's head, the head the verdict is recorded under, and why the
// move was accepted (hivecommons/hive#15823).
type verdictHeadMove struct {
	From string
	To   string
	Why  string
}

const (
	// headMoveReportedCurrent: the reviewer reported the PR's current head —
	// it reviewed the live PR, which is exactly the verdict wanted.
	headMoveReportedCurrent = "reported-current-head"
	// headMoveSamePatch: the dispatched head and the reported head carry the
	// same patch against the base (a rebase or merge-from-base).
	headMoveSamePatch = "same-patch"
)

// verdictHeadMover returns the head-move test for one verdict: given a
// dispatch row's head and the head the reviewer reported, the move to accept
// or nil. The PR fetch and the compares run only when a row's head actually
// differs from a SHA-like reported head, and each pair is tested once.
func (c *Client) verdictHeadMover(ctx context.Context, req ReviewRequest, report review.PerspectiveReport, prSnapshot func() reviewedPRSnapshot) func(rowHead string) *verdictHeadMove {
	reported := strings.TrimSpace(report.HeadSHA)
	if prSnapshot == nil || !verdictHeadLooksLikeSHA(reported) {
		return func(string) *verdictHeadMove { return nil }
	}
	memo := map[string]*verdictHeadMove{}
	return func(rowHead string) *verdictHeadMove {
		rowHead = strings.TrimSpace(rowHead)
		if rowHead == "" || review.SameHeadSHA(rowHead, reported) {
			return nil
		}
		if move, seen := memo[rowHead]; seen {
			return move
		}
		var move *verdictHeadMove
		snap := prSnapshot()
		switch {
		case snap.Head != "" && review.SameHeadSHA(reported, snap.Head):
			move = &verdictHeadMove{From: rowHead, To: snap.Head, Why: headMoveReportedCurrent}
		case c.ReviewHeadsSamePatch(ctx, req.Repo, snap.Base, rowHead, reported):
			move = &verdictHeadMove{From: rowHead, To: reported, Why: headMoveSamePatch}
		}
		memo[rowHead] = move
		return move
	}
}

// acceptVerdictAcrossHeadMove records a verdict bound across a head move
// under the head it was accepted for, and rewrites the in-flight dispatch
// row to that head so the planner sees the perspective as asked (not owed)
// at the PR's current head instead of dispatching it again.
func (c *Client) acceptVerdictAcrossHeadMove(auth verdictAuthorization, state review.DispatchState, report review.PerspectiveReport, req ReviewRequest) verdictAuthorization {
	move := auth.HeadMove
	auth.HeadSHA = move.To
	if rebound, changed := review.RebindPendingHead(state, req.Repo, req.Number, report.Perspective, move.From, move.To); changed {
		if err := review.WriteDispatchState("", rebound); err != nil {
			c.logger.Warn("review-request watcher: could not rewrite dispatch row head after head move",
				slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
				slog.String("error", err.Error()))
		}
	}
	c.logger.Info("verdict accepted across head move",
		slog.String("pr", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
		slog.String("h1", move.From),
		slog.String("h2", move.To),
		slog.String("why", move.Why),
		slog.String("perspective", string(report.Perspective)),
		slog.String("agent", strings.TrimSpace(req.Agent)))
	return auth
}

// logAdvisoryFallbackCandidates records why a verdict from an agent this hive
// dispatched to for the PR bound to no dispatch row and fell back to advisory:
// what the verdict claimed, and every row for that repo and number it was
// compared with (hivecommons/hive#15559). A verdict from an agent with no row
// for the PR is an ordinary advisory review and logs nothing.
func (c *Client) logAdvisoryFallbackCandidates(state review.DispatchState, report review.PerspectiveReport, req ReviewRequest) {
	agent := strings.TrimSpace(req.Agent)
	var candidates []string
	agentHasRow := false
	add := func(kind, repo string, number int, rowAgent string, perspective review.Perspective, headSHA string, decision bool) {
		if number != report.Number || !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(report.Repo)) {
			return
		}
		if strings.TrimSpace(rowAgent) == agent {
			agentHasRow = true
		}
		candidates = append(candidates, fmt.Sprintf("%s agent=%s perspective=%s head=%s decision=%t", kind, strings.TrimSpace(rowAgent), perspective, strings.TrimSpace(headSHA), decision))
	}
	for _, p := range state.Pending {
		add("pending", p.Repo, p.Number, p.Agent, p.Perspective, p.HeadSHA, p.Decision)
	}
	for _, r := range state.Recent {
		add("recent", r.Repo, r.Number, r.Agent, r.Perspective, r.HeadSHA, r.Decision)
	}
	if !agentHasRow || c.logger == nil {
		return
	}
	c.logger.Info("review-request watcher: verdict matched no dispatch row for its agent, falling back to advisory",
		slog.String("reviewed", fmt.Sprintf("%s#%d", req.Repo, req.Number)),
		slog.String("agent", agent),
		slog.String("perspective", string(report.Perspective)),
		slog.String("reported_head", strings.TrimSpace(report.HeadSHA)),
		slog.String("candidates", strings.Join(candidates, "; ")))
}

// advisoryVerdictAuthorized admits a verdict no dispatch row binds. The
// verdict feeds the same aggregate that gates merge eligibility and
// recommends closing a PR, so the submitting lane must be one this hive
// reviews with: any other lane that can reach the relay could otherwise
// approve or veto any PR it did not author.
func (c *Client) advisoryVerdictAuthorized(report review.PerspectiveReport, req ReviewRequest) verdictAuthorization {
	head := strings.TrimSpace(report.HeadSHA)
	if head == "" {
		return verdictRefused("missing_head_sha_without_dispatch")
	}
	if !c.isReviewerAgent(req.Agent) {
		return verdictRefused("non_reviewer_without_dispatch")
	}
	if c.sameVerdictAuthor(verdictAuthorAgent("", report), req.Agent) {
		return verdictRefused("author_self_approval")
	}
	return verdictAuthorization{OK: true, HeadSHA: head, BodyDigest: strings.TrimSpace(report.BodyDigest)}
}

// Dispatch row tiers, best first. A verdict binds to the best-ranked row.
const (
	rowTierExact        = iota // same agent, same head
	rowTierReviewerRole        // same head, another reviewer-role agent
	rowTierHeadMove            // same agent, earlier head (verdictHeadMove)
	rowTierHeadMoveRole        // another reviewer-role agent, earlier head
)

// reviewDispatchRowTier ranks one dispatch row against a verdict: -1 when it
// does not bind, else the tier, with the head move that made it bind when
// the row's head differs from the verdict's.
func reviewDispatchRowTier(repo string, number int, headSHA string, perspective review.Perspective, agent string, report review.PerspectiveReport, req ReviewRequest, reviewerRole bool, moves func(rowHead string) *verdictHeadMove) (int, *verdictHeadMove) {
	if !strings.EqualFold(strings.TrimSpace(repo), strings.TrimSpace(report.Repo)) {
		return -1, nil
	}
	if number != report.Number || perspective != report.Perspective {
		return -1, nil
	}
	sameAgent := strings.TrimSpace(agent) == strings.TrimSpace(req.Agent)
	// The row's agent need not be the posting one when the poster is a
	// reviewer-role lane: the perspective was asked, and a verdict from any
	// lane this hive reviews with answers it (hivecommons/hive#15823). A
	// non-reviewer still binds only to its own row.
	if !sameAgent && !reviewerRole {
		return -1, nil
	}
	tier := rowTierExact
	if !sameAgent {
		tier = rowTierReviewerRole
	}
	if reviewDispatchHeadMatches(headSHA, report) {
		return tier, nil
	}
	if moves == nil {
		return -1, nil
	}
	move := moves(headSHA)
	if move == nil {
		return -1, nil
	}
	if sameAgent {
		return rowTierHeadMove, move
	}
	return rowTierHeadMoveRole, move
}

func reviewDispatchHeadMatches(headSHA string, report review.PerspectiveReport) bool {
	// An abbreviated head from the reviewer still binds to the dispatch that
	// named the full one (review.SameHeadSHA). A head that is not a SHA at all
	// (prose, a placeholder) names no revision, so it binds like a blank one
	// and the dispatch head is stored instead (hivecommons/hive#14361).
	reported := report.HeadSHA
	if !verdictHeadLooksLikeSHA(reported) {
		reported = ""
	}
	return review.HeadSHAMatchesOrBlank(headSHA, reported)
}

// verdictHeadLooksLikeSHA reports whether a reviewer-written head_sha is a
// commit SHA, full or abbreviated hex. Anything else is treated as absent.
func verdictHeadLooksLikeSHA(head string) bool {
	h := strings.TrimSpace(head)
	if h == "" {
		return false
	}
	return review.IsAbbreviatedSHA(h) || review.IsFullHexSHA(h)
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
