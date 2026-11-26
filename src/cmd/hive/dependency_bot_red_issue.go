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
	"github.com/hivecommons/hive/pkg/escalation"
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
//     open PR in the snapshot fails one of the same checks for what reads
//     as the same cause (sharedCause: both PRs running without the
//     repository's secrets, or the same failure-level annotation message;
//     with no message on one side, logSharedCause compares the two failed
//     jobs' logs at filing time, in memory only),
//     and no open `[shared-ci]` incident names one (a shared cause, such as
//     a secret every bot PR lacks). A check name alone is no shared cause:
//     a catch-all check is red on unrelated PRs for unrelated reasons;
//   - every failing required check concluded success on the base tip (not
//     skipped or neutral), in a run that started after this head's failing
//     run started;
//   - the failure reproduced, one of two ways, and no run of these checks
//     passed on the head:
//     in-suite: the head already contains that base tip, and within one
//     check suite (re-run attempts of one workflow run) a failing run
//     started after an earlier failing run had finished, or the hive's
//     transient re-run failed again;
//     across heads: the same checks failed on the PR's previous head (a
//     different SHA, the head seen red just before this one, not triaged
//     transient, external or shared, and with no passing run of them),
//     whoever pushed this newer head (the bot regenerating it or a hive
//     base refresh); this head contains a base commit (its merge base with
//     the tip) that the previous head lacks; and every failing check passed
//     on that very base commit (crossHeadProof). The head may be behind the
//     current base tip, but a pass on the tip alone is then no proof: the
//     fix may have reached the base after the head was made. The sweep
//     keeps that history itself (depBotRedHeads), because
//     the bot PRs of a busy repository are nearly always behind after the
//     hand-off's 4 hours and a re-run needs Actions write, which an App may
//     not have;
//   - nothing else: the issue quotes the newest failing runs' failure-level
//     annotations beyond the bare exit code; for a check that left only that
//     exit code (a failed shell step), it names the job and its failed steps
//     (jobStepsEvidence) and points at the job's own log, linked, never
//     quoting it.
//
// At most depBotRedIssueMaxPerPass PRs per pass do the reads; any other PR is
// handed off at once without the issue, exactly as with the switch off. No
// hand-off ever waits for the filer. The hand-off and its needs-human label
// are unchanged apart from one line naming the issue, and no label is added
// to the PR, so the stale-red pass can still ask the bot to rebase once a fix
// lands on the base (staleRedOwnRedCIHandoff).
//
// A PR still parked by its own red-CI hand-off for an earlier head gets no
// second hand-off for a newer red head, so the filer also runs once for that
// newer head after the same grace (fileForParkedHead). It files the issue
// only; the PR gets no new comment, and the issue's mention of the PR shows
// in the PR's timeline.
//
// An open issue a member (owner, member or collaborator, not the PR's
// author) filed in the repository about the PR since it opened, naming it,
// is not filed beside: the hand-off names it instead (personIssueLine) and
// it is recorded. That answer is not final: the parked path re-reads the
// issue now and then (recheckPersonIssue), and once it closes with the PR
// still red the head is judged again. The hive never notes on it.
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

// depBotRedIssueMaxJobSteps bounds the failed jobs whose steps one
// evaluation reads (jobStepsEvidence: one call per job). A check past it is
// filed without its step names.
const depBotRedIssueMaxJobSteps = 3

// depBotRedIssueMaxPeerLogs bounds the other PRs whose job log one
// evaluation reads to compare with this PR's (logSharedCause: one call per
// peer, plus one per check for this PR's own log). More peers than that to
// compare is refused: an unread peer may share the cause.
const depBotRedIssueMaxPeerLogs = 2

// depBotRedIssueLogDeadline bounds all the log reads of one evaluation
// together, on top of each download's own timeout; past it the reads fail
// and the head is tried again on a later pass.
const depBotRedIssueLogDeadline = 60 * time.Second

// depBotRedIssuePersonRecheck is how often a recorded person-filed issue is
// read again to see whether it is still open, and
// depBotRedIssueMaxPersonChecksPerPass caps those reads per pass.
const (
	depBotRedIssuePersonRecheck          = time.Hour
	depBotRedIssueMaxPersonChecksPerPass = 5
)

// depBotRedIssueRules is the version of the filer's rules a head's outcome
// was reached under, kept on its red-head record. A head with no issue whose
// record carries an older version is judged once more on the parked path
// (reopenRedHeadForRules). Version 2 stopped counting a check name alone as
// a shared cause and files without a quotable excerpt, so a head refused
// under version 1 (records without a version) for either reason, a final
// answer then, gets the new rules.
const depBotRedIssueRules = 2

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
	depBotRedIssueSkipBudget       = "pass-budget-spent"
	depBotRedIssueSkipNoBase       = "base-unreadable"
	depBotRedIssueSkipBaseVerdict  = "base-not-green"
	depBotRedIssueSkipBaseStale    = "base-proof-older-than-failure"
	depBotRedIssueSkipRebaseDue    = "bot-rebase-due"
	depBotRedIssueSkipNotRepro     = "failure-not-reproduced"
	depBotRedIssueSkipNoMergeBase  = "cross-head-merge-base-unknown"
	depBotRedIssueSkipNoBaseUpdate = "cross-head-no-base-update"
	depBotRedIssueSkipPrevCompare  = "cross-head-previous-head-compare-failed"
	depBotRedIssueSkipLabelNotOwn  = "needs-human-not-the-hand-off-label"
	depBotRedIssueSkipLabelRead    = "label-events-unreadable"
	depBotRedIssueSkipBuiltOnRed   = "cross-head-base-commit-not-green"
	depBotRedIssueSkipPrevPassed   = "cross-head-previous-head-passed"
	depBotRedIssueSkipPassed       = "check-passed-on-head"
	depBotRedIssueSkipRunsRead     = "runs-unreadable"
	depBotRedIssueSkipJobLog       = "job-log-unreadable"
	depBotRedIssueSkipLookup       = "issue-lookup-failed"
	depBotRedIssueSkipSharedCI     = "shared-ci-incident-open"
	depBotRedIssueSkipNotOwned     = "marker-issue-not-hive-written"
	depBotRedIssueSkipRecentClosed = "recently-closed"
	depBotRedIssueSkipTimeline     = "pr-timeline-unreadable"
	depBotRedIssueSkipCreate       = "create-failed"
)

