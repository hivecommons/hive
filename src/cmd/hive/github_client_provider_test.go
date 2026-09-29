package main

// Tests for #9621: long-lived GitHub consumers follow client rebuilds, and the
// request relays start when a usable App first arrives.
//
//   - TestAppDeliveredAfterAppLessBootStartsRelaysAndFulfilsQueuedPR is the
//     hosted-spoke scenario from the issue: boot with no App, queue a PR
//     request, deliver the App the way the heartbeat does, and the relays
//     start and consume the request without a restart.
//   - TestAdoptGitHubClientRepointsEveryConsumer: after a rebuild the
//     provider, the sandbox, the relays and the scheduler's triage commenter
//     all use the new client.
//   - TestRequestRelayHandOverNeverDoubleProcesses: hand-overs never run two
//     generations at once, and every queued request is consumed exactly once.
//   - TestLongLivedGitHubConsumersReadThroughProvider pins, at the source
//     level, that no long-lived consumer is handed a captured b.ghClient.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/agent"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/pushbroker"
)

const (
	// fakeRelayPollInterval is the fake relay's scan tick.
	fakeRelayPollInterval = 2 * time.Millisecond
	// fakeRelayWorkTime is how long "opening" one request takes, so a
	// hand-over can land while a request is in flight.
	fakeRelayWorkTime = time.Millisecond
	// fakeRelayWaitTimeout bounds every wait on the fake relay.
	fakeRelayWaitTimeout = 10 * time.Second
	// handOverRequests / handOverClients size the double-processing test.
	handOverRequests = 24
	handOverClients  = 6
)

// fakeRequestQueue is a request relay over a real directory with the real
// watchers' consume semantics: scan on a tick, stop between files once ctx is
// cancelled but finish the one in flight, and remove a request once it has
// been fulfilled. It records which client fulfilled each request and the
// highest number of relay loops that were ever scanning at once.
type fakeRequestQueue struct {
	dir string

	mu        sync.Mutex
	processed map[string][]*github.Client
	fulfilled chan string

	active    atomic.Int32
	maxActive atomic.Int32
}

func newFakeRequestQueue(t *testing.T) *fakeRequestQueue {
	t.Helper()
	return &fakeRequestQueue{
		dir:       t.TempDir(),
		processed: map[string][]*github.Client{},
		fulfilled: make(chan string, handOverRequests*handOverClients),
	}
}

func (q *fakeRequestQueue) enqueue(t *testing.T, name string, req github.PRRequest) {
	t.Helper()
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := os.WriteFile(filepath.Join(q.dir, name), data, 0o600); err != nil {
		t.Fatalf("queue request: %v", err)
	}
}

func (q *fakeRequestQueue) run(ctx context.Context, c *github.Client) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(fakeRelayPollInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
			}
			if ctx.Err() != nil {
				return
			}
			n := q.active.Add(1)
			for {
				m := q.maxActive.Load()
				if n <= m || q.maxActive.CompareAndSwap(m, n) {
					break
				}
			}
			q.scan(ctx, c)
			q.active.Add(-1)
		}
	}()
	return done
}

func (q *fakeRequestQueue) scan(ctx context.Context, c *github.Client) {
	entries, err := os.ReadDir(q.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if ctx.Err() != nil {
			return
		}
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".json") {
			continue
		}
		path := filepath.Join(q.dir, name)
		if _, err := os.ReadFile(path); err != nil {
			continue
		}
		time.Sleep(fakeRelayWorkTime)
		q.mu.Lock()
		q.processed[name] = append(q.processed[name], c)
		q.mu.Unlock()
		_ = os.Remove(path)
		q.fulfilled <- name
	}
}

// waitFulfilled waits until n more requests have been fulfilled.
func (q *fakeRequestQueue) waitFulfilled(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(fakeRelayWaitTimeout)
	for i := 0; i < n; i++ {
		select {
		case <-q.fulfilled:
		case <-deadline:
			t.Fatalf("only %d of %d queued requests were fulfilled", i, n)
		}
	}
}

