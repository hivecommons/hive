package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/delegatedecision"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
)

// depBotRedIssueLineText is a substring of every hand-off line naming an
// issue.
const depBotRedIssueLineText = "asks for that fix in the repository instead of on this branch"

// depBotRedFailStart is when the fake's failing runs started; the fake base
// tip's passing runs finished an hour later.
var depBotRedFailStart = depBotT0.Add(-time.Hour)

// depBotRedIssueFakeIssue is one issue the fake repository holds.
type depBotRedIssueFakeIssue struct {
	number   int
	title    string
	body     string
	open     bool
	owned    bool
	closedAt time.Time
}

// depBotRedIssueFake answers the filer's GitHub reads and records its
// writes. Issues live in the fake, not the store, so a fresh store sees the
// issues an earlier one filed (a restart).
type depBotRedIssueFake struct {
	ownUnknown bool
	gateClosed bool
	base       github.StaleRedBaseState
	baseErr    error
	baseGone   bool
	behind     int
	behindErr  error
	// behindBy overrides behind per "<base>..<head>" or per base SHA
	// compared, and behindErrBase limits behindErr to one base SHA.
	// mergeBase is the merge base per head (default: the base itself when
	// the head is not behind it, else none).
	behindBy      map[string]int
	behindErrBase string
	mergeBase     map[string]string
	// baseAt is the check-run state per base commit read by SHA.
	baseAt    map[string]github.StaleRedBaseState
	baseAtErr error
	// passedOn names a check with a passing run per head.
	passedOn  map[string]string
	passedErr error
	passReads int
	// labelNotOwn: the needs-human label events do not show the App's own
	// label; labelErr: they cannot be read.
	labelNotOwn bool
	labelErr    error
	labelReads  int
	// runs overrides the run facts per check; any other check failed twice
	// in one suite, never passed, started at depBotRedFailStart and left one
	// error annotation.
	runs    map[string]github.DependencyBotRedCheckRuns
	runsErr error
	// runsErrHead fails the run read for one head only.
	runsErrHead map[string]error
	sharedCI    map[string]int
	lookupErr   error
	refuse      bool
	createErr   error
	// existing makes the create return this issue as AlreadyExisted.
	existing   *depBotRedIssueFakeIssue
	isOwnErr   error
	noteErr    error
	issues     []*depBotRedIssueFakeIssue
	notes      []string
	gateChecks int
	baseReads  int
	compares   int
	runReads   int
	lookups    int
	creates    int
	nextNumber int
}

func newDepBotRedIssueFake() *depBotRedIssueFake {
	f := &depBotRedIssueFake{
		base: github.StaleRedBaseState{
			SHA:         "base-1",
			Completed:   map[string]bool{},
			Red:         map[string]bool{},
			Reported:    map[string]bool{},
			SucceededAt: map[string]time.Time{},
		},
		nextNumber: 100,
	}
	f.baseGreen("build", "test")
	return f
}

// baseGreen marks checks as passing on the base tip after the failure.
func (f *depBotRedIssueFake) baseGreen(checks ...string) {
	for _, c := range checks {
		f.base.Completed[c] = true
		f.base.Reported[c] = true
		f.base.SucceededAt[c] = depBotRedFailStart.Add(time.Hour)
	}
}

func (f *depBotRedIssueFake) policy() depBotRedIssuePolicy {
	return depBotRedIssuePolicy{Enabled: true, GitHub: f}
}

func (f *depBotRedIssueFake) reads() int {
	return f.baseReads + f.compares + f.runReads + f.lookups + f.passReads + f.labelReads
}

func (f *depBotRedIssueFake) DependencyBotRedIssueOwnAccountsKnown() bool { return !f.ownUnknown }

func (f *depBotRedIssueFake) DependencyBotRedIssueFileAllowed(string) bool {
	f.gateChecks++
	return !f.gateClosed
}

func (f *depBotRedIssueFake) StaleRedBaseStateFor(context.Context, string, string) (github.StaleRedBaseState, bool, error) {
	f.baseReads++
	if f.baseErr != nil {
		return github.StaleRedBaseState{}, true, f.baseErr
	}
	if f.baseGone {
		return github.StaleRedBaseState{}, false, nil
	}
	return f.base, true, nil
}

func (f *depBotRedIssueFake) DependencyBotRedCompare(_ context.Context, _ string, baseSHA, headSHA string) (github.DependencyBotRedComparison, error) {
	f.compares++
	if f.behindErr != nil && (f.behindErrBase == "" || f.behindErrBase == baseSHA) {
		return github.DependencyBotRedComparison{}, f.behindErr
	}
	behind := f.behind
	if n, ok := f.behindBy[baseSHA+".."+headSHA]; ok {
		behind = n
	} else if n, ok := f.behindBy[baseSHA]; ok {
		behind = n
	}
	mb, ok := f.mergeBase[headSHA]
	if !ok && behind == 0 {
		mb = baseSHA
	}
	return github.DependencyBotRedComparison{BehindBy: behind, MergeBase: mb}, nil
}

func (f *depBotRedIssueFake) DependencyBotRedBaseStateAt(_ context.Context, _ string, sha string) (github.StaleRedBaseState, error) {
	f.baseReads++
	if f.baseAtErr != nil {
		return github.StaleRedBaseState{}, f.baseAtErr
	}
	st, ok := f.baseAt[sha]
	if !ok {
		st = github.StaleRedBaseState{SHA: sha}
	}
	return st, nil
}

func (f *depBotRedIssueFake) DependencyBotRedAnyPassed(_ context.Context, _ string, headSHA string, _ []string) (string, error) {
	f.passReads++
	return f.passedOn[headSHA], f.passedErr
}

func (f *depBotRedIssueFake) StaleRedNeedsHumanLabeledByApp(context.Context, string, int) (bool, error) {
	f.labelReads++
	return !f.labelNotOwn && f.labelErr == nil, f.labelErr
}

func (f *depBotRedIssueFake) DependencyBotRedRunFacts(_ context.Context, _ string, headSHA string, checks []string) (map[string]github.DependencyBotRedCheckRuns, error) {
	f.runReads++
	if f.runsErr != nil {
		return nil, f.runsErr
	}
	if err := f.runsErrHead[headSHA]; err != nil {
		return nil, err
	}
	out := map[string]github.DependencyBotRedCheckRuns{}
	for _, c := range checks {
		r, ok := f.runs[c]
		if !ok {
			r = github.DependencyBotRedCheckRuns{Reproduced: true, FailureStartedAt: depBotRedFailStart, Errors: []string{c + " broke: undefined: lib.OldName"}}
		}
		out[c] = r
	}
	return out, nil
}

func (f *depBotRedIssueFake) LookupDependencyBotRedIssue(_ context.Context, _ string, marker string, checks []string, now time.Time) (github.DependencyBotRedIssueLookup, error) {
	f.lookups++
	if f.lookupErr != nil {
		return github.DependencyBotRedIssueLookup{}, f.lookupErr
	}
	var out github.DependencyBotRedIssueLookup
	for _, c := range checks {
		if n := f.sharedCI[c]; n > 0 {
			out.SharedCI = n
			return out, nil
		}
	}
	for _, is := range f.issues {
		first, _, _ := strings.Cut(is.body, "\n")
		if strings.TrimSpace(first) != marker {
			continue
		}
		if is.open {
			return github.DependencyBotRedIssueLookup{Open: is.number, OpenURL: fmt.Sprintf("https://example.test/issues/%d", is.number), OpenOwned: is.owned}, nil
		}
		if now.Sub(is.closedAt) < 24*time.Hour {
			out.RecentlyClosed = true
		}
	}
	return out, nil
}

func (f *depBotRedIssueFake) FileDependencyBotRedIssue(_ context.Context, _ string, title, body string) (github.CreateIssueResult, error) {
	if f.refuse {
		return github.CreateIssueResult{}, github.ErrDependencyBotRedIssueRefused
	}
	if f.createErr != nil {
		return github.CreateIssueResult{}, f.createErr
	}
	if f.existing != nil {
		f.issues = append(f.issues, f.existing)
		return github.CreateIssueResult{Number: f.existing.number, URL: fmt.Sprintf("https://example.test/issues/%d", f.existing.number), AlreadyExisted: true}, nil
	}
	f.creates++
	f.nextNumber++
	f.issues = append(f.issues, &depBotRedIssueFakeIssue{number: f.nextNumber, title: title, body: body, open: true, owned: true})
	return github.CreateIssueResult{Number: f.nextNumber, URL: fmt.Sprintf("https://example.test/issues/%d", f.nextNumber)}, nil
}

func (f *depBotRedIssueFake) DependencyBotRedIssueIsOwn(_ context.Context, _ string, number int, marker string) (bool, error) {
	if f.isOwnErr != nil {
		return false, f.isOwnErr
	}
	for _, is := range f.issues {
		first, _, _ := strings.Cut(is.body, "\n")
		if is.number == number {
			return is.owned && strings.TrimSpace(first) == marker, nil
		}
	}
	return false, nil
}

func (f *depBotRedIssueFake) NoteDependencyBotRedIssue(_ context.Context, repo string, number int, marker, body string) (bool, error) {
	if f.noteErr != nil {
		return false, f.noteErr
	}
	for _, is := range f.issues {
		first, _, _ := strings.Cut(is.body, "\n")
		if is.number == number && is.open && is.owned && strings.TrimSpace(first) == marker {
			f.notes = append(f.notes, escalation.Key(repo, number)+": "+body)
			return true, nil
		}
	}
	return false, nil
}

var _ depBotRedIssueGitHub = (*depBotRedIssueFake)(nil)

// depBotCountingReader counts PR state reads.
type depBotCountingReader struct {
	*depBotFakeReader
	stateReads int
}

func (r *depBotCountingReader) GetPRState(ctx context.Context, repo string, number int) (github.PRState, error) {
	r.stateReads++
	return r.depBotFakeReader.GetPRState(ctx, repo, number)
}

func (h *depBotHarness) redIssuePassWith(at time.Time, adopt depBotAdoptPolicy, ri depBotRedIssuePolicy, reader depBotPRReader, prs ...github.PullRequest) {
	h.t.Helper()
	a := &github.ActionableResult{}
	a.PRs.Items = prs
	sweepDependencyBotHandoffs(context.Background(), h.store, "acme", 0, adopt, ri, h.writer, reader, a, func() time.Time { return at }, discardLogger())
}

func (h *depBotHarness) redIssuePassAt(at time.Time, adopt depBotAdoptPolicy, ri depBotRedIssuePolicy, prs ...github.PullRequest) {
	h.t.Helper()
	h.redIssuePassWith(at, adopt, ri, h.reader, prs...)
}

