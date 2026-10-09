package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/config"
)

func TestStatusContextStateMapsToCheckRunStatusAndConclusion(t *testing.T) {
	for _, tt := range []struct {
		state          string
		wantStatus     string
		wantConclusion string
	}{
		{state: "SUCCESS", wantStatus: "completed", wantConclusion: "success"},
		{state: " success ", wantStatus: "completed", wantConclusion: "success"},
		{state: "FAILURE", wantStatus: "completed", wantConclusion: "failure"},
		{state: "ERROR", wantStatus: "completed", wantConclusion: "failure"},
		{state: "PENDING", wantStatus: "in_progress", wantConclusion: ""},
		{state: "EXPECTED", wantStatus: "in_progress", wantConclusion: ""},
		{state: "", wantStatus: "in_progress", wantConclusion: ""},
	} {
		t.Run(fmt.Sprintf("%q", tt.state), func(t *testing.T) {
			if got := statusFromStatusContext(tt.state); got != tt.wantStatus {
				t.Fatalf("statusFromStatusContext(%q) = %q, want %q", tt.state, got, tt.wantStatus)
			}
			if got := conclusionFromStatusContext(tt.state); got != tt.wantConclusion {
				t.Fatalf("conclusionFromStatusContext(%q) = %q, want %q", tt.state, got, tt.wantConclusion)
			}
		})
	}
}

func decodeGQLPRNode(t *testing.T, raw string) graphQLPRBatchNode {
	t.Helper()
	var n graphQLPRBatchNode
	if err := json.Unmarshal([]byte(raw), &n); err != nil {
		t.Fatalf("decode node: %v", err)
	}
	return n
}

func TestGraphQLCheckRunsMixesCheckRunsAndStatusContexts(t *testing.T) {
	n := decodeGQLPRNode(t, gqlPRNodeWithContextsJSON(1, "sha-1",
		`{"__typename":"CheckRun","name":"unit","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://ci.example/unit"},`+
			`{"__typename":"StatusContext","context":"DCO","state":"FAILURE","targetUrl":"https://probot.github.io/apps/dco/"},`+
			`{"__typename":"StatusContext","context":"ci/pending","state":"PENDING"},`+
			`{"__typename":"SomethingElse","name":"ignored","status":"COMPLETED","conclusion":"FAILURE"}`))

	checks, ok := graphQLCheckRuns(n)
	if !ok {
		t.Fatalf("graphQLCheckRuns ok=false with a statusCheckRollup present")
	}
	if len(checks) != 3 {
		t.Fatalf("got %d check runs, want 3 (unknown __typename skipped)", len(checks))
	}
	want := []struct{ name, status, conclusion, url string }{
		{"unit", "completed", "success", "https://ci.example/unit"},
		{"DCO", "completed", "failure", "https://probot.github.io/apps/dco/"},
		{"ci/pending", "in_progress", "", ""},
	}
	for i, w := range want {
		cr := checks[i]
		if cr.GetID() != int64(i+1) {
			t.Fatalf("check[%d] id = %d, want %d", i, cr.GetID(), i+1)
		}
		if cr.GetName() != w.name || cr.GetStatus() != w.status || cr.GetConclusion() != w.conclusion || cr.GetDetailsURL() != w.url {
			t.Fatalf("check[%d] = %s/%s/%s/%s, want %s/%s/%s/%s", i,
				cr.GetName(), cr.GetStatus(), cr.GetConclusion(), cr.GetDetailsURL(),
				w.name, w.status, w.conclusion, w.url)
		}
	}
}

func TestGraphQLCheckRunsWithoutRollupReportsNotOK(t *testing.T) {
	if checks, ok := graphQLCheckRuns(graphQLPRBatchNode{}); ok || checks != nil {
		t.Fatalf("no commits: got ok=%v checks=%v, want ok=false nil", ok, checks)
	}
	n := decodeGQLPRNode(t, gqlPRNodeWithContextsJSON(1, "sha-1", ""))
	if len(n.Commits.Nodes) != 1 || n.Commits.Nodes[0].Commit.StatusCheckRollup != nil {
		t.Fatalf("fixture should carry one commit with a null rollup")
	}
	if checks, ok := graphQLCheckRuns(n); ok || checks != nil {
		t.Fatalf("nil rollup: got ok=%v checks=%v, want ok=false nil", ok, checks)
	}
}