func (q *fakeRequestQueue) processedBy(name string) []*github.Client {
	q.mu.Lock()
	defer q.mu.Unlock()
	return append([]*github.Client(nil), q.processed[name]...)
}

// --- the issue's acceptance scenario -----------------------------------------

func TestAppDeliveredAfterAppLessBootStartsRelaysAndFulfilsQueuedPR(t *testing.T) {
	q := newFakeRequestQueue(t)
	f := newBootAgentsFake()
	var armedWith atomic.Pointer[requestRelays]
	f.deps.startRequestRelays = func(ctx context.Context, c *github.Client, r requestRelays) <-chan struct{} {
		armedWith.Store(&r)
		return q.run(ctx, c)
	}
	f.deps.startSelfAuthoredSweep = func(ctx context.Context, _ *github.Client, _ int, _ bool, _ *int, _ automerge.Options) <-chan struct{} {
		return ctx.Done()
	}

	// A hosted spoke boots with no App: no installation to author as.
	cfg := bootAgentsConfig()
	b := newBootAgentsBoot(t, f, cfg)
	b.bootAgentsWith(f.deps)

	// An agent queues a PR request while the spoke has no App.
	const reqName = "scanner-1.json"
	q.enqueue(t, reqName, github.PRRequest{Agent: "scanner", Repo: "acme/widgets", Head: "fix/x", Title: "fix x"})
	if c, _ := b.requestRelays.current(); c != nil {
		t.Fatal("relays started with no App; a request would open under no bot identity")
	}
	if _, err := os.Stat(filepath.Join(q.dir, reqName)); err != nil {
		t.Fatalf("queued request vanished before any relay ran: %v", err)
	}

	// The heartbeat delivers App credentials: config first, then a client
	// built through the shared constructor, then adoption. This is the
	// heartbeat rebuild site's exact sequence.
	cfg.GitHub.AppID = rebuildTestAppID
	cfg.GitHub.InstallationID = rebuildTestInstallID
	auth := testAppAuth(t, cfg)
	delivered := b.newConfiguredGitHubAppClient(auth)
	b.adoptGitHubClient(delivered, auth)

	q.waitFulfilled(t, 1)
	if got := q.processedBy(reqName); len(got) != 1 || got[0] != delivered {
		t.Fatalf("queued PR request fulfilled by %v, want exactly once by the delivered App client", got)
	}
	if _, err := os.Stat(filepath.Join(q.dir, reqName)); !os.IsNotExist(err) {
		t.Fatalf("fulfilled request still queued (stat err %v)", err)
	}
	if r := armedWith.Load(); r == nil || r.prOpen == nil || r.issueOpen == nil || r.review == nil || r.merge == nil || r.holdLabel == nil {
		t.Fatal("lazily started relays were armed without the manager's authorizers")
	}
	if err := armedWith.Load().prOpen("ghost", 0); err == nil {
		t.Fatal("lazily started PR relay accepted an unknown agent; it must carry the manager's own gate")
	}
	if b.currentGitHubClient() != delivered {
		t.Fatal("client provider still serves the boot-time (nil) client after heartbeat delivery")
	}
}

// --- every consumer follows a rebuild ----------------------------------------

