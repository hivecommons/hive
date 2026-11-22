package automerge

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	hgithub "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Danathar/goodreads-mcp#466: the owner asked for changes on head A, the hive
// merged the base into the branch (head B), and the change request stopped
// routing because a GitHub review is bound to its commit. A trusted
// maintainer's change request now follows the PR over the App's own base
// merge, and only that: a merge by anyone else, or one that changed the
// PR's diff, ends it as before.
func TestHumanChangeRequestCarriesOverTheHivesBaseMerge(t *testing.T) {
	const oldHead, newHead, baseTip = "aaaa", "72246f98", "b45e"
	for _, tc := range []struct {
		name        string
		mergeAuthor string
		newFiles    []map[string]any
		reviewState string
		carried     bool
	}{
		{name: "the App merged the base in", mergeAuthor: testHiveAppBotLogin, newFiles: refreshFiles("blob1"), reviewState: "CHANGES_REQUESTED", carried: true},
		{name: "the App's merge changed the diff", mergeAuthor: testHiveAppBotLogin, newFiles: refreshFiles("blob9"), reviewState: "CHANGES_REQUESTED"},
		{name: "a person merged the base in", mergeAuthor: "mendezr", newFiles: refreshFiles("blob1"), reviewState: "CHANGES_REQUESTED"},
		{name: "the reviewer approved since", mergeAuthor: testHiveAppBotLogin, newFiles: refreshFiles("blob1"), reviewState: "APPROVED"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api := &baseRefreshAPI{mergeAuthor: tc.mergeAuthor, oldHead: oldHead, newHead: newHead, baseTip: baseTip, newFiles: tc.newFiles}
			inner := api.handler(t)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/15649/reviews") {
					_ = json.NewEncoder(w).Encode([]map[string]any{{
						"id": 77, "state": tc.reviewState, "commit_id": oldHead, "body": "please fix the parser",
						"user": map[string]string{"login": "clubanderson", "type": "User"}, "submitted_at": "2026-11-21T02:54:00Z",
					}})
					return
				}
				inner.ServeHTTP(w, r)
			}))
			defer server.Close()
			store := review.NewHumanChangeRequests("")
			client := hgithub.NewClient("token", "hivecommons", []string{"hive"}, nil, server.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			var logs bytes.Buffer
			c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), HumanChangeRequests: store,
				MergerAuthorizer: func(login string) bool { return login == "clubanderson" }, Logger: slog.New(slog.NewTextHandler(&logs, nil))})

			for pass := 0; pass < 2; pass++ {
				if got := c.humanChangesRequestedForHead(context.Background(), "hivecommons", "hive", 15649, newHead); got != tc.carried {
					t.Fatalf("pass %d: changes requested on %s = %v, want %v", pass, newHead, got, tc.carried)
				}
			}
			rec, ok := store.For("hivecommons/hive", 15649, newHead)
			if ok != tc.carried {
				t.Fatalf("record on the new head present = %v, want %v (%+v)", ok, tc.carried, rec)
			}
			if !tc.carried {
				return
			}
			if rec.Source != review.HumanChangeRequestSourceReview || rec.SourceID != 77 || rec.CarriedFromHead != oldHead || rec.Body != "please fix the parser" || rec.Actor != "clubanderson" {
				t.Fatalf("record = %+v, want the review carried from %s", rec, oldHead)
			}
			// The review planner reads exactly this to kick the fixer.
			if req := review.HumanChangeRequestFor(store, "hivecommons/hive", 15649, newHead); req == nil {
				t.Fatal("the planner finds no change request on the new head")
			}
			if n := strings.Count(logs.String(), humanChangesRequestedCarriedMessage); n != 1 {
				t.Fatalf("carry logged %d times, want once:\n%s", n, logs.String())
			}
		})
	}
}

