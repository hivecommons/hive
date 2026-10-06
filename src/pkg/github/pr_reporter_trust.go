package github

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
)

// The reporter-trust hold (hivecommons/hive#9665) is the merge-side half of
// project.issue_filter.reporter_trust. The admission half keeps a stranger's
// issue out of the queue until a maintainer triages it; this half makes sure
// an unaccepted stranger's request never merges without a human, at any ACMM
// level. A PR whose rationale traces to an unaccepted issue filed by a reporter
// the operator does not trust receives literal `hold` plus `needs-human` and a
// notice, and only a person removes it — the App never auto-releases a reporter-trust hold.
// An issue's configured acceptance label already supplies that sign-off.
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

// EvaluateReporterTrust decides whether a PR request's rationale traces to an
// issue filed by a reporter the operator does not trust.
//
// Any one unaccepted untrusted-reporter citation holds: unlike #5117, where one human
// rationale is enough to show a person asked, here the question is whether a
// stranger's request is about to merge unattended, and a PR that also cites a
// maintainer's issue is still delivering the stranger's. Holding is the safe
// direction; a maintainer who disagrees removes the label.
//
// Issues the hive or a bot filed are skipped — #5117 owns those. An issue that
// cannot be read decides nothing, for the reason pr_self_authorization.go gives:
// a hold nobody can explain is a hold people learn to strip.
func (c *Client) EvaluateReporterTrust(ctx context.Context, repo, title, body string, declared []int) ReporterTrust {
	if c == nil || c.client == nil {
		return ReporterTrust{}
	}
	refs := rationaleIssues(repo, title, body, declared)
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
		if !c.isHumanAuthor(issue.GetUser()) {
			// Hive- or bot-filed: #5117's business, or no author to judge.
			continue
		}
		login := strings.TrimSpace(issue.GetUser().GetLogin())
		association := strings.TrimSpace(issue.GetAuthorAssociation())
		if c.reporterIsTrusted(login, association) || c.reporterIssueAccepted(issue) {
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
> This PR was authored by the hive; the untrusted **reporter**, not the PR author, is @%s. Its rationale traces to %s#%d, filed by @%s (GitHub association: %s). The hold concerns that issue reporter, not the PR author. This hive holds unaccepted work requested by untrusted issue reporters so that it never merges without a maintainer looking at it, whatever the hive's autonomy level (hivecommons/hive#9665).
>
> Nothing here is a review of the change. To release the hold, a maintainer removes the `+"`hold`"+` label; Hive will not remove it on its own. The `+"`%s`"+` label marks it as waiting on a person. To trust this reporter in future, add them under **Settings → Labels → Reporter trust**.`,
		reporterTrustMarker(finding), finding.NeedsHumanReason(), finding.Reporter, finding.Repo, finding.Issue, finding.Reporter, associationOrUnknown(finding.Association), issueNeedsHumanLabel)
}

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
				if !finding.OwnsNeedsHuman {
					finding.OwnsNeedsHuman = true
					// Persist ownership when upgrading a legacy invisible hold.
					if err := c.CreateIssueComment(ctx, repo, pr.GetNumber(), reporterTrustNotice(finding)); err != nil {
						return labels, reason
					}
				}
				if err := c.AddLabels(ctx, repo, pr.GetNumber(), []string{issueNeedsHumanLabel}); err == nil {
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
