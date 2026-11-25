package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// Issues for red dependency-bot PRs whose bump needs a repository change
// (auto_merge.dependency_bot_red_issue).
//
// The red-CI hand-off tells a person that a dependency-bot PR has failed a
// required check on the same head for depBotRedCIGrace. Two cases already
// have a hive answer: the bot is asked to rebase when the base has since
// fixed the check (stale_red_bot_rebase.go), and a trusted bot's head the
// review asked to change is adopted (dependency_bot_adopt.go). Otherwise the
// bump usually needs a companion change in the repository (an upstream
// Makefile or config changed), and a person had to diagnose that and file
// the issue a contributor then fixes.
//
// With the switch on, at the moment the red-CI hand-off is posted, the sweep
// files that issue itself, once per (PR, failing check set), and the hand-off
// names it. It files only on evidence that this bump is at fault, and fails
// closed on anything it cannot prove:
//   - the hive can tell its own issues from copies, the issue_file gate
//     admits the write, and the repository is on GitHub;
//   - the failure was not triaged as transient, external or shared, no other
//     open PR in the snapshot fails any of the same checks, and no open
//     `[shared-ci]` incident names one (a shared cause, such as a secret
//     every bot PR lacks);
//   - every failing required check concluded success on the base tip (not
//     skipped or neutral), in a run that finished after this head's failing
//     run started, and the head already contains that tip;
//   - the failure reproduced: within one check suite (re-run attempts of one
//     workflow run) a failing run started after an earlier failing run had
//     finished, or the hive's transient re-run failed again, and no run of
//     these checks passed on the head;
//   - the newest failing runs left failure-level annotations beyond the bare
//     exit code.
//
// At most depBotRedIssueMaxPerPass PRs per pass do the reads; any other PR is
// handed off at once without the issue, exactly as with the switch off. No
// hand-off ever waits for the filer. The hand-off and its needs-human label
// are unchanged apart from one line naming the issue, and no label is added
// to the PR, so the stale-red pass can still ask the bot to rebase once a fix
// lands on the base (staleRedOwnRedCIHandoff).
//
// Lifecycle: when the PR later merges, closes or goes green, the sweep posts
// one note on the issue it filed (only while that issue is open and is the
// hive's own) and forgets it. It never closes an issue.

// depBotRedIssueMaxPerPass caps the PRs one pass evaluates with GitHub reads
// (and so files for). A PR over the cap is handed off now without the issue.
const depBotRedIssueMaxPerPass = 3

// depBotRedIssueMaxChecks bounds the failing check set the filer reads runs
// for (two calls per check). A wider red is unlikely to be one bump's.
const depBotRedIssueMaxChecks = 5

// depBotRedIssueMaxNotesPerPass caps the lifecycle notes one pass posts, and
// depBotRedIssueMaxStateReadsPerPass the PR state reads the lifecycle makes
// for PRs that left the snapshot.
const (
	depBotRedIssueMaxNotesPerPass      = 5
	depBotRedIssueMaxStateReadsPerPass = 5
)