// redIssuePastGrace runs the two passes that start the red-CI clock and
// reach the grace on each PR's head, and returns the second pass's time.
func (h *depBotHarness) redIssuePastGrace(start time.Time, ri depBotRedIssuePolicy, prs ...github.PullRequest) time.Time {
	h.t.Helper()
	h.redIssuePassAt(start, depBotAdoptPolicy{}, ri, prs...)
	at := start.Add(depBotRedCIGrace)
	h.redIssuePassAt(at, depBotAdoptPolicy{}, ri, prs...)
	return at
}

func depBotRedBump(sha string) github.PullRequest {
	return depBotPR("dependabot[bot]", sha, func(p *github.PullRequest) {
		redCI(p)
		p.Title = "Bump example.com/lib from 1.2.0 to 2.0.0"
		p.URL = "https://github.com/acme/widgets/pull/42"
		p.CIFailureExcerpt = "build: undefined: lib.OldName\ntest: exit status 1"
	})
}

func (h *depBotHarness) wantOneHandoff(what string) string {
	h.t.Helper()
	if len(h.writer.comments) != 1 {
		h.t.Fatalf("%s: comments = %v, want the one person hand-off", what, h.writer.comments)
	}
	if want := depBotKey + ": " + escalation.NeedsHumanLabel; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		h.t.Fatalf("%s: adds = %v, want only [%q]", what, h.writer.adds, want)
	}
	return h.writer.comments[0]
}

// wantPlainHandoff asserts today's hand-off with no issue and nothing filed.
func (h *depBotHarness) wantPlainHandoff(f *depBotRedIssueFake, what string) {
	h.t.Helper()
	got := h.wantOneHandoff(what)
	if f.creates != 0 || strings.Contains(got, depBotRedIssueLineText) {
		h.t.Fatalf("%s: creates=%d hand-off=%q, want no issue", what, f.creates, got)
	}
	if _, ok := h.store.redIssue(depBotKey); ok {
		h.t.Fatalf("%s: recorded an issue", what)
	}
}

// Switch off: today's hand-off, word for word, and nothing read or filed.
func TestDependencyBotRedIssueSwitchOffKeepsHandoff(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	off := f.policy()
	off.Enabled = false
	pr := depBotRedBump("sha-1")
	h.redIssuePastGrace(depBotT0, off, pr)
	got := h.wantOneHandoff("switch off")
	if want := depBotKey + ": " + dependencyBotRedCIComment(pr, depBotTransientRerun{}); got != want {
		t.Fatalf("hand-off = %q, want today's %q", got, want)
	}
	if f.gateChecks != 0 || f.reads() != 0 || f.creates != 0 {
		t.Fatalf("switch off: gate=%d reads=%d creates=%d, want none", f.gateChecks, f.reads(), f.creates)
	}
	if _, ok := h.store.redIssue(depBotKey); ok {
		t.Fatal("switch off recorded an issue")
	}
}

// Switch on, a failure reproduced within one suite on every failing check,
// all green on the base tip the head contains after the failure: one issue,
// named in the hand-off; a second pass, a later head with the same checks
// and a restart (persisted or fresh memory) never file again.
func TestDependencyBotRedIssueFilesOncePerPRAndChecks(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	pr := depBotRedBump("sha-1")
	pr.CIActivity = &github.PRCIActivity{Head: "sha-1", Checks: []github.PRActivityCheck{
		{Name: "build", State: "completed", Conclusion: "failure", URL: "https://github.com/acme/widgets/actions/runs/1/job/11", Head: "sha-1"},
		{Name: "test", State: "completed", Conclusion: "failure", URL: "https://ci.elsewhere.example/job/12", Head: "sha-1"},
	}}

	h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), pr)
	if f.reads() != 0 || len(h.writer.comments) != 0 {
		t.Fatalf("before the grace: reads=%d comments=%v, want none", f.reads(), h.writer.comments)
	}
	h.redIssuePassAt(depBotT0.Add(depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), pr)
	if f.creates != 1 || len(f.issues) != 1 {
		t.Fatalf("creates = %d, want exactly one issue", f.creates)
	}
	is := f.issues[0]
	comment := h.wantOneHandoff("switch on")
	if !strings.Contains(comment, fmt.Sprintf("Issue #%d %s", is.number, depBotRedIssueLineText)) {
		t.Fatalf("hand-off does not name issue #%d: %q", is.number, comment)
	}
	if !strings.HasPrefix(comment, depBotKey+": "+dependencyBotRedCIComment(pr, depBotTransientRerun{})) {
		t.Fatalf("hand-off lost today's text: %q", comment)
	}
	afterFix := depBotRedIssueAfterFix(pr, false)
	if !strings.Contains(afterFix, "Hive cannot ask Dependabot") || !strings.Contains(comment, afterFix) {
		t.Fatalf("hand-off does not say what happens after the fix on a Dependabot PR: %q", comment)
	}
	marker := github.DependencyBotRedIssueMarker(42, []string{"build", "test"})
	if first, _, _ := strings.Cut(is.body, "\n"); first != marker {
		t.Fatalf("body does not open with the marker: %q", first)
	}
	for _, want := range []string{
		"#42",
		"`build`, `test`",
		"https://github.com/acme/widgets/actions/runs/1/job/11",
		"build: build broke: undefined: lib.OldName",
		"- Reproduced: each failing check failed again within one check suite of this head after an earlier failing attempt had finished (a re-run of the same workflow run), and no run of these checks passed on this head.",
		"`base-1`",
		"Fix the repository so this bump passes; do not edit the bot's branch. " + afterFix,
	} {
		if !strings.Contains(is.body, want) {
			t.Errorf("issue body has no %q:\n%s", want, is.body)
		}
	}
	if strings.Contains(is.body, "ci.elsewhere.example") || strings.Contains(is.body, "will regenerate") {
		t.Errorf("issue body links off the forge's host, or promises a regeneration:\n%s", is.body)
	}
	for _, bad := range []string{"\u2014", "\u2013", "@"} {
		if strings.Contains(is.body, bad) || strings.Contains(is.title, bad) || strings.Contains(comment, bad) {
			t.Errorf("emitted text contains %q", bad)
		}
	}

	// Second pass, same head, label on or off: nothing new.
	labeled := pr
	labeled.Labels = []string{escalation.NeedsHumanLabel}
	h.redIssuePassAt(depBotT0.Add(2*depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), labeled)
	h.redIssuePassAt(depBotT0.Add(3*depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), pr)
	if f.creates != 1 || len(h.writer.comments) != 1 {
		t.Fatalf("second pass: creates=%d comments=%v, want no new writes", f.creates, h.writer.comments)
	}

	// The bot's next head fails the same checks: it is judged again, the
	// marker lookup finds the hive's open issue, and its hand-off links it.
	lookups := f.lookups
	h.redIssuePastGrace(depBotT0.Add(4*depBotRedCIGrace), f.policy(), depBotRedBump("sha-2"))
	if f.creates != 1 || f.lookups != lookups+1 {
		t.Fatalf("next head: creates=%d lookups=%d (was %d), want one lookup and no filing", f.creates, f.lookups, lookups)
	}
	if len(h.writer.comments) != 2 || !strings.Contains(h.writer.comments[1], fmt.Sprintf("Issue #%d", is.number)) {
		t.Fatalf("next head hand-off = %v, want it to name #%d", h.writer.comments, is.number)
	}

	// Restart with the ledger on disk.
	h.store = loadDepBotConflictStore(h.path)
	if rec, ok := h.store.redIssue(depBotKey); !ok || rec.Issue != is.number {
		t.Fatalf("persisted record = %+v (ok=%v), want issue #%d", rec, ok, is.number)
	}

	// Restart with fresh memory: the marker lookup finds the open issue.
	h.store = loadDepBotConflictStore(filepath.Join(t.TempDir(), "dependency-bot-conflicts.json"))
	h.redIssuePastGrace(depBotT0.Add(6*depBotRedCIGrace), f.policy(), depBotRedBump("sha-3"))
	if f.creates != 1 || f.lookups != lookups+2 {
		t.Fatalf("fresh memory: creates=%d lookups=%d, want one lookup and no filing", f.creates, f.lookups)
	}
	if len(h.writer.comments) != 3 || !strings.Contains(h.writer.comments[2], fmt.Sprintf("Issue #%d", is.number)) {
		t.Fatalf("fresh-memory hand-off = %v, want it to name #%d", h.writer.comments, is.number)
	}
}

// Only a failure reproduced within one check suite files: a single failed
// attempt, failures only across suites, or a check that also passed on the
// head (fail, pass, fail) files nothing. The transient re-run having failed
// again counts as reproduced.
func TestDependencyBotRedIssueOnlyReproducedFailures(t *testing.T) {
	ok := github.DependencyBotRedCheckRuns{Reproduced: true, FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}}
	for name, test := range map[string]github.DependencyBotRedCheckRuns{
		// One failed attempt, failures only in other suites, or failures
		// overlapping in one suite all read as not reproduced.
		"not reproduced": {FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}},
		"fail pass fail": {Reproduced: true, Passed: true, FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotRedIssueFake()
			f.runs = map[string]github.DependencyBotRedCheckRuns{"build": ok, "test": test}
			h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
			h.wantPlainHandoff(f, name)
			if f.runReads != 1 || f.lookups != 0 {
				t.Fatalf("run reads=%d lookups=%d, want one bounded read and no lookup", f.runReads, f.lookups)
			}
		})
	}

	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	f.runsErr = errors.New("502")
	h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	h.wantPlainHandoff(f, "runs unreadable")

	// The transient re-run ran and failed again: reproduced even without a
	// sequential failure in one suite, but a passing run still vetoes it.
	f4 := newDepBotRedIssueFake()
	f4.runs = map[string]github.DependencyBotRedCheckRuns{
		"build": {FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}},
		"test":  {FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}},
	}
	pass := newDepBotRedIssuePass(f4.policy(), nil)
	store := loadDepBotConflictStore("")
	out := pass.issueFor(context.Background(), store, "acme/widgets", depBotKey, depBotRedBump("sha-1"), depBotTransientRerun{State: depBotRerunDone}, depBotT0, discardLogger())
	if out.Number == 0 || f4.creates != 1 {
		t.Fatalf("re-run done: out=%+v creates=%d, want one issue", out, f4.creates)
	}
	if !strings.Contains(f4.issues[0].body, "- Reproduced: Hive re-ran the failed jobs of this head once and they failed again, and no run of these checks passed on this head.") {
		t.Fatalf("body does not say how it reproduced:\n%s", f4.issues[0].body)
	}
	f5 := newDepBotRedIssueFake()
	f5.runs = map[string]github.DependencyBotRedCheckRuns{"test": {Reproduced: true, Passed: true, FailureStartedAt: depBotRedFailStart, Errors: []string{"boom"}}}
	pass = newDepBotRedIssuePass(f5.policy(), nil)
	// A fresh store: the one above recorded f4's issue for this head, and the
	// same-head retry shortcut would link it without reading the runs.
	store = loadDepBotConflictStore("")
	if out := pass.issueFor(context.Background(), store, "acme/widgets", depBotKey, depBotRedBump("sha-1"), depBotTransientRerun{State: depBotRerunDone}, depBotT0, discardLogger()); out.Number != 0 || f5.creates != 0 {
		t.Fatalf("re-run done but a run passed: out=%+v, want no issue", out)
	}
}

