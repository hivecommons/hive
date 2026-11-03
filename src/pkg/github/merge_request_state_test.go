package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingCIServer serves the fixture's CI evidence and counts PR GETs, the
// first call of every CI-gate evaluation, so a test can tell an evaluated
// tick from a skipped one.
func countingCIServer(t *testing.T, f *ciFixture, prGets *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/42") {
			prGets.Add(1)
		}
		if f.serveCI(w, r) {
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
}

func TestMergeCIPendingDelay(t *testing.T) {
	for _, tc := range []struct {
		pendingFor time.Duration
		want       time.Duration
	}{
		{0, 0},
		{59 * time.Second, 0},
		{time.Minute, time.Minute},
		{9 * time.Minute, time.Minute},
		{10 * time.Minute, 5 * time.Minute},
		{3 * time.Hour, 5 * time.Minute},
	} {
		if got := mergeCIPendingDelay(tc.pendingFor); got != tc.want {
			t.Errorf("mergeCIPendingDelay(%s) = %s, want %s", tc.pendingFor, got, tc.want)
		}
	}
}

// Pending CI for one PR+SHA is polled every tick for the first minute, then
// once a minute, then every five minutes (#11895), and the wait still ends
// after an hour even though far fewer than mergeRequestMaxCIWaits polls ran.
func TestMergeRequestWatcher_PendingCIBacksOff(t *testing.T) {
	f := &ciFixture{defaultHead: "abc", checks: []ciCheck{{name: "build", status: "queued"}}}
	var prGets atomic.Int32
	srv := countingCIServer(t, f, &prGets)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)

	start := fixedNow()
	clock := start
	now := func() time.Time { return clock }
	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})

	steps := []struct {
		at        time.Duration
		evaluated bool
	}{
		{0, true},
		{10 * time.Second, true},
		{50 * time.Second, true},
		{70 * time.Second, true}, // pending >= 1m: next poll at 130s
		{80 * time.Second, false},
		{129 * time.Second, false},
		{130 * time.Second, true}, // next at 190s
		{180 * time.Second, false},
		{11 * time.Minute, true}, // pending >= 10m: next at 16m
		{15 * time.Minute, false},
		{16 * time.Minute, true},
	}
	for _, step := range steps {
		clock = start.Add(step.at)
		before := prGets.Load()
		c.handleOneMergeRequest(context.Background(), reqPath, now)
		if evaluated := prGets.Load() != before; evaluated != step.evaluated {
			t.Fatalf("at +%s: evaluated=%v, want %v", step.at, evaluated, step.evaluated)
		}
	}
	resp := readMergeResult(t, reqPath)
	if resp.Attempts != 0 || resp.CIWaits != 7 {
		t.Fatalf("expected 7 recorded waits and no attempt, got %+v", resp)
	}

	clock = start.Add(time.Hour + time.Minute)
	c.handleOneMergeRequest(context.Background(), reqPath, now)
	resp = readMergeResult(t, reqPath)
	if resp.Attempts != 1 || !strings.Contains(resp.Error, "still pending after") {
		t.Fatalf("an hour of pending CI must become a failed attempt, got %+v", resp)
	}
}

