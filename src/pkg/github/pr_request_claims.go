package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path"
	"regexp"
	"strconv"
	"strings"

	gh "github.com/google/go-github/v72/github"
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

var uncheckedTaskItemRE = regexp.MustCompile(`(?m)^\s*[-*+]\s*\[\s\]`)

// validatePRRequestBody runs the cheap, local body checks — no API calls — so
// they sit before any gate that spends GitHub quota. Both rejections are
// permanent (retrying the same file cannot fix them) and both exist because of
// the same observed failure: hive-open-pr silently dropped `--body-file`, so
// PRs went out whose entire body was the attribution footer, and the
// "Closes #N" line the agent had written never reached GitHub (the issue then
// stayed open after the fix merged).
//
//  1. An empty (or whitespace-only) body is refused outright. Every shipped
//     policy requires a real body; an empty one here means the body was lost
//     between the agent and the request. There is deliberately no larger size
//     floor: "Closes #12" is a legitimate minimal body under
//     scanner-automerge, so anything above "non-blank" would reject real work.
//  2. When the request declares originating issues (req.IssueN, set by
//     hive-open-pr --issues), the title+body must reference each one — as a
//     closing keyword ("Closes #N") or an explicit non-closing reference
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
			"the body must carry a \"Closes %s\" line (or \"Refs %s\" with a stated reason part of the issue stays open). "+
			"A missing line usually means the body was truncated or replaced; re-run hive-open-pr with the full body",
			strings.Join(missing, ", "), missing[0], missing[0])
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
		comparison, _, err := c.client.Repositories.CompareCommits(ctx, owner, repo, base, strings.TrimSpace(req.Head), nil)
		if err != nil {
			// #5343: same diagnosis as the content gate — an unpushed branch
			// must be reported as a push-auth failure, not as a bad title.
			return "", "", fmt.Errorf("validating PR title artifacts: %w",
				c.diagnoseCompareFailure(ctx, owner, repo, base, strings.TrimSpace(req.Head), err))
		}
		if comparison == nil {
			return "", "", fmt.Errorf("validating PR title against %s/%s diff %s...%s: GitHub returned an empty comparison", owner, repo, base, req.Head)
		}
		for _, rule := range claimedArtifacts {
			matched := false
			for _, file := range comparison.Files {
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
			return "", "", fmt.Errorf("validating closing reference %s#%d: %w", ref.Repo, ref.Issue, err)
		}
		reason := incompleteIssueReason(issue)
		if reason == "" {
			reason = humanFiledBugReason(issue)
		}
		if reason != "" {
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
	return downgradeClosingReferences(title, defaultRepo, downgrade), downgradeClosingReferences(body, defaultRepo, downgrade), nil
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
	if uncheckedTaskItemRE.MatchString(issue.GetBody()) {
		return "issue has unchecked task items"
	}
	return ""
}

// humanFiledBugConfirmationMarker is the case-insensitive marker a reporter (or
// a maintainer with write access) can drop into the issue body — or add as a
// label — to explicitly permit the App bot to auto-close the issue via a PR's
// closing keyword. When present, humanFiledBugReason returns "" and normal
// Closes # → auto-close behaviour is preserved.
//
// The marker is deliberately short and unambiguous so it can be pasted into a
// comment quote-block or copied from CONTRIBUTING without transcription risk.
const humanFiledBugConfirmationMarker = "hive: reporter-confirmed"

// humanFiledBugReason returns a non-empty downgrade reason when issue is a
// human-filed bug report that has NOT been marked as reporter-confirmed. When
// this reason is applied by validatePRRequestClaims the PR body's "Closes #N"
// is rewritten to "Refs #N", so a merge does NOT auto-close the reporter's
// bug (kubestellar/hive#6781).
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
// pkg/dashboard/contribute_ws.go (#6730): do not act — including "act by
// merging a Closes"-carrying PR — on a weak signal. A merged PR is evidence
// the CODE landed; it is NOT evidence the REPORTER'S SYMPTOM is gone. The
// reporter (or a maintainer) confirms that by dropping
// humanFiledBugConfirmationMarker on the issue.
//
// Scope. This gate ONLY applies to bugs filed by HUMANS. An agent's own
// bug-labeled finding — always stamped with AttributionTrailerPrefix, and
// often authored by an App/Bot account — is not affected: the App bot may
// still Closes # those, since the reporter is itself. See isHumanFiledBugReport
// for the exact detector.
func humanFiledBugReason(issue *gh.Issue) string {
	if !isHumanFiledBugReport(issue) {
		return ""
	}
	if hasReporterConfirmation(issue) {
		return ""
	}
	return "human-filed bug: reporter must confirm the fix before auto-closing (add " +
		strconv.Quote(humanFiledBugConfirmationMarker) + " to the issue body or apply the same label); " +
		"see kubestellar/hive#6781"
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

// isHumanFiledBugReport reports whether issue is a bug filed by a human. Three
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
func isHumanFiledBugReport(issue *gh.Issue) bool {
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
	if strings.Contains(issue.GetBody(), AttributionTrailerPrefix) {
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
	if issue == nil {
		return false
	}
	if strings.Contains(strings.ToLower(issue.GetBody()), humanFiledBugConfirmationMarker) {
		return true
	}
	for _, l := range issue.Labels {
		if strings.EqualFold(strings.TrimSpace(l.GetName()), humanFiledBugConfirmationMarker) {
			return true
		}
	}
	return false
}

func downgradeClosingReferences(text, defaultRepo string, downgrade map[string]string) string {
	if text == "" {
		return text
	}
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
		if _, ok := downgrade[claimKey(strings.ToLower(repo), issue)]; !ok {
			return match
		}
		return "Refs" + match[len(parts[1]):]
	})
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
