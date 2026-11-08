package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/issueclaim"
)

// prRequestPolicyError is a permanent mismatch between a PR's claim and its
// diff. It is distinct from forge/API errors, which the watcher should retry.
type prRequestPolicyError struct {
	reason string
}

func (e *prRequestPolicyError) Error() string { return e.reason }

func prRequestPolicyReason(err error) (string, bool) {
	var policyErr *prRequestPolicyError
	if !errors.As(err, &policyErr) {
		return "", false
	}
	return policyErr.reason, true
}

type titleArtifactRule struct {
	name       string
	titleMatch *regexp.Regexp
	fileMatch  func(string) bool
}

var titleArtifactRules = []titleArtifactRule{
	{name: "workflow", titleMatch: regexp.MustCompile(`(?i)\bworkflows?\b`), fileMatch: isWorkflowFile},
	{name: "test", titleMatch: regexp.MustCompile(`(?i)\btests?\b`), fileMatch: isTestFile},
	{name: "migration", titleMatch: regexp.MustCompile(`(?i)\bmigrations?\b`), fileMatch: isMigrationFile},
}

// delegatedTaskSubjectRE matches the SUBJECT of a task-list item that hands its
// work to another issue — "#4199", "owner/repo#12". Anchored at the start of the
// item text, exactly like trackerTaskListRe (client.go), because that is the
// form GitHub renders as a tracked task list: it is a structural convention the
// author opted into, not a phrase they happened to write. A prose item that
// merely mentions an issue ("rename the field (see #318)") is not delegation.
var delegatedTaskSubjectRE = regexp.MustCompile(`^(?:[\w.-]+/[\w.-]+)?#\d+\b`)

// hasUnfinishedDelegatedWork reports whether body carries an UNCHECKED
// task-list item whose subject is another issue — i.e. the issue is a
// multi-phase plan that tracks work living somewhere else, so merging one PR
// cannot finish it.
//
// This is deliberately narrower than "the body contains any `- [ ]`"
// (hivecommons/hive#7156). All 18 policy templates under pkg/policies/defaults
// instruct the filing agent that an unsplittable finding MUST carry a `- [ ]`
// task list as its completion criterion — so the broad test downgraded the
// closing keyword on precisely the issue format the hive mandates for itself,
// and the compensating task-list sweep could not recover it because nothing in
// the tree ever ticks a box. A definition-of-done checklist for one PR is not
// a tracker; a list of issue references is.
//
// Genuine trackers keep their downgrade: via the epic/tracker/meta-tracker
// labels, the "[epic]"/"[tracker]" title prefixes and IsTrackerIssue (three or
// more issue-referencing items) in incompleteIssueReason, and via this function
// for the one- and two-item delegating lists those three miss.
//
// Boxes inside fenced code blocks do not count, matching countTaskListBoxes in
// the sweep (task_list_sweep.go). The two scanners disagreeing was its own bug:
// an issue that merely QUOTED the policy's own `- [ ]` example was downgraded
// for the quote, while the sweep that was supposed to close it ignored the same
// line. Both now read a body the same way, through listTaskListItems.
func hasUnfinishedDelegatedWork(body string) bool {
	for _, item := range listTaskListItems(body) {
		if item.Checked {
			continue
		}
		if delegatedTaskSubjectRE.MatchString(item.Text) {
			return true
		}
	}
	return false
}

