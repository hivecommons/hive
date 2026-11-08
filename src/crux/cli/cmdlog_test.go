package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/config"
)

// fakeCmdlogAPI serves one run's command log, five records, in pages of
// at most two, the way crux serve pages it.
func fakeCmdlogAPI(t *testing.T) (string, func() []string) {
	t.Helper()
	exit := func(v int) *int { return &v }
	ms := int64(1200)
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	end := at.Add(time.Duration(ms) * time.Millisecond)
	var records []api.CommandRecord
	for i := 1; i <= 5; i++ {
		rec := api.CommandRecord{Seq: int64(i), Command: []string{"make", "step" + strconv.Itoa(i)}, Cwd: "/workspace", Capture: api.CommandCaptureRunner,
			Status: api.CommandCompleted, StartedAt: at, EndedAt: &end, DurationMs: &ms, ExitCode: exit(0),
			Stdout: &api.CommandOutput{Bytes: 12, Head: "head", Tail: "tail", Truncated: true}, Stderr: &api.CommandOutput{Bytes: 0}}
		if i == 3 {
			rec.Status, rec.ExitCode, rec.Reason = api.CommandFailed, exit(2), "exit status 2"
		}
		records = append(records, rec)
	}
	var mu sync.Mutex
	var queries []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/cmdlog", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		if r.PathValue("id") != "run_ok" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found", Code: api.CodeNotFound})
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_seq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2 {
			limit = 2
		}
		page := api.CommandPage{Run: "run_ok", NextSeq: since, Commands: []api.CommandRecord{}}
		for _, rec := range records {
			if rec.Seq <= since {
				continue
			}
			if len(page.Commands) == limit {
				page.More = true
				break
			}
			page.Commands = append(page.Commands, rec)
			page.NextSeq = rec.Seq
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...)
	}
}

func TestCmdlogIsRemoteOnly(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	h := newHarness(t)
	if code := h.run("cmdlog", "list", "run_ok"); code != ExitUsage || !strings.Contains(h.stderr.String(), "needs --server") {
		t.Fatalf("local cmdlog: %d %s", code, h.stderr)
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("cmdlog touched the local runtime: %v", h.rt.Calls())
	}
	h = newHarness(t)
	h.run("help")
	if !strings.Contains(h.stderr.String(), "cmdlog list|show <run>") {
		t.Fatalf("usage does not list cmdlog:\n%s", h.stderr)
	}
}

func TestCmdlogListAndShow(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, queries := fakeCmdlogAPI(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "cmdlog"}, args...)...)
	}

	// One page: the server's size.
	h, code := remote("list", "run_ok")
	if code != ExitOK || h.stdout.String() != "1 completed exit=0 1200ms  make step1\n2 completed exit=0 1200ms  make step2\n" {
		t.Fatalf("list: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	// --all follows next_seq; --since starts later.
	h, code = remote("list", "--all", "--since", "1", "run_ok")
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if code != ExitOK || len(lines) != 4 || !strings.HasPrefix(lines[0], "2 completed") || !strings.HasPrefix(lines[1], "3 failed exit=2") || !strings.HasPrefix(lines[3], "5 completed") {
		t.Fatalf("list --all: %d %q", code, h.stdout)
	}
	if q := queries(); len(q) != 3 || q[1] != "since_seq=1" || q[2] != "since_seq=3" {
		t.Fatalf("queries = %q", q)
	}
	h, code = remote("list", "--json", "--limit", "1", "run_ok")
	var recs []api.CommandRecord
	if err := json.Unmarshal(h.stdout.Bytes(), &recs); code != ExitOK || err != nil || len(recs) != 1 || recs[0].Seq != 1 {
		t.Fatalf("list --json: %d %v %s", code, err, h.stdout)
	}

	h, code = remote("show", "run_ok", "3")
	out := h.stdout.String()
	for _, want := range []string{"3 failed exit=2 1200ms  make step3", "cwd: /workspace", "capture: runner", "started: 2026-11-08T12:00:00.000Z", "ended:   2026-11-08T12:00:01.200Z", "reason: exit status 2", "--- stdout (12 bytes, middle omitted)", "head\n[…]\ntail\n", "--- stderr (0 bytes)"} {
		if !strings.Contains(out, want) {
			t.Fatalf("show lacks %q:\n%s", want, out)
		}
	}
	if code != ExitOK {
		t.Fatalf("show: %d %s", code, h.stderr)
	}
	h, code = remote("show", "--json", "run_ok", "5")
	var rec api.CommandRecord
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); code != ExitOK || err != nil || rec.Seq != 5 {
		t.Fatalf("show --json: %d %v %s", code, err, h.stdout)
	}
	if h, code := remote("show", "run_ok", "9"); code != ExitError || !strings.Contains(h.stderr.String(), "has no command 9") {
		t.Fatalf("show missing seq: %d %s", code, h.stderr)
	}
	if h, code := remote("list", "run_gone"); code != ExitError || !strings.Contains(h.stderr.String(), "run not found") {
		t.Fatalf("list unknown run: %d %s", code, h.stderr)
	}
}

func TestCmdlogUsage(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, _ := fakeCmdlogAPI(t)
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"cmdlog"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "help"}, ExitOK, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{[]string{"cmdlog", "list"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "list", "--nope", "run_ok"}, ExitUsage, "flag provided but not defined"},
		{[]string{"cmdlog", "show", "run_ok"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "show", "run_ok", "zero"}, ExitUsage, "seq must be a positive integer"},
		{[]string{"cmdlog", "show", "run_ok", "0"}, ExitUsage, "seq must be a positive integer"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", server}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d stderr=%q", tc.args, code, h.stderr)
		}
	}
}
