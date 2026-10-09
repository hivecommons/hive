package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestGraphQLPRBatchPaginationAndMergeStateMapping(t *testing.T) {
	restore := resetPRDetailCacheForTest(time.Now, 128)
	defer restore()

	states := []string{"BEHIND", "BLOCKED", "CLEAN", "DIRTY", "DRAFT", "HAS_HOOKS", "UNKNOWN", "UNSTABLE"}
	var gqlCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rate_limit" {
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4993,"reset":1791500400}}}`)
			return
		}
		if r.URL.Path != "/graphql" {
			t.Fatalf("unexpected REST call during batch prefetch: %s", r.URL.Path)
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatal(err)
		}
		call := gqlCalls.Add(1)
		nodes := states[:4]
		hasNext, cursor := true, "page-2"
		if call == 2 {
			nodes, hasNext, cursor = states[4:], false, ""
		}
		fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":%t,"endCursor":%q},"nodes":[`, hasNext, cursor)
		for i, state := range nodes {
			if i > 0 {
				fmt.Fprint(w, ",")
			}
			num := i + 1
			if call == 2 {
				num += 4
			}
			fmt.Fprint(w, gqlPRNodeJSON(num, state, "MERGEABLE", fmt.Sprintf("sha-%d", num), "completed", "success"))
		}
		fmt.Fprint(w, `]}},"rateLimit":{"cost":7,"remaining":4993,"resetAt":"2026-10-08T23:00:00Z"}}}`)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 4 })

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	if got := gqlCalls.Load(); got != 2 {
		t.Fatalf("GraphQL calls = %d, want 2", got)
	}
	if !hasPRBatchGraphQLConsumer() {
		t.Fatalf("REST accounting did not record hive:pr_batch /graphql consumer")
	}
	for i, state := range states {
		want := strings.ToLower(state)
		pr, ok := sharedPRDetailCache.getAny("org/repo", i+1, time.Hour)
		if want == "unknown" {
			if ok {
				t.Fatalf("UNKNOWN mergeable state should miss detail cache")
			}
			continue
		}
		if !ok || pr.GetMergeableState() != want {
			t.Fatalf("PR %d mergeable_state = %q ok=%v, want %q", i+1, pr.GetMergeableState(), ok, want)
		}
	}
	info, err := c.RateLimits(context.Background())
	if err == nil && info.GraphQLPRBatch.Pages != 2 {
		t.Fatalf("graphql_pr_batch pages = %d, want 2", info.GraphQLPRBatch.Pages)
	}
}

func hasPRBatchGraphQLConsumer() bool {
	for _, c := range RESTTopConsumers(1000) {
		if c.Caller == "hive:pr_batch" && c.Endpoint == "/graphql" {
			return true
		}
	}
	return false
}

