package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/prfollowup"
)

type fakePRFollowUpSessions map[string]string

func (f fakePRFollowUpSessions) SessionID(name string) (string, bool) {
	s, ok := f[name]
	return s, ok
}

// prFollowUpTestDir points the pointer store at a temp dir and resets the
// eval-tick state (comment cache, sweep clock) around the test.
func prFollowUpTestDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv(prfollowup.DirEnvVar, dir)
	resetPRFollowUpTickState()
	t.Cleanup(resetPRFollowUpTickState)
	return dir
}

func resetPRFollowUpTickState() {
	prFollowUpCommentsLastRefresh = time.Time{}
	prFollowUpCommentsCache = nil
	prFollowUpLastSweep = time.Time{}
}

func redFollowUpActionable() *github.ActionableResult {
	return &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{{
		Repo: "hivecommons/hive", Number: 9, CIStatus: "failure", HeadSHA: "abc1234def",
	}}}}
}

const prFollowUpTestBody = "## Why\nthe scheduler dropped kicks on reload\n\n## Approach\nre-read the config under the lock\n\n## Alternatives considered\na second watcher goroutine"

func openedDetail(repo string) github.PROpenedDetail {
	return github.PROpenedDetail{Agent: "scanner", Repo: repo, Number: 9, URL: "https://example.invalid/pr/9", Body: prFollowUpTestBody}
}

func TestPRFollowUpWiring_DefaultOffIsInert(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "")
	dir := prFollowUpTestDir(t)
	cfg := &config.Config{}

	recordPRFollowUpPointer(cfg, fakePRFollowUpSessions{"scanner": "s1"}, openedDetail("hivecommons/hive"), time.Now(), discardLogger())
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("default-off hook wrote %d file(s)", len(entries))
	}
	if out := routePRFollowUps(context.Background(), cfg, nil, redFollowUpActionable(), nil, nil, discardLogger()); out != nil {
		t.Fatalf("default-off router produced outcomes: %+v", out)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("default-off router wrote %d file(s) (counters or sweep ran)", len(entries))
	}
}

func TestPRFollowUpWiring_EnabledRecordsAndRoutes(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	threadsPath := redirectReviewThreadsPath(t)
	cfg := &config.Config{}

	if err := github.WriteReviewThreadsReport(threadsPath, github.ReviewThreadsReport{Enabled: true, PRs: []github.ReviewThreadPR{{
		Repo: "hivecommons/hive", Number: 9, Threads: []github.ReviewThread{{ThreadID: "PRRT_x", Path: "a.go", Author: "bot", Body: "fix"}},
	}}}); err != nil {
		t.Fatal(err)
	}

	recordPRFollowUpPointer(cfg, fakePRFollowUpSessions{"scanner": "s1"}, openedDetail("hivecommons/hive"), time.Now(), discardLogger())

	// No agent manager: the pointer makes the PR eligible, both the red CI
	// and the review-bot thread are detected, and it falls back cleanly.
	out := routePRFollowUps(context.Background(), cfg, nil, redFollowUpActionable(), nil, nil, discardLogger())
	if len(out) != 1 || out[0].Route != prfollowup.RouteFallback || out[0].Reason != prfollowup.ReasonNoResumer || len(out[0].Events) != 2 {
		t.Fatalf("outcomes = %+v, want one fallback with CI + thread events", out)
	}
	// The fallback's fresh kick carries the note extracted from the PR body.
	section := prfollowup.HandoffSection(context.Background(), dir, "scanner")
	for _, want := range []string{"Why: the scheduler dropped kicks on reload", "Approach: re-read the config under the lock", "Rejected alternatives: a second watcher goroutine"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section missing %q:\n%s", want, section)
		}
	}

	// An escalated PR is a human's now: never routed.
	escalated := map[string]bool{escalation.Key("hivecommons/hive", 9): true}
	next := redFollowUpActionable()
	next.PRs.Items[0].HeadSHA = "new0000sha"
	if out := routePRFollowUps(context.Background(), cfg, nil, next, escalated, nil, discardLogger()); len(out) != 0 {
		t.Fatalf("escalated PR routed: %+v", out)
	}
	if out := routePRFollowUps(context.Background(), cfg, nil, nil, nil, nil, discardLogger()); out != nil {
		t.Fatalf("nil actionable routed: %+v", out)
	}
}

