package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
)

// A code_scanning change made on the live config between two sweep passes is
// what the next pass runs with, on the same worker, with no restart.
//
// PUT /api/config/code-scanning changes boot.cfg in place (the dashboard's
// deps.Config is that pointer). The sweep workers never read it: they read
// the snapshot publishSweepConfig stores, which the run loop refreshes at the
// top of every governor tick. This test drives that path with the real
// pieces: the live config, publishSweepConfig, the code_scanning sweep job
// and runSweepWorker, against a GitHub stub that counts alert listings.
func TestCodeScanningSweepWorkerRunsWithSettingsChangedBetweenPasses(t *testing.T) {
	t.Cleanup(github.ResetRESTAccountingForTest(nil))
	var mu sync.Mutex
	listed := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/code-scanning/alerts"):
			mu.Lock()
			listed[r.URL.Path]++
			mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues"):
			_, _ = w.Write([]byte(`[]`))
		default:
			// With no open alert the sweep has nothing to write.
			if r.Method != http.MethodGet {
				t.Errorf("unexpected GitHub write: %s %s", r.Method, r.URL.Path)
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		}
	}))
	defer srv.Close()
	const widgetAlerts, gadgetAlerts = "/repos/testorg/widget/code-scanning/alerts", "/repos/testorg/gadget/code-scanning/alerts"
	listings := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return listed[path]
	}
	ghClient := github.NewClientForTest(srv.URL, "testorg", []string{"widget", "gadget"}, sweepTestLogger(nil))

	live := &config.Config{}
	live.Project.Org = "testorg"
	live.Project.Repos = []string{"widget", "gadget"}
	b := &boot{cfg: live, logger: sweepTestLogger(nil)}
	b.publishSweepConfig()

	var job sweepJob
	for _, j := range b.sweepJobs() {
		if j.name == "code_scanning" {
			job = j
		}
	}
	if job.run == nil {
		t.Fatal("no code_scanning sweep job")
	}

	ctx, cancel := context.WithCancel(context.Background())
	ticker := newFakeTicker()
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	now := func() time.Time { return time.Unix(0, clock.Load()) }
	ran := make(chan struct{}, 4)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runSweepWorker(ctx, job, 0, b.sweepConfig.Load, func(time.Duration) loopTicker { return ticker }, now, func(cfg *config.Config, _ uint64) {
			job.runTagged(ctx, ghClient, cfg)
			ran <- struct{}{}
		})
	}()
	defer func() { cancel(); waitDone(t, done) }()
	waitPass := func(what string) {
		t.Helper()
		select {
		case <-ran:
		case <-time.After(5 * time.Second):
			t.Fatalf("no sweep pass: %s", what)
		}
	}
	// nextPass lets one interval go by on the same worker.
	nextPass := func(what string) {
		t.Helper()
		clock.Add(int64(job.interval(b.sweepConfig.Load())))
		select {
		case ticker.ch <- now():
		case <-time.After(5 * time.Second):
			t.Fatalf("sweep timer stalled: %s", what)
		}
		waitPass(what)
	}

	// Pass 1: the sweep is off everywhere, so it reads nothing.
	waitPass("first pass")
	if n := listings(widgetAlerts) + listings(gadgetAlerts); n != 0 {
		t.Fatalf("a sweep that is off listed alerts %d times", n)
	}

	// The owner turns the sweep on for widget only. The handler changes
	// the live block; the worker's snapshot is a copy and does not move
	// until the governor tick publishes the next one.
	on := true
	live.CodeScanning.Repos = map[string]config.CodeScanningRepoConfig{"widget": {Enabled: &on}}
	if _, enabled := b.sweepConfig.Load().CodeScanning.For("testorg", "widget"); enabled {
		t.Fatal("the worker's snapshot changed before it was published")
	}
	b.publishSweepConfig()
	nextPass("after turning widget on")
	if listings(widgetAlerts) != 1 || listings(gadgetAlerts) != 0 {
		t.Fatalf("after turning widget on: widget listed %d times, gadget %d; want 1 and 0", listings(widgetAlerts), listings(gadgetAlerts))
	}

	// Turned off again: the next pass reads nothing more.
	off := false
	live.CodeScanning.Repos = map[string]config.CodeScanningRepoConfig{"widget": {Enabled: &off}}
	b.publishSweepConfig()
	nextPass("after turning widget off")
	if listings(widgetAlerts) != 1 || listings(gadgetAlerts) != 0 {
		t.Fatalf("after turning widget off: widget listed %d times, gadget %d; want still 1 and 0", listings(widgetAlerts), listings(gadgetAlerts))
	}
}