// Skip reasons logged when the switch is on and no issue is filed.
const (
	depBotRedIssueSkipNotGitHub    = "repo-not-on-github"
	depBotRedIssueSkipOwnUnknown   = "own-accounts-unknown"
	depBotRedIssueSkipWriteback    = "writeback-refused"
	depBotRedIssueSkipTriaged      = "transient-or-external-triage"
	depBotRedIssueSkipBaseRed      = "base-red"
	depBotRedIssueSkipNoChecks     = "no-failing-checks"
	depBotRedIssueSkipTooMany      = "too-many-failing-checks"
	depBotRedIssueSkipOtherPRs     = "same-check-failing-on-other-prs"
	depBotRedIssueSkipRebasePend   = "bot-rebase-pending"
	depBotRedIssueSkipNoExcerpt    = "no-annotation-excerpt"
	depBotRedIssueSkipBudget       = "pass-budget-spent"
	depBotRedIssueSkipNoBase       = "base-unreadable"
	depBotRedIssueSkipBaseVerdict  = "base-not-green"
	depBotRedIssueSkipBaseStale    = "base-proof-older-than-failure"
	depBotRedIssueSkipRebaseDue    = "bot-rebase-due"
	depBotRedIssueSkipNotRepro     = "failure-not-reproduced"
	depBotRedIssueSkipPassed       = "check-passed-on-head"
	depBotRedIssueSkipRunsRead     = "runs-unreadable"
	depBotRedIssueSkipNoErrors     = "no-error-annotation"
	depBotRedIssueSkipLookup       = "issue-lookup-failed"
	depBotRedIssueSkipSharedCI     = "shared-ci-incident-open"
	depBotRedIssueSkipNotOwned     = "marker-issue-not-hive-written"
	depBotRedIssueSkipRecentClosed = "recently-closed"
	depBotRedIssueSkipCreate       = "create-failed"
)

// depBotRedIssueGitHub is what the filer reads and writes. *github.Client
// implements it.
type depBotRedIssueGitHub interface {
	DependencyBotRedIssueOwnAccountsKnown() bool
	DependencyBotRedIssueFileAllowed(repo string) bool
	StaleRedBaseStateFor(ctx context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error)
	StaleRedBehindBy(ctx context.Context, repo, baseSHA, headSHA string) (int, error)
	DependencyBotRedRunFacts(ctx context.Context, repo, headSHA string, checks []string) (map[string]github.DependencyBotRedCheckRuns, error)
	DependencyBotRedIssueIsOwn(ctx context.Context, repo string, number int, marker string) (bool, error)
	LookupDependencyBotRedIssue(ctx context.Context, repo, marker string, checks []string, now time.Time) (github.DependencyBotRedIssueLookup, error)
	FileDependencyBotRedIssue(ctx context.Context, repo, title, body string) (github.CreateIssueResult, error)
	NoteDependencyBotRedIssue(ctx context.Context, repo string, number int, marker, body string) (bool, error)
}

var _ depBotRedIssueGitHub = (*github.Client)(nil)

// depBotRedIssuePolicy is the switch and its GitHub side. The zero value is
// the switch off: the sweep behaves exactly as without it.
type depBotRedIssuePolicy struct {
	Enabled bool
	GitHub  depBotRedIssueGitHub
	// OnGitHub reports whether a repository lives on GitHub
	// (config.RepoOnGitHub); nil means every repository does.
	OnGitHub func(repo string) bool
}

var depBotRedIssueNoFilerOnce sync.Once

// depBotRedIssuePolicyFromConfig builds the live policy; reader is the
// sweep's PR reader (*github.Client).
func depBotRedIssuePolicyFromConfig(cfg *config.Config, reader depBotPRReader, logger *slog.Logger) depBotRedIssuePolicy {
	if cfg == nil || !cfg.AutoMerge.DependencyBotRedIssue.IsEnabled() {
		return depBotRedIssuePolicy{}
	}
	gh, ok := reader.(depBotRedIssueGitHub)
	if !ok {
		depBotRedIssueNoFilerOnce.Do(func() {
			if logger != nil {
				logger.Warn("dependency-bot red CI: auto_merge.dependency_bot_red_issue is on, but this reader cannot file issues; no repository issues will be filed")
			}
		})
		return depBotRedIssuePolicy{}
	}
	return depBotRedIssuePolicy{Enabled: true, GitHub: gh, OnGitHub: cfg.RepoOnGitHub}
}

// depBotRedIssueBase is one base tip read, cached for the pass.
type depBotRedIssueBase struct {
	state  github.StaleRedBaseState
	exists bool
	err    error
}

