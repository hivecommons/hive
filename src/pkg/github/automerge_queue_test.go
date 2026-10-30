package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestQueuePRAutoMergeApprovesThenLabels(t *testing.T) {
	var sawReview, sawLabel bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "head7"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+AutoMergeQueuedLabel:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": AutoMergeQueuedLabel})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			var req struct {
				CommitID string `json:"commit_id"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode review request: %v", err)
			}
			if req.CommitID != "head7" {
				t.Fatalf("review commit_id=%q, want head7", req.CommitID)
			}
			if sawLabel {
				t.Fatal("review must be created before the PR is labeled queued")
			}
			sawReview = true
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/statuses/head7":
			// The default queued label is lgtm, which also binds a prow/lgtm status.
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "success"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			if !sawReview {
				t.Fatal("label must not be applied before approval succeeds")
			}
			sawLabel = true
			json.NewEncoder(w).Encode([]map[string]any{{"name": AutoMergeQueuedLabel}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
		t.Fatalf("QueuePRAutoMerge returned error: %v", err)
	}
	if !sawReview || !sawLabel {
		t.Fatalf("sawReview=%v sawLabel=%v, want both true", sawReview, sawLabel)
	}
}

func TestQueuePRAutoMergeUsesConfiguredLabel(t *testing.T) {
	const label = "ship-it"
	var sawLabel bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "head7"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/"+label:
			http.NotFound(w, r)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/labels":
			var req struct {
				Name string `json:"name"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode create-label request: %v", err)
			}
			if req.Name != label {
				t.Fatalf("created label %q, want %q", req.Name, label)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"name": label})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1, "state": "APPROVED"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			var got []string
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
				t.Fatalf("decode add-label request: %v", err)
			}
			if len(got) != 1 || got[0] != label {
				t.Fatalf("added labels = %v, want [%s]", got, label)
			}
			sawLabel = true
			json.NewEncoder(w).Encode([]map[string]any{{"name": label}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel(label)
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err != nil {
		t.Fatalf("QueuePRAutoMerge returned error: %v", err)
	}
	if !sawLabel {
		t.Fatal("configured label was not applied")
	}
}

func TestQueuePRAutoMergeBlockedBySentinelLabel(t *testing.T) {
	var sawReview, sawLabel bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{
				"head":   map[string]string{"sha": "head7"},
				"labels": []map[string]string{{"name": "Sentinel-Alert"}},
			})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/reviews"):
			sawReview = true
			t.Fatalf("sentinel-labelled PR must not be approved")
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			sawLabel = true
			t.Fatalf("sentinel-labelled PR must not be labelled for auto-merge")
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetSentinelAlertLabel("sentinel-alert")
	var gotAction, gotDetail string
	c.SetAttributionAudit(func(action, detail, agent string) { gotAction, gotDetail = action, detail })
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err == nil {
		t.Fatal("QueuePRAutoMerge returned nil error for sentinel-labelled PR")
	}
	if sawReview || sawLabel {
		t.Fatalf("sawReview=%v sawLabel=%v, want neither", sawReview, sawLabel)
	}
	if gotAction != AuditActionSentinelBlockedApproval || !strings.Contains(gotDetail, "label=sentinel-alert") {
		t.Fatalf("audit = %q %q, want sentinel-blocked-approval with label", gotAction, gotDetail)
	}
}

func TestQueuePRAutoMergeRequiresQueuedBy(t *testing.T) {
	called := false
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		t.Fatalf("unexpected request for missing queuedBy: %s %s", r.Method, r.URL.Path)
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, " "); err == nil {
		t.Fatal("QueuePRAutoMerge returned nil error for blank queuedBy")
	}
	if called {
		t.Fatal("GitHub API should not be called without queuedBy")
	}
}

func TestQueuePRAutoMergeRequiresHeadSHA(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"number": 7})
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	if err := c.QueuePRAutoMerge(context.Background(), "acme/widget", 7, "alice"); err == nil {
		t.Fatal("QueuePRAutoMerge returned nil error for missing head SHA")
	}
}

