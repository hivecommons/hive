package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
	"github.com/hivecommons/hive/pkg/writeback"
)

// The reporter-trust hold (hivecommons/hive#9665) is the merge-side half of
// project.issue_filter.reporter_trust. The admission half keeps a stranger's
// issue out of the queue until a maintainer triages it; this half makes sure
// an unaccepted stranger's request never merges without a human, at any ACMM
// level. A PR whose rationale traces to an unaccepted issue filed by a reporter
// the operator does not trust receives literal `hold` plus `needs-human` and a
// notice, and only a person removes it — the App never auto-releases a
// reporter-trust hold it placed on somebody else's PR. An issue's configured
// acceptance label already supplies that sign-off.
//
// The gate judges the PR's linked issues only (closing keywords and the
// request's declared issues), never a pull request a number happens to
// resolve to, and never a PR the hive itself authored (hivecommons/hive#15020:
// the hive held its own approved fix because "closed by #15017" named a
// sibling PR and that PR's author was read as an untrusted reporter). A
// hive-set reporter-trust hold left on a hive-authored PR by an older build
// is released by the stage-label pass (automerge.releaseSelfImposedReporterTrustHold).
//
// It composes with, and never replaces, the #5117 self-authorization gate:
// that one asks "did a person ask for this at all"; this one asks "was that
// person someone we trust". Bot- and hive-filed issues are #5117's business
// and are skipped here.

// ReporterTrust is the gate's finding for one PR request.
type ReporterTrust struct {
	// OwnsNeedsHuman proves this gate, not an earlier escalation, added the label.
	OwnsNeedsHuman bool
	// Held is true when the PR must carry "hold" regardless of ACMM level.
	Held bool
	// Issue is the untrusted-reporter issue that decided it. Zero when Held is false.
	Issue int
	// Repo is that issue's repository ("owner/repo").
	Repo string
	// Reporter is the issue author's login.
	Reporter string
	// Association is GitHub's author_association for that reporter.
	Association string
	// Reason is a short human-readable explanation for the audit entry and log.
	Reason string
}

// ReporterTrustNoticeMarker marks the notice the #9665 gate posts, so the
// level-hold release path can recognise a reporter-trust hold and leave it to
// a human, and so operators can search for it.
const ReporterTrustNoticeMarker = "<!-- hive:reporter-trust-hold:9665 -->"

// SetReporterTrustHoldEnabled installs the live per-repo switch for the #9665
// hold. nil means off: the gate is opt-in, so an unwired client never holds.
func (c *Client) SetReporterTrustHoldEnabled(fn func(repo string) bool) {
	if c == nil {
		return
	}
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.reporterTrustHoldEnabled = fn
}

// SetReporterTrusted installs the live reporter-trust predicate — the same
// one project.issue_filter.reporter_trust uses for admission, so the two
// halves of the gate can never disagree about who is trusted. nil trusts
// nobody, which with the hold switched on holds every human-filed rationale;
// the boot wiring always supplies one.
func (c *Client) SetReporterTrusted(fn func(login, association string) bool) {
	if c == nil {
		return
	}
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.reporterTrusted = fn
}

// SetReporterTrustUpstream installs the live upstream_watch resolver. An empty
// result disables upstream acceptance for this repo. Only a verified merged PR
// in this upstream supplies acceptance; prose and port markers are not proof.
func (c *Client) SetReporterTrustUpstream(fn func(context.Context, string) (string, error)) {
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.reporterTrustUpstream = fn
}

func (c *Client) reporterMergedUpstreamAccepted(ctx context.Context, repo, source string, issue *gh.Issue) bool {
	if !issue.IsPullRequest() {
		return false
	}
	c.reporterTrustMu.RLock()
	resolve := c.reporterTrustUpstream
	c.reporterTrustMu.RUnlock()
	if resolve == nil {
		return false
	}
	upstream, err := resolve(ctx, repo)
	if err != nil || upstream == "" || !strings.EqualFold(upstream, source) {
		return false
	}
	owner, name := splitRepoRef(source, c.org)
	pr, _, err := c.client.PullRequests.Get(ctx, owner, name, issue.GetNumber())
	return err == nil && pr.GetMerged() && !pr.GetMergedAt().IsZero()
}