// depBotRedIssuePass carries one sweep pass's budget and snapshot facts.
type depBotRedIssuePass struct {
	policy depBotRedIssuePolicy
	// failing maps "<owner/repo>\x00<check>" (lower case) to the open PRs
	// in the snapshot failing that check.
	failing map[string][]int
	// states are the closed-PR states the undo pass already read this
	// pass, by store key.
	states map[string]github.PRState
	// bases caches each "<owner/repo>\x00<base ref>" tip read this pass, so
	// PRs on one base share one read of its tip and check runs.
	bases      map[string]depBotRedIssueBase
	evaluated  int
	notes      int
	stateReads int
}

func newDepBotRedIssuePass(p depBotRedIssuePolicy, seen map[string]github.PullRequest) *depBotRedIssuePass {
	pass := &depBotRedIssuePass{policy: p}
	if !pass.on() {
		// Off: nil states, so the undo pass keeps nothing extra.
		return pass
	}
	pass.states = map[string]github.PRState{}
	pass.bases = map[string]depBotRedIssueBase{}
	pass.failing = map[string][]int{}
	for key, pr := range seen {
		repo, _, ok := splitEscalationKey(key)
		if !ok || !pr.HasFailingCheck() {
			continue
		}
		for _, check := range github.DependencyBotRedChecks(pr.FailingChecks) {
			k := strings.ToLower(repo) + "\x00" + strings.ToLower(check)
			pass.failing[k] = append(pass.failing[k], pr.Number)
		}
	}
	return pass
}

func (p *depBotRedIssuePass) on() bool {
	return p != nil && p.policy.Enabled && p.policy.GitHub != nil
}

// otherPRFailing returns another open PR in the snapshot that fails one of
// checks in repo (0: none).
func (p *depBotRedIssuePass) otherPRFailing(repo string, number int, checks []string) int {
	for _, check := range checks {
		for _, n := range p.failing[strings.ToLower(repo)+"\x00"+strings.ToLower(check)] {
			if n != number {
				return n
			}
		}
	}
	return 0
}

// baseFor reads repo's baseRef tip once per pass.
func (p *depBotRedIssuePass) baseFor(ctx context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error) {
	k := strings.ToLower(repo) + "\x00" + baseRef
	if b, ok := p.bases[k]; ok {
		return b.state, b.exists, b.err
	}
	st, exists, err := p.policy.GitHub.StaleRedBaseStateFor(ctx, repo, baseRef)
	if p.bases != nil {
		p.bases[k] = depBotRedIssueBase{state: st, exists: exists, err: err}
	}
	return st, exists, err
}

// depBotRedIssueOutcome is what the filer tells the hand-off: the issue the
// hand-off links (0: none) and the sentence the hand-off gains for it.
type depBotRedIssueOutcome struct {
	Number int
	Line   string
}

// depBotRedIssueChecks is the failing required check set on pr's head: the
// branch-protection list when it is known and names some, else every failing
// check (the hand-off already established that a required one is failing).
func depBotRedIssueChecks(pr github.PullRequest) []string {
	if f := pr.Protection; f != nil && f.RequiredChecksKnown && len(f.FailingRequiredChecks) > 0 {
		return github.DependencyBotRedChecks(f.FailingRequiredChecks)
	}
	return github.DependencyBotRedChecks(pr.FailingChecks)
}

// depBotRedIssueRuns are the failing runs of checks on pr's head with a
// link, from the CI activity the enumeration already read: the newest per
// check.
func depBotRedIssueRuns(pr github.PullRequest, checks []string) []github.DependencyBotRedRun {
	a := pr.CIActivity
	if a == nil || (a.Head != "" && !strings.EqualFold(a.Head, pr.HeadSHA)) {
		return nil
	}
	var out []github.DependencyBotRedRun
	for _, name := range checks {
		url := ""
		var at time.Time
		for _, c := range a.Checks {
			if c.Name != name || strings.TrimSpace(c.URL) == "" || (c.Head != "" && !strings.EqualFold(c.Head, pr.HeadSHA)) {
				continue
			}
			if !strings.EqualFold(c.State, "completed") || (c.Conclusion != "failure" && c.Conclusion != "action_required") {
				continue
			}
			since := time.Time{}
			if c.Since != nil {
				since = *c.Since
			}
			if url == "" || since.After(at) {
				url, at = strings.TrimSpace(c.URL), since
			}
		}
		if url != "" {
			out = append(out, github.DependencyBotRedRun{Check: name, URL: url})
		}
	}
	return out
}

