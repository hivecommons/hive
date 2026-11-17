package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// fakeRepoIssueAlerts records the per-repo governor-issue alerts.
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

// fakeGovernorForge stands in for GitHub: one governor issue per repo, found
// by EnsureAdvisoryIssue's search before it creates, as the real client does.
type fakeGovernorForge struct {
	existing map[string]int   // repo -> governor issue already on GitHub
	refuse   map[string]error // repo -> error every ensure returns
	creates  map[string]int   // repo -> issues created
	calls    map[string]int   // repo -> ensure calls
	next     int
}

func newFakeGovernorForge() *fakeGovernorForge {
	return &fakeGovernorForge{existing: map[string]int{}, refuse: map[string]error{}, creates: map[string]int{}, calls: map[string]int{}, next: 100}
}

func (f *fakeGovernorForge) ensure(_ context.Context, repo string) (int, error) {
	f.calls[repo]++
	if err := f.refuse[repo]; err != nil {
		return 0, err
	}
	if n, ok := f.existing[repo]; ok {
		return n, nil
	}
	f.next++
	f.existing[repo] = f.next
	f.creates[repo]++
	return f.next, nil
}

func multiRepoConfig(repos ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "nate"
	cfg.Project.Repos = repos
	return cfg
}

func TestAdvisorySecondaryRepos_AllButPrimaryDeduped(t *testing.T) {
	cfg := multiRepoConfig("pr-pal", "talks", "nate/Talks", "other/notes")
	got := advisorySecondaryRepos(cfg)
	want := []string{"talks", "other/notes"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("secondary repos = %v, want %v", got, want)
	}
	if got := advisorySecondaryRepos(multiRepoConfig("only")); got != nil {
		t.Fatalf("single-repo hive has no secondary repos, got %v", got)
	}
	cfg.Project.PrimaryRepo = "talks"
	if got := advisorySecondaryRepos(cfg); fmt.Sprint(got) != fmt.Sprint([]string{"pr-pal", "other/notes"}) {
		t.Fatalf("explicit primary must be excluded, got %v", got)
	}
}

// TestEnsureSecondaryAdvisoryIssues_EveryRepoGetsOne is #18448: a two-repo hive
// must end with a governor issue in BOTH repos, numbers stored per repo.
func TestEnsureSecondaryAdvisoryIssues_EveryRepoGetsOne(t *testing.T) {
	cfg := multiRepoConfig("pr-pal", "talks", "slides")
	forge := newFakeGovernorForge()
	forge.existing["pr-pal"] = 9 // the primary, ensured at boot
	issues := map[string]int{"pr-pal": 9}

	errs := ensureSecondaryAdvisoryIssues(context.Background(), issues, advisorySecondaryRepos(cfg),
		forge.ensure, newAdvisoryRepoIssueBackoff(), newFakeRepoIssueAlerts(), time.Now(), nil)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	for _, repo := range []string{"pr-pal", "talks", "slides"} {
		if n, ok := issues[repo]; !ok || n <= 0 {
			t.Fatalf("repo %s has no governor issue: %v", repo, issues)
		}
	}
	if issues["talks"] == issues["slides"] {
		t.Fatalf("each repo needs its own issue number, got %v", issues)
	}
	if forge.calls["pr-pal"] != 0 {
		t.Fatalf("primary is owned by the primary path, ensured %d times here", forge.calls["pr-pal"])
	}
}

// TestEnsureSecondaryAdvisoryIssues_IdempotentAcrossCyclesAndRestart: a resolved
// repo costs no call on later cycles, and a restart (empty map) reuses the
// existing issue instead of creating a duplicate.
func TestEnsureSecondaryAdvisoryIssues_IdempotentAcrossCyclesAndRestart(t *testing.T) {
	repos := []string{"talks"}
	forge := newFakeGovernorForge()
	issues := map[string]int{}
	gate := newAdvisoryRepoIssueBackoff()
	now := time.Now()

	for cycle := 0; cycle < 3; cycle++ {
		ensureSecondaryAdvisoryIssues(context.Background(), issues, repos, forge.ensure, gate, nil, now, nil)
	}
	if forge.calls["talks"] != 1 {
		t.Fatalf("resolved repo re-ensured: %d calls, want 1", forge.calls["talks"])
	}
	first := issues["talks"]

	restarted := map[string]int{}
	ensureSecondaryAdvisoryIssues(context.Background(), restarted, repos, forge.ensure, newAdvisoryRepoIssueBackoff(), nil, now, nil)
	if restarted["talks"] != first {
		t.Fatalf("restart resolved #%d, want existing #%d", restarted["talks"], first)
	}
	if forge.creates["talks"] != 1 {
		t.Fatalf("restart created a duplicate: %d creates", forge.creates["talks"])
	}
}

