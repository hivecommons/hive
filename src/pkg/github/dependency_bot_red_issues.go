package github

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/advisory"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/writeback"
)

// Dependency-bot red-CI issues (auto_merge.dependency_bot_red_issue).
//
// A dependency-bot PR whose failing required checks all pass on the base tip,
// and whose failure reproduced (within one check suite of a head that
// already contains that tip, or across two successive heads, the newer one
// brought up to date with the base), is red because of the bump itself: an upstream change usually needs a
// companion change in the repository (a Makefile target, a config key, a
// renamed import). The governor's dependency-bot sweep
// (cmd/hive/dependency_bot_red_issue.go) files one issue here saying so,
// through the same gate and create path as the shared-CI tracking issues
// (shared_ci_issues.go): write-back kind issue_file and CreateIssue with
// governor origin. Each issue body starts with a marker line naming the PR
// and its failing check set, so a restart finds it again instead of filing a
// second one, and a later note is only ever posted on an issue the hive
// itself wrote. Everything taken from the PR (title, check names, run links)
// is neutralized first, so it can neither forge that marker nor trip the
// issue template guard.

// DependencyBotRedIssueMarkerPrefix starts the marker line that opens every
// issue body this filer writes.
const DependencyBotRedIssueMarkerPrefix = "<!-- hive-dependency-bot-red "

const (
	// After an issue for the same PR and checks closes, none is filed
	// again within these windows, as for the shared-CI incidents.
	dependencyBotRedIssueRefileAfterCompleted  = sharedCIIssueRefileAfterCompleted
	dependencyBotRedIssueRefileAfterNotPlanned = sharedCIIssueRefileAfterNotPlanned

	dependencyBotRedIssueMaxExcerptLines = 10
	dependencyBotRedIssueMaxTitleChecks  = 3
	dependencyBotRedIssueMaxTextLen      = 200
)

// ErrDependencyBotRedIssueRefused is returned when the write-back gate
// refuses the issue_file write; nothing was sent to GitHub.
var ErrDependencyBotRedIssueRefused = errors.New("dependency-bot red-CI issue refused by the write-back gate")

// DependencyBotRedSafeText makes one piece of PR-supplied text (a title, a
// check name) safe to put in an issue title or body: one line, at most
// dependencyBotRedIssueMaxTextLen bytes, no backtick (it would end a code
// span) and no angle bracket (it could open a marker comment or read as an
// unfilled template placeholder).
func DependencyBotRedSafeText(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	s = strings.ReplaceAll(s, "`", "'")
	s = strings.ReplaceAll(s, "<", "‹")
	s = strings.ReplaceAll(s, ">", "›")
	if len(s) > dependencyBotRedIssueMaxTextLen {
		s = strings.ToValidUTF8(s[:dependencyBotRedIssueMaxTextLen], "")
	}
	return s
}

// DependencyBotRedSafeURL returns u when it is a plain http(s) link on
// host (the forge's own web host, read from the PR's own URL) with nothing
// that could break out of the markdown around it, else "". An empty host
// admits nothing.
func DependencyBotRedSafeURL(u, host string) string {
	u = strings.TrimSpace(u)
	host = strings.TrimSpace(host)
	if host == "" || (!strings.HasPrefix(u, "https://") && !strings.HasPrefix(u, "http://")) {
		return ""
	}
	if strings.ContainsAny(u, " \t\r\n<>`\"'()[]{}|\\^") || len(u) > 500 {
		return ""
	}
	parsed, err := url.Parse(u)
	if err != nil || parsed.User != nil || !strings.EqualFold(parsed.Host, host) {
		return ""
	}
	return u
}

// DependencyBotRedURLHost is the host of a PR's own web URL, the only host
// run links in the issue may point at ("" when it cannot be read).
func DependencyBotRedURLHost(prURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(prURL))
	if err != nil || parsed.User != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	return parsed.Host
}

