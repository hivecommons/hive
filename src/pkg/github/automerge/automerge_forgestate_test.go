package automerge

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/forgestate"
)

// fakePRSource is an OpenPRSource over a fixed snapshot per repo. ReadThrough
// answers from the snapshot unless readThrough is set.
type fakePRSource struct {
	mu          sync.Mutex
	snaps       map[string]forgestate.Snapshot
	readThrough func(repo string, n int) (forgestate.Item, bool, error)
	snapshots   int
	reads       []int
	ready       chan struct{}
}

func newFakePRSource(repo string, prs ...forgestate.Item) *fakePRSource {
	snap := forgestate.Snapshot{Repo: repo, PRs: map[int]forgestate.Item{}, Issues: map[int]forgestate.Item{}, Ready: true}
	for _, pr := range prs {
		pr.Kind, pr.State = forgestate.KindPR, "open"
		snap.PRs[pr.Number] = pr
	}
	return &fakePRSource{snaps: map[string]forgestate.Snapshot{strings.ToLower(repo): snap}}
}

func (f *fakePRSource) Snapshot(repo string) (forgestate.Snapshot, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots++
	snap, ok := f.snaps[strings.ToLower(repo)]
	return snap, ok
}

func (f *fakePRSource) ReadThrough(_ context.Context, repo string, _ forgestate.Kind, n int, bound time.Duration) (forgestate.Item, bool, error) {
	if bound != automergeMergeFreshnessBound {
		return forgestate.Item{}, false, errors.New("read-through bound is not the merge freshness bound")
	}
	f.mu.Lock()
	f.reads = append(f.reads, n)
	rt := f.readThrough
	snap, ok := f.snaps[strings.ToLower(repo)]
	f.mu.Unlock()
	if rt != nil {
		return rt(repo, n)
	}
	if !ok {
		return forgestate.Item{}, false, forgestate.ErrUnknownRepo
	}
	if snap.Stale {
		return forgestate.Item{}, false, forgestate.ErrStale
	}
	it, ok := snap.PRs[n]
	return it, ok, nil
}

func (f *fakePRSource) WaitReady(ctx context.Context) error {
	if f.ready == nil {
		return nil
	}
	select {
	case <-f.ready:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (f *fakePRSource) snapshotCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots
}

func (f *fakePRSource) setStale(repo string, stale bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap := f.snaps[strings.ToLower(repo)]
	snap.Stale = stale
	f.snaps[strings.ToLower(repo)] = snap
}

// requestCounter wraps the self-authored fake API and counts requests by
// method+path so a test can prove which GitHub calls a pass made.
type requestCounter struct {
	mu     sync.Mutex
	counts map[string]int
}

func (rc *requestCounter) wrap(t *testing.T, inner *httptest.Server) *httptest.Server {
	t.Helper()
	rc.counts = map[string]int{}
	handler := inner.Config.Handler
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rc.mu.Lock()
		rc.counts[r.Method+" "+r.URL.Path]++
		rc.mu.Unlock()
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (rc *requestCounter) get(key string) int {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	return rc.counts[key]
}

const widgetListPath = "GET /repos/acme/widget/pulls"

func greenCachedPR(n int) forgestate.Item {
	return forgestate.Item{Number: n, Author: testHiveAppBotLogin, HeadSHA: "sha" + strconv.Itoa(n), FetchedAt: time.Now()}
}

func greenAPIPRs(numbers ...int) []selfAuthoredPR {
	var out []selfAuthoredPR
	for _, n := range numbers {
		out = append(out, selfAuthoredPR{
			number: n, author: testHiveAppBotLogin, mergeableState: "clean",
			statusState: "success", checkStatus: "completed", checkConclusion: "success",
		})
	}
	return out
}

func newCachedSweepEngine(t *testing.T, src OpenPRSource, prs []selfAuthoredPR, merged *[]int) (*Engine, *requestCounter) {
	t.Helper()
	inner := newSelfAuthoredAutoMergeAPI(t, prs, merged)
	t.Cleanup(inner.Close)
	var rc requestCounter
	api := rc.wrap(t, inner)
	c := newAutoMergeSweepClient(api.URL)
	c.openPRs = src
	c.sweepTrigger = NewSweepTrigger(src)
	return c, &rc
}

// With a forgestate source wired, a pass reads the cached open-PR set and
// never lists /pulls; every merge first reads the PR through the cache.
func TestSweepSelfAuthoredReadsCachedPRSetWithoutListing(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget", greenCachedPR(11), greenCachedPR(12))
	c, rc := newCachedSweepEngine(t, src, greenAPIPRs(11, 12), &merged)

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if got := rc.get(widgetListPath); got != 0 {
		t.Fatalf("list calls = %d, want none: the lane reads the cached set", got)
	}
	if result.Seen != 2 || len(merged) != 2 {
		t.Fatalf("seen=%d merged=%v, want both cached PRs evaluated and merged", result.Seen, merged)
	}
	if len(src.reads) != 2 {
		t.Fatalf("read-through calls = %v, want one per merge", src.reads)
	}
}

// An event-driven pass with targets evaluates only the PR the webhook named.
func TestSweepTargetsEvaluateOnlyTheNamedPR(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget", greenCachedPR(11), greenCachedPR(12))
	c, rc := newCachedSweepEngine(t, src, greenAPIPRs(11, 12), &merged)

	targets := SweepTargets{}
	targets.add("ACME/Widget", 12)
	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{Targets: targets})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 1 || len(merged) != 1 || merged[0] != 12 {
		t.Fatalf("seen=%d merged=%v, want only PR 12", result.Seen, merged)
	}
	if rc.get("GET /repos/acme/widget/pulls/11") != 0 {
		t.Fatal("PR 11 was fetched although the pass targeted PR 12 only")
	}
	if rc.get(widgetListPath) != 0 {
		t.Fatal("targeted pass listed /pulls")
	}

	// Targets naming another repo skip this one entirely.
	other := SweepTargets{}
	other.add("acme/other", 3)
	result, err = c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{Targets: other})
	if err != nil || result.Seen != 0 {
		t.Fatalf("pass targeting another repo: seen=%d err=%v, want nothing visited", result.Seen, err)
	}
}