func TestAdoptGitHubClientRepointsEveryConsumer(t *testing.T) {
	var bootHits, rebuiltHits atomic.Int32
	server := func(hits *atomic.Int32) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			hits.Add(1)
			_, _ = io.WriteString(w, "[]")
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	bootSrv, rebuiltSrv := server(&bootHits), server(&rebuiltHits)

	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	cfg.GitHub.AppID = rebuildTestAppID
	cfg.GitHub.InstallationID = rebuildTestInstallID
	b := newBootAgentsBoot(t, f, cfg)
	bootClient := github.NewClientForTest(bootSrv.URL, "acme", []string{"widgets"}, quietLogger())
	b.ghClient = bootClient
	b.publishGitHubClient(bootClient)
	b.bootAgentsWith(f.deps)
	if c, _, _ := f.waitRelaysStarted(t); c != bootClient {
		t.Fatal("relays did not start on the boot client")
	}

	triage := liveTriageCommenter{client: b.currentGitHubClient}
	if _, err := triage.IssueCommentsContain(context.Background(), "widgets", 1, "x"); err != nil {
		t.Fatalf("triage commenter on boot client: %v", err)
	}
	if bootHits.Load() == 0 {
		t.Fatal("positive control: the triage commenter did not reach the boot client")
	}

	rebuilt := github.NewClientForTest(rebuiltSrv.URL, "acme", []string{"widgets"}, quietLogger())
	rebuiltAuth := testAppAuth(t, cfg)
	b.adoptGitHubClient(rebuilt, rebuiltAuth)

	if b.ghClient != rebuilt || b.appAuth != rebuiltAuth {
		t.Fatal("adoptGitHubClient did not swap b.ghClient / b.appAuth")
	}
	if b.currentGitHubClient() != rebuilt {
		t.Fatal("provider still serves the old client; collectors and the scheduler would stay stale")
	}
	pr, minter := b.agentMgr.SandboxGitHubWiring()
	if pr != agent.PRCreator(rebuilt) {
		t.Fatalf("sandbox PR client = %v, want the rebuilt client", pr)
	}
	if m, ok := minter.(pushbroker.GitHubAppMinter); !ok || m.Auth != rebuiltAuth {
		t.Fatalf("sandbox push minter = %#v, want one minting with the rebuilt AppAuth", minter)
	}
	if relayClient, _, sweepClient := f.waitRelaysStarted(t); relayClient != rebuilt || sweepClient != rebuilt {
		t.Fatal("request relays / self-authored sweep were not handed over to the rebuilt client")
	}
	if c, gens := b.requestRelays.current(); c != rebuilt || gens != 2 {
		t.Fatalf("relay supervisor = (%p, %d generations), want (rebuilt, 2)", c, gens)
	}

	before := bootHits.Load()
	if _, err := triage.IssueCommentsContain(context.Background(), "widgets", 1, "x"); err != nil {
		t.Fatalf("triage commenter on rebuilt client: %v", err)
	}
	if rebuiltHits.Load() == 0 || bootHits.Load() != before {
		t.Fatalf("triage commenter hits: boot %d->%d, rebuilt %d; want it on the rebuilt client only",
			before, bootHits.Load(), rebuiltHits.Load())
	}

	// Adopting the same client again is not a hand-over.
	b.adoptGitHubClient(rebuilt, rebuiltAuth)
	if _, gens := b.requestRelays.current(); gens != 2 {
		t.Fatalf("re-adopting the running client restarted the relays (generations %d)", gens)
	}
}