// DependencyBotRedChecks returns checks trimmed, without blanks or repeats
// (case-insensitive), sorted.
func DependencyBotRedChecks(checks []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range checks {
		c = strings.TrimSpace(c)
		if c == "" || seen[strings.ToLower(c)] {
			continue
		}
		seen[strings.ToLower(c)] = true
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// DependencyBotRedChecksKey is a short stable key for a failing check set:
// the same names in any order or case give the same key.
func DependencyBotRedChecksKey(checks []string) string {
	names := DependencyBotRedChecks(checks)
	for i, n := range names {
		names[i] = strings.ToLower(n)
	}
	sort.Strings(names)
	sum := sha256.Sum256([]byte(strings.Join(names, "\n")))
	return hex.EncodeToString(sum[:])[:12]
}

// DependencyBotRedIssueMarker is the marker line for PR prNumber failing
// checks. The issue lives in the PR's own repository, so the repository is
// implicit.
func DependencyBotRedIssueMarker(prNumber int, checks []string) string {
	return fmt.Sprintf("%spr=%d checks=%s -->", DependencyBotRedIssueMarkerPrefix, prNumber, DependencyBotRedChecksKey(checks))
}

// dependencyBotRedIssueHasMarker reports whether body opens with exactly the
// marker line. Only that anchored position counts, so a marker quoted
// anywhere else (or forged inside PR-supplied text) never matches.
func dependencyBotRedIssueHasMarker(body, marker string) bool {
	first, _, _ := strings.Cut(body, "\n")
	return strings.TrimSpace(first) == marker
}

// DependencyBotRedIssueTitle is the issue title (CreateIssue adds the
// `[governor]` filer tag). The PR number comes first, before any check name,
// so the dedupe canonicalisation (issue_dedupe.go), which cuts a title at
// ": " or " - ", never makes two PRs' issues look alike.
func DependencyBotRedIssueTitle(prNumber int, checks []string) string {
	// The checks key sits right after the PR number: the title lists only the
	// first checks, so two failing sets of one PR could otherwise share a
	// title (and CreateIssue's same-title reuse), and the dedupe stem keeps
	// it because it comes before any check name (a name with ": " cuts the
	// stem there).
	return fmt.Sprintf("Dependency-bot PR #%d [%s] needs a repository change to pass %s", prNumber, DependencyBotRedChecksKey(checks), dependencyBotRedQuoted(checks, dependencyBotRedIssueMaxTitleChecks))
}

func dependencyBotRedQuoted(checks []string, limit int) string {
	names := DependencyBotRedChecks(checks)
	if len(names) == 0 {
		return "its required checks"
	}
	more := ""
	if limit > 0 && len(names) > limit {
		more = fmt.Sprintf(" and %d more", len(names)-limit)
		names = names[:limit]
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		quoted[i] = "`" + DependencyBotRedSafeText(n) + "`"
	}
	return strings.Join(quoted, ", ") + more
}

// DependencyBotRedCrossHeadReproduced is the issue's fixed "Reproduced:"
// wording when the failure counts as reproduced across heads: the same
// checks failed on the PR's previous head prevHead and again on the current
// head after it was brought up to date with the base.
func DependencyBotRedCrossHeadReproduced(prevHead string) string {
	return fmt.Sprintf("the same checks failed on the previous head `%s` and again on this head after it was brought up to date with the base, and no run of these checks passed on this head.",
		DependencyBotRedSafeText(shortSHA(strings.TrimSpace(prevHead))))
}

// DependencyBotRedRun is one failing run linked from the issue.
type DependencyBotRedRun struct {
	Check string
	URL   string
}

// DependencyBotRedFailedJob is where one failing check's job failed, for a
// check with no line to quote: the job's name (said only when it differs
// from the check's) and the steps that concluded failure.
type DependencyBotRedFailedJob struct {
	Check string
	Job   string
	Steps []string
}

// DependencyBotRedIssueFacts is what the issue body says.
type DependencyBotRedIssueFacts struct {
	PRNumber int
	PRTitle  string
	// ChecksURL links the PR's checks when no failing run has a URL.
	ChecksURL string
	// ForgeHost is the only host a run or checks link may point at
	// (DependencyBotRedURLHost of the PR's URL).
	ForgeHost string
	// Bot is the bot's display name ("Renovate", "Dependabot").
	Bot     string
	HeadSHA string
	BaseRef string
	BaseSHA string
	// Checks are the failing required checks the issue is about.
	Checks []string
	Runs   []DependencyBotRedRun
	// ErrorLines are "<check>: <message>" failure-level annotation lines
	// from the newest failing run of each check (DependencyBotRedRunFacts).
	ErrorLines []string
	// FailedJobs are the job and failed step names of checks whose
	// annotations held only the bare exit code (DependencyBotRedJobSteps).
	// The issue never quotes a job's log: it is public, and the log is not.
	// With no ErrorLines it says the job's own log has the details.
	FailedJobs []DependencyBotRedFailedJob
	// WithoutSecrets: the PR's runs cannot see the repository's Actions
	// secrets (a Dependabot PR, a PR from a fork). With no ErrorLines the
	// issue names a missing secret as a possible cause.
	WithoutSecrets bool
	// Regenerated: the bot already rebased the branch once after the
	// hive's stale-red rebase request, and the new head fails the same
	// checks. Otherwise the head already contains BaseSHA.
	Regenerated bool
	// PreviousHeadSHA is set when the failure counts as reproduced across
	// heads: the PR's previous head failed the same checks, and this head,
	// brought up to date with the base since, fails them again. The head
	// may then be behind BaseSHA, so the body says that instead of claiming
	// it contains the tip. MergeBaseSHA is the base commit this head
	// contains (and the previous head did not), where the checks passed.
	PreviousHeadSHA string
	MergeBaseSHA    string
	// Reproduced says how the failure was seen more than once ("Hive
	// re-ran the failed jobs and they failed again", ...).
	Reproduced string
	// AfterFix is what has to happen on the PR once the repository fix is
	// on the base (the same sentence the hand-off comment carries).
	AfterFix string
}

// dependencyBotRedUsefulMessage reports whether an annotation message says
// more than the bare "Process completed with exit code N" every failing step
// leaves.
func dependencyBotRedUsefulMessage(msg string) bool {
	msg = strings.TrimSpace(msg)
	return msg != "" && !exitCodeOnlyAnnotation.MatchString(msg)
}

// DependencyBotRedExcerpt picks up to dependencyBotRedIssueMaxExcerptLines
// distinct annotation lines for the given checks, as "<check>: <message>",
// leaving out exit-code-only lines. The PR's CIFailureExcerpt does not keep
// each annotation's level, so this is only the cheap first filter; the
// failure-level lines the issue quotes come from DependencyBotRedRunFacts.
func DependencyBotRedExcerpt(excerpt string, checks []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, line := range strings.Split(excerpt, "\n") {
		line = strings.TrimSpace(line)
		for _, check := range checks {
			msg, ok := strings.CutPrefix(line, check+": ")
			msg = strings.TrimSpace(msg)
			if !ok || !dependencyBotRedUsefulMessage(msg) || seen[check+"\x00"+msg] {
				continue
			}
			seen[check+"\x00"+msg] = true
			out = append(out, check+": "+msg)
			break
		}
		if len(out) >= dependencyBotRedIssueMaxExcerptLines {
			break
		}
	}
	return out
}

// RenderDependencyBotRedIssueBody is the issue body for f. The marker is its
// first line. Mentions are neutralized by FileDependencyBotRedIssue.
func RenderDependencyBotRedIssueBody(f DependencyBotRedIssueFacts) string {
	checks := DependencyBotRedChecks(f.Checks)
	quoted := dependencyBotRedQuoted(checks, 0)
	base := "the base branch"
	if b := DependencyBotRedSafeText(f.BaseRef); b != "" {
		base = "`" + b + "`"
	}
	bot := DependencyBotRedSafeText(f.Bot)
	if bot == "" {
		bot = "dependency bot"
	}
	var b strings.Builder
	b.WriteString(DependencyBotRedIssueMarker(f.PRNumber, checks) + "\n")
	fmt.Fprintf(&b, "**Summary:** The %s pull request #%d fails %s on its current head, and the same checks pass on the tip of %s. "+
		"The bump most likely needs a matching change in this repository (a build file, a configuration or code that uses the dependency).\n\n",
		bot, f.PRNumber, quoted, base)
	b.WriteString("### Evidence\n\n")
	fmt.Fprintf(&b, "- Pull request: #%d", f.PRNumber)
	if t := DependencyBotRedSafeText(f.PRTitle); t != "" {
		fmt.Fprintf(&b, " (`%s`)", t)
	}
	if f.HeadSHA != "" {
		fmt.Fprintf(&b, ", head `%s`", DependencyBotRedSafeText(shortSHA(f.HeadSHA)))
	}
	b.WriteString("\n")
	fmt.Fprintf(&b, "- Failing required checks: %s\n", quoted)
	var runs []string
	for _, r := range f.Runs {
		if u := DependencyBotRedSafeURL(r.URL, f.ForgeHost); u != "" {
			runs = append(runs, fmt.Sprintf("  - `%s`: %s\n", DependencyBotRedSafeText(r.Check), u))
		}
	}
	switch {
	case len(runs) > 0:
		b.WriteString("- Failing runs:\n")
		for _, r := range runs {
			b.WriteString(r)
		}
	case DependencyBotRedSafeURL(f.ChecksURL, f.ForgeHost) != "":
		fmt.Fprintf(&b, "- Failing runs: see %s\n", DependencyBotRedSafeURL(f.ChecksURL, f.ForgeHost))
	}
	if steps := dependencyBotRedFailedSteps(f.FailedJobs); len(steps) > 0 {
		b.WriteString("- Failed steps:\n")
		for _, s := range steps {
			b.WriteString(s)
		}
	}
	if r := strings.TrimSpace(f.Reproduced); r != "" {
		fmt.Fprintf(&b, "- Reproduced: %s\n", r)
	}
	fmt.Fprintf(&b, "- Base branch: %s", base)
	if f.BaseSHA != "" {
		fmt.Fprintf(&b, " at `%s`", DependencyBotRedSafeText(shortSHA(f.BaseSHA)))
	}
	b.WriteString(", where the newest run of each of these checks passed, starting after this head's failing run started.\n")
	switch {
	case strings.TrimSpace(f.PreviousHeadSHA) != "":
		built := "a base commit"
		if m := DependencyBotRedSafeText(shortSHA(strings.TrimSpace(f.MergeBaseSHA))); m != "" {
			built = "base commit `" + m + "`"
		}
		fmt.Fprintf(&b, "- The previous head `%s` failed the same checks. This head contains %s that the previous head did not, every one of these checks passed on that base commit itself, and it fails them again.\n",
			DependencyBotRedSafeText(shortSHA(strings.TrimSpace(f.PreviousHeadSHA))), built)
	case f.Regenerated:
		b.WriteString("- The bot already regenerated this branch once after Hive asked it to rebase, and the new head fails the same checks.\n")
	default:
		b.WriteString("- The head already contains that base commit, so updating the branch alone would not change the result.\n")
	}
	dependencyBotRedExcerptBlock(&b, "Errors from the newest failing runs (failure annotations):", f.ErrorLines)
	if len(f.ErrorLines) == 0 {
		if len(runs) > 0 {
			b.WriteString("\nThe failing runs left no annotation beyond the exit code; the failing job's own log, linked above, has the details.")
		} else {
			b.WriteString("\nThe failing runs left no annotation beyond the exit code; the check's details page has the full output.")
		}
		if f.WithoutSecrets {
			b.WriteString(" This pull request's runs cannot see the repository's Actions secrets (Dependabot runs get only the Dependabot secrets, and runs from a fork get none), so a secret the workflow needs is a possible cause.")
		}
		b.WriteString("\n")
	}
	b.WriteString("\n### Next step\n\n")
	b.WriteString("Fix the repository so this bump passes; do not edit the bot's branch.")
	if a := strings.TrimSpace(f.AfterFix); a != "" {
		b.WriteString(" " + a)
	}
	b.WriteString("\n\n_Hive files one issue per pull request and set of failing checks. It does not close this issue; it may add one note when the pull request merges, closes or goes green._\n")
	return b.String()
}

// dependencyBotRedExcerptBlock writes lines (at most
// dependencyBotRedIssueMaxExcerptLines) under heading in a text fence, and
// nothing when there are none.
func dependencyBotRedExcerptBlock(b *strings.Builder, heading string, lines []string) {
	if len(lines) == 0 {
		return
	}
	if len(lines) > dependencyBotRedIssueMaxExcerptLines {
		lines = lines[:dependencyBotRedIssueMaxExcerptLines]
	}
	b.WriteString("\n" + heading + "\n\n```text\n")
	for _, line := range lines {
		b.WriteString(sharedCIIssueExcerptLine(line) + "\n")
	}
	b.WriteString("```\n")
}

// dependencyBotRedFailedSteps renders one "Failed steps" sub-bullet per job
// with at least one failed step, every name neutralized.
func dependencyBotRedFailedSteps(jobs []DependencyBotRedFailedJob) []string {
	var out []string
	for _, j := range jobs {
		var steps []string
		for _, st := range j.Steps {
			if t := DependencyBotRedSafeText(st); t != "" {
				steps = append(steps, "`"+t+"`")
			}
		}
		check := DependencyBotRedSafeText(j.Check)
		if len(steps) == 0 || check == "" {
			continue
		}
		name := "`" + check + "`"
		if job := DependencyBotRedSafeText(j.Job); job != "" && !strings.EqualFold(job, check) {
			name += " (job `" + job + "`)"
		}
		out = append(out, fmt.Sprintf("  - %s: %s\n", name, strings.Join(steps, ", ")))
	}
	return out
}

// DependencyBotRedIssueLookup is what one open-issue listing (plus, when
// needed, one closed-issue listing) found in a repository.
type DependencyBotRedIssueLookup struct {
	// Open is the lowest-numbered open issue whose body opens with the
	// marker (0: none), and OpenOwned whether the hive wrote it.
	Open      int
	OpenURL   string
	OpenOwned bool
	// SharedCI is an open `[shared-ci] <check> failing across <repo>`
	// incident for one of the checks (0: none): the failure is shared, not
	// this bump's.
	SharedCI int
	// RecentlyClosed: no open one, and one closed within the refile window.
	RecentlyClosed bool
}

// dependencyBotRedIssueOwned reports whether issue opens with marker and
// one of the hive's own accounts wrote it. A hive that cannot name its own
// accounts (PAT auth with no resolvable login) owns none: fail closed.
func (c *Client) dependencyBotRedIssueOwned(issue *gh.Issue, marker string) bool {
	if issue == nil || !dependencyBotRedIssueHasMarker(issue.GetBody(), marker) {
		return false
	}
	return c.knowsOwnAccounts() && c.isHiveAccount(issue.GetUser())
}

// DependencyBotRedIssueOwnAccountsKnown reports whether the hive can tell
// an issue it wrote from a copy. Without that, nothing is filed: the hive
// could not recognise its own issue again, and a forged marker would read
// as its own.
func (c *Client) DependencyBotRedIssueOwnAccountsKnown() bool {
	return c != nil && c.knowsOwnAccounts()
}

// DependencyBotRedIssueFileAllowed reports, without recording anything,
// whether the write-back gate would admit an issue_file write in repo, so
// a refused gate costs no API call (the stale-red pass checks
// bot_rebase_request the same way).
func (c *Client) DependencyBotRedIssueFileAllowed(repo string) bool {
	if c == nil {
		return false
	}
	return c.WritebackGate().Decide(repo, writeback.Item{HiveAuthored: true}, writeback.KindIssueFile).Allowed
}

// DependencyBotRedCheckRuns is what every run (all attempts) of one check
// on a PR head says.
type DependencyBotRedCheckRuns struct {
	// Reproduced: within one check suite, a failing run of the check started
	// after another failing run of it had completed. Re-run attempts of one
	// workflow run share its suite and run one after another, so that is the
	// same job failing again. Failures in different suites (two workflows
	// reporting the same name) or overlapping in time (a matrix, duplicate
	// names in one attempt) are not.
	Reproduced bool
	// Passed: some run of the check on the head succeeded (a flaky check).
	Passed bool
	// FailureStartedAt is when the newest failing run started (zero when
	// GitHub gave no time).
	FailureStartedAt time.Time
	// Errors are the newest failing run's failure-level annotation
	// messages, credential-shaped text masked, first line only, without
	// exit-code-only lines or warnings.
	Errors []string
	// JobID is the newest failing run's id when GitHub Actions ran it (or
	// GitHub named no app): an Actions check run is its workflow job, with
	// the same id. 0 for a run from other CI. A failed shell step leaves
	// only the bare exit-code annotation; when Errors is empty, the filer
	// names this job's failed steps (DependencyBotRedJobSteps) and compares
	// its log with another PR's failing the same check
	// (DependencyBotRedJobErrorLines), never quoting it.
	JobID int64
}

// dependencyBotRedSequentialFailures reports whether one failing run started
// after another one had completed (both times known).
func dependencyBotRedSequentialFailures(failed []*gh.CheckRun) bool {
	for _, a := range failed {
		done := a.GetCompletedAt().Time
		if done.IsZero() {
			continue
		}
		for _, b := range failed {
			if b != a && b.GetStartedAt().Time.After(done) {
				return true
			}
		}
	}
	return false
}

// DependencyBotRedComparison is what one compare of a base commit with a
// head says: how many commits of the base the head lacks, and the merge
// base (the newest base commit the head contains; "" when GitHub gave
// none).
type DependencyBotRedComparison struct {
	BehindBy  int
	MergeBase string
}

// DependencyBotRedCompare compares baseSHA with headSHA (one call).
func (c *Client) DependencyBotRedCompare(ctx context.Context, repo, baseSHA, headSHA string) (DependencyBotRedComparison, error) {
	if c == nil || c.client == nil {
		return DependencyBotRedComparison{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	cmp, _, err := c.client.Repositories.CompareCommits(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), owner, name, baseSHA, headSHA, &gh.ListOptions{PerPage: 1})
	if err != nil {
		return DependencyBotRedComparison{}, fmt.Errorf("comparing %s/%s@%s with %s: %w", owner, name, shortSHA(baseSHA), shortSHA(headSHA), err)
	}
	return DependencyBotRedComparison{BehindBy: cmp.GetBehindBy(), MergeBase: cmp.GetMergeBaseCommit().GetSHA()}, nil
}

// DependencyBotRedBaseStateAt reads the latest completed check run per name
// on the base commit sha, as StaleRedBaseStateFor does for a branch tip.
// The cross-head rule reads it for the merge base of the PR head, the base
// commit that head actually contains.
func (c *Client) DependencyBotRedBaseStateAt(ctx context.Context, repo, sha string) (StaleRedBaseState, error) {
	if c == nil || c.client == nil {
		return StaleRedBaseState{}, ErrNoGitHubClient
	}
	return c.staleRedBaseStateAt(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), repo, sha)
}

// DependencyBotRedAnyPassed returns the first of checks (in the order
// given) with a run on headSHA that completed with success, "" when none
// did: one listing per check (every attempt). The cross-head rule reads it
// for the previous head, so a flaky check there is no evidence.
func (c *Client) DependencyBotRedAnyPassed(ctx context.Context, repo, headSHA string, checks []string) (string, error) {
	if c == nil || c.client == nil {
		return "", ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	owner, name := c.splitRepo(repo)
	for _, check := range checks {
		opts := &gh.ListCheckRunsOptions{CheckName: gh.Ptr(check), Filter: gh.Ptr("all"), ListOptions: gh.ListOptions{PerPage: 100}}
		runs, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, headSHA, opts)
		if err != nil {
			return "", fmt.Errorf("listing %q runs on %s/%s at %s: %w", check, owner, name, shortSHA(headSHA), err)
		}
		if runs == nil {
			continue
		}
		for _, cr := range runs.CheckRuns {
			if cr != nil && cr.GetName() == check && (cr.GetHeadSHA() == "" || strings.EqualFold(cr.GetHeadSHA(), headSHA)) &&
				cr.GetStatus() == "completed" && cr.GetConclusion() == "success" {
				return check, nil
			}
		}
	}
	return "", nil
}

// DependencyBotRedFinalError reports whether err is GitHub refusing a call
// for good: a 4xx answer other than 429 and other than a rate limit (a 403
// carrying Retry-After, X-RateLimit-Remaining: 0 or a rate-limit message,
// or go-github's rate-limit errors). The red-CI issue filer then stops
// retrying the head. A 5xx, a network error, a rate limit or anything
// unknown is temporary.
func DependencyBotRedFinalError(err error) bool {
	if err == nil || isRateLimitErr(err) {
		return false
	}
	var ghErr *gh.ErrorResponse
	if !errors.As(err, &ghErr) || ghErr.Response == nil {
		return false
	}
	code := ghErr.Response.StatusCode
	switch {
	case code == 429:
		return false
	case code == 403 && dependencyBotRedRateLimited(ghErr):
		return false
	}
	return code >= 400 && code < 500
}

func dependencyBotRedRateLimited(e *gh.ErrorResponse) bool {
	h := e.Response.Header
	if h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0" {
		return true
	}
	return strings.Contains(strings.ToLower(e.Message), "rate limit")
}

// DependencyBotRedNotFound reports whether err is GitHub answering 404 or
// 410: the issue, PR or repository is gone, so a record about it can go.
func DependencyBotRedNotFound(err error) bool {
	return githubStatusError(err, 404) || githubStatusError(err, 410)
}

// DependencyBotRedIssueIsOwn reads issue number in repo and reports whether
// its body opens with marker and the hive wrote it
// (dependencyBotRedIssueOwned). CreateIssue's exact-title reuse returns an
// existing issue without saying who wrote it; this is checked before such an
// issue is linked.
func (c *Client) DependencyBotRedIssueIsOwn(ctx context.Context, repo string, number int, marker string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), owner, name, number)
	if err != nil {
		return false, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	return c.dependencyBotRedIssueOwned(issue, marker), nil
}

