package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// Opt-in per-repo advisory reports (#18448): governor.advisory.per_repo,
// default off. Off keeps the #18568 rule (one report, in the primary repo).

// fakeRepoIssueAlerts records the per-repo advisory-report alerts.
type fakeRepoIssueAlerts struct {
	alerts map[string]string
}

func newFakeRepoIssueAlerts() *fakeRepoIssueAlerts {
	return &fakeRepoIssueAlerts{alerts: map[string]string{}}
}

func (f *fakeRepoIssueAlerts) AddSystemAlert(id, severity, message string) {
	f.alerts[id] = severity + ": " + message
}

func (f *fakeRepoIssueAlerts) ClearSystemAlertsExcept(prefix string, keep map[string]struct{}) {
	for id := range f.alerts {
		if !strings.HasPrefix(id, prefix) {
			continue
		}
		if _, ok := keep[id]; !ok {
			delete(f.alerts, id)
		}
	}
}

// fakeReportForge stands in for GitHub behind EnsureSecondaryAdvisoryIssue:
// one report per repo, found (open or closed) before anything is created.
type fakeReportForge struct {
	open    map[string]int   // repo -> open report
	closed  map[string]int   // repo -> report a human closed
	refuse  map[string]error // repo -> error every ensure returns
	creates map[string]int
	calls   map[string]int
	next    int
}

func newFakeReportForge() *fakeReportForge {
	return &fakeReportForge{
		open: map[string]int{}, closed: map[string]int{}, refuse: map[string]error{},
		creates: map[string]int{}, calls: map[string]int{}, next: 100,
	}
}

func (f *fakeReportForge) ensure(_ context.Context, repo string) (int, error) {
	f.calls[repo]++
	if err := f.refuse[repo]; err != nil {
		return 0, err
	}
	if n, ok := f.open[repo]; ok {
		return n, nil
	}
	if n, ok := f.closed[repo]; ok {
		return 0, &github.AdvisoryReportClosedError{Repo: repo, Number: n}
	}
	f.next++
	f.open[repo] = f.next
	f.creates[repo]++
	return f.next, nil
}

func perRepoConfig(on *bool, primary string, repos ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.PrimaryRepo = primary
	cfg.Project.Repos = repos
	cfg.Governor.Advisory.PerRepo = on
	return cfg
}

func TestAdvisorySecondaryRepos_AllButPrimaryDeduped(t *testing.T) {
	cfg := perRepoConfig(nil, "", "hive", "crux", "hivecommons/Crux", "other/notes")
	if got, want := fmt.Sprint(advisorySecondaryRepos(cfg)), fmt.Sprint([]string{"crux", "other/notes"}); got != want {
		t.Fatalf("secondary repos = %s, want %s", got, want)
	}
	cfg.Project.PrimaryRepo = "crux"
	if got, want := fmt.Sprint(advisorySecondaryRepos(cfg)), fmt.Sprint([]string{"hive", "other/notes"}); got != want {
		t.Fatalf("explicit primary must be excluded: got %s, want %s", got, want)
	}
	if got := advisorySecondaryRepos(perRepoConfig(nil, "", "only")); len(got) != 0 {
		t.Fatalf("single-repo hive has no secondary repos, got %v", got)
	}
}

// TestAdvisoryPerRepo_DefaultOffCreatesNothingOutsidePrimary: with per_repo
// absent (and the env override unset) or explicitly false, the eval cycle's
// secondary list is empty, so no repo but the primary is ever asked.
func TestAdvisoryPerRepo_DefaultOffCreatesNothingOutsidePrimary(t *testing.T) {
	t.Setenv(config.AdvisoryPerRepoEnvVar, "")
	for name, on := range map[string]*bool{"absent": nil, "explicit false": boolPtr(false)} {
		t.Run(name, func(t *testing.T) {
			cfg := perRepoConfig(on, "hive", "hive", "crux", "dibs", "docs")
			if got := advisoryPerRepoSecondaryRepos(cfg); len(got) != 0 {
				t.Fatalf("per_repo off: secondary repos = %v, want none", got)
			}
			forge := newFakeReportForge()
			issues := map[string]int{"hive": 9}
			for cycle := 0; cycle < 3; cycle++ {
				ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
					forge.ensure, newAdvisoryRepoIssueBackoff(), newFakeRepoIssueAlerts(), time.Now(), nil)
			}
			if len(forge.calls) != 0 || len(forge.creates) != 0 {
				t.Fatalf("per_repo off asked %v / created %v outside the primary", forge.calls, forge.creates)
			}
			if len(issues) != 1 {
				t.Fatalf("advisory issues = %v, want only the primary", issues)
			}
		})
	}
}

