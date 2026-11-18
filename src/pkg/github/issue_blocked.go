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
// tracked PR merges or closes. The issue form, `<!-- hive:blocked-on
// issue=owner/repo#N -->`, names an issue the block waits on instead (a
// contributor's blocked verdict on another repository's work); the sweep
// lifts that label once the issue closes and never as marker-less. A blocker
// issue closed as a duplicate of, or replaced by, an issue that is still
// open moves the block to that issue instead (blocked_successor.go).
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
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/gfi"
	"github.com/hivecommons/hive/pkg/writeback"
)

// BlockedLabel is the workflow label that parks an issue on an unmerged PR
// (or any outside blocker). It is reserved for agents (issue_request_label.go)
// — the hive applies it from the review backlog and the blocked-on comment
// relay, and a contributor relay applies it on a `blocked` verdict with no
// marker (hivecommons/hive#7924) — and the contributor lane already refuses
// it as a workflow skip.
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
	blockedOnIssueMarkerRE = regexp.MustCompile(`<!--\s*hive:blocked-on\s+issue=([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)\s*-->`)
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

// parseBlockedOnIssueMarker returns the first issue-form blocked-on marker,
// `<!-- hive:blocked-on issue=owner/repo#N -->`, in text.
func parseBlockedOnIssueMarker(text string) (repo string, issue int, ok bool) {
	m := blockedOnIssueMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return "", 0, false
	}
	n, err := strconv.Atoi(m[2])
	if err != nil || n <= 0 {
		return "", 0, false
	}
	return m[1], n, true
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

// blockedOnRef is one blocker a blocked-on marker names: a PR, or an issue
// when issue is set (the `issue=` form).
type blockedOnRef struct {
	repo   string
	number int
	issue  bool
}

func (r blockedOnRef) ref() string { return r.repo + "#" + strconv.Itoa(r.number) }

func (r blockedOnRef) key() string {
	kind := "pr "
	if r.issue {
		kind = "issue "
	}
	return kind + strings.ToLower(r.ref())
}

// marker renders the blocked-on marker that names r.
func (r blockedOnRef) marker() string {
	if r.issue {
		return blockedOnIssueMarker(r.repo, r.number)
	}
	return BlockedOnMarker(r.repo, r.number)
}

// reasonRef is r as the filtered reason writes it after "blocked on ":
// "owner/repo#N", or "issue owner/repo#N" for the issue form.
func (r blockedOnRef) reasonRef() string {
	if r.issue {
		return blockedOnIssueReasonWord + r.ref()
	}
	return r.ref()
}

// parseBlockedOnRefs returns every distinct blocked-on marker in text, both
// forms, in the order they appear. ParseBlockedOnMarker and
// parseBlockedOnIssueMarker read only the first; one comment (or body) may
// name several blockers (projectbluefin/testsuite#1023 named two issues), and
// the block lasts until every one of them is settled, as blockedPRMarkedTargets
// does for a PR's markers.
func parseBlockedOnRefs(text string) []blockedOnRef {
	type hit struct {
		at  int
		ref blockedOnRef
	}
	var hits []hit
	for _, form := range []struct {
		re    *regexp.Regexp
		issue bool
	}{{blockedOnMarkerRE, false}, {blockedOnIssueMarkerRE, true}} {
		for _, m := range form.re.FindAllStringSubmatchIndex(text, -1) {
			n, err := strconv.Atoi(text[m[4]:m[5]])
			if err != nil || n <= 0 {
				continue
			}
			hits = append(hits, hit{at: m[0], ref: blockedOnRef{repo: text[m[2]:m[3]], number: n, issue: form.issue}})
		}
	}
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	seen := map[string]bool{}
	var out []blockedOnRef
	for _, h := range hits {
		if !seen[h.ref.key()] {
			seen[h.ref.key()] = true
			out = append(out, h.ref)
		}
	}
	return out
}

// blockedOnRefsIfSeveral is parseBlockedOnRefs(text) when it names more than
// one blocker, else nil: a single blocker keeps the one-target path.
func blockedOnRefsIfSeveral(text string) []blockedOnRef {
	if refs := parseBlockedOnRefs(text); len(refs) > 1 {
		return refs
	}
	return nil
}

// blockedOnRefsFromReasons reads RepoWorkIssue.BlockedOn back into refs.
func blockedOnRefsFromReasons(reasons []string) []blockedOnRef {
	var out []blockedOnRef
	for _, s := range reasons {
		if repo, n, issue, ok := parseBlockedOnReason(blockedFilterReasonPrefix + " on " + s); ok {
			out = append(out, blockedOnRef{repo: repo, number: n, issue: issue})
		}
	}
	return out
}

// BlockedKindOwnerAction is the kind of a blocked-on marker whose blocker is
// a person's action outside this issue's own work (a permission to grant, an
// account to configure, a decision in another repository). No PR or issue
// closes it, so the sweep keeps the label until a human lifts it.
const BlockedKindOwnerAction = "owner-action"

// blockedOwnerReasonMaxRunes bounds the reason a marker carries; the reason
// is shown on the row and must stay one short line.
const blockedOwnerReasonMaxRunes = 200

// blockedOwnerReasonPrefix opens the enumeration's filtered reason for a
// block that waits on a person: "blocked owner-action: <reason>". It starts
// with the blocked prefix (IsBlockedIssueReason) but never with "blocked on
// ", so parseBlockedOnReason does not read it as a PR or issue.
const blockedOwnerReasonPrefix = blockedFilterReasonPrefix + " " + BlockedKindOwnerAction + ": "

// blockedOwnerMarkerRE matches `<!-- hive:blocked-on kind=owner-action
// reason="..." -->`; the reason attribute is optional.
var blockedOwnerMarkerRE = regexp.MustCompile(`<!--\s*hive:blocked-on\s+kind=` + BlockedKindOwnerAction + `(?:\s+reason="([^"]*)")?\s*-->`)

// blockedOwnerReasonStripRE removes what could close the marker or break its
// quoting from a reason.
var blockedOwnerReasonStripRE = regexp.MustCompile(`-->|[<>"\x00-\x1f]`)