// depBotRedIssueGitHub is what the filer reads and writes. *github.Client
// implements it.
type depBotRedIssueGitHub interface {
	DependencyBotRedIssueOwnAccountsKnown() bool
	DependencyBotRedIssueFileAllowed(repo string) bool
	StaleRedBaseStateFor(ctx context.Context, repo, baseRef string) (github.StaleRedBaseState, bool, error)
	DependencyBotRedCompare(ctx context.Context, repo, baseSHA, headSHA string) (github.DependencyBotRedComparison, error)
	DependencyBotRedBaseStateAt(ctx context.Context, repo, sha string) (github.StaleRedBaseState, error)
	DependencyBotRedAnyPassed(ctx context.Context, repo, headSHA string, checks []string) (string, error)
	StaleRedNeedsHumanLabeledByApp(ctx context.Context, repo string, number int) (bool, error)
	DependencyBotRedRunFacts(ctx context.Context, repo, headSHA string, checks []string) (map[string]github.DependencyBotRedCheckRuns, error)
	DependencyBotRedJobErrorLines(ctx context.Context, repo string, jobID int64) (github.DependencyBotRedJobLines, error)
	DependencyBotRedJobSteps(ctx context.Context, repo string, jobID int64) github.DependencyBotRedFailedJob
	DependencyBotRedIssueIsOwn(ctx context.Context, repo string, number int, marker string) (bool, error)
	LookupDependencyBotRedIssue(ctx context.Context, repo, marker string, checks []string, now time.Time) (github.DependencyBotRedIssueLookup, error)
	DependencyBotRedPersonIssue(ctx context.Context, repo string, q github.DependencyBotRedPersonIssueQuery) (github.DependencyBotRedPersonIssue, error)
	DependencyBotRedIssueOpen(ctx context.Context, repo string, number int) (bool, error)
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
	// in the snapshot failing that check, with what sharedCause compares.
	failing map[string][]depBotRedIssuePeer
	// states are the closed-PR states the undo pass already read this
	// pass, by store key.
	states map[string]github.PRState
	// bases caches each "<owner/repo>\x00<base ref>" tip read this pass, so
	// PRs on one base share one read of its tip and check runs.
	bases        map[string]depBotRedIssueBase
	evaluated    int
	notes        int
	stateReads   int
	personChecks int
}

func newDepBotRedIssuePass(p depBotRedIssuePolicy, seen map[string]github.PullRequest) *depBotRedIssuePass {
	pass := &depBotRedIssuePass{policy: p}
	if !pass.on() {
		// Off: nil states, so the undo pass keeps nothing extra.
		return pass
	}
	pass.states = map[string]github.PRState{}
	pass.bases = map[string]depBotRedIssueBase{}
	pass.failing = map[string][]depBotRedIssuePeer{}
	for key, pr := range seen {
		repo, _, ok := splitEscalationKey(key)
		if !ok || !pr.HasFailingCheck() {
			continue
		}
		for _, check := range github.DependencyBotRedChecks(pr.FailingChecks) {
			k := strings.ToLower(repo) + "\x00" + strings.ToLower(check)
			pass.failing[k] = append(pass.failing[k], depBotRedIssuePeer{
				number:         pr.Number,
				messages:       depBotRedIssueMessages(pr.CIFailureAnnotations, check),
				withoutSecrets: depBotRedIssueWithoutSecrets(pr),
				jobID:          depBotRedIssueRunID(pr, check),
			})
		}
	}
	return pass
}

// depBotRedIssuePeer is one open PR in the snapshot failing a check, with
// what the shared-cause test compares.
type depBotRedIssuePeer struct {
	number int
	// messages are the PR's failure-level annotation messages for the
	// check (depBotRedIssueMessages); none when it left only the exit code.
	messages map[string]bool
	// withoutSecrets: its workflows run without the repository's Actions
	// secrets (depBotRedIssueWithoutSecrets).
	withoutSecrets bool
	// jobID is its failing run of the check, an Actions job id when
	// Actions ran it (0: unknown), whose log logSharedCause may compare.
	jobID int64
}

// depBotRedIssueRunID is the id of pr's failing run of check in the
// snapshot (FailingCheckRunIDs, in FailingChecks' order; 0: unknown).
func depBotRedIssueRunID(pr github.PullRequest, check string) int64 {
	for i, name := range pr.FailingChecks {
		if strings.EqualFold(strings.TrimSpace(name), check) && i < len(pr.FailingCheckRunIDs) {
			return pr.FailingCheckRunIDs[i]
		}
	}
	return 0
}

// depBotRedIssueMessages are the failure-level annotation messages beyond
// the bare exit code that a PR's snapshot (CIFailureAnnotations,
// "<check>: <message>" lines, no warnings) carries for check, normalized
// (github.DependencyBotRedNormalizeLine) so the same message on two PRs
// compares equal, without the generic ones every failing step prints
// ("exit status 1", "The operation was canceled."). It reads nothing.
func depBotRedIssueMessages(annotations, check string) map[string]bool {
	var out map[string]bool
	for _, line := range github.DependencyBotRedExcerpt(annotations, []string{check}) {
		msg := github.DependencyBotRedNormalizeLine(strings.TrimPrefix(line, check+": "))
		if github.DependencyBotRedGenericLine(msg) {
			continue
		}
		if out == nil {
			out = map[string]bool{}
		}
		out[msg] = true
	}
	return out
}

// depBotRedIssueWithoutSecrets reports whether pr's workflows run without
// the repository's Actions secrets, which a push to the base has: a pull
// request from a fork (no secrets) or one Dependabot opened (only the
// Dependabot secrets).
func depBotRedIssueWithoutSecrets(pr github.PullRequest) bool {
	return pr.FromFork || strings.EqualFold(strings.TrimSpace(pr.Author), depBotDependabotAuthor)
}

func (p *depBotRedIssuePass) on() bool {
	return p != nil && p.policy.Enabled && p.policy.GitHub != nil
}

// Bases on which sharedCause or logSharedCause counts another PR's failure
// as pr's cause, logged with the refusal (never the text compared).
const (
	depBotRedIssueSharedMessage   = "same-message"
	depBotRedIssueSharedNoSecrets = "no-message-both-without-repository-secrets"
	depBotRedIssueSharedLogLine   = "same-job-log-error-line"
	depBotRedIssueSharedNoLines   = "no-job-log-error-line-on-either"
	depBotRedIssueSharedOneSided  = "job-log-error-lines-on-one-side-only"
	depBotRedIssueSharedNoLog     = "no-job-log-to-compare"
	depBotRedIssueSharedTooMany   = "too-many-peers-to-compare"
)

// depBotRedIssueComparison is one other PR failing one of the checks whose
// failure the snapshot cannot tell apart from pr's (no failure-level
// message on one side or both): logSharedCause compares the two job logs.
type depBotRedIssueComparison struct {
	check string
	peer  depBotRedIssuePeer
}

// sharedCause is the cheap half of the shared-cause test, from the
// snapshot alone (it reads nothing). It returns another open PR whose
// failure of one of checks in repo reads as the same cause as pr's (0:
// none) and the basis, and the other PRs it cannot judge from the snapshot,
// for logSharedCause. Failing a check of the same name is not enough: a
// catch-all check (one job running a whole test suite) is red on unrelated
// PRs for unrelated reasons. Another PR counts at once when
//   - both PRs carry failure-level annotation messages for the check
//     (CIFailureAnnotations: no warnings, so a deprecation warning every
//     run prints is no shared cause, and no generic line such as "exit
//     status 1") and they share one: the same failure (a missing secret, a
//     broken runner) on both. Different messages are different failures;
//   - or both PRs' workflows run without the repository's secrets (a
//     fork's PR, Dependabot's), whatever their messages say (a missing
//     secret's message often names a PR-specific token or id). A secret
//     those runs lack fails them alike while the base, which has it, stays
//     green.
//
// Any other PR with no message to compare on one side is returned for
// logSharedCause, which compares the two failed jobs' logs once every other
// proof holds.
func (p *depBotRedIssuePass) sharedCause(repo string, pr github.PullRequest, checks []string) (int, string, []depBotRedIssueComparison) {
	without := depBotRedIssueWithoutSecrets(pr)
	var compare []depBotRedIssueComparison
	for _, check := range checks {
		mine := depBotRedIssueMessages(pr.CIFailureAnnotations, check)
		for _, other := range p.failing[strings.ToLower(repo)+"\x00"+strings.ToLower(check)] {
			if other.number == pr.Number {
				continue
			}
			if without && other.withoutSecrets {
				// Both without the repository's secrets: refused whatever
				// the messages say, since a missing secret's message often
				// carries a PR-specific token or id.
				return other.number, depBotRedIssueSharedNoSecrets, nil
			}
			if len(mine) > 0 && len(other.messages) > 0 {
				for msg := range mine {
					if other.messages[msg] {
						return other.number, depBotRedIssueSharedMessage, nil
					}
				}
				continue
			}
			compare = append(compare, depBotRedIssueComparison{check: check, peer: other})
		}
	}
	return 0, "", compare
}

