package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/effects"
)

// A rejected advisory create must not fence the same claim after access is
// restored; an ambiguous failure must still fence it to prevent duplicates.
func TestEnsureAdvisoryIssue_CreateFailureJournal(t *testing.T) {
	for _, tc := range []struct {
		name     string
		status   int
		definite bool
	}{
		{"unauthorized", http.StatusUnauthorized, true},
		{"forbidden", http.StatusForbidden, true},
		{"private repository not selected", http.StatusNotFound, true},
		{"issues disabled", http.StatusGone, true},
		{"validation", http.StatusUnprocessableEntity, true},
		{"server error", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var creates int
			mux := http.NewServeMux()
			mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
			})
			mux.HandleFunc("/repos/o/r/issues", func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					json.NewEncoder(w).Encode([]any{})
					return
				}
				creates++
				if creates == 1 {
					w.WriteHeader(tc.status)
					fmt.Fprint(w, `{"message":"create failed"}`)
					return
				}
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"number":7,"html_url":"https://github.example/o/r/issues/7"}`)
			})
			mux.HandleFunc("/repos/o/r/labels", func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusCreated)
				fmt.Fprint(w, `{"name":"hive/advisory"}`)
			})
			mux.HandleFunc("/repos/o/r", func(w http.ResponseWriter, r *http.Request) {
				fmt.Fprint(w, `{"full_name":"o/r","has_issues":true}`)
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			c := newTestClient(t, server, "o", []string{"r"})
			c.SetMutationBoundary(replayTestBoundary(t))
			previousNow := issueCreateNow
			issueCreateNow = func() time.Time { return time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC) }
			t.Cleanup(func() { issueCreateNow = previousNow })

			_, err := c.EnsureAdvisoryIssue(context.Background(), "r")
			if err == nil || errors.Is(err, effects.ErrNotApplied) != tc.definite || githubStatusOf(err) != tc.status {
				t.Fatalf("first create: err=%v, want status %d and definite=%v", err, tc.status, tc.definite)
			}
			number, err := c.EnsureAdvisoryIssue(context.Background(), "r")
			if tc.definite {
				if err != nil || number != 7 || creates != 2 {
					t.Fatalf("retry after refusal: number=%d err=%v POSTs=%d, want 7, nil, 2", number, err, creates)
				}
			} else if !errors.Is(err, effects.ErrNeedsReconciliation) || creates != 1 {
				t.Fatalf("retry after ambiguous failure: number=%d err=%v POSTs=%d, want reconciliation and 1 POST", number, err, creates)
			}
		})
	}
}
