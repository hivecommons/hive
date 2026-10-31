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
// On the hive's own project such a checklist instead gets one soak-evidence
// comment and the checklist lines it proves ticked (hivecommons/hive#13311,
// issue_recheck_soak.go).

import (
	"context"
	"fmt"
	"github.com/hivecommons/hive/pkg/prstatus"
	"log/slog"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
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

var (
	blockedOnMarkerRE      = regexp.MustCompile(`<!--\s*hive:blocked-on\s+pr=([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)\s*-->`)
	adoptedSupersedeRE     = regexp.MustCompile(`(?i)<!--\s*hive:adopted\b[^>]*\bmode=supersede\b[^>]*\bsuperseded_by=(\d+)`)
	adoptedSupersedeAttrRE = regexp.MustCompile(`(?i)<!--\s*hive:adopted\b[^>]*\bsuperseded_by=(\d+)[^>]*\bmode=supersede`)
	supersededByRE         = regexp.MustCompile(`(?i)\bSuperseded by #?(\d+)\b`)
	// blockedFilterReasonRE recognises blockedFilterReason's marker form so
	// a snapshot row that carries only the reason (RepoWorkIssue.Reason)
	// can still name its blocker.
	blockedFilterReasonRE = regexp.MustCompile(`^` + blockedFilterReasonPrefix + ` on ([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)$`)
)

// ParseSupersededBy finds a superseding PR number in text from an adoption
// marker or a "Superseded by #N" comment.
func ParseSupersededBy(text string) (int, bool) {
	if m := adoptedSupersedeRE.FindStringSubmatch(text); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	if m := adoptedSupersedeAttrRE.FindStringSubmatch(text); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	if m := supersededByRE.FindStringSubmatch(text); len(m) > 1 {
		if n, err := strconv.Atoi(m[1]); err == nil && n > 0 {
			return n, true
		}
	}
	return 0, false
}

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

// ParseBlockedOnReason returns the PR a blockedFilterReason names ("blocked
// on owner/repo#N"); false for every other reason.
func ParseBlockedOnReason(reason string) (repo string, pr int, ok bool) {
	m := blockedFilterReasonRE.FindStringSubmatch(strings.TrimSpace(reason))
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], n, true
}

// BlockedOnTarget names the PR an issue is blocked on from its body marker
// when the body is at hand, else from the enumeration's filtered reason.
func BlockedOnTarget(body, reason string) (repo string, pr int, ok bool) {
	if repo, pr, ok = ParseBlockedOnMarker(body); ok {
		return repo, pr, true
	}
	return ParseBlockedOnReason(reason)
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

// blockedRecheckDone reports whether the soak triage already ran on the
// body (hivecommons/hive#13311).
func blockedRecheckDone(body string) bool {
	return strings.Contains(body, RecheckDoneMarker)
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
const blockedSweepCommentLookups = 40

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
	// SoakTriaged counts ops-tracking issues that got soak evidence.
	SoakTriaged int
	Retargeted  int
	Errors      int
}

// blockedIssueTarget is one blocked issue the sweep looks at.
type blockedIssueTarget struct {
	repo         string
	number       int
	labels       []string
	reason       string
	recheckAfter time.Time
	opsTracking  bool
	recheckDone  bool
}

// ReconcileBlockedIssues walks the cycle's blocked issues (filtered rows
// whose reason is the blocked signal's) and, for each with a blocked-on
// marker in its body or comments, lifts the `blocked` label with a one-line
// note once the tracked PR has merged or closed — the next enumeration then
// stages it 010 from scratch. While the PR is open it strips `help wanted`
// (the org workflow adds it on every labelled issue) so the parked issue
// does not advertise itself. A row with no tracked PR whose recheck-after
// deadline has passed loses `blocked` with a one-line note (once per
// deadline) so the next enumeration re-triages it; an ops-tracking row on the
// hive's own project gets soak evidence instead (runRecheckSoak) and keeps
// `blocked`, other ops-tracking rows are left alone. A row with no marker,
// no deadline and no ops-tracking whose `blocked` the hive itself applied is
// lifted as a defect (liftBlockedMarkerless, hivecommons/hive#13364). Lookup
// failures are counted and skipped.
func ReconcileBlockedIssues(ctx context.Context, client *Client, result *ActionableResult, logger *slog.Logger) BlockedIssueResult {
	var res BlockedIssueResult
	if client == nil || client.client == nil || result == nil {
		return res
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
			targets = append(targets, blockedIssueTarget{repo: row.Repo, number: row.Number, labels: row.Labels, reason: row.Reason, recheckAfter: row.RecheckAfter, opsTracking: row.OpsTracking, recheckDone: row.RecheckDone})
		}
	}
	return client.reconcileBlockedTargets(ctx, targets, logger)
}

