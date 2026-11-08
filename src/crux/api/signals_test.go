package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSignalsClientRoutesAndDecodes(t *testing.T) {
	at := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	var requests, bodies []string
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", ContentTypeJSON)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	record := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			mu.Lock()
			requests = append(requests, r.Method+" "+r.URL.RequestURI())
			bodies = append(bodies, string(data))
			mu.Unlock()
			h(w, r)
		}
	}
	mux.HandleFunc("GET "+PathRuns+"/{id}/signals", record(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, RunSignalPage{Run: r.PathValue("id"), NextSeq: 2, More: true, Signals: []RunSignal{
			{Seq: 1, Kind: SignalStarted, Source: SignalSourceRunner, At: at},
			{Seq: 2, Kind: SignalDone, Source: SignalSourceRunner, At: at, Data: json.RawMessage(`{"status":"failed","exit_code":2,"timed_out":true}`)},
		}})
	}))
	mux.HandleFunc("POST "+PathRuns+"/{id}/signals", record(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "run_done" {
			writeJSON(w, http.StatusConflict, Error{Error: "closed", Code: CodeSignalsClosed})
			return
		}
		writeJSON(w, http.StatusCreated, RunSignal{Seq: 3, Kind: SignalTool, Source: SignalSourceCaller, At: at})
	}))
	mux.HandleFunc("GET "+PathAgents+"/{name}/kick-outcome", record(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, AgentKickOutcome{Agent: r.PathValue("name"), LastKick: &at, SettledForLastKick: true,
			Outcome: &KickOutcome{Kind: "ended", At: at.Add(time.Minute), KickAt: at}})
	}))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	page, err := c.RunSignals(ctx, "run_1", 0, 0)
	var done SignalDoneData
	if err != nil || page.Run != "run_1" || len(page.Signals) != 2 || !page.More || json.Unmarshal(page.Signals[1].Data, &done) != nil ||
		done.Status != "failed" || done.ExitCode == nil || *done.ExitCode != 2 || !done.TimedOut {
		t.Fatalf("signals %+v %+v %v", page, done, err)
	}
	if _, err := c.RunSignals(ctx, "run_1", 2, 50); err != nil {
		t.Fatal(err)
	}
	sig, err := c.EmitRunSignal(ctx, "run_1", RunSignalRequest{Kind: SignalTool, Message: "go test", Data: json.RawMessage(`{"n":1}`)})
	if err != nil || sig.Seq != 3 || sig.Source != SignalSourceCaller {
		t.Fatalf("emit %+v %v", sig, err)
	}
	var se *StatusError
	if _, err := c.EmitRunSignal(ctx, "run_done", RunSignalRequest{Kind: SignalTool}); !errors.As(err, &se) || se.Status != http.StatusConflict || se.Body.Code != CodeSignalsClosed {
		t.Fatalf("closed: %v", err)
	}
	ko, err := c.AgentKickOutcome(ctx, "alpha")
	if err != nil || ko.Agent != "alpha" || ko.Outcome == nil || !ko.Outcome.KickAt.Equal(at) || !ko.SettledForLastKick {
		t.Fatalf("kick outcome %+v %v", ko, err)
	}

	mu.Lock()
	defer mu.Unlock()
	want := []string{
		"GET /v1/runs/run_1/signals",
		"GET /v1/runs/run_1/signals?limit=50&since_seq=2",
		"POST /v1/runs/run_1/signals",
		"POST /v1/runs/run_done/signals",
		"GET /v1/agents/alpha/kick-outcome",
	}
	if strings.Join(requests, "\n") != strings.Join(want, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(want, "\n"))
	}
	if bodies[2] != `{"kind":"tool","message":"go test","data":{"n":1}}` {
		t.Fatalf("emit body %q", bodies[2])
	}
}

func TestRunSignalEventCarriesTheRunBesideTheSignal(t *testing.T) {
	raw, err := json.Marshal(RunSignalEvent{Run: "run_1", RunSignal: RunSignal{Seq: 4, Kind: SignalBlocked, Source: SignalSourceGate, At: time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)}})
	if err != nil || string(raw) != `{"run":"run_1","seq":4,"kind":"blocked","source":"gate","at":"2026-11-08T12:00:00Z"}` {
		t.Fatalf("event = %s, %v", raw, err)
	}
}