// A repo configured (and requested) bare is qualified with the org on both
// sides, so the pointer, the router and the review-thread report agree.
func TestPRFollowUpWiring_BareRepoIsQualified(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	prFollowUpTestDir(t)
	redirectReviewThreadsPath(t)
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons"}}

	recordPRFollowUpPointer(cfg, nil, openedDetail("hive"), time.Now(), discardLogger())
	bare := redFollowUpActionable()
	bare.PRs.Items[0].Repo = "hive"
	out := routePRFollowUps(context.Background(), cfg, nil, bare, map[string]bool{}, nil, discardLogger())
	if len(out) != 1 || out[0].Repo != "hivecommons/hive" {
		t.Fatalf("outcomes = %+v, want the bare repo routed as hivecommons/hive", out)
	}
	// An escalation recorded under the bare spelling still skips the PR.
	bare.PRs.Items[0].HeadSHA = "another0sha"
	if out := routePRFollowUps(context.Background(), cfg, nil, bare, map[string]bool{escalation.Key("hive", 9): true}, nil, discardLogger()); len(out) != 0 {
		t.Fatalf("bare-keyed escalation not honoured: %+v", out)
	}
	if got := qualifyPRFollowUpRepo("", "hive"); got != "hive" {
		t.Fatalf("no org: %q", got)
	}
}

