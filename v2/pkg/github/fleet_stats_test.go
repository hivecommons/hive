package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// fleetSearchServer returns an httptest server that answers /search/issues with
// a total keyed by which qualifier substrings appear in the "q" query param, so
// each of the three fleet-stat searches can return a distinct count.
func fleetSearchServer(t *testing.T, byQualifier map[string]int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query().Get("q")
		total := 0
		for sub, n := range byQualifier {
			if strings.Contains(q, sub) {
				total = n
				break
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": total, "items": []any{}})
	})
	return httptest.NewServer(mux)
}

func TestSearchMergedPRCountSince(t *testing.T) {
	srv := fleetSearchServer(t, map[string]int{"is:merged merged:>=": 12})
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	got, err := c.SearchMergedPRCountSince(context.Background(), "bot", "org", time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 12 {
		t.Errorf("got %d, want 12", got)
	}
}

func TestSearchRejectedPRCountSince(t *testing.T) {
	srv := fleetSearchServer(t, map[string]int{"is:closed is:unmerged closed:>=": 4})
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	got, err := c.SearchRejectedPRCountSince(context.Background(), "bot", "org", time.Now().AddDate(0, 0, -90))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 4 {
		t.Errorf("got %d, want 4", got)
	}
}

func TestSearchCVEPRCount(t *testing.T) {
	srv := fleetSearchServer(t, map[string]int{`"CVE-"`: 7})
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	got, err := c.SearchCVEPRCount(context.Background(), "bot", "org")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != 7 {
		t.Errorf("got %d, want 7", got)
	}
}

func TestComputeFleetContribCounts(t *testing.T) {
	tests := []struct {
		name      string
		author    string
		org       string
		byQual    map[string]int
		nilClient bool
		want      FleetContribCounts
		wantErr   bool
	}{
		{
			name:   "all three counts",
			author: "bot",
			org:    "org",
			byQual: map[string]int{
				"is:merged merged:>=":             30,
				"is:closed is:unmerged closed:>=": 5,
				`"CVE-"`:                          9,
			},
			want: FleetContribCounts{PRsMerged: 30, PRsRejected: 5, CVEsClosed: 9},
		},
		{
			name:   "empty author yields zero, no error",
			author: "",
			org:    "org",
			want:   FleetContribCounts{},
		},
		{
			name:   "empty org yields zero, no error",
			author: "bot",
			org:    "",
			want:   FleetContribCounts{},
		},
		{
			name:      "nil client yields zero, no error",
			author:    "bot",
			org:       "org",
			nilClient: true,
			want:      FleetContribCounts{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c *Client
			if !tt.nilClient {
				srv := fleetSearchServer(t, tt.byQual)
				defer srv.Close()
				c = newTestClient(t, srv, tt.org, []string{"repo"})
			}
			got, err := c.ComputeFleetContribCounts(context.Background(), tt.author, tt.org)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

// failOnServer returns a search server that 500s only when the query contains
// failSub, and otherwise returns okTotal. Lets us exercise each of the three
// sequential error returns in ComputeFleetContribCounts independently.
func failOnServer(t *testing.T, failSub string, okTotal int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Query().Get("q"), failSub) {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": okTotal, "items": []any{}})
	})
	return httptest.NewServer(mux)
}

func TestComputeFleetContribCounts_SearchError(t *testing.T) {
	cases := []struct{ name, failSub string }{
		{"merged fails", "is:merged merged:>="},
		{"rejected fails", "is:unmerged"},
		{"cve fails", `"CVE-"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := failOnServer(t, tc.failSub, 1)
			defer srv.Close()
			c := newTestClient(t, srv, "org", []string{"repo"})
			_, err := c.ComputeFleetContribCounts(context.Background(), "bot", "org")
			if err == nil {
				t.Fatal("expected error on search failure")
			}
		})
	}
}