func depBotRedIssueChecksURL(pr github.PullRequest) string {
	u := strings.TrimSpace(pr.URL)
	if u == "" {
		return ""
	}
	return strings.TrimSuffix(u, "/") + "/checks"
}

// depBotRedIssueRegenerated reports that the stale-red pass gave up on this
// PR for these checks: the bot rebased once after its request and the new
// head fails them again, so nothing asks the bot again.
func depBotRedIssueRegenerated(store *depBotConflictStore, key string, pr github.PullRequest, checks []string) bool {
	last, asked := store.staleRedAsk(key)
	return asked && !strings.EqualFold(last.HeadSHA, pr.HeadSHA) && staleRedChecksOverlap(last.Checks, checks)
}

// depBotRedIssueBaseName is pr's base ref, neutralized, in backticks.
func depBotRedIssueBaseName(pr github.PullRequest) string {
	if b := github.DependencyBotRedSafeText(pr.BaseRef); b != "" {
		return "`" + b + "`"
	}
	return "the base branch"
}

// depBotRedIssueAfterFix is what has to happen on the PR once the repository
// fix is on the base, said the same way in the issue and the hand-off. It
// promises nothing the hive does not do: Dependabot is never asked, and
// after a regenerated head the stale-red pass does not ask again.
func depBotRedIssueAfterFix(pr github.PullRequest, regenerated bool) string {
	base := depBotRedIssueBaseName(pr)
	switch {
	case strings.EqualFold(strings.TrimSpace(pr.Author), depBotDependabotAuthor):
		return fmt.Sprintf("Once the fix is on %s, ask Dependabot to rebase the pull request (its rebase command, from an account with push access) so its checks run again; Hive cannot ask Dependabot.", base)
	case depBotRenovateBranch(pr) && regenerated:
		return fmt.Sprintf("Once the fix is on %s, tick the rebase/retry box on the pull request so Renovate rebases it and its checks run again; Hive does not ask again for these checks.", base)
	case depBotRenovateBranch(pr):
		return fmt.Sprintf("Once the fix is on %s, Renovate has to rebase the pull request so its checks run again: Hive may tick its rebase/retry box once when it is allowed to ask the bot; otherwise tick the box by hand.", base)
	}
	return fmt.Sprintf("Once the fix is on %s, have the bot rebase or regenerate the pull request so its checks run again.", base)
}

// dependencyBotRedIssueLine is the line the red-CI hand-off gains when an
// issue covers the PR. The hand-off's own text lists fixing this branch as
// one option; this line says why the issue asks for the fix in the
// repository instead, and what then happens to this PR.
func dependencyBotRedIssueLine(pr github.PullRequest, number int, regenerated bool) string {
	base := depBotRedIssueBaseName(pr)
	why := fmt.Sprintf("its failing checks pass on %s, which this head already contains", base)
	if regenerated {
		why = fmt.Sprintf("its failing checks pass on %s, and the bot's regenerated branch failed them again", base)
	}
	return fmt.Sprintf("Issue #%d asks for that fix in the repository instead of on this branch (%s; a commit on the bot's branch stops the bot updating it, or is dropped when the bot regenerates it). %s",
		number, why, depBotRedIssueAfterFix(pr, regenerated))
}

