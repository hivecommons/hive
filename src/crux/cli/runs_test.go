package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
	"github.com/hivecommons/hive/crux/runs"
	"github.com/hivecommons/hive/crux/runstate"
)

// fakeRunsAPI is the contained-run family of a `crux serve` stand-in.
type fakeRunsAPI struct {
	mu        sync.Mutex
	created   []api.ContainedRunRequest
	cancelled []string
}

func (f *fakeRunsAPI) handler() http.Handler {
	exit := func(v int) *int { return &v }
	cut := int64(5)
	records := map[string]api.ContainedRun{
		"run_ok":    {ID: "run_ok", Status: api.RunCompleted, ExitCode: exit(0), Stdout: "hello\n", Stderr: "warned\n"},
		"run_fail":  {ID: "run_fail", Status: api.RunCompleted, ExitCode: exit(2), Reason: "exit status 2"},
		"run_cut":   {ID: "run_cut", Status: api.RunCompleted, ExitCode: exit(0), Stdout: "12345", StdoutTruncatedAt: &cut, Stderr: "abcde", StderrTruncatedAt: &cut},
		"run_lost":  {ID: "run_lost", Status: api.RunUnfinished, Reason: "pod vanished"},
		"run_late":  {ID: "run_late", Status: api.RunFailed, TimedOut: true, Reason: "timed out", AlreadyFinished: true},
		"run_sleep": {ID: "run_sleep", Status: api.RunRunning},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST "+api.PathRuns, func(w http.ResponseWriter, r *http.Request) {
		var req api.ContainedRunRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(api.Error{Error: err.Error(), Code: api.CodeBadRequest})
			return
		}
		f.mu.Lock()
		f.created = append(f.created, req)
		f.mu.Unlock()
		if req.Profile == api.RunProfileAgent {
			w.WriteHeader(http.StatusNotImplemented)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "profile agent is not available yet", Code: api.CodeUnsupported})
			return
		}
		if req.Detach {
			w.WriteHeader(http.StatusAccepted)
			_ = json.NewEncoder(w).Encode(api.ContainedRun{ID: "run_new", Status: api.RunAccepted})
			return
		}
		_ = json.NewEncoder(w).Encode(records["run_ok"])
	})
	mux.HandleFunc("GET "+api.PathRuns+"/{id}", func(w http.ResponseWriter, r *http.Request) {
		rec, ok := records[r.PathValue("id")]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found", Code: api.CodeNotFound})
			return
		}
		_ = json.NewEncoder(w).Encode(rec)
	})
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		f.mu.Lock()
		f.cancelled = append(f.cancelled, id)
		f.mu.Unlock()
		rec, ok := records[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found", Code: api.CodeNotFound})
			return
		}
		if rec.Status == api.RunRunning {
			rec.Status = api.RunCancelled
		}
		_ = json.NewEncoder(w).Encode(rec)
	})
	return mux
}

func TestRunsIsRemoteOnly(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	h := newHarness(t)
	if code := h.run("runs", "status", "run_ok"); code != ExitUsage || !strings.Contains(h.stderr.String(), "needs --server") {
		t.Fatalf("local runs: %d %s", code, h.stderr)
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("runs touched the local runtime: %v", h.rt.Calls())
	}
	h = newHarness(t)
	h.run("help")
	if !strings.Contains(h.stderr.String(), "runs start|status|cancel") {
		t.Fatalf("usage does not list runs:\n%s", h.stderr)
	}
}