func (c *Client) reporterTrustHoldActive(repo string) bool {
	if c == nil {
		return false
	}
	c.reporterTrustMu.RLock()
	fn := c.reporterTrustHoldEnabled
	c.reporterTrustMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(repo)
}

func (c *Client) reporterIsTrusted(login, association string) bool {
	if c == nil {
		return false
	}
	c.reporterTrustMu.RLock()
	fn := c.reporterTrusted
	c.reporterTrustMu.RUnlock()
	if fn == nil {
		return false
	}
	return fn(login, association)
}

// hiveSelfReportMarkerPrefix opens the marker fleetreport writes into every
// issue a hive files about itself (fleetreport.SelfReportMarkerPrefix; not
// imported to keep this package free of that dependency). A self-report's
// reporter is the hive, whatever account posted it.
const hiveSelfReportMarkerPrefix = "<!-- hive:selfreport "

// IsHiveSelfReport reports whether an issue body carries the hive's
// self-report marker.
func IsHiveSelfReport(body string) bool {
	return strings.Contains(body, hiveSelfReportMarkerPrefix)
}

// reporterTrustRationale collects the issues whose REPORTER this gate judges:
// what the PR declares it closes (Fixes/Closes/Resolves keywords) and the
// request's own declared issue list, plus explicit foreign-source references
// for ports. Local bare mentions are deliberately left out:
// "Refs #15009 (closed by #15017)" cites a sibling PR, and a reporter
// found that way is somebody else's PR author, not the person who asked for
// this change (hivecommons/hive#15020 held for the author of a sibling PR).
func reporterTrustRationale(repo, title, body string, declared []int) []ClaimedRef {
	refs := ParseClaimedIssues(title+"\n"+body, repo)
	seen := make(map[string]bool, len(refs))
	for _, ref := range refs {
		seen[fmt.Sprintf("%s#%d", ref.Repo, ref.Issue)] = true
	}
	for _, ref := range ParseReferencedIssues(title+"\n"+body, repo) {
		key := fmt.Sprintf("%s#%d", ref.Repo, ref.Issue)
		if strings.EqualFold(ref.Repo, repo) || seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ref)
	}
	for _, n := range declared {
		if n <= 0 {
			continue
		}
		key := fmt.Sprintf("%s#%d", repo, n)
		if seen[key] {
			continue
		}
		seen[key] = true
		refs = append(refs, ClaimedRef{Repo: repo, Issue: n})
	}
	return refs
}

// hiveAuthoredLogin reports whether login is one of this hive's own accounts
// (the App bot, the configured hive identity, or any "[bot]" account).
func (c *Client) hiveAuthoredLogin(login string) bool {
	login = strings.TrimSpace(login)
	if login == "" {
		return false
	}
	return !c.isHumanAuthor(&gh.User{Login: gh.Ptr(login)})
}

