package automerge

// The lanes' once-per-head memory across sweeps. Production runs the queued,
// trusted-author and contributor lanes every minute through the package
// functions (SweepContributorLaneAutoMerges, SweepTrustedAuthorAutoMerges),
// each of which builds a new Engine with New(transport, opts); these tests
// build one engine per sweep on one shared client the same way.
// projectbluefin, 2026-11-08 01:45-02:11Z: fsdk-containers#508 (a fork head
// behind a base that changed workflow files) logged "update-branch refused"
// and sent the same refused PUT update-branch on every sweep.

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

// sweepTickEngines returns a constructor for one engine per sweep tick, all
// over one client and one set of options, as cmd/hive's
// runAutoMergeSweepIfDue builds them.
func sweepTickEngines(apiURL string, opts Options) func() *Engine {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	if opts.BasePreparation == nil {
		opts.BasePreparation = NewBasePreparationStore("")
	}
	if opts.HeadFirstSeen == nil {
		opts.HeadFirstSeen = NewHeadFirstSeenTracker()
	}
	if opts.MergerAuthorizer == nil {
		opts.MergerAuthorizer = func(login string) bool {
			return testTrustedMergers[strings.ToLower(strings.TrimSpace(login))]
		}
	}
	return func() *Engine { return New(client, opts) }
}

// contributorLaneSweepClient is the client and options the contributor
// lane's package-level sweep runs with in these tests.
func contributorLaneSweepClient(apiURL string, policy ContributorLanePolicy, clock *contributorLaneClock) (*hgithub.Client, Options) {
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, apiURL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	return client, Options{
		ContributorLanePolicy:  func() ContributorLanePolicy { return policy },
		ContributorLaneVerdict: approvedVerdict,
		ContributorSoak:        soakedTracker(clock),
		BasePreparation:        NewBasePreparationStore(""),
		HeadFirstSeen:          NewHeadFirstSeenTracker(),
		StageStore:             stagelabels.NewStore("", nil),
		Now:                    clock.now,
		SentinelLabel:          func() string { return "hive-sentinel" },
	}
}

// A fork head GitHub refuses to update (the base changed workflow files)
// is sent once across sweeps, not once per sweep; the lane keeps reporting
// the refusal.
func TestContributorLaneRefusedUpdateBranchNotRetriedAcrossSweeps(t *testing.T) {
	fx := validContributorFixture()
	fx.headRepo = "someone/widget"
	fx.mergeableState = "behind"
	fx.updateStatus = http.StatusForbidden
	fx.updateMessage = workflowPermissionRefusal
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)}
	client, opts := contributorLaneSweepClient(api.URL, defaultContributorLanePolicy(), clock)

	for i := 1; i <= 3; i++ {
		result, err := SweepContributorLaneAutoMerges(context.Background(), client, opts, AutoMergeSweepOptions{})
		if err != nil {
			t.Fatalf("sweep %d err = %v", i, err)
		}
		if len(result.Merged) != 0 || result.Skipped != 1 {
			t.Fatalf("sweep %d result = %+v, want one skip", i, result)
		}
		clock.advance(80 * time.Second)
	}
	if fx.updates != 1 {
		t.Fatalf("update-branch calls = %d across 3 sweeps, want 1", fx.updates)
	}
}

// A merge GitHub refused for want of an approving review is not re-PUT by
// the next sweep's engine inside the recheck window.
func TestContributorLaneRefusedMergeNotRetriedAcrossSweeps(t *testing.T) {
	fx := validContributorFixture()
	fx.mergeStatus = http.StatusMethodNotAllowed
	fx.mergeMessage = "At least 1 approving review is required by reviewers with write access."
	api := newContributorLaneAPI(t, &fx)
	defer api.Close()
	clock := &contributorLaneClock{at: time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC)}
	client, opts := contributorLaneSweepClient(api.URL, defaultContributorLanePolicy(), clock)

	for i := 1; i <= 3; i++ {
		result, err := SweepContributorLaneAutoMerges(context.Background(), client, opts, AutoMergeSweepOptions{})
		if err != nil {
			t.Fatalf("sweep %d err = %v", i, err)
		}
		if len(result.Merged) != 0 {
			t.Fatalf("sweep %d merged %+v", i, result.Merged)
		}
		clock.advance(80 * time.Second)
	}
	if fx.merges != 1 {
		t.Fatalf("merge calls = %d across 3 sweeps, want 1", fx.merges)
	}
	clock.advance(mergeRefusalRecheck)
	if _, err := SweepContributorLaneAutoMerges(context.Background(), client, opts, AutoMergeSweepOptions{}); err != nil {
		t.Fatalf("sweep after the recheck window err = %v", err)
	}
	if fx.merges != 2 {
		t.Fatalf("merge calls after the recheck window = %d, want 2", fx.merges)
	}
}