// The base proof must be no older than the failure: a passing base run that
// finished before the head's failing run started, or without a time, is
// unknown, on the re-run-done path too.
func TestDependencyBotRedIssueBaseProofMustPostdateFailure(t *testing.T) {
	for name, mut := range map[string]func(*depBotRedIssueFake){
		"base run finished before the failure": func(f *depBotRedIssueFake) { f.base.SucceededAt["test"] = depBotRedFailStart.Add(-time.Minute) },
		"base run finished at the same time":   func(f *depBotRedIssueFake) { f.base.SucceededAt["test"] = depBotRedFailStart },
		"base run time unknown":                func(f *depBotRedIssueFake) { delete(f.base.SucceededAt, "test") },
		// A skipped or neutral base run is completed and not red, so the
		// stale-red verdict clears, but it has no success time.
		"base run skipped or neutral": func(f *depBotRedIssueFake) {
			f.base.Completed["test"] = true
			delete(f.base.SucceededAt, "test")
		},
		"failure start unknown": func(f *depBotRedIssueFake) {
			f.runs = map[string]github.DependencyBotRedCheckRuns{"test": {Reproduced: true, Errors: []string{"boom"}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotRedIssueFake()
			mut(f)
			h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
			h.wantPlainHandoff(f, name)

			f2 := newDepBotRedIssueFake()
			mut(f2)
			pass := newDepBotRedIssuePass(f2.policy(), nil)
			if out := pass.issueFor(context.Background(), loadDepBotConflictStore(""), "acme/widgets", depBotKey, depBotRedBump("sha-1"), depBotTransientRerun{State: depBotRerunDone}, depBotT0, discardLogger()); out.Number != 0 || f2.creates != 0 {
				t.Fatalf("re-run done: out=%+v, want no issue", out)
			}
		})
	}
}

// Junk evidence files nothing: an excerpt of exit-code-only lines (no read
// at all), or newest failing runs with no failure-level error annotation.
func TestDependencyBotRedIssueNeedsRealErrors(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	pr := depBotRedBump("sha-1")
	pr.CIFailureExcerpt = "build: Process completed with exit code 1.\nlint: something else"
	h.redIssuePastGrace(depBotT0, f.policy(), pr)
	h.wantPlainHandoff(f, "exit-code excerpt")
	if f.reads() != 0 {
		t.Fatalf("exit-code excerpt read GitHub %d times", f.reads())
	}

	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	f2.runs = map[string]github.DependencyBotRedCheckRuns{
		"build": {Reproduced: true, FailureStartedAt: depBotRedFailStart},
		"test":  {Reproduced: true, FailureStartedAt: depBotRedFailStart},
	}
	h2.redIssuePastGrace(depBotT0, f2.policy(), depBotRedBump("sha-1"))
	h2.wantPlainHandoff(f2, "no error annotations")
}

// The Dependabot-secrets scenario: the same check failing on five bot PRs
// is a shared cause, so nothing is filed for any of them, at no API cost.
func TestDependencyBotRedIssueSkipsCheckFailingOnOtherPRs(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	var prs []github.PullRequest
	for i := 0; i < 5; i++ {
		pr := depBotRedBump("sha-1")
		pr.Number = 60 + i
		pr.FailingChecks = []string{"build"}
		pr.CIFailureExcerpt = "build: secret DEPLOY_TOKEN is empty"
		prs = append(prs, pr)
	}
	h.redIssuePastGrace(depBotT0, f.policy(), prs...)
	if len(h.writer.comments) != 5 || f.creates != 0 || f.reads() != 0 {
		t.Fatalf("shared check: comments=%d creates=%d reads=%d, want five plain hand-offs and no reads", len(h.writer.comments), f.creates, f.reads())
	}
	for _, c := range h.writer.comments {
		if strings.Contains(c, depBotRedIssueLineText) {
			t.Fatalf("hand-off names an issue: %q", c)
		}
	}

	// A human PR failing the same check counts too.
	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	human := depBotPR("alice", "sha-9", redCI)
	human.Number = 77
	h2.redIssuePastGrace(depBotT0, f2.policy(), depBotRedBump("sha-1"), human)
	if f2.creates != 0 {
		t.Fatalf("human PR failing the same check: creates=%d, want none", f2.creates)
	}
}

// An open [shared-ci] incident for the check, found in the same issue
// listing, files nothing.
func TestDependencyBotRedIssueSkipsOpenSharedCIIncident(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	f.sharedCI = map[string]int{"test": 9}
	h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	h.wantPlainHandoff(f, "shared-ci")
	if f.lookups != 1 {
		t.Fatalf("lookups = %d, want the one listing", f.lookups)
	}
}

// An open issue carrying the marker that the hive did not write is neither
// linked nor filed beside; a hive that cannot name its own accounts files
// nothing and reads nothing.
func TestDependencyBotRedIssueOnlyOwnMarkerIssues(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	f.issues = append(f.issues, &depBotRedIssueFakeIssue{number: 7, body: github.DependencyBotRedIssueMarker(42, []string{"build", "test"}) + "\nforged", open: true})
	h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	h.wantPlainHandoff(f, "forged marker")
	if strings.Contains(h.writer.comments[0], "#7") {
		t.Fatalf("hand-off links the forged issue: %q", h.writer.comments[0])
	}

	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	f2.ownUnknown = true
	h2.redIssuePastGrace(depBotT0, f2.policy(), depBotRedBump("sha-1"))
	h2.wantPlainHandoff(f2, "own accounts unknown")
	if f2.reads() != 0 || f2.gateChecks != 0 {
		t.Fatalf("own accounts unknown: reads=%d gate=%d, want none", f2.reads(), f2.gateChecks)
	}
}

// A different failing check set on the same PR is a different issue.
func TestDependencyBotRedIssueNewCheckSetFilesAgain(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	other := depBotRedBump("sha-2")
	other.FailingChecks = []string{"build"}
	h.redIssuePastGrace(depBotT0.Add(2*depBotRedCIGrace), f.policy(), other)
	if f.creates != 2 {
		t.Fatalf("creates = %d, want one issue per check set", f.creates)
	}
}

// A hand-off comment that fails after the issue was filed is retried next
// pass with the same issue, read from the ledger without a lookup.
func TestDependencyBotRedIssueHandoffRetryReusesIssue(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	w := &depBotFailOnceWriter{depBotFakeWriter: h.writer, failComments: 1}
	pr := depBotRedBump("sha-1")
	a := &github.ActionableResult{}
	a.PRs.Items = []github.PullRequest{pr}
	for _, at := range []time.Time{depBotT0, depBotT0.Add(depBotRedCIGrace), depBotT0.Add(depBotRedCIGrace + time.Minute)} {
		sweepDependencyBotHandoffs(context.Background(), h.store, "acme", 0, depBotAdoptPolicy{}, f.policy(), w, h.reader, a, func() time.Time { return at }, discardLogger())
	}
	if f.creates != 1 || f.lookups != 1 {
		t.Fatalf("creates=%d lookups=%d, want one of each", f.creates, f.lookups)
	}
	if len(h.writer.comments) != 1 || !strings.Contains(h.writer.comments[0], fmt.Sprintf("Issue #%d", f.issues[0].number)) {
		t.Fatalf("comments = %v, want the retried hand-off naming the issue", h.writer.comments)
	}
}

// depBotFailOnceWriter fails the first failComments hand-off comments.
type depBotFailOnceWriter struct {
	*depBotFakeWriter
	failComments int
}

func (w *depBotFailOnceWriter) CreateIssueComment(ctx context.Context, repo string, number int, body string) error {
	if w.failComments > 0 {
		w.failComments--
		return errors.New("502")
	}
	return w.depBotFakeWriter.CreateIssueComment(ctx, repo, number, body)
}

// Every case without proof that the bump needs a repository change files
// nothing, and the hand-off is today's.
func TestDependencyBotRedIssueFailsClosed(t *testing.T) {
	cases := []struct {
		name string
		fake func(*depBotRedIssueFake)
		pr   func(*github.PullRequest)
	}{
		{"base red on the same check", func(f *depBotRedIssueFake) { f.base.Red["test"] = true }, nil},
		{"base never ran a check", func(f *depBotRedIssueFake) { delete(f.base.Completed, "test") }, nil},
		{"base unreadable", func(f *depBotRedIssueFake) { f.baseErr = errors.New("502") }, nil},
		{"base branch gone", func(f *depBotRedIssueFake) { f.baseGone = true }, nil},
		{"head behind a passing base", func(f *depBotRedIssueFake) { f.behind = 2 }, nil},
		{"compare failed", func(f *depBotRedIssueFake) { f.behindErr = errors.New("502") }, nil},
		{"lookup failed", func(f *depBotRedIssueFake) { f.lookupErr = errors.New("502") }, nil},
		{"base red recorded on the PR", nil, func(p *github.PullRequest) { p.BaseRedChecks = []string{"test"} }},
		{"no base ref", nil, func(p *github.PullRequest) { p.BaseRef = "" }},
		{"too many failing checks", nil, func(p *github.PullRequest) { p.FailingChecks = []string{"a", "b", "c", "d", "e", "f"} }},
		{"recently closed issue", func(f *depBotRedIssueFake) {
			f.issues = append(f.issues, &depBotRedIssueFakeIssue{number: 7, body: github.DependencyBotRedIssueMarker(42, []string{"build", "test"}), owned: true, closedAt: depBotT0})
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotRedIssueFake()
			if tc.fake != nil {
				tc.fake(f)
			}
			pr := depBotRedBump("sha-1")
			if tc.pr != nil {
				tc.pr(&pr)
			}
			h.redIssuePastGrace(depBotT0, f.policy(), pr)
			h.wantPlainHandoff(f, tc.name)
		})
	}
}

// A gate that refuses issue_file costs nothing: no read at all. A refusal
// at file time, or a failed create, files nothing and keeps the hand-off.
func TestDependencyBotRedIssueWritebackRefusal(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	f.gateClosed = true
	h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	h.wantPlainHandoff(f, "gate closed")
	if f.reads() != 0 {
		t.Fatalf("gate closed: reads=%d, want none", f.reads())
	}

	h1 := newDepBotHarness(t)
	f1 := newDepBotRedIssueFake()
	f1.refuse = true
	h1.redIssuePastGrace(depBotT0, f1.policy(), depBotRedBump("sha-1"))
	h1.wantPlainHandoff(f1, "refused at file time")

	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	f2.createErr = errors.New("500")
	h2.redIssuePastGrace(depBotT0, f2.policy(), depBotRedBump("sha-1"))
	h2.wantPlainHandoff(f2, "create failed")
}

// A repository not on GitHub files nothing and reads nothing.
func TestDependencyBotRedIssueGitHubOnly(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	p := f.policy()
	p.OnGitHub = func(string) bool { return false }
	h.redIssuePastGrace(depBotT0, p, depBotRedBump("sha-1"))
	h.wantPlainHandoff(f, "not on GitHub")
	if f.reads() != 0 || f.gateChecks != 0 {
		t.Fatalf("not on GitHub: reads=%d gate=%d, want nothing", f.reads(), f.gateChecks)
	}
}

// Adoption takes the head: no hand-off and no issue.
func TestDependencyBotRedIssueNotWhenAdoptionApplies(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	adopt := newDepBotAdoptFake().policy()
	pr := depBotRedBump("sha-1")
	h.redIssuePassAt(depBotT0, adopt, f.policy(), pr)
	h.redIssuePassAt(depBotT0.Add(depBotRedCIGrace), adopt, f.policy(), pr)
	if want := depBotKey + ": " + delegatedecision.LabelAdoptSupersede; len(h.writer.adds) != 1 || h.writer.adds[0] != want {
		t.Fatalf("adds = %v, want the adoption label only", h.writer.adds)
	}
	if len(h.writer.comments) != 0 || f.reads() != 0 || f.creates != 0 {
		t.Fatalf("adoption: comments=%v reads=%d creates=%d, want none", h.writer.comments, f.reads(), f.creates)
	}
}

// A stale-red rebase request on this head holds the hand-off (and so the
// issue) for its window; the filer itself also refuses a head with a request
// pending, and a head behind a passing base (the rebase is due).
func TestDependencyBotRedIssueNotWhenRebaseDueOrPending(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	pr := depBotRedBump("sha-1")
	// Asked just before the red-CI grace ends, so the request's window
	// still holds the hand-off when the grace is reached.
	h.store.markStaleRedRebaseRequested(depBotKey, "sha-1", "base-1", staleRedBotTriggerBaseClears, depBotViaRenovateCheckbox, []string{"build", "test"}, depBotT0.Add(depBotRedCIGrace-time.Minute))
	h.redIssuePastGrace(depBotT0, f.policy(), pr)
	if len(h.writer.comments) != 0 || f.creates != 0 || f.reads() != 0 {
		t.Fatalf("pending request: comments=%v creates=%d reads=%d, want nothing", h.writer.comments, f.creates, f.reads())
	}

	pass := newDepBotRedIssuePass(f.policy(), nil)
	if out := pass.issueFor(context.Background(), h.store, "acme/widgets", depBotKey, pr, depBotTransientRerun{}, depBotT0, discardLogger()); out.Number != 0 {
		t.Fatalf("pending request: issueFor = %+v, want no issue", out)
	}
	if f.reads() != 0 {
		t.Fatalf("pending request read GitHub %d times", f.reads())
	}

	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	f2.behind = 3
	h2.redIssuePastGrace(depBotT0, f2.policy(), pr)
	h2.wantPlainHandoff(f2, "rebase due")
}

// After the bot regenerated the branch once for a stale-red request and the
// new head fails the same checks, the stale-red pass never asks again. The
// head must still contain the passing base tip: behind it, nothing is filed.
// When it does, the text names the person's step instead of a rebase Hive
// will not ask.
func TestDependencyBotRedIssueAfterBotRegenerated(t *testing.T) {
	regeneratedPR := func(h *depBotHarness) github.PullRequest {
		h.store.markStaleRedRebaseRequested(depBotKey, "sha-0", "base-0", staleRedBotTriggerBaseClears, depBotViaRenovateCheckbox, []string{"build"}, depBotT0)
		pr := depBotRedBump("sha-1")
		pr.Author = renovateBotAuthor
		return pr
	}
	hb := newDepBotHarness(t)
	fb := newDepBotRedIssueFake()
	fb.behind = 4
	hb.redIssuePastGrace(depBotT0, fb.policy(), regeneratedPR(hb))
	hb.wantPlainHandoff(fb, "regenerated but behind")
	if fb.compares != 1 {
		t.Fatalf("regenerated: compares = %d, want the behind check", fb.compares)
	}

	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	h.redIssuePastGrace(depBotT0, f.policy(), regeneratedPR(h))
	if f.creates != 1 || f.compares != 1 {
		t.Fatalf("creates=%d compares=%d, want one issue after one compare", f.creates, f.compares)
	}
	body := f.issues[0].body
	for _, want := range []string{"already regenerated this branch once", "Hive does not ask again for these checks"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body has no %q:\n%s", want, body)
		}
	}
	if len(h.writer.comments) != 1 || !strings.Contains(h.writer.comments[0], "the bot's regenerated branch failed them again") {
		t.Fatalf("hand-off = %v, want the regenerated wording", h.writer.comments)
	}
}

