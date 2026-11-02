package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const terminalEnrichmentTestTimeout = 5 * time.Second

func TestTerminalEnrichmentIdenticalInFlightReplayDoesNotStarveNextTask(t *testing.T) {
	c := &Client{}
	deferTerminalTasks(c)
	first, next := historicalTask(1, time.Now()), historicalTask(2, time.Now())
	c.observeTerminalPR(context.Background(), first)
	c.observeTerminalPR(context.Background(), next)
	var seen []int
	c.SetPRTerminalObservedHook(func(obs PRTerminalObservation) {
		seen = append(seen, obs.Number)
		if obs.Number == first.observation.Number {
			c.observeTerminalPR(context.Background(), first)
		}
	})
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, 1)
	if _, exists := c.prTerminalEnrichment.pending[first.key()]; exists {
		t.Fatal("identical in-flight replay prevented acknowledgment")
	}
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, 1)
	if len(seen) != 2 || seen[1] != next.observation.Number || len(c.prTerminalEnrichment.pending) != 0 {
		t.Fatalf("identical old replay starved subsequent history: %v", seen)
	}
}

func TestTerminalEnrichmentReplacedClientStopsWithoutRootCancellation(t *testing.T) {
	c := &Client{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := c.StartPRTerminalEnrichment(ctx)
	c.StopPRTerminalEnrichment()
	waitTerminalEnrichment(t, done)
	if ctx.Err() != nil {
		t.Fatal("retiring old client cancelled shared boot context")
	}
}

func TestTerminalEnrichmentCoalescesWithoutOverwritingNewerHistory(t *testing.T) {
	c := &Client{}
	deferTerminalTasks(c)
	at := time.Now()
	current := historicalTask(1, at)
	current.observation.URL = "current"
	older := historicalTask(1, at.Add(-time.Hour))
	older.observation.URL = "obsolete"
	c.observeTerminalPR(context.Background(), current)
	c.observeTerminalPR(context.Background(), older)
	var seen []PRTerminalObservation
	c.SetPRTerminalObservedHook(func(obs PRTerminalObservation) { seen = append(seen, obs) })
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(seen) != 1 || seen[0].URL != "current" {
		t.Fatalf("older scan replaced current historical evidence: %+v", seen)
	}
}

func waitTerminalEnrichment(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(terminalEnrichmentTestTimeout):
		t.Fatal("terminal enrichment did not reach synchronization point")
	}
}

func deferTerminalTasks(c *Client) {
	c.prTerminalEnrichment.started = true
	c.prTerminalEnrichment.pending = make(map[string]terminalEnrichmentTask)
	c.prTerminalEnrichment.wake = make(chan struct{}, 1)
}

func historicalTask(number int, at time.Time) terminalEnrichmentTask {
	return terminalEnrichmentTask{
		owner: "torch-spyre", repoName: "spyre-inference", updatedAt: at,
		observation: PRTerminalObservation{
			Repo: "spyre-inference", Number: number, State: PRStateClosed, ObservedAt: at,
		},
	}
}

func TestTerminalEnrichmentSlowReviewsDoNotBlockClaimSafety(t *testing.T) {
	entered := make(chan struct{})
	var once sync.Once
	var failListing atomic.Bool
	now := time.Now().UTC().Truncate(time.Second)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/reviews") {
			once.Do(func() { close(entered) })
			<-r.Context().Done()
			return
		}
		if failListing.Load() {
			http.Error(w, "listing unavailable", http.StatusForbidden)
			return
		}
		if r.URL.Query().Get("state") == "closed" {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				mergedPR(500, "kubestellar-hive[bot]", "done", "Fixes #200", now, now),
			})
		} else {
			_ = json.NewEncoder(w).Encode([]map[string]any{
				pr(423, "kubestellar-hive[bot]", "current work", "Fixes #100"),
			})
		}
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	c.SetPRReviewObservedHook(func(PRReviewObservation) {})
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartPRTerminalEnrichment(ctx)
	defer func() { cancel(); waitTerminalEnrichment(t, done) }()
	if c.StartPRTerminalEnrichment(ctx) != done {
		t.Fatal("starting twice created another worker")
	}
	identity := HiveIdentity{AppLogin: "kubestellar-hive[bot]"}
	scan, err := c.FetchClaimScan(ctx, identity)
	if err != nil || len(scan.Claims) != 2 {
		t.Fatalf("initial authoritative claims: %+v, %v", scan, err)
	}
	waitTerminalEnrichment(t, entered)
	// Historical HTTP remains blocked; another current-work scan must finish.
	currentCtx, stop := context.WithTimeout(ctx, terminalEnrichmentTestTimeout)
	defer stop()
	repeated, err := c.FetchClaimScan(currentCtx, identity)
	if err != nil || len(repeated.Claims) != 2 {
		t.Fatalf("current claims waited for historical reviews: %+v, %v", repeated, err)
	}
	ledger := NewClaimLedger("", testLogger())
	ledger.ReconcileScan(scan, true, nil)
	failListing.Store(true)
	partial, err := c.FetchClaimScan(currentCtx, identity)
	if err == nil {
		t.Fatal("failed authoritative listing reported success")
	}
	ledger.ReconcileScan(partial, false, nil)
	if claims := ledger.Claims(); len(claims) != 2 {
		t.Fatalf("historical isolation released claims on failed listing: %+v", claims)
	}
}