// DependencyBotRedRunFacts reads, per check, every run of that check on
// headSHA (filter=all, one page) and the failure-level annotations of its
// newest failing run: two calls per check. The caller bounds the check set.
func (c *Client) DependencyBotRedRunFacts(ctx context.Context, repo, headSHA string, checks []string) (map[string]DependencyBotRedCheckRuns, error) {
	if c == nil || c.client == nil {
		return nil, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	owner, name := c.splitRepo(repo)
	out := map[string]DependencyBotRedCheckRuns{}
	for _, check := range checks {
		opts := &gh.ListCheckRunsOptions{CheckName: gh.Ptr(check), Filter: gh.Ptr("all"), ListOptions: gh.ListOptions{PerPage: 100}}
		runs, _, err := c.client.Checks.ListCheckRunsForRef(ctx, owner, name, headSHA, opts)
		if err != nil {
			return nil, fmt.Errorf("listing %q runs on %s/%s at %s: %w", check, owner, name, shortSHA(headSHA), err)
		}
		var facts DependencyBotRedCheckRuns
		bySuite := map[int64][]*gh.CheckRun{}
		var newest *gh.CheckRun
		if runs != nil {
			for _, cr := range runs.CheckRuns {
				if cr == nil || cr.GetName() != check || (cr.GetHeadSHA() != "" && !strings.EqualFold(cr.GetHeadSHA(), headSHA)) || cr.GetStatus() != "completed" {
					continue
				}
				switch cr.GetConclusion() {
				case "success":
					facts.Passed = true
				case "failure", "action_required":
					if suite := cr.GetCheckSuite().GetID(); suite > 0 {
						bySuite[suite] = append(bySuite[suite], cr)
					}
					if newest == nil || cr.GetStartedAt().Time.After(newest.GetStartedAt().Time) ||
						(cr.GetStartedAt().Time.Equal(newest.GetStartedAt().Time) && cr.GetID() > newest.GetID()) {
						newest = cr
					}
				}
			}
		}
		for _, failed := range bySuite {
			if dependencyBotRedSequentialFailures(failed) {
				facts.Reproduced = true
				break
			}
		}
		if newest != nil {
			facts.FailureStartedAt = newest.GetStartedAt().Time
			if slug := newest.GetApp().GetSlug(); slug == "" || slug == "github-actions" {
				facts.JobID = newest.GetID()
			}
			anns, _, err := c.client.Checks.ListCheckRunAnnotations(ctx, owner, name, newest.GetID(), &gh.ListOptions{PerPage: 50})
			if err != nil {
				return nil, fmt.Errorf("listing annotations of %q on %s/%s: %w", check, owner, name, err)
			}
			seen := map[string]bool{}
			for _, a := range anns {
				if a.GetAnnotationLevel() != "failure" {
					continue
				}
				// Masked before it is cut to its first line and 300 bytes,
				// so neither cut leaves part of a secret (a private-key
				// block spans lines) the scrubber no longer recognises.
				msg := strings.TrimSpace(redactAuditText(a.GetMessage()))
				if nl := strings.IndexByte(msg, '\n'); nl > 0 {
					msg = strings.TrimSpace(msg[:nl])
				}
				if len(msg) > 300 {
					msg = strings.ToValidUTF8(msg[:300], "")
				}
				if !dependencyBotRedUsefulMessage(msg) || seen[msg] {
					continue
				}
				seen[msg] = true
				facts.Errors = append(facts.Errors, msg)
			}
		}
		out[check] = facts
	}
	return out, nil
}

// Bounds for DependencyBotRedJobErrorLines. The log is streamed, never
// held: at most dependencyBotRedJobLogMaxBytes are read, lines past
// dependencyBotRedJobLogMaxLineBytes are cut there, and only the last
// dependencyBotRedJobLogMaxLines distinct error lines are kept (a job fails
// at its end). At most dependencyBotRedJobMaxSteps failed step names are
// kept.
const (
	dependencyBotRedJobLogMaxBytes     = 4 << 20
	dependencyBotRedJobLogMaxLineBytes = 64 << 10
	dependencyBotRedJobLogMaxLines     = dependencyBotRedIssueMaxExcerptLines
	dependencyBotRedJobMaxSteps        = 3
	dependencyBotRedJobLogTimeout      = 30 * time.Second
)

var (
	// dependencyBotRedLogTimestamp is the time GitHub puts before every job
	// log line ("2026-11-25T18:25:31.1234567Z ").
	dependencyBotRedLogTimestamp = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?Z ?`)
	// dependencyBotRedLogANSI is a terminal color or cursor code.
	dependencyBotRedLogANSI = regexp.MustCompile(`\x1b\[[0-9;?]*[ -/]*[@-~]`)
	// dependencyBotRedLogErrorLine is a job log line worth comparing: an
	// error, an assertion, a failed test or a panic, in the spellings test
	// runners and compilers print.
	dependencyBotRedLogErrorLine = regexp.MustCompile(`Error|ERROR|error:|FAILED|failed|::error|panic:|fatal:`)
	// dependencyBotRedDigits is a run of digits, which DependencyBotRedNormalizeLine
	// makes one "0": durations, counts, ids and line numbers vary between
	// runs of the same failure. dependencyBotRedUUID and dependencyBotRedHex
	// (a run of at least 7 hex characters: a commit, a digest, a token) are
	// folded the same way first, so a PR-specific id never makes the same
	// failure read as two.
	dependencyBotRedDigits = regexp.MustCompile(`[0-9]+`)
	dependencyBotRedUUID   = regexp.MustCompile(`\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	dependencyBotRedHex    = regexp.MustCompile(`\b[0-9a-f]{7,}\b`)
	// dependencyBotRedGenericLine is a normalized line every failure of a
	// step prints whatever its cause: it says nothing about the cause, so it
	// never makes two failures one.
	dependencyBotRedGenericLine = regexp.MustCompile(`^(?:` +
		`exit status 0|` +
		`(?:error: )?process completed with exit code 0\.?|` +
		`the operation was canceled\.?|` +
		`error: the process '[^']*' failed with exit code 0\.?|` +
		`error: command failed with exit code 0\.?|` +
		`make(?:\[0\])?: \*\*\* .*error 0` +
		`)$`)
)

// DependencyBotRedNormalizeLine is how two PRs' failure messages or log
// lines are compared: spaces collapsed, lower case, every UUID, every run of
// at least 7 hex characters and every run of digits one "0".
func DependencyBotRedNormalizeLine(s string) string {
	s = strings.ToLower(strings.Join(strings.Fields(s), " "))
	s = dependencyBotRedUUID.ReplaceAllString(s, "0")
	s = dependencyBotRedHex.ReplaceAllString(s, "0")
	return dependencyBotRedDigits.ReplaceAllString(s, "0")
}

// DependencyBotRedGenericLine reports whether a normalized line
// (DependencyBotRedNormalizeLine) is one every failing step prints ("exit
// status 1", "The operation was canceled.", a make "Error 2"), which never
// counts as a shared cause.
func DependencyBotRedGenericLine(normalized string) bool {
	return normalized == "" || dependencyBotRedGenericLine.MatchString(normalized)
}

// DependencyBotRedJobLines is what one failed GitHub Actions job's log says,
// for comparing two PRs' failures only. The lines are never logged, stored
// or posted: an Actions log may carry secrets no scrubber recognises, and
// it is the CI's text, not the hive's.
type DependencyBotRedJobLines struct {
	// Read: a log was read. False when there is no log to read (not an
	// Actions job, the log expired or deleted, no Actions read access).
	Read bool
	// Lines are the last distinct error lines (dependencyBotRedLogLine),
	// normalized (DependencyBotRedNormalizeLine), without the generic ones
	// (DependencyBotRedGenericLine).
	Lines []string
}

// DependencyBotRedJobErrorLines reads the log of GitHub Actions job jobID in
// repo (one call, GET /repos/{owner}/{repo}/actions/jobs/{job_id}/logs,
// which answers with a short-lived storage link; the download itself is not
// a GitHub API call). A job with no log to read (a lasting 4xx on the link,
// or the storage refusing the link) is no error: Read is false. A temporary
// failure (5xx, 429, a rate limit, the network) returns the error, without
// the signed link, so the caller tries again on a later pass.
func (c *Client) DependencyBotRedJobErrorLines(ctx context.Context, repo string, jobID int64) (DependencyBotRedJobLines, error) {
	if c == nil || c.client == nil {
		return DependencyBotRedJobLines{}, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	owner, name := c.splitRepo(repo)
	loc, resp, err := c.client.Actions.GetWorkflowJobLogs(ctx, owner, name, jobID, 1)
	if err != nil {
		if dependencyBotRedNoLog(resp, err) {
			return DependencyBotRedJobLines{}, nil
		}
		// go-github parses the signed Location itself; its parse error
		// names the link, so only the host is kept.
		return DependencyBotRedJobLines{}, fmt.Errorf("reading the log link of job %d on %s/%s: %w", jobID, owner, name, dependencyBotRedSafeErr(err))
	}
	base := c.client.BaseURL
	if loc == nil || (loc.Scheme != "https" && !(base != nil && base.Scheme == "http" && loc.Scheme == "http" && loc.Host == base.Host)) {
		// Not a link the log could be fetched from safely; it will not
		// become one on a later pass.
		return DependencyBotRedJobLines{}, nil
	}
	raw, read, err := dependencyBotRedDownloadLog(ctx, loc)
	if err != nil {
		return DependencyBotRedJobLines{}, fmt.Errorf("downloading the log of job %d on %s/%s: %w", jobID, owner, name, dependencyBotRedSafeErr(err))
	}
	if !read {
		return DependencyBotRedJobLines{}, nil
	}
	out := DependencyBotRedJobLines{Read: true}
	for _, line := range raw {
		if n := DependencyBotRedNormalizeLine(line); !DependencyBotRedGenericLine(n) {
			out.Lines = append(out.Lines, n)
		}
	}
	return out, nil
}

// dependencyBotRedDownloadLog fetches the signed log link loc and returns
// its last error lines (dependencyBotRedLogErrorLines), and whether a log
// was read at all. It asks for the last dependencyBotRedJobLogMaxBytes only
// (a suffix Range): a job fails at its end. Storage that ignores the Range
// answers with the whole log, and then only its first
// dependencyBotRedJobLogMaxBytes are read, so the tail of a larger log is
// not seen; one that refuses it (416) is asked once more without it. The
// client is the hive's shared proxy-trusting transport, so a proxied
// deployment reaches the storage and its egress policy applies, with no
// GitHub token (the link needs none), no redirects and a timeout. An error
// never carries the link: it is signed.
func dependencyBotRedDownloadLog(ctx context.Context, loc *url.URL) ([]string, bool, error) {
	hc := &http.Client{
		Transport:     sharedProxyTrust.sharedTransport(),
		Timeout:       dependencyBotRedJobLogTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	for _, tail := range []bool{true, false} {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, loc.String(), nil)
		if err != nil {
			return nil, false, nil
		}
		if tail {
			req.Header.Set("Range", fmt.Sprintf("bytes=-%d", dependencyBotRedJobLogMaxBytes))
		}
		res, err := hc.Do(req)
		if err != nil {
			return nil, false, dependencyBotRedSafeErr(err)
		}
		lines, done, read, err := dependencyBotRedReadLog(res, tail)
		if done {
			return lines, read, err
		}
	}
	return nil, false, nil
}

// dependencyBotRedReadLog reads one log download response and closes it.
// done is false only for a 416 to the tail request, which is retried
// without the Range. A body that breaks off (a timeout, a cancelled
// context, a reset) is an error, never a complete read: the cut tail is
// where a job's errors are.
func dependencyBotRedReadLog(res *http.Response, tail bool) (lines []string, done, read bool, err error) {
	defer res.Body.Close()
	switch {
	case res.StatusCode == http.StatusPartialContent || res.StatusCode == http.StatusOK:
		// A ranged read's first line may start mid-line.
		lines, err := dependencyBotRedLogErrorLines(io.LimitReader(res.Body, dependencyBotRedJobLogMaxBytes), res.StatusCode == http.StatusPartialContent)
		if err != nil {
			return nil, true, false, dependencyBotRedSafeErr(err)
		}
		return lines, true, true, nil
	case res.StatusCode == http.StatusRequestedRangeNotSatisfiable && tail:
		return nil, false, false, nil
	case res.StatusCode == http.StatusTooManyRequests || res.StatusCode >= 500:
		return nil, true, false, fmt.Errorf("HTTP %d", res.StatusCode)
	}
	// The link expired or was refused: no log.
	return nil, true, false, nil
}

// dependencyBotRedSafeErr keeps a signed link out of an error: when err is
// or wraps a *url.Error, the whole error becomes its operation, the link's
// host and its cause (stripped the same way). Anything else is unchanged.
func dependencyBotRedSafeErr(err error) error {
	var ue *url.Error
	if err == nil || !errors.As(err, &ue) {
		return err
	}
	host := "the log link"
	if u, perr := url.Parse(ue.URL); perr == nil && u.Host != "" {
		host = u.Host
	}
	return fmt.Errorf("%s %s: %w", ue.Op, host, dependencyBotRedSafeErr(ue.Err))
}

// dependencyBotRedNoLog reports whether a failed log-link read is GitHub
// saying for good that there is no log to read: a 4xx other than 429 and
// other than a rate limit. go-github returns the log link's answer as a
// plain error with the response, not an *ErrorResponse, so the status is
// read from resp as well.
func dependencyBotRedNoLog(resp *gh.Response, err error) bool {
	if DependencyBotRedFinalError(err) {
		return true
	}
	if resp == nil || resp.Response == nil || isRateLimitErr(err) {
		return false
	}
	code := resp.StatusCode
	if code == http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "" || resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return false
	}
	return code >= 400 && code < 500
}

// dependencyBotRedLogErrorLines reads a GitHub Actions job log and returns
// its last dependencyBotRedJobLogMaxLines distinct error lines
// (dependencyBotRedLogLine), oldest first. skipFirst drops the first line,
// which a ranged read may have cut. Only io.EOF ends the read cleanly (the
// caller's byte cap ends it the same way); any other read error is
// returned, so a log that broke off is never taken for a whole one.
func dependencyBotRedLogErrorLines(r io.Reader, skipFirst bool) ([]string, error) {
	br := bufio.NewReaderSize(r, dependencyBotRedJobLogMaxLineBytes)
	var keep []string
	seen := map[string]bool{}
	inRun := false
	for first := true; ; first = false {
		raw, more, err := br.ReadLine()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		line := string(raw)
		// An overlong line is cut at the buffer; the rest is dropped.
		for more {
			if _, more, err = br.ReadLine(); err == io.EOF {
				break
			} else if err != nil {
				return nil, err
			}
		}
		if first && skipFirst {
			continue
		}
		msg, ok := dependencyBotRedLogLine(line, &inRun)
		if !ok || seen[msg] {
			continue
		}
		seen[msg] = true
		keep = append(keep, msg)
		if len(keep) > dependencyBotRedJobLogMaxLines {
			keep = keep[1:]
		}
	}
	return keep, nil
}

// dependencyBotRedLogLine cleans one job log line and reports whether it is
// an error line to compare. GitHub's timestamp and color codes are
// stripped. The "Run" group GitHub opens for every step echoes the step's
// script and environment, not its output, so nothing inside it counts (a
// script line such as `|| echo failed` is no failure). Other log commands
// ("##[group]", "##[warning]", "[command]...") do not count; "##[error]"
// lines do, without that prefix. The bare "Process completed with exit code
// N" never counts. The line is cut to 300 bytes.
func dependencyBotRedLogLine(line string, inRun *bool) (string, bool) {
	line = strings.TrimPrefix(line, "\ufeff")
	line = dependencyBotRedLogTimestamp.ReplaceAllString(line, "")
	line = dependencyBotRedLogANSI.ReplaceAllString(line, "")
	line = strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(line, "##[group]Run "):
		*inRun = true
		return "", false
	case strings.HasPrefix(line, "##[endgroup]"):
		*inRun = false
		return "", false
	case *inRun:
		return "", false
	case strings.HasPrefix(line, "##[error]"):
		line = strings.TrimSpace(strings.TrimPrefix(line, "##[error]"))
	case strings.HasPrefix(line, "##["), strings.HasPrefix(line, "[command]"):
		return "", false
	case !dependencyBotRedLogErrorLine.MatchString(line):
		return "", false
	}
	line = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, line)
	line = strings.TrimSpace(line)
	if len(line) > 300 {
		line = strings.TrimSpace(strings.ToValidUTF8(line[:300], ""))
	}
	if !dependencyBotRedUsefulMessage(line) {
		return "", false
	}
	return line, true
}