// BlockedSweepOptions configures SweepBlockedIssues.
type BlockedSweepOptions struct {
	Repos []string
}

// SweepBlockedIssues lists open issues with the `blocked` label across repos
// and reconciles their blocker status (unblocking settled blockers, re-targeting
// on supersede, stripping help wanted on open blockers).
func (c *Client) SweepBlockedIssues(ctx context.Context, opts BlockedSweepOptions) (BlockedIssueResult, error) {
	if c == nil || c.client == nil {
		return BlockedIssueResult{}, ErrNoGitHubClient
	}
	repos := opts.Repos
	if len(repos) == 0 {
		repos = c.getRepos()
	}
	sort.Strings(repos)
	var targets []blockedIssueTarget
	for _, repo := range repos {
		owner, repoName := c.splitRepo(repo)
		issues, err := c.listOpenBlockedIssues(ctx, owner, repoName)
		if err != nil {
			return BlockedIssueResult{}, err
		}
		for _, issue := range issues {
			if issue == nil || issue.GetNumber() <= 0 {
				continue
			}
			labels := issueLabelNames(issue.Labels)
			if !hasBlockedLabel(labels) {
				continue
			}
			body := issue.GetBody()
			at, ops := blockedRecheck(body)
			targets = append(targets, blockedIssueTarget{
				repo:         repo,
				number:       issue.GetNumber(),
				labels:       labels,
				reason:       blockedFilterReason(body),
				recheckAfter: at,
				opsTracking:  ops,
				recheckDone:  blockedRecheckDone(body),
			})
		}
	}
	return c.reconcileBlockedTargets(ctx, targets, c.logger), nil
}