// What the text promises after the fix depends on the bot and the case, and
// the base ref is neutralized.
func TestDependencyBotRedIssueAfterFixText(t *testing.T) {
	renovate := depBotPR(renovateBotAuthor, "s", nil)
	if got := depBotRedIssueAfterFix(renovate, false); !strings.Contains(got, "Hive may tick its rebase/retry box once when it is allowed to ask the bot; otherwise tick the box by hand") {
		t.Fatalf("renovate: %q", got)
	}
	if got := depBotRedIssueAfterFix(depBotPR("dependabot[bot]", "s", nil), false); !strings.Contains(got, "Hive cannot ask Dependabot") {
		t.Fatalf("dependabot: %q", got)
	}
	if got := depBotRedIssueAfterFix(depBotPR("mergeraptor[bot]", "s", nil), false); strings.Contains(got, "Hive") {
		t.Fatalf("other bot promises a hive step: %q", got)
	}
	for _, s := range []string{depBotRedIssueAfterFix(renovate, true), dependencyBotRedIssueLine(renovate, 5, false, ""), dependencyBotRedIssueLine(renovate, 5, true, "b00c45bcd70a")} {
		if strings.Contains(s, "will regenerate") || strings.ContainsAny(s, "@\u2014\u2013") {
			t.Fatalf("text %q promises a regeneration or carries a dash or an at sign", s)
		}
	}
	odd := depBotPR(renovateBotAuthor, "s", func(p *github.PullRequest) { p.BaseRef = "rel`<x>" })
	if line := dependencyBotRedIssueLine(odd, 5, false, ""); strings.ContainsAny(line, "<>") || strings.Contains(line, "rel`") {
		t.Fatalf("base ref not neutralized: %q", line)
	}
}

// Transient, external or shared-CI triage, or a re-run that is still owed or
// re-ran nothing, is not shown to be the bump's fault.
func TestDependencyBotRedIssueNotForTriagedFailures(t *testing.T) {
	f := newDepBotRedIssueFake()
	pass := newDepBotRedIssuePass(f.policy(), nil)
	store := loadDepBotConflictStore("")
	for name, mut := range map[string]func(*github.PullRequest){
		"transient": func(p *github.PullRequest) { p.TransientCITriaged = true },
		"external":  func(p *github.PullRequest) { p.ExternalCIDeferred = true },
		"shared":    func(p *github.PullRequest) { p.SharedCIIncident = 9 },
	} {
		pr := depBotRedBump("sha-1")
		mut(&pr)
		if out := pass.issueFor(context.Background(), store, "acme/widgets", depBotKey, pr, depBotTransientRerun{}, depBotT0, discardLogger()); out.Number != 0 {
			t.Fatalf("%s: issueFor = %+v, want no issue", name, out)
		}
	}
	for _, st := range []depBotRerunState{depBotRerunOwed, depBotRerunNotRun} {
		if out := pass.issueFor(context.Background(), store, "acme/widgets", depBotKey, depBotRedBump("sha-1"), depBotTransientRerun{State: st}, depBotT0, discardLogger()); out.Number != 0 {
			t.Fatalf("re-run state %d: issueFor = %+v, want no issue", st, out)
		}
	}
	if f.reads() != 0 {
		t.Fatalf("triaged failures read GitHub %d times", f.reads())
	}
}

// At most depBotRedIssueMaxPerPass PRs per pass do the reads; any other PR
// is handed off on the same pass without the issue (no hand-off waits), and
// PRs on one base share one read of its tip.
func TestDependencyBotRedIssuePerPassBudget(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	var prs []github.PullRequest
	for i := 0; i < depBotRedIssueMaxPerPass+2; i++ {
		check := fmt.Sprintf("check-%d", i)
		f.baseGreen(check)
		pr := depBotRedBump("sha-1")
		pr.Number = 50 + i
		pr.FailingChecks = []string{check}
		pr.CIFailureExcerpt = check + ": failed to compile"
		prs = append(prs, pr)
	}
	h.redIssuePastGrace(depBotT0, f.policy(), prs...)
	if len(h.writer.comments) != len(prs) {
		t.Fatalf("comments = %d, want every PR handed off on the grace pass", len(h.writer.comments))
	}
	if f.creates != depBotRedIssueMaxPerPass || f.runReads != depBotRedIssueMaxPerPass || f.baseReads != 1 {
		t.Fatalf("creates=%d run reads=%d base reads=%d, want %d, %d and one shared base read", f.creates, f.runReads, f.baseReads, depBotRedIssueMaxPerPass, depBotRedIssueMaxPerPass)
	}
	for i, c := range h.writer.comments {
		named := strings.Contains(c, depBotRedIssueLineText)
		if named != (i < depBotRedIssueMaxPerPass) {
			t.Fatalf("hand-off %d names an issue = %v, want only the first %d to: %q", i, named, depBotRedIssueMaxPerPass, c)
		}
	}
}