// DependencyBotRedJobSteps reads GitHub Actions job jobID in repo (one call)
// for its name and the names of up to dependencyBotRedJobMaxSteps steps
// that concluded failure, credential-shaped text masked (the issue body
// neutralizes them further). These come from the workflow file, not the
// job's output. Any failure to read leaves them out: they only help a
// person find the failure.
func (c *Client) DependencyBotRedJobSteps(ctx context.Context, repo string, jobID int64) DependencyBotRedFailedJob {
	if c == nil || c.client == nil {
		return DependencyBotRedFailedJob{}
	}
	owner, name := c.splitRepo(repo)
	job, _, err := c.client.Actions.GetWorkflowJobByID(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), owner, name, jobID)
	if err != nil || job == nil {
		return DependencyBotRedFailedJob{}
	}
	out := DependencyBotRedFailedJob{Job: redactAuditText(strings.TrimSpace(job.GetName()))}
	for _, st := range job.Steps {
		if st == nil || st.GetConclusion() != "failure" || strings.TrimSpace(st.GetName()) == "" {
			continue
		}
		out.Steps = append(out.Steps, redactAuditText(strings.TrimSpace(st.GetName())))
		if len(out.Steps) >= dependencyBotRedJobMaxSteps {
			break
		}
	}
	return out
}