// issueFor runs where the red-CI hand-off is about to be posted for pr's
// head, after the adoption request did not apply and the hand-off's own
// write-back gate admitted the comment. It returns the issue to name in the
// hand-off: one filed now, one filed for this head on an earlier pass, or an
// open one the hive wrote with the same marker. Anything it cannot prove
// files nothing and returns no issue, and the hand-off posts as it would
// with the switch off. It never holds the hand-off.
func (p *depBotRedIssuePass) issueFor(ctx context.Context, store *depBotConflictStore, repo, key string, pr github.PullRequest, rerun depBotTransientRerun, now time.Time, logger *slog.Logger) depBotRedIssueOutcome {
	if !p.on() || pr.HeadSHA == "" {
		return depBotRedIssueOutcome{}
	}
	skip := func(why string, args ...any) depBotRedIssueOutcome {
		logger.Info("dependency-bot red CI: no repository issue filed",
			append([]any{"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", why}, args...)...)
		return depBotRedIssueOutcome{}
	}
	gh := p.policy.GitHub
	// Cheap refusals first: none of these reads the PR or the base.
	switch {
	case p.policy.OnGitHub != nil && !p.policy.OnGitHub(repo):
		return skip(depBotRedIssueSkipNotGitHub)
	case !gh.DependencyBotRedIssueOwnAccountsKnown():
		// The hive could not tell its own issue from a copy later.
		return skip(depBotRedIssueSkipOwnUnknown)
	case !gh.DependencyBotRedIssueFileAllowed(repo):
		return skip(depBotRedIssueSkipWriteback)
	case rerun.State == depBotRerunOwed || rerun.State == depBotRerunNotRun || pr.TransientCITriaged || pr.ExternalCIDeferred || pr.SharedCIIncident > 0:
		// Triaged as transient, external or shared, and not re-run to a
		// second failure: not shown to be the bump's fault.
		return skip(depBotRedIssueSkipTriaged)
	case len(pr.BaseRedChecks) > 0:
		return skip(depBotRedIssueSkipBaseRed, "base_red_checks", pr.BaseRedChecks)
	}
	checks := depBotRedIssueChecks(pr)
	switch {
	case len(checks) == 0:
		return skip(depBotRedIssueSkipNoChecks)
	case len(checks) > depBotRedIssueMaxChecks:
		return skip(depBotRedIssueSkipTooMany, "checks", len(checks))
	}
	if other := p.otherPRFailing(repo, pr.Number, checks); other > 0 {
		// The same check is red on another open PR: a shared cause (a
		// missing secret, a broken runner), not this bump.
		return skip(depBotRedIssueSkipOtherPRs, "other_pr", other)
	}
	if st, found := store.rebaseState(key); found && !st.RequestedAt.IsZero() && strings.EqualFold(st.HeadSHA, pr.HeadSHA) {
		// The bot was asked to rebase this head; its new head decides.
		return skip(depBotRedIssueSkipRebasePend)
	}
	checksKey := github.DependencyBotRedChecksKey(checks)
	if rec, ok := store.redIssue(key); ok && rec.ChecksKey == checksKey && rec.Issue > 0 && strings.EqualFold(rec.HeadSHA, pr.HeadSHA) {
		// Filed (or found) for this head on an earlier pass whose hand-off
		// did not go out: link it, read nothing. A later head is judged
		// again, and the marker lookup finds the open issue.
		regenerated := depBotRedIssueRegenerated(store, key, pr, checks)
		return depBotRedIssueOutcome{Number: rec.Issue, Line: dependencyBotRedIssueLine(pr, rec.Issue, regenerated)}
	}
	// A first cheap look at the evidence the issue would quote: without a
	// line beyond the bare exit code there is nothing to file.
	if len(github.DependencyBotRedExcerpt(pr.CIFailureExcerpt, checks)) == 0 {
		return skip(depBotRedIssueSkipNoExcerpt)
	}
	if strings.TrimSpace(pr.BaseRef) == "" {
		return skip(depBotRedIssueSkipNoBase)
	}

	// The read-heavy part is budgeted per pass; over the budget the PR is
	// handed off now without the issue.
	if p.evaluated >= depBotRedIssueMaxPerPass {
		return skip(depBotRedIssueSkipBudget)
	}
	p.evaluated++

	base, exists, err := p.baseFor(ctx, repo, pr.BaseRef)
	if err != nil || !exists || base.SHA == "" {
		return skip(depBotRedIssueSkipNoBase, "error", err)
	}
	// Every failing check must have completed green on the base tip. Red
	// there, or never run there, files nothing.
	if verdict, check := github.StaleRedBaseVerdictFor(checks, base); verdict != github.StaleRedBaseClears {
		return skip(depBotRedIssueSkipBaseVerdict, "verdict", string(verdict), "check", check, "base_sha", base.SHA)
	}
	regenerated := depBotRedIssueRegenerated(store, key, pr, checks)
	// The head must contain the passing base tip, regenerated or not: a
	// head behind it may only need a rebase onto that tip, not a repository
	// change (for Renovate the stale-red bot rebase or a person ticking the
	// box; for Dependabot a person asks it to rebase).
	behind, err := gh.StaleRedBehindBy(ctx, repo, base.SHA, pr.HeadSHA)
	if err != nil {
		return skip(depBotRedIssueSkipNoBase, "error", err)
	}
	if behind > 0 {
		return skip(depBotRedIssueSkipRebaseDue, "behind_by", behind, "base_sha", base.SHA, "regenerated", regenerated)
	}

	runs, err := gh.DependencyBotRedRunFacts(ctx, repo, pr.HeadSHA, checks)
	if err != nil {
		return skip(depBotRedIssueSkipRunsRead, "error", err)
	}
	allReproduced := true
	var errorLines []string
	for _, check := range checks {
		r := runs[check]
		if r.Passed {
			// A run of this check passed on this very head: flaky.
			return skip(depBotRedIssueSkipPassed, "check", check)
		}
		allReproduced = allReproduced && r.Reproduced
		// The base proof must be a run that concluded success (not skipped
		// or neutral) and no older than the failure: it finished after the
		// head's newest failing run started.
		done := base.SucceededAt[check]
		if done.IsZero() || r.FailureStartedAt.IsZero() || !done.After(r.FailureStartedAt) {
			return skip(depBotRedIssueSkipBaseStale, "check", check, "base_succeeded_at", done, "failure_started_at", r.FailureStartedAt)
		}
		for _, msg := range r.Errors {
			errorLines = append(errorLines, check+": "+msg)
		}
	}
	reproduced := ""
	switch {
	case rerun.State == depBotRerunDone:
		reproduced = "Hive re-ran the failed jobs of this head once and they failed again, and no run of these checks passed on this head."
	case allReproduced:
		reproduced = "each failing check failed again within one check suite of this head after an earlier failing attempt had finished (a re-run of the same workflow run), and no run of these checks passed on this head."
	default:
		return skip(depBotRedIssueSkipNotRepro)
	}
	if len(errorLines) == 0 {
		return skip(depBotRedIssueSkipNoErrors)
	}

	marker := github.DependencyBotRedIssueMarker(pr.Number, checks)
	found, err := gh.LookupDependencyBotRedIssue(ctx, repo, marker, checks, now)
	switch {
	case err != nil:
		return skip(depBotRedIssueSkipLookup, "error", err)
	case found.SharedCI > 0:
		return skip(depBotRedIssueSkipSharedCI, "incident", found.SharedCI)
	case found.Open > 0 && !found.OpenOwned:
		// An open issue carries the marker but the hive did not write it
		// (a copy, or a forgery): neither link it nor file beside it.
		return skip(depBotRedIssueSkipNotOwned, "issue", found.Open)
	case found.Open > 0:
		store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: checksKey, Checks: checks, Issue: found.Open, URL: found.OpenURL, HeadSHA: pr.HeadSHA, FiledAt: now})
		return depBotRedIssueOutcome{Number: found.Open, Line: dependencyBotRedIssueLine(pr, found.Open, regenerated)}
	case found.RecentlyClosed:
		return skip(depBotRedIssueSkipRecentClosed)
	}

	title := github.DependencyBotRedIssueTitle(pr.Number, checks)
	body := github.RenderDependencyBotRedIssueBody(github.DependencyBotRedIssueFacts{
		PRNumber:    pr.Number,
		PRTitle:     pr.Title,
		ChecksURL:   depBotRedIssueChecksURL(pr),
		ForgeHost:   github.DependencyBotRedURLHost(pr.URL),
		Bot:         dependencyBotDisplayName(pr.Author),
		HeadSHA:     pr.HeadSHA,
		BaseRef:     pr.BaseRef,
		BaseSHA:     base.SHA,
		Checks:      checks,
		Runs:        depBotRedIssueRuns(pr, checks),
		ErrorLines:  errorLines,
		Regenerated: regenerated,
		Reproduced:  reproduced,
		AfterFix:    depBotRedIssueAfterFix(pr, regenerated),
	})
	res, err := gh.FileDependencyBotRedIssue(ctx, repo, title, body)
	switch {
	case errors.Is(err, github.ErrDependencyBotRedIssueRefused):
		return skip(depBotRedIssueSkipWriteback)
	case err != nil:
		logger.Warn("dependency-bot red CI: filing the repository issue failed; handing off without it",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "error", err)
		return skip(depBotRedIssueSkipCreate)
	case res.RejectedTwin || res.Number <= 0:
		// A maintainer recently rejected the same finding.
		return skip(depBotRedIssueSkipRecentClosed, "rejected_issue", res.Number)
	}
	if res.AlreadyExisted {
		// CreateIssue reused an open issue with the same title without
		// saying who wrote it: link it only if it is the hive's own.
		own, err := gh.DependencyBotRedIssueIsOwn(ctx, repo, res.Number, marker)
		if err != nil || !own {
			return skip(depBotRedIssueSkipNotOwned, "issue", res.Number, "error", err)
		}
	}
	store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: checksKey, Checks: checks, Issue: res.Number, URL: res.URL, HeadSHA: pr.HeadSHA, FiledAt: now})
	logger.Info("dependency-bot red CI: repository issue filed",
		"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "issue", res.Number, "checks", checks, "already_existed", res.AlreadyExisted)
	return depBotRedIssueOutcome{Number: res.Number, Line: dependencyBotRedIssueLine(pr, res.Number, regenerated)}
}

