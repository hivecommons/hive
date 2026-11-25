package github

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
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
// on a head that already contains that tip, and whose failure reproduced, is
// red because of the bump itself: an upstream change usually needs a
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
	return fmt.Sprintf("Dependency-bot PR #%d needs a repository change to pass %s", prNumber, dependencyBotRedQuoted(checks, dependencyBotRedIssueMaxTitleChecks))
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

// DependencyBotRedRun is one failing run linked from the issue.
type DependencyBotRedRun struct {
	Check string
	URL   string
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
	// Regenerated: the bot already rebased the branch once after the
	// hive's stale-red rebase request, and the new head fails the same
	// checks. Otherwise the head already contains BaseSHA.
	Regenerated bool
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
	if r := strings.TrimSpace(f.Reproduced); r != "" {
		fmt.Fprintf(&b, "- Reproduced: %s\n", r)
	}
	fmt.Fprintf(&b, "- Base branch: %s", base)
	if f.BaseSHA != "" {
		fmt.Fprintf(&b, " at `%s`", DependencyBotRedSafeText(shortSHA(f.BaseSHA)))
	}
	b.WriteString(", where the newest run of each of these checks passed, finishing after this head's failing run started.\n")
	if f.Regenerated {
		b.WriteString("- The bot already regenerated this branch once after Hive asked it to rebase, and the new head fails the same checks.\n")
	} else {
		b.WriteString("- The head already contains that base commit, so updating the branch alone would not change the result.\n")
	}
	if lines := f.ErrorLines; len(lines) > 0 {
		if len(lines) > dependencyBotRedIssueMaxExcerptLines {
			lines = lines[:dependencyBotRedIssueMaxExcerptLines]
		}
		b.WriteString("\nErrors from the newest failing runs (failure annotations):\n\n```text\n")
		for _, line := range lines {
			b.WriteString(sharedCIIssueExcerptLine(line) + "\n")
		}
		b.WriteString("```\n")
	}
	b.WriteString("\n### Next step\n\n")
	b.WriteString("Fix the repository so this bump passes; do not edit the bot's branch.")
	if a := strings.TrimSpace(f.AfterFix); a != "" {
		b.WriteString(" " + a)
	}
	b.WriteString("\n\n_Hive files one issue per pull request and set of failing checks. It does not close this issue; it may add one note when the pull request merges, closes or goes green._\n")
	return b.String()
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
	// messages, first line only, without exit-code-only lines or
	// warnings.
	Errors []string
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
			anns, _, err := c.client.Checks.ListCheckRunAnnotations(ctx, owner, name, newest.GetID(), &gh.ListOptions{PerPage: 50})
			if err != nil {
				return nil, fmt.Errorf("listing annotations of %q on %s/%s: %w", check, owner, name, err)
			}
			seen := map[string]bool{}
			for _, a := range anns {
				if a.GetAnnotationLevel() != "failure" {
					continue
				}
				msg := strings.TrimSpace(a.GetMessage())
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