// validatePRRequestBody runs the cheap, local body checks — no API calls — so
// they sit before any gate that spends GitHub quota. Both rejections are
// permanent (retrying the same file cannot fix them) and both exist because of
// the same observed failure: hive-open-pr silently dropped `--body-file`, so
// PRs went out whose entire body was the attribution footer, and the
// "Fixes #N" line the agent had written never reached GitHub (the issue then
// stayed open after the fix merged).
//
//  1. An empty (or whitespace-only) body is refused outright. Every shipped
//     policy requires a real body; an empty one here means the body was lost
//     between the agent and the request. There is deliberately no larger size
//     floor: "Fixes #12" is a legitimate minimal body under
//     scanner-automerge, so anything above "non-blank" would reject real work.
//  2. When the request declares originating issues (req.IssueN, set by
//     hive-open-pr --issues), the title+body must reference each one — as a
//     closing keyword ("Fixes #N") or an explicit non-closing reference
//     ("Refs #N"). A body that arrives without the reference it was supposed
//     to carry is the same lost-content failure in partial form.
//
// Returns "" when the request passes, else the policy reason for rejection.
func (c *Client) validatePRRequestBody(req PRRequest) string {
	if strings.TrimSpace(req.Body) == "" {
		return "PR body is empty — refusing to open a body-less PR. The body was " +
			"probably lost on the way in (hive-open-pr accepts --body and --body-file); " +
			"re-run hive-open-pr with the full body"
	}
	if len(req.IssueN) == 0 {
		return ""
	}
	// GitHub numbers issues from 1, and ParseClaimedIssues/ParseReferencedIssues
	// drop #0 for the same reason — so a declared 0 can never be satisfied by
	// any body. It is the "--issues <N>" placeholder left unsubstituted, not
	// lost content; say so, or the "re-run with the full body" remedy below
	// sends the agent into a retry loop that cannot succeed (#16162).
	for _, n := range req.IssueN {
		if n <= 0 {
			return fmt.Sprintf("request declares originating issue #%d, which is not a GitHub issue number (issues are numbered from 1) — "+
				"the --issues value was a placeholder, not the issue this PR is for. "+
				"Re-run hive-open-pr with the real issue number, or omit --issues when the work did not start from an issue", n)
		}
	}
	owner, repo := c.prRequestRepo(req.Repo)
	defaultRepo := owner + "/" + repo
	text := req.Title + "\n" + req.Body
	referenced := make(map[int]bool)
	for _, ref := range append(ParseClaimedIssues(text, defaultRepo), ParseReferencedIssues(text, defaultRepo)...) {
		if strings.EqualFold(ref.Repo, defaultRepo) {
			referenced[ref.Issue] = true
		}
	}
	var missing []string
	for _, n := range req.IssueN {
		if !referenced[n] {
			missing = append(missing, "#"+strconv.Itoa(n))
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("request declares originating issue(s) %s but the PR body never references them — "+
			"the body must carry a \"Fixes %s\" line (or \"Refs %s\" with a stated reason part of the issue stays open; use \"Refs %s (needs-human: <reason>)\" when the remainder requires a human). "+
			"A missing line usually means the body was truncated or replaced; re-run hive-open-pr with the full body",
			strings.Join(missing, ", "), missing[0], missing[0], missing[0])
	}
	return ""
}

// validatePRRequestClaims checks objective artifact claims against the compare
// file list and downgrades unsafe closing references on structurally incomplete
// issues. It returns the title/body to send to GitHub.
func (c *Client) validatePRRequestClaims(ctx context.Context, req PRRequest) (string, string, error) {
	if c == nil || c.client == nil {
		return "", "", ErrNoGitHubClient
	}

	owner, repo := c.prRequestRepo(req.Repo)
	defaultRepo := owner + "/" + repo
	title, body := req.Title, req.Body

	var claimedArtifacts []titleArtifactRule
	for _, rule := range titleArtifactRules {
		if rule.titleMatch.MatchString(title) {
			claimedArtifacts = append(claimedArtifacts, rule)
		}
	}
	if len(claimedArtifacts) > 0 {
		base := strings.TrimSpace(req.Base)
		if base == "" {
			resolved, err := c.DefaultBranch(ctx, owner, repo)
			if err != nil {
				return "", "", fmt.Errorf("validating PR title artifacts: %w", err)
			}
			base = resolved
		}
		comparison, resp, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, strings.TrimSpace(req.Head),
			&gh.ListOptions{PerPage: githubCompareFileLimit})
		if err != nil {
			// #5343: same diagnosis as the content gate — an unpushed branch
			// must be reported as a push-auth failure, not as a bad title.
			return "", "", fmt.Errorf("validating PR title artifacts: %w",
				c.diagnoseCompareFailure(ctx, owner, repo, base, strings.TrimSpace(req.Head), err))
		}
		if comparison == nil {
			return "", "", fmt.Errorf("validating PR title against %s/%s diff %s...%s: GitHub returned an empty comparison", owner, repo, base, req.Head)
		}
		// The compare endpoint pages its files array; scan every page so an
		// artifact beyond the first 300 files is still seen.
		files := comparison.Files
		for resp != nil && resp.NextPage != 0 && len(files) < prRequestMaxScanFiles {
			next, r, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, strings.TrimSpace(req.Head),
				&gh.ListOptions{PerPage: githubCompareFileLimit, Page: resp.NextPage})
			if err != nil {
				return "", "", fmt.Errorf("validating PR title artifacts: %w", err)
			}
			if next == nil || len(next.Files) == 0 {
				break
			}
			files = append(files, next.Files...)
			resp = r
		}
		for _, rule := range claimedArtifacts {
			matched := false
			for _, file := range files {
				if file != nil && rule.fileMatch(file.GetFilename()) {
					matched = true
					break
				}
			}
			if !matched {
				return "", "", &prRequestPolicyError{reason: fmt.Sprintf("title claims %s but diff contains no %s file", rule.name, rule.name)}
			}
		}
	}

	refs := ParseClaimedIssues(title+"\n"+body, defaultRepo)
	if len(refs) == 0 {
		return title, body, nil
	}

	downgrade := make(map[string]string)
	for _, ref := range refs {
		refOwner, refRepo := c.prRequestRepo(ref.Repo)
		issue, _, err := c.client.Issues.Get(ctx, refOwner, refRepo, ref.Issue)
		if err != nil {
			// A 404 is permanent: the referenced issue does not exist in that
			// repository (typically a cross-repo number written bare, so it
			// resolved against the PR's own repo). Retrying can never fix it.
			if githubStatusError(err, http.StatusNotFound) {
				return "", "", &prRequestPolicyError{reason: fmt.Sprintf(
					"closing reference %s#%d does not exist; a cross-repo issue must be written as owner/repo#N",
					refOwner+"/"+refRepo, ref.Issue)}
			}
			return "", "", fmt.Errorf("validating closing reference %s#%d: %w", ref.Repo, ref.Issue, err)
		}
		if reason := incompleteIssueReason(issue); reason != "" {
			key := claimKey(strings.ToLower(refOwner+"/"+refRepo), ref.Issue)
			downgrade[key] = reason
			c.logger.Warn("pr-request watcher: downgraded closing reference to Refs",
				slog.String("repo", refOwner+"/"+refRepo), slog.Int("issue", ref.Issue),
				slog.String("reason", reason), slog.String("head", req.Head))
		}
	}
	if len(downgrade) == 0 {
		return title, body, nil
	}
	// The title is rewritten without the note: a one-line title has no room for
	// a reason, and the note belongs where a reviewer reads the reference.
	return downgradeClosingReferences(title, defaultRepo, downgrade, false),
		downgradeClosingReferences(body, defaultRepo, downgrade, true), nil
}