func TestAdvisoryPerRepo_EnvOverrideOnlyWhenUnset(t *testing.T) {
	t.Setenv(config.AdvisoryPerRepoEnvVar, "true")
	if got := advisoryPerRepoSecondaryRepos(perRepoConfig(nil, "hive", "hive", "crux")); fmt.Sprint(got) != "[crux]" {
		t.Fatalf("env on, key absent: got %v, want [crux]", got)
	}
	if got := advisoryPerRepoSecondaryRepos(perRepoConfig(boolPtr(false), "hive", "hive", "crux")); len(got) != 0 {
		t.Fatalf("explicit false must beat the env: got %v", got)
	}
}

// TestAdvisoryPerRepo_OnCreatesOnePerRepoOnce: switched on, each secondary
// repo gets exactly one report, and later cycles cost no further calls.
func TestAdvisoryPerRepo_OnCreatesOnePerRepoOnce(t *testing.T) {
	cfg := perRepoConfig(boolPtr(true), "hive", "hive", "crux", "dibs")
	forge := newFakeReportForge()
	issues := map[string]int{"hive": 9}
	gate := newAdvisoryRepoIssueBackoff()
	const cycles = 4
	for cycle := 0; cycle < cycles; cycle++ {
		if errs := ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
			forge.ensure, gate, newFakeRepoIssueAlerts(), time.Now(), nil); len(errs) != 0 {
			t.Fatalf("cycle %d: %v", cycle, errs)
		}
	}
	for _, repo := range []string{"crux", "dibs"} {
		if forge.creates[repo] != 1 || forge.calls[repo] != 1 {
			t.Fatalf("%s: %d creates over %d calls, want exactly one of each", repo, forge.creates[repo], forge.calls[repo])
		}
		if issues[repo] <= 0 {
			t.Fatalf("%s has no recorded report: %v", repo, issues)
		}
	}
	if issues["crux"] == issues["dibs"] {
		t.Fatalf("each repo needs its own report, got %v", issues)
	}
	if forge.calls["hive"] != 0 {
		t.Fatal("the primary is owned by the primary path, not the per-repo helper")
	}
}

// TestAdvisoryPerRepo_RestartReusesExistingReports: a restart (empty map,
// fresh gate) adopts each repo's existing report and creates none.
func TestAdvisoryPerRepo_RestartReusesExistingReports(t *testing.T) {
	cfg := perRepoConfig(boolPtr(true), "hive", "hive", "crux", "dibs")
	forge := newFakeReportForge()
	forge.open["crux"], forge.open["dibs"] = 501, 502
	const restarts = 3
	for i := 0; i < restarts; i++ {
		issues := map[string]int{}
		ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
			forge.ensure, newAdvisoryRepoIssueBackoff(), newFakeRepoIssueAlerts(), time.Now(), nil)
		if issues["crux"] != 501 || issues["dibs"] != 502 {
			t.Fatalf("restart %d: issues = %v, want the existing reports", i, issues)
		}
	}
	if len(forge.creates) != 0 {
		t.Fatalf("restarts created reports: %v", forge.creates)
	}
}

