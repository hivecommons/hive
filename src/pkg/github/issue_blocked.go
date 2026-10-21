package github

// Blocked-on-PR signal (hivecommons/hive#13093).
//
// A review-backlog finding whose evidence lives only in an unmerged PR — the
// cited file does not exist on the PR's base branch — is not work anyone can
// start: the code it names is not on any branch the hive or a contributor
// can push to. The same holds for a scanner triage that concludes "blocked on
// #N (not starting)". Before this signal existed such an issue was filed
// stage/010-actionable with `help wanted`, a contributor relay claimed it
// three times in an hour and the scanner @-mentioned the fork author to do
// the hive's work.
//
// The signal is the `blocked` label plus a machine-readable marker naming the
// PR, `<!-- hive:blocked-on pr=owner/repo#N -->`, in the issue body or a
// comment. The enumeration files a blocked issue under the filtered bucket,
// IssueStageFor derives no stage for it, IssueClaimBlockReason refuses every
// automated claim on it, and ReconcileBlockedIssues lifts the label once the
// tracked PR merges or closes.
//
// A blocked issue with no tracked PR may instead carry a deadline,
// `<!-- hive:recheck-after <RFC3339> -->` (hivecommons/hive#13310): the
// enumeration records it on the row ("blocked until <t>") and the sweep lifts
// the label once it passes, unless the body also carries
// `<!-- hive:ops-tracking -->` (a maintainer's soak checklist, left blocked).

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/gfi"
)

// BlockedLabel is the workflow label that parks an issue on an unmerged PR
// (or any outside blocker). It is reserved for agents (issue_request_label.go)
// — the hive applies it from the review backlog and the blocked-on comment
// relay — and the contributor lane already refuses it as a workflow skip.
const BlockedLabel = "blocked"

// ClaimBlockReasonBlockedPrefix prefixes the IssueClaimBlockReason for a
// blocked issue: "blocked: hivecommons/hive#13092", or "blocked: blocked"
// when the label stands without a marker.
const ClaimBlockReasonBlockedPrefix = "blocked: "

// blockedOnMarkerPrefix opens the marker. Kept distinct from every other
// hive:* marker so the sweep that lifts the block parses only its own.
const blockedOnMarkerPrefix = "<!-- hive:blocked-on pr="

// blockedUnblockedMarker opens the one-line note the sweep posts when it lifts
// the block; re-runs look for it before posting again.
const blockedUnblockedMarker = "<!-- hive:unblocked -->"

var blockedOnMarkerRE = regexp.MustCompile(`<!--\s*hive:blocked-on\s+pr=([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)\s*-->`)

// BlockedOnMarker renders the marker that names the PR an issue waits on.
func BlockedOnMarker(repo string, pr int) string {
	return blockedOnMarkerPrefix + strings.TrimSpace(repo) + "#" + strconv.Itoa(pr) + " -->"
}

// ParseBlockedOnMarker returns the first blocked-on marker in text.
func ParseBlockedOnMarker(text string) (repo string, pr int, ok bool) {
	m := blockedOnMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], n, true
}

// blockedFilterReasonPrefix opens the enumeration's filtered reason for a
// blocked issue; IssueStageFor and the sweep recognise it by prefix.
const blockedFilterReasonPrefix = "blocked"

// blockedFilterReason is the work-detail reason for a blocked issue:
// "blocked on owner/repo#N" when the body carries the marker, "blocked until
// <RFC3339>" when it carries a recheck-after deadline instead, else the bare
// label. It is what IssueStageFor echoes as the stage reason.
func blockedFilterReason(body string) string {
	if repo, pr, ok := ParseBlockedOnMarker(body); ok {
		return blockedFilterReasonPrefix + " on " + repo + "#" + strconv.Itoa(pr)
	}
	if at, ok := gfi.ParseRecheckAfter(body); ok {
		return blockedFilterReasonPrefix + " until " + at.Format(time.RFC3339)
	}
	return blockedFilterReasonPrefix
}

// blockedRecheck reads a blocked issue's recheck-after deadline (zero when
// absent) and whether the body carries the ops-tracking marker.
func blockedRecheck(body string) (time.Time, bool) {
	at, _ := gfi.ParseRecheckAfter(body)
	return at, gfi.HasOpsTrackingMarker(body)
}

