package github

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/sentinel"
)

func observerFixture(author string, private bool) *sentinelFixture {
	pr := sentinelPR(3, author, "head3")
	pr["base"] = map[string]any{"repo": map[string]any{"private": private}}
	return &sentinelFixture{
		prs: []map[string]any{pr},
		files: map[int][]map[string]any{3: {{
			"filename": "pkg/x/x.go", "status": "modified", "additions": 2, "deletions": 1, "patch": "@@ -1 +1 @@\n-a\n+b\n",
		}}},
	}
}

type observations struct {
	mu  sync.Mutex
	got []SentinelPatchObservation
}

func (o *observations) add(v SentinelPatchObservation) {
	o.mu.Lock()
	o.got = append(o.got, v)
	o.mu.Unlock()
}

func TestSentinelPatchObserverNilIsNoOp(t *testing.T) {
	f := observerFixture("alice", false)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	res, err := c.SweepSentinel(context.Background(), defaultSentinelOpts(nil))
	if err != nil || res.Skipped != 1 || len(res.Errors) != 0 || f.filesHits != 1 {
		t.Fatalf("res=%+v err=%v filesHits=%d", res, err, f.filesHits)
	}
}

func TestSentinelPatchObserverReceivesFetchedFiles(t *testing.T) {
	f := observerFixture("alice", true)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	var obs observations
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = obs.add
	before := time.Now()
	if _, err := c.SweepSentinel(context.Background(), opts); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if len(obs.got) != 1 || f.filesHits != 1 {
		t.Fatalf("observations=%d filesHits=%d", len(obs.got), f.filesHits)
	}
	o := obs.got[0]
	if o.Repo != "o/r" || o.Number != 3 || o.HeadSHA != "head3" || o.Author != "alice" || !o.Private || o.At.Before(before) {
		t.Fatalf("observation=%+v", o)
	}
	want := sentinel.File{Path: "pkg/x/x.go", Status: "modified", Additions: 2, Deletions: 1, Patch: "@@ -1 +1 @@\n-a\n+b\n"}
	if len(o.Files) != 1 || o.Files[0] != want {
		t.Fatalf("files=%+v", o.Files)
	}

	// Same head again: the sentinel skips it, so nothing is observed.
	if _, err := c.SweepSentinel(context.Background(), opts); err != nil {
		t.Fatalf("sweep 2: %v", err)
	}
	if len(obs.got) != 1 {
		t.Fatalf("unchanged head observed again: %d", len(obs.got))
	}
}

func TestSentinelPatchObserverPublicRepoNotPrivate(t *testing.T) {
	f := observerFixture("alice", false)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	var obs observations
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = obs.add
	if _, err := c.SweepSentinel(context.Background(), opts); err != nil || len(obs.got) != 1 || obs.got[0].Private {
		t.Fatalf("err=%v obs=%+v", err, obs.got)
	}
}

func TestSentinelPatchObserverNotCalledOnFetchFailure(t *testing.T) {
	f := observerFixture("alice", false)
	f.filesStatus = 502
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	var obs observations
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = obs.add
	res, _ := c.SweepSentinel(context.Background(), opts)
	if len(res.Errors) != 1 || len(obs.got) != 0 {
		t.Fatalf("res=%+v obs=%d", res, len(obs.got))
	}

	// Incomplete list (reported > returned) is not observed either. The
	// head must change: the sweep records head3 as seen before the file
	// fetch, so re-sweeping the same head would be skipped, not re-fetched.
	f.mu.Lock()
	f.filesStatus = 0
	f.prs[0]["changed_files"] = 3
	f.prs[0]["head"] = map[string]string{"sha": "head3b"}
	f.mu.Unlock()
	res, _ = c.SweepSentinel(context.Background(), opts)
	if len(res.Errors) != 1 || len(obs.got) != 0 {
		t.Fatalf("res=%+v obs=%d", res, len(obs.got))
	}
}

func TestSentinelPatchObserverHiveIdentityFailsClosed(t *testing.T) {
	f := observerFixture("hive[bot]", false)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	if c.knowsOwnAccounts() {
		t.Fatal("fixture must leave the hive unable to name its own accounts")
	}
	var obs observations
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = obs.add
	if _, err := c.SweepSentinel(context.Background(), opts); err != nil || len(obs.got) != 1 || obs.got[0].HiveIdentified {
		t.Fatalf("unknown identity must not be identified: err=%v obs=%+v", err, obs.got)
	}
}

func TestSentinelPatchObserverHiveIdentity(t *testing.T) {
	for _, tc := range []struct {
		author string
		want   bool
	}{{"hive[bot]", true}, {"stranger", false}} {
		f := observerFixture(tc.author, false)
		srv := newSentinelServer(t, f)
		c := newTestClient(t, srv, "o", []string{"o/r"})
		c.additionalBotLogins = []string{"hive[bot]"}
		var obs observations
		opts := defaultSentinelOpts(nil)
		opts.PatchObserver = obs.add
		if _, err := c.SweepSentinel(context.Background(), opts); err != nil || len(obs.got) != 1 || obs.got[0].HiveIdentified != tc.want {
			t.Fatalf("author %s: err=%v obs=%+v", tc.author, err, obs.got)
		}
		srv.Close()
	}
}

func TestSentinelPatchObserverPanicIsIsolated(t *testing.T) {
	f := observerFixture("alice", false)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = func(SentinelPatchObservation) { panic("boom") }
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil || res.Skipped != 1 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
}

func TestSentinelPatchObserverSlowIsBounded(t *testing.T) {
	old := sentinelPatchObserverTimeout
	sentinelPatchObserverTimeout = 50 * time.Millisecond
	defer func() { sentinelPatchObserverTimeout = old }()
	f := observerFixture("alice", false)
	srv := newSentinelServer(t, f)
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	release := make(chan struct{})
	defer close(release)
	opts := defaultSentinelOpts(nil)
	opts.PatchObserver = func(SentinelPatchObservation) { <-release }
	start := time.Now()
	res, err := c.SweepSentinel(context.Background(), opts)
	if err != nil || res.Skipped != 1 || len(res.Errors) != 0 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("sweep blocked on slow observer for %v", d)
	}
}