// TestEnsureSecondaryAdvisoryIssues_RepoAddedLater: a repo added in Settings
// after boot gets its issue on the next cycle, with no restart.
func TestEnsureSecondaryAdvisoryIssues_RepoAddedLater(t *testing.T) {
	cfg := multiRepoConfig("pr-pal")
	forge := newFakeGovernorForge()
	issues := map[string]int{"pr-pal": 9}
	gate := newAdvisoryRepoIssueBackoff()
	now := time.Now()

	ensureSecondaryAdvisoryIssues(context.Background(), issues, advisorySecondaryRepos(cfg), forge.ensure, gate, nil, now, nil)
	if len(issues) != 1 {
		t.Fatalf("single-repo hive touched other repos: %v", issues)
	}
	cfg.Project.Repos = append(cfg.Project.Repos, "talks")
	ensureSecondaryAdvisoryIssues(context.Background(), issues, advisorySecondaryRepos(cfg), forge.ensure, gate, nil, now.Add(time.Minute), nil)
	if _, ok := advisoryIssueNumber(issues, "talks"); !ok {
		t.Fatalf("repo added later got no governor issue: %v", issues)
	}
}

// TestEnsureSecondaryAdvisoryIssues_NoWriteAccess: a repo the App cannot write
// is logged, surfaced as a per-repo alert and backed off, without stopping the
// other repos; once access is granted the alert clears.
func TestEnsureSecondaryAdvisoryIssues_NoWriteAccess(t *testing.T) {
	repos := []string{"talks", "slides"}
	forge := newFakeGovernorForge()
	forge.refuse["talks"] = errors.New("creating advisory issue: POST https://api.github.com/repos/nate/talks/issues: 403 Resource not accessible by integration []")
	issues := map[string]int{}
	gate := newAdvisoryRepoIssueBackoff()
	alerts := newFakeRepoIssueAlerts()
	now := time.Now()

	errs := ensureSecondaryAdvisoryIssues(context.Background(), issues, repos, forge.ensure, gate, alerts, now, nil)
	if errs["talks"] == nil {
		t.Fatal("refused repo must report its error")
	}
	if _, ok := advisoryIssueNumber(issues, "slides"); !ok {
		t.Fatalf("a refused repo must not block the others: %v", issues)
	}
	alert, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"talks"]
	if !ok || !strings.Contains(alert, "talks") || !strings.HasPrefix(alert, advisoryRepoIssueAlertSeverity) {
		t.Fatalf("no-write-access repo not surfaced: %v", alerts.alerts)
	}

	// Inside the backoff: no new call, alert kept.
	ensureSecondaryAdvisoryIssues(context.Background(), issues, repos, forge.ensure, gate, alerts, now.Add(time.Minute), nil)
	if forge.calls["talks"] != 1 {
		t.Fatalf("refused repo retried inside backoff: %d calls", forge.calls["talks"])
	}
	if _, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"talks"]; !ok {
		t.Fatal("alert dropped while the repo is still refused")
	}

	// Access granted: the retry after the backoff resolves and clears it.
	delete(forge.refuse, "talks")
	ensureSecondaryAdvisoryIssues(context.Background(), issues, repos, forge.ensure, gate, alerts, now.Add(advisoryRepoIssueRefusedBackoff+time.Minute), nil)
	if _, ok := advisoryIssueNumber(issues, "talks"); !ok {
		t.Fatalf("repo not resolved after access granted: %v", issues)
	}
	if len(alerts.alerts) != 0 {
		t.Fatalf("alert not cleared after the repo resolved: %v", alerts.alerts)
	}
}