// blockedStageReason is the IssueStageFor reason for an issue wearing the
// blocked label: the enumeration's reason when the cycle recorded one (it
// names the PR), else the bare label.
func blockedStageReason(in IssueStageInput) string {
	if in.Bucket == workBucketFiltered {
		if r := strings.TrimSpace(in.Reason); strings.HasPrefix(strings.ToLower(r), blockedFilterReasonPrefix) {
			return r
		}
	}
	return blockedFilterReasonPrefix
}

// blockedClaimReason is the IssueClaimBlockReason for a blocked issue.
func blockedClaimReason(body string) string {
	if repo, pr, ok := ParseBlockedOnMarker(body); ok {
		return ClaimBlockReasonBlockedPrefix + repo + "#" + strconv.Itoa(pr)
	}
	return ClaimBlockReasonBlockedPrefix + BlockedLabel
}

// IsBlockedIssueReason reports whether a work-detail or stage reason is the
// blocked signal's.
func IsBlockedIssueReason(reason string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(reason)), blockedFilterReasonPrefix)
}

// helpWantedLabel is the label the org's add-help-wanted workflow puts on
// every freshly labelled, unassigned issue. A blocked issue is not help the
// hive wants, so the filing omits it and the sweep strips it.
const helpWantedLabel = "help wanted"

// blockedSweepCommentLookups bounds how many blocked issues without a body
// marker have their comments read per cycle for the scanner's marker.
const blockedSweepCommentLookups = 10

// blockedSweepNow is the sweep's clock for recheck-after deadlines; tests
// override it.
var blockedSweepNow = time.Now

// reviewBacklogBaseProbe answers, once per filing pass, whether a finding's
// evidence file exists on the reviewed PR's base branch. The PR is fetched
// lazily; a fetch failure disables the probe for the pass (the finding files
// as it always did) rather than guessing.
type reviewBacklogBaseProbe struct {
	c       *Client
	repo    string
	pr      int
	loaded  bool
	baseRef string
	exists  map[string]bool
}

func (c *Client) newReviewBacklogBaseProbe(repo string, pr int) *reviewBacklogBaseProbe {
	return &reviewBacklogBaseProbe{c: c, repo: repo, pr: pr, exists: map[string]bool{}}
}

// BaseRef returns the PR's base branch, "" when it could not be read.
func (p *reviewBacklogBaseProbe) BaseRef(ctx context.Context) string {
	if p == nil || p.c == nil || p.c.client == nil {
		return ""
	}
	if !p.loaded {
		p.loaded = true
		owner, name := p.c.splitRepo(p.repo)
		pr, _, err := p.c.client.PullRequests.Get(ctx, owner, name, p.pr)
		if err == nil && pr != nil {
			p.baseRef = strings.TrimSpace(pr.GetBase().GetRef())
		}
	}
	return p.baseRef
}

// FileMissingOnBase reports true only when the probe can say for certain the
// file does not exist on the base branch: the PR introduces it, so the finding
// is blocked on the PR. Any uncertainty (no base ref, lookup error, empty
// path) reports false.
func (p *reviewBacklogBaseProbe) FileMissingOnBase(ctx context.Context, file string) bool {
	file = strings.TrimSpace(file)
	if file == "" {
		return false
	}
	base := p.BaseRef(ctx)
	if base == "" {
		return false
	}
	if exists, ok := p.exists[file]; ok {
		return !exists
	}
	owner, name := p.c.splitRepo(p.repo)
	exists, err := p.c.PathExistsAtRef(ctx, owner, name, file, base)
	if err != nil {
		return false
	}
	p.exists[file] = exists
	return !exists
}

// blockedBacklogLabels returns the filing labels for a blocked finding: the
// configured set plus `blocked`, minus `help wanted`.
func blockedBacklogLabels(labels []string) []string {
	out := make([]string, 0, len(labels)+1)
	for _, l := range labels {
		if strings.EqualFold(strings.TrimSpace(l), helpWantedLabel) {
			continue
		}
		out = append(out, l)
	}
	return appendLabelOnce(out, BlockedLabel)
}

// blockedBacklogNote is the body section that parks a review-backlog finding
// on the PR that introduces its evidence.
func blockedBacklogNote(repo string, pr int, file, baseRef string) string {
	var b strings.Builder
	b.WriteString(BlockedOnMarker(repo, pr))
	fmt.Fprintf(&b, "\n**Blocked on PR #%d.** `%s` does not exist on `%s`; this finding tracks code the PR introduces, so there is nothing to ship until it lands. The hive lifts the `blocked` label when the PR merges or closes.\n\n", pr, strings.TrimSpace(file), baseRef)
	return b.String()
}

