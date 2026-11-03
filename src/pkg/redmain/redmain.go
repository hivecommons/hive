// Package redmain is the red-main circuit breaker (hivecommons/hive#13630,
// RFC #13617 P3).
//
// Hive already knows a PR is red only because its base branch is red
// (github.PullRequest.BaseRedChecks) and keeps such PRs out of the repair
// lanes. What it did not know is how LONG the default branch has been red.
// When a required check stays red on a repository's default branch for more
// than the per-repo threshold (DefaultThreshold, 6h), this package sets a
// red-main Fact for the repo. While the fact is set:
//
//   - open PRs that fix main (FixesMain: the fixes-main label, a closing
//     reference to the red-main tracking issue, or changed files that overlap
//     the failing check) rank first in the review queue;
//   - the hive refuses NEW PR-open requests in that repo unless the request
//     itself fixes main (existing PRs keep going; filing issues is not
//     affected);
//   - the repo card shows "main red since <time> on <check>; fix: #N waiting
//     on <who>".
//
// The fact clears on the first observation in which the check is green
// again (a completed run that is not red), which lifts the pause and the
// ordering with nothing to undo. A check whose run on the tip has not
// completed yet is neither red nor green: it keeps its previous state, so a
// push to a red branch does not restart the clock. Trust
// levels, the trusted-merger definition and the self-merge floor are not
// read or changed here.
package redmain

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

// DefaultThreshold is how long a required check must stay red on the
// default branch before the circuit breaker trips, when the repo sets no
// red_main_threshold of its own (maintainer decision on #13617).
const DefaultThreshold = 6 * time.Hour

// FixesMainLabel is the explicit label a maintainer or agent puts on a PR to
// mark it as the fix for a red default branch.
const FixesMainLabel = "fixes-main"

// Ways a PR is detected as fixing main (FixPR.Via).
const (
	ViaLabel         = "label"
	ViaTrackingIssue = "tracking-issue"
	ViaFileOverlap   = "file-overlap"
)

// RedRun is one required check currently red on a default branch tip.
type RedRun struct {
	// URL is the failing check run's page.
	URL string
	// CompletedAt is when that run concluded; zero when unknown. It bounds
	// how far back the red is dated on first sight.
	CompletedAt time.Time
	// TrackingIssue is the open `[shared-ci] <check> failing across
	// <owner/repo>` issue for this check, 0 when none is open.
	TrackingIssue int
	// Paths are the files the failing run's annotations point at (the
	// failing tests), used for file-overlap fixes-main detection.
	Paths []string
}

// FixPR is an open PR detected as fixing a red default branch.
type FixPR struct {
	Number int    `json:"number"`
	URL    string `json:"url,omitempty"`
	Via    string `json:"via"`
	// Confirmed is set when the PR's own checks pass while main fails. It is
	// supporting evidence only and never makes a PR a fix on its own.
	Confirmed bool `json:"confirmed,omitempty"`
	// WaitingOn names who the fix PR is waiting on (reviewers, CI, author).
	WaitingOn string `json:"waiting_on,omitempty"`
}

// Fact is a repo whose default branch has had a required check red for
// longer than its threshold.
type Fact struct {
	Repo          string    `json:"repo"`
	Branch        string    `json:"branch,omitempty"`
	Check         string    `json:"check"`
	Since         time.Time `json:"since"`
	RunURL        string    `json:"run_url,omitempty"`
	TrackingIssue int       `json:"tracking_issue,omitempty"`
	// Paths are the failing run's annotated files (see RedRun.Paths).
	Paths []string `json:"paths,omitempty"`
	// FixPRs are the open PRs detected as fixing main, best first.
	FixPRs []FixPR `json:"fix_prs,omitempty"`
}

// PrimaryFix is the fix PR to name: the first confirmed one, else the first.
func (f *Fact) PrimaryFix() *FixPR {
	if f == nil || len(f.FixPRs) == 0 {
		return nil
	}
	for i := range f.FixPRs {
		if f.FixPRs[i].Confirmed {
			return &f.FixPRs[i]
		}
	}
	return &f.FixPRs[0]
}

// IsFixPR reports whether number is one of the fact's fix PRs.
func (f *Fact) IsFixPR(number int) bool {
	if f == nil {
		return false
	}
	for _, p := range f.FixPRs {
		if p.Number == number {
			return true
		}
	}
	return false
}

func (f *Fact) sinceText() string {
	return f.Since.UTC().Format("2006-01-02 15:04 UTC")
}

func (f *Fact) fixText() string {
	fix := f.PrimaryFix()
	if fix == nil {
		return "no fix PR yet"
	}
	return fmt.Sprintf("#%d waiting on %s", fix.Number, firstNonEmpty(fix.WaitingOn, "review"))
}

