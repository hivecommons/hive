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

// fakeSignalsAPI serves run_ok's signals (three, in pages of at most two),
// accepts emits on it, and records each request's query and body.
func fakeSignalsAPI(t *testing.T) (string, func() (queries, bodies []string)) {
	t.Helper()
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	sigs := []api.RunSignal{
		{Seq: 1, Kind: api.SignalStarted, Source: api.SignalSourceRunner, At: at, Message: "sandbox launched"},
		{Seq: 2, Kind: api.SignalNeedsInput, Source: api.SignalSourceGate, At: at, Message: "approve?", Data: json.RawMessage(`{"gate":"gate_1"}`)},
		{Seq: 3, Kind: api.SignalDone, Source: api.SignalSourceRunner, At: at, Data: json.RawMessage(`{"status":"completed","exit_code":0}`)},
	}
	var mu sync.Mutex
	var queries, bodies []string
	fail := func(w http.ResponseWriter, status int, code, msg string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(api.Error{Error: msg, Code: code})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+api.PathRuns+"/{id}/signals", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		queries = append(queries, r.URL.RawQuery)
		mu.Unlock()
		if r.PathValue("id") != "run_ok" {
			fail(w, http.StatusNotFound, api.CodeNotFound, "run not found")
			return
		}
		since, _ := strconv.ParseInt(r.URL.Query().Get("since_seq"), 10, 64)
		limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
		if limit <= 0 || limit > 2 {
			limit = 2
		}
		page := api.RunSignalPage{Run: "run_ok", NextSeq: since, Signals: []api.RunSignal{}}
		for _, sig := range sigs {
			if sig.Seq <= since {
				continue
			}
			if len(page.Signals) == limit {
				page.More = true
				break
			}
			page.Signals = append(page.Signals, sig)
			page.NextSeq = sig.Seq
		}
		_ = json.NewEncoder(w).Encode(page)
	})
	mux.HandleFunc("POST "+api.PathRuns+"/{id}/signals", func(w http.ResponseWriter, r *http.Request) {
		var in api.RunSignalRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		raw, _ := json.Marshal(in)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		switch {
		case r.PathValue("id") == "run_done":
			fail(w, http.StatusConflict, api.CodeSignalsClosed, "the run's done signal is already recorded")
		case in.Kind == api.SignalDone:
			fail(w, http.StatusBadRequest, api.CodeBadRequest, "a caller may not append a done signal")
		default:
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.RunSignal{Seq: 4, Kind: in.Kind, Source: api.SignalSourceCaller, At: at, Message: in.Message, Data: in.Data})
		}
	})
	mux.HandleFunc("POST "+api.PathGates, func(w http.ResponseWriter, r *http.Request) {
		var in api.GateRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		raw, _ := json.Marshal(in)
		mu.Lock()
		bodies = append(bodies, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.Gate{ID: "gate_1", Status: api.GateWaiting, Subject: in.Subject, Run: in.Run})
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts.URL, func() ([]string, []string) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), queries...), append([]string(nil), bodies...)
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