// TestAdvisoryPerRepo_HumanClosedReportNotReopenedOrReplaced: a closed
// secondary report is left alone, this run and after a restart, and nothing
// is posted to it.
func TestAdvisoryPerRepo_HumanClosedReportNotReopenedOrReplaced(t *testing.T) {
	cfg := perRepoConfig(boolPtr(true), "hive", "hive", "crux")
	forge := newFakeReportForge()
	forge.closed["crux"] = 777
	for restart := 0; restart < 2; restart++ {
		issues := map[string]int{"hive": 9}
		gate := newAdvisoryRepoIssueBackoff()
		for cycle := 0; cycle < 3; cycle++ {
			ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
				forge.ensure, gate, newFakeRepoIssueAlerts(), time.Now(), nil)
		}
		if _, ok := issues["crux"]; ok {
			t.Fatalf("closed report recorded as a post target: %v", issues)
		}
		var posted []string
		postAdvisoryDigestToSecondaryRepos(context.Background(), "digest", issues, advisoryPerRepoSecondaryRepos(cfg), gate,
			advisorySecondaryPostDeps{post: func(_ context.Context, repo string, _ int, _ string) error {
				posted = append(posted, repo)
				return nil
			}}, time.Now(), nil)
		if len(posted) != 0 {
			t.Fatalf("posted to %v, want nothing", posted)
		}
	}
	if forge.creates["crux"] != 0 {
		t.Fatalf("a replacement for the closed report was created")
	}
	if forge.calls["crux"] != 2 {
		t.Fatalf("closed report looked up %d times over 2 runs, want once per run", forge.calls["crux"])
	}
}

// TestAdvisoryPerRepo_ClosedMidRunStopsPosting: a report closed after it was
// resolved is dropped at the next open check and never posted to again.
func TestAdvisoryPerRepo_ClosedMidRunStopsPosting(t *testing.T) {
	cfg := perRepoConfig(boolPtr(true), "hive", "hive", "crux", "dibs")
	issues := map[string]int{"hive": 9, "crux": 501, "dibs": 502}
	gate := newAdvisoryRepoIssueBackoff()
	open := map[string]bool{"crux": false, "dibs": true}
	var posted []string
	deps := advisorySecondaryPostDeps{
		post: func(_ context.Context, repo string, _ int, _ string) error {
			posted = append(posted, repo)
			return nil
		},
		isOpen: func(_ context.Context, repo string, _ int) (bool, error) { return open[repo], nil },
	}
	now := time.Now()
	postAdvisoryDigestToSecondaryRepos(context.Background(), "digest", issues, advisoryPerRepoSecondaryRepos(cfg), gate, deps, now, nil)
	postAdvisoryDigestToSecondaryRepos(context.Background(), "digest", issues, advisoryPerRepoSecondaryRepos(cfg), gate, deps,
		now.Add(2*advisorySecondaryOpenCheckInterval), nil)
	if fmt.Sprint(posted) != "[dibs dibs]" {
		t.Fatalf("posted = %v, want dibs twice and never crux", posted)
	}
	if _, ok := issues["crux"]; ok {
		t.Fatal("closed report still recorded as a post target")
	}
	forge := newFakeReportForge()
	ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
		forge.ensure, gate, newFakeRepoIssueAlerts(), now, nil)
	if forge.calls["crux"] != 0 {
		t.Fatal("a report closed mid-run was looked up (and could be re-created) again")
	}
}