// LookupDependencyBotRedIssue reads the open issues of repo once (newest
// first, up to sharedCIIssueMaxIssuePages pages) for an issue opening with
// marker and for an open `[shared-ci]` incident on any of checks; then,
// when neither is open, the issues closed within the refile window. Any open
// issue opening with the marker counts, whoever wrote it, so a copy never
// leads to a second filing. A failed listing returns the error and the
// caller files nothing.
func (c *Client) LookupDependencyBotRedIssue(ctx context.Context, repo, marker string, checks []string, now time.Time) (DependencyBotRedIssueLookup, error) {
	if c == nil || c.client == nil {
		return DependencyBotRedIssueLookup{}, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	owner, name := c.splitRepo(repo)
	want := map[string]bool{}
	for _, ch := range checks {
		want[strings.ToLower(strings.TrimSpace(ch))] = true
	}
	var out DependencyBotRedIssueLookup
	opts := &gh.IssueListByRepoOptions{State: "open", Sort: "created", Direction: "desc", ListOptions: gh.ListOptions{PerPage: 100}}
	for page := 0; page < sharedCIIssueMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, opts)
		if err != nil {
			return DependencyBotRedIssueLookup{}, fmt.Errorf("listing open issues in %s/%s: %w", owner, name, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() {
				continue
			}
			if check, ok := sharedCIIncidentCheck(issue.GetTitle(), owner+"/"+name); ok && want[check] {
				if out.SharedCI == 0 || issue.GetNumber() < out.SharedCI {
					out.SharedCI = issue.GetNumber()
				}
				continue
			}
			if !dependencyBotRedIssueHasMarker(issue.GetBody(), marker) {
				continue
			}
			if out.Open == 0 || issue.GetNumber() < out.Open {
				out.Open, out.OpenURL, out.OpenOwned = issue.GetNumber(), issue.GetHTMLURL(), c.dependencyBotRedIssueOwned(issue, marker)
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		opts.ListOptions.Page = resp.NextPage
	}
	if out.Open > 0 || out.SharedCI > 0 {
		return out, nil
	}
	closed := &gh.IssueListByRepoOptions{
		State:       "closed",
		Sort:        "updated",
		Direction:   "desc",
		Since:       cacheableSince(now.Add(-dependencyBotRedIssueRefileAfterNotPlanned)),
		ListOptions: gh.ListOptions{PerPage: 100},
	}
	for page := 0; page < sharedCIIssueMaxIssuePages; page++ {
		issues, resp, err := c.client.Issues.ListByRepo(ctx, owner, name, closed)
		if err != nil {
			return DependencyBotRedIssueLookup{}, fmt.Errorf("listing closed issues in %s/%s: %w", owner, name, err)
		}
		for _, issue := range issues {
			if issue == nil || issue.IsPullRequest() || issue.ClosedAt == nil || !dependencyBotRedIssueHasMarker(issue.GetBody(), marker) {
				continue
			}
			wait := dependencyBotRedIssueRefileAfterCompleted
			if strings.EqualFold(issue.GetStateReason(), "not_planned") {
				wait = dependencyBotRedIssueRefileAfterNotPlanned
			}
			if now.Sub(issue.GetClosedAt().Time) < wait {
				out.RecentlyClosed = true
				return out, nil
			}
		}
		if resp == nil || resp.NextPage == 0 {
			break
		}
		closed.ListOptions.Page = resp.NextPage
	}
	return out, nil
}

// dependencyBotRedTimelinePage is the timeline page size
// DependencyBotRedPersonIssue reads (at most two pages).
const dependencyBotRedTimelinePage = 100

// DependencyBotRedPersonIssue is an open issue a person with write access
// filed in the PR's own repository about the PR (0: none).
type DependencyBotRedPersonIssue struct {
	Number int
	URL    string
}

// DependencyBotRedPersonIssueQuery is the PR DependencyBotRedPersonIssue
// looks for issues about.
type DependencyBotRedPersonIssueQuery struct {
	Number int
	// Author is the PR's author; URL its web URL; Since when it was
	// opened. An issue older than Since is not about it.
	Author string
	URL    string
	Since  time.Time
}

// dependencyBotRedTrustedAssociations are the author associations whose
// issue counts as a person's report of the PR's failure: anyone can open an
// issue on a public repository.
var dependencyBotRedTrustedAssociations = map[string]bool{"OWNER": true, "MEMBER": true, "COLLABORATOR": true}

// DependencyBotRedPersonIssue reads the newest events of PR q.Number's
// timeline in repo (dependencyBotRedTimelineNewest: at most two calls) for
// an issue a person already filed about it: a
// `cross-referenced` event whose source is an issue (not a pull request)
// that is open, in the same repository, opened by an OWNER, MEMBER or
// COLLABORATOR who is not the PR's author, not a bot and not the hive,
// created at or after q.Since, and whose title or body names the PR (`#N`,
// alone or as `owner/repo#N`, or the PR's URL). A tracker that only
// mentions the PR in passing from elsewhere, or an issue anyone may open,
// does not count. The newest such issue is returned. A zero q.Since admits
// none. A failed read returns the error and the caller files nothing.
func (c *Client) DependencyBotRedPersonIssue(ctx context.Context, repo string, q DependencyBotRedPersonIssueQuery) (DependencyBotRedPersonIssue, error) {
	if c == nil || c.client == nil {
		return DependencyBotRedPersonIssue{}, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	full := strings.ToLower(owner + "/" + name)
	events, err := c.dependencyBotRedTimelineNewest(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), owner, name, q.Number)
	if err != nil {
		return DependencyBotRedPersonIssue{}, err
	}
	if q.Since.IsZero() || q.Number <= 0 {
		return DependencyBotRedPersonIssue{}, nil
	}
	names := dependencyBotRedNamesPR(full, q)
	var out DependencyBotRedPersonIssue
	var newest time.Time
	for _, event := range events {
		if event == nil || !strings.EqualFold(event.GetEvent(), "cross-referenced") {
			continue
		}
		src := event.GetSource()
		if src == nil || src.Issue == nil || src.Issue.IsPullRequest() || !strings.EqualFold(src.Issue.GetState(), "open") {
			continue
		}
		is := src.Issue
		if r := strings.ToLower(is.GetRepository().GetFullName()); r != full && (r != "" || !strings.HasSuffix(strings.ToLower(is.GetRepositoryURL()), "/repos/"+full)) {
			continue
		}
		u := is.GetUser()
		login := strings.TrimSpace(u.GetLogin())
		if u == nil || login == "" || strings.EqualFold(u.GetType(), "Bot") || strings.HasSuffix(strings.ToLower(login), "[bot]") ||
			strings.EqualFold(login, strings.TrimSpace(q.Author)) || (c.knowsOwnAccounts() && c.isHiveAccount(u)) {
			continue
		}
		if !dependencyBotRedTrustedAssociations[strings.ToUpper(is.GetAuthorAssociation())] {
			continue
		}
		created := is.GetCreatedAt().Time
		if created.IsZero() || created.Before(q.Since) || !names.MatchString(is.GetTitle()+"\n"+is.GetBody()) {
			continue
		}
		if out.Number == 0 || created.After(newest) || (created.Equal(newest) && is.GetNumber() > out.Number) {
			out, newest = DependencyBotRedPersonIssue{Number: is.GetNumber(), URL: is.GetHTMLURL()}, created
		}
	}
	return out, nil
}

// dependencyBotRedTimelineNewest returns the newest events of a PR's
// timeline in at most two calls. GitHub lists the timeline oldest first, so
// a busy PR's first page holds only its oldest events. The first call reads
// page 1 and learns the last page from the Link header; with more pages, the
// second reads the last one. With exactly two pages that covers them all;
// with more, a short last page is read together with page 1, not with the
// page before it, so the bound stays at two calls.
func (c *Client) dependencyBotRedTimelineNewest(ctx context.Context, owner, name string, number int) ([]*gh.Timeline, error) {
	events, resp, err := c.client.Issues.ListIssueTimeline(ctx, owner, name, number, &gh.ListOptions{PerPage: dependencyBotRedTimelinePage})
	if err != nil {
		return nil, fmt.Errorf("listing the timeline of %s/%s#%d: %w", owner, name, number, err)
	}
	if resp == nil || resp.LastPage <= 1 {
		return events, nil
	}
	last, _, err := c.client.Issues.ListIssueTimeline(ctx, owner, name, number, &gh.ListOptions{PerPage: dependencyBotRedTimelinePage, Page: resp.LastPage})
	if err != nil {
		return nil, fmt.Errorf("listing the newest timeline events of %s/%s#%d: %w", owner, name, number, err)
	}
	if resp.LastPage == 2 || len(last) < dependencyBotRedTimelinePage {
		return append(events, last...), nil
	}
	return last, nil
}

// dependencyBotRedNamesPR matches text naming PR q in repository full, not
// followed by another digit: `#N` on its own (not after a word, a path or
// another repository's name), `owner/repo#N` with nothing of a longer name
// before it (so `notacme/app#12` and `other/acme/app#12` do not count), or
// a link to the same number on the PR's host, as a pull request or an
// issue, with or without the scheme or `www.`.
func dependencyBotRedNamesPR(full string, q DependencyBotRedPersonIssueQuery) *regexp.Regexp {
	n := fmt.Sprint(q.Number)
	alts := []string{
		`(?:^|[^\w/.#-])#` + n,
		`(?:^|[^A-Za-z0-9_./-])` + regexp.QuoteMeta(full) + `#` + n,
	}
	if u, err := url.Parse(strings.TrimSpace(q.URL)); err == nil && u.Host != "" {
		host := strings.TrimPrefix(strings.ToLower(u.Host), "www.")
		alts = append(alts, `(?:^|[^A-Za-z0-9_.-])(?:https?://)?(?:www\.)?`+regexp.QuoteMeta(host)+`/`+regexp.QuoteMeta(full)+`/(?:pull|issues)/`+n)
	}
	return regexp.MustCompile(`(?i)(?:` + strings.Join(alts, "|") + `)(?:[^0-9]|$)`)
}

// DependencyBotRedIssueOpen reports whether issue number in repo is open
// (one call). The filer re-checks a person's issue it named this way: once
// that issue closes with the PR still red, the PR is judged again. An issue
// that is gone (404, 410) is not open.
func (c *Client) DependencyBotRedIssueOpen(ctx context.Context, repo string, number int) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(WithRESTCaller(ctx, "hive:dependency_bot_red_issue"), owner, name, number)
	if err != nil {
		if DependencyBotRedNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	return strings.EqualFold(issue.GetState(), "open"), nil
}

// FileDependencyBotRedIssue files the issue in repo through the write-back
// gate (kind issue_file, as every hive-authored new issue) and CreateIssue
// with governor origin. It adds no label of its own; CreateIssue adds the
// origin label every hive issue carries. A gate refusal returns
// ErrDependencyBotRedIssueRefused.
func (c *Client) FileDependencyBotRedIssue(ctx context.Context, repo, title, body string) (CreateIssueResult, error) {
	if c == nil || c.client == nil {
		return CreateIssueResult{}, ErrNoGitHubClient
	}
	if d := c.WritebackAllowed(repo, writeback.Item{HiveAuthored: true}, writeback.KindIssueFile, title); !d.Allowed {
		return CreateIssueResult{}, ErrDependencyBotRedIssueRefused
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	return c.CreateIssue(ctx, CreateIssueRequest{Repo: repo, Title: title, Body: advisory.NeutralizeMentions(body), Origin: config.OriginSourceGovernor}) // writeback:gated, WritebackAllowed consulted above
}

// NoteDependencyBotRedIssue posts body as one comment on issue number in
// repo, only when the issue is open and is one this filer wrote (its body
// opens with marker and, when the hive knows its own accounts, one of them
// opened it), and the write-back gate admits it (kind issue_file, the hive's
// own item). posted is false, with no error, when any of that is not so.
func (c *Client) NoteDependencyBotRedIssue(ctx context.Context, repo string, number int, marker, body string) (bool, error) {
	if c == nil || c.client == nil {
		return false, ErrNoGitHubClient
	}
	ctx = WithRESTCaller(ctx, "hive:dependency_bot_red_issue")
	owner, name := c.splitRepo(repo)
	issue, _, err := c.client.Issues.Get(ctx, owner, name, number)
	if err != nil {
		return false, fmt.Errorf("reading %s/%s#%d: %w", owner, name, number, err)
	}
	if !strings.EqualFold(issue.GetState(), "open") || !c.dependencyBotRedIssueOwned(issue, marker) {
		return false, nil
	}
	item := writeback.Item{HiveAuthored: true, Number: number, Author: safeGetLogin(issue.GetUser()), Labels: issueLabelNames(issue.Labels)}
	if d := c.WritebackAllowed(repo, item, writeback.KindIssueFile, "note on dependency-bot red-CI issue"); !d.Allowed {
		return false, nil
	}
	if err := c.CreateIssueComment(ctx, owner+"/"+name, number, advisory.NeutralizeMentions(body)); err != nil { // writeback:gated, WritebackAllowed consulted above
		return false, err
	}
	return true, nil
}