func TestRunsStartStatusCancel(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	f := &fakeRunsAPI{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	server := ts.URL

	h := newHarness(t)
	code := h.run("--server", server, "runs", "start", "--repo", "https://github.com/hivecommons/hive", "--commit", strings.Repeat("a", 40),
		"--env", "PATH", "--env", "LANG", "--network", "none", "--timeout", "2s", "--max-output", "64", "--idempotency-key", "k1", "--", "make", "-j2", "test")
	if code != ExitOK {
		t.Fatalf("start: %d %s", code, h.stderr)
	}
	if h.stdout.String() != "hello\n" || !strings.Contains(h.stderr.String(), "run_ok completed exit=0") || !strings.Contains(h.stderr.String(), "warned") {
		t.Fatalf("start output:\nstdout=%q\nstderr=%q", h.stdout, h.stderr)
	}
	req := f.created[0]
	if req.Worktree == nil || req.Worktree.Repo != "https://github.com/hivecommons/hive" || req.Worktree.Commit != strings.Repeat("a", 40) ||
		strings.Join(req.Command, " ") != "make -j2 test" || strings.Join(req.EnvAllowlist, ",") != "PATH,LANG" ||
		req.Network != "none" || req.TimeoutMs != 2000 || req.MaxOutputBytes != 64 || req.IdempotencyKey != "k1" || req.Detach {
		t.Fatalf("request = %+v (worktree %+v)", req, req.Worktree)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "runs", "start", "--workspace", "/srv/ws", "--detach", "--json", "--", "true"); code != ExitOK {
		t.Fatalf("detach: %d %s", code, h.stderr)
	}
	var detached api.ContainedRun
	if err := json.Unmarshal([]byte(h.stdout.String()), &detached); err != nil || detached.ID != "run_new" || detached.Status != api.RunAccepted {
		t.Fatalf("detach json = %q (%v)", h.stdout, err)
	}
	if req := f.created[1]; req.WorkspacePath != "/srv/ws" || req.Worktree != nil || !req.Detach {
		t.Fatalf("detach request = %+v", req)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "runs", "start", "--checkout", "chk_a1b2c3", "--detach", "--", "true"); code != ExitOK {
		t.Fatalf("checkout: %d %s", code, h.stderr)
	}
	if req := f.created[2]; req.Checkout != "chk_a1b2c3" || req.Worktree != nil || req.WorkspacePath != "" {
		t.Fatalf("checkout request = %+v", req)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "runs", "start", "--workspace", "/srv/ws", "--profile", "agent", "--", "true"); code != ExitError || !strings.Contains(h.stderr.String(), "not available yet") {
		t.Fatalf("unsupported: %d %s", code, h.stderr)
	}

	for _, tc := range []struct {
		id     string
		code   int
		stdout string
		stderr string
	}{
		{"run_ok", ExitOK, "hello\n", "run_ok completed exit=0"},
		{"run_fail", ExitError, "", "run_fail completed exit=2 (exit status 2)"},
		{"run_cut", ExitOK, "12345", "stdout truncated at byte 5"},
		{"run_lost", ExitError, "", "run_lost unfinished (pod vanished)"},
		{"run_sleep", ExitOK, "", "run_sleep running"},
		{"run_missing", ExitError, "", "run not found"},
	} {
		h = newHarness(t)
		if code := h.run("--server", server, "runs", "status", tc.id); code != tc.code || h.stdout.String() != tc.stdout || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("status %s: %d stdout=%q stderr=%q", tc.id, code, h.stdout, h.stderr)
		}
	}
	h = newHarness(t)
	if code := h.run("--server", server, "runs", "status", "--json", "run_cut"); code != ExitOK || !strings.Contains(h.stdout.String(), `"stderr_truncated_at": 5`) {
		t.Fatalf("status json: %d %s", code, h.stdout)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "runs", "cancel", "run_sleep"); code != ExitError || !strings.Contains(h.stderr.String(), "run_sleep cancelled") {
		t.Fatalf("cancel: %d %s", code, h.stderr)
	}
	h = newHarness(t)
	if code := h.run("--server", server, "runs", "cancel", "--json", "run_late"); code != ExitError || !strings.Contains(h.stderr.String(), "had already finished") || !strings.Contains(h.stdout.String(), `"timed_out": true`) {
		t.Fatalf("late cancel: %d stdout=%s stderr=%s", code, h.stdout, h.stderr)
	}
	if strings.Join(f.cancelled, ",") != "run_sleep,run_late" {
		t.Fatalf("cancelled = %v", f.cancelled)
	}
}

func TestRunsUsage(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	ts := httptest.NewServer((&fakeRunsAPI{}).handler())
	t.Cleanup(ts.Close)
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"runs"}, ExitUsage, "usage: crux --server ADDR runs start"},
		{[]string{"runs", "help"}, ExitOK, "usage: crux --server ADDR runs start"},
		{[]string{"runs", "bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{[]string{"runs", "start"}, ExitUsage, "usage: crux --server ADDR runs start"},
		{[]string{"runs", "start", "-h"}, ExitOK, "-repo"},
		{[]string{"runs", "start", "-h"}, ExitOK, "-checkout"},
		{[]string{"runs", "start", "--bogus", "x"}, ExitUsage, "bogus"},
		{[]string{"runs", "status"}, ExitUsage, "usage: crux --server ADDR runs status"},
		{[]string{"runs", "status", "-bogus"}, ExitUsage, "bogus"},
		{[]string{"runs", "cancel", "a", "b"}, ExitUsage, "usage: crux --server ADDR runs cancel"},
		{[]string{"runs", "cancel", "-bogus"}, ExitUsage, "bogus"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", ts.URL}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d %s", tc.args, code, h.stderr)
		}
	}
}

func TestOpenRunsReconcilesAndLoadsTheBackend(t *testing.T) {
	t.Setenv(runsConfigEnvVar, "")
	h := newHarness(t)
	workDir := t.TempDir()
	store, err := runstate.Open(workDir)
	if err != nil {
		t.Fatal(err)
	}
	stale, _, err := store.Create(runstate.Record{})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := h.env.openRuns(workDir, "")
	if err != nil {
		t.Fatal(err)
	}
	if svc.Configured() {
		t.Fatal("configured without a config file")
	}
	if run, err := svc.Get(stale.ID); err != nil || run.Status != api.RunUnfinished {
		t.Fatalf("stale record = %+v, %v", run, err)
	}
	if !strings.Contains(h.stderr.String(), stale.ID+" was in flight") {
		t.Fatalf("reconcile not reported: %s", h.stderr)
	}

	cfgPath := writeFile(t, filepath.Join(t.TempDir(), "runs.json"), `{"image":"img","workspace_roots":["/srv"]}`)
	svc, err = h.env.openRuns(workDir, cfgPath)
	if err != nil || !svc.Configured() || svc.Backend.Name != runs.BackendPodman {
		t.Fatalf("configured = %v, %v", svc, err)
	}
	t.Setenv(runsConfigEnvVar, cfgPath)
	if svc, err = h.env.openRuns(workDir, ""); err != nil || !svc.Configured() {
		t.Fatalf("env config = %v, %v", svc, err)
	}
	if _, err := h.env.openRuns(workDir, writeFile(t, filepath.Join(t.TempDir(), "bad.json"), `{"backend":"job","image":"img"}`)); err == nil {
		t.Fatal("bad config accepted")
	}
	if _, err := h.env.openRuns(writeFile(t, filepath.Join(t.TempDir(), "file"), ""), cfgPath); err == nil {
		t.Fatal("work dir under a file accepted")
	}
	if code := h.run("serve", "--runs-config", filepath.Join(t.TempDir(), "missing.json")); code != ExitUsage || !strings.Contains(h.stderr.String(), "crux: runs:") {
		t.Fatalf("serve with a missing runs config: %d %s", code, h.stderr)
	}
}