// Human feedback is fetched only for PRs with a pointer, the hive's own
// reply never becomes a follow-up, the fetch is rate-limited, and feedback
// that cannot be resumed reaches the next fresh kick with the note.
func TestPRFollowUpWiring_HumanCommentsRouteAndOwnRepliesDoNot(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	redirectReviewThreadsPath(t)
	var graphQLCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/graphql") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		graphQLCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":{"repository":{"pullRequest":{
			"comments":{"nodes":[
				{"databaseId":71,"url":"u71","body":"please also cover the reload path","createdAt":"2099-01-01T00:00:00Z","authorAssociation":"MEMBER","author":{"__typename":"User","login":"alice"}},
				{"databaseId":72,"url":"u72","body":"done, pushed abc123","createdAt":"2099-01-01T00:05:00Z","authorAssociation":"NONE","author":{"__typename":"Bot","login":"hive-app"}},
				{"databaseId":73,"url":"u73","body":"LGTM from the CI bot","createdAt":"2099-01-01T00:06:00Z","authorAssociation":"NONE","author":{"__typename":"User","login":"ci-helper[bot]"}}
			]},
			"reviews":{"nodes":[]},
			"reviewThreads":{"nodes":[]}
		}}}}`))
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, discardLogger())
	client.SetAppBotLogin("hive-app[bot]")
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons"}}

	recordPRFollowUpPointer(cfg, nil, openedDetail("hivecommons/hive"), time.Now(), discardLogger())
	// A PR with no pointer is never read.
	actionable := redFollowUpActionable()
	actionable.PRs.Items[0].CIStatus = "success"
	actionable.PRs.Items = append(actionable.PRs.Items, github.PullRequest{Repo: "hivecommons/hive", Number: 10})

	out := routePRFollowUps(context.Background(), cfg, client, actionable, nil, nil, discardLogger())
	if len(out) != 1 || len(out[0].Events) != 1 || out[0].Events[0].Key != "comment:conversation:71" || out[0].Queued != 1 {
		t.Fatalf("outcomes = %+v, want only alice's comment, queued for the next kick", out)
	}
	if n := graphQLCalls.Load(); n != 1 {
		t.Fatalf("GraphQL calls = %d, want 1 (only the PR with a pointer)", n)
	}
	section := prfollowup.HandoffSection(context.Background(), dir, "scanner")
	if !strings.Contains(section, "please also cover the reload path") || strings.Contains(section, "pushed abc123") || strings.Contains(section, "CI bot") {
		t.Fatalf("handoff section = %s", section)
	}

	// Within the refresh interval the cached result is reused (no new call)
	// and the already-handled comment is not routed again.
	if again := routePRFollowUps(context.Background(), cfg, client, actionable, nil, nil, discardLogger()); len(again) != 0 {
		t.Fatalf("comment re-routed: %+v", again)
	}
	if n := graphQLCalls.Load(); n != 1 {
		t.Fatalf("GraphQL calls = %d after a second tick inside the interval, want 1", n)
	}
	if got := collectPRFollowUpComments(context.Background(), nil, dir, actionable.PRs.Items, time.Now(), discardLogger()); got != nil {
		t.Fatalf("no client must collect nothing, got %v", got)
	}
}

func TestCollectPRFollowUpComments_FetchFailureSkipsPR(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, discardLogger())
	recordPRFollowUpPointer(&config.Config{}, nil, openedDetail("hivecommons/hive"), time.Now(), discardLogger())
	prs := []github.PullRequest{{Repo: "hivecommons/hive", Number: 9}, {Repo: "hivecommons/hive", Number: 9, Draft: true}}
	if got := collectPRFollowUpComments(context.Background(), client, dir, prs, time.Now(), discardLogger()); len(got) != 0 {
		t.Fatalf("failed fetch produced comments: %v", got)
	}
}

func TestHeldPRFollowUpCandidates_RecordsAttributionFallback(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	reviewAt := time.Date(2026, 9, 30, 14, 15, 0, 0, time.UTC)
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons"}}
	held := []github.PullRequest{
		{
			Repo: "hive", Number: 9, Title: "fix auth", URL: "https://example.invalid/pr/9", HiveAttributed: true,
			Protection: &github.ProtectionFacts{
				ReviewDecision:               github.ReviewDecisionChangesRequested,
				ChangesRequestedBy:           []string{"alice"},
				LatestHumanReviewState:       github.ReviewDecisionChangesRequested,
				LatestHumanReviewBy:          "alice",
				LatestHumanReviewSubmittedAt: reviewAt,
			},
		},
		{
			Repo: "hive", Number: 10, Title: "human pr",
			Protection: &github.ProtectionFacts{LatestHumanReviewState: github.ReviewDecisionChangesRequested},
		},
	}
	candidates := heldPRFollowUpCandidates(context.Background(), cfg, nil, dir, held, map[string]string{"hivecommons/hive#9": "sec-check"}, reviewAt, discardLogger())
	if len(candidates) != 1 || candidates[0].Repo != "hivecommons/hive" || candidates[0].Number != 9 {
		t.Fatalf("candidates = %+v, want only attributed held hive PR", candidates)
	}
	out := prfollowup.Route(context.Background(), candidates, nil, prfollowup.Options{Dir: dir}, reviewAt)
	if len(out) != 1 || out[0].Agent != "sec-check" || out[0].Route != prfollowup.RouteFallback || len(out[0].Events) != 1 || out[0].Events[0].Kind != prfollowup.EventChangesRequested {
		t.Fatalf("route outcomes = %+v, want changes_requested fallback to sec-check", out)
	}
}

// The sweep deletes the pointer of a PR that merged after leaving the open
// list, keeps a hold-gated PR's pointer without a lookup, and runs at most
// once per interval.
func TestSweepPRFollowUps_PrunesMergedKeepsHeld(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	var gets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/repos/hivecommons/hive/pulls/9" {
			gets.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"number":9,"state":"closed","merged_at":"2026-09-29T12:00:00Z","closed_at":"2026-09-29T12:00:00Z"}`))
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	client := github.NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, discardLogger())
	cfg := &config.Config{Project: config.ProjectConfig{Org: "hivecommons"}}
	now := time.Now()
	recordPRFollowUpPointer(cfg, nil, openedDetail("hivecommons/hive"), now, discardLogger())
	held := openedDetail("hive")
	held.Number = 11
	recordPRFollowUpPointer(cfg, nil, held, now, discardLogger())

	res := sweepPRFollowUps(context.Background(), cfg, client, dir, nil,
		[]github.HoldItem{{Type: "pr", Repo: "hive", Number: 11}, {Type: "issue", Repo: "hive", Number: 12}}, nil, now, discardLogger())
	if res == nil || res.Merged != 1 || res.Kept != 1 {
		t.Fatalf("sweep = %+v, want 1 merged, 1 kept", res)
	}
	if _, ok := prfollowup.PointerCreatedAt(context.Background(), dir, "hivecommons/hive", 9); ok {
		t.Fatal("merged PR's pointer survived the sweep")
	}
	if _, ok := prfollowup.PointerCreatedAt(context.Background(), dir, "hivecommons/hive", 11); !ok {
		t.Fatal("hold-gated (open) PR's pointer was pruned")
	}
	if gets.Load() != 1 {
		t.Fatalf("PR state GETs = %d, want 1 (the held PR is known open)", gets.Load())
	}
	if again := sweepPRFollowUps(context.Background(), cfg, client, dir, nil, nil, nil, now.Add(time.Minute), discardLogger()); again != nil {
		t.Fatalf("second sweep inside the interval ran: %+v", again)
	}
	s, err := prfollowup.ReadStats(dir)
	if err != nil || s.Pruned[prfollowup.PruneMerged] != 1 {
		t.Fatalf("counters = %+v, %v; want one merged prune", s, err)
	}
}

func TestPRLifecycleState(t *testing.T) {
	cases := map[string]github.PRState{
		prfollowup.PRStateMerged: {State: "closed", MergedAt: time.Now()},
		prfollowup.PRStateClosed: {State: "closed"},
		prfollowup.PRStateOpen:   {State: "open"},
	}
	for want, st := range cases {
		if got := prLifecycleState(st); got != want {
			t.Errorf("prLifecycleState(%+v) = %q, want %q", st, got, want)
		}
	}
}