func (c *Client) reconcileBlockedTargets(ctx context.Context, targets []blockedIssueTarget, logger *slog.Logger) BlockedIssueResult {
	var res BlockedIssueResult
	if c == nil || c.client == nil {
		return res
	}
	if logger == nil {
		logger = slog.Default()
	}
	lookups := 0
	now := blockedSweepNow()
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		res.Items++
		prRepo, prNumber, ok := parseBlockedOnReason(t.reason)
		// scanned: the comments were read without error and hold no marker,
		// so a marker-less lift may trust that nothing names a PR.
		scanned := false
		if lookups < blockedSweepCommentLookups {
			lookups++
			scan, err := c.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
			scanned = err == nil
			if scan.ok {
				prRepo, prNumber, ok = scan.prRepo, scan.pr, scan.ok
			}
			if !ok && !t.recheckAfter.IsZero() && !now.Before(t.recheckAfter) {
				if t.opsTracking && isRecheckSoakRepo(t.repo) {
					// hivecommons/hive#13311: a maintainer's soak checklist
					// on the hive's own project gets evidence, not a lift.
					runRecheckSoak(ctx, c, t, scan.evidenceNoted, err, now, &res, logger)
				} else {
					liftBlockedDeadline(ctx, c, t, scan.deadlineNoted, err, &res, logger)
					continue
				}
			}
		}
		if !ok {
			// No tracked PR, no deadline, no ops-tracking: a block the hive
			// applied itself with no marker is always a bug (a blocked
			// filing folded into an open twin, hivecommons/hive#13364), so
			// lift it. A maintainer's or a relay's marker-less label
			// (hivecommons/hive#13267) is left alone: nothing to lift, but
			// the issue still must not advertise itself.
			if scanned && t.recheckAfter.IsZero() && !t.opsTracking {
				if liftBlockedMarkerless(ctx, c, t, &res, logger) {
					continue
				}
			}
			stripBlockedHelpWanted(ctx, c, t, &res, logger)
			continue
		}
		settled, merged, err := c.pullRequestSettledDetails(ctx, prRepo, prNumber)
		if err != nil {
			res.Errors++
			logger.Warn("blocked sweep: tracked PR lookup failed", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(prNumber), "error", err)
			continue
		}
		if settled {
			// Only a closed-unmerged blocker can be superseded: a merged
			// blocker landed, so its issue is unblocked, never re-targeted.
			supersededBy, isSuperseded := 0, false
			if !merged {
				supersededBy, isSuperseded = c.pullRequestSupersededBy(ctx, prRepo, prNumber)
			}
			if isSuperseded && supersededBy > 0 && supersededBy != prNumber {
				ySettled, _, yErr := c.pullRequestSettledDetails(ctx, prRepo, supersededBy)
				if yErr != nil {
					res.Errors++
					logger.Warn("blocked sweep: superseding PR lookup failed", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(supersededBy), "error", yErr)
					continue
				}
				if ySettled {
					if err := c.unblockIssue(ctx, t.repo, t.number); err != nil {
						res.Errors++
						logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
						continue
					}
					note := blockedUnblockedMarker + fmt.Sprintf("\nUnblocked: %s#%d (superseding #%d) is no longer open. This issue is actionable again.", prRepo, supersededBy, prNumber)
					c.postUnblockedNote(ctx, t.repo, t.number, note, logger)
					res.Unblocked++
					logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(supersededBy), "superseded", prNumber)
					continue
				}
				// Superseding PR is still open: re-target relation to Y. The
				// re-target lives in a comment while the body still names X,
				// so a pass whose comment budget ran out before this issue
				// re-detects the supersede from the body every cycle; read
				// the comments now and skip when the newest marker already
				// moved off X.
				if !scanned {
					scan, scanErr := c.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
					if scanErr != nil {
						res.Errors++
						logger.Warn("blocked sweep: comment lookup before re-target failed", "repo", t.repo, "issue", t.number, "error", scanErr)
						continue
					}
					if scan.ok && (scan.prRepo != prRepo || scan.pr != prNumber) {
						stripBlockedHelpWanted(ctx, c, t, &res, logger)
						continue
					}
				}
				note := fmt.Sprintf("%s\nRe-targeted: blocker #%d was superseded by #%d.", BlockedOnMarker(prRepo, supersededBy), prNumber, supersededBy)
				if err := c.CreateIssueCommentBudgeted(ctx, t.repo, t.number, note); err != nil {
					logger.Warn("blocked sweep: re-target note not posted", "repo", t.repo, "issue", t.number, "error", err)
				}
				stripBlockedHelpWanted(ctx, c, t, &res, logger)
				res.Retargeted++
				logger.Info("blocked sweep: issue re-targeted on supersede", "repo", t.repo, "issue", t.number, "old_pr", prNumber, "new_pr", supersededBy)
				continue
			}

			if err := c.unblockIssue(ctx, t.repo, t.number); err != nil {
				res.Errors++
				logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
				continue
			}
			note := blockedUnblockedMarker + "\nUnblocked: " + prRepo + "#" + strconv.Itoa(prNumber) + " is no longer open. This issue is actionable again."
			c.postUnblockedNote(ctx, t.repo, t.number, note, logger)
			res.Unblocked++
			logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "pr", prRepo+"#"+strconv.Itoa(prNumber))
			continue
		}
		stripBlockedHelpWanted(ctx, c, t, &res, logger)
	}
	return res
}

// stripBlockedHelpWanted removes `help wanted` from a blocked issue that
// wears it, counting the strip or the failure on res.
func stripBlockedHelpWanted(ctx context.Context, client *Client, t blockedIssueTarget, res *BlockedIssueResult, logger *slog.Logger) {
	if !issueHasLabel(t.labels, helpWantedLabel) {
		return
	}
	if err := client.RemoveLabel(ctx, t.repo, t.number, helpWantedLabel); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: help wanted not stripped", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	res.Stripped++
}