// Lifecycle: once the PR goes green, or leaves the snapshot closed, one note
// on the hive's issue, then the record is forgotten. The issue is never
// closed, and a closed PR's state is read once per pass.
func TestDependencyBotRedIssueLifecycleNotes(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	at := h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
	if f.creates != 1 {
		t.Fatalf("creates = %d, want one", f.creates)
	}
	green := depBotPR("dependabot[bot]", "sha-2", nil)
	h.redIssuePassAt(at.Add(time.Hour), depBotAdoptPolicy{}, f.policy(), green)
	if len(f.notes) != 1 || !strings.Contains(f.notes[0], "is green now") {
		t.Fatalf("notes = %v, want one green note", f.notes)
	}
	if _, ok := h.store.redIssue(depBotKey); ok {
		t.Fatal("record kept after the note")
	}
	h.redIssuePassAt(at.Add(2*time.Hour), depBotAdoptPolicy{}, f.policy(), green)
	if len(f.notes) != 1 {
		t.Fatalf("notes = %v, want no second note", f.notes)
	}
	if !f.issues[0].open {
		t.Fatal("the issue was closed")
	}

	// A PR that left the snapshot closed without merging: the undo pass's
	// read is reused.
	h2 := newDepBotHarness(t)
	f2 := newDepBotRedIssueFake()
	at = h2.redIssuePastGrace(depBotT0, f2.policy(), depBotRedBump("sha-1"))
	h2.reader.state = "closed"
	counting := &depBotCountingReader{depBotFakeReader: h2.reader}
	h2.redIssuePassWith(at.Add(time.Hour), depBotAdoptPolicy{}, f2.policy(), counting)
	if len(f2.notes) != 1 || !strings.Contains(f2.notes[0], "closed without merging") {
		t.Fatalf("notes = %v, want one closed note", f2.notes)
	}
	if counting.stateReads != 1 {
		t.Fatalf("state reads = %d, want the undo pass's one read reused", counting.stateReads)
	}

	// Switch off: no lifecycle reads or notes.
	h3 := newDepBotHarness(t)
	f3 := newDepBotRedIssueFake()
	at = h3.redIssuePastGrace(depBotT0, f3.policy(), depBotRedBump("sha-1"))
	off := f3.policy()
	off.Enabled = false
	h3.redIssuePassAt(at.Add(time.Hour), depBotAdoptPolicy{}, off, depBotPR("dependabot[bot]", "sha-2", nil))
	if len(f3.notes) != 0 {
		t.Fatalf("switch off posted notes %v", f3.notes)
	}
}

// PRs that left the snapshot and are still open cost at most
// depBotRedIssueMaxStateReadsPerPass state reads per pass.
func TestDependencyBotRedIssueLifecycleStateReadsCapped(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	for i := 0; i < depBotRedIssueMaxStateReadsPerPass+3; i++ {
		h.store.markRedIssue(escalation.Key("acme/widgets", 300+i), depBotRedIssueRecord{ChecksKey: "k", Checks: []string{"build"}, Issue: 900 + i, HeadSHA: "sha-1", FiledAt: depBotT0})
	}
	counting := &depBotCountingReader{depBotFakeReader: h.reader}
	h.redIssuePassWith(depBotT0, depBotAdoptPolicy{}, f.policy(), counting)
	if counting.stateReads != depBotRedIssueMaxStateReadsPerPass || len(f.notes) != 0 {
		t.Fatalf("state reads = %d notes = %v, want %d reads and no notes", counting.stateReads, f.notes, depBotRedIssueMaxStateReadsPerPass)
	}
}

// The live policy is off unless the switch is set and the reader can file.
func TestDependencyBotRedIssuePolicyFromConfig(t *testing.T) {
	cfg := &config.Config{}
	if p := depBotRedIssuePolicyFromConfig(cfg, &depBotFakeReader{}, discardLogger()); p.Enabled {
		t.Fatal("policy enabled with the switch unset")
	}
	cfg.AutoMerge.DependencyBotRedIssue.Enabled = true
	if p := depBotRedIssuePolicyFromConfig(cfg, &depBotFakeReader{}, discardLogger()); p.Enabled {
		t.Fatal("policy enabled with a reader that cannot file issues")
	}
	if p := depBotRedIssuePolicyFromConfig(nil, nil, discardLogger()); p.Enabled {
		t.Fatal("nil config enabled the policy")
	}
}

// Required checks are the branch-protection list when it is known.
func TestDependencyBotRedIssueChecks(t *testing.T) {
	pr := depBotRedBump("sha-1")
	if got := depBotRedIssueChecks(pr); strings.Join(got, ",") != "build,test" {
		t.Fatalf("checks = %v, want build,test", got)
	}
	pr.Protection = &github.ProtectionFacts{RequiredChecksKnown: true, FailingRequiredChecks: []string{"test"}}
	if got := depBotRedIssueChecks(pr); strings.Join(got, ",") != "test" {
		t.Fatalf("checks = %v, want the required test only", got)
	}
}

// CreateIssue's exact-title reuse is linked only when the hive wrote that
// issue; otherwise the hand-off posts without a link and nothing is recorded.
func TestDependencyBotRedIssueAlreadyExistedMustBeOwn(t *testing.T) {
	marker := github.DependencyBotRedIssueMarker(42, []string{"build", "test"})
	for name, tc := range map[string]struct {
		issue  *depBotRedIssueFakeIssue
		err    error
		linked bool
	}{
		"someone else's issue": {issue: &depBotRedIssueFakeIssue{number: 55, body: "a person's issue with the same title", open: true}},
		"unreadable":           {issue: &depBotRedIssueFakeIssue{number: 55, body: marker, open: true, owned: true}, err: errors.New("502")},
		"the hive's own issue": {issue: &depBotRedIssueFakeIssue{number: 55, body: marker + "\nbody", open: true, owned: true}, linked: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotRedIssueFake()
			f.existing = tc.issue
			f.isOwnErr = tc.err
			h.redIssuePastGrace(depBotT0, f.policy(), depBotRedBump("sha-1"))
			got := h.wantOneHandoff(name)
			_, recorded := h.store.redIssue(depBotKey)
			if strings.Contains(got, "Issue #55") != tc.linked || recorded != tc.linked {
				t.Fatalf("linked=%v recorded=%v, want %v: %q", strings.Contains(got, "Issue #55"), recorded, tc.linked, got)
			}
		})
	}
}

// depBotGoneReader answers every PR state read with GitHub's 404.
type depBotGoneReader struct{ *depBotFakeReader }

func (r *depBotGoneReader) GetPRState(context.Context, string, int) (github.PRState, error) {
	return github.PRState{}, &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}, Message: "Not Found"}
}

// A lifecycle record whose PR, repository or issue is gone (404 or 410) is
// forgotten instead of being read every pass; any other error keeps it.
func TestDependencyBotRedIssueLifecycleForgetsGoneRecords(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	h.store.markRedIssue(escalation.Key("acme/widgets", 301), depBotRedIssueRecord{ChecksKey: "k", Checks: []string{"build"}, Issue: 901, HeadSHA: "sha-1", FiledAt: depBotT0})
	h.redIssuePassWith(depBotT0, depBotAdoptPolicy{}, f.policy(), &depBotGoneReader{depBotFakeReader: h.reader})
	if _, ok := h.store.redIssue(escalation.Key("acme/widgets", 301)); ok {
		t.Fatal("record kept for a PR GitHub reports gone")
	}

	for name, tc := range map[string]struct {
		err  error
		kept bool
	}{
		"issue deleted (410)": {err: fmt.Errorf("reading acme/widgets#901: %w", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusGone}}), kept: false},
		"issue missing (404)": {err: fmt.Errorf("reading acme/widgets#901: %w", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}), kept: false},
		"server error":        {err: errors.New("502"), kept: true},
	} {
		t.Run(name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotRedIssueFake()
			f.noteErr = tc.err
			key := escalation.Key("acme/widgets", 302)
			h.store.markRedIssue(key, depBotRedIssueRecord{ChecksKey: "k", Checks: []string{"build"}, Issue: 902, HeadSHA: "sha-1", FiledAt: depBotT0})
			h.reader.state = "closed"
			h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy())
			if _, ok := h.store.redIssue(key); ok != tc.kept {
				t.Fatalf("record kept = %v, want %v", ok, tc.kept)
			}
		})
	}
}

// newDepBotCrossHeadFake is the projectbluefin/utah#1486 shape on the GitHub
// side: the base tip base-3 passes both checks after the failure; the newer
// head sha-2 is 3 commits behind it, built on base-2 (its merge base), which
// the previous head sha-1 lacks and where both checks passed; and each check
// failed once per head (no re-run in one suite, so the in-suite rule cannot
// fire).
func newDepBotCrossHeadFake() *depBotRedIssueFake {
	f := newDepBotRedIssueFake()
	f.base.SHA = "base-3"
	f.behindBy = map[string]int{"base-3": 3, "base-2..sha-1": 1}
	f.mergeBase = map[string]string{"sha-2": "base-2"}
	builtOn := github.StaleRedBaseState{SHA: "base-2", Completed: map[string]bool{}, Red: map[string]bool{}, Reported: map[string]bool{}, SucceededAt: map[string]time.Time{}}
	for _, c := range []string{"build", "test"} {
		builtOn.Completed[c], builtOn.Reported[c] = true, true
		// Before the newer head's failure started: the same commit, so
		// the time does not matter.
		builtOn.SucceededAt[c] = depBotRedFailStart.Add(-2 * time.Hour)
	}
	f.baseAt = map[string]github.StaleRedBaseState{"base-2": builtOn}
	f.runs = map[string]github.DependencyBotRedCheckRuns{
		"build": {FailureStartedAt: depBotRedFailStart, Errors: []string{"build broke: undefined: lib.OldName"}},
		"test":  {FailureStartedAt: depBotRedFailStart, Errors: []string{"test broke"}},
	}
	return f
}

// depBotCrossHeadPR is a red mergeraptor PR head.
func depBotCrossHeadPR(sha string) github.PullRequest {
	pr := depBotRedBump(sha)
	pr.Author = "mergeraptor[bot]"
	return pr
}

func depBotParked(pr github.PullRequest) github.PullRequest {
	pr.Labels = []string{escalation.NeedsHumanLabel}
	return pr
}

// The utah#1486 shape: the first head is handed off red without an issue
// (one failed attempt, behind the base), a newer head arrives while the
// hand-off's needs-human is still on, and it fails the same checks while
// still behind the current tip. One issue is filed for the newer head, the
// PR gets no second comment, and later passes file nothing more.
func TestDependencyBotRedIssueCrossHeadParkedPR(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	at := h.redIssuePastGrace(depBotT0, f.policy(), depBotCrossHeadPR("sha-1"))
	h.wantPlainHandoff(f, "first head")
	if f.runReads != 0 {
		t.Fatalf("first head read runs %d times, want none (behind, no earlier head)", f.runReads)
	}

	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	h.redIssuePassAt(at.Add(time.Hour), depBotAdoptPolicy{}, f.policy(), parked)
	if f.creates != 0 {
		t.Fatalf("filed before the newer head's grace: creates=%d", f.creates)
	}
	h.redIssuePassAt(at.Add(time.Hour+depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), parked)
	if f.creates != 1 || len(f.issues) != 1 {
		t.Fatalf("creates = %d, want exactly one issue for the newer head", f.creates)
	}
	if len(h.writer.comments) != 1 || len(h.writer.adds) != 1 {
		t.Fatalf("comments=%v adds=%v, want only the first head's hand-off", h.writer.comments, h.writer.adds)
	}
	if f.labelReads != 1 {
		t.Fatalf("label reads = %d, want one before filing", f.labelReads)
	}
	body := f.issues[0].body
	for _, want := range []string{
		"- Reproduced: " + github.DependencyBotRedCrossHeadReproduced("sha-1"),
		"The previous head `sha-1` failed the same checks. This head contains base commit `base-2` that the previous head did not",
		"head `sha-2`",
		"`base-3`",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body has no %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "The head already contains that base commit") {
		t.Errorf("cross-head body claims the head contains the tip:\n%s", body)
	}
	if rec, ok := h.store.redIssue(depBotKey); !ok || rec.HeadSHA != "sha-2" || rec.PreviousHead != "sha-1" {
		t.Fatalf("record = %+v (ok=%v), want sha-2 after sha-1", rec, ok)
	}
	reads := f.reads()
	for i := 2; i < 4; i++ {
		h.redIssuePassAt(at.Add(time.Hour+time.Duration(i)*depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), parked)
	}
	if f.creates != 1 || f.reads() != reads || len(h.writer.comments) != 1 {
		t.Fatalf("later passes: creates=%d reads=%d (was %d) comments=%d, want nothing new", f.creates, f.reads(), reads, len(h.writer.comments))
	}
}