func TestGraphQLMergeableMapping(t *testing.T) {
	if got := graphQLMergeable(" mergeable "); got == nil || !*got {
		t.Fatalf("MERGEABLE -> %v, want true", got)
	}
	if got := graphQLMergeable("CONFLICTING"); got == nil || *got {
		t.Fatalf("CONFLICTING -> %v, want false", got)
	}
	for _, raw := range []string{"UNKNOWN", "", "garbage"} {
		if got := graphQLMergeable(raw); got != nil {
			t.Fatalf("%q -> %v, want nil", raw, *got)
		}
	}
}

func TestCloneCheckRunsSkipsNilAndCopies(t *testing.T) {
	started := &gh.Timestamp{Time: mustTime("2026-10-08T20:00:00Z")}
	src := []*gh.CheckRun{nil, {ID: gh.Ptr(int64(7)), Name: gh.Ptr("unit"), StartedAt: started}}
	out := cloneCheckRuns(src)
	if len(out) != 1 || out[0].GetID() != 7 || out[0].GetName() != "unit" {
		t.Fatalf("cloneCheckRuns = %+v, want one copy of id 7", out)
	}
	*out[0].Name = "mutated"
	if src[1].GetName() != "unit" {
		t.Fatalf("clone shares Name pointer with source")
	}
	if out[0].StartedAt == src[1].StartedAt {
		t.Fatalf("clone shares StartedAt pointer with source")
	}
}

func TestGraphQLPRBatchPageSizeDefaultsWithoutConfig(t *testing.T) {
	var nilClient *Client
	if nilClient.graphQLPRBatchEnabledEffective() {
		t.Fatalf("nil client reports batch enabled")
	}
	if got := nilClient.graphQLPRBatchPageSizeEffective(); got != config.DefaultGraphQLPRBatchPageSize {
		t.Fatalf("nil client page size = %d, want %d", got, config.DefaultGraphQLPRBatchPageSize)
	}
	c := &Client{}
	if c.graphQLPRBatchEnabledEffective() {
		t.Fatalf("unconfigured client reports batch enabled")
	}
	if got := c.graphQLPRBatchPageSizeEffective(); got != config.DefaultGraphQLPRBatchPageSize {
		t.Fatalf("unconfigured page size = %d, want %d", got, config.DefaultGraphQLPRBatchPageSize)
	}
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 500 })
	if got := c.graphQLPRBatchPageSizeEffective(); got != 100 {
		t.Fatalf("oversized page size = %d, want clamp to 100", got)
	}
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 0 })
	if got := c.graphQLPRBatchPageSizeEffective(); got != config.DefaultGraphQLPRBatchPageSize {
		t.Fatalf("zero page size = %d, want default %d", got, config.DefaultGraphQLPRBatchPageSize)
	}
	nilClient.SetGraphQLPRBatchConfig(func() bool { return true }, nil)
}

// gqlPRNodeWithContextsJSON is gqlPRNodeJSON with caller-supplied
// statusCheckRollup contexts (already JSON-encoded, comma-joined). An empty
// rollup argument omits statusCheckRollup entirely (null), which is what the
// API returns for a head commit no app has reported against.
func gqlPRNodeWithContextsJSON(number int, sha, contexts string) string {
	rollup := "null"
	if contexts != "" {
		rollup = fmt.Sprintf(`{"state":"FAILURE","contexts":{"nodes":[%s]}}`, contexts)
	}
	return fmt.Sprintf(`{"number":%d,"headRefOid":%q,"updatedAt":"2026-10-08T20:00:00Z","isDraft":false,"mergeable":"MERGEABLE","mergeStateStatus":"CLEAN","reviewDecision":"APPROVED","labels":{"nodes":[{"name":""}]},"author":{"login":"bot"},"baseRefName":"v5","title":"PR %d","commits":{"nodes":[{"commit":{"statusCheckRollup":%s}}]},"closingIssuesReferences":{"nodes":[{"number":0}]}}`, number, sha, number, rollup)
}

// pullDetailPath matches GET /repos/{owner}/{repo}/pulls/{number} and
// nothing beneath it (reviews, files, comments are other lookups).
var pullDetailPath = regexp.MustCompile(`/pulls/\d+$`)