// liftBlockedMarkerless lifts `blocked` from an issue with no blocked-on
// marker, no recheck-after deadline and no ops-tracking marker when the
// label's most recent application was by this hive (hivecommons/hive#13364):
// nothing the hive writes applies `blocked` without a marker, so such a block
// is a defect and would otherwise never lift. Reports whether the label came
// off. A human-applied label, an unreadable timeline, or a client with no
// hive identity configured leaves the issue alone, the last with one WARN so
// the stuck block is at least visible.
func liftBlockedMarkerless(ctx context.Context, client *Client, t blockedIssueTarget, res *BlockedIssueResult, logger *slog.Logger) bool {
	actor, found, err := client.blockedLabelActor(ctx, t.repo, t.number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: timeline lookup failed; marker-less block left alone", "repo", t.repo, "issue", t.number, "error", err)
		return false
	}
	if !found {
		return false
	}
	if !client.isOwnBotLogin(actor) && !client.IsHiveAuthorLogin(actor) {
		if len(client.hiveBotLogins()) == 0 {
			logger.Warn("blocked sweep: marker-less blocked label with no hive identity to compare against; left alone", "repo", t.repo, "issue", t.number, "actor", actor)
		}
		return false
	}
	if err := client.unblockIssue(ctx, t.repo, t.number); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
		return false
	}
	client.postUnblockedNote(ctx, t.repo, t.number, blockedMarkerlessNote, logger)
	res.Unblocked++
	logger.Info("blocked sweep: hive-applied marker-less block lifted", "repo", t.repo, "issue", t.number, "actor", actor)
	return true
}

// blockedMarkerlessNote is the note posted when the sweep lifts a
// hive-applied block that named no PR.
const blockedMarkerlessNote = blockedUnblockedMarker + "\nUnblocked: the `blocked` label was applied by the hive without a blocked-on marker, so nothing could lift it. This issue is actionable again."

// blockedLabelActor returns the login that most recently applied `blocked`
// to the issue, per its timeline; found is false when no labeled event
// names the label.
func (c *Client) blockedLabelActor(ctx context.Context, repo string, number int) (actor string, found bool, err error) {
	events, err := c.IssueTimelineSince(ctx, repo, number, time.Time{})
	if err != nil {
		return "", false, err
	}
	for i := len(events) - 1; i >= 0; i-- {
		ev := events[i]
		if ev.Event == "labeled" && strings.EqualFold(ev.Label, BlockedLabel) {
			return ev.Actor, true, nil
		}
	}
	return "", false, nil
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
	if err := client.unblockIssue(ctx, t.repo, t.number); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	client.postUnblockedNote(ctx, t.repo, t.number, blockedDeadlineNote(deadline), logger)
	res.Unblocked++
	logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "recheck_after", deadline)
}

// postUnblockedNote leaves note on the issue once per cause: a comment
// already carrying this exact note — the sweep saw the issue again before
// the label removal was visible, or a second sweep path lifted the same
// block (hivecommons/hive#15536 ×2 within a minute) — means nothing is
// posted. An issue the hive is closing for the same merge (a close-on-merge
// or issue-stage closing note is present) gets no unblocked note either:
// it is done, not actionable again. In quiet notification mode the note is
// a section of the issue's status comment, so only the first note on an
// issue ever creates a comment.
func (c *Client) postUnblockedNote(ctx context.Context, repo string, number int, note string, logger *slog.Logger) {
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		logger.Warn("blocked sweep: comment lookup before unblock note failed; posting anyway", "repo", repo, "issue", number, "error", err)
	}
	for _, cm := range comments {
		body := cm.GetBody()
		switch {
		case strings.Contains(body, note):
			c.noteCommentSuppressed()
			return
		case strings.Contains(body, CloseOnMergeMarker), strings.Contains(body, mergedPRClosedStageMarker):
			logger.Info("blocked sweep: issue is being closed by its merged PR; unblock note not posted", "repo", repo, "issue", number)
			c.noteCommentSuppressed()
			return
		}
	}
	if c.notificationsQuiet() {
		if created, err := c.UpsertPRStatusSection(ctx, repo, number, prstatus.KeyUnblocked, note); err != nil {
			logger.Warn("blocked sweep: unblock note not recorded on status comment", "repo", repo, "issue", number, "error", err)
		} else if !created {
			c.noteCommentSuppressed()
		}
		return
	}
	if err := c.CreateIssueCommentBudgeted(ctx, repo, number, note); err != nil {
		logger.Warn("blocked sweep: unblock note not posted", "repo", repo, "issue", number, "error", err)
	}
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
	evidenceNoted bool
}

