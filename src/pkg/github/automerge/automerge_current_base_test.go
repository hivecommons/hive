package automerge

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCurrentBaseGate(t *testing.T) {
	const head = "sha-head"
	cases := []struct {
		name          string
		optIn         bool
		behindBy      int
		compareStatus int
		updateAllowed bool
		wantStop      bool
		wantReason    string
		wantErr       bool
		wantCompares  int
		wantUpdates   int
		wantUpdated   bool
	}{
		{name: "off by default", optIn: false, behindBy: 3, updateAllowed: true},
		{name: "up to date merges", optIn: true, behindBy: 0, updateAllowed: true, wantCompares: 1},
		{name: "behind updates branch", optIn: true, behindBy: 2, updateAllowed: true, wantStop: true, wantReason: "check-pending", wantCompares: 1, wantUpdates: 1, wantUpdated: true},
		{name: "behind without update budget holds", optIn: true, behindBy: 2, updateAllowed: false, wantStop: true, wantReason: "behind", wantCompares: 1},
		{name: "compare failure fails closed", optIn: true, compareStatus: http.StatusInternalServerError, updateAllowed: true, wantStop: true, wantReason: autoMergeReasonCurrentBaseCheck, wantErr: true, wantCompares: 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var mu sync.Mutex
			compares, updates := 0, 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/compare/main..."+head:
					compares++
					if tc.compareStatus != 0 {
						w.WriteHeader(tc.compareStatus)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"behind_by": tc.behindBy, "ahead_by": 1})
				case r.Method == http.MethodPut && r.URL.Path == "/repos/acme/widget/pulls/7/update-branch":
					updates++
					var body map[string]any
					_ = json.NewDecoder(r.Body).Decode(&body)
					if got, _ := body["expected_head_sha"].(string); got != head {
						t.Errorf("update-branch expected_head_sha = %q, want %q", got, head)
					}
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"message":"Updating pull request branch."}`))
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := newAutoMergeSweepClient(srv.URL)
			c.requireCurrentBase = func(repo string) bool {
				return tc.optIn && strings.EqualFold(repo, "acme/widget")
			}
			event, reason, stop, err := c.currentBaseGate(context.Background(), "acme/widget", "acme", "widget", 7, "main", head, tc.updateAllowed, "behind")
			if stop != tc.wantStop || reason != tc.wantReason {
				t.Fatalf("stop, reason = %v, %q; want %v, %q", stop, reason, tc.wantStop, tc.wantReason)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if event.BranchUpdated != tc.wantUpdated {
				t.Fatalf("BranchUpdated = %v, want %v", event.BranchUpdated, tc.wantUpdated)
			}
			mu.Lock()
			defer mu.Unlock()
			if compares != tc.wantCompares || updates != tc.wantUpdates {
				t.Fatalf("compares, updates = %d, %d; want %d, %d", compares, updates, tc.wantCompares, tc.wantUpdates)
			}
		})
	}
}

func TestCurrentBaseGateNilEngine(t *testing.T) {
	var c *Engine
	if c.requireCurrentBaseFor("acme/widget") {
		t.Fatal("nil engine must not require a current base")
	}
}