// The review's case: the newer head was made on base-2, the fix for the
// check reached the base later as base-3 (the tip), so the tip passes after
// the newer head's failure while base-2, the commit the head contains, does
// not. The tip's pass is the only proof: nothing is filed.
func TestDependencyBotRedIssueCrossHeadFixLandedAfterHead(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	builtOn := f.baseAt["base-2"]
	builtOn.Red["test"] = true
	delete(builtOn.SucceededAt, "test")
	h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), depBotCrossHeadPR("sha-1"))
	h.redIssuePastGrace(depBotT0.Add(time.Hour), f.policy(), depBotCrossHeadPR("sha-2"))
	h.wantPlainHandoff(f, "fix landed after the head")
	if f.baseReads != 2 {
		t.Fatalf("base reads = %d, want the tip and the merge base", f.baseReads)
	}
}

// When the newer head is handed off itself (the first head was replaced
// before its grace), the hand-off names the issue with the cross-head
// reason, and a head that sat in CI between the two still counts as the
// direct successor. A newer head up to date with the tip uses the tip as
// its merge base, with no extra base read.
func TestDependencyBotRedIssueCrossHeadHandoff(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), depBotCrossHeadPR("sha-1"))
	pending := depBotCrossHeadPR("sha-2")
	pending.CIStatus, pending.FailingChecks, pending.MergeableState = "pending", nil, "clean"
	h.redIssuePassAt(depBotT0.Add(time.Hour), depBotAdoptPolicy{}, f.policy(), pending)
	h.redIssuePastGrace(depBotT0.Add(2*time.Hour), f.policy(), depBotCrossHeadPR("sha-2"))
	if f.creates != 1 {
		t.Fatalf("creates = %d, want one", f.creates)
	}
	got := h.wantOneHandoff("cross-head hand-off")
	for _, want := range []string{
		fmt.Sprintf("Issue #%d %s", f.issues[0].number, depBotRedIssueLineText),
		"they failed on the previous head `sha-1` and again on this head after it was brought up to date with the base",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("hand-off has no %q: %q", want, got)
		}
	}
	for _, bad := range []string{"\u2014", "\u2013", "@"} {
		if strings.Contains(got, bad) || strings.Contains(f.issues[0].body, bad) {
			t.Errorf("emitted text contains %q", bad)
		}
	}

	h2 := newDepBotHarness(t)
	f2 := newDepBotCrossHeadFake()
	f2.behindBy = map[string]int{"base-3..sha-1": 2}
	f2.mergeBase = map[string]string{}
	h2.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f2.policy(), depBotCrossHeadPR("sha-1"))
	h2.redIssuePastGrace(depBotT0.Add(time.Hour), f2.policy(), depBotCrossHeadPR("sha-2"))
	// sha-2 is not behind the tip but failed once: in-suite does not
	// fire, cross-head does, on the tip's own timed pass.
	if f2.creates != 1 || f2.baseReads != 1 {
		t.Fatalf("up-to-date newer head: creates=%d base reads=%d, want one issue and only the tip read", f2.creates, f2.baseReads)
	}
}

// Every cross-head case without the proof files nothing, and the hand-off
// is today's.
func TestDependencyBotRedIssueCrossHeadFailsClosed(t *testing.T) {
	cases := []struct {
		name  string
		fake  func(*depBotRedIssueFake)
		first func(*github.PullRequest)
	}{
		{"a different check failed on the previous head", nil, func(p *github.PullRequest) {
			p.FailingChecks = []string{"lint"}
			p.CIFailureExcerpt = "lint: unused import"
		}},
		{"the previous head failed only one of the checks", nil, func(p *github.PullRequest) { p.FailingChecks = []string{"build"} }},
		{"the previous head was triaged transient", nil, func(p *github.PullRequest) { p.TransientCITriaged = true }},
		{"the previous head was deferred to external CI", nil, func(p *github.PullRequest) { p.ExternalCIDeferred = true }},
		{"the previous head was under a shared-ci incident", nil, func(p *github.PullRequest) { p.SharedCIIncident = 9 }},
		{"the previous head's check was red on the base", nil, func(p *github.PullRequest) { p.BaseRedChecks = []string{"test"} }},
		{"a run passed on the previous head", func(f *depBotRedIssueFake) { f.passedOn = map[string]string{"sha-1": "test"} }, nil},
		{"the previous head's runs unreadable", func(f *depBotRedIssueFake) { f.passedErr = errors.New("502") }, nil},
		{"no base update: the previous head already had the merge base", func(f *depBotRedIssueFake) { f.behindBy["base-2..sha-1"] = 0 }, nil},
		{"merge base unknown", func(f *depBotRedIssueFake) { f.mergeBase = map[string]string{} }, nil},
		{"the previous-head compare failed", func(f *depBotRedIssueFake) { f.behindErr, f.behindErrBase = errors.New("502"), "base-2" }, nil},
		{"the merge base never ran a check", func(f *depBotRedIssueFake) { delete(f.baseAt["base-2"].SucceededAt, "test") }, nil},
		{"the merge base unreadable", func(f *depBotRedIssueFake) { f.baseAtErr = errors.New("502") }, nil},
		{"base proof older than the newer failure", func(f *depBotRedIssueFake) { f.base.SucceededAt["test"] = depBotRedFailStart.Add(-time.Minute) }, nil},
		{"a run passed on the newer head", func(f *depBotRedIssueFake) {
			f.runs["test"] = github.DependencyBotRedCheckRuns{Passed: true, FailureStartedAt: depBotRedFailStart, Errors: []string{"test broke"}}
		}, nil},
		{"base red on the check", func(f *depBotRedIssueFake) { f.base.Red["test"] = true }, nil},
		{"an open shared-ci incident", func(f *depBotRedIssueFake) { f.sharedCI = map[string]int{"build": 9} }, nil},
		{"no error annotation", func(f *depBotRedIssueFake) {
			f.runs = map[string]github.DependencyBotRedCheckRuns{"build": {FailureStartedAt: depBotRedFailStart}, "test": {FailureStartedAt: depBotRedFailStart}}
		}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newDepBotHarness(t)
			f := newDepBotCrossHeadFake()
			if tc.fake != nil {
				tc.fake(f)
			}
			first := depBotCrossHeadPR("sha-1")
			if tc.first != nil {
				tc.first(&first)
			}
			h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), first)
			h.redIssuePastGrace(depBotT0.Add(time.Hour), f.policy(), depBotCrossHeadPR("sha-2"))
			h.wantPlainHandoff(f, tc.name)
		})
	}
}

// The same head seen on several passes is one head, not two: behind the
// passing base, it gets no issue and no compare beyond the tip's.
func TestDependencyBotRedIssueCrossHeadSameHeadTwice(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	pr := depBotCrossHeadPR("sha-1")
	h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), pr)
	h.redIssuePassAt(depBotT0.Add(2*time.Hour), depBotAdoptPolicy{}, f.policy(), pr)
	h.redIssuePassAt(depBotT0.Add(depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), pr)
	h.wantPlainHandoff(f, "same head twice")
	if f.compares != 1 || f.runReads != 0 {
		t.Fatalf("compares=%d run reads=%d, want the tip compare only", f.compares, f.runReads)
	}
	if _, ok := h.store.redHeadBefore(depBotKey, "sha-1", []string{"build", "test"}); ok {
		t.Fatal("one head reads as two")
	}
}

// A newer head that went green ends the chain: no hand-off or issue for it,
// its history is dropped, and the next red head has no previous head.
func TestDependencyBotRedIssueCrossHeadGreenBreaksChain(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	h.redIssuePassAt(depBotT0, depBotAdoptPolicy{}, f.policy(), depBotCrossHeadPR("sha-1"))
	green := depBotPR("mergeraptor[bot]", "sha-2", nil)
	h.redIssuePassAt(depBotT0.Add(time.Hour), depBotAdoptPolicy{}, f.policy(), green)
	h.redIssuePassAt(depBotT0.Add(time.Hour+depBotRedCIGrace), depBotAdoptPolicy{}, f.policy(), green)
	if len(h.writer.comments) != 0 || f.creates != 0 || f.reads() != 0 {
		t.Fatalf("green head: comments=%v creates=%d reads=%d, want nothing", h.writer.comments, f.creates, f.reads())
	}
	if _, ok := h.store.redHeads[depBotKey]; ok {
		t.Fatal("history kept after the head went green")
	}
	h.redIssuePastGrace(depBotT0.Add(2*depBotRedCIGrace), f.policy(), depBotCrossHeadPR("sha-3"))
	h.wantPlainHandoff(f, "red after green")
}