// blockedSweepComments reads the issue's comments once: the newest
// blocked-on marker (the scanner's blocked note carries one) and, when
// recheckAfter is set, whether the unblocked note for that deadline or a
// soak-evidence comment is already there.
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
		if strings.Contains(body, recheckEvidenceMarkerPrefix) {
			scan.evidenceNoted = true
		}
	}
	return scan, nil
}

// pullRequestSettledDetails reports whether the PR is merged or closed, and
// whether it was merged. A 404 is reported as an error: not found is not settled.
func (c *Client) pullRequestSettledDetails(ctx context.Context, repo string, number int) (bool, bool, error) {
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil {
		return false, false, err
	}
	merged := pr.GetMerged()
	closed := strings.EqualFold(pr.GetState(), "closed")
	return merged || closed, merged, nil
}

// pullRequestSupersededBy checks whether a PR was closed as superseded
// and returns the superseding PR number.
func (c *Client) pullRequestSupersededBy(ctx context.Context, repo string, number int) (int, bool) {
	if c == nil || c.client == nil {
		return 0, false
	}
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil || pr == nil {
		return 0, false
	}
	if n, ok := ParseSupersededBy(pr.GetBody()); ok && n != number {
		return c.followSupersededChain(ctx, repo, n, 1)
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return 0, false
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if n, ok := ParseSupersededBy(comments[i].GetBody()); ok && n != number {
			return c.followSupersededChain(ctx, repo, n, 1)
		}
	}
	return 0, false
}

func (c *Client) followSupersededChain(ctx context.Context, repo string, number, depth int) (int, bool) {
	if depth >= 3 {
		return number, true
	}
	owner, name := c.splitRepo(repo)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, number)
	if err != nil || pr == nil || !strings.EqualFold(pr.GetState(), "closed") {
		return number, true
	}
	if n, ok := ParseSupersededBy(pr.GetBody()); ok && n != number {
		return c.followSupersededChain(ctx, repo, n, depth+1)
	}
	comments, err := c.listIssueComments(ctx, owner, name, number)
	if err != nil {
		return number, true
	}
	for i := len(comments) - 1; i >= 0; i-- {
		if n, ok := ParseSupersededBy(comments[i].GetBody()); ok && n != number {
			return c.followSupersededChain(ctx, repo, n, depth+1)
		}
	}
	return number, true
}

// listOpenBlockedIssues lists open issues with the `blocked` label in owner/repo.
func (c *Client) listOpenBlockedIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, error) {
	opts := &gh.IssueListByRepoOptions{
		State:       "open",
		Labels:      []string{BlockedLabel},
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	var all []*gh.Issue
	for {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, repo, opts)
		if err != nil {
			return all, err
		}
		for _, is := range issues {
			if is != nil && !is.IsPullRequest() {
				all = append(all, is)
			}
		}
		if resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	return all, nil
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

// stripBlockedOnMarkers removes any <!-- hive:blocked-on ... --> marker from text.
func stripBlockedOnMarkers(text string) string {
	return strings.TrimSpace(blockedOnMarkerRE.ReplaceAllString(text, ""))
}

// blockedTargetSettledNote renders the note posted when an issue is requested to
// be blocked on a PR that is already merged or closed. The relation is not written.
func blockedTargetSettledNote(repo string, pr int, merged bool, body string) string {
	state := "closed"
	if merged {
		state = "merged"
	}
	notice := fmt.Sprintf("Target PR %s#%d is already %s — not parking as blocked. Verify this finding on current base branch; close this issue without a PR if it is already fixed, otherwise work it.", repo, pr, state)
	b := strings.TrimSpace(defangMentions(stripBlockedOnMarkers(body)))
	if b != "" {
		return notice + "\n\n" + b
	}
	return notice
}

// blockedTargetNotFoundNote renders the note posted when the target PR was not found.
func blockedTargetNotFoundNote(repo string, pr int, body string) string {
	notice := fmt.Sprintf("Target PR %s#%d was not found — not parking as blocked.", repo, pr)
	b := strings.TrimSpace(defangMentions(stripBlockedOnMarkers(body)))
	if b != "" {
		return notice + "\n\n" + b
	}
	return notice
}