// The human-merge rule (a needs-owner-merge label or a breaking-change title)
// stops the lane before it reads reviews; a trusted maintainer's change
// request on the head still turns the reason into human-changes-requested,
// so the fixer is kicked. Without one, or for another reason, nothing
// changes.
func TestHumanMergeRequiredUnlessHumanChangesRequested(t *testing.T) {
	const head = "72246f98"
	cr := []map[string]any{{"id": 5, "state": "CHANGES_REQUESTED", "commit_id": head, "user": map[string]string{"login": "clubanderson", "type": "User"}}}
	for _, tc := range []struct {
		name    string
		reason  string
		reviews []map[string]any
		labels  []string
		observe bool
		want    string
	}{
		{name: "change request on the head", reason: autoMergeReasonHumanMergeRequired,
			reviews: []map[string]any{{"id": 5, "state": "CHANGES_REQUESTED", "commit_id": head, "user": map[string]string{"login": "clubanderson", "type": "User"}}},
			want:    autoMergeReasonHumanChangesRequested},
		{name: "no review", reason: autoMergeReasonHumanMergeRequired, want: autoMergeReasonHumanMergeRequired},
		{name: "a stranger's change request", reason: autoMergeReasonHumanMergeRequired,
			reviews: []map[string]any{{"id": 5, "state": "CHANGES_REQUESTED", "commit_id": head, "user": map[string]string{"login": "passerby", "type": "User"}}},
			want:    autoMergeReasonHumanMergeRequired},
		// A person's hold (or any label block) stops the lane first: a
		// change request must not send the fixer past it.
		{name: "a person's hold beside the label", reason: autoMergeReasonHumanMergeRequired, reviews: cr, labels: []string{"hold"}, want: autoMergeReasonHumanMergeRequired},
		{name: "do-not-merge beside the label", reason: autoMergeReasonHumanMergeRequired, reviews: cr, labels: []string{"do-not-merge"}, want: autoMergeReasonHumanMergeRequired},
		{name: "observe-only pass", reason: autoMergeReasonHumanMergeRequired, reviews: cr, observe: true, want: autoMergeReasonHumanMergeRequired},
		{name: "another reason is untouched", reason: "label:hold",
			reviews: []map[string]any{{"id": 5, "state": "CHANGES_REQUESTED", "commit_id": head, "user": map[string]string{"login": "clubanderson", "type": "User"}}},
			want:    "label:hold"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reads := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews"):
					reads++
					reviews := tc.reviews
					if reviews == nil {
						reviews = []map[string]any{}
					}
					_ = json.NewEncoder(w).Encode(reviews)
				default:
					t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			store := review.NewHumanChangeRequests("")
			client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, server.URL)
			client.SetAppBotLogin(testHiveAppBotLogin)
			c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), StageTracker: NewStageTracker(), HumanChangeRequests: store, MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})
			c.observeOnly = tc.observe
			pr := &gh.PullRequest{Number: gh.Ptr(7), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}, Labels: []*gh.Label{{Name: gh.Ptr(hgithub.HumanMergePathLabel)}}}
			for _, l := range tc.labels {
				pr.Labels = append(pr.Labels, &gh.Label{Name: gh.Ptr(l)})
			}
			if got := c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, tc.reason); got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
			if (tc.reason != autoMergeReasonHumanMergeRequired || tc.observe || len(tc.labels) > 0) && reads != 0 {
				t.Fatalf("reviews read %d times where the refinement must not read", reads)
			}
			if tc.want != autoMergeReasonHumanChangesRequested && len(tc.reviews) > 0 {
				if req := review.HumanChangeRequestFor(store, "acme/widget", 7, head); req != nil {
					t.Fatalf("a change request was recorded where the refinement must not apply: %+v", req)
				}
			}
			if tc.want == autoMergeReasonHumanChangesRequested {
				if req := review.HumanChangeRequestFor(store, "acme/widget", 7, head); req == nil {
					t.Fatal("the change request was not recorded for the planner")
				}
			}
		})
	}
}

// A head whose PR has not changed since a read found no change request is
// not read again; a new review moves updated_at and is read.
func TestHumanMergeRequiredRefinementSkipsAnUnchangedPR(t *testing.T) {
	const head = "72246f98"
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews") {
			reads++
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, server.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), StageTracker: NewStageTracker(), HumanChangeRequests: review.NewHumanChangeRequests(""), MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})
	at := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	pr := &gh.PullRequest{Number: gh.Ptr(7), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}, UpdatedAt: &gh.Timestamp{Time: at}, Labels: []*gh.Label{{Name: gh.Ptr(hgithub.HumanMergePathLabel)}}}
	for i := 0; i < 3; i++ {
		if got := c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired); got != autoMergeReasonHumanMergeRequired {
			t.Fatalf("reason = %q", got)
		}
	}
	if reads != 1 {
		t.Fatalf("reviews read %d times for an unchanged PR, want once", reads)
	}
	pr.UpdatedAt = &gh.Timestamp{Time: at.Add(time.Minute)}
	_ = c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired)
	if reads != 2 {
		t.Fatalf("reviews read %d times after the PR changed, want twice", reads)
	}
}

