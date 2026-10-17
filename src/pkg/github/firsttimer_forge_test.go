package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/firsttimer"
)

func TestFirstTimerForge(t *testing.T) {
	var searchQueries []string
	var created []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/search/issues":
			searchQueries = append(searchQueries, r.URL.Query().Get("q"))
			if r.URL.Query().Get("per_page") != "10" || r.URL.Query().Get("sort") != "created" || r.URL.Query().Get("order") != "asc" {
				http.Error(w, "unexpected search options: "+r.URL.RawQuery, http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"total_count": 2,
				"items":       []map[string]any{{"number": 3}, {"number": 8}, {"number": 0}},
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/8/comments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 1, "body": "unrelated"}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/3/comments"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 2, "body": firsttimer.Marker + "\nhi"}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/8/comments"):
			b, _ := io.ReadAll(r.Body)
			created = append(created, string(b))
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 9})
		default:
			http.Error(w, r.Method+" "+r.URL.Path, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widgets"})
	ctx := context.Background()
	forge := c.FirstTimerForge()

	nums, err := forge.ListAuthorPullRequests(ctx, "acme/widgets", "@newbie", 10)
	if err != nil || len(nums) != 2 || nums[0] != 3 || nums[1] != 8 {
		t.Fatalf("ListAuthorPullRequests = %v, %v", nums, err)
	}
	if len(searchQueries) != 1 || searchQueries[0] != "repo:acme/widgets is:pr author:newbie" {
		t.Fatalf("search queries = %q", searchQueries)
	}
	if _, err := forge.ListAuthorPullRequests(ctx, "widgets", " ", 10); err == nil {
		t.Fatal("empty author must error")
	}

	peek, err := forge.UpsertIssueMarkerComment(ctx, "widgets", 3, firsttimer.Marker, "", false)
	if err != nil || !peek.Found || !strings.Contains(peek.Previous, firsttimer.Marker) {
		t.Fatalf("peek on welcomed PR = %+v, %v", peek, err)
	}
	res, err := forge.UpsertIssueMarkerComment(ctx, "widgets", 8, firsttimer.Marker, firsttimer.Marker+"\nwelcome", true)
	if err != nil || !res.Created || len(created) != 1 || !strings.Contains(created[0], "welcome") {
		t.Fatalf("create = %+v, %v, created=%v", res, err, created)
	}

	// End to end through the sweeper: PR 8 by newbie, who was already
	// welcomed on PR 3 — nothing new is posted.
	before := len(created)
	out := firsttimer.NewSweeper().Sweep(ctx, forge, []firsttimer.Candidate{{
		Repo: "acme/widgets", Number: 8, Author: "newbie", AuthorAssociation: firsttimer.AssociationFirstTimeContributor,
	}}, firsttimer.SweepOptions{Links: firsttimer.Links{Origin: "https://h.example"}})
	if len(out.Welcomed) != 0 || out.Skipped[firsttimer.SkipAlreadyWelcomed] != 1 || len(created) != before {
		t.Fatalf("sweep through client forge: %+v created=%d", out, len(created))
	}

	var nilClient *Client
	if _, err := nilClient.FirstTimerForge().ListAuthorPullRequests(ctx, "widgets", "x", 1); err == nil {
		t.Fatal("nil client should error")
	}
	if _, err := nilClient.FirstTimerForge().UpsertIssueMarkerComment(ctx, "widgets", 1, firsttimer.Marker, "", false); err == nil {
		t.Fatal("nil client should error")
	}
}