// noteDependencyBotRedIssues is the minimal lifecycle: once the PR an issue
// was filed for merges, closes or goes green, it posts one note on the
// issue (open and the hive's own only; NoteDependencyBotRedIssue checks)
// and forgets the record. It never closes the issue. A PR that left the
// snapshot reuses the state the undo pass read for it this pass, and other
// state reads are capped per pass.
func (p *depBotRedIssuePass) noteDependencyBotRedIssues(ctx context.Context, store *depBotConflictStore, reader depBotPRReader, seen map[string]github.PullRequest, logger *slog.Logger) {
	if !p.on() {
		return
	}
	for _, key := range store.redIssueKeys() {
		if p.notes >= depBotRedIssueMaxNotesPerPass {
			return
		}
		rec, ok := store.redIssue(key)
		if !ok {
			continue
		}
		repo, number, ok := splitEscalationKey(key)
		if !ok || rec.Issue <= 0 {
			store.forgetRedIssue(key)
			continue
		}
		var note string
		if pr, visible := seen[key]; visible {
			if !strings.EqualFold(strings.TrimSpace(pr.CIStatus), "success") {
				continue
			}
			note = fmt.Sprintf("The dependency-bot pull request #%d is green now on its current head `%s`. "+
				"If a change in this repository fixed it, this issue can be closed once that pull request merges.", number, stagelabels.ShortSHA(pr.HeadSHA))
		} else {
			st, known := p.states[key]
			if !known {
				if p.stateReads >= depBotRedIssueMaxStateReadsPerPass {
					continue
				}
				p.stateReads++
				var err error
				if st, err = reader.GetPRState(ctx, repo, number); err != nil {
					if github.DependencyBotRedNotFound(err) {
						// The PR or its repository is gone: nothing will
						// ever end this record, so drop it.
						store.forgetRedIssue(key)
						logger.Info("dependency-bot red CI: pull request gone; forgetting its repository issue",
							"repo", repo, "pr", number, "issue", rec.Issue)
					}
					continue
				}
			}
			if !strings.EqualFold(st.State, "closed") {
				continue
			}
			if !st.MergedAt.IsZero() {
				note = fmt.Sprintf("The dependency-bot pull request #%d has merged. If nothing else is left here, this issue can be closed.", number)
			} else {
				note = fmt.Sprintf("The dependency-bot pull request #%d was closed without merging. The bot may open a new one; "+
					"this issue stays open until a person closes it.", number)
			}
		}
		p.notes++
		posted, err := p.policy.GitHub.NoteDependencyBotRedIssue(ctx, repo, rec.Issue, github.DependencyBotRedIssueMarker(number, rec.Checks), note)
		if err != nil {
			if github.DependencyBotRedNotFound(err) {
				// The issue (or its repository) is gone or deleted.
				store.forgetRedIssue(key)
				logger.Info("dependency-bot red CI: repository issue gone; forgetting it",
					"repo", repo, "pr", number, "issue", rec.Issue)
				continue
			}
			logger.Warn("dependency-bot red CI: note on the repository issue failed; will retry next pass",
				"repo", repo, "pr", number, "issue", rec.Issue, "error", err)
			continue
		}
		store.forgetRedIssue(key)
		logger.Info("dependency-bot red CI: repository issue lifecycle note",
			"repo", repo, "pr", number, "issue", rec.Issue, "posted", posted)
	}
}

