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

func TestGatePathsAndCodes(t *testing.T) {
	if PathGates != "/v1/gates" || GatePath("gate_a") != "/v1/gates/gate_a" ||
		GateAnswerPath("gate_a") != "/v1/gates/gate_a/answer" || GateWaitPath("gate_a") != "/v1/gates/gate_a/wait" {
		t.Fatalf("paths: %s %s %s %s", PathGates, GatePath("gate_a"), GateAnswerPath("gate_a"), GateWaitPath("gate_a"))
	}
	seen := map[string]bool{CodeRuntime: true, CodeBadRequest: true, CodeForbidden: true, CodeConflict: true, CodeTooLarge: true}
	for _, code := range []string{CodeGateContextTooLarge, CodeGateInvalidChoice, CodeGateSelfAnswer, CodeGateClosed} {
		if seen[code] {
			t.Fatalf("gate code %q overloads an existing code", code)
		}
		seen[code] = true
	}
}

// workServicesServer is the work-services half of the wire contract
// (checkouts, runs, gates): it records every request and answers canned
// records so the typed client methods are tested against the contract,
// not crux/serve.
type workServicesServer struct {
	mu       sync.Mutex
	requests []string
	bodies   []string
}

func (s *workServicesServer) handler() http.Handler {
	mux := http.NewServeMux()
	record := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			data, _ := io.ReadAll(r.Body)
			s.mu.Lock()
			s.requests = append(s.requests, r.Method+" "+r.URL.RequestURI())
			s.bodies = append(s.bodies, string(data))
			s.mu.Unlock()
			h(w, r)
		}
	}
	writeJSON := func(w http.ResponseWriter, status int, v any) {
		w.Header().Set("Content-Type", ContentTypeJSON)
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(v)
	}
	opened := time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)
	gate := func(id, status string) Gate {
		return Gate{ID: id, Status: status, Subject: "ship?", Choices: []string{"ship", "hold"}, Timeout: "0", OpenedBy: "atlas", OpenedByPrincipal: "project:atlas", OpenedAt: opened}
	}

	mux.HandleFunc("POST "+PathGates, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, gate("gate_1", GateWaiting))
	}))
	mux.HandleFunc("GET "+PathGates, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, GateList{Gates: []Gate{gate("gate_1", GateWaiting), gate("gate_2", GateExpired)}})
	}))
	mux.HandleFunc("GET "+PathGates+"/{id}", record(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == "gate_missing" {
			writeJSON(w, http.StatusNotFound, Error{Error: "gate gate_missing", Code: CodeNotFound})
			return
		}
		writeJSON(w, http.StatusOK, gate(r.PathValue("id"), GateWaiting))
	}))
	mux.HandleFunc("POST "+PathGates+"/{id}/answer", record(func(w http.ResponseWriter, r *http.Request) {
		var in GateAnswerRequest
		_ = json.NewDecoder(strings.NewReader(s.lastBody())).Decode(&in)
		if in.Answer == "sure" {
			writeJSON(w, http.StatusUnprocessableEntity, Error{Error: `"sure" is not one of ship, hold`, Code: CodeGateInvalidChoice})
			return
		}
		g := gate(r.PathValue("id"), GateAnswered)
		g.Answer, g.AnsweredBy, g.Note, g.OperatorOverride = in.Answer, "kelly", in.Note, in.AsOperator
		writeJSON(w, http.StatusOK, g)
	}))
	mux.HandleFunc("GET "+PathGates+"/{id}/wait", record(func(w http.ResponseWriter, r *http.Request) {
		g := gate(r.PathValue("id"), GateAnswered)
		g.Answer = "ship"
		writeJSON(w, http.StatusOK, g)
	}))

	mux.HandleFunc("POST "+PathCheckouts, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, Checkout{ID: "co_1", Repo: "https://github.com/org/name.git", Commit: strings.Repeat("a", 40), CreatedAt: opened})
	}))
	mux.HandleFunc("GET "+PathCheckouts, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, CheckoutList{Checkouts: []Checkout{{ID: "co_1"}}})
	}))
	mux.HandleFunc("GET "+PathCheckouts+"/{id}", record(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, Checkout{ID: r.PathValue("id")})
	}))
	mux.HandleFunc("DELETE "+PathCheckouts+"/{id}", record(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	mux.HandleFunc("POST "+PathRuns, record(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusCreated, ContainedRun{ID: "run_1", Status: "running", CreatedAt: opened})
	}))
	mux.HandleFunc("GET "+PathRuns+"/{id}", record(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ContainedRun{ID: r.PathValue("id"), Status: "succeeded"})
	}))
	mux.HandleFunc("POST "+PathRuns+"/{id}/cancel", record(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, ContainedRun{ID: r.PathValue("id"), Status: "cancelled"})
	}))
	return mux
}

func (s *workServicesServer) lastBody() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bodies[len(s.bodies)-1]
}

func (s *workServicesServer) seen() (requests, bodies []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...), append([]string(nil), s.bodies...)
}