func (c *Client) prRequestRepo(repo string) (string, string) {
	owner := c.org
	if parts := strings.SplitN(strings.TrimSpace(repo), "/", 2); len(parts) == 2 {
		return parts[0], parts[1]
	}
	return owner, strings.TrimSpace(repo)
}

func incompleteIssueReason(issue *gh.Issue) string {
	if issue == nil {
		return "issue metadata is empty"
	}
	if why, ok := isTrackingIssue(issue); ok {
		return "issue is a tracking/archive issue (" + why + "); merging does not resolve it"
	}
	labels := make([]string, 0, len(issue.Labels))
	for _, label := range issue.Labels {
		name := label.GetName()
		labels = append(labels, name)
		if strings.EqualFold(name, "epic") || strings.EqualFold(name, "tracker") || strings.EqualFold(name, "meta-tracker") {
			return "issue is labeled as a tracker or epic"
		}
	}
	title := strings.ToLower(strings.TrimSpace(issue.GetTitle()))
	if strings.HasPrefix(title, "[epic]") || strings.HasPrefix(title, "[tracker]") {
		return "issue title marks it as a tracker or epic"
	}
	if IsTrackerIssue(issue.GetTitle(), labels, issue.GetBody()) {
		return "issue is a tracker"
	}
	if hasUnfinishedDelegatedWork(issue.GetBody()) {
		return "issue has unchecked task items that delegate work to other issues"
	}
	return ""
}

// humanFiledBugConfirmationMarker is the legacy case-insensitive marker a
// reporter (or a maintainer with write access) can drop into the issue body —
// or add as a label — after verifying the fix. Close-on-merge is now the
// default, so this marker is normally redundant; it still bypasses the
// opt-in reporter-confirmation gate for parked historical issues.
//
// The marker is deliberately short and unambiguous so it can be pasted into a
// comment quote-block or copied from CONTRIBUTING without transcription risk.
const humanFiledBugConfirmationMarker = "hive: reporter-confirmed"

