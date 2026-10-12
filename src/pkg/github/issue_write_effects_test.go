package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/effects"
)

const uncertainEffectMsg = "effect outcome uncertain, reconciliation required"

// issueWriteServer serves issue 63 of o/r. Each write (POST comment, POST
// labels, DELETE label) answers with the next status in writeStatuses (the
// last repeats); a status of 0 blocks until the client gives up. GETs report
// the issue's labels and comments for reconciliation.
type issueWriteServer struct {
	writes, gets  atomic.Int32
	writeStatuses []int
	labels        []string
	comments      []string
}

func newIssueWriteServer(t *testing.T, s *issueWriteServer) *httptest.Server {
	t.Helper()
	done := make(chan struct{})
	t.Cleanup(func() { close(done) })
	write := func(w http.ResponseWriter, r *http.Request, ok any) {
		n := int(s.writes.Add(1)) - 1
		if n >= len(s.writeStatuses) {
			n = len(s.writeStatuses) - 1
		}
		status := s.writeStatuses[n]
		if status == 0 {
			// Drain the body first: net/http only starts the background read
			// that cancels r.Context() on client disconnect once the request
			// body is consumed, so a blocked POST would otherwise never see
			// the client's deadline. done bounds the wait to the test.
			_, _ = io.Copy(io.Discard, r.Body)
			select {
			case <-r.Context().Done():
			case <-done:
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status < 300 {
			_ = json.NewEncoder(w).Encode(ok)
			return
		}
		msg := "Server Error"
		if status == http.StatusForbidden {
			msg = "Resource not accessible by integration"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": msg})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/issues/63/comments", func(w http.ResponseWriter, r *http.Request) {
		write(w, r, map[string]any{"id": 1, "html_url": "https://github.com/o/r/issues/63#issuecomment-1"})
	})
	mux.HandleFunc("POST /repos/o/r/issues/63/labels", func(w http.ResponseWriter, r *http.Request) {
		write(w, r, []map[string]any{{"name": "claimed"}})
	})
	mux.HandleFunc("DELETE /repos/o/r/issues/63/labels/{name}", func(w http.ResponseWriter, r *http.Request) {
		write(w, r, []map[string]any{})
	})
	mux.HandleFunc("GET /repos/o/r/issues/63", func(w http.ResponseWriter, _ *http.Request) {
		s.gets.Add(1)
		labels := make([]map[string]any, 0, len(s.labels))
		for _, l := range s.labels {
			labels = append(labels, map[string]any{"name": l})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 63, "labels": labels})
	})
	mux.HandleFunc("GET /repos/o/r/issues/63/comments", func(w http.ResponseWriter, _ *http.Request) {
		s.gets.Add(1)
		comments := make([]map[string]any, 0, len(s.comments))
		for i, body := range s.comments {
			comments = append(comments, map[string]any{"id": i + 1, "body": body, "html_url": fmt.Sprintf("https://github.com/o/r/issues/63#issuecomment-%d", i+1)})
		}
		_ = json.NewEncoder(w).Encode(comments)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type issueWriteCase struct {
	name  string
	write func(context.Context, *Client) error

	// unappliedLabels is the issue's label set while the write is NOT applied.
	unappliedLabels []string
}

func issueWriteCases() []issueWriteCase {
	return []issueWriteCase{
		{"CreateIssueComment", func(ctx context.Context, c *Client) error {
			return c.CreateIssueComment(ctx, "o/r", 63, "claiming this issue")
		}, nil},
		{"AddLabels", func(ctx context.Context, c *Client) error {
			return c.AddLabels(ctx, "o/r", 63, []string{"claimed"})
		}, nil},
		{"RemoveLabel", func(ctx context.Context, c *Client) error {
			return c.RemoveLabel(ctx, "o/r", 63, "claimed")
		}, []string{"claimed"}},
	}
}

// TestIssueWrite_DefiniteRefusalAllowsRetry is the #11556 reproduction: a 403
// "Resource not accessible by integration" on an issue comment or label write
// is journaled NotApplied, so once permissions are fixed the next call with
// the same inputs runs the effect instead of returning ErrNeedsReconciliation.
func TestIssueWrite_DefiniteRefusalAllowsRetry(t *testing.T) {
	for _, tc := range issueWriteCases() {
		t.Run(tc.name, func(t *testing.T) {
			s := &issueWriteServer{writeStatuses: []int{http.StatusForbidden, http.StatusOK}}
			c := NewClientForTest(newIssueWriteServer(t, s).URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			err := tc.write(context.Background(), c)
			if err == nil || !strings.Contains(err.Error(), "Resource not accessible by integration") {
				t.Fatalf("first write err = %v, want GitHub's 403", err)
			}
			if !errors.Is(err, effects.ErrNotApplied) || strings.Contains(err.Error(), uncertainEffectMsg) {
				t.Fatalf("403 was not recorded NotApplied: %v", err)
			}
			if !githubStatusError(err, http.StatusForbidden) {
				t.Fatalf("NotApplied wrap lost the GitHub error chain: %v", err)
			}
			if err := tc.write(context.Background(), c); err != nil {
				t.Fatalf("retry after permissions fixed: %v", err)
			}
			if got := s.writes.Load(); got != 2 {
				t.Fatalf("write ran %d times, want 2", got)
			}
			if got := s.gets.Load(); got != 0 {
				t.Fatalf("NotApplied retry reconciled (%d GETs), want a direct retry", got)
			}
		})
	}
}

// TestIssueWrite_AmbiguousFailureStaysUnknown: a 5xx or a context deadline
// may have applied the write, so it is still journaled Unknown and the next
// call reconciles against GitHub before deciding whether to write again.
func TestIssueWrite_AmbiguousFailureStaysUnknown(t *testing.T) {
	failures := []struct {
		name    string
		status  int
		timeout time.Duration
	}{
		{"5xx", http.StatusBadGateway, 0},
		{"deadline", 0, time.Second},
	}
	for _, tc := range issueWriteCases() {
		for _, f := range failures {
			t.Run(tc.name+"/"+f.name, func(t *testing.T) {
				s := &issueWriteServer{writeStatuses: []int{f.status, http.StatusOK}, labels: tc.unappliedLabels}
				c := NewClientForTest(newIssueWriteServer(t, s).URL, "o", nil, prTestLogger())
				c.SetMutationBoundary(replayTestBoundary(t))

				ctx := context.Background()
				if f.timeout > 0 {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, f.timeout)
					defer cancel()
				}
				err := tc.write(ctx, c)
				if err == nil || !strings.Contains(err.Error(), uncertainEffectMsg) || errors.Is(err, effects.ErrNotApplied) {
					t.Fatalf("first write err = %v, want Unknown", err)
				}
				if err := tc.write(context.Background(), c); err != nil {
					t.Fatalf("second write after reconciliation: %v", err)
				}
				if got := s.gets.Load(); got != 1 {
					t.Fatalf("reconciliation GETs = %d, want 1", got)
				}
				if got := s.writes.Load(); got != 2 {
					t.Fatalf("write ran %d times, want 2 (absent on GitHub, so retried)", got)
				}
			})
		}
	}
}

// TestIssueWrite_ReconcilesAppliedUnknown: an Unknown write that GitHub shows
// as already applied replays as success without writing a duplicate.
func TestIssueWrite_ReconcilesAppliedUnknown(t *testing.T) {
	tests := []struct {
		name   string
		server *issueWriteServer
		write  func(context.Context, *Client) error
	}{
		{"comment present", &issueWriteServer{comments: []string{"other", "claiming this issue\r\n"}}, issueWriteCases()[0].write},
		{"label present", &issueWriteServer{labels: []string{"Claimed"}}, issueWriteCases()[1].write},
		{"label absent", &issueWriteServer{}, issueWriteCases()[2].write},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := tt.server
			s.writeStatuses = []int{http.StatusInternalServerError, http.StatusOK}
			c := NewClientForTest(newIssueWriteServer(t, s).URL, "o", nil, prTestLogger())
			c.SetMutationBoundary(replayTestBoundary(t))

			if err := tt.write(context.Background(), c); err == nil {
				t.Fatal("first write must fail on 500")
			}
			if err := tt.write(context.Background(), c); err != nil {
				t.Fatalf("second write = %v, want idempotent success", err)
			}
			if got := s.writes.Load(); got != 1 {
				t.Fatalf("write ran %d times, want 1 (already applied on GitHub)", got)
			}
		})
	}
}

// TestIssueWrite_UnreadableIssueKeepsRefusal: when reconciliation cannot read
// the issue the journal sentinel stands and nothing is written.
func TestIssueWrite_UnreadableIssueKeepsRefusal(t *testing.T) {
	var writes atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("POST /repos/o/r/issues/63/labels", func(w http.ResponseWriter, _ *http.Request) {
		writes.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	})
	mux.HandleFunc("GET /repos/o/r/issues/63", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "o", nil, prTestLogger())
	c.SetMutationBoundary(replayTestBoundary(t))

	_ = c.AddLabels(context.Background(), "o/r", 63, []string{"claimed"})
	err := c.AddLabels(context.Background(), "o/r", 63, []string{"claimed"})
	if !errors.Is(err, effects.ErrNeedsReconciliation) || !strings.Contains(err.Error(), "reconciliation lookup failed") {
		t.Fatalf("second AddLabels err = %v, want the journal sentinel", err)
	}
	if got := writes.Load(); got != 1 {
		t.Fatalf("write ran %d times, want 1", got)
	}
}

func TestIsDefiniteIssueWriteRefusal(t *testing.T) {
	resp := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Request: httptest.NewRequest(http.MethodPost, "https://api.github.com/repos/o/r/issues/63/labels", nil)}
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"403", &gh.ErrorResponse{Response: resp(http.StatusForbidden), Message: "Resource not accessible by integration"}, true},
		{"404", &gh.ErrorResponse{Response: resp(http.StatusNotFound)}, true},
		{"410", &gh.ErrorResponse{Response: resp(http.StatusGone)}, true},
		{"422", &gh.ErrorResponse{Response: resp(http.StatusUnprocessableEntity)}, true},
		{"429", &gh.ErrorResponse{Response: resp(http.StatusTooManyRequests)}, true},
		{"rate limit", &gh.RateLimitError{Response: resp(http.StatusForbidden)}, true},
		{"abuse rate limit", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden)}, true},
		{"wrapped 403", fmt.Errorf("posting: %w", &gh.ErrorResponse{Response: resp(http.StatusForbidden)}), true},
		{"500", &gh.ErrorResponse{Response: resp(http.StatusInternalServerError)}, false},
		{"502", &gh.ErrorResponse{Response: resp(http.StatusBadGateway)}, false},
		{"deadline", context.DeadlineExceeded, false},
		{"transport", errors.New("connection reset by peer"), false},
		{"no response", &gh.ErrorResponse{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isDefiniteIssueWriteRefusal(tt.err); got != tt.want {
				t.Fatalf("isDefiniteIssueWriteRefusal(%v) = %v, want %v", tt.err, got, tt.want)
			}
			wrapped := issueWriteAPIErrorNotApplied(tt.err)
			if errors.Is(wrapped, effects.ErrNotApplied) != tt.want {
				t.Fatalf("issueWriteAPIErrorNotApplied(%v) NotApplied = %v, want %v", tt.err, !tt.want, tt.want)
			}
		})
	}
	if issueWriteAPIErrorNotApplied(nil) != nil {
		t.Fatal("nil error must stay nil")
	}
}
