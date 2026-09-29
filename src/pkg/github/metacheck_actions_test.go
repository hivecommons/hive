package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCreateIssueComment covers the nil-receiver guard and the happy path
// through the httptest server (the forge-neutral comment primitive used by
// the escalation flow).
func TestCreateIssueComment(t *testing.T) {
	var nilClient *Client
	if err := nilClient.CreateIssueComment(context.Background(), "o/r", 1, "hi"); err != ErrNoGitHubClient {
		t.Fatalf("nil receiver: got %v, want ErrNoGitHubClient", err)
	}

	var gotBody string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/7/comments", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		var payload struct {
			Body string `json:"body"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotBody = payload.Body
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "o", []string{"o/r"})
	if err := c.CreateIssueComment(context.Background(), "o/r", 7, "escalation evidence"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if gotBody != "escalation evidence" {
		t.Errorf("body = %q, want %q", gotBody, "escalation evidence")
	}
}

// TestAddLabels covers the nil-receiver guard, the empty-list no-op, and the
// happy path.
func TestAddLabels(t *testing.T) {
	var nilClient *Client
	if err := nilClient.AddLabels(context.Background(), "o/r", 1, []string{"x"}); err != ErrNoGitHubClient {
		t.Fatalf("nil receiver: got %v, want ErrNoGitHubClient", err)
	}

	c := &Client{} // empty list must short-circuit before any API call
	if err := c.AddLabels(context.Background(), "o/r", 1, nil); err != nil {
		t.Fatalf("empty labels should be a no-op, got %v", err)
	}

	var gotLabels []string
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/issues/9/labels", func(w http.ResponseWriter, r *http.Request) {
		var payload []string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		gotLabels = payload
		json.NewEncoder(w).Encode([]map[string]any{{"name": "needs-human"}})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "o", []string{"o/r"})
	if err := client.AddLabels(context.Background(), "o/r", 9, []string{"needs-human"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if len(gotLabels) != 1 || gotLabels[0] != "needs-human" {
		t.Errorf("labels = %v, want [needs-human]", gotLabels)
	}
}

// TestFetchFailureExcerpt pulls failure/warning annotations from the given
// check runs, prefixes each with the run name, dedupes, and bounds the output.
func TestFetchFailureExcerpt(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/check-runs/101/annotations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"annotation_level": "failure", "message": "ReferenceError: seedMission is not defined"},
			{"annotation_level": "notice", "message": "ignored notice"},
			{"annotation_level": "failure", "message": "ReferenceError: seedMission is not defined"}, // dup
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "o", []string{"o/r"})
	got := c.fetchFailureExcerpt(context.Background(), "o", "r", []int64{101}, []string{"Coverage Suite"})
	if !strings.Contains(got, "seedMission is not defined") {
		t.Fatalf("excerpt missing the error: %q", got)
	}
	if !strings.HasPrefix(got, "Coverage Suite: ") {
		t.Errorf("excerpt should be prefixed with the run name, got %q", got)
	}
	if strings.Count(got, "seedMission") != 1 {
		t.Errorf("duplicate annotation not deduped: %q", got)
	}
	if strings.Contains(got, "ignored notice") {
		t.Errorf("non-failure annotation leaked into excerpt: %q", got)
	}
}

// TestFetchFailureExcerpt_RoundRobinCoversEveryRun guards against
// hivecommons/hive#9475: with the old excerptMaxRuns == 2, only the first two
// failing runs (in whatever order the caller passed them) ever contributed
// evidence, so a noisy shard could crowd out every other failing check. The
// excerpt must now read up to excerptMaxRuns runs and fill lines round-robin,
// one per run, before any run gets a second line.
func TestFetchFailureExcerpt_RoundRobinCoversEveryRun(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/check-runs/101/annotations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"annotation_level": "failure", "message": "run-a line 1"},
			{"annotation_level": "failure", "message": "run-a line 2"},
			{"annotation_level": "failure", "message": "run-a line 3"},
		})
	})
	mux.HandleFunc("/repos/o/r/check-runs/102/annotations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"annotation_level": "failure", "message": "run-b line 1"},
		})
	})
	mux.HandleFunc("/repos/o/r/check-runs/103/annotations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"annotation_level": "failure", "message": "run-c line 1"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "o", []string{"o/r"})
	got := c.fetchFailureExcerpt(context.Background(), "o", "r",
		[]int64{101, 102, 103}, []string{"run-a", "run-b", "run-c"})

	for _, want := range []string{"run-a: run-a line 1", "run-b: run-b line 1", "run-c: run-c line 1"} {
		if !strings.Contains(got, want) {
			t.Errorf("excerpt missing %q from a covered run: %q", want, got)
		}
	}
	lines := strings.Split(got, "\n")
	if lines[0] != "run-a: run-a line 1" || lines[1] != "run-b: run-b line 1" || lines[2] != "run-c: run-c line 1" {
		t.Errorf("excerpt did not fill round-robin (one line per run first), got %v", lines)
	}
}

// TestFetchFailureExcerpt_DropsBareExitCodeWhenOtherEvidenceExists confirms
// that a run's "Process completed with exit code N." annotation — which
// GitHub Actions attaches to nearly every failing job regardless of cause —
// does not take a line slot away from a more informative annotation on the
// same run.
func TestFetchFailureExcerpt_DropsBareExitCodeWhenOtherEvidenceExists(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/check-runs/101/annotations", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"annotation_level": "failure", "message": "Process completed with exit code 1."},
			{"annotation_level": "failure", "message": "TestFoo: assertion failed"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "o", []string{"o/r"})
	got := c.fetchFailureExcerpt(context.Background(), "o", "r", []int64{101}, []string{"unit"})
	if strings.Contains(got, "exit code") {
		t.Errorf("bare exit-code annotation should be dropped when a real cause exists, got %q", got)
	}
	if !strings.Contains(got, "assertion failed") {
		t.Errorf("excerpt missing the real cause: %q", got)
	}
}

// TestFetchFailureExcerpt_APIErrorDegrades confirms the excerpt is enrichment,
// never a gate: an annotations API error yields "" rather than blocking.
func TestFetchFailureExcerpt_APIErrorDegrades(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/check-runs/500/annotations", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "o", []string{"o/r"})
	if got := c.fetchFailureExcerpt(context.Background(), "o", "r", []int64{500}, []string{"x"}); got != "" {
		t.Errorf("API error should degrade to empty excerpt, got %q", got)
	}
}