// CardLine is the repo-card summary:
// "main red since <time> on <check>; fix: #N waiting on <who>".
func (f *Fact) CardLine() string {
	if f == nil {
		return ""
	}
	return fmt.Sprintf("main red since %s on %s; fix: %s", f.sinceText(), f.Check, f.fixText())
}

// PauseReason is the message an agent gets when the hive refuses to open a
// new PR in the repo, and the kick-message notice. It names the check and
// the fix PR and says how to get a fix through.
func (f *Fact) PauseReason() string {
	if f == nil {
		return ""
	}
	var b strings.Builder
	branch := f.Branch
	if branch == "" {
		branch = "the default branch"
	}
	fmt.Fprintf(&b, "new PRs on %s are paused: required check %q has been red on %s since %s", f.Repo, f.Check, branch, f.sinceText())
	if f.RunURL != "" {
		fmt.Fprintf(&b, " (%s)", f.RunURL)
	}
	b.WriteString(". ")
	if fix := f.PrimaryFix(); fix != nil {
		fmt.Fprintf(&b, "The fix is #%d (waiting on %s) — help it land (review it, repair its CI) instead of opening new PRs that will be red too. ", fix.Number, firstNonEmpty(fix.WaitingOn, "review"))
	} else {
		b.WriteString("No fix PR is open yet. ")
	}
	if f.TrackingIssue > 0 {
		fmt.Fprintf(&b, "A PR that fixes main is still accepted: reference the tracking issue with `Fixes #%d` (hive-open-pr --issues %d). ", f.TrackingIssue, f.TrackingIssue)
	} else {
		fmt.Fprintf(&b, "A PR that fixes main is still accepted: file the `[shared-ci] %s failing across %s` issue first (issue filing is not paused), then open the PR with `Fixes #<that issue>`. ", f.Check, f.Repo)
	}
	b.WriteString("Existing PRs keep going, and the pause lifts automatically once the check is green on main.")
	return b.String()
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// PRInput is the PR-side evidence FixesMain reads.
type PRInput struct {
	Labels []string
	// Paths are the PR's changed files; nil when unknown.
	Paths []string
	// ClosingIssues are the same-repo issues the PR closes (`Fixes #N`).
	ClosingIssues []int
	// CIStatus is the PR head's rolled-up CI ("success", "failure", ...),
	// and FailingChecks the names of its failing checks.
	CIStatus      string
	FailingChecks []string
}

// FixesMain reports whether a PR fixes the red default branch f describes,
// per the maintainer's definition on #13617: the explicit fixes-main label,
// OR a `Fixes` reference to the red-main tracking issue, OR changed files
// that overlap the failing test/check. via names which signal matched.
// Passing PR checks are not detection (see Confirmed).
func FixesMain(f *Fact, pr PRInput) (via string, ok bool) {
	if f == nil {
		return "", false
	}
	for _, l := range pr.Labels {
		if strings.EqualFold(strings.TrimSpace(l), FixesMainLabel) {
			return ViaLabel, true
		}
	}
	if f.TrackingIssue > 0 {
		for _, n := range pr.ClosingIssues {
			if n == f.TrackingIssue {
				return ViaTrackingIssue, true
			}
		}
	}
	if PathsOverlap(f.Paths, pr.Paths) {
		return ViaFileOverlap, true
	}
	return "", false
}

// Confirmed reports the supporting evidence that a PR fixes main: its own
// checks pass, including the one red on main, while main fails. It never
// makes a PR a fix on its own.
func Confirmed(f *Fact, pr PRInput) bool {
	if f == nil || !strings.EqualFold(pr.CIStatus, "success") {
		return false
	}
	for _, name := range pr.FailingChecks {
		if name == f.Check {
			return false
		}
	}
	return true
}

// PathsOverlap reports whether a PR's changed paths overlap the failing
// check: a changed workflow under .github/workflows/ (the check's own
// definition), a changed file the failing run's annotations name, or a
// changed file in the same directory as one (the code under a failing test).
func PathsOverlap(failing, changed []string) bool {
	if len(changed) == 0 {
		return false
	}
	files := map[string]bool{}
	dirs := map[string]bool{}
	for _, p := range failing {
		p = cleanPath(p)
		if p == "" || p == ".github" {
			continue
		}
		files[p] = true
		if d := path.Dir(p); d != "." && d != "/" {
			dirs[d] = true
		}
	}
	for _, p := range changed {
		p = cleanPath(p)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, ".github/workflows/") {
			return true
		}
		if files[p] || dirs[path.Dir(p)] {
			return true
		}
	}
	return false
}