// depBotRedIssueRecord is the persisted issue for one PR: the failing check
// set it was filed for and its number. One per PR; a new check set that
// gets its own issue replaces it.
type depBotRedIssueRecord struct {
	ChecksKey string    `json:"checks_key"`
	Checks    []string  `json:"checks,omitempty"`
	Issue     int       `json:"issue"`
	URL       string    `json:"url,omitempty"`
	HeadSHA   string    `json:"head_sha,omitempty"`
	FiledAt   time.Time `json:"filed_at"`
}

func (s *depBotConflictStore) redIssue(key string) (depBotRedIssueRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redIssues[key]
	if r == nil {
		return depBotRedIssueRecord{}, false
	}
	return *r, true
}

func (s *depBotConflictStore) redIssueKeys() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.redIssues))
	for k := range s.redIssues {
		out = append(out, k)
	}
	return out
}

func (s *depBotConflictStore) markRedIssue(key string, rec depBotRedIssueRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redIssues == nil {
		s.redIssues = map[string]*depBotRedIssueRecord{}
	}
	s.redIssues[key] = &rec
	s.saveRedIssuesLocked()
}

func (s *depBotConflictStore) forgetRedIssue(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.redIssues[key]; !ok {
		return
	}
	delete(s.redIssues, key)
	s.saveRedIssuesLocked()
}

func depBotRedIssuePath(path string) string {
	if path == "" {
		return ""
	}
	return strings.TrimSuffix(path, ".json") + "-red-issue.json"
}

func loadDepBotRedIssues(path string) map[string]*depBotRedIssueRecord {
	out := map[string]*depBotRedIssueRecord{}
	if path == "" {
		return out
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return out
	}
	var m map[string]*depBotRedIssueRecord
	if json.Unmarshal(data, &m) == nil && m != nil {
		out = m
	}
	return out
}

func (s *depBotConflictStore) saveRedIssuesLocked() {
	path := depBotRedIssuePath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(s.redIssues, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}