func TestPRFollowUpResumer_AdaptsManager(t *testing.T) {
	mgr := agent.NewManager(map[string]config.AgentConfig{}, discardLogger(), agent.ProjectContext{})
	r := prFollowUpResumer{mgr: mgr}
	if _, ok := r.SessionID("ghost"); ok {
		t.Fatal("unknown agent must have no session")
	}
	err := r.SendResumeKick("ghost", "msg", "s1")
	if !errors.Is(err, agent.ErrResumeSessionGone) || errors.Is(err, prfollowup.ErrBusy) {
		t.Fatalf("err = %v, want session-gone (a fallback), not busy", err)
	}
}

func TestPRFollowUpMetricsCounters_ReadsStatsFile(t *testing.T) {
	dir := prFollowUpTestDir(t)
	if _, ok := prFollowUpMetricsCounters(); ok {
		t.Fatal("counters reported with no stats file")
	}
	data := `{"resumed":3,"fallback":{"session gone":2},"deferred":1,"handoffs_queued":4,"handoffs_delivered":2,"pruned":{"merged":5},"updated_at":"2026-09-29T00:00:00Z"}`
	if err := os.WriteFile(dir+"/"+prfollowup.StatsFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	c, ok := prFollowUpMetricsCounters()
	if !ok {
		t.Fatal("counters not reported with a stats file")
	}
	if c.Resumed != 3 || c.Fallback["session gone"] != 2 || c.Deferred != 1 ||
		c.HandoffsQueued != 4 || c.HandoffsDelivered != 2 || c.Pruned["merged"] != 5 {
		t.Errorf("counters = %+v", c)
	}
}

// fakePRFollowUpResumeSessions is a session source that also exposes a
// backend-native resume handle, like *agent.Manager does (#9606).
type fakePRFollowUpResumeSessions struct {
	fakePRFollowUpSessions
	handle agent.ResumeHandle
	ok     bool
}

func (f fakePRFollowUpResumeSessions) ResumeHandle(string) (agent.ResumeHandle, bool) {
	return f.handle, f.ok
}

func TestPRFollowUpWiring_CapturesBackendResumeHandle(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	redirectReviewThreadsPath(t)
	cfg := &config.Config{}
	transcript := filepath.Join(t.TempDir(), "events.jsonl")
	if err := os.WriteFile(transcript, []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	sessions := fakePRFollowUpResumeSessions{
		fakePRFollowUpSessions: fakePRFollowUpSessions{"scanner": "s1"},
		handle: agent.ResumeHandle{
			Backend: "copilot", SessionID: "sess-9606", Transcript: transcript,
			Command: "copilot --resume sess-9606", CapturedAt: time.Now(),
		},
		ok: true,
	}

	recordPRFollowUpPointer(cfg, sessions, openedDetail("hivecommons/hive"), time.Now(), discardLogger())
	// No manager on the tick: the PR falls back, and the fresh kick offers
	// the captured handle beside the handoff note.
	if out := routePRFollowUps(context.Background(), cfg, nil, redFollowUpActionable(), nil, nil, discardLogger()); len(out) != 1 {
		t.Fatalf("outcomes = %+v, want one fallback", out)
	}
	section := prfollowup.HandoffSectionWithResume(context.Background(), dir, time.Hour, time.Now(), "scanner")
	for _, want := range []string{"copilot --resume sess-9606", transcript, "Why: the scheduler dropped kicks on reload"} {
		if !strings.Contains(section, want) {
			t.Errorf("handoff section missing %q:\n%s", want, section)
		}
	}
}

// A session source without a resume handle (or one whose backend keeps none)
// records the pointer exactly as before.
func TestPRFollowUpWiring_NoResumeHandleIsInert(t *testing.T) {
	t.Setenv(config.PRFollowUpResumeEnvVar, "true")
	dir := prFollowUpTestDir(t)
	redirectReviewThreadsPath(t)
	cfg := &config.Config{}

	none := fakePRFollowUpResumeSessions{fakePRFollowUpSessions: fakePRFollowUpSessions{"scanner": "s1"}}
	recordPRFollowUpPointer(cfg, none, openedDetail("hivecommons/hive"), time.Now(), discardLogger())
	if h := capturePRFollowUpResume(fakePRFollowUpSessions{"scanner": "s1"}, "scanner"); h.Valid() {
		t.Errorf("a plain session source produced a handle: %+v", h)
	}
	if out := routePRFollowUps(context.Background(), cfg, nil, redFollowUpActionable(), nil, nil, discardLogger()); len(out) != 1 {
		t.Fatalf("outcomes = %+v, want one fallback", out)
	}
	if section := prfollowup.HandoffSection(context.Background(), dir, "scanner"); strings.Contains(section, "--resume") {
		t.Errorf("handle offered with none captured:\n%s", section)
	}
}