// The App merges the base into the branch twice (A to B to C): the
// change request follows the head to C, still naming the reviewed head A, so
// the stage keeps reading it as the author's turn. The planner kicks the
// fixer once per request (review.carriedRequestFixPending).
func TestHumanChangeRequestFollowsTheHeadAcrossTwoBaseMerges(t *testing.T) {
	const headA, headB, headC = "aaaa", "bbbb", "cccc"
	// mergeBase is the compare's merge base per head: the base commit the
	// App merged in at that hop.
	mergeBase := map[string]string{headA: "base0", headB: "base1", headC: "base2"}
	parents := map[string][2]string{headB: {headA, "base1"}, headC: {headB, "base2"}}
	current := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/15649/reviews"):
			_ = json.NewEncoder(w).Encode([]map[string]any{{
				"id": 77, "state": "CHANGES_REQUESTED", "commit_id": headA, "body": "please fix the parser",
				"user": map[string]string{"login": "clubanderson", "type": "User"}, "submitted_at": "2026-11-21T02:54:00Z",
			}})
		case r.Method == http.MethodGet && strings.HasSuffix(path, "/pulls/15649"):
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 15649, "head": map[string]any{"sha": current}, "base": map[string]any{"ref": "v5"}})
		case r.Method == http.MethodGet && strings.Contains(path, "/compare/"):
			_, head, _ := strings.Cut(path[strings.Index(path, "/compare/")+len("/compare/"):], "...")
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ahead", "merge_base_commit": map[string]any{"sha": mergeBase[head]}, "files": refreshFiles("blob1")})
		case r.Method == http.MethodGet && strings.Contains(path, "/commits/"):
			sha := path[strings.LastIndex(path, "/")+1:]
			p, ok := parents[sha]
			if !ok {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"sha": sha, "author": map[string]any{"login": testHiveAppBotLogin}, "committer": map[string]any{"login": "web-flow"},
				"commit": map[string]any{"verification": map[string]any{"verified": true}}, "parents": []map[string]any{{"sha": p[0]}, {"sha": p[1]}}})
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	store := review.NewHumanChangeRequests("")
	client := hgithub.NewClient("token", "hivecommons", []string{"hive"}, nil, server.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), HumanChangeRequests: store, MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})

	for _, head := range []string{headB, headC} {
		current = head
		if !c.humanChangesRequestedForHead(context.Background(), "hivecommons", "hive", 15649, head) {
			t.Fatalf("change request lost on %s after the App's base merge", head)
		}
		rec, ok := store.For("hivecommons/hive", 15649, head)
		if !ok || rec.SourceID != 77 || rec.CarriedFromHead != headA {
			t.Fatalf("record on %s = %+v (ok=%v), want review 77 carried from %s", head, rec, ok, headA)
		}
	}
	if _, ok := store.For("hivecommons/hive", 15649, headB); ok {
		t.Fatal("the record must follow the head: B's entry should be superseded by C's")
	}
}

// A failed review read is not "no change request": the next pass on the
// same head and updated_at reads again and finds it.
func TestHumanMergeRequiredRefinementDoesNotMemoiseAFailedRead(t *testing.T) {
	const head = "72246f98"
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews") {
			calls++
			if calls == 1 {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": 5, "state": "CHANGES_REQUESTED", "commit_id": head, "user": map[string]string{"login": "clubanderson", "type": "User"}}})
			return
		}
		t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, server.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	c := New(client, Options{HeadFirstSeen: NewHeadFirstSeenTracker(), StageTracker: NewStageTracker(), HumanChangeRequests: review.NewHumanChangeRequests(""), MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})
	at := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	pr := &gh.PullRequest{Number: gh.Ptr(7), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}, UpdatedAt: &gh.Timestamp{Time: at}, Labels: []*gh.Label{{Name: gh.Ptr(hgithub.HumanMergePathLabel)}}}
	if got := c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired); got != autoMergeReasonHumanMergeRequired {
		t.Fatalf("first pass (read failed) = %q", got)
	}
	if got := c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired); got != autoMergeReasonHumanChangesRequested {
		t.Fatalf("second pass on the same head and updated_at = %q, want the change request found", got)
	}
}

// A settled "no change request" expires: a trusted-merger list change, or
// a review that did not move updated_at, is read within the TTL.
func TestHumanMergeRequiredRefinementMemoExpires(t *testing.T) {
	const head = "72246f98"
	reads := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pulls/7/reviews") {
			reads++
			_ = json.NewEncoder(w).Encode([]map[string]any{})
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()
	client := hgithub.NewClient("token", "acme", []string{"widget"}, nil, server.URL)
	client.SetAppBotLogin(testHiveAppBotLogin)
	clock := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC)
	c := New(client, Options{Now: func() time.Time { return clock }, HeadFirstSeen: NewHeadFirstSeenTracker(), StageTracker: NewStageTracker(), HumanChangeRequests: review.NewHumanChangeRequests(""), MergerAuthorizer: func(login string) bool { return login == "clubanderson" }})
	pr := &gh.PullRequest{Number: gh.Ptr(7), Head: &gh.PullRequestBranch{SHA: gh.Ptr(head)}, UpdatedAt: &gh.Timestamp{Time: clock}, Labels: []*gh.Label{{Name: gh.Ptr(hgithub.HumanMergePathLabel)}}}
	_ = c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired)
	clock = clock.Add(10 * time.Minute)
	_ = c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired)
	if reads != 1 {
		t.Fatalf("reads = %d within the TTL, want 1", reads)
	}
	clock = clock.Add(humanMergeNoChangeRequestTTL)
	_ = c.humanMergeRequiredUnlessHumanChangesRequested(context.Background(), "acme", "widget", pr, autoMergeReasonHumanMergeRequired)
	if reads != 2 {
		t.Fatalf("reads = %d after the TTL, want 2", reads)
	}
}
