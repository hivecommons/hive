package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ciFailedFixture serves acme/widget (default branch main) for the ci.failed
// tests. runs is the tip's check-run list; suites maps a check suite ID to
// its head branch. required puts "unit" in classic protection; without it
// the repo has no required checks, so the red-main breaker skips it.
type ciFailedFixture struct {
	mu          sync.Mutex
	required    bool
	runs        []map[string]any
	suites      map[int64]string
	suitesFail  bool
	checkRuns   int
	suiteReads  int
	suitesBySHA []string
}

func (f *ciFailedFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "widget", "default_branch": "main"})
	})
	mux.HandleFunc("/repos/acme/widget/branches/main/protection/required_status_checks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		contexts := []string{}
		if f.required {
			contexts = []string{"unit"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"strict": false, "contexts": contexts})
	})
	mux.HandleFunc("/repos/acme/widget/rules/branches/main", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/issues", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/check-runs/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("[]"))
	})
	mux.HandleFunc("/repos/acme/widget/commits/", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := strings.TrimPrefix(r.URL.Path, "/repos/acme/widget/commits/")
		ref, endpoint, _ := strings.Cut(rest, "/")
		switch endpoint {
		case "check-runs":
			if ref != "main" {
				t.Errorf("check runs read for ref %q, want the default branch", ref)
			}
			f.checkRuns++
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(f.runs), "check_runs": f.runs})
		case "check-suites":
			f.suiteReads++
			f.suitesBySHA = append(f.suitesBySHA, ref)
			if f.suitesFail {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			suites := []map[string]any{}
			for id, branch := range f.suites {
				suites = append(suites, map[string]any{"id": id, "head_branch": branch, "head_sha": ref})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(suites), "check_suites": suites})
		default:
			http.NotFound(w, r)
		}
	})
	return httptest.NewServer(mux)
}

func (f *ciFailedFixture) set(runs []map[string]any, suites map[int64]string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.runs, f.suites = runs, suites
}

func (f *ciFailedFixture) counts() (checkRuns, suiteReads int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.checkRuns, f.suiteReads
}

func ciFailedRun(id int64, name, sha, conclusion string, suite int64) map[string]any {
	return map[string]any{
		"id": id, "name": name, "head_sha": sha, "status": "completed", "conclusion": conclusion,
		"html_url":     "https://github.test/acme/widget/runs/" + name,
		"check_suite":  map[string]any{"id": suite},
		"app":          map[string]any{"id": 15368},
		"started_at":   "2026-11-10T14:16:00Z",
		"completed_at": "2026-11-10T14:30:00Z",
	}
}

type ciFailedRecorder struct {
	mu   sync.Mutex
	runs []CIFailedRun
}

func (r *ciFailedRecorder) emit(_ context.Context, run CIFailedRun) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs = append(r.runs, run)
}

func (r *ciFailedRecorder) take() []CIFailedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.runs
	r.runs = nil
	return out
}