// logSharedCause is the read half of the shared-cause test, for the other
// PRs sharedCause could not judge, at filing time only (after every other
// proof, inside the per-pass budget). For each, it reads the failed job's
// log of the check on this PR (once per check) and on the other PR
// (github.DependencyBotRedJobErrorLines: the last error lines, normalized,
// without generic ones) and counts the other PR as the same cause when
//   - the two logs share an error line (the same curl 404, the same build
//     error);
//   - or either log has no error line (on one side or both): nothing
//     comparable tells the failures apart;
//   - or either log cannot be read (no Actions job, no log, no access):
//     nothing to compare, so it fails closed.
//
// Different error lines on both sides are different failures. More than
// depBotRedIssueMaxPeerLogs other PRs to compare is refused unread, and all
// the reads share one deadline (depBotRedIssueLogDeadline). The
// lines are compared in memory only: never logged, stored or posted (an
// Actions log may carry secrets, and the issue is public). err is a failed
// read the caller retries.
func (p *depBotRedIssuePass) logSharedCause(ctx context.Context, repo string, runs map[string]github.DependencyBotRedCheckRuns, compare []depBotRedIssueComparison) (other int, basis string, err error) {
	if len(compare) == 0 {
		return 0, "", nil
	}
	if len(compare) > depBotRedIssueMaxPeerLogs {
		return compare[0].peer.number, depBotRedIssueSharedTooMany, nil
	}
	ctx, cancel := context.WithTimeout(ctx, depBotRedIssueLogDeadline)
	defer cancel()
	gh := p.policy.GitHub
	mine := map[string]github.DependencyBotRedJobLines{}
	for _, c := range compare {
		own, read := mine[c.check]
		if !read {
			if id := runs[c.check].JobID; id > 0 {
				if own, err = gh.DependencyBotRedJobErrorLines(ctx, repo, id); err != nil {
					return 0, "", err
				}
			}
			mine[c.check] = own
		}
		if !own.Read || c.peer.jobID <= 0 {
			return c.peer.number, depBotRedIssueSharedNoLog, nil
		}
		theirs, err := gh.DependencyBotRedJobErrorLines(ctx, repo, c.peer.jobID)
		if err != nil {
			return 0, "", err
		}
		switch {
		case !theirs.Read:
			return c.peer.number, depBotRedIssueSharedNoLog, nil
		case len(own.Lines) == 0 && len(theirs.Lines) == 0:
			return c.peer.number, depBotRedIssueSharedNoLines, nil
		case len(own.Lines) == 0 || len(theirs.Lines) == 0:
			return c.peer.number, depBotRedIssueSharedOneSided, nil
		}
		seen := map[string]bool{}
		for _, l := range own.Lines {
			seen[l] = true
		}
		for _, l := range theirs.Lines {
			if seen[l] {
				return c.peer.number, depBotRedIssueSharedLogLine, nil
			}
		}
	}
	return 0, "", nil
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
// Retry: no verdict for this head yet, only a temporary refusal (the
// per-pass budget spent, a GitHub call that failed for a reason other than a
// lasting 4xx, own accounts not known yet), so a later pass may judge it
// again (fileForParkedHead retries it, at most depBotRedIssueMaxAttempts
// times; a hand-off does not wait). Budget: the refusal was only the spent
// per-pass budget, which is no attempt of the head's own. Any other outcome
// is final for the head.
type depBotRedIssueOutcome struct {
	Number int
	Line   string
	Retry  bool
	Budget bool
}

// depBotRedIssueMaxAttempts caps the attempts that ended in a temporary
// refusal for one head (the hand-off's and the parked path's together);
// after that the head is judged, so a lasting error cannot take an
// evaluation slot every pass.
const depBotRedIssueMaxAttempts = 3

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
// repository instead, and what then happens to this PR. prevHead is the
// previous head when the failure counted as reproduced across heads ("":
// in-suite).
func dependencyBotRedIssueLine(pr github.PullRequest, number int, regenerated bool, prevHead string) string {
	return dependencyBotRedIssueLineFor(pr, depBotRedIssueRecord{Issue: number, PreviousHead: prevHead}, regenerated)
}

// personIssueLine is the hand-off line when a person already filed an open
// issue in the repository that references the PR: the hive files none
// beside it, and the line says what then happens to this PR, as for the
// hive's own.
func personIssueLine(pr github.PullRequest, number int, regenerated bool) string {
	return fmt.Sprintf("A person already filed #%d, which names this pull request, so Hive files no issue of its own; its failing checks pass on %s, so the fix most likely belongs in the repository, not on the bot's branch. %s",
		number, depBotRedIssueBaseName(pr), depBotRedIssueAfterFix(pr, regenerated))
}

// dependencyBotRedIssueLineFor is the hand-off line for a recorded issue:
// personIssueLine for one a person filed, else the hive issue's line
// (dependencyBotRedIssueLine).
func dependencyBotRedIssueLineFor(pr github.PullRequest, rec depBotRedIssueRecord, regenerated bool) string {
	if rec.PersonFiled {
		return personIssueLine(pr, rec.Issue, regenerated)
	}
	number, prevHead := rec.Issue, rec.PreviousHead
	base := depBotRedIssueBaseName(pr)
	why := fmt.Sprintf("its failing checks pass on %s, which this head already contains", base)
	switch {
	case strings.TrimSpace(prevHead) != "":
		why = fmt.Sprintf("its failing checks pass on %s, and they failed on the previous head `%s` and again on this head after it was brought up to date with the base",
			base, github.DependencyBotRedSafeText(stagelabels.ShortSHA(prevHead)))
	case regenerated:
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
	return p.judge(ctx, store, repo, key, pr, rerun, false, now, logger)
}

// judge is issueFor; parked (fileForParkedHead) adds one read after the
// cheap refusals, inside the per-pass budget: the PR's needs-human label
// events, which must show the App's own label (the sweep's hand-off), not
// one a person took off and put back.
func (p *depBotRedIssuePass) judge(ctx context.Context, store *depBotConflictStore, repo, key string, pr github.PullRequest, rerun depBotTransientRerun, parked bool, now time.Time, logger *slog.Logger) depBotRedIssueOutcome {
	if !p.on() || pr.HeadSHA == "" {
		return depBotRedIssueOutcome{}
	}
	skip := func(why string, args ...any) depBotRedIssueOutcome {
		logger.Info("dependency-bot red CI: no repository issue filed",
			append([]any{"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "reason", why}, args...)...)
		return depBotRedIssueOutcome{}
	}
	retry := func(why string, args ...any) depBotRedIssueOutcome {
		out := skip(why, args...)
		out.Retry = true
		return out
	}
	// failed: a GitHub call failed. A lasting 4xx (not 429, not a rate
	// limit) is final for the head; anything else may pass later.
	failed := func(why string, err error, args ...any) depBotRedIssueOutcome {
		args = append([]any{"error", err}, args...)
		if github.DependencyBotRedFinalError(err) {
			return skip(why, args...)
		}
		return retry(why, args...)
	}
	gh := p.policy.GitHub
	// Cheap refusals first: none of these reads the PR or the base.
	switch {
	case p.policy.OnGitHub != nil && !p.policy.OnGitHub(repo):
		return skip(depBotRedIssueSkipNotGitHub)
	case !gh.DependencyBotRedIssueOwnAccountsKnown():
		// The hive could not tell its own issue from a copy later.
		return retry(depBotRedIssueSkipOwnUnknown)
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
	other, basis, compare := p.sharedCause(repo, pr, checks)
	if other > 0 {
		// Another open PR fails the same check for what reads as the same
		// cause (a missing secret, a broken runner), not this bump.
		return skip(depBotRedIssueSkipOtherPRs, "other_pr", other, "basis", basis)
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
		return depBotRedIssueOutcome{Number: rec.Issue, Line: dependencyBotRedIssueLineFor(pr, rec, regenerated)}
	}
	// No look at the excerpt here: a failed shell step leaves only the bare
	// exit code in its annotations, which says nothing about whose fault
	// the failure is. The proofs below decide; the excerpt is only what the
	// issue quotes.
	if strings.TrimSpace(pr.BaseRef) == "" {
		return skip(depBotRedIssueSkipNoBase)
	}

	// The read-heavy part is budgeted per pass; over the budget the PR is
	// handed off now without the issue.
	if p.evaluated >= depBotRedIssueMaxPerPass {
		out := retry(depBotRedIssueSkipBudget)
		out.Budget = true
		return out
	}
	p.evaluated++

	if parked && !store.staleRedLabelNotOwn(key, pr.HeadSHA) {
		own, err := gh.StaleRedNeedsHumanLabeledByApp(ctx, repo, pr.Number)
		if err != nil {
			return failed(depBotRedIssueSkipLabelRead, err)
		}
		if !own {
			store.rememberStaleRedLabelNotOwn(key, pr.HeadSHA)
			return skip(depBotRedIssueSkipLabelNotOwn)
		}
	} else if parked {
		return skip(depBotRedIssueSkipLabelNotOwn)
	}

	base, exists, err := p.baseFor(ctx, repo, pr.BaseRef)
	if err != nil {
		return failed(depBotRedIssueSkipNoBase, err)
	}
	if !exists || base.SHA == "" {
		return skip(depBotRedIssueSkipNoBase)
	}
	// Every failing check must have completed green on the base tip. Red
	// there, or never run there, files nothing.
	if verdict, check := github.StaleRedBaseVerdictFor(checks, base); verdict != github.StaleRedBaseClears {
		return skip(depBotRedIssueSkipBaseVerdict, "verdict", string(verdict), "check", check, "base_sha", base.SHA)
	}
	regenerated := depBotRedIssueRegenerated(store, key, pr, checks)
	// In-suite, the head must contain the passing base tip, regenerated or
	// not: a head behind it may only need a rebase onto that tip, not a
	// repository change (for Renovate the stale-red bot rebase or a person
	// ticking the box; for Dependabot a person asks it to rebase). Across
	// heads it need not, but then the compare's merge base, the base commit
	// the head does contain, must pass the checks itself (crossHeadProof).
	cmp, err := gh.DependencyBotRedCompare(ctx, repo, base.SHA, pr.HeadSHA)
	if err != nil {
		return failed(depBotRedIssueSkipNoBase, err)
	}
	behind := cmp.BehindBy
	prev, crossHead := store.redHeadBefore(key, pr.HeadSHA, checks)
	if behind > 0 && !crossHead {
		return skip(depBotRedIssueSkipRebaseDue, "behind_by", behind, "base_sha", base.SHA, "regenerated", regenerated)
	}

	runs, err := gh.DependencyBotRedRunFacts(ctx, repo, pr.HeadSHA, checks)
	if err != nil {
		return failed(depBotRedIssueSkipRunsRead, err)
	}
	allReproduced := true
	var errorLines []string
	// quoteless are the failed Actions jobs of checks whose newest failing
	// run left no annotation beyond the exit code, in check order.
	var quoteless []depBotRedIssueJob
	for _, check := range checks {
		r := runs[check]
		if r.Passed {
			// A run of this check passed on this very head: flaky.
			return skip(depBotRedIssueSkipPassed, "check", check)
		}
		allReproduced = allReproduced && r.Reproduced
		// The base proof must be a run that concluded success (not skipped
		// or neutral) and no older than the failure: it started after the
		// head's newest failing run started. A base run that started before
		// the breakage proves nothing about it, however late it finished.
		started := base.SucceededStartedAt[check]
		if base.SucceededAt[check].IsZero() || started.IsZero() || r.FailureStartedAt.IsZero() || !started.After(r.FailureStartedAt) {
			return skip(depBotRedIssueSkipBaseStale, "check", check, "base_started_at", started, "failure_started_at", r.FailureStartedAt)
		}
		for _, msg := range r.Errors {
			errorLines = append(errorLines, check+": "+msg)
		}
		if len(r.Errors) == 0 && r.JobID > 0 {
			quoteless = append(quoteless, depBotRedIssueJob{check: check, id: r.JobID})
		}
	}
	reproduced, prevHead, mergeBase := "", "", ""
	switch {
	case behind == 0 && rerun.State == depBotRerunDone:
		reproduced = "Hive re-ran the failed jobs of this head once and they failed again, and no run of these checks passed on this head."
	case behind == 0 && allReproduced:
		reproduced = "each failing check failed again within one check suite of this head after an earlier failing attempt had finished (a re-run of the same workflow run), and no run of these checks passed on this head."
	case crossHead:
		proof := p.crossHeadProof(ctx, repo, checks, base, cmp, prev)
		switch {
		case proof.err != nil:
			return failed(proof.reason, proof.err, "previous_head", prev.HeadSHA, "merge_base", proof.mergeBase)
		case proof.reason != "":
			return skip(proof.reason, append([]any{"previous_head", prev.HeadSHA, "merge_base", proof.mergeBase}, proof.args...)...)
		}
		reproduced, prevHead, mergeBase = github.DependencyBotRedCrossHeadReproduced(prev.HeadSHA), prev.HeadSHA, proof.mergeBase
	default:
		return skip(depBotRedIssueSkipNotRepro)
	}

	marker := github.DependencyBotRedIssueMarker(pr.Number, checks)
	found, err := gh.LookupDependencyBotRedIssue(ctx, repo, marker, checks, now)
	switch {
	case err != nil:
		return failed(depBotRedIssueSkipLookup, err)
	case found.SharedCI > 0:
		return skip(depBotRedIssueSkipSharedCI, "incident", found.SharedCI)
	case found.Open > 0 && !found.OpenOwned:
		// An open issue carries the marker but the hive did not write it
		// (a copy, or a forgery): neither link it nor file beside it.
		return skip(depBotRedIssueSkipNotOwned, "issue", found.Open)
	case found.Open > 0:
		store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: checksKey, Checks: checks, Issue: found.Open, URL: found.OpenURL, HeadSHA: pr.HeadSHA, PreviousHead: prevHead, FiledAt: now})
		return depBotRedIssueOutcome{Number: found.Open, Line: dependencyBotRedIssueLine(pr, found.Open, regenerated, prevHead)}
	case found.RecentlyClosed:
		return skip(depBotRedIssueSkipRecentClosed)
	}

	// Other PRs failing a check whose snapshot gave nothing to compare:
	// their job logs against this PR's, in memory only. Only the count of
	// lines and the basis are logged.
	if other, basis, err := p.logSharedCause(ctx, repo, runs, compare); err != nil {
		return failed(depBotRedIssueSkipJobLog, err)
	} else if other > 0 {
		return skip(depBotRedIssueSkipOtherPRs, "other_pr", other, "basis", basis, "compared", len(compare))
	}

	// A person may already have filed the issue: one timeline page of the
	// PR. An open issue a member filed about the PR is named instead of
	// filing a second one, and recorded; later passes re-check that it is
	// still open (recheckPersonIssue).
	person, err := gh.DependencyBotRedPersonIssue(ctx, repo, github.DependencyBotRedPersonIssueQuery{
		Number: pr.Number, Author: pr.Author, URL: pr.URL, Since: pr.CreatedAt,
	})
	if err != nil {
		return failed(depBotRedIssueSkipTimeline, err)
	}
	if person.Number > 0 {
		store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: checksKey, Checks: checks, Issue: person.Number, URL: person.URL, HeadSHA: pr.HeadSHA, PreviousHead: prevHead, PersonFiled: true, FiledAt: now, CheckedAt: now})
		logger.Info("dependency-bot red CI: a person already filed an issue for this pull request; naming it instead of filing",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "issue", person.Number, "checks", checks)
		return depBotRedIssueOutcome{Number: person.Number, Line: personIssueLine(pr, person.Number, regenerated)}
	}

	// Only now, with every proof made and an issue about to be filed, the
	// failed steps of the jobs that left nothing to quote.
	failedJobs := p.jobStepsEvidence(ctx, repo, quoteless)
	title := github.DependencyBotRedIssueTitle(pr.Number, checks)
	body := github.RenderDependencyBotRedIssueBody(github.DependencyBotRedIssueFacts{
		PRNumber:        pr.Number,
		PRTitle:         pr.Title,
		ChecksURL:       depBotRedIssueChecksURL(pr),
		ForgeHost:       github.DependencyBotRedURLHost(pr.URL),
		Bot:             dependencyBotDisplayName(pr.Author),
		HeadSHA:         pr.HeadSHA,
		BaseRef:         pr.BaseRef,
		BaseSHA:         base.SHA,
		Checks:          checks,
		Runs:            depBotRedIssueRuns(pr, checks),
		ErrorLines:      errorLines,
		FailedJobs:      failedJobs,
		WithoutSecrets:  depBotRedIssueWithoutSecrets(pr),
		Regenerated:     regenerated,
		PreviousHeadSHA: prevHead,
		MergeBaseSHA:    mergeBase,
		Reproduced:      reproduced,
		AfterFix:        depBotRedIssueAfterFix(pr, regenerated),
	})
	res, err := gh.FileDependencyBotRedIssue(ctx, repo, title, body)
	switch {
	case errors.Is(err, github.ErrDependencyBotRedIssueRefused):
		return skip(depBotRedIssueSkipWriteback)
	case err != nil:
		logger.Warn("dependency-bot red CI: filing the repository issue failed; handing off without it",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "error", err)
		return failed(depBotRedIssueSkipCreate, err)
	case res.RejectedTwin || res.Number <= 0:
		// A maintainer recently rejected the same finding.
		return skip(depBotRedIssueSkipRecentClosed, "rejected_issue", res.Number)
	}
	if res.AlreadyExisted {
		// CreateIssue reused an open issue with the same title without
		// saying who wrote it: link it only if it is the hive's own.
		own, err := gh.DependencyBotRedIssueIsOwn(ctx, repo, res.Number, marker)
		if err != nil {
			return failed(depBotRedIssueSkipNotOwned, err, "issue", res.Number)
		}
		if !own {
			return skip(depBotRedIssueSkipNotOwned, "issue", res.Number)
		}
	}
	store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: checksKey, Checks: checks, Issue: res.Number, URL: res.URL, HeadSHA: pr.HeadSHA, PreviousHead: prevHead, FiledAt: now})
	logger.Info("dependency-bot red CI: repository issue filed",
		"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "issue", res.Number, "checks", checks, "already_existed", res.AlreadyExisted, "previous_head", prevHead,
		"annotation_lines", len(errorLines), "failed_jobs", len(failedJobs))
	return depBotRedIssueOutcome{Number: res.Number, Line: dependencyBotRedIssueLine(pr, res.Number, regenerated, prevHead)}
}