func TestGraphQLPRBatchStatusContextFailureDrivesCIStatus(t *testing.T) {
	restore := resetPRDetailCacheForTest(time.Now, 128)
	defer restore()

	var checkRuns, pullGets atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/graphql":
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s,%s,%s]}},"rateLimit":{"cost":3,"remaining":4997,"resetAt":"2026-10-08T23:00:00Z"}}}`,
				gqlPRNodeWithContextsJSON(1, "sha-1",
					`{"__typename":"CheckRun","name":"unit","status":"COMPLETED","conclusion":"SUCCESS","detailsUrl":"https://ci.example/1"},`+
						`{"__typename":"StatusContext","context":"DCO","state":"FAILURE","targetUrl":"https://probot.github.io/apps/dco/"}`),
				gqlPRNodeWithContextsJSON(2, "sha-2",
					`{"__typename":"StatusContext","context":"ci/build","state":"PENDING","targetUrl":""}`),
				gqlPRNodeWithContextsJSON(3, "sha-3", ""))
		case strings.Contains(r.URL.Path, "/commits/") && strings.HasSuffix(r.URL.Path, "/check-runs"):
			checkRuns.Add(1)
			fmt.Fprint(w, `{"total_count":1,"check_runs":[{"id":99,"name":"unit","status":"completed","conclusion":"success"}]}`)
		case pullDetailPath.MatchString(r.URL.Path):
			pullGets.Add(1)
			http.Error(w, `{"message":"unexpected REST pull detail"}`, http.StatusNotFound)
		case r.URL.Path == "/rate_limit":
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4997,"reset":1791500400}}}`)
		default:
			// Annotation, head-tree and shared-CI marker lookups on the
			// failing PR are best-effort; a 404 leaves CIStatus untouched.
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")

	if _, ok := c.graphQLBatchCheckRuns("org/repo", 3, "sha-3"); ok {
		t.Fatalf("PR 3 has no statusCheckRollup but a batch check-run entry was stored")
	}
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 1, ""); ok {
		t.Fatalf("empty head SHA must never match a batch check-run entry")
	}
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 1, "sha-other"); ok {
		t.Fatalf("stale head SHA must not match a batch check-run entry")
	}

	prs := []PullRequest{
		{Repo: "org/repo", Number: 1, HeadSHA: "sha-1", UpdatedAt: mustTime("2026-10-08T20:00:00Z")},
		{Repo: "org/repo", Number: 2, HeadSHA: "sha-2", UpdatedAt: mustTime("2026-10-08T20:00:00Z")},
		{Repo: "org/repo", Number: 3, HeadSHA: "sha-3", UpdatedAt: mustTime("2026-10-08T20:00:00Z")},
	}
	c.EnrichCIStatus(context.Background(), prs)

	if prs[0].CIStatus != "failure" {
		t.Fatalf("PR 1 (DCO StatusContext FAILURE) CIStatus = %q, want failure", prs[0].CIStatus)
	}
	if len(prs[0].FailingChecks) != 1 || prs[0].FailingChecks[0] != "DCO" {
		t.Fatalf("PR 1 FailingChecks = %v, want [DCO]", prs[0].FailingChecks)
	}
	if prs[0].CIChecksRunning {
		t.Fatalf("PR 1 CIChecksRunning = true, want false (every context completed)")
	}
	if prs[1].CIStatus != "pending" {
		t.Fatalf("PR 2 (StatusContext PENDING) CIStatus = %q, want pending", prs[1].CIStatus)
	}
	if prs[2].CIStatus != "success" {
		t.Fatalf("PR 3 (no rollup, REST check-runs) CIStatus = %q, want success", prs[2].CIStatus)
	}
	if got := checkRuns.Load(); got != 1 {
		t.Fatalf("REST check-runs calls = %d, want 1 (only the PR without a rollup)", got)
	}
	if got := pullGets.Load(); got != 0 {
		t.Fatalf("REST pull detail calls = %d, want 0 (all served from the batch)", got)
	}
}