// BlockedIssueResult summarises one ReconcileBlockedIssues pass.
type BlockedIssueResult struct {
	Items     int
	Unblocked int
	Stripped  int
	Errors    int
}

// blockedIssueTarget is one blocked issue the sweep looks at.
type blockedIssueTarget struct {
	repo         string
	number       int
	labels       []string
	reason       string
	recheckAfter time.Time
	opsTracking  bool
}

// ReconcileBlockedIssues walks the cycle's blocked issues (filtered rows
// whose reason is the blocked signal's) and, for each with a blocked-on
// marker in its body or comments, lifts the `blocked` label with a one-line
// note once the tracked PR has merged or closed — the next enumeration then
// stages it 010 from scratch. While the PR is open it strips `help wanted`
// (the org workflow adds it on every labelled issue) so the parked issue
// does not advertise itself. A row with no tracked PR whose recheck-after
// deadline has passed loses `blocked` with a one-line note (once per
// deadline) so the next enumeration re-triages it; ops-tracking rows are left
// alone. Lookup failures are counted and skipped.
func ReconcileBlockedIssues(ctx context.Context, client *Client, result *ActionableResult, logger *slog.Logger) BlockedIssueResult {
	var res BlockedIssueResult
	if client == nil || client.client == nil || result == nil {
		return res
	}
	if logger == nil {
		logger = slog.Default()
	}
	var targets []blockedIssueTarget
	repos := make([]string, 0, len(result.WorkDetailsByRepo))
	for repo := range result.WorkDetailsByRepo {
		repos = append(repos, repo)
	}
	sort.Strings(repos)
	for _, repo := range repos {
		for _, row := range result.WorkDetailsByRepo[repo].Issues {
			if row.Number <= 0 || row.Bucket != workBucketFiltered || !IsBlockedIssueReason(row.Reason) || !hasBlockedLabel(row.Labels) {
				continue
			}
			targets = append(targets, blockedIssueTarget{repo: row.Repo, number: row.Number, labels: row.Labels, reason: row.Reason, recheckAfter: row.RecheckAfter, opsTracking: row.OpsTracking})
		}
	}
	lookups := 0
	now := blockedSweepNow()
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		res.Items++
		prRepo, prNumber, ok := parseBlockedOnReason(t.reason)
		if !ok {
			if lookups >= blockedSweepCommentLookups {
				continue
			}
			lookups++
			scan, err := client.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
			prRepo, prNumber, ok = scan.prRepo, scan.pr, scan.ok
			if !ok {
				if !t.recheckAfter.IsZero() && !now.Before(t.recheckAfter) {
					liftBlockedDeadline(ctx, client, t, scan.deadlineNoted, err, &res, logger)
				}
				continue
			}
		}
		closed, err := client.pullRequestSettled(ctx, prRepo, prNumber)
		if err != nil {
			res.Errors++
			logger.Warn("blocked sweep: tracked PR lookup failed", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(prNumber), "error", err)
			continue
		}
		if closed {
			if err := client.RemoveLabel(ctx, t.repo, t.number, BlockedLabel); err != nil {
				res.Errors++
				logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
				continue
			}
			note := blockedUnblockedMarker + "\nUnblocked: " + prRepo + "#" + strconv.Itoa(prNumber) + " is no longer open. This issue is actionable again."
			if err := client.CreateIssueCommentBudgeted(ctx, t.repo, t.number, note); err != nil {
				logger.Warn("blocked sweep: unblock note not posted", "repo", t.repo, "issue", t.number, "error", err)
			}
			res.Unblocked++
			logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(prNumber))
			continue
		}
		if issueHasLabel(t.labels, helpWantedLabel) {
			if err := client.RemoveLabel(ctx, t.repo, t.number, helpWantedLabel); err != nil {
				res.Errors++
				logger.Warn("blocked sweep: help wanted not stripped", "repo", t.repo, "issue", t.number, "error", err)
				continue
			}
			res.Stripped++
		}
	}
	return res
}