func TestGraphQLPRBatchEnrichUsesCacheAndUnknownFallsBack(t *testing.T) {
	restore := resetPRDetailCacheForTest(time.Now, 128)
	defer restore()

	var pullGets, checkRuns atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s,%s]}},"rateLimit":{"cost":3,"remaining":4997,"resetAt":"2026-10-08T23:00:00Z"}}}`,
				gqlPRNodeJSON(1, "CLEAN", "MERGEABLE", "sha-1", "completed", "success"),
				gqlPRNodeJSON(2, "UNKNOWN", "UNKNOWN", "sha-2", "completed", "success"))
		case strings.HasSuffix(r.URL.Path, "/pulls/2"):
			pullGets.Add(1)
			fmt.Fprint(w, `{"number":2,"state":"open","updated_at":"2026-10-08T20:00:00Z","mergeable":false,"mergeable_state":"dirty","head":{"sha":"sha-2"}}`)
		case strings.Contains(r.URL.Path, "/check-runs"):
			checkRuns.Add(1)
			fmt.Fprint(w, `{"total_count":0,"check_runs":[]}`)
		case r.URL.Path == "/rate_limit":
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4997,"reset":1791500400}}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "sha-1", UpdatedAt: mustTime("2026-10-08T20:00:00Z")},
		{Repo: "org/repo", Number: 2, HeadSHA: "sha-2", UpdatedAt: mustTime("2026-10-08T20:00:00Z")},
	}
	c.EnrichCIStatus(context.Background(), prs)
	if got := pullGets.Load(); got != 1 {
		t.Fatalf("REST pull GETs = %d, want 1 for UNKNOWN only", got)
	}
	if got := checkRuns.Load(); got != 0 {
		t.Fatalf("REST check-runs = %d, want 0", got)
	}
	if prs[0].CIStatus != "success" || prs[1].CIStatus != "success" {
		t.Fatalf("CI statuses = %q/%q, want success/success", prs[0].CIStatus, prs[1].CIStatus)
	}
}

func TestGraphQLPRBatchDisabledAndErrorUseREST(t *testing.T) {
	for _, tt := range []struct {
		name     string
		disabled bool
		gqlError bool
	}{
		{name: "disabled", disabled: true},
		{name: "graphql error", gqlError: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			restore := resetPRDetailCacheForTest(time.Now, 128)
			defer restore()
			var pullGets, checkRuns atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.URL.Path == "/graphql":
					var body struct {
						Query string `json:"query"`
					}
					_ = json.NewDecoder(r.Body).Decode(&body)
					if !strings.Contains(body.Query, "pullRequests(states: OPEN") {
						fmt.Fprint(w, `{"data":{"repository":null,"rateLimit":{"cost":1,"remaining":4999,"resetAt":"2026-10-08T23:00:00Z"}}}`)
						return
					}
					if !tt.gqlError {
						t.Fatalf("GraphQL called while disabled")
					}
					fmt.Fprint(w, `{"errors":[{"message":"boom"}]}`)
				case strings.HasSuffix(r.URL.Path, "/pulls/1"):
					pullGets.Add(1)
					fmt.Fprint(w, `{"number":1,"state":"open","updated_at":"2026-10-08T20:00:00Z","mergeable":true,"mergeable_state":"clean","head":{"sha":"sha-1"}}`)
				case strings.Contains(r.URL.Path, "/check-runs"):
					checkRuns.Add(1)
					fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":1,"name":"unit","status":"completed","conclusion":"success"}]}`)
				default:
					t.Fatalf("unexpected path %s", r.URL.Path)
				}
			}))
			defer srv.Close()
			c := newTestClient(t, srv, "org", []string{"repo"})
			c.SetGraphQLPRBatchConfig(func() bool { return !tt.disabled }, func() int { return 50 })
			c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
			prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "sha-1", UpdatedAt: mustTime("2026-10-08T20:00:00Z")}}
			c.EnrichCIStatus(context.Background(), prs)
			if pullGets.Load() != 1 || checkRuns.Load() != 1 || prs[0].CIStatus != "success" {
				t.Fatalf("REST fallback pulls=%d checks=%d status=%q", pullGets.Load(), checkRuns.Load(), prs[0].CIStatus)
			}
		})
	}
}

func gqlPRNodeJSON(number int, mergeState, mergeable, sha, checkStatus, conclusion string) string {
	return fmt.Sprintf(`{"number":%d,"headRefOid":%q,"updatedAt":"2026-10-08T20:00:00Z","isDraft":false,"mergeable":%q,"mergeStateStatus":%q,"reviewDecision":"APPROVED","labels":{"nodes":[{"name":"bug"}]},"author":{"login":"bot"},"baseRefName":"v5","title":"PR %d","commits":{"nodes":[{"commit":{"statusCheckRollup":{"state":"SUCCESS","contexts":{"nodes":[{"__typename":"CheckRun","name":"unit","status":%q,"conclusion":%q,"detailsUrl":"https://ci.example/%d"}]}}}}]},"closingIssuesReferences":{"nodes":[{"number":111}]}}`, number, sha, mergeable, mergeState, number, checkStatus, conclusion, number)
}

func mustTime(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		panic(err)
	}
	return t
}
