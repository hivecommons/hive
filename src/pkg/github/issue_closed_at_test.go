package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// These tests pin IssueClosedAt (client.go), the lookup the advisory digest
// uses to retire findings whose named remediations (issues, hold-gated PRs)
// have landed — the #6080 fix. The semantics under test are the safety
// contract spelled out in its doc comment:
//
//   - closed issue  -> (closedAt, true, nil)
//   - open issue    -> (zero, false, nil)
//   - 404 not found -> (zero, false, nil): "not found is not closed", so a
//     vanished reference keeps the finding open (the safe direction)
//   - other errors  -> returned, so the caller can tell "open" from
//     "could not tell"
//   - nil receiver  -> ErrNoGitHubClient
//
// A regression that turned a 404 or a 500 into "closed" would silently retire
// live findings from the digest.

func TestIssueClosedAt_ClosedIssueReportsWhen(t *testing.T) {
	closedAt := time.Date(2026, 3, 14, 15, 9, 26, 0, time.UTC)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widgets/issues/42" {
			t.Errorf("unexpected path %q", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"number":42,"state":"closed","closed_at":%q}`, closedAt.Format(time.RFC3339))
	}))
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"acme/widgets"})
	got, closed, err := c.IssueClosedAt(context.Background(), "acme", "widgets", 42)
	if err != nil {
		t.Fatalf("IssueClosedAt: %v", err)
	}
	if !closed {
		t.Fatal("closed issue reported as not closed")
	}
	if !got.Equal(closedAt) {
		t.Fatalf("closedAt = %v, want %v", got, closedAt)
	}
}

func TestIssueClosedAt_OpenIssueIsNotClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"number":7,"state":"open"}`)
	}))
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"acme/widgets"})
	got, closed, err := c.IssueClosedAt(context.Background(), "acme", "widgets", 7)
	if err != nil {
		t.Fatalf("IssueClosedAt: %v", err)
	}
	if closed {
		t.Fatal("open issue reported as closed")
	}
	if !got.IsZero() {
		t.Fatalf("open issue reported closedAt %v, want zero", got)
	}
}

// A 404 must read as "not closed", never as an error and never as closed:
// the caller retires a finding only when every named reference is closed, so
// an unreadable reference must keep the finding open.
func TestIssueClosedAt_NotFoundIsNotClosedAndNotAnError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	}))
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"acme/widgets"})
	got, closed, err := c.IssueClosedAt(context.Background(), "acme", "widgets", 9999)
	if err != nil {
		t.Fatalf("404 must not surface as an error, got: %v", err)
	}
	if closed {
		t.Fatal("404 reported as closed — this would retire findings whose references vanished")
	}
	if !got.IsZero() {
		t.Fatalf("404 reported closedAt %v, want zero", got)
	}
}

// Non-404 failures must be returned, not swallowed: "open" and "could not
// tell" are different answers to the digest.
func TestIssueClosedAt_ServerErrorIsReturned(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newTestClient(t, server, "acme", []string{"acme/widgets"})
	_, closed, err := c.IssueClosedAt(context.Background(), "acme", "widgets", 1)
	if err == nil {
		t.Fatal("server error swallowed; want error so caller can tell open from unknown")
	}
	if closed {
		t.Fatal("server error reported as closed")
	}
}

func TestIssueClosedAt_NilClient(t *testing.T) {
	var c *Client
	_, closed, err := c.IssueClosedAt(context.Background(), "acme", "widgets", 1)
	if !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
	if closed {
		t.Fatal("nil client reported closed")
	}
}