func TestGetPRAuthor(t *testing.T) {
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/acme/widget/pulls/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{
			"number": 7,
			"user":   map[string]string{"login": "alice"},
		})
	}))
	defer api.Close()

	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	author, err := c.GetPRAuthor(context.Background(), "widget", 7)
	if err != nil {
		t.Fatalf("GetPRAuthor returned error: %v", err)
	}
	if author != "alice" {
		t.Fatalf("author = %q, want alice", author)
	}
}

// TestQueuePRAutoMergeOnOwnPRCommentsInsteadOfReview (hivecommons/hive#15666):
// GitHub refuses an App's APPROVE on its own pull request (422 "Can not
// approve your own pull request"), so on a PR the hive opened no review is
// attempted: the queue line lands as an issue comment, the label is still
// applied and the result reports ReviewSubmitted=false.
func TestQueuePRAutoMergeOnOwnPRCommentsInsteadOfReview(t *testing.T) {
	var comments []string
	var sawLabel bool
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "head7"}, "user": map[string]any{"login": "hive[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/hive/auto-merge":
			json.NewEncoder(w).Encode(map[string]any{"name": "hive/auto-merge"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			t.Fatal("the App must not review its own pull request")
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments = append(comments, body["body"])
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			sawLabel = true
			json.NewEncoder(w).Encode([]map[string]any{{"name": "hive/auto-merge"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel("hive/auto-merge")
	c.SetAppBotLogin("hive[bot]")
	res, err := c.QueuePRAutoMergeWithNoteResult(context.Background(), "acme/widget", 7, "alice", "note")
	if err != nil {
		t.Fatalf("QueuePRAutoMergeWithNoteResult: %v", err)
	}
	if res.ReviewSubmitted || !res.SelfAuthored || res.HeadSHA != "head7" {
		t.Fatalf("result = %+v", res)
	}
	if len(comments) != 1 || !strings.HasPrefix(comments[0], "Approved by @alice for Hive auto-merge on green CI.") || !strings.HasSuffix(comments[0], "note") {
		t.Fatalf("comments = %q", comments)
	}
	if !sawLabel {
		t.Fatal("label must still be applied")
	}
}

// TestQueuePRAutoMergeOwnPRRefusalFallsBackToComment: when the hive did not
// recognise the author as itself and GitHub still answers 422 "Can not
// approve your own pull request", the refusal is policy, not a fault: the
// queue falls back to the comment and the label rather than failing.
func TestQueuePRAutoMergeOwnPRRefusalFallsBackToComment(t *testing.T) {
	var comments []string
	var reviews, labels int
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/pulls/7":
			json.NewEncoder(w).Encode(map[string]any{"head": map[string]string{"sha": "head7"}, "user": map[string]any{"login": "other-app[bot]", "type": "Bot"}})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/labels/hive/auto-merge":
			json.NewEncoder(w).Encode(map[string]any{"name": "hive/auto-merge"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/pulls/7/reviews":
			reviews++
			w.WriteHeader(http.StatusUnprocessableEntity)
			json.NewEncoder(w).Encode(map[string]any{"message": "Unprocessable Entity", "errors": []string{"Review Can not approve your own pull request"}})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			comments = append(comments, body["body"])
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			labels++
			json.NewEncoder(w).Encode([]map[string]any{{"name": "hive/auto-merge"}})
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer api.Close()
	c := NewClient("token", "acme", []string{"widget"}, nil, api.URL)
	c.SetAutoMergeLabel("hive/auto-merge")
	res, err := c.QueuePRAutoMergeWithNoteResult(context.Background(), "acme/widget", 7, "alice", "")
	if err != nil {
		t.Fatalf("QueuePRAutoMergeWithNoteResult: %v", err)
	}
	if reviews != 1 || res.ReviewSubmitted || !res.SelfAuthored || labels != 1 || len(comments) != 1 {
		t.Fatalf("reviews=%d labels=%d comments=%q result=%+v", reviews, labels, comments, res)
	}
}