// humanFiledBugCloseOnMergeMarker is the legacy filing-time marker for
// human-filed bugs whose merged fix IS the verification. Close-on-merge is now
// default, so this marker is normally redundant; it remains accepted as a
// bypass when the opt-in reporter-confirmation gate is active.
const humanFiledBugCloseOnMergeMarker = "hive: close-on-merge"

// humanFiledBugNeedsConfirmationMarker opts one human-filed bug issue into the
// reporter-confirmation gate. It is accepted in the issue body or as a label.
const humanFiledBugNeedsConfirmationMarker = "hive: needs-confirmation"

// humanFiledBugReason returns a non-empty close-gate reason when issue is a
// human-filed bug report and reporter confirmation has been explicitly
// requested by marker/label or by the operator config switch. The issue close
// path uses this reason to ask for reporter confirmation rather than closing
// the issue as completed.
//
// Why this exists. GitHub auto-closes an issue on the merge of a PR that
// carries "Closes #N", attributed to whoever pushed the merge — for the hive,
// that is the App bot. GitHub only lets an issue be reopened by users with
// write access or by whoever closed it, so the ORIGINAL REPORTER cannot
// reopen an App-bot-closed issue if the symptom persists. #6500 was closed
// this way while the symptom was still live; @MikeSpreitzer could not reopen
// and re-filed the identical bug as #6767 and #6762. One unverified closure
// booked two extra maintainer-filed bugs within hours.
//
// The philosophy mirrors isEvidenceLessCompletion in
// pkg/dashboard/contribute_ws.go (#6730): do not close a human reporter's
// issue on a weak signal. The default has since changed: fix merges close
// issues, and /reopen is the backstop. For exceptional cases where the
// reporter genuinely must verify a symptom, add
// humanFiledBugNeedsConfirmationMarker or enable the operator config switch.
//
// Scope. This gate ONLY applies to bugs filed by HUMANS. An agent's own
// bug-labeled finding — always stamped with AttributionTrailerPrefix, and
// often authored by an App/Bot account — is not affected: the App bot may
// still Closes # those, since the reporter is itself. See IsHumanFiledBugReport
// for the exact detector.
func humanFiledBugReason(issue *gh.Issue, reporterConfirmationDefault bool) string {
	if !IsHumanFiledBugReport(issue) {
		return ""
	}
	if hasReporterConfirmation(issue) || hasCloseOnMergeOptIn(issue) {
		return ""
	}
	if !reporterConfirmationDefault && !hasReporterConfirmationOptIn(issue) {
		return ""
	}
	return "human-filed bug opted into reporter confirmation: reporter must confirm the fix before closing (remove " +
		strconv.Quote(humanFiledBugNeedsConfirmationMarker) + ", add " +
		strconv.Quote(humanFiledBugConfirmationMarker) + ", or apply " +
		strconv.Quote(humanFiledBugCloseOnMergeMarker) + " when the merged fix is the verification); " +
		"see hivecommons/hive#11119"
}

// bugLabelNames is the small closed set of label names the hive treats as a
// "this issue is a bug report". Kept case-insensitive and space-insensitive via
// normalizeBugLabelName. If a project uses another label, humanFiledBugReason
// simply does not fire and current behaviour is preserved — this gate never
// causes a false downgrade for issues it did not identify as bugs.
var bugLabelNames = map[string]struct{}{
	"bug":              {},
	"kind/bug":         {},
	"type/bug":         {},
	"type:bug":         {},
	"adoption-blocker": {},
}

func normalizeBugLabelName(name string) string {
	// Collapse "type: bug" and "Type / Bug" onto the canonical "type:bug".
	return strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), " ", "")
}

// IsHumanFiledBugReport reports whether issue is a bug filed by a human. Three
// conservative signals combine — a missing signal returns false so we never
// gate agent-filed findings (which are safe to auto-close, since the reporter
// is the hive itself):
//
//  1. A bug-family label is present (bug / kind/bug / type/bug / type:bug /
//     adoption-blocker; matched via normalizeBugLabelName).
//  2. The body does NOT carry AttributionTrailerPrefix ("— hive:"). Every
//     hive-mediated create is stamped by AppendTrailer with that greppable
//     marker; if it is present, the issue came from the hive (an agent
//     finding), not from a maintainer.
//  3. The author is not a Bot. An App-installation-token-authored issue
//     carries User.Type == "Bot"; if the issue came from a bot account, it is
//     not a human report and this gate does not apply.
//
// All three must hold. Missing any one keeps current auto-close behaviour, so
// the gate is fail-open on ambiguity by design: an agent's bug finding stays
// closeable, and a maintainer's bug is protected.
func IsHumanFiledBugReport(issue *gh.Issue) bool {
	if issue == nil {
		return false
	}
	hasBug := false
	for _, l := range issue.Labels {
		if _, ok := bugLabelNames[normalizeBugLabelName(l.GetName())]; ok {
			hasBug = true
			break
		}
	}
	if !hasBug {
		return false
	}
	if HasAttributionTrailer(issue.GetBody()) {
		return false
	}
	if issue.User != nil && strings.EqualFold(issue.User.GetType(), "Bot") {
		return false
	}
	return true
}