func cleanPath(p string) string {
	p = strings.TrimSpace(strings.ReplaceAll(p, "\\", "/"))
	return strings.TrimPrefix(p, "./")
}

// SortFixPRs orders fix PRs best first: confirmed before unconfirmed, then
// by number (oldest PR first).
func SortFixPRs(prs []FixPR) {
	sort.SliceStable(prs, func(i, j int) bool {
		if prs[i].Confirmed != prs[j].Confirmed {
			return prs[i].Confirmed
		}
		return prs[i].Number < prs[j].Number
	})
}

type redEntry struct {
	since time.Time
	run   RedRun
}

// Tracker remembers, per repo and required check, when the check was first
// seen red on the default branch since it was last green. It is safe for
// concurrent use; use NewTracker.
//
// State is process-local: after a restart a check that is still red is dated
// from its current failing run's completion, which can only make the breaker
// trip later, never earlier.
type Tracker struct {
	mu    sync.RWMutex
	repos map[string]map[string]redEntry
	// branches records the default branch each repo was observed on.
	branches map[string]string
}

// NewTracker returns an empty tracker.
func NewTracker() *Tracker {
	return &Tracker{repos: map[string]map[string]redEntry{}, branches: map[string]string{}}
}

func repoKey(repo string) string { return strings.ToLower(strings.TrimSpace(repo)) }

// Observe records one look at repo's default branch: red holds every
// required check currently red there. A check absent from red is green (or
// no longer required) and is forgotten, which is what clears the fact. A
// check first seen red is dated from its run's completion when that is
// known and not in the future, else from now.
func (t *Tracker) Observe(repo, branch string, red map[string]RedRun, now time.Time) {
	t.ObservePending(repo, branch, red, nil, now)
}

// ObservePending is Observe with the required checks whose run on the tip
// has not completed yet (queued, in progress, or not created). Only a
// completed green run clears a check: a pending check that was red keeps
// its entry unchanged — same since, same run — and one that was not red
// stays untracked.
func (t *Tracker) ObservePending(repo, branch string, red map[string]RedRun, pending map[string]bool, now time.Time) {
	if t == nil {
		return
	}
	key := repoKey(repo)
	if key == "" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	prev := t.repos[key]
	next := make(map[string]redEntry, len(red))
	for check, run := range red {
		if e, ok := prev[check]; ok {
			e.run = run
			next[check] = e
			continue
		}
		since := now
		if !run.CompletedAt.IsZero() && run.CompletedAt.Before(now) {
			since = run.CompletedAt
		}
		next[check] = redEntry{since: since, run: run}
	}
	for check, isPending := range pending {
		if !isPending {
			continue
		}
		if _, seen := next[check]; seen {
			continue
		}
		if e, ok := prev[check]; ok {
			next[check] = e
		}
	}
	if len(next) == 0 {
		delete(t.repos, key)
	} else {
		t.repos[key] = next
	}
	if branch != "" {
		t.branches[key] = branch
	}
}

// TrackingIssue returns the tracking issue recorded for repo's red check,
// 0 when none is or the check is not red.
func (t *Tracker) TrackingIssue(repo, check string) int {
	if t == nil {
		return 0
	}
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.repos[repoKey(repo)][check].run.TrackingIssue
}

// SetTrackingIssue records issue as the tracking issue of repo's red check.
// It does nothing when the check is not tracked as red.
func (t *Tracker) SetTrackingIssue(repo, check string, issue int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	checks := t.repos[repoKey(repo)]
	e, ok := checks[check]
	if !ok {
		return
	}
	e.run.TrackingIssue = issue
	checks[check] = e
}

// Forget drops everything known about repo.
func (t *Tracker) Forget(repo string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.repos, repoKey(repo))
	delete(t.branches, repoKey(repo))
}

// Active returns repo's red-main fact: the longest-red required check that
// has been red for MORE than threshold (a non-positive threshold means
// DefaultThreshold). nil when main is green or every red check is younger.
func (t *Tracker) Active(repo string, threshold time.Duration, now time.Time) *Fact {
	if t == nil {
		return nil
	}
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	key := repoKey(repo)
	t.mu.RLock()
	defer t.mu.RUnlock()
	var best *Fact
	for check, e := range t.repos[key] {
		if now.Sub(e.since) <= threshold {
			continue
		}
		if best != nil && (best.Since.Before(e.since) || (best.Since.Equal(e.since) && best.Check < check)) {
			continue
		}
		best = &Fact{
			Repo:          strings.TrimSpace(repo),
			Branch:        t.branches[key],
			Check:         check,
			Since:         e.since,
			RunURL:        e.run.URL,
			TrackingIssue: e.run.TrackingIssue,
			Paths:         append([]string(nil), e.run.Paths...),
		}
	}
	return best
}