// The trusted-author lane's merge-forward is tried once per head across
// sweeps: a conflict is one POST /merges and one note, and a workflow
// refusal keeps reporting workflow-permission without another POST.
func TestMergeForwardTriedOncePerHeadAcrossSweeps(t *testing.T) {
	for _, tc := range []struct {
		name         string
		status       int
		message      string
		wantReason   string
		wantComments int
	}{
		{name: "conflict", status: http.StatusConflict, wantReason: "conflicting", wantComments: 1},
		{name: "workflow refusal", status: http.StatusForbidden, message: workflowPermissionRefusal, wantReason: autoMergeReasonWorkflowPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &mergeForwardAPI{mergeStatus: tc.status, mergeMessage: tc.message}
			srv := newMergeForwardServer(t, api)
			defer srv.Close()
			tick := sweepTickEngines(srv.URL, Options{MergeForwardTrustedHuman: func(string) bool { return true }})
			pr := mergeForwardPR(false, "author/human")
			for i := 1; i <= 3; i++ {
				if _, reason, err := tick().mergeForwardOrConflict(context.Background(), "acme/widget", "acme", "widget", pr, 7); reason != tc.wantReason || err != nil {
					t.Fatalf("sweep %d: reason, err = %q, %v; want %q", i, reason, err, tc.wantReason)
				}
			}
			api.mu.Lock()
			defer api.mu.Unlock()
			if api.merges != 1 || len(api.comments) != tc.wantComments {
				t.Fatalf("merges, comments = %d, %d across 3 sweeps; want 1, %d", api.merges, len(api.comments), tc.wantComments)
			}
		})
	}
}

// The current-base gate's refused update-branch (the path
// fsdk-containers#508 looped on) is sent once per head and base across
// sweeps, and once more when the base moves.
func TestCurrentBaseGateRefusedUpdateNotRetriedAcrossSweeps(t *testing.T) {
	head := "sha-head"
	var baseMu sync.Mutex
	baseTip := "sha-base-1"
	compare := func(w http.ResponseWriter, r *http.Request) {
		baseMu.Lock()
		defer baseMu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main...sha-head":
			_, _ = w.Write([]byte(`{"behind_by":2,"ahead_by":1,"base_commit":{"sha":"` + baseTip + `"}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/branches/main":
			_, _ = w.Write([]byte(`{"name":"main","commit":{"sha":"` + baseTip + `"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}
	srv, updates := updateBranchRefusalServer(t, workflowPermissionRefusal, &head, compare)
	defer srv.Close()
	tick := sweepTickEngines(srv.URL, Options{RequireCurrentBase: func(string) bool { return true }})
	sweep := func(step string, wantUpdates int) {
		t.Helper()
		_, reason, stop, err := tick().currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, true, "behind", currentBaseTestPR(true, "feature", "alice", "behind"))
		if !stop || reason != autoMergeReasonForkWorkflowUpdate || err != nil {
			t.Fatalf("%s: stop, reason, err = %v, %q, %v; want true, %q", step, stop, reason, err, autoMergeReasonForkWorkflowUpdate)
		}
		if got := updates(); got != wantUpdates {
			t.Fatalf("%s: update-branch calls = %d, want %d", step, got, wantUpdates)
		}
	}
	sweep("first sweep", 1)
	sweep("second sweep", 1)
	sweep("third sweep", 1)
	baseMu.Lock()
	baseTip = "sha-base-2"
	baseMu.Unlock()
	sweep("the base moved", 2)
	sweep("same base again", 2)
}

// A hold logged "once per head" is logged once across sweeps: on
// projectbluefin "merge train: PR left out of every train" repeated for
// ~15 PRs on every sweep.
func TestMergeTrainSkipLoggedOnceAcrossSweeps(t *testing.T) {
	api := &mergeTrainAPI{files: map[int][]string{}, state: map[int]string{}}
	srv := newMergeTrainServer(t, api)
	defer srv.Close()
	var logs bytes.Buffer
	tick := sweepTickEngines(srv.URL, Options{
		Logger:     slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
		MergeTrain: func() MergeTrainPolicy { return DefaultMergeTrainPolicy },
	})
	pr := mtPR(15405, time.Date(2026, 11, 8, 2, 0, 0, 0, time.UTC), "")
	pr.Labels = []*gh.Label{{Name: gh.Ptr(stagelabels.Fixing.Label())}}
	for i := 0; i < 3; i++ {
		tick().planMergeTrains(context.Background(), stageLaneContributor, "acme/widget", "acme", "widget", []*gh.PullRequest{pr}, func(*gh.PullRequest) string { return "" })
	}
	if got := strings.Count(logs.String(), "left out of every train"); got != 1 {
		t.Fatalf("skip logged %d times across 3 sweeps, want 1\n%s", got, logs.String())
	}
	pr.Head.SHA = gh.Ptr("sha15405-2")
	tick().planMergeTrains(context.Background(), stageLaneContributor, "acme/widget", "acme", "widget", []*gh.PullRequest{pr}, func(*gh.PullRequest) string { return "" })
	if got := strings.Count(logs.String(), "left out of every train"); got != 2 {
		t.Fatalf("a new head logged %d lines in total, want 2", got)
	}
}

// Engines over one client share the memory; a read-only integration probe
// and an engine over a transport that keeps no state each get their own.
func TestSweepMemorySharedPerClient(t *testing.T) {
	tick := sweepTickEngines("http://127.0.0.1:0", Options{})
	first, second := tick(), tick()
	if first.memory() != second.memory() {
		t.Fatal("two sweep engines over one client keep separate memories")
	}
	probe := New(first.transport, first.integrationReadOnlyOptions())
	if probe.memory() == first.memory() {
		t.Fatal("the read-only integration probe shares the sweeps' memory")
	}
	if !first.markMergeForwardOnce("k") || second.markMergeForwardOnce("k") || !probe.markMergeForwardOnce("k") {
		t.Fatal("once marks do not follow the shared memory")
	}
	other := sweepTickEngines("http://127.0.0.1:0", Options{})()
	if other.memory() == first.memory() {
		t.Fatal("engines over different clients share a memory")
	}
	if (&Engine{}).memory() == nil {
		t.Fatal("an engine not made by New has no memory")
	}
}