// The red-head history: a direct successor (even after a CI-pending
// sighting of it) keeps the previous red head; a second other head between
// them, or a gap over depBotRedHeadMaxGap, does not; the checks must all
// have failed on the previous head, and a head whose red was not the bump's
// own on any sighting is never the previous head.
func TestDependencyBotRedHeadHistory(t *testing.T) {
	red := func(sha string, at time.Time, checks ...string) depBotRedHead {
		return depBotRedHead{HeadSHA: sha, Checks: checks, SeenAt: at}
	}
	s := loadDepBotConflictStore("")
	s.noteRedHead("k", red("a", depBotT0, "build", "test"))
	s.noteOtherHead("k", "b")
	s.noteOtherHead("k", "b")
	s.noteRedHead("k", red("b", depBotT0.Add(3*time.Hour), "build"))
	prev, ok := s.redHeadBefore("k", "b", []string{"Build"})
	if !ok || prev.HeadSHA != "a" {
		t.Fatalf("successor: prev=%+v ok=%v, want a", prev, ok)
	}
	if _, ok := s.redHeadBefore("k", "b", []string{"build", "lint"}); ok {
		t.Fatal("a check the previous head did not fail counts")
	}
	if _, ok := s.redHeadBefore("k", "a", []string{"build"}); ok {
		t.Fatal("the previous head reads as current")
	}

	s.noteOtherHead("k", "c")
	s.noteOtherHead("k", "d")
	s.noteRedHead("k", red("d", depBotT0.Add(4*time.Hour), "build"))
	if _, ok := s.redHeadBefore("k", "d", []string{"build"}); ok {
		t.Fatal("a head with another head between counts as the successor")
	}

	s.noteRedHead("k", red("e", depBotT0.Add(4*time.Hour+depBotRedHeadMaxGap+time.Minute), "build"))
	if _, ok := s.redHeadBefore("k", "e", []string{"build"}); ok {
		t.Fatal("a successor past the gap counts")
	}

	// A reason the red is not the bump's sticks to the head through later
	// clean sightings, and keeps it from being the previous head.
	triaged := red("e", depBotT0.Add(30*time.Hour), "build", "test")
	triaged.NotEvidence = "transient-triage"
	s.noteRedHead("k", triaged)
	s.noteRedHead("k", red("e", depBotT0.Add(32*time.Hour), "build", "test"))
	if got := s.redHeads["k"].Last; got.NotEvidence != "transient-triage" || len(got.Checks) != 2 {
		t.Fatalf("refreshed head = %+v, want two checks and the triage kept", got)
	}
	s.noteRedHead("k", red("f", depBotT0.Add(33*time.Hour), "build"))
	if _, ok := s.redHeadBefore("k", "f", []string{"build"}); ok {
		t.Fatal("a triaged previous head counts")
	}

	s.markRedHeadPending("k", "f", true)
	if !s.redHeadPending("k", "f") || s.redHeadPending("k", "e") {
		t.Fatal("pending mark wrong")
	}
	s.markRedHeadJudged("k", "f")
	s.markRedHeadJudged("other", "z")
	if !s.redHeadJudged("k", "f") || !s.redHeadJudged("other", "z") || s.redHeadJudged("k", "d") || s.redHeadPending("k", "f") {
		t.Fatal("judged marks wrong, or judging kept the pending retry")
	}
	if _, ok := s.redHeadBefore("other", "z", []string{"build"}); ok {
		t.Fatal("a judged-only record is cross-head evidence")
	}
	s.forgetRedHeads("k")
	s.pruneRedHeads(map[string]github.PullRequest{})
	if len(s.redHeads) != 0 {
		t.Fatalf("history = %v, want none after forget and prune", s.redHeads)
	}
}

// What makes a red not the bump's own.
func TestDependencyBotRedHeadNotEvidence(t *testing.T) {
	for want, mut := range map[string]func(*github.PullRequest){
		"":                   func(*github.PullRequest) {},
		"transient-triage":   func(p *github.PullRequest) { p.TransientCITriaged = true },
		"external-ci":        func(p *github.PullRequest) { p.ExternalCIDeferred = true },
		"shared-ci-incident": func(p *github.PullRequest) { p.SharedCIIncident = 3 },
		"base-red":           func(p *github.PullRequest) { p.BaseRedChecks = []string{"test"} },
	} {
		pr := depBotRedBump("sha-1")
		mut(&pr)
		if got := depBotRedHeadNotEvidence(pr); got != want {
			t.Fatalf("not evidence = %q, want %q", got, want)
		}
	}
}

