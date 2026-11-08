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

// fakeCmdlogAPI serves the command log of one run, run_ok, paging two
// commands at a time unless the request says otherwise.
type fakeCmdlogAPI struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeCmdlogAPI) handler() http.Handler {
	started := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	ended := started.Add(1500 * time.Millisecond)
	recs := []api.CommandRecord{
		{Seq: 1, EndSeq: 2, Command: []string{"make", "test"}, Cwd: "/ws", Capture: api.CaptureRunner, Status: api.CommandCompleted,
			StartedAt: started, EndedAt: &ended, DurationMs: new(int64(1500)), ExitCode: new(0),
			Stdout: &api.CommandOutput{Bytes: 6, Head: "hello\n"}},
		{Seq: 3, EndSeq: 4, Command: []string{"sleep", "9"}, Cwd: "/ws", Capture: api.CaptureRunner, Status: api.CommandFailed, Reason: "timed out",
			StartedAt: started, EndedAt: &ended, DurationMs: new(int64(20)),
			Stdout: &api.CommandOutput{Bytes: 100, Head: "aaaa", Tail: "zz", Truncated: true}, Stderr: &api.CommandOutput{}},
		{Seq: 5, Command: []string{"tail", "-f"}, Capture: api.CaptureRunner, Status: api.CommandUnfinished, StartedAt: started},
	}
	latest := func(r api.CommandRecord) int64 { return max(r.Seq, r.EndSeq) }
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/cmdlog", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests = append(f.requests, r.URL.RequestURI())
		f.mu.Unlock()
		if r.PathValue("id") != "run_ok" {
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found", Code: api.CodeNotFound})
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_seq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit == 0 {
			limit = 2
		}
		page := api.CommandLog{Run: "run_ok", Commands: []api.CommandRecord{}, NextSeq: since}
		for _, rec := range recs {
			if latest(rec) <= since {
				continue
			}
			if len(page.Commands) == limit {
				page.More = true
				break
			}
			page.Commands = append(page.Commands, rec)
			page.NextSeq = latest(rec)
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	return mux
}

func (f *fakeCmdlogAPI) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := f.requests
	f.requests = nil
	return out
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

// tableRow returns the whitespace-split fields of the table line for seq.
func tableRow(t *testing.T, out string, seq string) []string {
	t.Helper()
	for _, line := range strings.Split(out, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[0] == seq {
			return fields
		}
	}
	t.Fatalf("no row for seq %s in:\n%s", seq, out)
	return nil
}

func TestCmdlogListAndShow(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	f := &fakeCmdlogAPI{}
	ts := httptest.NewServer(f.handler())
	t.Cleanup(ts.Close)
	server := ts.URL

	h := newHarness(t)
	if code := h.run("--server", server, "cmdlog", "list", "run_ok"); code != ExitOK {
		t.Fatalf("list: %d %s", code, h.stderr)
	}
	if got := strings.Join(tableRow(t, h.stdout.String(), "1"), " "); got != "1 completed 0 1.5s 6 - runner make test" {
		t.Fatalf("row 1 = %q", got)
	}
	if got := strings.Join(tableRow(t, h.stdout.String(), "3"), " "); got != "3 failed - 20ms 100 0 runner sleep 9" {
		t.Fatalf("row 3 = %q", got)
	}
	if strings.Contains(h.stdout.String(), "tail -f") || !strings.Contains(h.stderr.String(), "next page: --since-seq 4") {
		t.Fatalf("first page:\nstdout=%s\nstderr=%s", h.stdout, h.stderr)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "cmdlog", "list", "run_ok", "--since-seq", "4", "--limit", "5"); code != ExitOK {
		t.Fatalf("list next page: %d %s", code, h.stderr)
	}
	if got := strings.Join(tableRow(t, h.stdout.String(), "5"), " "); got != "5 unfinished - - - - runner tail -f" || strings.Contains(h.stderr.String(), "next page") {
		t.Fatalf("second page row = %q, stderr %s", got, h.stderr)
	}
	if got := f.seen(); strings.Join(got, ",") != "/v1/runs/run_ok/cmdlog,/v1/runs/run_ok/cmdlog?limit=5&since_seq=4" {
		t.Fatalf("list requests = %v", got)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "cmdlog", "list", "--json", "run_ok"); code != ExitOK {
		t.Fatalf("list json: %d %s", code, h.stderr)
	}
	var page api.CommandLog
	if err := json.Unmarshal(h.stdout.Bytes(), &page); err != nil || page.Run != "run_ok" || len(page.Commands) != 2 || !page.More || page.NextSeq != 4 {
		t.Fatalf("list json = %s (%v)", h.stdout, err)
	}
	f.seen()

	// show follows every page and prints each command in full.
	h = newHarness(t)
	if code := h.run("--server", server, "cmdlog", "show", "run_ok"); code != ExitOK {
		t.Fatalf("show: %d %s", code, h.stderr)
	}
	out := h.stdout.String()
	for _, want := range []string{
		"seq 1  completed  capture=runner\ncommand: make test\ncwd:     /ws\nstarted: 2026-11-08T12:00:00Z\nended:   2026-11-08T12:00:01.5Z (1.5s)\nexit:    0\nstdout:  6 bytes\n    hello\n",
		"seq 3  failed  capture=runner", "reason:  timed out", "stdout:  100 bytes\n    aaaa\n    [… 94 bytes not kept …]\n    zz\nstderr:  0 bytes\n",
		"seq 5  unfinished  capture=runner\ncommand: tail -f\nstarted: 2026-11-08T12:00:00Z\nexit:    -\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("show output lacks %q:\n%s", want, out)
		}
	}
	if got := f.seen(); strings.Join(got, ",") != "/v1/runs/run_ok/cmdlog,/v1/runs/run_ok/cmdlog?since_seq=4" {
		t.Fatalf("show requests = %v", got)
	}

	h = newHarness(t)
	if code := h.run("--server", server, "cmdlog", "show", "--seq", "3", "--json", "run_ok"); code != ExitOK {
		t.Fatalf("show one: %d %s", code, h.stderr)
	}
	var one []api.CommandRecord
	if err := json.Unmarshal(h.stdout.Bytes(), &one); err != nil || len(one) != 1 || one[0].Seq != 3 || one[0].Reason != "timed out" {
		t.Fatalf("show one json = %s (%v)", h.stdout, err)
	}

	for _, tc := range []struct {
		args   []string
		stderr string
	}{
		{[]string{"show", "--seq", "9", "run_ok"}, "no command with seq 9"},
		{[]string{"show", "run_missing"}, "run not found"},
		{[]string{"list", "run_missing"}, "run not found"},
	} {
		h = newHarness(t)
		if code := h.run(append([]string{"--server", server, "cmdlog"}, tc.args...)...); code != ExitError || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d %s", tc.args, code, h.stderr)
		}
	}
}

func TestCmdlogUsage(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	ts := httptest.NewServer((&fakeCmdlogAPI{}).handler())
	t.Cleanup(ts.Close)
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"cmdlog"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "help"}, ExitOK, "capture \"runner\""},
		{[]string{"cmdlog", "bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{[]string{"cmdlog", "list"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "list", "a", "b"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
		{[]string{"cmdlog", "list", "-h"}, ExitOK, "-since-seq"},
		{[]string{"cmdlog", "show", "-bogus"}, ExitUsage, "bogus"},
		{[]string{"cmdlog", "show"}, ExitUsage, "usage: crux --server ADDR cmdlog list"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", ts.URL}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d %s", tc.args, code, h.stderr)
		}
	}
}