// cleanBlockedOwnerReason folds a free-text reason into one short line that
// is safe inside the marker's quoted attribute.
func cleanBlockedOwnerReason(reason string) string {
	r := strings.Join(strings.Fields(blockedOwnerReasonStripRE.ReplaceAllString(reason, " ")), " ")
	if rs := []rune(r); len(rs) > blockedOwnerReasonMaxRunes {
		r = strings.TrimSpace(string(rs[:blockedOwnerReasonMaxRunes]))
	}
	return r
}

// BlockedOwnerMarker renders the marker that says a `blocked` issue waits on
// a person's action, with the reason in one line.
func BlockedOwnerMarker(reason string) string {
	m := "<!-- hive:blocked-on kind=" + BlockedKindOwnerAction
	if r := cleanBlockedOwnerReason(reason); r != "" {
		m += ` reason="` + r + `"`
	}
	return m + " -->"
}

// ParseBlockedOwnerMarker returns the first owner-action marker in text and
// its reason (empty when the marker carries none).
func ParseBlockedOwnerMarker(text string) (reason string, ok bool) {
	m := blockedOwnerMarkerRE.FindStringSubmatch(text)
	if m == nil {
		return "", false
	}
	return cleanBlockedOwnerReason(m[1]), true
}

// ParseBlockedOwnerReason reads a filtered reason written for an
// owner-action block ("blocked owner-action: <reason>") back into the
// reason. A bare "blocked owner-action" has an empty reason.
func ParseBlockedOwnerReason(reason string) (string, bool) {
	r := strings.TrimSpace(reason)
	if r == strings.TrimSpace(blockedOwnerReasonPrefix[:len(blockedOwnerReasonPrefix)-1]) {
		return "", true
	}
	rest, ok := strings.CutPrefix(r, blockedOwnerReasonPrefix)
	return strings.TrimSpace(rest), ok
}