// hasReporterConfirmation reports whether the reporter has explicitly opted
// in to auto-close via the marker (in the body) or the corresponding label.
// Body match is case-insensitive so a marker pasted in mixed case is honoured.
func hasReporterConfirmation(issue *gh.Issue) bool {
	return hasIssueMarker(issue, humanFiledBugConfirmationMarker)
}

// hasCloseOnMergeOptIn reports whether the filer opted in to close-on-merge
// via humanFiledBugCloseOnMergeMarker in the body or as a label
// (hivecommons/hive#10304).
func hasCloseOnMergeOptIn(issue *gh.Issue) bool {
	return hasIssueMarker(issue, humanFiledBugCloseOnMergeMarker)
}

// hasReporterConfirmationOptIn reports whether the issue explicitly asks the
// hive to wait for reporter verification before closing.
func hasReporterConfirmationOptIn(issue *gh.Issue) bool {
	return hasIssueMarker(issue, humanFiledBugNeedsConfirmationMarker)
}

// hasIssueMarker reports whether marker (lower-case) appears in the issue body
// (case-insensitive substring) or as a label (case-insensitive, trimmed).
func hasIssueMarker(issue *gh.Issue, marker string) bool {
	if issue == nil {
		return false
	}
	if strings.Contains(strings.ToLower(issue.GetBody()), marker) {
		return true
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(l.GetName()), marker) {
			return true
		}
	}
	return false
}

// downgradeClosingReferences rewrites every closing keyword in text to "Refs"
// for the issues named in downgrade. When annotate is true the FIRST rewritten
// reference for each issue also carries the reason inline, so a maintainer
// reading the opened PR can see that the issue will not close and why
// (hivecommons/hive#7156). Before that, the only record was a WARN line in a
// container log with a day or two of retention, and a merged "Refs #N" was
// indistinguishable from an agent that deliberately chose not to close.
//
// The reason is wrapped in backticks: reasons quote issue references
// themselves, and GitHub does not create a cross-reference from text inside a
// code span, so the note cannot spam an unrelated issue with mentions.
func downgradeClosingReferences(text, defaultRepo string, downgrade map[string]string, annotate bool) string {
	if text == "" {
		return text
	}
	annotated := make(map[string]bool, len(downgrade))
	return claimRefPattern.ReplaceAllStringFunc(text, func(match string) string {
		parts := claimRefPattern.FindStringSubmatch(match)
		if len(parts) < 4 {
			return match
		}
		issue, err := strconv.Atoi(parts[3])
		if err != nil {
			return match
		}
		repo := parts[2]
		if repo == "" {
			repo = defaultRepo
		}
		key := claimKey(strings.ToLower(repo), issue)
		reason, ok := downgrade[key]
		if !ok {
			return match
		}
		rewritten := "Refs" + match[len(parts[1]):]
		if !annotate || reason == "" || annotated[key] {
			return rewritten
		}
		annotated[key] = true
		return rewritten + downgradeNote(reason)
	})
}

// closingKeywordWithheldNote marks a reference the watcher deliberately made
// non-closing; later link repair must not upgrade it back to a closing keyword.
const closingKeywordWithheldNote = "closing keyword withheld by the hive watcher"

// downgradeNote renders the inline explanation appended to a rewritten
// reference. Kept separate so the wording is asserted in one place by the
// tests rather than spelled out at each call site.
func downgradeNote(reason string) string {
	return " — " + closingKeywordWithheldNote + ": `" + reason +
		"`. Merging this PR will not close the issue."
}

func isWorkflowFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	if !strings.HasPrefix(filename, ".github/workflows/") {
		return false
	}
	ext := path.Ext(filename)
	return ext == ".yml" || ext == ".yaml"
}

func isTestFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	base := path.Base(filename)
	ext := path.Ext(base)
	stem := strings.TrimSuffix(base, ext)
	if strings.HasSuffix(stem, "_test") || strings.HasSuffix(stem, "_spec") ||
		strings.HasPrefix(base, "test_") || strings.HasPrefix(base, "test-") ||
		strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") || ext == ".bats" {
		return true
	}
	for _, segment := range strings.Split(filename, "/") {
		if segment == "test" || segment == "tests" || segment == "spec" || segment == "specs" || segment == "__tests__" {
			return true
		}
	}
	return false
}

func isMigrationFile(filename string) bool {
	filename = strings.ToLower(strings.TrimPrefix(path.Clean(filename), "./"))
	for _, segment := range strings.Split(filename, "/") {
		if segment == "migration" || segment == "migrations" || segment == "migrate" {
			return true
		}
	}
	return false
}

// prRequestClaimTargetMarker tags the notice the claim-target gate posts on
// an issue it refused a PR request for, so a retried or repeated request
// edits the one notice instead of stacking another.
const prRequestClaimTargetMarker = "<!-- hive:pr-request-claim-target -->"

// prRequestTargetRefusal is one closing reference the claim-target gate
// refused: the issue, and who holds it — a live claim (Holder) or an open
// pull request by another author that covers it (CoveringPR).
type prRequestTargetRefusal struct {
	Repo       string
	Issue      int
	Holder     string
	CoveringPR string
}

func (r prRequestTargetRefusal) why() string {
	if r.Holder != "" {
		return "already claimed by " + r.Holder
	}
	return "covered by " + r.CoveringPR
}

// validatePRRequestTargets is the claim-target gate (hivecommons/hive#17298):
// a PR request may not close an issue another principal is working. For each
// closing reference it refuses the issue when a live claim is held by anyone
// other than the requesting agent (contributor, person or another agent), or
// when an open pull request by an author outside this hive covers it. The
// requesting agent's own claim and the hive's own covering PR pass. It runs
// before the request books a start on any issue (notifyAgentStart), so
// epic-driven work that never claimed the target is checked too — the case
// that let #17250 supersede a contributor's #17248 on #17243.
//
// A refusal is returned as a prRequestPolicyError alongside the refused
// targets, as is a closing reference whose issue does not exist (permanent,
// so never retried); any other claim or coverage lookup failure returns a
// plain error, so the request is not opened (fail closed) and is retried
// later.
func (c *Client) validatePRRequestTargets(ctx context.Context, req PRRequest, now time.Time) ([]prRequestTargetRefusal, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	owner, repo := c.prRequestRepo(req.Repo)
	defaultRepo := owner + "/" + repo
	var refused []prRequestTargetRefusal
	seen := make(map[string]bool)
	for _, ref := range ParseClaimedIssues(req.Title+"\n"+req.Body, defaultRepo) {
		refOwner, refRepo := c.prRequestRepo(ref.Repo)
		key := claimKey(strings.ToLower(refOwner+"/"+refRepo), ref.Issue)
		if seen[key] {
			continue
		}
		seen[key] = true
		refusal, blocked, err := c.prRequestTargetBlock(ctx, req.Agent, refOwner, refRepo, ref.Issue, now)
		if err != nil {
			// A missing target is permanent (typically a cross-repo number
			// written bare): reject it the way validatePRRequestClaims does
			// rather than retrying until the horizon quarantines the request.
			if errors.Is(err, errPRRequestTargetMissing) {
				return nil, &prRequestPolicyError{reason: fmt.Sprintf(
					"closing reference %s#%d does not exist; a cross-repo issue must be written as owner/repo#N",
					refOwner+"/"+refRepo, ref.Issue)}
			}
			return nil, fmt.Errorf("checking claims on closing reference %s/%s#%d: %w", refOwner, refRepo, ref.Issue, err)
		}
		if blocked {
			refused = append(refused, refusal)
		}
	}
	if len(refused) == 0 {
		return nil, nil
	}
	parts := make([]string, 0, len(refused))
	for _, r := range refused {
		parts = append(parts, fmt.Sprintf("%s#%d is %s", r.Repo, r.Issue, r.why()))
	}
	return refused, &prRequestPolicyError{reason: strings.Join(parts, "; ") +
		" — another principal is working it, so this PR may not close it; the owner decides whether to proceed. " +
		"Pick other work, or reference the issue without a closing keyword"}
}

