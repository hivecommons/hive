package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// fakeStepsAPI serves run_ok's steps and records each posted request.
func fakeStepsAPI(t *testing.T) (string, func() []api.StepRequest) {
	t.Helper()
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	fin := at.Add(time.Minute)
	steps := []api.Step{
		{Run: "run_ok", N: 1, Status: api.StepSucceeded, Backend: "claude", Model: "opus", CreatedAt: at, FinishedAt: &fin, Attempts: 2,
			Answer: json.RawMessage(`{"verdict":"pass"}`), Usage: api.StepUsage{InputTokens: 10, OutputTokens: 4}},
		{Run: "run_ok", N: 2, Status: api.StepSchemaMismatch, Backend: "process", CreatedAt: at, FinishedAt: &fin, Attempts: 1,
			Reason: "the answer did not match output_schema after 1 attempt(s)", Violations: []api.SchemaViolation{{Path: "$.verdict", Message: "required property is missing"}}},
	}
	var mu sync.Mutex
	var posted []api.StepRequest
	fail := func(w http.ResponseWriter, status int, code, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(api.Error{Error: msg, Code: code})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "run_ok" {
			fail(w, http.StatusNotFound, api.CodeNotFound, "run not found")
			return
		}
		_ = json.NewEncoder(w).Encode(api.StepList{Run: "run_ok", Steps: steps})
	})
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/steps/{n}", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "run_ok" || r.PathValue("n") != "1" {
			fail(w, http.StatusNotFound, api.CodeNotFound, "step not found")
			return
		}
		_ = json.NewEncoder(w).Encode(steps[0])
	})
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/steps", func(w http.ResponseWriter, r *http.Request) {
		var in api.StepRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		mu.Lock()
		posted = append(posted, in)
		mu.Unlock()
		switch {
		case r.PathValue("id") == "run_busy":
			fail(w, http.StatusConflict, api.CodeStepBusy, "the session is already running a step")
		case r.PathValue("id") != "run_ok":
			fail(w, http.StatusConflict, api.CodeNotSession, "not an open agent step session")
		case in.Backend == "process":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(steps[1])
		default:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(steps[0])
		}
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, func() []api.StepRequest {
		mu.Lock()
		defer mu.Unlock()
		return append([]api.StepRequest(nil), posted...)
	}
}

func TestStepIsRemoteOnly(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	h := newHarness(t)
	if code := h.run("step", "list", "run_ok"); code != ExitUsage || !strings.Contains(h.stderr.String(), "needs --server") {
		t.Fatalf("local step: %d %s", code, h.stderr)
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("step touched the local runtime: %v", h.rt.Calls())
	}
	h = newHarness(t)
	h.run("help")
	if !strings.Contains(h.stderr.String(), "step run|list|show <run>") {
		t.Fatalf("usage does not list step:\n%s", h.stderr)
	}
}