func TestArmRequestRelaysRequiresUsableApp(t *testing.T) {
	cfg := bootAgentsConfig()
	b := &boot{cfg: cfg, logger: quietLogger()}
	client := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
	if b.armRequestRelays(client) {
		t.Fatal("armed with no relay supervisor")
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	started := make(chan *github.Client, 1)
	b.requestRelays = newRequestRelaySupervisor(ctx, func(ctx context.Context, c *github.Client) <-chan struct{} {
		started <- c
		return ctx.Done()
	}, quietLogger())

	if b.armRequestRelays(client) {
		t.Fatal("relays armed with no usable App (no installation to author as)")
	}
	cfg.GitHub.AppID = rebuildTestAppID
	if b.armRequestRelays(client) {
		t.Fatal("relays armed with an App ID but no installation")
	}
	cfg.GitHub.InstallationID = rebuildTestInstallID
	if b.armRequestRelays(nil) {
		t.Fatal("relays armed on a nil client")
	}
	if !b.armRequestRelays(client) {
		t.Fatal("relays not armed with a usable App")
	}
	select {
	case c := <-started:
		if c != client {
			t.Fatal("relays started on the wrong client")
		}
	case <-time.After(fakeRelayWaitTimeout):
		t.Fatal("relays never started")
	}
}

// --- hand-over safety --------------------------------------------------------

func TestRequestRelayHandOverNeverDoubleProcesses(t *testing.T) {
	q := newFakeRequestQueue(t)
	for i := 0; i < handOverRequests; i++ {
		q.enqueue(t, fmt.Sprintf("req-%02d.json", i), github.PRRequest{Agent: "scanner", Repo: "acme/widgets", Head: fmt.Sprintf("b%d", i), Title: "t"})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := newRequestRelaySupervisor(ctx, q.run, quietLogger())

	clients := make([]*github.Client, handOverClients)
	for i := range clients {
		clients[i] = github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
		if !s.switchTo(clients[i]) {
			t.Fatalf("switch %d not scheduled", i)
		}
		// Let the generation run briefly so hand-overs land mid-queue.
		time.Sleep(fakeRelayWorkTime * 2)
	}
	q.waitFulfilled(t, handOverRequests)

	cancel()
	select {
	case <-s.stopped():
	case <-time.After(fakeRelayWaitTimeout):
		t.Fatal("relays did not stop after the parent context was cancelled")
	}

	if got := q.maxActive.Load(); got != 1 {
		t.Fatalf("%d relay generations scanned the request dir at once; a hand-over must wait for the old one to stop", got)
	}
	for i := 0; i < handOverRequests; i++ {
		name := fmt.Sprintf("req-%02d.json", i)
		if got := q.processedBy(name); len(got) != 1 {
			t.Errorf("%s processed %d times across hand-overs, want exactly once", name, len(got))
		}
	}
	if _, gens := s.current(); gens != handOverClients {
		t.Fatalf("generations = %d, want %d", gens, handOverClients)
	}
	// The last client must be the one serving requests from now on.
	if c, _ := s.current(); c != clients[len(clients)-1] {
		t.Fatal("supervisor does not report the latest client")
	}
}

func TestRequestRelaySupervisorSwitchToEdges(t *testing.T) {
	var nilSup *requestRelaySupervisor
	client := github.NewClientForTest("http://127.0.0.1:1", "acme", []string{"widgets"}, quietLogger())
	if nilSup.switchTo(client) {
		t.Fatal("nil supervisor scheduled a start")
	}
	if c, gens := nilSup.current(); c != nil || gens != 0 {
		t.Fatal("nil supervisor reported a client")
	}
	if nilSup.stopped() != nil {
		t.Fatal("nil supervisor returned a stop channel")
	}

	ctx, cancel := context.WithCancel(context.Background())
	var starts atomic.Int32
	startedCh := make(chan struct{}, 1)
	s := newRequestRelaySupervisor(ctx, func(ctx context.Context, _ *github.Client) <-chan struct{} {
		starts.Add(1)
		startedCh <- struct{}{}
		return ctx.Done()
	}, nil)
	if s.stopped() != nil {
		t.Fatal("stop channel before any start")
	}
	if s.switchTo(nil) {
		t.Fatal("nil client scheduled a start")
	}
	if !s.switchTo(client) || s.switchTo(client) {
		t.Fatal("want exactly one start for the same client")
	}
	select {
	case <-startedCh:
	case <-time.After(fakeRelayWaitTimeout):
		t.Fatal("relays never started")
	}
	cancel()
	<-s.stopped()
	if s.switchTo(github.NewClientForTest("http://127.0.0.1:1", "acme", nil, quietLogger())) {
		t.Fatal("start scheduled after shutdown")
	}
	if starts.Load() != 1 {
		t.Fatalf("start func ran %d times, want 1", starts.Load())
	}

	// A start func that returns nil (nothing to wait for) still lets the
	// generation finish.
	ctx2, cancel2 := context.WithCancel(context.Background())
	s2 := newRequestRelaySupervisor(ctx2, func(context.Context, *github.Client) <-chan struct{} { return nil }, quietLogger())
	s2.switchTo(client)
	cancel2()
	select {
	case <-s2.stopped():
	case <-time.After(fakeRelayWaitTimeout):
		t.Fatal("generation with a nil done channel never finished")
	}
}

func TestJoinDoneWaitsForEveryChannel(t *testing.T) {
	a, c := make(chan struct{}), make(chan struct{})
	joined := joinDone(a, nil, c)
	close(a)
	select {
	case <-joined:
		t.Fatal("joined closed before every channel closed")
	case <-time.After(fakeRelayWorkTime):
	}
	close(c)
	select {
	case <-joined:
	case <-time.After(fakeRelayWaitTimeout):
		t.Fatal("joined never closed")
	}
}

func TestLiveTriageCommenterWithoutClient(t *testing.T) {
	for name, l := range map[string]liveTriageCommenter{
		"nil provider":   {},
		"nil client yet": {client: func() *github.Client { return nil }},
	} {
		if _, err := l.IssueCommentsContain(context.Background(), "widgets", 1, "x"); !errors.Is(err, github.ErrNoGitHubClient) {
			t.Errorf("%s: IssueCommentsContain err = %v, want ErrNoGitHubClient", name, err)
		}
		if err := l.CreateIssueComment(context.Background(), "widgets", 1, "x"); !errors.Is(err, github.ErrNoGitHubClient) {
			t.Errorf("%s: CreateIssueComment err = %v, want ErrNoGitHubClient", name, err)
		}
	}
}

func TestCurrentGitHubClientFallsBackUntilPublished(t *testing.T) {
	var nilBoot *boot
	if nilBoot.currentGitHubClient() != nil {
		t.Fatal("nil boot returned a client")
	}
	nilBoot.publishGitHubClient(nil) // must not panic
	nilBoot.adoptGitHubClient(nil, nil)

	b := &boot{}
	legacy := github.NewClientForTest("http://127.0.0.1:1", "acme", nil, quietLogger())
	b.ghClient = legacy
	if b.currentGitHubClient() != legacy {
		t.Fatal("unpublished provider must fall back to b.ghClient")
	}
	// Published nil means "no client", not "fall back".
	b.publishGitHubClient(nil)
	if b.currentGitHubClient() != nil {
		t.Fatal("published nil client fell back to a stale b.ghClient")
	}
	b.adoptGitHubClient(nil, nil)
	if b.ghClient != legacy {
		t.Fatal("adopting a nil client replaced the hive's client")
	}
}

// --- source-level pin ---------------------------------------------------------

// TestLongLivedGitHubConsumersReadThroughProvider fails when a long-lived
// consumer is constructed with the boot-time b.ghClient again: it would keep
// that client for the life of the process (#9621). Short-lived per-tick calls
// (the eval cycle, sweeps) read b.ghClient when they run and are fine.
func TestLongLivedGitHubConsumersReadThroughProvider(t *testing.T) {
	// consumer call -> index of its client argument.
	consumers := map[string]int{
		"NewMetricsCollector":    0,
		"NewFleetStatsCollector": 0,
		"SetRunTriageDeps":       1,
		"startRequestRelays":     1,
		"startSelfAuthoredSweep": 1,
	}
	seen := map[string]int{}
	providerInstalls := 0
	for _, f := range hivePackageFiles(t) {
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				if sel.Sel.Name == "SetGitHubClientProvider" && len(call.Args) == 1 && exprString(call.Args[0]) == "b.currentGitHubClient" {
					providerInstalls++
				}
				idx, ok := consumers[sel.Sel.Name]
				if !ok || exprString(sel.X) == "" {
					return true
				}
				seen[sel.Sel.Name]++
				if idx < len(call.Args) && exprString(call.Args[idx]) == "b.ghClient" {
					t.Errorf("%s: %s is handed the captured b.ghClient; read the client through "+
						"b.currentGitHubClient or the relay supervisor so rebuilds reach it (#9621)", fd.Name.Name, sel.Sel.Name)
				}
				return true
			})
		}
	}
	for name := range consumers {
		if seen[name] == 0 {
			t.Errorf("no call to %s found; the source walk is broken or the consumer moved", name)
		}
	}
	const collectorsWithProvider = 2 // metrics + fleet stats
	if providerInstalls < collectorsWithProvider {
		t.Errorf("found %d SetGitHubClientProvider(b.currentGitHubClient) calls, want %d (metrics and fleet-stats collectors)",
			providerInstalls, collectorsWithProvider)
	}
}