// The history and the issues share the sidecar and survive a restart; a
// sidecar in the first layout (a bare map of issues) still loads its issues.
func TestDependencyBotRedIssueSidecarPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dependency-bot-conflicts.json")
	s := loadDepBotConflictStore(path)
	s.noteRedHead(depBotKey, depBotRedHead{HeadSHA: "a", Checks: []string{"build"}, SeenAt: depBotT0})
	s.noteRedHead(depBotKey, depBotRedHead{HeadSHA: "b", Checks: []string{"build"}, SeenAt: depBotT0.Add(time.Hour)})
	s.markRedHeadPending(depBotKey, "b", true)
	s.markRedIssue(depBotKey, depBotRedIssueRecord{ChecksKey: "k", Checks: []string{"build"}, Issue: 7, HeadSHA: "b", PreviousHead: "a", FiledAt: depBotT0})
	re := loadDepBotConflictStore(path)
	if prev, ok := re.redHeadBefore(depBotKey, "b", []string{"build"}); !ok || prev.HeadSHA != "a" || !re.redHeadPending(depBotKey, "b") {
		t.Fatalf("history after restart: prev=%+v ok=%v pending=%v", prev, ok, re.redHeadPending(depBotKey, "b"))
	}
	if rec, ok := re.redIssue(depBotKey); !ok || rec.Issue != 7 || rec.PreviousHead != "a" {
		t.Fatalf("issue after restart = %+v (ok=%v)", rec, ok)
	}

	old := filepath.Join(t.TempDir(), "old-red-issue.json")
	if err := os.WriteFile(old, []byte(`{"acme/widgets#42": {"checks_key": "k", "issue": 9, "head_sha": "a", "filed_at": "2026-10-12T12:00:00Z"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	issues, heads := loadDepBotRedIssueFile(old)
	if issues[depBotKey] == nil || issues[depBotKey].Issue != 9 || len(heads) != 0 {
		t.Fatalf("first-layout sidecar: issues=%v heads=%v", issues, heads)
	}
	if issues, heads := loadDepBotRedIssueFile(filepath.Join(t.TempDir(), "missing.json")); len(issues) != 0 || len(heads) != 0 {
		t.Fatal("a missing sidecar is not empty")
	}
}

// depBotParkedStore is a store whose sweep handed off sha-1 (red-ci, its
// own label) and then saw sha-2 red directly after it.
func depBotParkedStore() *depBotConflictStore {
	store := loadDepBotConflictStore("")
	store.markCommented(depBotKey, "sha-1", depBotHandoffRedCI, depBotT0.Add(-time.Hour))
	store.setLabelApplied(depBotKey, true)
	store.noteRedHead(depBotKey, depBotRedHead{HeadSHA: "sha-1", Checks: []string{"build", "test"}, SeenAt: depBotT0.Add(-time.Hour)})
	store.noteRedHead(depBotKey, depBotRedHead{HeadSHA: "sha-2", Checks: []string{"build", "test"}, SeenAt: depBotT0})
	return store
}

// fileForParkedHead runs only for the sweep's own earlier red-CI hand-off
// on another head; a spent budget, a failed read or an unreadable label
// keeps the head for a later pass instead of judging it; a needs-human a
// person put back files nothing.
func TestDependencyBotRedIssueParkedHeadOnlyOwnHandoff(t *testing.T) {
	newTestEscalationStore(t)
	f := newDepBotCrossHeadFake()
	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	past := depBotT0.Add(depBotRedCIGrace)
	ctx := context.Background()

	// A person's needs-human (no hand-off record), or the hand-off on this
	// very head with nothing pending: not judged, nothing read.
	store := loadDepBotConflictStore("")
	pass := newDepBotRedIssuePass(f.policy(), nil)
	for _, at := range []time.Time{depBotT0, past} {
		if pass.fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, at, discardLogger()) {
			t.Fatal("a person's label keeps a grace clock")
		}
	}
	store.markCommented(depBotKey, "sha-2", depBotHandoffRedCI, depBotT0)
	store.setLabelApplied(depBotKey, true)
	for _, at := range []time.Time{depBotT0, past} {
		pass.fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, at, discardLogger())
	}
	if f.reads() != 0 || f.gateChecks != 0 {
		t.Fatalf("reads=%d gate=%d, want none", f.reads(), f.gateChecks)
	}

	// The hand-off was for sha-1: sha-2 waits the grace, a spent budget
	// keeps it without a read, a failed read keeps it, and the next pass
	// judges it once.
	store = depBotParkedStore()
	if !pass.fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0, discardLogger()) {
		t.Fatal("inside the grace: the clock is not kept")
	}
	spent := newDepBotRedIssuePass(f.policy(), nil)
	spent.evaluated = depBotRedIssueMaxPerPass
	if !spent.fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) || store.redHeadJudged(depBotKey, "sha-2") || f.reads() != 0 {
		t.Fatal("a spent budget read, judged the head or dropped its clock")
	}
	f.runsErr = errors.New("502")
	if !newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) ||
		store.redHeadJudged(depBotKey, "sha-2") || f.creates != 0 {
		t.Fatal("unreadable runs judged the head")
	}
	f.runsErr = nil
	if newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) || !store.redHeadJudged(depBotKey, "sha-2") {
		t.Fatal("past the grace: the head is not judged once")
	}
	if f.creates != 1 || f.labelReads != 2 {
		t.Fatalf("creates=%d label reads=%d, want one issue, the label read on each attempt", f.creates, f.labelReads)
	}

	// The needs-human was taken off and put back by a person: nothing is
	// filed, and the head is not read again.
	fp := newDepBotCrossHeadFake()
	fp.labelNotOwn = true
	store = depBotParkedStore()
	for i := 0; i < 2; i++ {
		newDepBotRedIssuePass(fp.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0.Add(time.Duration(i)*past.Sub(depBotT0)), discardLogger())
	}
	newDepBotRedIssuePass(fp.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past.Add(time.Hour), discardLogger())
	if fp.creates != 0 || fp.labelReads != 1 || fp.runReads != 0 {
		t.Fatalf("a person's label: creates=%d label reads=%d run reads=%d, want one label read and nothing else", fp.creates, fp.labelReads, fp.runReads)
	}

	// Label events unreadable: kept for a later pass, nothing filed.
	fe := newDepBotCrossHeadFake()
	fe.labelErr = errors.New("502")
	store = depBotParkedStore()
	newDepBotRedIssuePass(fe.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0, discardLogger())
	if !newDepBotRedIssuePass(fe.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) || fe.creates != 0 || store.redHeadJudged(depBotKey, "sha-2") {
		t.Fatal("unreadable label events judged the head or filed")
	}
}

// A hand-off whose issue attempt lost the per-pass budget is retried for
// the same head on a later pass, through the parked path, once its
// needs-human is on.
func TestDependencyBotRedIssueBudgetLostHandoffRetried(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	var prs []github.PullRequest
	for i := 0; i < depBotRedIssueMaxPerPass+1; i++ {
		check := fmt.Sprintf("check-%d", i)
		f.baseGreen(check)
		pr := depBotRedBump("sha-1")
		pr.Number = 50 + i
		pr.FailingChecks = []string{check}
		pr.CIFailureExcerpt = check + ": failed to compile"
		prs = append(prs, pr)
	}
	at := h.redIssuePastGrace(depBotT0, f.policy(), prs...)
	if f.creates != depBotRedIssueMaxPerPass || len(h.writer.comments) != len(prs) {
		t.Fatalf("creates=%d comments=%d, want %d issues and every hand-off", f.creates, len(h.writer.comments), depBotRedIssueMaxPerPass)
	}
	last := escalation.Key("acme/widgets", prs[len(prs)-1].Number)
	if !h.store.redHeadPending(last, "sha-1") {
		t.Fatal("the PR past the budget is not pending")
	}
	parked := make([]github.PullRequest, len(prs))
	for i, pr := range prs {
		parked[i] = depBotParked(pr)
	}
	h.redIssuePassAt(at.Add(time.Minute), depBotAdoptPolicy{}, f.policy(), parked...)
	if f.creates != depBotRedIssueMaxPerPass+1 || len(h.writer.comments) != len(prs) {
		t.Fatalf("retry pass: creates=%d comments=%d, want one more issue and no new comment", f.creates, len(h.writer.comments))
	}
	if h.store.redHeadPending(last, "sha-1") || !h.store.redHeadJudged(last, "sha-1") {
		t.Fatal("the retried head is still pending, or not judged")
	}
}

// With the switch off, two red heads leave no history and read nothing.
func TestDependencyBotRedIssueCrossHeadSwitchOff(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotCrossHeadFake()
	off := f.policy()
	off.Enabled = false
	at := h.redIssuePastGrace(depBotT0, off, depBotCrossHeadPR("sha-1"))
	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	h.redIssuePassAt(at.Add(time.Hour), depBotAdoptPolicy{}, off, parked)
	h.redIssuePassAt(at.Add(time.Hour+depBotRedCIGrace), depBotAdoptPolicy{}, off, parked)
	if len(h.store.redHeads) != 0 || f.reads() != 0 || f.gateChecks != 0 || f.creates != 0 || len(h.writer.comments) != 1 {
		t.Fatalf("switch off: heads=%v reads=%d gate=%d creates=%d comments=%d", h.store.redHeads, f.reads(), f.gateChecks, f.creates, len(h.writer.comments))
	}
}

// depBotGitHubErr is GitHub answering code.
func depBotGitHubErr(code int) error {
	return fmt.Errorf("creating issue: %w", &gh.ErrorResponse{Response: &http.Response{StatusCode: code, Header: http.Header{}}, Message: "refused"})
}

// A lasting 4xx (410: issues disabled) is final for the head: judged at
// once, never read again. A 429 is temporary and keeps the head pending.
func TestDependencyBotRedIssueParkedHeadLasting4xxIsFinal(t *testing.T) {
	newTestEscalationStore(t)
	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	past := depBotT0.Add(depBotRedCIGrace)
	ctx := context.Background()

	f := newDepBotCrossHeadFake()
	f.createErr = depBotGitHubErr(http.StatusGone)
	store := depBotParkedStore()
	newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0, discardLogger())
	if newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) ||
		!store.redHeadJudged(depBotKey, "sha-2") || store.redHeadPending(depBotKey, "sha-2") {
		t.Fatal("a 410 on create left the head to retry")
	}
	reads := f.reads()
	newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past.Add(time.Hour), discardLogger())
	if f.reads() != reads {
		t.Fatal("a judged head was read again")
	}

	f2 := newDepBotCrossHeadFake()
	f2.createErr = depBotGitHubErr(http.StatusTooManyRequests)
	store = depBotParkedStore()
	newDepBotRedIssuePass(f2.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0, discardLogger())
	if !newDepBotRedIssuePass(f2.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger()) ||
		store.redHeadJudged(depBotKey, "sha-2") || !store.redHeadPending(depBotKey, "sha-2") {
		t.Fatal("a 429 on create was final")
	}

	// On the hand-off path too: the 4xx leaves nothing pending.
	h := newDepBotHarness(t)
	f3 := newDepBotRedIssueFake()
	f3.createErr = depBotGitHubErr(http.StatusUnprocessableEntity)
	h.redIssuePastGrace(depBotT0, f3.policy(), depBotRedBump("sha-1"))
	h.wantPlainHandoff(f3, "422 on create")
	if h.store.redHeadPending(depBotKey, "sha-1") {
		t.Fatal("a 422 on create left the hand-off head pending")
	}
}

// A head whose answer stays temporary is tried at most
// depBotRedIssueMaxAttempts times, then judged; and stuck PRs that fill the
// per-pass budget hold another PR back only until their attempts are spent.
func TestDependencyBotRedIssueRetriesCappedNoStarvation(t *testing.T) {
	h := newDepBotHarness(t)
	f := newDepBotRedIssueFake()
	f.runsErrHead = map[string]error{}
	var prs []github.PullRequest
	for i := 0; i < depBotRedIssueMaxPerPass+1; i++ {
		check := fmt.Sprintf("check-%d", i)
		f.baseGreen(check)
		head := fmt.Sprintf("stuck-%d", i)
		if i == depBotRedIssueMaxPerPass {
			head = "sha-ok"
		} else {
			f.runsErrHead[head] = errors.New("502")
		}
		pr := depBotRedBump(head)
		pr.Number = 60 + i
		pr.FailingChecks = []string{check}
		pr.CIFailureExcerpt = check + ": failed to compile"
		prs = append(prs, pr)
	}
	at := h.redIssuePastGrace(depBotT0, f.policy(), prs...)
	if f.creates != 0 || f.runReads != depBotRedIssueMaxPerPass || len(h.writer.comments) != len(prs) {
		t.Fatalf("hand-off pass: creates=%d run reads=%d comments=%d", f.creates, f.runReads, len(h.writer.comments))
	}
	parked := make([]github.PullRequest, len(prs))
	for i, pr := range prs {
		parked[i] = depBotParked(pr)
	}
	healthy := escalation.Key("acme/widgets", prs[depBotRedIssueMaxPerPass].Number)
	// Attempts 2 and 3 of every stuck PR fill the budget; the healthy PR
	// waits without spending an attempt.
	for i := 1; i <= depBotRedIssueMaxAttempts-1; i++ {
		h.redIssuePassAt(at.Add(time.Duration(i)*time.Minute), depBotAdoptPolicy{}, f.policy(), parked...)
		if f.creates != 0 || !h.store.redHeadPending(healthy, "sha-ok") {
			t.Fatalf("parked pass %d: creates=%d, want the healthy PR still waiting", i, f.creates)
		}
	}
	for i := 0; i < depBotRedIssueMaxPerPass; i++ {
		if key := escalation.Key("acme/widgets", prs[i].Number); !h.store.redHeadJudged(key, prs[i].HeadSHA) {
			t.Fatalf("stuck PR %d not judged after %d attempts", prs[i].Number, depBotRedIssueMaxAttempts)
		}
	}
	h.redIssuePassAt(at.Add(time.Hour), depBotAdoptPolicy{}, f.policy(), parked...)
	if f.creates != 1 || !h.store.redHeadJudged(healthy, "sha-ok") {
		t.Fatalf("after the stuck PRs gave up: creates=%d, want the healthy PR filed", f.creates)
	}
	if want := depBotRedIssueMaxPerPass*depBotRedIssueMaxAttempts + 1; f.runReads != want {
		t.Fatalf("run reads = %d, want %d (each stuck head capped, one for the healthy PR)", f.runReads, want)
	}
	reads := f.reads()
	h.redIssuePassAt(at.Add(2*time.Hour), depBotAdoptPolicy{}, f.policy(), parked...)
	if f.reads() != reads || f.creates != 1 {
		t.Fatalf("later pass: reads=%d (was %d) creates=%d, want nothing", f.reads(), reads, f.creates)
	}
}

// On the parked path the filer's cheap refusals come first: a repository
// not on GitHub, or a refused issue_file gate, reads nothing at all (not
// even the label events) and is final for the head.
func TestDependencyBotRedIssueParkedHeadCheapRefusalsFirst(t *testing.T) {
	newTestEscalationStore(t)
	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	past := depBotT0.Add(depBotRedCIGrace)
	for name, mut := range map[string]func(*depBotRedIssueFake, *depBotRedIssuePolicy){
		"not on GitHub": func(_ *depBotRedIssueFake, p *depBotRedIssuePolicy) { p.OnGitHub = func(string) bool { return false } },
		"gate refused":  func(f *depBotRedIssueFake, _ *depBotRedIssuePolicy) { f.gateClosed = true },
	} {
		f := newDepBotCrossHeadFake()
		pol := f.policy()
		mut(f, &pol)
		store := depBotParkedStore()
		for _, at := range []time.Time{depBotT0, past, past.Add(time.Hour)} {
			newDepBotRedIssuePass(pol, nil).fileForParkedHead(context.Background(), store, "acme/widgets", depBotKey, parked, at, discardLogger())
		}
		if f.reads() != 0 || f.creates != 0 || !store.redHeadJudged(depBotKey, "sha-2") {
			t.Fatalf("%s: reads=%d creates=%d judged=%v, want no read and a final answer", name, f.reads(), f.creates, store.redHeadJudged(depBotKey, "sha-2"))
		}
	}
}

// An "own label" answer is not kept: after a temporary failure the label
// events are read again, and a needs-human a person put back in between
// files nothing.
func TestDependencyBotRedIssueParkedHeadLabelReadEachAttempt(t *testing.T) {
	newTestEscalationStore(t)
	parked := depBotParked(depBotCrossHeadPR("sha-2"))
	past := depBotT0.Add(depBotRedCIGrace)
	ctx := context.Background()
	f := newDepBotCrossHeadFake()
	f.runsErr = errors.New("502")
	store := depBotParkedStore()
	newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, depBotT0, discardLogger())
	newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past, discardLogger())
	if f.labelReads != 1 || !store.redHeadPending(depBotKey, "sha-2") {
		t.Fatalf("first attempt: label reads=%d pending=%v, want one read and a retry", f.labelReads, store.redHeadPending(depBotKey, "sha-2"))
	}
	f.runsErr, f.labelNotOwn = nil, true
	newDepBotRedIssuePass(f.policy(), nil).fileForParkedHead(ctx, store, "acme/widgets", depBotKey, parked, past.Add(time.Hour), discardLogger())
	if f.creates != 0 || f.labelReads != 2 || !store.redHeadJudged(depBotKey, "sha-2") {
		t.Fatalf("person's label after a retry: creates=%d label reads=%d judged=%v, want nothing filed", f.creates, f.labelReads, store.redHeadJudged(depBotKey, "sha-2"))
	}
}

// The attempt counter: a spent budget is no attempt, a new head starts
// over, and the last attempt judges the head.
func TestDependencyBotRedHeadPendingAttempts(t *testing.T) {
	s := loadDepBotConflictStore("")
	if s.markRedHeadPending("k", "a", false) || s.markRedHeadPending("k", "a", false) {
		t.Fatal("budget deferrals used up the head")
	}
	for i := 1; i < depBotRedIssueMaxAttempts; i++ {
		if s.markRedHeadPending("k", "a", true) {
			t.Fatalf("attempt %d used up the head", i)
		}
	}
	if s.markRedHeadPending("k", "b", true) || s.redHeads["k"].Attempts != 1 {
		t.Fatal("a new head did not start over")
	}
	for i := 2; i < depBotRedIssueMaxAttempts; i++ {
		s.markRedHeadPending("k", "b", true)
	}
	if !s.markRedHeadPending("k", "b", true) || !s.redHeadJudged("k", "b") || s.redHeadPending("k", "b") {
		t.Fatal("the last attempt did not judge the head")
	}
}
