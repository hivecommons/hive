package github

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// newSubIssueMockServer mocks POST /repos/{owner}/{repo}/issues/{n}/sub_issues.
// linked records every parent number a sub-issue link was posted for and
// requests captures the decoded body of each call. failFirst, when >0, makes
// that many leading calls return 422 (GitHub's "already at the sub-issue cap"
// shape) before succeeding.
func newSubIssueMockServer(t *testing.T, linked *[]int, requests *[]map[string]any, failFirst *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/sub_issues") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if failFirst != nil && *failFirst > 0 {
			*failFirst--
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"message":"Maximum number of sub-issues reached"}`)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if requests != nil {
			*requests = append(*requests, body)
		}
		// Path is .../issues/{n}/sub_issues; pull {n} out crudely for the test.
		parts := strings.Split(strings.TrimSuffix(r.URL.Path, "/sub_issues"), "/")
		var n int
		_, _ = jsonNumberFromString(parts[len(parts)-1], &n)
		if linked != nil {
			*linked = append(*linked, n)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":1}`)
	}))
}

// jsonNumberFromString parses a decimal string into *out, ignoring errors —
// good enough for a test double reading a path segment it controls itself.
func jsonNumberFromString(s string, out *int) (int, error) {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			break
		}
		n = n*10 + int(r-'0')
	}
	*out = n
	return n, nil
}

func TestAddSubIssue_PostsExpectedPayload(t *testing.T) {
	var linked []int
	var requests []map[string]any
	srv := newSubIssueMockServer(t, &linked, &requests, nil)
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.AddSubIssue(context.Background(), "o/r", 100, 12345); err != nil {
		t.Fatalf("AddSubIssue: unexpected error: %v", err)
	}
	if len(linked) != 1 || linked[0] != 100 {
		t.Fatalf("expected one call against parent 100, got %v", linked)
	}
	got, ok := requests[0]["sub_issue_id"]
	if !ok {
		t.Fatalf("request body missing sub_issue_id: %v", requests[0])
	}
	if n, ok := got.(float64); !ok || int64(n) != 12345 {
		t.Fatalf("expected sub_issue_id=12345, got %v", got)
	}
}

func TestAddSubIssue_RequiresParentAndSubIssueID(t *testing.T) {
	c := NewClientForTest("http://unused.invalid", "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.AddSubIssue(context.Background(), "o/r", 0, 5); err == nil {
		t.Fatal("expected error for zero parent number")
	}
	if err := c.AddSubIssue(context.Background(), "o/r", 5, 0); err == nil {
		t.Fatal("expected error for zero sub-issue id")
	}
}

func TestAddSubIssue_NilClient(t *testing.T) {
	var c *Client
	if err := c.AddSubIssue(context.Background(), "o/r", 1, 2); err != ErrNoGitHubClient {
		t.Fatalf("expected ErrNoGitHubClient, got %v", err)
	}
}

func TestAddSubIssue_PropagatesAPIFailure(t *testing.T) {
	failFirst := 1
	srv := newSubIssueMockServer(t, nil, nil, &failFirst)
	defer srv.Close()

	c := NewClientForTest(srv.URL, "o", []string{"r"}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := c.AddSubIssue(context.Background(), "o/r", 100, 12345); err == nil {
		t.Fatal("expected error when the sub-issues API call fails")
	}
}
