package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRunPaths(t *testing.T) {
	if PathRuns != "/v1/runs" || RunPath("run_a") != "/v1/runs/run_a" || RunCancelPath("run_a") != "/v1/runs/run_a/cancel" {
		t.Fatalf("paths: %s %s %s", PathRuns, RunPath("run_a"), RunCancelPath("run_a"))
	}
	if CodeUnsupported == CodeNotSupported {
		t.Fatal("unsupported-limit refusals must be distinguishable from a not-configured server")
	}
}

func TestSavedRunPathsAndQuery(t *testing.T) {
	if RunRestartPath("run_a") != "/v1/runs/run_a/restart" || RunHistoryPath("run_a") != "/v1/runs/run_a/history" {
		t.Fatalf("paths: %s %s", RunRestartPath("run_a"), RunHistoryPath("run_a"))
	}
	for _, tc := range []struct {
		status string
		limit  int
		want   string
	}{
		{"", 0, "/v1/runs"},
		{"", -1, "/v1/runs"},
		{RunUnfinished, 0, "/v1/runs?status=unfinished"},
		{"", 7, "/v1/runs?limit=7"},
		{"a b", 2, "/v1/runs?limit=2&status=a+b"},
	} {
		if got := RunsListQuery(tc.status, tc.limit); got != tc.want {
			t.Fatalf("RunsListQuery(%q, %d) = %q want %q", tc.status, tc.limit, got, tc.want)
		}
	}
}

func TestSavedRunClientRoutesAndDecodes(t *testing.T) {
	var seen []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+PathRuns, func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		_ = json.NewEncoder(w).Encode(ContainedRunList{Runs: []ContainedRun{{ID: "run_1", Status: RunUnfinished}}})
	})
	mux.HandleFunc("POST "+PathRuns+"/{id}/restart", func(w http.ResponseWriter, r *http.Request) {
		var req RunRestartRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		seen = append(seen, r.Method+" "+r.URL.RequestURI()+" "+req.Reason)
		if r.PathValue("id") == "run_live" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(Error{Error: "run run_live is running", Code: CodeConflict})
			return
		}
		_ = json.NewEncoder(w).Encode(ContainedRun{ID: "run_2", Status: RunAccepted, RestartedFrom: r.PathValue("id")})
	})
	mux.HandleFunc("GET "+PathRuns+"/{id}/history", func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.RequestURI())
		_ = json.NewEncoder(w).Encode(RunHistory{Run: r.PathValue("id"), Transitions: []RunTransition{{Status: RunAccepted}, {Status: RunRunning}}})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c, err := NewClient(srv.URL, "tok")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	list, err := c.ListRuns(ctx, RunUnfinished, 5)
	if err != nil || len(list.Runs) != 1 || list.Runs[0].ID != "run_1" {
		t.Fatalf("ListRuns = %+v, %v", list, err)
	}
	again, err := c.RestartRun(ctx, "run_1", RunRestartRequest{Reason: "flaky"})
	if err != nil || again.ID != "run_2" || again.RestartedFrom != "run_1" {
		t.Fatalf("RestartRun = %+v, %v", again, err)
	}
	var se *StatusError
	if _, err := c.RestartRun(ctx, "run_live", RunRestartRequest{}); !errors.As(err, &se) || se.Status != http.StatusConflict || se.Body.Code != CodeConflict {
		t.Fatalf("live restart error = %v", err)
	}
	hist, err := c.RunHistory(ctx, "run_1")
	if err != nil || hist.Run != "run_1" || len(hist.Transitions) != 2 {
		t.Fatalf("RunHistory = %+v, %v", hist, err)
	}
	want := []string{"GET /v1/runs?limit=5&status=unfinished", "POST /v1/runs/run_1/restart flaky", "POST /v1/runs/run_live/restart ", "GET /v1/runs/run_1/history"}
	if strings.Join(seen, "|") != strings.Join(want, "|") {
		t.Fatalf("requests = %q", seen)
	}
}
