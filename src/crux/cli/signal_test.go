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

// fakeSignalAPI serves one run's signals (started, three caller signals,
// done) in pages of at most two, and records emits.
func fakeSignalAPI(t *testing.T) (string, func() []string, func() []api.SignalRequest) {
	t.Helper()
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	records := []api.Signal{
		{Seq: 1, Kind: api.SignalStarted, Source: api.SignalSourceRunner, At: at},
		{Seq: 2, Kind: api.SignalThinking, Source: api.SignalSourceCaller, By: "project:atlas", At: at.Add(time.Second), Message: "reading the issue"},
		{Seq: 3, Kind: api.SignalNeedsInput, Source: api.SignalSourceGate, At: at.Add(2 * time.Second), Message: "spend $40?"},
		{Seq: 4, Kind: api.SignalInput, Source: api.SignalSourceGate, At: at.Add(3 * time.Second), Message: "approve"},
		{Seq: 5, Kind: api.SignalDone, Source: api.SignalSourceRunner, At: at.Add(4 * time.Second)},
	}
	var mu sync.Mutex
	var queries []string
	var emits []api.SignalRequest
	mux := http.NewServeMux()
	notFound := func(w http.ResponseWriter) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(api.Error{Error: "run not found", Code: api.CodeNotFound})
	}
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/signals", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		if r.PathValue("id") != "run_ok" {
			notFound(w)
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_seq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2 {
			limit = 2
		}
		page := api.SignalPage{Run: "run_ok", NextSeq: since, Signals: []api.Signal{}}
		for _, rec := range records {
			if rec.Seq <= since {
				continue
			}
			if len(page.Signals) == limit {
				page.More = true
				break
			}
			page.Signals = append(page.Signals, rec)
			page.NextSeq = rec.Seq
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/signals", func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != "run_ok" {
			notFound(w)
			return
		}
		var req api.SignalRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		emits = append(emits, req)
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.Signal{Seq: 6, Kind: req.Kind, Source: api.SignalSourceCaller, By: "operator:kelly", At: at.Add(5 * time.Second), Message: req.Message, Data: req.Data})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, func() []string {
			mu.Lock()
			defer mu.Unlock()
			return append([]string(nil), queries...)
		}, func() []api.SignalRequest {
			mu.Lock()
			defer mu.Unlock()
			return append([]api.SignalRequest(nil), emits...)
		}
}

func TestSignalIsRemoteOnly(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	h := newHarness(t)
	if code := h.run("signal", "list", "run_ok"); code != ExitUsage || !strings.Contains(h.stderr.String(), "needs --server") {
		t.Fatalf("local signal: %d %s", code, h.stderr)
	}
	if len(h.rt.Calls()) != 0 {
		t.Fatalf("signal touched the local runtime: %v", h.rt.Calls())
	}
	h = newHarness(t)
	h.run("help")
	if !strings.Contains(h.stderr.String(), "signal emit|list <run>") {
		t.Fatalf("usage does not list signal:\n%s", h.stderr)
	}
}

func TestSignalListAndEmit(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, queries, emits := fakeSignalAPI(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "signal"}, args...)...)
	}

	h, code := remote("list", "run_ok")
	if code != ExitOK || h.stdout.String() != "1 2026-11-08T12:00:00.000Z runner started\n2 2026-11-08T12:00:01.000Z caller thinking  reading the issue\n" {
		t.Fatalf("list: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	h, code = remote("list", "--all", "--since", "1", "run_ok")
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if code != ExitOK || len(lines) != 4 || !strings.HasPrefix(lines[1], "3 ") || !strings.HasSuffix(lines[1], "gate needs-input  spend $40?") || !strings.HasSuffix(lines[3], "runner done") {
		t.Fatalf("list --all: %d %q", code, h.stdout)
	}
	if q := queries(); len(q) != 3 || q[1] != "since_seq=1" || q[2] != "since_seq=3" {
		t.Fatalf("queries = %q", q)
	}
	h, code = remote("list", "--json", "--limit", "1", "run_ok")
	var recs []api.Signal
	if err := json.Unmarshal(h.stdout.Bytes(), &recs); code != ExitOK || err != nil || len(recs) != 1 || recs[0].Kind != api.SignalStarted {
		t.Fatalf("list --json: %d %v %s", code, err, h.stdout)
	}

	h, code = remote("emit", "--message", "go test ./...", "--data", `{"pkg":"crux"}`, "run_ok", "tool")
	if code != ExitOK || h.stdout.String() != "6 2026-11-08T12:00:05.000Z caller tool  go test ./...\n" {
		t.Fatalf("emit: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	h, code = remote("emit", "--json", "run_ok", "blocked")
	var rec api.Signal
	if err := json.Unmarshal(h.stdout.Bytes(), &rec); code != ExitOK || err != nil || rec.Kind != api.SignalBlocked || rec.By != "operator:kelly" {
		t.Fatalf("emit --json: %d %v %s", code, err, h.stdout)
	}
	if e := emits(); len(e) != 2 || e[0].Kind != "tool" || e[0].Message != "go test ./..." || string(e[0].Data) != `{"pkg":"crux"}` || e[1].Data != nil {
		t.Fatalf("emits = %+v", e)
	}
	if h, code := remote("list", "run_gone"); code != ExitError || !strings.Contains(h.stderr.String(), "run not found") {
		t.Fatalf("list unknown run: %d %s", code, h.stderr)
	}
	if h, code := remote("emit", "run_gone", "tool"); code != ExitError || !strings.Contains(h.stderr.String(), "run not found") {
		t.Fatalf("emit unknown run: %d %s", code, h.stderr)
	}
}

func TestSignalUsage(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, _, emits := fakeSignalAPI(t)
	for _, tc := range []struct {
		args   []string
		code   int
		stderr string
	}{
		{[]string{"signal"}, ExitUsage, "usage: crux --server ADDR signal list"},
		{[]string{"signal", "help"}, ExitOK, "usage: crux --server ADDR signal list"},
		{[]string{"signal", "bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{[]string{"signal", "list"}, ExitUsage, "usage: crux --server ADDR signal list"},
		{[]string{"signal", "list", "--nope", "run_ok"}, ExitUsage, "flag provided but not defined"},
		{[]string{"signal", "emit", "run_ok"}, ExitUsage, "usage: crux --server ADDR signal list"},
		{[]string{"signal", "emit", "run_ok", "done"}, ExitUsage, "kind must be one of"},
		{[]string{"signal", "emit", "run_ok", "finished"}, ExitUsage, "kind must be one of"},
		{[]string{"signal", "emit", "--data", "{nope", "run_ok", "tool"}, ExitUsage, "--data is not valid JSON"},
	} {
		h := newHarness(t)
		if code := h.run(append([]string{"--server", server}, tc.args...)...); code != tc.code || !strings.Contains(h.stderr.String(), tc.stderr) {
			t.Fatalf("%v: %d stderr=%q", tc.args, code, h.stderr)
		}
	}
	if len(emits()) != 0 {
		t.Fatal("a refused emit reached the server")
	}
}