// EvaluateReporterTrust decides whether a PR's rationale traces to an issue
// filed by a reporter the operator does not trust. author is the PR's own
// author login.
//
// A PR the hive authored is never held here: the hive is the author, and
// whether the hive works an issue at all is the admission half's call
// (project.issue_filter.reporter_trust). Holding the hive's own green fix
// for the trust level of whoever reported the bug parks it on a person for
// nothing (hivecommons/hive#15020). The gate stays for PRs other accounts
// opened, where "whose request is this" is a live question.
//
// Any one unaccepted untrusted-reporter citation holds: unlike #5117, where one human
// rationale is enough to show a person asked, here the question is whether a
// stranger's request is about to merge unattended, and a PR that also cites a
// maintainer's issue is still delivering the stranger's. Holding is the safe
// direction; a maintainer who disagrees removes the label.
//
// Local pull requests decide nothing (their authors are not reporters).
// Foreign port sources require trust or verified upstream acceptance. Issues the hive or
// a bot filed, and hive self-reports, are skipped — #5117 owns those. An
// issue that cannot be read decides nothing, for the reason
// pr_self_authorization.go gives: a hold nobody can explain is a hold people
// learn to strip.
func (c *Client) EvaluateReporterTrust(ctx context.Context, repo, title, body string, declared []int, author string) ReporterTrust {
	if c == nil || c.client == nil {
		return ReporterTrust{}
	}
	if c.hiveAuthoredLogin(author) {
		c.logger.Debug("reporter-trust gate: PR is hive-authored, the issue reporter's trust does not gate the hive's own fix",
			"repo", repo, "author", author)
		return ReporterTrust{}
	}
	refs := reporterTrustRationale(repo, title, body, declared)
	if len(refs) == 0 {
		return ReporterTrust{}
	}
	for _, ref := range refs {
		owner, name := splitRepoRef(ref.Repo, c.org)
		if owner == "" || name == "" {
			continue
		}
		issue, _, err := c.client.Issues.Get(ctx, owner, name, ref.Issue)
		if err != nil {
			c.logger.Warn("reporter-trust gate: could not read a cited issue, it decides nothing",
				"repo", ref.Repo, "issue", ref.Issue, "error", err.Error())
			continue
		}
		localOwner, localName := splitRepoRef(repo, c.org)
		local := strings.EqualFold(owner+"/"+name, localOwner+"/"+localName)
		if local && issue.IsPullRequest() {
			c.logger.Debug("reporter-trust gate: cited number is a pull request, not an issue; it decides nothing",
				"repo", ref.Repo, "number", ref.Issue)
			continue
		}
		if !c.isHumanAuthor(issue.GetUser()) || IsHiveSelfReport(issue.GetBody()) {
			// Hive- or bot-filed, a hive self-report, or no author to judge:
			// #5117's business.
			continue
		}
		login := strings.TrimSpace(issue.GetUser().GetLogin())
		association := strings.TrimSpace(issue.GetAuthorAssociation())
		trustedAssociation := association
		if !local {
			// GitHub associations describe the cited repository, not this hive.
			trustedAssociation = ""
		}
		if c.reporterIsTrusted(login, trustedAssociation) ||
			(local && c.reporterIssueAccepted(issue)) ||
			(!local && c.reporterMergedUpstreamAccepted(ctx, repo, owner+"/"+name, issue)) {
			continue
		}
		return ReporterTrust{
			Held:        true,
			Issue:       ref.Issue,
			Repo:        ref.Repo,
			Reporter:    login,
			Association: association,
			Reason: fmt.Sprintf("issue %s#%d was filed by %s (%s), who is not a trusted reporter for this hive",
				ref.Repo, ref.Issue, login, associationOrUnknown(association)),
		}
	}
	return ReporterTrust{}
}

// reporterIssueAccepted honours the same acceptance labels as issue admission,
// even when the PR hold is explicitly enabled independently of admission.
func (c *Client) reporterIssueAccepted(issue *gh.Issue) bool {
	ra, _ := c.getIssueFilter().(ReporterAdmitter)
	for _, required := range reporterTrustRequiredLabelsForNotice(ra) {
		for _, label := range issue.Labels {
			if strings.EqualFold(required, label.GetName()) {
				return true
			}
		}
	}
	return false
}

func associationOrUnknown(association string) string {
	if strings.TrimSpace(association) == "" {
		return "association unknown"
	}
	return association
}

// NeedsHumanReason is the one-line reason a reporter-trust hold raises
// needs-human with (hivecommons/hive#10773). It names the issue and the
// untrusted reporter, not the PR author: the PR itself was opened by the hive.
func (r ReporterTrust) NeedsHumanReason() string {
	if !r.Held {
		return ""
	}
	return fmt.Sprintf("reporter-trust hold — issue #%d filed by @%s", r.Issue, r.Reporter)
}

