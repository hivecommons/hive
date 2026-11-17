package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestBaseRedMemory(t *testing.T) {
	now := time.Date(2026, 11, 17, 19, 0, 0, 0, time.UTC)
	m := &baseRedMemory{now: func() time.Time { return now }}
	key := baseRedMemoryKey("Acme", "Widget", "main")
	names := func(red map[string]bool) []string {
		var out []string
		for _, n := range []string{"E2E", "lint", "unit"} {
			if red[n] {
				out = append(out, n)
			}
		}
		return out
	}

	if got := m.remembered(key); got != nil {
		t.Fatalf("nothing read yet: %v, want nil (today's behaviour)", got)
	}
	if got := names(m.observe(key, map[string]baseCheckState{"E2E": baseCheckRed, "unit": baseCheckGreen})); !reflect.DeepEqual(got, []string{"E2E"}) {
		t.Fatalf("first read = %v", got)
	}
	// The base tip moved: its E2E run is in progress, and lint has no run yet.
	now = now.Add(5 * time.Minute)
	if got := names(m.observe(key, map[string]baseCheckState{"E2E": baseCheckRunning, "unit": baseCheckGreen})); !reflect.DeepEqual(got, []string{"E2E"}) {
		t.Fatalf("tip run in progress: %v, want E2E still red", got)
	}
	if got := names(m.observe(key, map[string]baseCheckState{})); !reflect.DeepEqual(got, []string{"E2E"}) {
		t.Fatalf("tip with no run of E2E yet: %v, want E2E still red", got)
	}
	// A failed listing reuses the set.
	if got := names(m.remembered(key)); !reflect.DeepEqual(got, []string{"E2E"}) {
		t.Fatalf("lookup error: %v, want the remembered E2E", got)
	}
	if got := m.snapshot(); !reflect.DeepEqual(got, map[string][]string{"acme/widget@main": {"E2E"}}) {
		t.Fatalf("snapshot = %v", got)
	}
	if got := BaseRedRememberedKey("acme/widget", "main"); got != key {
		t.Fatalf("BaseRedRememberedKey = %q, want %q", got, key)
	}
	// Only a completed non-red run of the check drops it.
	if got := m.observe(key, map[string]baseCheckState{"E2E": baseCheckGreen}); got != nil {
		t.Fatalf("tip completed green: %v, want nothing red", got)
	}
	if got := m.remembered(key); got != nil {
		t.Fatalf("after green: %v, want nil", got)
	}

	// The TTL bounds a check nobody sees red again.
	m.observe(key, map[string]baseCheckState{"lint": baseCheckRed})
	now = now.Add(baseRedMemoryTTL - time.Minute)
	if got := names(m.remembered(key)); !reflect.DeepEqual(got, []string{"lint"}) {
		t.Fatalf("inside the TTL: %v", got)
	}
	now = now.Add(time.Minute)
	if got := m.remembered(key); got != nil {
		t.Fatalf("TTL expired: %v, want nil", got)
	}
	if got := m.snapshot(); len(got) != 0 {
		t.Fatalf("snapshot after expiry = %v, want empty", got)
	}
}

// projectbluefin/common#1684: main's E2E check stayed red all afternoon,
// but a pass that read the tip while its run was in progress, or could not
// list the runs, called the PR's E2E red its own. The base's red now holds
// until the tip's run of the check completes green.
func TestEnrichBaseBranchRedRemembersAcrossPasses(t *testing.T) {
	baseRuns := []string{
		`{"total_count":2,"check_runs":[{"id":1,"name":"E2E","status":"completed","conclusion":"failure"},{"id":2,"name":"unit","status":"completed","conclusion":"success"}]}`,
		`{"total_count":2,"check_runs":[{"id":3,"name":"E2E","status":"in_progress"},{"id":4,"name":"unit","status":"completed","conclusion":"success"}]}`,
		"", // the listing fails
		`{"total_count":2,"check_runs":[{"id":5,"name":"E2E","status":"completed","conclusion":"success"},{"id":6,"name":"unit","status":"completed","conclusion":"success"}]}`,
	}
	pass := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/commits/main/check-runs", func(w http.ResponseWriter, r *http.Request) {
		body := baseRuns[pass]
		if body == "" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/1684/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{"filename": "system_files/shared/usr/bin/ujust"}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})

	for i, want := range [][]string{{"E2E"}, {"E2E"}, {"E2E"}, nil} {
		pass = i
		prs := []PullRequest{{Repo: "acme/widget", Number: 1684, BaseRef: "main", CIStatus: "failure", FailingChecks: []string{"E2E"}}}
		c.enrichBaseBranchRed(context.Background(), prs)
		if !reflect.DeepEqual(prs[0].BaseRedChecks, want) {
			t.Fatalf("pass %d: BaseRedChecks = %v, want %v", i, prs[0].BaseRedChecks, want)
		}
	}
	if got := c.BaseRedSnapshot(); len(got) != 0 {
		t.Fatalf("snapshot after the base went green = %v, want empty", got)
	}
}

func TestBaseCheckStatesForRefPrecedence(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/commits/main/check-runs", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"total_count":5,"check_runs":[
			{"id":1,"name":"E2E","status":"completed","conclusion":"success","app":{"id":1}},
			{"id":2,"name":"E2E","status":"in_progress","app":{"id":2}},
			{"id":3,"name":"unit","status":"completed","conclusion":"failure","app":{"id":1}},
			{"id":4,"name":"unit","status":"in_progress","app":{"id":2}},
			{"id":5,"name":"lint","status":"completed","conclusion":"success"}]}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	got, err := c.baseCheckStatesForRef(context.Background(), "acme", "widget", "main")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]baseCheckState{"E2E": baseCheckRunning, "unit": baseCheckRed, "lint": baseCheckGreen}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("states = %v, want %v (red > running > green across apps)", got, want)
	}
}