func TestEnsureSecondaryAdvisoryIssues_TransientRetriesNextCycleWithoutAlert(t *testing.T) {
	forge := newFakeGovernorForge()
	forge.refuse["talks"] = errors.New("searching for advisory issue: 502 Bad Gateway")
	issues := map[string]int{}
	gate := newAdvisoryRepoIssueBackoff()
	alerts := newFakeRepoIssueAlerts()
	now := time.Now()

	ensureSecondaryAdvisoryIssues(context.Background(), issues, []string{"talks"}, forge.ensure, gate, alerts, now, nil)
	ensureSecondaryAdvisoryIssues(context.Background(), issues, []string{"talks"}, forge.ensure, gate, alerts, now.Add(time.Minute), nil)
	if forge.calls["talks"] != 2 {
		t.Fatalf("transient failure must retry every cycle: %d calls", forge.calls["talks"])
	}
	if len(alerts.alerts) != 0 {
		t.Fatalf("transient failure raised an alert: %v", alerts.alerts)
	}
}

func TestEnsureSecondaryAdvisoryIssues_IssuesDisabledAndPaused(t *testing.T) {
	forge := newFakeGovernorForge()
	forge.refuse["fork"] = fmt.Errorf("ensure: %w", &github.IssuesDisabledError{Repo: "nate/fork", Fork: true})
	forge.refuse["held"] = &github.RepoPausedError{Repo: "nate/held"}
	alerts := newFakeRepoIssueAlerts()

	ensureSecondaryAdvisoryIssues(context.Background(), map[string]int{}, []string{"fork", "held"}, forge.ensure, newAdvisoryRepoIssueBackoff(), alerts, time.Now(), nil)
	if _, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"fork"]; !ok {
		t.Fatalf("Issues-disabled repo not surfaced: %v", alerts.alerts)
	}
	if _, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"held"]; ok {
		t.Fatal("a paused repo is the operator's choice, not an alert")
	}
}

func TestEnsureSecondaryAdvisoryIssues_RemovedRepoAlertCleared(t *testing.T) {
	alerts := newFakeRepoIssueAlerts()
	alerts.AddSystemAlert(advisoryRepoIssueAlertPrefix+"gone", advisoryRepoIssueAlertSeverity, "stale")
	alerts.AddSystemAlert("unrelated", "error", "kept")
	ensureSecondaryAdvisoryIssues(context.Background(), map[string]int{}, nil, newFakeGovernorForge().ensure, newAdvisoryRepoIssueBackoff(), alerts, time.Now(), nil)
	if _, ok := alerts.alerts[advisoryRepoIssueAlertPrefix+"gone"]; ok {
		t.Fatal("alert for a repo no longer managed was not cleared")
	}
	if _, ok := alerts.alerts["unrelated"]; !ok {
		t.Fatal("cleared an alert outside the governor-issue prefix")
	}
}

func TestPostAdvisoryDigestToSecondaryRepos(t *testing.T) {
	issues := map[string]int{"talks": 3, "slides": 4, "locked": 5}
	var posts []string
	var forbidden []string
	deps := advisorySecondaryPostDeps{
		post: func(_ context.Context, repo string, n int, md string) error {
			if repo == "locked" {
				return errors.New("403 Resource not accessible by integration")
			}
			posts = append(posts, fmt.Sprintf("%s#%d", repo, n))
			return nil
		},
		due:             func(repo string) bool { return repo != "slides" },
		recordForbidden: func(repo string) { forbidden = append(forbidden, repo) },
	}
	got := postAdvisoryDigestToSecondaryRepos(context.Background(), "digest", issues, []string{"talks", "slides", "locked", "unresolved"}, deps, nil)
	if fmt.Sprint(got) != "[talks]" || fmt.Sprint(posts) != "[talks#3]" {
		t.Fatalf("posted = %v (%v), want only talks#3", got, posts)
	}
	if fmt.Sprint(forbidden) != "[locked]" {
		t.Fatalf("forbidden = %v, want [locked]", forbidden)
	}
	if got := postAdvisoryDigestToSecondaryRepos(context.Background(), "", issues, []string{"talks"}, deps, nil); got != nil {
		t.Fatalf("empty digest posted: %v", got)
	}
}