// A stale snapshot never drives a merge: the pass skips the repo, and the
// merge gate refuses a PR whose read-through reports ErrStale even when the
// lane's own live evaluation found it green.
func TestForgeStateStaleSnapshotBlocksMerge(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget", greenCachedPR(11))
	c, rc := newCachedSweepEngine(t, src, greenAPIPRs(11), &merged)
	src.setStale("acme/widget", true)

	result, err := c.SweepSelfAuthoredAutoMerges(context.Background(), AutoMergeSweepOptions{})
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if result.Seen != 0 || len(merged) != 0 || rc.get("GET /repos/acme/widget/pulls/11") != 0 {
		t.Fatalf("stale snapshot: seen=%d merged=%v fetches=%d, want the repo skipped", result.Seen, merged, rc.get("GET /repos/acme/widget/pulls/11"))
	}

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "acme/widget", "acme", "widget", 11, false)
	if err != nil || reason != autoMergeReasonForgeStateStale {
		t.Fatalf("merge gate on stale snapshot: reason=%q err=%v, want %q", reason, err, autoMergeReasonForgeStateStale)
	}
	if len(merged) != 0 || rc.get("PUT /repos/acme/widget/pulls/11/merge") != 0 {
		t.Fatalf("merge was attempted from a stale snapshot: %v", merged)
	}
}

func TestForgeStateMergeGateDecisions(t *testing.T) {
	cases := []struct {
		name string
		rt   func(string, int) (forgestate.Item, bool, error)
		want string
		err  bool
	}{
		{"fresh at evaluated head allows", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{HeadSHA: "sha11"}, true, nil
		}, "", false},
		{"head moved refuses", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{HeadSHA: "other"}, true, nil
		}, autoMergeReasonHeadChangedSinceEval, false},
		{"closed or merged refuses", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{}, false, nil
		}, autoMergeReasonGone, false},
		{"not ready refuses", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{}, false, forgestate.ErrNotReady
		}, autoMergeReasonForgeStateNotReady, false},
		{"unknown repo refuses", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{}, false, forgestate.ErrUnknownRepo
		}, autoMergeReasonForgeStateUnknownRepo, false},
		{"refresh failure is an error", func(string, int) (forgestate.Item, bool, error) {
			return forgestate.Item{}, false, errors.New("boom")
		}, autoMergeReasonForgeStateRefresh, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := newFakePRSource("acme/widget")
			src.readThrough = tc.rt
			c := &Engine{openPRs: src}
			reason, err := c.forgestateMergeGate(context.Background(), "acme", "widget", 11, "sha11")
			if reason != tc.want || (err != nil) != tc.err {
				t.Fatalf("reason=%q err=%v, want %q err=%v", reason, err, tc.want, tc.err)
			}
		})
	}
	var none *Engine
	if reason, err := none.forgestateMergeGate(context.Background(), "acme", "widget", 11, "sha11"); reason != "" || err != nil {
		t.Fatalf("no source must allow: %q %v", reason, err)
	}
}