func TestGraphQLPRBatchRefetchClearsStaleRepoEntries(t *testing.T) {
	restore := resetPRDetailCacheForTest(time.Now, 128)
	defer restore()

	var call atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/graphql":
			nodes := gqlPRNodeJSON(1, "CLEAN", "MERGEABLE", "sha-1", "completed", "success") + "," +
				gqlPRNodeJSON(2, "CLEAN", "MERGEABLE", "sha-2", "completed", "success")
			if call.Add(1) > 1 {
				nodes = gqlPRNodeJSON(2, "CLEAN", "MERGEABLE", "sha-2b", "completed", "success")
			}
			fmt.Fprintf(w, `{"data":{"repository":{"pullRequests":{"pageInfo":{"hasNextPage":false,"endCursor":""},"nodes":[%s]}},"rateLimit":{"cost":2,"remaining":4998,"resetAt":"2026-10-08T23:00:00Z"}}}`, nodes)
		case "/rate_limit":
			fmt.Fprint(w, `{"resources":{"core":{"limit":5000,"remaining":4999,"reset":1791500400},"graphql":{"limit":5000,"remaining":4998,"reset":1791500400}}}`)
		default:
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})
	c.SetGraphQLPRBatchConfig(func() bool { return true }, func() int { return 50 })

	// Another repo's entries must survive a refetch of org/repo.
	c.storeGraphQLPRBatchNode("org/other", graphQLPRBatchNode{Number: 9, HeadRefOID: "sha-9", ReviewDecision: "APPROVED"})

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	for _, want := range []struct {
		number int
		sha    string
	}{{1, "sha-1"}, {2, "sha-2"}} {
		if _, ok := c.graphQLBatchCheckRuns("org/repo", want.number, want.sha); !ok {
			t.Fatalf("first fetch: PR %d check runs missing", want.number)
		}
	}
	states, ok := c.graphQLBatchReviewStates("org/repo")
	if !ok || len(states) != 2 || states[1].decision != ReviewDecision("APPROVED") {
		t.Fatalf("first fetch: review states = %v ok=%v, want 2 approved entries", states, ok)
	}
	if len(states[1].linkedIssues) != 1 || states[1].linkedIssues[0].Number != 111 {
		t.Fatalf("first fetch: PR 1 linked issues = %v, want [111]", states[1].linkedIssues)
	}
	// Mutating the returned copy must not leak into the client's cache.
	states[1].linkedIssues[0].Number = 999
	if again, _ := c.graphQLBatchReviewStates("org/repo"); again[1].linkedIssues[0].Number != 111 {
		t.Fatalf("graphQLBatchReviewStates returned a shared linkedIssues slice")
	}

	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 1, "sha-1"); ok {
		t.Fatalf("second fetch: closed PR 1 still has batch check runs")
	}
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 2, "sha-2"); ok {
		t.Fatalf("second fetch: PR 2 old head sha-2 still has batch check runs")
	}
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 2, "sha-2b"); !ok {
		t.Fatalf("second fetch: PR 2 new head sha-2b missing batch check runs")
	}
	states, ok = c.graphQLBatchReviewStates("org/repo")
	if !ok || len(states) != 1 {
		t.Fatalf("second fetch: review states = %v ok=%v, want only PR 2", states, ok)
	}
	if _, present := states[1]; present {
		t.Fatalf("second fetch: closed PR 1 review state survived the clear")
	}
	other, ok := c.graphQLBatchReviewStates("org/other")
	if !ok || len(other) != 1 || other[9].decision != ReviewDecision("APPROVED") {
		t.Fatalf("org/other review state = %v ok=%v, want untouched", other, ok)
	}
	if _, ok := c.graphQLBatchReviewStates("org/unseen"); ok {
		t.Fatalf("never-fetched repo reports batch review states")
	}

	// Disabling the batch clears the repo on the next prefetch without
	// touching GraphQL, so a stale batch never masks a REST re-fetch.
	c.SetGraphQLPRBatchConfig(func() bool { return false }, func() int { return 50 })
	c.prefetchOpenPRDetailsGraphQL(context.Background(), "org", "repo", "org/repo")
	if _, ok := c.graphQLBatchCheckRuns("org/repo", 2, "sha-2b"); ok {
		t.Fatalf("disabled batch: PR 2 check runs survived the clear")
	}
	if _, ok := c.graphQLBatchReviewStates("org/repo"); ok {
		t.Fatalf("disabled batch: review states survived the clear")
	}
	if got := call.Load(); got != 2 {
		t.Fatalf("GraphQL calls = %d, want 2 (none while disabled)", got)
	}
}
