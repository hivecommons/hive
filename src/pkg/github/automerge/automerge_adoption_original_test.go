package automerge

// An adoption PR (hive/adopt-<n>) whose original a person merged at the same
// head is closed by the self-authored lane with one note
// (projectbluefin/utah#897 after #885). These pin the close, the head-match
// guard that keeps a PR carrying extra hive commits open, the per-PR lookup
// throttle, and that only an adoption branch in the base repo qualifies.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

type adoptionOriginalAPI struct {
	originalMerged bool
	originalHead   string
	originalGets   int
	closed         []int
	comments       []string
}

func (a *adoptionOriginalAPI) server(t *testing.T, adoption map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/897":
			_ = json.NewEncoder(w).Encode(adoption)
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/885":
			a.originalGets++
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 885, "state": "closed", "merged": a.originalMerged,
				"head": map[string]any{"sha": a.originalHead},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/pulls/897":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in["state"] == "closed" {
				a.closed = append(a.closed, 897)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 897, "state": "closed"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/897/comments":
			var in map[string]any
			_ = json.NewDecoder(r.Body).Decode(&in)
			body, _ := in["body"].(string)
			a.comments = append(a.comments, body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1, "body": body})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func adoptionPRPayload(ref, headRepo string) map[string]any {
	return map[string]any{
		"number": 897, "state": "open",
		"user": map[string]any{"login": testHiveAppBotLogin},
		"head": map[string]any{"ref": ref, "sha": "04cab9b", "repo": map[string]any{"full_name": headRepo}},
		"base": map[string]any{"ref": "main"},
	}
}

func adoptionEngine(url string, now *time.Time) *Engine {
	c := newAutoMergeSweepClient(url)
	c.mutation = &recordingBoundary{}
	c.now = func() time.Time { return *now }
	return c
}

func TestTrySweepClosesAdoptionPRWhenOriginalMergedAtSameHead(t *testing.T) {
	api := &adoptionOriginalAPI{originalMerged: true, originalHead: "04cab9b"}
	srv := api.server(t, adoptionPRPayload("hive/adopt-885", "acme/widget"))
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	c := adoptionEngine(srv.URL, &now)

	_, reason, err := c.trySweepSelfAuthoredPR(context.Background(), "widget", "acme", "widget", 897, true)
	if err != nil || reason != "closed" {
		t.Fatalf("trySweepSelfAuthoredPR = (%q, %v), want (closed, nil)", reason, err)
	}
	if len(api.closed) != 1 {
		t.Fatalf("closed = %v, want the adoption PR closed once", api.closed)
	}
	if len(api.comments) != 1 || !strings.HasPrefix(api.comments[0], adoptionOriginalMergedMarker) || !strings.Contains(api.comments[0], "#885") {
		t.Fatalf("comments = %q, want one marked note naming #885", api.comments)
	}
}

func TestCloseAdoptionPRIfOriginalMergedGuards(t *testing.T) {
	cases := []struct {
		name         string
		merged       bool
		originalHead string
		ref, repo    string
		wantGets     int
	}{
		{name: "original merged at a different head stays open", merged: true, originalHead: "deadbee", ref: "hive/adopt-885", repo: "acme/widget", wantGets: 1},
		{name: "original still open stays open", merged: false, originalHead: "04cab9b", ref: "hive/adopt-885", repo: "acme/widget", wantGets: 1},
		{name: "not an adoption branch is not looked up", merged: true, originalHead: "04cab9b", ref: "hive/fix-885", repo: "acme/widget"},
		{name: "adoption branch on a fork is not looked up", merged: true, originalHead: "04cab9b", ref: "hive/adopt-885", repo: "someone/widget"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			api := &adoptionOriginalAPI{originalMerged: tc.merged, originalHead: tc.originalHead}
			srv := api.server(t, nil)
			now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
			c := adoptionEngine(srv.URL, &now)
			pr := &gh.PullRequest{
				Number: gh.Ptr(897),
				Head:   &gh.PullRequestBranch{Ref: gh.Ptr(tc.ref), SHA: gh.Ptr("04cab9b"), Repo: &gh.Repository{FullName: gh.Ptr(tc.repo)}},
			}
			if c.closeAdoptionPRIfOriginalMerged(context.Background(), "widget", "acme", "widget", pr) {
				t.Fatal("closed an adoption PR that must stay open")
			}
			if api.originalGets != tc.wantGets || len(api.closed) != 0 || len(api.comments) != 0 {
				t.Fatalf("gets=%d closed=%v comments=%q, want gets=%d and no writes", api.originalGets, api.closed, api.comments, tc.wantGets)
			}
		})
	}
}

func TestCloseAdoptionPRIfOriginalMergedThrottlesLookups(t *testing.T) {
	api := &adoptionOriginalAPI{originalMerged: false, originalHead: "04cab9b"}
	srv := api.server(t, nil)
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	c := adoptionEngine(srv.URL, &now)
	pr := &gh.PullRequest{
		Number: gh.Ptr(897),
		Head:   &gh.PullRequestBranch{Ref: gh.Ptr("hive/adopt-885"), SHA: gh.Ptr("04cab9b"), Repo: &gh.Repository{FullName: gh.Ptr("acme/widget")}},
	}
	c.closeAdoptionPRIfOriginalMerged(context.Background(), "widget", "acme", "widget", pr)
	now = now.Add(time.Minute)
	c.closeAdoptionPRIfOriginalMerged(context.Background(), "widget", "acme", "widget", pr)
	if api.originalGets != 1 {
		t.Fatalf("original looked up %d times within the interval, want 1", api.originalGets)
	}
	now = now.Add(adoptCheckInterval)
	api.originalMerged = true
	if !c.closeAdoptionPRIfOriginalMerged(context.Background(), "widget", "acme", "widget", pr) || api.originalGets != 2 {
		t.Fatalf("after the interval: gets=%d closed=%v, want a second lookup that closes", api.originalGets, api.closed)
	}
}