// The wait counters and the exhausted record live in the /data ledger, so a
// restart (new client, tmpfs request dir wiped) cannot reset them, and a
// re-dropped request for an exhausted head is refused without any API call
// (#11894). A new head SHA starts fresh.
func TestMergeRequestWatcher_StateSurvivesRestart(t *testing.T) {
	statePath := filepath.Join(t.TempDir(), "merge-request-state.json")
	newClient := func(srvURL string) *Client {
		c := testMergeClient(t, srvURL)
		c.SetMergeRequestStatePath(statePath)
		return c
	}

	cases := []struct {
		name  string
		setup func(t *testing.T, f *ciFixture, srvURL string)
		check func(t *testing.T, f *ciFixture, srvURL string, prGets *atomic.Int32)
	}{
		{
			name: "pending waits accumulate across restarts and request files",
			setup: func(t *testing.T, f *ciFixture, srvURL string) {
				f.checks = []ciCheck{{name: "build", status: "queued"}}
				reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
				newClient(srvURL).handleOneMergeRequest(context.Background(), reqPath, fixedNow)
			},
			check: func(t *testing.T, f *ciFixture, srvURL string, prGets *atomic.Int32) {
				reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
				newClient(srvURL).handleOneMergeRequest(context.Background(), reqPath, fixedNow)
				if resp := readMergeResult(t, reqPath); resp.CIWaits != 2 {
					t.Fatalf("restart must not reset ci_waits, got %+v", resp)
				}
			},
		},
		{
			name: "exhausted head is refused after restart, new head is evaluated",
			setup: func(t *testing.T, f *ciFixture, srvURL string) {
				// Zero evidence: unverified, a failed attempt every tick.
				reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
				c := newClient(srvURL)
				for i := 0; i < mergeRequestMaxAttempts; i++ {
					c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
				}
				mustExist(t, reqPath+".exhausted", "request after retry budget")
			},
			check: func(t *testing.T, f *ciFixture, srvURL string, prGets *atomic.Int32) {
				c := newClient(srvURL)
				before := prGets.Load()
				reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
				c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
				if prGets.Load() != before {
					t.Fatalf("refusing an exhausted head must not call GitHub")
				}
				resp := readMergeResult(t, reqPath)
				if resp.OK || !strings.Contains(resp.Error, "exhausted on the same head") || !strings.Contains(resp.Error, "absent CI is not passing") {
					t.Fatalf("expected a refusal carrying the recorded reason, got %+v", resp)
				}
				mustExist(t, reqPath+".exhausted", "refused re-request")

				// After the refusal window the same head is evaluated again.
				later := func() time.Time { return fixedNow().Add(mergeRequestExhaustedRefusal + time.Minute) }
				retry, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
				c.handleOneMergeRequest(context.Background(), retry, later)
				if resp := readMergeResult(t, retry); resp.Attempts != 1 || strings.Contains(resp.Error, "exhausted on the same head") {
					t.Fatalf("an expired exhausted record must not refuse, got %+v", resp)
				}

				f.defaultHead = "def"
				moved, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "def", Agent: "scanner"})
				c.handleOneMergeRequest(context.Background(), moved, fixedNow)
				if resp := readMergeResult(t, moved); resp.Attempts != 1 || strings.Contains(resp.Error, "exhausted on the same head") {
					t.Fatalf("a new head must start a fresh attempt cycle, got %+v", resp)
				}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_ = os.Remove(statePath)
			f := &ciFixture{defaultHead: "abc"}
			var prGets atomic.Int32
			srv := countingCIServer(t, f, &prGets)
			defer srv.Close()
			tc.setup(t, f, srv.URL)

			b, err := os.ReadFile(statePath)
			if err != nil {
				t.Fatalf("wait state was not persisted: %v", err)
			}
			var ledger mergeRequestStateLedger
			if err := json.Unmarshal(b, &ledger); err != nil {
				t.Fatalf("bad ledger json: %v", err)
			}
			if st, ok := ledger.PRs["o/r#42"]; !ok || st.SHA != "abc" {
				t.Fatalf("expected a ledger entry for o/r#42@abc, got %+v", ledger.PRs)
			}
			tc.check(t, f, srv.URL, &prGets)
		})
	}
}

// A merged PR drops its ledger entry so the file does not accumulate.
func TestMergeRequestWatcher_MergeClearsState(t *testing.T) {
	f := greenFixture()
	f.defaultHead = "abc"
	var merges atomic.Int32
	srv := ciGateServer(t, f, &merges)
	defer srv.Close()
	c := testMergeClient(t, srv.URL)
	c.SetMergeRequestStatePath(filepath.Join(t.TempDir(), "merge-request-state.json"))
	c.putMergeRequestState("o/r", 42, mergeRequestPRState{SHA: "abc", CIWaits: 3, At: fixedNow()})

	reqPath, _ := WriteMergeRequest(t.TempDir(), MergeRequest{Repo: "o/r", Number: 42, ExpectSHA: "abc", Agent: "scanner"})
	c.handleOneMergeRequest(context.Background(), reqPath, fixedNow)
	if merges.Load() != 1 {
		t.Fatalf("green CI should merge, got %d (%+v)", merges.Load(), readMergeResult(t, reqPath))
	}
	if _, ok := c.mergeRequestState("o/r", 42, "abc"); ok {
		t.Fatalf("merged PR must not keep wait state")
	}
}