func TestSweepTriggerCoalescesAndResolvesHeads(t *testing.T) {
	src := newFakePRSource("acme/widget", greenCachedPR(11), greenCachedPR(12))
	tr := NewSweepTrigger(src)
	if got := tr.NotifyPullRequests("acme/widget", 11, 11, 0); got != 1 {
		t.Fatalf("notify = %d, want 1 (dedupe and ignore 0)", got)
	}
	if got := tr.NotifyHead("ACME/WIDGET", "sha12"); got != 1 {
		t.Fatalf("notify head = %d, want PR 12 resolved from the snapshot", got)
	}
	if got := tr.NotifyHead("acme/widget", "nope"); got != 0 {
		t.Fatalf("unknown head scheduled %d", got)
	}
	select {
	case <-tr.C():
	default:
		t.Fatal("trigger did not signal")
	}
	select {
	case <-tr.C():
		t.Fatal("trigger signalled twice for one batch")
	default:
	}
	if tr.Pending() != 2 {
		t.Fatalf("pending = %d, want 2", tr.Pending())
	}
	targets := tr.take()
	if nums := targets.Numbers("acme/widget"); len(nums) != 2 || nums[0] != 11 || nums[1] != 12 {
		t.Fatalf("targets = %v, want [11 12]", nums)
	}
	if !targets.selects("acme/widget", 12) || targets.selects("acme/widget", 13) || targets.selectsRepo("acme/other") {
		t.Fatal("targets select the wrong PRs")
	}
	if tr.Pending() != 0 {
		t.Fatal("take did not drain")
	}
	tr.NotifyPullRequests("acme/widget", 11)
	tr.RequestFull()
	if got := tr.take(); got != nil {
		t.Fatalf("full request must yield nil targets, got %v", got)
	}
	var nilTargets SweepTargets
	if !nilTargets.selectsRepo("any") || !nilTargets.selects("any", 1) {
		t.Fatal("nil targets must select everything")
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	timeout := time.After(5 * time.Second)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-timeout:
			t.Fatalf("timed out waiting for %s", what)
		case <-tick.C:
		}
	}
}

// The event-driven loop runs one pass for a trigger, touching only the PR
// named, and otherwise relies on the floor pass.
func TestEventDrivenSweepRunsTargetedPassOnTrigger(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget", greenCachedPR(11), greenCachedPR(12))
	src.ready = make(chan struct{})
	c, rc := newCachedSweepEngine(t, src, greenAPIPRs(11, 12), &merged)
	c.sweepFloor = time.Hour

	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartSelfAuthoredAutoMergeSweep(ctx, 3, true, nil)
	<-time.After(20 * time.Millisecond)
	if src.snapshotCalls() != 0 {
		t.Fatal("loop passed before the source was ready")
	}
	close(src.ready)
	waitFor(t, "startup pass", func() bool { return src.snapshotCalls() >= 1 && rc.get("PUT /repos/acme/widget/pulls/12/merge") == 1 })
	fetched11 := rc.get("GET /repos/acme/widget/pulls/11")

	c.Trigger().NotifyPullRequests("acme/widget", 12)
	waitFor(t, "event pass", func() bool { return rc.get("PUT /repos/acme/widget/pulls/12/merge") == 2 })
	if got := rc.get("GET /repos/acme/widget/pulls/11"); got != fetched11 {
		t.Fatalf("event pass for PR 12 fetched PR 11 (%d -> %d)", fetched11, got)
	}
	if rc.get(widgetListPath) != 0 {
		t.Fatal("event-driven loop listed /pulls")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop")
	}
}

// With no webhook at all the floor pass still runs on its interval.
func TestEventDrivenSweepFloorFiresWithoutEvents(t *testing.T) {
	var merged []int
	src := newFakePRSource("acme/widget")
	c, rc := newCachedSweepEngine(t, src, nil, &merged)
	c.sweepFloor = 15 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartSelfAuthoredAutoMergeSweep(ctx, 3, true, nil)
	waitFor(t, "startup plus two floor passes", func() bool { return src.snapshotCalls() >= 3 })
	cancel()
	<-done
	if rc.get(widgetListPath) != 0 {
		t.Fatal("floor pass listed /pulls")
	}
	if c.Trigger().Pending() != 0 {
		t.Fatal("floor passes must not leave pending targets")
	}
}

func TestPullRequestFromItem(t *testing.T) {
	now := time.Now()
	it := forgestate.Item{
		Number: 7, State: "open", Draft: true, Author: "octo", Title: "t", Body: "b", HTMLURL: "u",
		Labels: []string{"hold"}, HeadSHA: "abc", HeadRef: "feat", HeadRepo: "fork/widget", BaseRef: "main",
		CreatedAt: now, UpdatedAt: now, MergeableState: "clean",
	}
	pr := pullRequestFromItem(it)
	if pr.GetNumber() != 7 || !pr.GetDraft() || pr.GetUser().GetLogin() != "octo" || pr.GetHead().GetSHA() != "abc" ||
		pr.GetHead().GetRepo().GetFullName() != "fork/widget" || pr.GetBase().GetRef() != "main" ||
		len(pr.Labels) != 1 || pr.Labels[0].GetName() != "hold" || pr.GetTitle() != "t" || pr.GetBody() != "b" ||
		pr.GetHTMLURL() != "u" || !pr.GetCreatedAt().Time.Equal(now) || pr.GetMergeableState() != "clean" {
		t.Fatalf("pullRequestFromItem lost fields: %+v", pr)
	}
	if labelNames(pr.Labels)[0] != "hold" {
		t.Fatal("labels not readable through labelNames")
	}
}