func TestSignalListFollowsPages(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, seen := fakeSignalsAPI(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "signal"}, args...)...)
	}

	h, code := remote("list", "run_ok")
	want := "1 2026-11-08T12:00:00.000Z started/runner  sandbox launched\n" +
		"2 2026-11-08T12:00:00.000Z needs-input/gate  approve?  {\"gate\":\"gate_1\"}\n"
	if code != ExitOK || h.stdout.String() != want {
		t.Fatalf("list: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	// --all follows next_seq; --since starts later.
	h, code = remote("list", "--all", "--since", "1", "run_ok")
	lines := strings.Split(strings.TrimSpace(h.stdout.String()), "\n")
	if code != ExitOK || len(lines) != 2 || !strings.HasPrefix(lines[1], "3 ") || !strings.Contains(lines[1], "done/runner") {
		t.Fatalf("list --all: %d %q", code, h.stdout)
	}
	if q, _ := seen(); len(q) != 2 || q[1] != "since_seq=1" {
		t.Fatalf("queries = %q", q)
	}
	h, code = remote("list", "--json", "--limit", "1", "run_ok")
	var got []api.RunSignal
	if err := json.Unmarshal(h.stdout.Bytes(), &got); code != ExitOK || err != nil || len(got) != 1 || got[0].Kind != api.SignalStarted {
		t.Fatalf("list --json: %d %v %s", code, err, h.stdout)
	}
	if h, code = remote("list", "run_missing"); code != ExitError || !strings.Contains(h.stderr.String(), "run not found") {
		t.Fatalf("missing run: %d %s", code, h.stderr)
	}
}

func TestSignalEmit(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, seen := fakeSignalsAPI(t)
	remote := func(args ...string) (*harness, int) {
		h := newHarness(t)
		return h, h.run(append([]string{"--server", server, "signal"}, args...)...)
	}

	h, code := remote("emit", "run_ok", "--kind", "tool", "--message", "go test", "--data", `{"tool":"bash"}`)
	if code != ExitOK || h.stdout.String() != "4 2026-11-08T12:00:00.000Z tool/caller  go test  {\"tool\":\"bash\"}\n" {
		t.Fatalf("emit: %d stdout=%q stderr=%q", code, h.stdout, h.stderr)
	}
	if _, bodies := seen(); len(bodies) != 1 || bodies[0] != `{"kind":"tool","message":"go test","data":{"tool":"bash"}}` {
		t.Fatalf("bodies = %q", bodies)
	}
	h, code = remote("emit", "--kind", "thinking", "--json", "run_ok")
	var sig api.RunSignal
	if err := json.Unmarshal(h.stdout.Bytes(), &sig); code != ExitOK || err != nil || sig.Kind != api.SignalThinking {
		t.Fatalf("emit --json: %d %v %s", code, err, h.stdout)
	}

	for _, tc := range []struct {
		args []string
		code int
		msg  string
	}{
		{[]string{"emit", "run_ok"}, ExitUsage, "usage: crux --server ADDR signal emit"},
		{[]string{"emit", "--kind", "tool"}, ExitUsage, "usage: crux --server ADDR signal emit"},
		{[]string{"emit", "run_ok", "--kind", "tool", "--data", "{"}, ExitUsage, "--data is not valid JSON"},
		{[]string{"emit", "run_ok", "--kind", "done"}, ExitUsage, "may not append a done signal"},
		{[]string{"emit", "run_done", "--kind", "tool"}, ExitError, "already recorded"},
		{[]string{"list"}, ExitUsage, "usage: crux --server ADDR signal"},
		{[]string{"bogus"}, ExitUsage, `unknown subcommand "bogus"`},
		{nil, ExitUsage, "usage: crux --server ADDR signal"},
	} {
		h, code := remote(tc.args...)
		if code != tc.code || !strings.Contains(h.stderr.String(), tc.msg) {
			t.Errorf("%v: %d %q", tc.args, code, h.stderr)
		}
	}
	if h, code := remote("help"); code != ExitOK || !strings.Contains(h.stderr.String(), "A run's last signal is done") {
		t.Fatalf("help: %d %q", code, h.stderr)
	}
}

func TestGateOpenSendsTheRun(t *testing.T) {
	t.Setenv(config.ServerEnvVar, "")
	t.Setenv(config.ServeTokenEnvVar, "")
	server, seen := fakeSignalsAPI(t)
	h := newHarness(t)
	if code := h.run("--server", server, "gate", "open", "--subject", "approve?", "--run", "run_ok"); code != ExitOK || h.stdout.String() != "gate_1\twaiting\n" {
		t.Fatalf("gate open --run: %d %q %q", code, h.stdout, h.stderr)
	}
	if _, bodies := seen(); len(bodies) != 1 || !strings.Contains(bodies[0], `"run":"run_ok"`) {
		t.Fatalf("bodies = %q", bodies)
	}
}