func TestTerminalEnrichmentSlowAccountingDoesNotBlockClaimScan(t *testing.T) {
	now := time.Now()
	srv := mergedClaimServer(t,
		[]map[string]any{pr(423, "kubestellar-hive[bot]", "current", "Fixes #100")},
		[]map[string]any{mergedPR(500, "kubestellar-hive[bot]", "done", "Fixes #200", now, now)})
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	c.SetPRTerminalObservedHook(func(PRTerminalObservation) {
		once.Do(func() { close(entered) })
		<-release
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := c.StartPRTerminalEnrichment(ctx)
	defer func() { close(release); cancel(); waitTerminalEnrichment(t, done) }()
	identity := HiveIdentity{AppLogin: "kubestellar-hive[bot]"}
	if _, err := c.FetchClaimScan(ctx, identity); err != nil {
		t.Fatal(err)
	}
	waitTerminalEnrichment(t, entered)
	currentCtx, stop := context.WithTimeout(ctx, terminalEnrichmentTestTimeout)
	defer stop()
	scan, err := c.FetchClaimScan(currentCtx, identity)
	if err != nil || len(scan.Claims) != 2 {
		t.Fatalf("slow accounting blocked current claim servicing: %+v, %v", scan, err)
	}
}

func TestTerminalEnrichmentBatchLimitAndInFlightRevision(t *testing.T) {
	c := &Client{}
	deferTerminalTasks(c)
	at := time.Now()
	first, next := historicalTask(1, at), historicalTask(2, at)
	c.observeTerminalPR(context.Background(), first)
	c.observeTerminalPR(context.Background(), next)
	var seen []int
	c.SetPRTerminalObservedHook(func(obs PRTerminalObservation) {
		seen = append(seen, obs.Number)
		if len(seen) == 1 {
			// Even an equal updated_at can carry corrected metadata.
			newer := first
			newer.observation.URL = "corrected"
			c.observeTerminalPR(context.Background(), newer)
		}
	})
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, 1)
	if len(seen) != 1 || seen[0] != 1 || len(c.prTerminalEnrichment.pending) != 2 {
		t.Fatalf("batch cap or in-flight update lost: seen=%v pending=%v", seen, c.prTerminalEnrichment.pending)
	}
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(c.prTerminalEnrichment.pending) != 0 || len(seen) != 3 || seen[2] != 2 {
		t.Fatalf("deferred work did not drain fairly: seen=%v pending=%v", seen, c.prTerminalEnrichment.pending)
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	c.observeTerminalPR(context.Background(), first)
	c.enrichTerminalPRBatch(cancelled, terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(seen) != 3 || len(c.prTerminalEnrichment.pending) != 1 {
		t.Fatal("cancelled batch consumed accounting")
	}
}

func TestTerminalEnrichmentFailedReviewsRetryWithoutBlockingHealthyTasks(t *testing.T) {
	var failed atomic.Bool
	failed.Store(true)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/1/") && failed.Load() {
			http.Error(w, "temporary", http.StatusForbidden)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	deferTerminalTasks(c)
	c.SetPRReviewObservedHook(func(PRReviewObservation) {})
	first := historicalTask(1, time.Now())
	c.observeTerminalPR(context.Background(), first)
	c.observeTerminalPR(context.Background(), historicalTask(2, first.updatedAt))
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(c.prTerminalEnrichment.pending) != 1 {
		t.Fatal("failed accounting disappeared or healthy accounting was blocked")
	}
	before := requests.Load()
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if requests.Load() != before {
		t.Fatal("failed historical request bypassed retry backoff")
	}
	failed.Store(false)
	c.prTerminalEnrichment.retries.clear(first.key())
	c.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(c.prTerminalEnrichment.pending) != 0 {
		t.Fatal("successful retry did not retain and acknowledge historical work")
	}
}

func TestTerminalEnrichmentDeadlineRetainsUnfinishedAccounting(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer srv.Close()
	c := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	deferTerminalTasks(c)
	c.SetPRReviewObservedHook(func(PRReviewObservation) {})
	c.observeTerminalPR(context.Background(), historicalTask(1, time.Now()))
	// The production budget is ten seconds; this fixture uses a short deadline,
	// not a sleep, so CI does not spend the real servicing interval waiting.
	const budget = 20 * time.Millisecond
	c.enrichTerminalPRBatch(context.Background(), budget, terminalEnrichmentBatch)
	if len(c.prTerminalEnrichment.pending) != 1 {
		t.Fatal("deadline lost the unfinished historical observation")
	}
}

func TestTerminalEnrichmentRestartReplaysUnprocessedRecentHistory(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	srv := mergedClaimServer(t, nil, []map[string]any{
		mergedPR(500, "kubestellar-hive[bot]", "done", "Fixes #200", now, now),
	})
	identity := HiveIdentity{AppLogin: "kubestellar-hive[bot]"}
	before := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	deferTerminalTasks(before)
	if _, err := before.FetchClaimScan(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if len(before.prTerminalEnrichment.pending) != 1 {
		t.Fatal("fixture did not leave historical accounting outstanding")
	}
	after := NewClientForTest(srv.URL, "torch-spyre", []string{"spyre-inference"}, testLogger())
	deferTerminalTasks(after)
	var observed []PRTerminalObservation
	after.SetPRTerminalObservedHook(func(obs PRTerminalObservation) { observed = append(observed, obs) })
	scan, err := after.FetchClaimScan(context.Background(), identity)
	if err != nil || len(scan.Claims) != 1 {
		t.Fatalf("restart lost authoritative merged claim: %+v, %v", scan, err)
	}
	after.enrichTerminalPRBatch(context.Background(), terminalEnrichmentTestTimeout, terminalEnrichmentBatch)
	if len(observed) != 1 || !observed[0].ObservedAt.Equal(now) {
		t.Fatalf("restart did not replay the real historical timestamp: %+v", observed)
	}
}