// blockedOwnerFilterReason is the filtered reason for an owner-action block.
func blockedOwnerFilterReason(reason string) string {
	return strings.TrimSpace(blockedOwnerReasonPrefix + cleanBlockedOwnerReason(reason))
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

// ParseBlockedOnIssueReason returns the issue a blockedFilterReason names
// ("blocked on issue owner/repo#N"); false for the PR form and every other
// reason. It shares parseBlockedOnReason with the sweep so the two never
// drift.
func ParseBlockedOnIssueReason(reason string) (repo string, issue int, ok bool) {
	repo, issue, isIssue, ok := parseBlockedOnReason(reason)
	if !ok || !isIssue {
		return "", 0, false
	}
	return repo, issue, true
}

// BlockedOnIssueTarget is BlockedOnTarget for the issue variant: the
// `issue=` marker in body, else the "blocked on issue owner/repo#N" reason.
func BlockedOnIssueTarget(body, reason string) (repo string, issue int, ok bool) {
	if repo, issue, ok = parseBlockedOnIssueMarker(body); ok {
		return repo, issue, true
	}
	return ParseBlockedOnIssueReason(reason)
}

// blockedFilterReasonPrefix opens the enumeration's filtered reason for a
// blocked issue; IssueStageFor and the sweep recognise it by prefix.
const blockedFilterReasonPrefix = "blocked"

// blockedOnIssueReasonWord sets an issue blocker apart from a PR one in the
// filtered reason: "blocked on issue owner/repo#N".
const blockedOnIssueReasonWord = "issue "

// blockedFilterReason is the work-detail reason for a blocked issue:
// "blocked on owner/repo#N" when the body carries the marker, "blocked on
// issue owner/repo#N" when it carries the issue form, "blocked until
// <RFC3339>" when it carries a recheck-after deadline instead, else the bare
// label. It is what IssueStageFor echoes as the stage reason.
func blockedFilterReason(body string) string {
	if repo, pr, ok := ParseBlockedOnMarker(body); ok {
		return blockedFilterReasonPrefix + " on " + repo + "#" + strconv.Itoa(pr)
	}
	if repo, issue, ok := parseBlockedOnIssueMarker(body); ok {
		return blockedFilterReasonPrefix + " on " + blockedOnIssueReasonWord + repo + "#" + strconv.Itoa(issue)
	}
	if reason, ok := ParseBlockedOwnerMarker(body); ok {
		return blockedOwnerFilterReason(reason)
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

// blockedReviewBacklogFiling reports whether body is a review-backlog item's
// (reviewBacklogFooter ends every one): the only hive path whose marker-less
// `blocked` liftBlockedMarkerless may lift (hivecommons/hive#13364).
func blockedReviewBacklogFiling(body string) bool {
	return strings.Contains(body, reviewBacklogFooter)
}

// setBlockedRowFacts records on a blocked work row what the sweep reads from
// the issue body and GitHub's open "blocked by" count from the same payload
// (#9839), so ReconcileBlockedIssues needs no re-read of either.
func setBlockedRowFacts(row *RepoWorkIssue, body string, openBlockedBy int) {
	row.RecheckAfter, row.OpsTracking = blockedRecheck(body)
	row.RecheckDone = blockedRecheckDone(body)
	row.ReviewBacklog = blockedReviewBacklogFiling(body)
	row.OpenBlockedBy = openBlockedBy
	row.BlockedOn = nil
	for _, r := range blockedOnRefsIfSeveral(body) {
		row.BlockedOn = append(row.BlockedOn, r.reasonRef())
	}
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
	// AskedHuman counts marker-less hive-applied blocks turned into a
	// needs-human ask (blockedMarkerlessAsk).
	AskedHuman int
	Errors     int
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
	// reviewBacklog: the body is a review-backlog item's; openBlockedBy:
	// GitHub's open "blocked by" count. Both gate liftBlockedMarkerless.
	reviewBacklog bool
	openBlockedBy int
	// blockers: every blocker the body's markers name, when there are
	// several (blockedOnRefsIfSeveral); nil otherwise.
	blockers []blockedOnRef
}

// ReconcileBlockedIssues walks the cycle's blocked issues (filtered rows
// whose reason is the blocked signal's) and, for each with a blocked-on
// marker in its body or comments, lifts the `blocked` label with a one-line
// note once the tracked PR has merged or closed — the next enumeration then
// re-triages it from scratch (010, or no stage while project.issue_filter
// refuses it). While the PR is open it strips `help wanted`
// (the org workflow adds it on every labelled issue) so the parked issue
// does not advertise itself. A row with no tracked PR whose recheck-after
// deadline has passed loses `blocked` with a one-line note (once per
// deadline) so the next enumeration re-triages it; an ops-tracking row on the
// hive's own project gets soak evidence instead (runRecheckSoak) and keeps
// `blocked`, other ops-tracking rows are left alone. A row whose marker names
// an issue (issue=) instead loses `blocked` once that issue closes and keeps
// it while it is open (reconcileBlockedOnIssue). A review-backlog item
// with no marker, no deadline, no ops-tracking and no open native blocker
// whose `blocked` the hive itself applied is lifted as a defect
// (liftBlockedMarkerless, hivecommons/hive#13364); every other marker-less
// block is a relay's verdict or a person's and stays. Lookup failures are
// counted and skipped.
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
			targets = append(targets, blockedIssueTarget{repo: row.Repo, number: row.Number, labels: row.Labels, reason: row.Reason, recheckAfter: row.RecheckAfter, opsTracking: row.OpsTracking, recheckDone: row.RecheckDone, reviewBacklog: row.ReviewBacklog, openBlockedBy: row.OpenBlockedBy, blockers: blockedOnRefsFromReasons(row.BlockedOn)})
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
		issues, blockedBy, err := c.listOpenBlockedIssues(ctx, owner, repoName)
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
				repo:          repo,
				number:        issue.GetNumber(),
				labels:        labels,
				reason:        blockedFilterReason(body),
				recheckAfter:  at,
				opsTracking:   ops,
				recheckDone:   blockedRecheckDone(body),
				reviewBacklog: blockedReviewBacklogFiling(body),
				openBlockedBy: blockedBy[issue.GetNumber()],
				blockers:      blockedOnRefsIfSeveral(body),
			})
		}
	}
	c.retainBlockedRepairHeld(repos, targets)
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
		// onIssue: the marker names an issue (issue=), not a PR.
		prRepo, prNumber, onIssue, ok := parseBlockedOnReason(t.reason)
		// scanned: the comments were read without error and hold no marker,
		// so a marker-less lift may trust that nothing names a blocker.
		scanned := false
		// blockers: every blocker the newest marker-bearing comment names
		// when it names several; fromComment: a comment carried the marker,
		// so the body's markers no longer count.
		var blockers []blockedOnRef
		fromComment := false
		// owner: the block waits on a person's action (kind=owner-action),
		// from the body's reason or the newest comment marker.
		ownerReason, owner := ParseBlockedOwnerReason(t.reason)
		if lookups < blockedSweepCommentLookups {
			lookups++
			scan, err := c.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
			scanned = err == nil
			if scan.ok {
				prRepo, prNumber, onIssue, ok = scan.prRepo, scan.pr, scan.issue, scan.ok
				noteBlockedOnSeenRef(t.repo, t.number, blockedOnRef{repo: scan.prRepo, number: scan.pr, issue: scan.issue})
				blockers, fromComment = scan.refs, true
				owner = false
			}
			if scan.owner {
				ownerReason, owner = scan.ownerReason, true
			}
			if !ok && !owner && !t.recheckAfter.IsZero() && !now.Before(t.recheckAfter) {
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
		if !ok && owner {
			// A person's action holds this block: no PR or issue closing
			// lifts it, and the marker (not the label's actor, which is the
			// hive bot for a relay too) says it was applied on purpose.
			c.rememberBlockedOwner(t.repo, t.number, ownerReason)
			stripBlockedHelpWanted(ctx, c, t, &res, logger)
			continue
		}
		c.forgetBlockedOwner(t.repo, t.number)
		if !ok {
			// No tracked PR, no deadline, no ops-tracking: a review-backlog
			// item the hive blocked with no marker is a bug (a blocked
			// filing folded into an open twin, hivecommons/hive#13364), so
			// lift it. A maintainer's or a relay's marker-less label
			// (hivecommons/hive#13267) is left alone: nothing to lift, but
			// the issue still must not advertise itself.
			if scanned && t.recheckAfter.IsZero() && !t.opsTracking {
				if liftBlockedMarkerless(ctx, c, t, &res, logger) {
					continue
				}
				if askHumanBlockedMarkerless(ctx, c, t, &res, logger) {
					stripBlockedHelpWanted(ctx, c, t, &res, logger)
					continue
				}
			}
			stripBlockedHelpWanted(ctx, c, t, &res, logger)
			continue
		}
		// A block that names what it waits on is not one the repair converts.
		forgetBlockedRepairHeld(c, t)
		// The body's markers stand when no comment carried one, provided the
		// blocker this pass resolved is among them (a reason the enumeration
		// took from a comment the budget left unread this pass is not).
		if !fromComment && blockedOnRefsInclude(t.blockers, blockedOnRef{repo: prRepo, number: prNumber, issue: onIssue}) {
			blockers = t.blockers
		}
		if len(blockers) > 1 {
			reconcileBlockedOnAll(ctx, c, t, blockers, scanned, &res, logger)
			continue
		}
		if onIssue {
			reconcileBlockedOnIssue(ctx, c, t, prRepo, prNumber, scanned, &res, logger)
			continue
		}
		settled, merged, err := c.pullRequestSettledDetails(ctx, prRepo, prNumber)
		if err != nil {
			if c.blockedPRMarkerNamesIssue(ctx, t, prRepo, prNumber, err, logger) {
				reconcileBlockedOnIssue(ctx, c, t, prRepo, prNumber, scanned, &res, logger)
				continue
			}
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
					note := blockedSupersededSettledNote(prRepo, supersededBy, prNumber, c.unblockAdmission(t.labels))
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
					if scan.ok && (scan.issue || scan.prRepo != prRepo || scan.pr != prNumber) {
						stripBlockedHelpWanted(ctx, c, t, &res, logger)
						continue
					}
				}
				// Stays a standalone comment even under the attention pilot:
				// blockedSweepComments finds the current blocker from the
				// newest comment carrying the marker, and a status-card
				// section keeps the card's original position and would
				// hide this marker from that ordering.
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
			note := blockedPRSettledNote(prRepo+"#"+strconv.Itoa(prNumber), c.unblockAdmission(t.labels))
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
	if err := stripHelpWanted(ctx, client, t.repo, t.number); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: help wanted not stripped", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	res.Stripped++
}

// stripHelpWanted takes `help wanted` off an issue the hive is not asking
// outside help for: one parked `blocked` (by the sweep or a --blocked-on-pr
// comment), or one its filer claimed as its own next step
// (hivecommons/hive#17299). Callers gate the write; this is the one shared
// removal so the writeback ratchet sees a single call site.
func stripHelpWanted(ctx context.Context, client *Client, repo string, number int) error {
	return client.RemoveLabel(ctx, repo, number, helpWantedLabel)
}

// liftBlockedMarkerless lifts `blocked` from a review-backlog item with no
// blocked-on marker, no recheck-after deadline and no ops-tracking marker
// when the label's most recent application was by this hive
// (hivecommons/hive#13364): the review backlog always writes the marker with
// the label, so such a block is a defect and would otherwise never lift.
// Reports whether the label came off.
//
// The actor alone cannot attribute the block: a contributor relay's
// `blocked` verdict is applied with a hub-minted task token, so its labeled
// event names the same App bot, and it carries no marker by design — lifting
// it is a human's (`/hive approve`, hivecommons/hive#7924 and #12376). Lifting
// those put the issue back in the ready band, the next agent hit the same
// blocker and the relay re-applied the label, daily. So any other issue, or
// one GitHub still reports an open "blocked by" dependency on (#9839), is left
// alone before the timeline is read. A human-applied label, an unreadable
// timeline, or a client with no hive identity configured leaves the issue
// alone too, the last with one WARN so the stuck block is at least visible.
func liftBlockedMarkerless(ctx context.Context, client *Client, t blockedIssueTarget, res *BlockedIssueResult, logger *slog.Logger) bool {
	if !t.reviewBacklog || t.openBlockedBy > 0 {
		return false
	}
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
// hive-applied block that named no PR. Like every unblocked note it says
// "re-triaging", never "actionable again": an issue project.issue_filter
// refuses stays out of the agent queue.
const blockedMarkerlessNote = blockedUnblockedMarker + "\nUnblocked: the `blocked` label was applied by the hive without a blocked-on marker, so nothing could lift it; re-triaging."

// askHumanBlockedMarkerless turns a `blocked` the hive applied with nothing
// naming what the issue waits on into a needs-human ask. A contributor
// relay's blocked verdict is applied with a hub-minted task token (so the
// label's actor is the App bot) and carries no marker when the verdict named
// no issue or PR (#15023 records one when it does). Lifting such a block put
// the issue back in the queue and the next relay re-blocked it, daily
// (projectbluefin/utah#771: 19 label events in 11 days); leaving it made the
// issue invisible, "labelled blocked — the label records no reason" in no
// inbox (projectbluefin/testsuite#908, projectbluefin/utah#1002, blocked 30s
// after a footer-only relay comment). needs-human keeps every agent off it
// and puts it in the maintainer inbox with the question. Only a hive-applied
// label with no open native "blocked by" link is converted; a person's
// marker-less block is theirs. Reports whether the issue was converted.
//
// The write-back gate is asked as blocked_repair with the fact established
// here, that the label is the hive's (Item.HiveAppliedBlock). The hive has
// already written to such an issue, so the gate does not hold the repair back
// for the issue predating the hive's write access or lacking the opt-in
// label; while it was asked as gate_comment both did, every sweep, and the
// issue stayed in the invisible state above for days. A pause, the repo mode
// and the kind's minimum level still refuse it.
func askHumanBlockedMarkerless(ctx context.Context, client *Client, t blockedIssueTarget, res *BlockedIssueResult, logger *slog.Logger) bool {
	if t.reviewBacklog || t.openBlockedBy > 0 {
		forgetBlockedRepairHeld(client, t)
		return false
	}
	actor, found, err := client.blockedLabelActor(ctx, t.repo, t.number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: timeline lookup failed; marker-less block left alone", "repo", t.repo, "issue", t.number, "error", err)
		return false
	}
	if !found || (!client.isOwnBotLogin(actor) && !client.IsHiveAuthorLogin(actor)) {
		forgetBlockedRepairHeld(client, t)
		return false
	}
	item, err := client.WritebackItemForNumber(ctx, t.repo, t.number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: item lookup failed; marker-less block left alone", "repo", t.repo, "issue", t.number, "error", err)
		return false
	}
	item.HiveAppliedBlock = true
	gateRepo := client.blockedRepairRepo(t.repo)
	if d := client.WritebackAllowed(gateRepo, item, writeback.KindBlockedRepair, blockedMarkerlessAskNote); !d.Allowed {
		// The burst hold lets a couple of asks per repo through per sweep,
		// and a failed write's retry is a duplicate for same_item_ttl; both
		// clear on a later sweep. Say so rather than fall through silently,
		// but once per issue and reason: the sweep asks again every few
		// minutes, and a hold that lasts (a pause, the repo mode, the level)
		// would repeat the line each time. Repeats go to the debug log.
		level := slog.LevelDebug
		if client.WritebackGate().NoteHeld(gateRepo, t.number, writeback.KindBlockedRepair, d.Reason) {
			level = slog.LevelInfo
		}
		logger.Log(ctx, level, "blocked sweep: needs-human ask held by writeback gate", "repo", t.repo, "issue", t.number, "reason", d.Reason, "detail", d.Detail)
		return false
	}
	forgetBlockedRepairHeld(client, t)
	if !issueHasLabel(t.labels, issueNeedsHumanLabel) {
		if err := client.AddLabels(ctx, t.repo, t.number, []string{issueNeedsHumanLabel}); err != nil { // writeback:gated
			res.Errors++
			logger.Warn("blocked sweep: needs-human not added; marker-less block left alone", "repo", t.repo, "issue", t.number, "error", err)
			return false
		}
	}
	if err := client.unblockIssue(ctx, t.repo, t.number); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
		return false
	}
	client.postUnblockedNote(ctx, t.repo, t.number, blockedMarkerlessAskNote, logger)
	res.AskedHuman++
	logger.Info("blocked sweep: marker-less hive-applied block turned into a needs-human ask", "repo", t.repo, "issue", t.number, "actor", actor)
	return true
}

// blockedMarkerlessAskNote is the note posted when a marker-less block
// becomes a needs-human ask. It says what was missing and the two ways out.
const blockedMarkerlessAskNote = blockedUnblockedMarker + "\n" +
	"Needs a maintainer: a contributor's agent reported this issue blocked but named no issue or pull request it waits on, so the `blocked` label could never be lifted and the issue showed in no inbox. " +
	"Hive replaced `blocked` with `needs-human`. Either name the dependency (a GitHub \"blocked by\" link, or re-add `blocked` with a `<!-- hive:blocked-on pr=owner/repo#N -->` comment; Hive lifts it when that closes) or remove `needs-human` to put the issue back in the queue."

// blockedOnSeen remembers the blocker the sweep read from an issue's newest
// blocked-on comment marker. The enumeration reads only the issue body, so a
// marker the hive or a maintainer left in a comment (projectbluefin/finpilot#624,
// #390) was invisible to the row's reason and the band.
var blockedOnSeen = struct {
	sync.Mutex
	m map[string]string
}{m: map[string]string{}}

func blockedOnSeenKey(repo string, number int) string {
	return strings.ToLower(strings.TrimSpace(repo)) + "#" + strconv.Itoa(number)
}

func noteBlockedOnSeen(repo string, number int, prRepo string, pr int) {
	noteBlockedOnSeenRef(repo, number, blockedOnRef{repo: prRepo, number: pr})
}

// noteBlockedOnSeenRef is noteBlockedOnSeen for either marker form. It keeps
// the issue form's "issue " word, so the reason the enumeration rebuilds from
// it ("blocked on issue owner/repo#N") still names an issue: in the PR form, a
// later pass whose comment budget skipped the issue read the reason back as a
// PR and looked the blocker up with GET /pulls/N, which 404s for an issue.
func noteBlockedOnSeenRef(repo string, number int, ref blockedOnRef) {
	blockedOnSeen.Lock()
	defer blockedOnSeen.Unlock()
	blockedOnSeen.m[blockedOnSeenKey(repo, number)] = ref.reasonRef()
}

func forgetBlockedOnSeen(repo string, number int) {
	blockedOnSeen.Lock()
	defer blockedOnSeen.Unlock()
	delete(blockedOnSeen.m, blockedOnSeenKey(repo, number))
}

// blockedFilterReasonFor is blockedFilterReason with the blocker the sweep
// last read from the issue's comments preferred over the body's marker, the
// same precedence the sweep lifts by.
func blockedFilterReasonFor(repo string, number int, body string) string {
	blockedOnSeen.Lock()
	seen := blockedOnSeen.m[blockedOnSeenKey(repo, number)]
	blockedOnSeen.Unlock()
	if seen != "" {
		return blockedFilterReasonPrefix + " on " + seen
	}
	return blockedFilterReason(body)
}

// blockedOwnerKey keys the owner-action reason cache.
func blockedOwnerKey(repo string, number int) string {
	return strings.ToLower(repo) + "#" + strconv.Itoa(number)
}

// rememberBlockedOwner records, for the next enumeration, the owner-action
// reason the sweep read from a comment marker: the enumeration reads only
// issue bodies, and the relay's marker is a comment.
func (c *Client) rememberBlockedOwner(repo string, number int, reason string) {
	c.blockedOwnerReasons.Store(blockedOwnerKey(repo, number), cleanBlockedOwnerReason(reason))
}

// forgetBlockedOwner drops a remembered reason once the sweep no longer finds
// an owner-action marker.
func (c *Client) forgetBlockedOwner(repo string, number int) {
	c.blockedOwnerReasons.Delete(blockedOwnerKey(repo, number))
}

// blockedReasonFor is blockedFilterReasonFor plus the sweep's remembered
// owner-action reason for a block whose marker is in a comment.
func (c *Client) blockedReasonFor(repo string, number int, body string) string {
	reason := blockedFilterReasonFor(repo, number, body)
	if reason != blockedFilterReasonPrefix {
		return reason
	}
	if v, ok := c.blockedOwnerReasons.Load(blockedOwnerKey(repo, number)); ok {
		if r, isStr := v.(string); isStr {
			return blockedOwnerFilterReason(r)
		}
	}
	return reason
}

// blockedPRSettledNote is the note posted when the PR a blocked issue
// waited on (pr, "owner/repo#N") has merged or closed.
func blockedPRSettledNote(pr, admission string) string {
	if admission != "" {
		return blockedUnblockedMarker + "\nUnblocked: " + pr + " is no longer open. Unblocked — waiting for a maintainer to admit it: " + admission
	}
	return blockedUnblockedMarker + "\nUnblocked: " + pr + " is no longer open; re-triaging."
}

// blockedSupersededSettledNote is the note posted when the PR that superseded
// an issue's blocker is no longer open either.
func blockedSupersededSettledNote(prRepo string, supersededBy, prNumber int, admission string) string {
	lead := blockedUnblockedMarker + fmt.Sprintf("\nUnblocked: %s#%d (superseding #%d) is no longer open.", prRepo, supersededBy, prNumber)
	if admission != "" {
		return lead + " Unblocked — waiting for a maintainer to admit it: " + admission
	}
	return lead + " This issue is actionable again."
}

// unblockAdmission is "" when project.issue_filter admits an issue with
// these labels once `blocked` is gone; otherwise the sentence an unblock note
// ends with instead of "actionable": nothing works an un-admitted issue
// until a maintainer admits it (projectbluefin/website#987 had no
// triage/accepted and was told it was actionable).
func (c *Client) unblockAdmission(labels []string) string {
	after := make([]string, 0, len(labels))
	for _, l := range labels {
		if !strings.EqualFold(strings.TrimSpace(l), BlockedLabel) {
			after = append(after, l)
		}
	}
	filter := c.getIssueFilter()
	if filter.Admits(after) {
		return ""
	}
	var required []string
	if notice, ok := filter.(IssueFilterNoticeConfig); ok {
		required = notice.RequireLabelsForNotice()
	}
	const lead = "`project.issue_filter` admits "
	switch len(required) {
	case 0:
		return lead + "only issues carrying one of its required labels, and a maintainer adding one admits this one."
	case 1:
		return lead + "only issues labelled " + quoteLabels(required) + ", and a maintainer adding it admits this one."
	default:
		return lead + "only issues carrying one of " + quoteLabels(required) + ", and a maintainer adding one admits this one."
	}
}

// unblockNoteRefRE reads the PR an unblock note is about: "Unblocked:
// owner/repo#N …" or "Unblocked: source PR owner/repo#N …".
var unblockNoteRefRE = regexp.MustCompile(`Unblocked: (?:source PR )?([\w.-]+/[\w.-]+)#(\d+)\b`)

// unblockAlreadyNoted reports whether an unblock note about the same PR as
// note is already on the issue, newer than the newest blocked-on marker
// comment: the block on that PR was lifted already, by the other sweep or an
// earlier pass. projectbluefin/website#987 got the review-backlog sweep's
// "merged … actionable now" and, 8s later, the blocked sweep's "no longer
// open; re-triaging" for the same #986. A note naming no PR (deadline,
// marker-less) is never matched here.
func unblockAlreadyNoted(comments []*gh.IssueComment, note string) bool {
	m := unblockNoteRefRE.FindStringSubmatch(note)
	if m == nil {
		return false
	}
	short := m[1]
	if i := strings.LastIndex(short, "/"); i >= 0 {
		short = short[i+1:]
	}
	same := regexp.MustCompile(`(?i)Unblocked: (?:source PR )?(?:[\w.-]+/)?` + regexp.QuoteMeta(short) + `#` + m[2] + `\b`)
	for i := len(comments) - 1; i >= 0; i-- {
		body := comments[i].GetBody()
		if strings.Contains(body, blockedUnblockedMarker) {
			if same.MatchString(body) {
				return true
			}
			continue
		}
		if _, _, ok := ParseBlockedOnMarker(body); ok {
			return false
		}
	}
	return false
}

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

// BlockedLabelAppliedByHive reports whether the most recent application of
// `blocked` to the issue was by this hive. A human-applied label, a missing
// labeled event, or a client with no hive identity reports false.
func (c *Client) BlockedLabelAppliedByHive(ctx context.Context, repo string, number int) (bool, error) {
	actor, found, err := c.blockedLabelActor(ctx, repo, number)
	if err != nil || !found {
		return false, err
	}
	return c.isOwnBotLogin(actor) || c.IsHiveAuthorLogin(actor), nil
}

// parseBlockedOnReason reads "blocked on owner/repo#N" (issue false) or
// "blocked on issue owner/repo#N" (issue true) back into its parts.
func parseBlockedOnReason(reason string) (repo string, number int, issue bool, ok bool) {
	rest, found := strings.CutPrefix(strings.TrimSpace(reason), blockedFilterReasonPrefix+" on ")
	if !found {
		return "", 0, false, false
	}
	rest, issue = strings.CutPrefix(strings.TrimSpace(rest), blockedOnIssueReasonWord)
	repo, num, found := strings.Cut(strings.TrimSpace(rest), "#")
	if !found || repo == "" {
		return "", 0, false, false
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return "", 0, false, false
	}
	return repo, n, issue, true
}

// reconcileBlockedOnIssue settles a block whose marker names an issue
// (`<!-- hive:blocked-on issue=owner/repo#N -->`): once that issue is closed
// the label comes off with one unblocked note; while it is open the block
// stays and only `help wanted` is stripped. A contributor's blocked verdict
// on another repository's work is not a marker-less defect: lifting it sent
// projectbluefin/utah#10 and #855 back to triage, where contributor after
// contributor re-verified "still blocked" and re-applied it. A lookup error
// is counted and leaves the block; a missing issue reads as open, so a
// mistyped reference never lifts it. A transferred issue is not closed
// either: GitHub redirects the old URL and the client follows it, so the
// block follows the issue's state in its new repository.
//
// A closed blocker is first followed to its successor
// (blockedSuccessorRetargetIssue): closed as a duplicate of an open issue,
// or replaced by one a trusted "Superseded by #N" names, it leaves the block
// on with one re-target comment naming the open issue. scanned says t's
// comments were read this pass (see there).
func reconcileBlockedOnIssue(ctx context.Context, c *Client, t blockedIssueTarget, repo string, number int, scanned bool, res *BlockedIssueResult, logger *slog.Logger) {
	ref := repo + "#" + strconv.Itoa(number)
	owner, name := c.splitRepo(repo)
	_, closed, err := c.IssueClosedAt(ctx, owner, name, number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: tracked issue lookup failed", "repo", t.repo, "issue", t.number, "blocker", ref, "error", err)
		return
	}
	if !closed {
		stripBlockedHelpWanted(ctx, c, t, res, logger)
		return
	}
	if blockedSuccessorRetargetIssue(ctx, c, t, repo, number, scanned, res, logger) {
		return
	}
	if err := c.unblockIssue(ctx, t.repo, t.number); err != nil {
		res.Errors++
		logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	blockedSuccessorLookupSettled(blockedSuccessorLookupKey(t.repo, t.number, ref))
	c.postUnblockedNote(ctx, t.repo, t.number, blockedUnblockedMarker+"\nUnblocked: "+ref+" is closed. This issue is actionable again.", logger)
	res.Unblocked++
	logger.Info("blocked sweep: issue unblocked", "repo", t.repo, "issue", t.number, "blocker", ref)
}

// reconcileBlockedOnAll settles a block whose markers name several blockers,
// PRs and issues alike. projectbluefin/testsuite#1023 named two issues in one
// comment; the sweep read only the first and lifted `blocked` ("actionable
// again") the moment it closed, while the second was still open. The block
// now lifts, with one note naming every blocker, only once all of them are
// settled: a PR merged or closed, an issue closed. A closed blocker replaced
// by one still open (a superseding PR, an issue's duplicate or successor)
// holds as that replacement, and the pass that finds it posts one comment
// carrying the marker of every blocker still open, so the newest
// marker-bearing comment keeps naming all of them. While any blocker is open
// only `help wanted` comes off. A lookup error is counted and keeps the block
// for the pass. scanned says t's comments were read this pass (see
// retargetBlockedOnAll).
func reconcileBlockedOnAll(ctx context.Context, c *Client, t blockedIssueTarget, blockers []blockedOnRef, scanned bool, res *BlockedIssueResult, logger *slog.Logger) {
	var open, settled []blockedOnRef
	var moves []string
	for _, b := range blockers {
		cur, holds, move, err := c.blockedOnRefState(ctx, t, b)
		if err != nil {
			res.Errors++
			logger.Warn("blocked sweep: blocker lookup failed; block kept", "repo", t.repo, "issue", t.number, "blocker", b.ref(), "error", err)
			return
		}
		if !holds {
			settled = append(settled, b)
			continue
		}
		if !blockedOnRefsInclude(open, cur) {
			open = append(open, cur)
		}
		if move != "" {
			moves = append(moves, move)
		}
	}
	if len(open) == 0 {
		if err := c.unblockIssue(ctx, t.repo, t.number); err != nil {
			res.Errors++
			logger.Warn("blocked sweep: unblock failed", "repo", t.repo, "issue", t.number, "error", err)
			return
		}
		c.postUnblockedNote(ctx, t.repo, t.number, blockedAllSettledNote(settled, c.unblockAdmission(t.labels)), logger)
		res.Unblocked++
		logger.Info("blocked sweep: issue unblocked; every blocker settled", "repo", t.repo, "issue", t.number, "blockers", blockedOnRefList(settled))
		return
	}
	noteBlockedOnSeenRef(t.repo, t.number, open[0])
	if len(moves) > 0 {
		retargetBlockedOnAll(ctx, c, t, open, moves, scanned, res, logger)
	}
	stripBlockedHelpWanted(ctx, c, t, res, logger)
}

// blockedOnRefState reads one of several blockers: whether it still holds
// the issue, and as what. A closed-unmerged PR superseded by one still open,
// or a closed issue replaced by one still open (blockedIssueSuccessorOf),
// holds as that replacement, and move says so for the re-target note. A
// merged PR, or a closed one or a closed issue with no open replacement, is
// settled. A successor read that keeps failing past
// blockedSuccessorLookupGrace releases the issue blocker, as the
// single-blocker path does.
func (c *Client) blockedOnRefState(ctx context.Context, t blockedIssueTarget, b blockedOnRef) (cur blockedOnRef, holds bool, move string, err error) {
	if b.issue {
		owner, name := c.splitRepo(b.repo)
		_, closed, err := c.IssueClosedAt(ctx, owner, name, b.number)
		if err != nil {
			return b, false, "", err
		}
		if !closed {
			return b, true, "", nil
		}
		key := blockedSuccessorLookupKey(t.repo, t.number, b.ref())
		succ, ok, err := c.blockedIssueSuccessorOf(ctx, t, b.repo, b.number)
		if err != nil {
			if giveUp, _ := blockedSuccessorLookupFailed(key); !giveUp {
				return b, false, "", err
			}
			return b, false, "", nil
		}
		blockedSuccessorLookupSettled(key)
		if !ok {
			return b, false, "", nil
		}
		how := "was replaced by"
		if succ.duplicate {
			how = "is a duplicate of"
		}
		return blockedOnRef{repo: succ.repo, number: succ.number, issue: true}, true, fmt.Sprintf("blocker %s %s %s", b.ref(), how, succ.ref()), nil
	}
	settled, merged, err := c.pullRequestSettledDetails(ctx, b.repo, b.number)
	if err != nil {
		if c.blockedPRMarkerNamesIssue(ctx, t, b.repo, b.number, err, nil) {
			b.issue = true
			return c.blockedOnRefState(ctx, t, b)
		}
		return b, false, "", err
	}
	if !settled {
		return b, true, "", nil
	}
	if merged {
		return b, false, "", nil
	}
	sup, ok := c.pullRequestSupersededBy(ctx, b.repo, b.number)
	if !ok || sup <= 0 || sup == b.number {
		return b, false, "", nil
	}
	supSettled, _, err := c.pullRequestSettledDetails(ctx, b.repo, sup)
	if err != nil {
		return b, false, "", err
	}
	if supSettled {
		return b, false, "", nil
	}
	return blockedOnRef{repo: b.repo, number: sup}, true, fmt.Sprintf("blocker %s was superseded by #%d", b.ref(), sup), nil
}

// retargetBlockedOnAll posts blockedOnAllRetargetNote through the write-back
// gate, as the issue successor re-target does. A held or failed note leaves
// the block as it is: the next pass follows the closed blocker to its open
// replacement again and asks once more. When the comments were not read this
// pass (the per-pass budget ran out) the blockers came from the body, which
// an earlier re-target comment may already have moved off, so the comments
// are read first and a marker there means nothing is posted.
func retargetBlockedOnAll(ctx context.Context, c *Client, t blockedIssueTarget, open []blockedOnRef, moves []string, scanned bool, res *BlockedIssueResult, logger *slog.Logger) {
	if !scanned {
		scan, err := c.blockedSweepComments(ctx, t.repo, t.number, t.recheckAfter)
		if err != nil {
			res.Errors++
			logger.Warn("blocked sweep: comment lookup before re-target failed; block kept", "repo", t.repo, "issue", t.number, "error", err)
			return
		}
		if scan.ok {
			return
		}
	}
	note := blockedOnAllRetargetNote(open, moves)
	item, err := c.WritebackItemForNumber(ctx, t.repo, t.number)
	if err != nil {
		res.Errors++
		logger.Warn("blocked sweep: item lookup before re-target failed; block kept", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	gateRepo := c.blockedRepairRepo(t.repo)
	if d := c.WritebackAllowed(gateRepo, item, writeback.KindGateComment, note); !d.Allowed {
		level := slog.LevelDebug
		if c.WritebackGate().NoteHeld(gateRepo, t.number, writeback.KindGateComment, d.Reason) {
			level = slog.LevelInfo
		}
		logger.Log(ctx, level, "blocked sweep: re-target of one of several blockers held by writeback gate; block kept", "repo", t.repo, "issue", t.number, "reason", d.Reason, "detail", d.Detail)
		return
	}
	c.WritebackGate().ForgetHeld(gateRepo, t.number, writeback.KindGateComment)
	if err := c.CreateIssueCommentBudgeted(ctx, t.repo, t.number, note); err != nil { // writeback:gated
		logger.Warn("blocked sweep: re-target note not posted; block kept", "repo", t.repo, "issue", t.number, "error", err)
		return
	}
	res.Retargeted++
	logger.Info("blocked sweep: one of several blockers re-targeted", "repo", t.repo, "issue", t.number, "blockers", blockedOnRefList(open))
}

// blockedOnAllRetargetNote is the standalone comment that moves one or more
// of several blockers to their open replacements. It carries the marker of
// every blocker still open, the untouched ones included, so the newest
// marker-bearing comment the sweep reads still names all of them.
func blockedOnAllRetargetNote(open []blockedOnRef, moves []string) string {
	var b strings.Builder
	for _, r := range open {
		b.WriteString(r.marker())
		b.WriteString("\n")
	}
	b.WriteString("Re-targeted: " + strings.Join(moves, "; ") + ". Still blocked on " + blockedOnRefList(open) + ".")
	return b.String()
}

// blockedAllSettledNote is the note posted when every one of several
// blockers is settled. It names the first right after "Unblocked: " so
// unblockAlreadyNoted recognises a repeat.
func blockedAllSettledNote(settled []blockedOnRef, admission string) string {
	lead := blockedUnblockedMarker + "\nUnblocked: " + blockedOnRefList(settled) + " are no longer open."
	if admission != "" {
		return lead + " Unblocked — waiting for a maintainer to admit it: " + admission
	}
	return lead + " This issue is actionable again."
}

// blockedOnRefList names refs as prose: "a", "a and b", "a, b and c".
func blockedOnRefList(refs []blockedOnRef) string {
	names := make([]string, 0, len(refs))
	for _, r := range refs {
		names = append(names, r.ref())
	}
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

// blockedOnRefsInclude reports whether refs names r (same form, repository
// compared case-insensitively).
func blockedOnRefsInclude(refs []blockedOnRef, r blockedOnRef) bool {
	for _, x := range refs {
		if x.key() == r.key() {
			return true
		}
	}
	return false
}

// liftBlockedDeadline lifts `blocked` from an issue with no tracked PR whose
// recheck-after deadline has passed and posts the unblocked note, so the next
// enumeration re-triages it — 010, or no stage while project.issue_filter
// refuses it (hivecommons/hive#13310). An
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
	if unblockAlreadyNoted(comments, note) {
		logger.Info("blocked sweep: the block on this PR was already lifted with a note; not posting a second", "repo", repo, "issue", number)
		c.noteCommentSuppressed()
		return
	}
	// The attention pilot folds dependency progress into the issue's
	// status card whatever github.notifications.mode says.
	if c.notificationsQuiet() || c.attentionPilot() {
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
// issue is set when the newest blocked-on marker is the issue form, so
// prRepo and pr then name an issue.
type blockedSweepScan struct {
	prRepo string
	pr     int
	issue  bool
	ok     bool
	// owner is set when the newest marker is an owner-action one; ok stays
	// false then, so no PR or issue path runs, and ownerReason carries why.
	owner         bool
	ownerReason   string
	deadlineNoted bool
	evidenceNoted bool
	// refs: every blocker that newest marker-bearing comment names, when
	// there are several (blockedOnRefsIfSeveral); nil otherwise.
	refs []blockedOnRef
}

// blockedSweepComments reads the issue's comments once: the newest
// blocked-on marker of either form (the scanner's blocked note carries one)
// and, when recheckAfter is set, whether the unblocked note for that
// deadline or a soak-evidence comment is already there.
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
		if !scan.ok && !scan.owner {
			scan.prRepo, scan.pr, scan.ok = ParseBlockedOnMarker(body)
			if !scan.ok {
				scan.prRepo, scan.pr, scan.ok = parseBlockedOnIssueMarker(body)
				scan.issue = scan.ok
			}
			if scan.ok {
				scan.refs = blockedOnRefsIfSeveral(body)
			}
			if !scan.ok {
				scan.ownerReason, scan.owner = ParseBlockedOwnerMarker(body)
			}
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

// blockedPRMarkerMisformed remembers the (blocked issue, blocker) pairs whose
// pr= marker was found to name an issue, so the sweep says so once, not on
// every pass.
var blockedPRMarkerMisformed sync.Map

// blockedPRMarkerNamesIssue reports whether repo#number, named by a pr=
// blocked-on marker whose PR lookup answered 404, is an issue, so the sweep
// reads the blocker as one. The marker is written by hand by people and
// agents alike, and the PR form reads as right as the issue form:
// projectbluefin/testsuite#1259 named issue #909 with pr=, every pass 404'd
// ("tracked PR lookup failed"), and the block could never lift. Only a 404
// on the PR read is checked, with one issue GET (GitHub's issues endpoint
// also answers for PRs, so an issue without a pull_request link is one);
// any other error keeps today's handling.
func (c *Client) blockedPRMarkerNamesIssue(ctx context.Context, t blockedIssueTarget, repo string, number int, err error, logger *slog.Logger) bool {
	if c == nil || c.client == nil || !githubStatusError(err, http.StatusNotFound) {
		return false
	}
	owner, name := c.splitRepo(repo)
	issue, _, ierr := c.client.Issues.Get(ctx, owner, name, number)
	if ierr != nil || issue == nil || issue.IsPullRequest() {
		return false
	}
	key := t.repo + "#" + strconv.Itoa(t.number) + ">" + repo + "#" + strconv.Itoa(number)
	if _, seen := blockedPRMarkerMisformed.LoadOrStore(key, true); !seen && logger != nil {
		logger.Info("blocked sweep: a pr= blocked-on marker names an issue; reading it as one (write issue= instead)", "repo", t.repo, "issue", t.number, "blocker", repo+"#"+strconv.Itoa(number))
	}
	return true
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

// listOpenBlockedIssues lists open issues with the `blocked` label in
// owner/repo and, keyed by number, the open "blocked by" count each payload's
// issue_dependencies_summary reports (go-github drops it; see
// listOpenIssuesWithBlockedCounts).
func (c *Client) listOpenBlockedIssues(ctx context.Context, owner, repo string) ([]*gh.Issue, map[int]int, error) {
	var all []*gh.Issue
	blockedBy := map[int]int{}
	page := 1
	for {
		q := url.Values{}
		q.Set("state", "open")
		q.Set("labels", BlockedLabel)
		q.Set("per_page", "100")
		q.Set("page", strconv.Itoa(page))
		req, err := c.client.NewRequest(http.MethodGet, fmt.Sprintf("repos/%s/%s/issues?%s", owner, repo, q.Encode()), nil)
		if err != nil {
			return all, blockedBy, err
		}
		var issues []*issueWithDependencies
		resp, err := c.client.Do(ctx, req, &issues)
		if err != nil {
			return all, blockedBy, err
		}
		for _, is := range issues {
			if is == nil || is.IsPullRequest() {
				continue
			}
			issue := is.Issue
			all = append(all, &issue)
			if is.DependenciesSummary != nil && is.DependenciesSummary.BlockedBy > 0 {
				blockedBy[issue.GetNumber()] = is.DependenciesSummary.BlockedBy
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		page = resp.NextPage
	}
	return all, blockedBy, nil
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