// A default-branch failure is reported once per (repo, check, head SHA),
// for every check (none is required here), with failure and timed_out
// counted and cancelled, success and a pull request's run on the same
// commit left out. A later failing SHA is reported again.
func TestSweepRedMainCIFailedOncePerSHA(t *testing.T) {
	fx := &ciFailedFixture{}
	fx.set([]map[string]any{
		ciFailedRun(21, "build", "aaa111", "failure", 901),
		ciFailedRun(22, "lint", "aaa111", "cancelled", 901),
		ciFailedRun(23, "unit", "aaa111", "success", 901),
		ciFailedRun(24, "slow", "aaa111", "timed_out", 901),
		ciFailedRun(25, "pr-check", "aaa111", "failure", 902),
	}, map[int64]string{901: "main", 902: "feature-x"})
	srv := fx.server(t)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	rec := &ciFailedRecorder{}
	c.SetCIFailedHook(func() bool { return true }, rec.emit)
	now := time.Date(2026, 11, 10, 14, 40, 0, 0, time.UTC)

	res, err := c.SweepRedMain(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Red != 0 || res.Skipped != 1 {
		t.Fatalf("no required checks: the breaker must still skip the repo, got %+v", res)
	}
	got := rec.take()
	if len(got) != 2 {
		t.Fatalf("emitted %+v, want build (failure) and slow (timed_out) only", got)
	}
	want := []CIFailedRun{
		{Repo: "widget", Branch: "main", Check: "build", HeadSHA: "aaa111", Conclusion: "failure", URL: "https://github.test/acme/widget/runs/build"},
		{Repo: "widget", Branch: "main", Check: "slow", HeadSHA: "aaa111", Conclusion: "timed_out", URL: "https://github.test/acme/widget/runs/slow"},
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("emit[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
	fx.mu.Lock()
	suiteRefs := append([]string(nil), fx.suitesBySHA...)
	fx.mu.Unlock()
	if len(suiteRefs) != 1 || suiteRefs[0] != "aaa111" {
		t.Fatalf("check suites read for %v, want once for the tip SHA", suiteRefs)
	}

	// Same red SHA on the next pass: nothing new, and no suite read either.
	if _, err := c.SweepRedMain(context.Background(), now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("same SHA re-emitted: %+v", got)
	}
	if _, suites := fx.counts(); suites != 1 {
		t.Fatalf("check suites read again for an already reported SHA (%d reads)", suites)
	}

	// A new push that fails again fires again; the check that went green
	// does not.
	fx.set([]map[string]any{
		ciFailedRun(31, "build", "bbb222", "failure", 903),
		ciFailedRun(34, "slow", "bbb222", "success", 903),
	}, map[int64]string{903: "main"})
	if _, err := c.SweepRedMain(context.Background(), now.Add(20*time.Minute)); err != nil {
		t.Fatal(err)
	}
	got = rec.take()
	if len(got) != 1 || got[0].Check != "build" || got[0].HeadSHA != "bbb222" {
		t.Fatalf("new failing SHA: emitted %+v, want build at bbb222 once", got)
	}
}

// With no trigger rule naming ci.failed (or no hook at all) the sweep is
// unchanged: a repo without required checks is skipped before its check
// runs are read, and no check suite is ever read.
func TestSweepRedMainCIFailedNoRuleIsNoOp(t *testing.T) {
	for _, tc := range []struct {
		name string
		hook func(*Client, *ciFailedRecorder)
	}{
		{"no hook", func(*Client, *ciFailedRecorder) {}},
		{"no ci.failed rule", func(c *Client, rec *ciFailedRecorder) {
			c.SetCIFailedHook(func() bool { return false }, rec.emit)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := &ciFailedFixture{}
			fx.set([]map[string]any{ciFailedRun(21, "build", "aaa111", "failure", 901)}, map[int64]string{901: "main"})
			srv := fx.server(t)
			defer srv.Close()
			c := newTestClient(t, srv, "acme", []string{"widget"})
			rec := &ciFailedRecorder{}
			tc.hook(c, rec)
			now := time.Date(2026, 11, 10, 14, 40, 0, 0, time.UTC)

			res, err := c.SweepRedMain(context.Background(), now)
			if err != nil || res.Skipped != 1 {
				t.Fatalf("SweepRedMain = %+v, %v", res, err)
			}
			if runs, suites := fx.counts(); runs != 0 || suites != 0 {
				t.Fatalf("read check runs %d / check suites %d times, want 0 / 0", runs, suites)
			}

			// A repo the breaker watches reads its check runs as before,
			// still without a suite read or an emit.
			fx.mu.Lock()
			fx.required = true
			fx.mu.Unlock()
			if _, err := c.SweepRedMain(context.Background(), now.Add(10*time.Minute)); err != nil {
				t.Fatal(err)
			}
			if runs, suites := fx.counts(); runs != 1 || suites != 0 {
				t.Fatalf("read check runs %d / check suites %d times, want 1 / 0", runs, suites)
			}
			if got := rec.take(); len(got) != 0 {
				t.Fatalf("emitted without a ci.failed rule: %+v", got)
			}
		})
	}
}

// A failed check-suite read reports nothing and records nothing, so the
// next pass reports the failure.
func TestSweepRedMainCIFailedSuiteReadErrorRetries(t *testing.T) {
	fx := &ciFailedFixture{suitesFail: true}
	fx.set([]map[string]any{ciFailedRun(21, "build", "aaa111", "failure", 901)}, map[int64]string{901: "main"})
	srv := fx.server(t)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	rec := &ciFailedRecorder{}
	c.SetCIFailedHook(func() bool { return true }, rec.emit)
	now := time.Date(2026, 11, 10, 14, 40, 0, 0, time.UTC)

	if _, err := c.SweepRedMain(context.Background(), now); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 0 {
		t.Fatalf("emitted without knowing the run's branch: %+v", got)
	}
	fx.mu.Lock()
	fx.suitesFail = false
	fx.mu.Unlock()
	if _, err := c.SweepRedMain(context.Background(), now.Add(10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := rec.take(); len(got) != 1 || got[0].Check != "build" {
		t.Fatalf("after the suite read recovered: emitted %+v, want build once", got)
	}
}