// depBotRedIssueJob is the failed GitHub Actions job of one failing check.
type depBotRedIssueJob struct {
	check string
	id    int64
}

// jobStepsEvidence reads, for the first depBotRedIssueMaxJobSteps of jobs,
// the job's name and failed steps (DependencyBotRedJobSteps), so an issue
// with nothing to quote still says where each job failed. The steps come
// from the workflow, not the job's output; the job's log is never quoted.
// A job read that fails only leaves its steps out.
func (p *depBotRedIssuePass) jobStepsEvidence(ctx context.Context, repo string, jobs []depBotRedIssueJob) []github.DependencyBotRedFailedJob {
	var out []github.DependencyBotRedFailedJob
	for i, j := range jobs {
		if i >= depBotRedIssueMaxJobSteps {
			break
		}
		if fj := p.policy.GitHub.DependencyBotRedJobSteps(ctx, repo, j.id); len(fj.Steps) > 0 {
			fj.Check = j.check
			out = append(out, fj)
		}
	}
	return out
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
		if rec.PersonFiled {
			// A person's issue: the hive posts nothing on it, and only
			// forgets it once the PR is done with.
			store.forgetRedIssue(key)
			logger.Info("dependency-bot red CI: pull request done; forgetting the person-filed issue it named",
				"repo", repo, "pr", number, "issue", rec.Issue)
			continue
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

// recheckPersonIssue re-reads the person-filed issue rec names for pr's
// head, at most every depBotRedIssuePersonRecheck and
// depBotRedIssueMaxPersonChecksPerPass reads per pass (one Issues.Get
// each). Still open: nothing changes. Closed or gone with the PR still red:
// the record is forgotten and the head reopened (pending for the hand-off's
// own head), so the caller judges it again now and may file. A failed read
// keeps the record for a later pass. It reports whether to judge.
func (p *depBotRedIssuePass) recheckPersonIssue(ctx context.Context, store *depBotConflictStore, repo, key string, pr github.PullRequest, rec depBotRedIssueRecord, handoffHead bool, now time.Time, logger *slog.Logger) bool {
	if !rec.CheckedAt.IsZero() && now.Sub(rec.CheckedAt) < depBotRedIssuePersonRecheck {
		return false
	}
	if p.personChecks >= depBotRedIssueMaxPersonChecksPerPass {
		return false
	}
	p.personChecks++
	open, err := p.policy.GitHub.DependencyBotRedIssueOpen(ctx, repo, rec.Issue)
	if err != nil {
		logger.Warn("dependency-bot red CI: reading the person-filed issue failed; will retry",
			"repo", repo, "pr", pr.Number, "issue", rec.Issue, "error", err)
		return false
	}
	if open {
		rec.CheckedAt = now
		store.markRedIssue(key, rec)
		return false
	}
	store.forgetRedIssue(key)
	store.reopenRedHead(key, pr.HeadSHA, handoffHead)
	logger.Info("dependency-bot red CI: the person-filed issue closed with the pull request still red; judging it again",
		"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "issue", rec.Issue)
	return true
}

// depBotRedIssueRecord is the persisted issue for one PR: the failing check
// set it was filed for and its number. One per PR; a new check set that
// gets its own issue replaces it. PreviousHead is set when the failure
// counted as reproduced across heads (the hand-off line says so).
type depBotRedIssueRecord struct {
	ChecksKey    string   `json:"checks_key"`
	Checks       []string `json:"checks,omitempty"`
	Issue        int      `json:"issue"`
	URL          string   `json:"url,omitempty"`
	HeadSHA      string   `json:"head_sha,omitempty"`
	PreviousHead string   `json:"previous_head,omitempty"`
	// PersonFiled: Issue is an open issue a member filed about the PR,
	// named instead of filing; the hive never notes on it. CheckedAt is
	// when it was last seen open (recheckPersonIssue).
	PersonFiled bool      `json:"person_filed,omitempty"`
	FiledAt     time.Time `json:"filed_at"`
	CheckedAt   time.Time `json:"checked_at,omitzero"`
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

// depBotRedIssueFile is the red-issue sidecar: the filed issues and the
// red-head history the cross-head rule reads, per PR key.
type depBotRedIssueFile struct {
	Issues   map[string]*depBotRedIssueRecord `json:"issues,omitempty"`
	RedHeads map[string]*depBotRedHeads       `json:"red_heads,omitempty"`
}

// loadDepBotRedIssueFile reads the sidecar. A file in the first layout (a
// bare map of issue records, before the red-head history) still loads its
// issues. Anything unreadable is empty.
func loadDepBotRedIssueFile(path string) (map[string]*depBotRedIssueRecord, map[string]*depBotRedHeads) {
	issues, heads := map[string]*depBotRedIssueRecord{}, map[string]*depBotRedHeads{}
	if path == "" {
		return issues, heads
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return issues, heads
	}
	var f depBotRedIssueFile
	if json.Unmarshal(data, &f) == nil && (f.Issues != nil || f.RedHeads != nil) {
		if f.Issues != nil {
			issues = f.Issues
		}
		if f.RedHeads != nil {
			heads = f.RedHeads
		}
		return issues, heads
	}
	var m map[string]*depBotRedIssueRecord
	if json.Unmarshal(data, &m) == nil && m != nil {
		for k, r := range m {
			if r != nil && r.Issue > 0 {
				issues[k] = r
			}
		}
	}
	return issues, heads
}

func (s *depBotConflictStore) saveRedIssuesLocked() {
	path := depBotRedIssuePath(s.path)
	if path == "" {
		return
	}
	data, err := json.MarshalIndent(depBotRedIssueFile{Issues: s.redIssues, RedHeads: s.redHeads}, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	tmp := path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, path)
	}
}

// depBotCrossHeadProof is what crossHeadProof found: the merge base the
// head contains on success, or the reason the cross-head path files
// nothing (with err when a GitHub call failed; the caller decides whether a
// later pass may try again).
type depBotCrossHeadProof struct {
	mergeBase string
	reason    string
	err       error
	args      []any
}

// crossHeadProof checks the cross-head evidence the red-head history cannot
// hold, for head pr (B) and its previous red head prev (A), inside the PR's
// per-pass budget:
//
//   - M, the merge base of B with the base tip (from the compare already
//     made), is the newest base commit B contains, the one it was refreshed
//     or regenerated onto;
//   - A lacks M (one compare of M with A): B was brought up to date with a
//     base commit A did not have. A commit stacked on A with no base update
//     is not evidence;
//   - every failing check's newest run on M itself concluded success. When
//     M is the tip, the tip's run already had to start after B's failure
//     started; when B is behind the tip, M's own runs are read (one listing)
//     and a success at any time counts, since it is the very commit B
//     contains. A pass on the tip alone proves nothing then: the fix may
//     have reached the base after B was made;
//   - no run of these checks passed on A (one listing per check), so A's
//     failure was not a flake.
func (p *depBotRedIssuePass) crossHeadProof(ctx context.Context, repo string, checks []string, tip github.StaleRedBaseState, cmp github.DependencyBotRedComparison, prev depBotRedHead) depBotCrossHeadProof {
	gh := p.policy.GitHub
	m := strings.TrimSpace(cmp.MergeBase)
	if m == "" {
		return depBotCrossHeadProof{reason: depBotRedIssueSkipNoMergeBase}
	}
	out := depBotCrossHeadProof{mergeBase: m}
	lacks, err := gh.DependencyBotRedCompare(ctx, repo, m, prev.HeadSHA)
	if err != nil {
		out.reason, out.err = depBotRedIssueSkipPrevCompare, err
		return out
	}
	if lacks.BehindBy == 0 {
		out.reason = depBotRedIssueSkipNoBaseUpdate
		return out
	}
	if !strings.EqualFold(m, tip.SHA) {
		at, err := gh.DependencyBotRedBaseStateAt(ctx, repo, m)
		if err != nil {
			out.reason, out.err = depBotRedIssueSkipNoBase, err
			return out
		}
		for _, check := range checks {
			if at.Red[check] || at.SucceededAt[check].IsZero() {
				out.reason, out.args = depBotRedIssueSkipBuiltOnRed, []any{"check", check}
				return out
			}
		}
	}
	passed, err := gh.DependencyBotRedAnyPassed(ctx, repo, prev.HeadSHA, checks)
	if err != nil {
		out.reason, out.err = depBotRedIssueSkipRunsRead, err
		return out
	}
	if passed != "" {
		out.reason, out.args = depBotRedIssueSkipPrevPassed, []any{"check", passed}
	}
	return out
}

// depBotRedHeadMaxGap bounds the time between the last sighting of a red
// head and the first sighting of the next red head for the two to count as
// successive. A longer gap (the hive down, or the switch off, while other
// heads came and went unseen) breaks the chain: fail closed.
const depBotRedHeadMaxGap = 24 * time.Hour

// depBotRedHeadRefresh is how far a red head's last sighting may advance
// before it is written again, so a head red for hours costs no write per
// pass.
const depBotRedHeadRefresh = time.Hour

// depBotRedHead is one head of a PR seen red on its failing required checks.
type depBotRedHead struct {
	HeadSHA string   `json:"head_sha"`
	Checks  []string `json:"checks"`
	// NotEvidence says why this head's red is not the bump's own on some
	// sighting (transient, external or shared-CI triage, or a check red on
	// the base too); such a head is never the previous head of the
	// cross-head rule. Once set it stays for the head.
	NotEvidence string `json:"not_evidence,omitempty"`
	// SeenAt is when the head was last seen red (to depBotRedHeadRefresh).
	SeenAt time.Time `json:"seen_at"`
}

// depBotRedHeads is the red-head history of one PR, kept only with
// auto_merge.dependency_bot_red_issue on: the newest head seen red and,
// when it directly followed it, the red head before it. It is the evidence
// "the previous head failed the same check" for the cross-head rule.
type depBotRedHeads struct {
	Last depBotRedHead  `json:"last"`
	Prev *depBotRedHead `json:"prev,omitempty"`
	// Next is the first other head seen after Last that was not red yet
	// (CI pending), and Skipped that yet another head followed it: the next
	// red head then did not directly follow Last.
	Next    string `json:"next,omitempty"`
	Skipped bool   `json:"skipped,omitempty"`
	// Judged is the head the filer reached a final outcome for after its
	// hand-off (fileForParkedHead), so it is judged once. Pending is a head
	// whose filer answer was only temporary (budget, a failed call), so
	// fileForParkedHead retries it, and Attempts counts its temporary
	// answers other than the budget (capped by depBotRedIssueMaxAttempts).
	Judged   string `json:"judged,omitempty"`
	Pending  string `json:"pending,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	// Rules is the depBotRedIssueRules version the newest head's outcome
	// was reached under (0: a record from before versions were kept).
	Rules int `json:"rules,omitempty"`
}

// depBotRedHeadNotEvidence is why pr's red is not the bump's own, "" when
// nothing says so.
func depBotRedHeadNotEvidence(pr github.PullRequest) string {
	switch {
	case pr.TransientCITriaged:
		return "transient-triage"
	case pr.ExternalCIDeferred:
		return "external-ci"
	case pr.SharedCIIncident > 0:
		return "shared-ci-incident"
	case len(pr.BaseRedChecks) > 0:
		return "base-red"
	}
	return ""
}

// noteRedHead records h as seen red now. The same head refreshes its checks
// (a reason it is not evidence sticks); a new head becomes Last, with the old
// Last as Prev only when it directly followed it (no other head seen
// between, within depBotRedHeadMaxGap).
func (s *depBotConflictStore) noteRedHead(key string, h depBotRedHead) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.redHeads == nil {
		s.redHeads = map[string]*depBotRedHeads{}
	}
	r := s.redHeads[key]
	switch {
	case r == nil:
		s.redHeads[key] = &depBotRedHeads{Last: h, Rules: depBotRedIssueRules}
	case strings.EqualFold(r.Last.HeadSHA, h.HeadSHA):
		if h.NotEvidence == "" {
			h.NotEvidence = r.Last.NotEvidence
		}
		if github.DependencyBotRedChecksKey(h.Checks) == github.DependencyBotRedChecksKey(r.Last.Checks) &&
			h.NotEvidence == r.Last.NotEvidence && h.SeenAt.Sub(r.Last.SeenAt) < depBotRedHeadRefresh {
			return
		}
		r.Last = h
	default:
		successive := !r.Skipped && (r.Next == "" || strings.EqualFold(r.Next, h.HeadSHA)) &&
			!h.SeenAt.Before(r.Last.SeenAt) && h.SeenAt.Sub(r.Last.SeenAt) <= depBotRedHeadMaxGap
		n := &depBotRedHeads{Last: h, Rules: depBotRedIssueRules}
		if successive {
			prev := r.Last
			n.Prev = &prev
		}
		s.redHeads[key] = n
	}
	s.saveRedIssuesLocked()
}

// noteOtherHead records that headSHA, not red yet, is the PR's current
// head.
func (s *depBotConflictStore) noteOtherHead(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	if r == nil || strings.EqualFold(r.Last.HeadSHA, headSHA) || strings.EqualFold(r.Next, headSHA) || r.Skipped {
		return
	}
	if r.Next == "" {
		r.Next = headSHA
	} else {
		r.Skipped = true
	}
	s.saveRedIssuesLocked()
}

// forgetRedHeads drops key's red-head history (its head went green).
func (s *depBotConflictStore) forgetRedHeads(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.redHeads[key]; !ok {
		return
	}
	delete(s.redHeads, key)
	s.saveRedIssuesLocked()
}

// pruneRedHeads drops the history of PRs not in the open snapshot. A PR
// missing from a partial snapshot loses its history: fail closed.
func (s *depBotConflictStore) pruneRedHeads(open map[string]github.PullRequest) {
	s.mu.Lock()
	defer s.mu.Unlock()
	changed := false
	for k := range s.redHeads {
		if _, ok := open[k]; !ok {
			delete(s.redHeads, k)
			changed = true
		}
	}
	if changed {
		s.saveRedIssuesLocked()
	}
}

// redHeadBefore returns the red head directly before headSHA when it may be
// the cross-head evidence for checks: headSHA is the newest red head, the
// one before it is a different SHA that failed every one of checks, and
// nothing said its red was not the bump's own. Anything else is false.
// crossHeadProof checks the rest against GitHub.
func (s *depBotConflictStore) redHeadBefore(key, headSHA string, checks []string) (depBotRedHead, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	if r == nil || r.Prev == nil || len(checks) == 0 || !strings.EqualFold(r.Last.HeadSHA, headSHA) ||
		strings.TrimSpace(r.Prev.HeadSHA) == "" || strings.EqualFold(r.Prev.HeadSHA, headSHA) || r.Prev.NotEvidence != "" {
		return depBotRedHead{}, false
	}
	failed := map[string]bool{}
	for _, c := range r.Prev.Checks {
		failed[strings.ToLower(strings.TrimSpace(c))] = true
	}
	for _, c := range checks {
		if !failed[strings.ToLower(strings.TrimSpace(c))] {
			return depBotRedHead{}, false
		}
	}
	prev := *r.Prev
	prev.Checks = append([]string(nil), r.Prev.Checks...)
	return prev, true
}

// redHeadsForLocked returns key's history, creating an empty one for
// the head when there is none (a record with no checks is never cross-head
// evidence). Callers hold s.mu.
func (s *depBotConflictStore) redHeadsForLocked(key, headSHA string) *depBotRedHeads {
	if s.redHeads == nil {
		s.redHeads = map[string]*depBotRedHeads{}
	}
	r := s.redHeads[key]
	if r == nil {
		r = &depBotRedHeads{Last: depBotRedHead{HeadSHA: headSHA}, Rules: depBotRedIssueRules}
		s.redHeads[key] = r
	}
	return r
}

// markRedHeadRules records that headSHA's outcome was reached under the
// current rules (the hand-off's own head, whose final outcome is otherwise
// not recorded), so reopenRedHeadForRules leaves it alone.
func (s *depBotConflictStore) markRedHeadRules(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	if r == nil || !strings.EqualFold(r.Last.HeadSHA, headSHA) || r.Rules >= depBotRedIssueRules {
		return
	}
	r.Rules = depBotRedIssueRules
	s.saveRedIssuesLocked()
}

// reopenRedHeadForRules lets headSHA, the PR's newest red head, be judged
// once more when its outcome was reached under older rules (Rules below
// depBotRedIssueRules): a final answer then (Judged) is cleared, and the
// hand-off's own head (handoffHead), whose final answer is not recorded at
// all, is marked pending, with its attempts started over. The record then
// carries the current rules, so this happens once per head. The caller
// checks first that no issue is recorded for the head, so nothing filed is
// filed again; the judgement keeps its per-pass budget and attempt cap.
func (s *depBotConflictStore) reopenRedHeadForRules(key, headSHA string, handoffHead bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	if r == nil || r.Rules >= depBotRedIssueRules || !strings.EqualFold(r.Last.HeadSHA, headSHA) {
		return false
	}
	s.reopenRedHeadLocked(r, headSHA, handoffHead)
	return true
}

// reopenRedHead lets headSHA be judged again (a person-filed issue it was
// answered with closed): reopenRedHeadForRules without the rules check.
func (s *depBotConflictStore) reopenRedHead(key, headSHA string, handoffHead bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reopenRedHeadLocked(s.redHeadsForLocked(key, headSHA), headSHA, handoffHead)
}

// reopenRedHeadLocked clears a final answer for headSHA, marks the
// hand-off's own head pending with its attempts started over, and stamps
// the current rules. Callers hold s.mu.
func (s *depBotConflictStore) reopenRedHeadLocked(r *depBotRedHeads, headSHA string, handoffHead bool) {
	r.Rules = depBotRedIssueRules
	if strings.EqualFold(r.Judged, headSHA) {
		r.Judged = ""
	}
	if handoffHead || strings.EqualFold(r.Pending, headSHA) {
		r.Pending, r.Attempts = headSHA, 0
	}
	s.saveRedIssuesLocked()
}

// redHeadJudged reports whether the filer reached a final outcome for
// headSHA after its hand-off.
func (s *depBotConflictStore) redHeadJudged(key, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	return r != nil && r.Judged != "" && strings.EqualFold(r.Judged, headSHA)
}

// markRedHeadJudged records a final outcome for headSHA (and clears a
// pending retry of it).
func (s *depBotConflictStore) markRedHeadJudged(key, headSHA string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeadsForLocked(key, headSHA)
	r.Judged = headSHA
	if strings.EqualFold(r.Pending, headSHA) {
		r.Pending, r.Attempts = "", 0
	}
	if strings.EqualFold(r.Last.HeadSHA, headSHA) {
		r.Rules = depBotRedIssueRules
	}
	s.saveRedIssuesLocked()
}

// redHeadPending reports whether headSHA's hand-off went out with only a
// temporary answer from the filer.
func (s *depBotConflictStore) redHeadPending(key, headSHA string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeads[key]
	return r != nil && r.Pending != "" && strings.EqualFold(r.Pending, headSHA)
}

// markRedHeadPending records that headSHA's filer answer was temporary.
// attempt counts it against depBotRedIssueMaxAttempts (a spent per-pass
// budget is no attempt). It reports whether the head has now used its
// attempts: it is then judged, and nothing retries it.
func (s *depBotConflictStore) markRedHeadPending(key, headSHA string, attempt bool) (exhausted bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := s.redHeadsForLocked(key, headSHA)
	if strings.EqualFold(r.Last.HeadSHA, headSHA) {
		r.Rules = depBotRedIssueRules
	}
	if !strings.EqualFold(r.Pending, headSHA) {
		r.Pending, r.Attempts = headSHA, 0
	} else if !attempt {
		return false
	}
	if attempt {
		r.Attempts++
	}
	if r.Attempts >= depBotRedIssueMaxAttempts {
		r.Judged, r.Pending, r.Attempts = headSHA, "", 0
		exhausted = true
	}
	s.saveRedIssuesLocked()
	return exhausted
}

// retryRedHead records a temporary filer answer for headSHA and logs, once,
// when that used up the head's attempts.
func retryRedHead(store *depBotConflictStore, repo, key string, pr github.PullRequest, out depBotRedIssueOutcome, logger *slog.Logger) {
	if store.markRedHeadPending(key, pr.HeadSHA, !out.Budget) {
		logger.Warn("dependency-bot red CI: giving up on a repository issue for this head after repeated temporary failures",
			"repo", repo, "pr", pr.Number, "head", pr.HeadSHA, "attempts", depBotRedIssueMaxAttempts)
	}
}

// noteRedHeads keeps the red-head history for every dependency-bot PR in
// the snapshot, before any hand-off is judged this pass: a settled red head
// with failing required checks is recorded (marked not evidence when it was
// triaged transient, external or shared, or a check is red on the base
// too), a head still in CI is noted as the next head, and a green head
// clears the history (the next red head starts a new chain). It reads
// nothing from GitHub. With the switch off it does nothing.
func (p *depBotRedIssuePass) noteRedHeads(store *depBotConflictStore, seen map[string]github.PullRequest, now time.Time) {
	if !p.on() || store == nil {
		return
	}
	store.pruneRedHeads(seen)
	for key, pr := range seen {
		if strings.TrimSpace(pr.HeadSHA) == "" || pr.Draft || (pr.State != "" && !strings.EqualFold(pr.State, "open")) {
			continue
		}
		if !github.IsDependencyBotAuthor(pr.Author) && !github.DependencyBotBranch(pr.HeadRef, pr.Author) {
			continue
		}
		switch {
		case strings.EqualFold(strings.TrimSpace(pr.CIStatus), "success"):
			store.forgetRedHeads(key)
		case pr.HasFailingRequiredCheck() && !pr.CIChecksRunning:
			if checks := depBotRedIssueChecks(pr); len(checks) > 0 {
				store.noteRedHead(key, depBotRedHead{HeadSHA: pr.HeadSHA, Checks: checks, NotEvidence: depBotRedHeadNotEvidence(pr), SeenAt: now})
			}
		default:
			store.noteOtherHead(key, pr.HeadSHA)
		}
	}
}

// fileForParkedHead runs the filer for a dependency-bot PR that is still
// parked by the sweep's own red-CI hand-off (reason red-ci, the label the
// sweep applied) and red: on a newer head than the hand-off's, or on the
// hand-off's own head when the filer only had a temporary answer then
// (budget, a failed read). The hand-off ends only when the PR goes green,
// so without this the newer head, the one the cross-head rule is about,
// would never be judged. A newer head waits the same red-CI grace and
// transient re-run as the hand-off. After the filer's cheap refusals and
// inside the per-pass budget, it reads the label events on every attempt
// (as the stale-red pass does, keeping only a "not own" answer): a
// needs-human a person took off and put back is not the sweep's, and
// nothing is filed. A temporary answer is retried on a later pass, at most
// depBotRedIssueMaxAttempts times per head (the spent budget does not
// count); a final one is recorded so the head is judged once. It files the
// issue and posts nothing on the PR. It reports whether the head's grace
// clock must be kept.
func (p *depBotRedIssuePass) fileForParkedHead(ctx context.Context, store *depBotConflictStore, repo, key string, pr github.PullRequest, now time.Time, logger *slog.Logger) bool {
	if !p.on() || store == nil || strings.TrimSpace(pr.HeadSHA) == "" || !escalation.HasNeedsHumanLabel(pr.Labels) ||
		!dependencyBotHandoffCandidate(staleRedWithoutNeedsHuman(pr)) || !dependencyBotRedCI(pr) {
		return false
	}
	rec, ok := store.get(key)
	if !ok || rec.Reason != depBotHandoffRedCI || !rec.LabelApplied {
		return false
	}
	sameHead := strings.EqualFold(rec.HeadSHA, pr.HeadSHA)
	if r, ok := store.redIssue(key); ok && r.Issue > 0 && strings.EqualFold(r.HeadSHA, pr.HeadSHA) {
		if !r.PersonFiled || !p.recheckPersonIssue(ctx, store, repo, key, pr, r, sameHead, now, logger) {
			return false
		}
	}
	// A head with no issue whose answer came from older rules (one refused
	// for a check name alone or for no annotation excerpt) is judged once
	// more under these.
	store.reopenRedHeadForRules(key, pr.HeadSHA, sameHead)
	if sameHead && !store.redHeadPending(key, pr.HeadSHA) {
		return false
	}
	if store.redHeadJudged(key, pr.HeadSHA) || store.staleRedLabelNotOwn(key, pr.HeadSHA) {
		return false
	}
	rerun := depBotTransientRerunFor(dbrLedger(), repo, pr)
	if !sameHead {
		red := now.Sub(store.observe(key, pr.HeadSHA, depBotHandoffRedCI, now))
		var owed time.Duration
		if rerun.State == depBotRerunOwed {
			owed = now.Sub(store.rerunOwedSince(key, pr.HeadSHA, now))
		}
		if red < depBotRedCIGrace || rerun.holdsHandoff(red, owed) {
			return true
		}
	}
	if out := p.judge(ctx, store, repo, key, pr, rerun, true, now, logger); out.Retry {
		retryRedHead(store, repo, key, pr, out, logger)
		return !store.redHeadJudged(key, pr.HeadSHA)
	}
	store.markRedHeadJudged(key, pr.HeadSHA)
	return false
}