// TestAdvisoryPerRepo_IssuesDisabledRepoSkipped: a repo with Issues disabled
// is skipped with one warning log and one dashboard alert, not retried every
// cycle, and does not hold up the other repos.
func TestAdvisoryPerRepo_IssuesDisabledRepoSkipped(t *testing.T) {
	cfg := perRepoConfig(boolPtr(true), "hive", "hive", "fork", "crux")
	forge := newFakeReportForge()
	forge.refuse["fork"] = &github.IssuesDisabledError{Repo: "hivecommons/fork", Fork: true}
	issues := map[string]int{"hive": 9}
	gate := newAdvisoryRepoIssueBackoff()
	alerts := newFakeRepoIssueAlerts()
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo}))
	now := time.Now()
	const cycles = 5
	for cycle := 0; cycle < cycles; cycle++ {
		ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
			forge.ensure, gate, alerts, now.Add(time.Duration(cycle)*time.Minute), logger)
	}
	if forge.calls["fork"] != 1 {
		t.Fatalf("Issues-disabled repo asked %d times in %d cycles, want once", forge.calls["fork"], cycles)
	}
	if forge.creates["fork"] != 0 {
		t.Fatal("created a report in an Issues-disabled repo")
	}
	if issues["crux"] <= 0 {
		t.Fatalf("other repos must still get their report: %v", issues)
	}
	if _, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"fork"]; !ok {
		t.Fatalf("no dashboard warning for the skipped repo: %v", alerts.alerts)
	}
	// Past the backoff it is checked once more, still without a second warning.
	ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
		forge.ensure, gate, alerts, now.Add(2*advisoryRepoIssueRefusedBackoff), logger)
	if forge.calls["fork"] != 2 {
		t.Fatalf("repo not re-checked after the backoff: %d calls", forge.calls["fork"])
	}
	if n := strings.Count(logs.String(), "per-repo advisory report skipped for repo"); n != 1 {
		t.Fatalf("skip warning logged %d times, want once:\n%s", n, logs.String())
	}
}

// TestAdvisoryPerRepo_SwitchedOffStopsUpdatingAndClearsAlerts: turning the
// setting off stops every per-repo post and clears per-repo alerts, without
// any call that could close a report.
func TestAdvisoryPerRepo_SwitchedOffStopsUpdatingAndClearsAlerts(t *testing.T) {
	cfg := perRepoConfig(boolPtr(false), "hive", "hive", "crux")
	issues := map[string]int{"hive": 9, "crux": 501}
	alerts := newFakeRepoIssueAlerts()
	alerts.alerts[advisoryRepoIssueAlertPrefix+"crux"] = "warning: stale"
	forge := newFakeReportForge()
	ensureSecondaryAdvisoryIssues(context.Background(), issues, advisoryPerRepoSecondaryRepos(cfg),
		forge.ensure, newAdvisoryRepoIssueBackoff(), alerts, time.Now(), nil)
	if len(alerts.alerts) != 0 {
		t.Fatalf("per-repo alerts not cleared when switched off: %v", alerts.alerts)
	}
	posted := postAdvisoryDigestToSecondaryRepos(context.Background(), "digest", issues, advisoryPerRepoSecondaryRepos(cfg),
		newAdvisoryRepoIssueBackoff(), advisorySecondaryPostDeps{post: func(context.Context, string, int, string) error {
			return errors.New("must not post")
		}}, time.Now(), nil)
	if len(posted) != 0 || len(forge.calls) != 0 {
		t.Fatalf("switched off still touched secondary repos: posted %v, ensure calls %v", posted, forge.calls)
	}
}

// TestAdvisorySecondaryHelpers_OnlyFedThePerRepoGatedList guards the eval
// cycle source: every call of a per-repo helper in package main must take
// its repo list straight from advisoryPerRepoSecondaryRepos, so the default
// (off) can never fan out to every repo again (#18568).
func TestAdvisorySecondaryHelpers_OnlyFedThePerRepoGatedList(t *testing.T) {
	// helpers maps each per-repo helper to the index of its repos argument.
	helpers := map[string]int{"ensureSecondaryAdvisoryIssues": 2, "postAdvisoryDigestToSecondaryRepos": 3}
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	seen := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			repoArg, isHelper := helpers[calledName(call.Fun)]
			if !isHelper {
				return true
			}
			seen++
			if len(call.Args) <= repoArg {
				t.Errorf("%s: unexpected call shape", fset.Position(call.Pos()))
				return true
			}
			inner, ok := call.Args[repoArg].(*ast.CallExpr)
			if !ok || calledName(inner.Fun) != "advisoryPerRepoSecondaryRepos" {
				t.Errorf("%s: %s must be fed advisoryPerRepoSecondaryRepos(cfg) so per-repo reports stay opt-in",
					fset.Position(call.Pos()), calledName(call.Fun))
			}
			return true
		})
	}
	if seen != len(helpers) {
		t.Fatalf("found %d per-repo helper calls in package main, want %d (one each)", seen, len(helpers))
	}
}