// issueRequestStartTargetBlock is the claim-target gate on the kick/start
// path (hivecommons/hive#17298): an issue request that starts work on
// req.Number — a claim, or a comment announcing a start — is checked the
// way a closing reference in hive-open-pr is, so epic-driven work that
// never claims its target is refused before the start is booked.
func (c *Client) issueRequestStartTargetBlock(ctx context.Context, req IssueRequest, now time.Time) (prRequestTargetRefusal, bool, error) {
	if c == nil || c.client == nil {
		return prRequestTargetRefusal{}, false, ErrNoGitHubClient
	}
	owner, repo := c.prRequestRepo(req.Repo)
	refusal, blocked, err := c.prRequestTargetBlock(ctx, req.Agent, owner, repo, req.Number, now)
	if err != nil {
		return refusal, false, fmt.Errorf("checking claims on start target %s/%s#%d: %w", owner, repo, req.Number, err)
	}
	return refusal, blocked, nil
}

// issueRequestStartRefusedReason is the policy reason written to a start
// request the claim-target gate refused.
func issueRequestStartRefusedReason(kind string, r prRequestTargetRefusal) string {
	verb := "start refused"
	if kind == "claim" {
		verb = "issue claim refused"
	}
	return fmt.Sprintf("%s: %s#%d is %s — another principal is working it; the owner decides whether to proceed. Pick other work",
		verb, r.Repo, r.Issue, r.why())
}

// prRequestTargetBlock resolves one target issue: its live claim (the
// forge marker or assignee when issue claims are on, else the hub's claim
// ledger) and the duplicate-PR ledger's covering PR, re-read so a PR that
// has since closed or merged no longer blocks. A target that is really a
// pull request is never blocked: a start comment on a PR (a hive relay
// note on a contributor's PR, say) does not start work on an issue.
func (c *Client) prRequestTargetBlock(ctx context.Context, agent, owner, repo string, number int, now time.Time) (prRequestTargetRefusal, bool, error) {
	refusal := prRequestTargetRefusal{Repo: owner + "/" + repo, Issue: number}
	claim, found, err := c.prRequestTargetClaim(ctx, owner, repo, number, now)
	if errors.Is(err, errPRRequestTargetIsPullRequest) {
		return refusal, false, nil
	}
	if err != nil {
		return refusal, false, err
	}
	if found && !c.isOwnTargetClaim(claim, agent) {
		refusal.Holder = claim.Identity
		return refusal, true, nil
	}
	issueRef := c.IssueStageRef(owner+"/"+repo, number)
	cover, ok := c.ledgerClaim(issueRef)
	if !ok || !cover.CoversOpenIssue() || !cover.ExternalAuthor {
		return refusal, false, nil
	}
	pr, ok := c.claimPRRef(cover, issueRef)
	if !ok {
		return refusal, false, nil
	}
	live, _, err := c.client.PullRequests.Get(ctx, pr.Owner, pr.Repo, pr.Number)
	if err != nil {
		return refusal, false, fmt.Errorf("covering PR %s: %w", pr, err)
	}
	if strings.EqualFold(live.GetState(), "closed") || live.GetMerged() || !live.GetMergedAt().IsZero() {
		return refusal, false, nil
	}
	refusal.CoveringPR = fmt.Sprintf("#%d", pr.Number)
	if !strings.EqualFold(pr.Owner+"/"+pr.Repo, owner+"/"+repo) {
		refusal.CoveringPR = fmt.Sprintf("%s/%s#%d", pr.Owner, pr.Repo, pr.Number)
	}
	if author := live.GetUser().GetLogin(); author != "" {
		refusal.CoveringPR += " (@" + author + ")"
	}
	return refusal, true, nil
}

// isOwnTargetClaim reports whether claim is the requesting agent's own. A
// bare agent name is only meaningful for kind=agent (or a legacy marker
// without kind): a contributor's login can equal an agent name, and a
// same-named agent in another hive is not this hive's agent.
func (c *Client) isOwnTargetClaim(claim issueclaim.Claim, agent string) bool {
	if claim.Kind != "" && !strings.EqualFold(claim.Kind, "agent") {
		return false
	}
	if !strings.EqualFold(strings.TrimSpace(claim.Identity), strings.TrimSpace(agent)) {
		return false
	}
	if c.issueClaimLedgerHive != nil {
		if hive := c.issueClaimLedgerHive(); hive != "" && claim.Hive != "" && claim.Hive != hive {
			return false
		}
	}
	return true
}