func TestStepRunPostsTheRequest(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, posted := fakeStepsAPI(t)
	schema := filepath.Join(t.TempDir(), "schema.json")
	if err := os.WriteFile(schema, []byte(`{"type":"object"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "step"}, args...)...)
	}

	h, code := remote("run", "run_ok", "--backend", "claude", "--model", "opus", "--prompt", "Judge it", "--schema", schema,
		"--tool", "Read", "--tool", "Grep", "--turn-limit", "3", "--retry", "1", "--timeout-ms", "5000")
	if code != ExitOK {
		t.Fatalf("run: %d %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{"step:     1", "status:   succeeded", "backend:  claude (opus)", "attempts: 2", "tokens:   10 in, 4 out", `answer:   {"verdict":"pass"}`, "finished: 2026-11-08T12:01:00.000Z"} {
		if !strings.Contains(out, want) {
			t.Errorf("run output lacks %q:\n%s", want, out)
		}
	}
	reqs := posted()
	if len(reqs) != 1 {
		t.Fatalf("posted %d", len(reqs))
	}
	got := reqs[0]
	if got.Backend != "claude" || got.Model != "opus" || got.Prompt != "Judge it" || string(got.OutputSchema) != `{"type":"object"}` ||
		strings.Join(got.Tools, ",") != "Read,Grep" || got.TurnLimit != 3 || got.RetryOnMismatch != 1 || got.TimeoutMs != 5000 || len(got.Command) != 0 {
		t.Fatalf("posted = %+v", got)
	}

	// The process backend takes its command after --; a step that did not
	// succeed exits non-zero and prints its violations.
	h, code = remote("run", "--backend", "process", "--prompt-file", "task.md", "--schema", schema, "run_ok", "--", "python3", "ask.py")
	if code != ExitError || !strings.Contains(h.stdout.String(), "status:   schema_mismatch") || !strings.Contains(h.stdout.String(), "violation: $.verdict: required property is missing") {
		t.Fatalf("process run: %d %s %s", code, h.stdout, h.stderr)
	}
	if got := posted()[1]; got.PromptPath != "task.md" || strings.Join(got.Command, " ") != "python3 ask.py" {
		t.Fatalf("process posted = %+v", got)
	}
	h, code = remote("run", "run_ok", "--backend", "claude", "--prompt", "p", "--schema", schema, "--json")
	var step api.Step
	if err := json.Unmarshal(h.stdout.Bytes(), &step); code != ExitOK || err != nil || step.N != 1 {
		t.Fatalf("run --json: %d %v %s", code, err, h.stdout)
	}

	// Refusals the caller can fix are usage errors.
	if h, code = remote("run", "run_ok", "--backend", "claude", "--prompt", "p"); code != ExitUsage {
		t.Fatalf("no schema: %d %s", code, h.stderr)
	}
	if h, code = remote("run", "run_ok", "--backend", "claude", "--prompt", "p", "--schema", filepath.Join(t.TempDir(), "nope.json")); code != ExitUsage || !strings.Contains(h.stderr.String(), "--schema") {
		t.Fatalf("missing schema file: %d %s", code, h.stderr)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(bad, []byte("{"), 0o600)
	if h, code = remote("run", "run_ok", "--backend", "claude", "--prompt", "p", "--schema", bad); code != ExitUsage || !strings.Contains(h.stderr.String(), "not valid JSON") {
		t.Fatalf("bad schema file: %d %s", code, h.stderr)
	}
	if h, code = remote("run", "run_busy", "--backend", "claude", "--prompt", "p", "--schema", schema); code != ExitUsage || !strings.Contains(h.stderr.String(), "already running") {
		t.Fatalf("busy: %d %s", code, h.stderr)
	}
	if h, code = remote("run", "run_exp", "--backend", "claude", "--prompt", "p", "--schema", schema); code != ExitUsage || !strings.Contains(h.stderr.String(), "not an open") {
		t.Fatalf("not a session: %d %s", code, h.stderr)
	}
}

func TestStepListAndShow(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, _ := fakeStepsAPI(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "step"}, args...)...)
	}
	h, code := remote("list", "run_ok")
	want := "1 2026-11-08T12:00:00.000Z claude succeeded (2 attempts)\n" +
		"2 2026-11-08T12:00:00.000Z process schema_mismatch  the answer did not match output_schema after 1 attempt(s)\n"
	if code != ExitOK || h.stdout.String() != want {
		t.Fatalf("list: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	h, code = remote("list", "--json", "run_ok")
	var got []api.Step
	if err := json.Unmarshal(h.stdout.Bytes(), &got); code != ExitOK || err != nil || len(got) != 2 {
		t.Fatalf("list --json: %d %v %s", code, err, h.stdout)
	}
	if h, code = remote("list", "run_missing"); code != ExitError || !strings.Contains(h.stderr.String(), "run not found") {
		t.Fatalf("missing run: %d %s", code, h.stderr)
	}
	if h, code = remote("list"); code != ExitUsage {
		t.Fatalf("list without a run: %d %s", code, h.stderr)
	}

	h, code = remote("show", "run_ok", "1")
	if code != ExitOK || !strings.Contains(h.stdout.String(), "step:     1\nrun:      run_ok\nstatus:   succeeded\n") {
		t.Fatalf("show: %d %q", code, h.stdout)
	}
	h, code = remote("show", "--json", "run_ok", "1")
	var step api.Step
	if err := json.Unmarshal(h.stdout.Bytes(), &step); code != ExitOK || err != nil || step.N != 1 {
		t.Fatalf("show --json: %d %v %s", code, err, h.stdout)
	}
	if h, code = remote("show", "run_ok", "2"); code != ExitError || !strings.Contains(h.stderr.String(), "step not found") {
		t.Fatalf("show missing: %d %s", code, h.stderr)
	}
	if h, code = remote("show", "run_ok", "x"); code != ExitUsage {
		t.Fatalf("show non-number: %d %s", code, h.stderr)
	}
	if h, code = remote("show", "run_ok"); code != ExitUsage {
		t.Fatalf("show without n: %d %s", code, h.stderr)
	}
	if h, code = remote("bogus"); code != ExitUsage || !strings.Contains(h.stderr.String(), "unknown subcommand") {
		t.Fatalf("bogus: %d %s", code, h.stderr)
	}
	if h, code = remote("help"); code != ExitOK || !strings.Contains(h.stderr.String(), "usage: crux --server ADDR step run") {
		t.Fatalf("help: %d %s", code, h.stderr)
	}
}