// reporterTrustHoldLabels is the label set a reporter-trust hold applies.
// "hold" is the enforcement; needs-human is the signal that puts the PR in
// front of a maintainer (hivecommons/hive#10773) — without it a green, held
// PR sits silently because nothing tells anyone it is waiting on them.
func reporterTrustHoldLabels() []string {
	return []string{"hold", issueNeedsHumanLabel}
}

// reporterTrustNotice is the comment left on a held PR. The person clearing
// the hold needs to know it is about who asked, not about the code — and that
// the outsider is the issue's reporter, not this PR's author.
func reporterTrustNotice(finding ReporterTrust) string {
	return fmt.Sprintf(`%s
> [!IMPORTANT]
> **Held for maintainer sign-off: the source issue reporter is not trusted.** (%s)
>
> The untrusted **reporter**, not this PR's author, is @%s. The PR's rationale traces to %s#%d, filed by @%s (GitHub association: %s). The hold concerns that issue reporter, not the PR author. This hive holds unaccepted work requested by untrusted issue reporters so that it never merges without a maintainer looking at it, whatever the hive's autonomy level (hivecommons/hive#9665).
>
> Nothing here is a review of the change. To release the hold, a maintainer removes the `+"`hold`"+` label; Hive will not remove it on its own. The `+"`%s`"+` label marks it as waiting on a person. To trust this reporter in future, add them under **Settings → Labels → Reporter trust**.`,
		reporterTrustMarker(finding), finding.NeedsHumanReason(), finding.Reporter, finding.Repo, finding.Issue, finding.Reporter, associationOrUnknown(finding.Association), issueNeedsHumanLabel)
}

// ReporterTrustNoticeBody renders the notice for a finding (exported for the
// stage-label pass's tests, which seed a PR thread with one).
func ReporterTrustNoticeBody(finding ReporterTrust) string { return reporterTrustNotice(finding) }

const reporterTrustMetadataPrefix = "<!-- hive:reporter-trust-metadata "

func reporterTrustMarker(finding ReporterTrust) string {
	data, _ := json.Marshal(finding)
	return ReporterTrustNoticeMarker + "\n" + reporterTrustMetadataPrefix + string(data) + " -->"
}

func reporterTrustFinding(body string) (ReporterTrust, bool) {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, reporterTrustMetadataPrefix) && strings.HasSuffix(line, " -->") {
			var finding ReporterTrust
			err := json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(line, reporterTrustMetadataPrefix), " -->")), &finding)
			return finding, err == nil && finding.Held && finding.Issue > 0 && finding.Reporter != ""
		}
	}
	// The original notice is durable evidence even if the reporter has since
	// become trusted or the source issue has been triaged. Neither releases hold.
	if match := legacyReporterTrustFinding.FindStringSubmatch(body); len(match) == 5 {
		number, err := strconv.Atoi(match[2])
		return ReporterTrust{Held: true, Repo: match[1], Issue: number, Reporter: match[3], Association: match[4]}, err == nil && number > 0
	}
	return ReporterTrust{}, false
}

var legacyReporterTrustFinding = regexp.MustCompile(`traces to ([^\s]+)#([0-9]+), filed by @([^\s]+) \(GitHub association: ([^)]+)\)`)

func (f ReporterTrust) escalationReason() string {
	return fmt.Sprintf("reporter-trust hold — issue #%d filed by @%s (%s)", f.Issue, f.Reporter, f.Repo)
}

// SetReporterTrustEscalation installs the durable policy-reason ledger. Its
// return value indicates an independent needs-human reason is still active.
func (c *Client) SetReporterTrustEscalation(fn func(string, int, string) bool) {
	c.reporterTrustMu.Lock()
	defer c.reporterTrustMu.Unlock()
	c.reporterTrustEscalation = fn
}