// parseBlockedOnReason reads "blocked on owner/repo#N" back into its parts.
func parseBlockedOnReason(reason string) (repo string, pr int, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(reason), blockedFilterReasonPrefix+" on ")
	if !found {
		return "", 0, false
	}
	repo, num, found := strings.Cut(strings.TrimSpace(rest), "#")
	if !found || repo == "" {
		return "", 0, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return repo, n, true
}

// liftBlockedDeadline lifts `blocked` from an issue with no tracked PR whose
// recheck-after deadline has passed and posts the unblocked note, so the next
// enumeration stages it 010 for a fresh triage (hivecommons/hive#13310). An
// ops-tracking issue is left alone, and so is one that already carries the
// note for this deadline: the label came back after the hive lifted it.
func liftBlockedDeadline(ctx context.Context, client *Client, t blockedIssueTarget, noted bool, lookupErr error, res *BlockedIssueResult, logger *slog.Logger) {
	deadline := t.recheckAfter.UTC().Format(time.RFC3339)
	if t.opsTracking {
		logger.Info("blocked sweep: deadline passed on an ops-tracking issue; left blocked", "repo", t.repo, "issue", t.number, "recheck_after", deadline)
		return
	}
	if lookupErr != nil {
		res.Errors++
		logger.Warn("blocked sweep: comment lookup failed; deadline not acted on", "repo", t.repo, "issue", t.number, "recheck_after", deadline, "error", lookupErr)
		return
	}
	if noted {
		return
	}
	if err := client.RemoveLabel(ctx, t.repo, t.number, BlockedLabel); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	if err := client.CreateIssueCommentBudgeted(ctx, t.repo, t.number, blockedDeadlineNote(deadline)); err != nil {
		logger.Warn("blocked sweep: unblock note not posted", "repo", t.repo, "issue", t.number, "error", err)
	}
	res.Unblocked++
	logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "recheck_after", deadline)
}

// blockedDeadlineNote is the note posted when a recheck-after deadline
// lifts the block.
func blockedDeadlineNote(deadline string) string {
	return blockedUnblockedMarker + "\nUnblocked: deadline " + deadline + " passed; re-triaging."
}

// blockedSweepScan is what one comment read tells the sweep about an issue.
type blockedSweepScan struct {
	prRepo        string
	pr            int
	ok            bool
	deadlineNoted bool
}

// blockedSweepComments reads the issue's comments once: the newest
// blocked-on marker (the scanner's blocked note carries one) and, when
// recheckAfter is set, whether the unblocked note for that deadline is
// already there.
func (c *Client) blockedSweepComments(ctx context.Context, repo string, number int, recheckAfter time.Time) (blockedSweepScan, error) {
	var scan blockedSweepScan
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return scan, err
	}
	var passed string
	if !recheckAfter.IsZero() {
		passed = "deadline " + recheckAfter.UTC().Format(time.RFC3339) + " passed"
	}
	for i := len(comments) - 1; i >= 0; i-- {
		body := comments[i].GetBody()
		if !scan.ok {
			scan.prRepo, scan.pr, scan.ok = ParseBlockedOnMarker(body)
		}
		if passed != "" && strings.Contains(body, blockedUnblockedMarker) && strings.Contains(body, passed) {
			scan.deadlineNoted = true
		}
	}
	return scan, nil
}

// pullRequestSettled reports whether the PR is merged or closed. A 404 is
// reported as an error: not found is not settled.
func (c *Client) pullRequestSettled(ctx context.Context, repo string, number int) (bool, error) {
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return false, err
	}
	return pr.GetMerged() || strings.EqualFold(pr.GetState(), "closed"), nil
}

// mentionPattern matches a GitHub @login that is not part of a path, an
// email, or inline code.
var mentionPattern = regexp.MustCompile("(^|[^\\w`/.])@([A-Za-z0-9](?:[A-Za-z0-9-]{0,38}))")

// defangMentions turns @login into login so a relayed note reads the same
// without notifying anyone.
func defangMentions(text string) string {
	return mentionPattern.ReplaceAllString(text, "$1$2")
}

// blockedNoteBody is the scanner's blocked note as posted: the marker on
// top, the note with its mentions defanged.
func blockedNoteBody(repo string, pr int, body string) string {
	if _, _, ok := ParseBlockedOnMarker(body); ok {
		return defangMentions(body)
	}
	return BlockedOnMarker(repo, pr) + "\n" + defangMentions(body)
}