// errPRRequestTargetMissing wraps the 404 from reading a claim target: the
// issue does not exist in that repository, which no retry can change.
var errPRRequestTargetMissing = errors.New("target issue does not exist")

// errPRRequestTargetIsPullRequest reports that the claim target resolved to
// a pull request, which carries no issue claim.
var errPRRequestTargetIsPullRequest = errors.New("target is a pull request")

// prRequestTargetClaim is the live claim on one issue, read the way the
// enumeration reads it (issueClaimFor) when issue claims are on, and from
// the hub's ledger alone otherwise.
func (c *Client) prRequestTargetClaim(ctx context.Context, owner, repo string, number int, now time.Time) (issueclaim.Claim, bool, error) {
	enabled, ttl := c.issueClaimsSetting()
	if !enabled {
		claim, ok := c.ledgerClaimFor(owner, repo, number, now)
		return claim, ok, nil
	}
	issue, _, err := c.client.Issues.Get(ctx, owner, repo, number)
	if err != nil {
		if githubStatusError(err, http.StatusNotFound) {
			return issueclaim.Claim{}, false, fmt.Errorf("%w: %w", errPRRequestTargetMissing, err)
		}
		return issueclaim.Claim{}, false, err
	}
	if issue.IsPullRequest() {
		return issueclaim.Claim{}, false, errPRRequestTargetIsPullRequest
	}
	snapshot := Issue{
		Repo:      owner + "/" + repo,
		Number:    number,
		UpdatedAt: issue.GetUpdatedAt().Time,
	}
	for _, a := range issue.Assignees {
		if login := a.GetLogin(); login != "" {
			snapshot.Assignees = append(snapshot.Assignees, login)
		}
	}
	return c.issueClaimFor(ctx, owner, repo, &snapshot, ttl, now)
}

// prRequestTargetAction names what the refused request was trying to do,
// so the notice on the target issue describes the event that happened.
type prRequestTargetAction string

const (
	// prRequestTargetActionOpenPR: hive-open-pr with a closing reference.
	prRequestTargetActionOpenPR prRequestTargetAction = "open-pr"
	// prRequestTargetActionClaim: hive-open-issue claim on the issue.
	prRequestTargetActionClaim prRequestTargetAction = "claim"
	// prRequestTargetActionStart: a relay comment announcing a start.
	prRequestTargetActionStart prRequestTargetAction = "start"
)

// issueRequestStartTargetAction maps a start-path request kind to the
// action named in the claim-target notice.
func issueRequestStartTargetAction(kind string) prRequestTargetAction {
	if kind == "claim" {
		return prRequestTargetActionClaim
	}
	return prRequestTargetActionStart
}

// noticePRRequestTargetRefusalBody is the notice posted on a refused target
// issue: what was asked, why it was refused, that nothing happened, and
// that the owner decides.
func noticePRRequestTargetRefusalBody(action prRequestTargetAction, r prRequestTargetRefusal) string {
	asked, outcome := "asked to open a pull request that would close this issue", "No PR was opened"
	switch action {
	case prRequestTargetActionClaim:
		asked, outcome = "asked to claim this issue", "The claim was not recorded"
	case prRequestTargetActionStart:
		asked, outcome = "asked to start work on this issue", "No work was started"
	}
	return fmt.Sprintf("%s\nA hive agent %s, but it is %s. %s; the owner decides whether to proceed.",
		prRequestClaimTargetMarker, asked, r.why(), outcome)
}

// noticePRRequestTargetRefusal posts (or refreshes) the one notice on a
// refused target issue. Best-effort: the rejected request is the
// enforcement, the notice tells the issue's watchers why.
func (c *Client) noticePRRequestTargetRefusal(ctx context.Context, action prRequestTargetAction, r prRequestTargetRefusal) {
	body := noticePRRequestTargetRefusalBody(action, r)
	if _, err := c.EditIssueMarkerComment(ctx, r.Repo, r.Issue, prRequestClaimTargetMarker, func(string) string { return body }); err != nil {
		c.logger.Warn("pr-request watcher: could not post the claim-target notice",
			slog.String("repo", r.Repo), slog.Int("issue", r.Issue), slog.String("error", err.Error()))
	}
}