func (c *Client) recordReporterTrustEscalation(repo string, number int, reason string) bool {
	c.reporterTrustMu.RLock()
	fn := c.reporterTrustEscalation
	c.reporterTrustMu.RUnlock()
	if fn == nil {
		// Without a ledger, releasing a label could erase another reason.
		return true
	}
	owner, name := c.splitRepo(repo)
	return fn(owner+"/"+name, number, reason)
}

// reconcileReporterTrustSignal repairs old holds, persists their visible
// human-only reason, and releases only this gate's ledger entry. The existing
// label-event release path removes needs-human after a human removes hold.
func (c *Client) reconcileReporterTrustSignal(ctx context.Context, repo string, pr *gh.PullRequest, labels []string) ([]string, string) {
	if !hasExactLabel(labels, "hold") && !hasExactLabel(labels, issueNeedsHumanLabel) {
		c.recordReporterTrustEscalation(repo, pr.GetNumber(), "")
		return labels, ""
	}
	if strings.TrimSpace(c.appBotLogin) == "" {
		return labels, ""
	}
	owner, name := c.splitRepo(repo)
	comments, err := c.listIssueComments(ctx, owner, name, pr.GetNumber())
	if err != nil {
		return labels, ""
	}
	for i := len(comments) - 1; i >= 0; i-- {
		comment := comments[i]
		if !c.isTrustedAppBotCommentAuthor(comment) || !IsReporterTrustHoldNotice(comment.GetBody()) {
			continue
		}
		finding, ok := reporterTrustFinding(comment.GetBody())
		if !ok {
			return labels, ""
		}
		if hasExactLabel(labels, "hold") {
			reason := finding.escalationReason()
			c.recordReporterTrustEscalation(repo, pr.GetNumber(), reason)
			if !hasExactLabel(labels, issueNeedsHumanLabel) {
				// Write-back gate: the hold may sit on a PR the hive did not
				// open; the escalation is recorded internally regardless.
				if d := c.WritebackAllowed(repo, c.sentinelWritebackItem(pr), writeback.KindReporterTrustHold, reporterTrustNotice(finding)); !d.Allowed {
					return labels, reason
				}
				if !finding.OwnsNeedsHuman {
					finding.OwnsNeedsHuman = true
					// Persist ownership when upgrading a legacy invisible hold.
					if err := c.CreateIssueComment(ctx, repo, pr.GetNumber(), reporterTrustNotice(finding)); err != nil { // writeback:gated
						return labels, reason
					}
				}
				if err := c.AddLabels(ctx, repo, pr.GetNumber(), []string{issueNeedsHumanLabel}); err == nil { // writeback:gated
					labels = append(labels, issueNeedsHumanLabel)
				}
			}
			return labels, reason
		}
		c.recordReporterTrustEscalation(repo, pr.GetNumber(), "")
		return labels, ""
	}
	return labels, ""
}

// IsReporterTrustHoldNotice reports whether a PR comment is the #9665 notice.
func IsReporterTrustHoldNotice(body string) bool {
	return strings.Contains(body, ReporterTrustNoticeMarker)
}

// ReporterTrustHoldNotice is the finding behind the newest App-authored
// #9665 notice on a PR, with when it was posted: the provenance the stage
// tracker and the self-imposed-hold release read.
type ReporterTrustHoldNotice struct {
	Finding  ReporterTrust
	PostedAt time.Time
}

// ReporterTrustNoticeFromComments returns the newest reporter-trust notice
// the App posted on a PR. An empty appBotLogin accepts any author, as the
// display helpers do; ok is false when no notice carries a readable finding.
func ReporterTrustNoticeFromComments(comments []*gh.IssueComment, appBotLogin string) (notice ReporterTrustHoldNotice, ok bool) {
	for _, comment := range comments {
		if comment == nil || !IsReporterTrustHoldNotice(comment.GetBody()) {
			continue
		}
		if strings.TrimSpace(appBotLogin) != "" && !strings.EqualFold(safeGetLogin(comment.GetUser()), appBotLogin) {
			continue
		}
		finding, found := reporterTrustFinding(comment.GetBody())
		if !found {
			continue
		}
		at := comment.GetCreatedAt().Time
		if !ok || at.After(notice.PostedAt) {
			notice, ok = ReporterTrustHoldNotice{Finding: finding, PostedAt: at}, true
		}
	}
	return notice, ok
}