func TestWorkServicesClientRoutesAndDecodes(t *testing.T) {
	s := &workServicesServer{}
	srv := httptest.NewServer(s.handler())
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	g, err := c.OpenGate(ctx, GateRequest{Subject: "ship?", Context: json.RawMessage(`{"pr":17}`), Choices: []string{"ship", "hold"}, Timeout: "1h", OpenedBy: "atlas", IdempotencyKey: "k1"})
	if err != nil || g.ID != "gate_1" || g.Status != GateWaiting || g.OpenedByPrincipal != "project:atlas" || !g.OpenedAt.Equal(time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("open %+v %v", g, err)
	}
	all, err := c.ListGates(ctx, "")
	if err != nil || len(all) != 2 || all[1].Status != GateExpired {
		t.Fatalf("list %+v %v", all, err)
	}
	if _, err := c.ListGates(ctx, GateWaiting); err != nil {
		t.Fatalf("list waiting %v", err)
	}
	if g, err = c.GetGate(ctx, "gate_1"); err != nil || g.ID != "gate_1" {
		t.Fatalf("get %+v %v", g, err)
	}
	var se *StatusError
	if _, err := c.GetGate(ctx, "gate_missing"); !errors.As(err, &se) || se.Status != http.StatusNotFound || se.Body.Code != CodeNotFound {
		t.Fatalf("missing gate: %v", err)
	}
	if _, err := c.AnswerGate(ctx, "gate_1", GateAnswerRequest{Answer: "sure"}); !errors.As(err, &se) || se.Status != http.StatusUnprocessableEntity || se.Body.Code != CodeGateInvalidChoice {
		t.Fatalf("off-menu answer: %v", err)
	}
	g, err = c.AnswerGate(ctx, "gate 1", GateAnswerRequest{Answer: "ship", Note: "green", AsOperator: true})
	if err != nil || g.Status != GateAnswered || g.Answer != "ship" || g.AnsweredBy != "kelly" || g.Note != "green" || !g.OperatorOverride {
		t.Fatalf("answer %+v %v", g, err)
	}
	if g, err = c.WaitGate(ctx, "gate_1", 0); err != nil || g.Answer != "ship" {
		t.Fatalf("wait %+v %v", g, err)
	}
	if g, err = c.WaitGate(ctx, "gate_1", 90*time.Second); err != nil || g.Status != GateAnswered {
		t.Fatalf("wait with window %+v %v", g, err)
	}

	co, err := c.CreateCheckout(ctx, CheckoutRequest{Repo: "https://github.com/org/name.git", Commit: strings.Repeat("a", 40), Owner: "nous-1"})
	if err != nil || co.ID != "co_1" {
		t.Fatalf("create checkout %+v %v", co, err)
	}
	if cos, err := c.ListCheckouts(ctx, "nous 1"); err != nil || len(cos) != 1 {
		t.Fatalf("list checkouts %+v %v", cos, err)
	}
	if cos, err := c.ListCheckouts(ctx, ""); err != nil || len(cos) != 1 {
		t.Fatalf("list all checkouts %+v %v", cos, err)
	}
	if co, err = c.GetCheckout(ctx, "co 1"); err != nil || co.ID != "co 1" {
		t.Fatalf("get checkout %+v %v", co, err)
	}
	if err := c.DeleteCheckout(ctx, "co_1"); err != nil {
		t.Fatalf("delete checkout %v", err)
	}

	run, err := c.CreateRun(ctx, ContainedRunRequest{Command: []string{"make", "test"}})
	if err != nil || run.ID != "run_1" || run.Status != "running" {
		t.Fatalf("create run %+v %v", run, err)
	}
	if run, err = c.GetRun(ctx, "run_1"); err != nil || run.Status != "succeeded" {
		t.Fatalf("get run %+v %v", run, err)
	}
	if run, err = c.CancelRun(ctx, "run_1"); err != nil || run.Status != "cancelled" {
		t.Fatalf("cancel run %+v %v", run, err)
	}

	requests, bodies := s.seen()
	wantRequests := []string{
		"POST /v1/gates",
		"GET /v1/gates",
		"GET /v1/gates?status=waiting",
		"GET /v1/gates/gate_1",
		"GET /v1/gates/gate_missing",
		"POST /v1/gates/gate_1/answer",
		"POST /v1/gates/gate%201/answer",
		"GET /v1/gates/gate_1/wait",
		"GET /v1/gates/gate_1/wait?timeout=1m30s",
		"POST /v1/checkouts",
		"GET /v1/checkouts?owner=nous+1",
		"GET /v1/checkouts",
		"GET /v1/checkouts/co%201",
		"DELETE /v1/checkouts/co_1",
		"POST /v1/runs",
		"GET /v1/runs/run_1",
		"POST /v1/runs/run_1/cancel",
	}
	if strings.Join(requests, "\n") != strings.Join(wantRequests, "\n") {
		t.Fatalf("requests:\n%s\nwant:\n%s", strings.Join(requests, "\n"), strings.Join(wantRequests, "\n"))
	}
	for i, want := range map[int]string{
		0:  `"subject":"ship?","context":{"pr":17},"choices":["ship","hold"],"timeout":"1h","opened_by":"atlas","idempotency_key":"k1"`,
		6:  `{"answer":"ship","note":"green","as_operator":true}`,
		9:  `"owner":"nous-1"`,
		14: `"command":["make","test"]`,
	} {
		if !strings.Contains(bodies[i], want) {
			t.Errorf("request %d %s: body %q lacks %q", i, requests[i], bodies[i], want)
		}
	}
	for _, i := range []int{1, 3, 7, 11, 13, 15} {
		if bodies[i] != "" {
			t.Errorf("request %d %s: unexpected body %q", i, requests[i], bodies[i])
		}
	}
}