// HoldSummary is the plain-words reason for the stage tracker's hold pill:
// "reporter trust (issue #15017 by @Karibhou, contributor)".
func (r ReporterTrust) HoldSummary() string {
	if !r.Held {
		return ""
	}
	association := strings.TrimSpace(r.Association)
	if association == "" {
		association = "association unknown"
	}
	return stagelabels.ReporterTrustReason(r.Issue, r.Reporter, association)
}

// HoldProvenance is the stage-tracker record of this hive-set hold: who set
// it ("hive"), the gate, the plain-words reason and the issue it judged.
// since is the label event time (or the notice time when no event is known).
func (r ReporterTrust) HoldProvenance(since time.Time) *stagelabels.HoldProvenance {
	if !r.Held {
		return nil
	}
	return &stagelabels.HoldProvenance{
		By:          stagelabels.HoldByHive,
		Source:      stagelabels.HoldSourceReporterTrust,
		Reason:      r.HoldSummary(),
		Since:       since,
		Issue:       r.Issue,
		Reporter:    r.Reporter,
		Association: strings.ToUpper(strings.TrimSpace(r.Association)),
	}
}

// Both notice generations carry the original finding. Read that evidence rather
// than re-evaluating today's trust settings: a historical hold requires a human
// even if the reporter has since become trusted or the gate has been disabled.
var reporterTrustReasonPattern = regexp.MustCompile(`reporter-trust hold — issue #([0-9]+) filed by @([A-Za-z0-9-]+)`)
var legacyReporterTrustReasonPattern = regexp.MustCompile(`rationale traces to [^\s]+#([0-9]+), filed by @([A-Za-z0-9-]+)`)

func reporterTrustReasonFromComments(comments []*gh.IssueComment, appBotLogin string) string {
	for _, comment := range comments {
		if comment == nil || !IsReporterTrustHoldNotice(comment.GetBody()) {
			continue
		}
		if strings.TrimSpace(appBotLogin) != "" && !strings.EqualFold(safeGetLogin(comment.GetUser()), appBotLogin) {
			continue
		}
		for _, pattern := range []*regexp.Regexp{reporterTrustReasonPattern, legacyReporterTrustReasonPattern} {
			match := pattern.FindStringSubmatch(comment.GetBody())
			if len(match) == 3 {
				issue, err := strconv.Atoi(match[1])
				if err == nil && issue > 0 {
					return (ReporterTrust{Held: true, Issue: issue, Reporter: match[2]}).NeedsHumanReason()
				}
			}
		}
		return "reporter-trust hold — maintainer sign-off required"
	}
	return ""
}

func (c *Client) reporterTrustHeldPRReason(ctx context.Context, owner, repo string, number int, labels []string) string {
	if strings.TrimSpace(c.appBotLogin) == "" {
		return ""
	}
	// Only held PRs call this helper. Holds using other labels need no comment
	// fetch; old notices without needs-human remain eligible for enrichment.
	candidate := false
	for _, label := range labels {
		candidate = candidate || label == issueNeedsHumanLabel || label == "hold"
	}
	if !candidate {
		return ""
	}
	comments, err := c.listIssueComments(ctx, owner, repo, number)
	if err != nil {
		return "" // Display enrichment must never fail enumeration.
	}
	return reporterTrustReasonFromComments(comments, c.appBotLogin)
}

func hasReporterTrustNotice(comments []*gh.IssueComment, appBotLogin string) bool {
	for _, comment := range comments {
		if comment == nil {
			continue
		}
		if strings.TrimSpace(appBotLogin) != "" && !strings.EqualFold(safeGetLogin(comment.GetUser()), appBotLogin) {
			continue
		}
		if IsReporterTrustHoldNotice(comment.GetBody()) {
			return true
		}
	}
	return false
}
