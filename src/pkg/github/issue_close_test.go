package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

func TestCloseIssueReporterConfirmationGate(t *testing.T) {
	tests := []struct {
		name           string
		issue          *gh.Issue
		overrideReason string
		wantErr        bool
		wantClosed     bool
		wantComment    string
	}{
		{
			name:        "human-filed bug is blocked and asks for confirmation",
			issue:       closeGateIssue("human", "User", "Bug: still broken", "reported by a person", []string{"bug"}),
			wantErr:     true,
			wantComment: "please confirm",
		},
		{
			name:       "bot-filed bug closes normally",
			issue:      closeGateIssue("hive-app[bot]", "Bot", "Bug: bot finding", "reported by automation", []string{"bug"}),
			wantClosed: true,
		},
		{
			name:       "human-filed non-bug closes normally",
			issue:      closeGateIssue("human", "User", "Question", "reported by a person", []string{"question"}),
			wantClosed: true,
		},
		{
			name:           "override records reason and closes",
			issue:          closeGateIssue("human", "User", "Bug: duplicate", "reported by a person", []string{"kind/bug"}),
			overrideReason: "duplicate of #99; reporter asked us to consolidate there",
			wantClosed:     true,
			wantComment:    "Reporter-confirmation close override used",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var closed bool
			var comments []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
					_ = json.NewEncoder(w).Encode(tt.issue)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
					var payload struct {
						Body string `json:"body"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					comments = append(comments, payload.Body)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": len(comments)})
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
					// No prior confirmation request on the issue, so the gate
					// posts one. The dedup path is covered separately by
					// TestCloseIssueAsksForConfirmationOnlyOnce.
					_ = json.NewEncoder(w).Encode([]any{})
				case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
					var payload struct {
						State string `json:"state"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					if payload.State == "closed" {
						closed = true
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := testClient(t, srv.URL)
			err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{OverrideReason: tt.overrideReason})
			if tt.wantErr {
				if !errors.Is(err, ErrReporterConfirmationRequired) {
					t.Fatalf("CloseIssue error = %v, want ErrReporterConfirmationRequired", err)
				}
			} else if err != nil {
				t.Fatalf("CloseIssue: %v", err)
			}
			if closed != tt.wantClosed {
				t.Fatalf("closed = %v, want %v", closed, tt.wantClosed)
			}
			if tt.wantComment == "" {
				if len(comments) != 0 {
					t.Fatalf("comments = %q, want none", comments)
				}
				return
			}
			if len(comments) != 1 || !strings.Contains(comments[0], tt.wantComment) {
				t.Fatalf("comments = %q, want one containing %q", comments, tt.wantComment)
			}
		})
	}
}

func TestReporterConfirmationPredicateSharedByPRAndCloseGates(t *testing.T) {
	tests := []struct {
		name      string
		issue     *gh.Issue
		wantHuman bool
		wantGate  bool
	}{
		{
			name:      "human bug without confirmation is gated",
			issue:     closeGateIssue("human", "User", "Bug", "plain body", []string{"type: bug"}),
			wantHuman: true,
			wantGate:  true,
		},
		{
			name:      "human bug with marker is human-filed but not gated",
			issue:     closeGateIssue("human", "User", "Bug", "hive: reporter-confirmed", []string{"bug"}),
			wantHuman: true,
			wantGate:  false,
		},
		{
			name:      "agent attribution trailer exempts issue",
			issue:     closeGateIssue("hive-app[bot]", "Bot", "Bug", "body\n"+AttributionTrailerPrefix+" agent=scanner", []string{"bug"}),
			wantHuman: false,
			wantGate:  false,
		},
		{
			name:      "human enhancement is not bug-family",
			issue:     closeGateIssue("human", "User", "Enhancement", "plain body", []string{"enhancement"}),
			wantHuman: false,
			wantGate:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsHumanFiledBugReport(tt.issue); got != tt.wantHuman {
				t.Fatalf("IsHumanFiledBugReport = %v, want %v", got, tt.wantHuman)
			}
			prGate := humanFiledBugReason(tt.issue) != ""
			closeGate := ReporterConfirmationCloseGateReason(tt.issue) != ""
			if prGate != closeGate {
				t.Fatalf("PR gate = %v, close gate = %v; both call sites must share the predicate", prGate, closeGate)
			}
			if closeGate != tt.wantGate {
				t.Fatalf("ReporterConfirmationCloseGateReason active = %v, want %v", closeGate, tt.wantGate)
			}
		})
	}
}

func closeGateIssue(login, userType, title, body string, labels []string) *gh.Issue {
	issue := &gh.Issue{
		Number: gh.Ptr(7),
		Title:  gh.Ptr(title),
		Body:   gh.Ptr(body),
		User: &gh.User{
			Login: gh.Ptr(login),
			Type:  gh.Ptr(userType),
		},
	}
	for _, label := range labels {
		issue.Labels = append(issue.Labels, &gh.Label{Name: gh.Ptr(label)})
	}
	return issue
}

// A blocked close is not a one-shot event: an agent may retry, and several
// agents may converge on the same issue. Re-notifying the reporter on every
// attempt is the same discourtesy the gate exists to prevent, so the request is
// posted once and the gate error is returned unchanged thereafter.
func TestCloseIssueAsksForConfirmationOnlyOnce(t *testing.T) {
	existing := []struct {
		name    string
		prior   []any
		wantNew int
	}{
		{
			name:    "first attempt posts the request",
			prior:   []any{map[string]any{"id": 1, "body": "unrelated chatter"}},
			wantNew: 1,
		},
		{
			name:    "second attempt reuses the existing request",
			prior:   []any{map[string]any{"id": 1, "body": reporterConfirmationRequestMarker + " please confirm"}},
			wantNew: 0,
		},
	}

	for _, tt := range existing {
		t.Run(tt.name, func(t *testing.T) {
			issue := closeGateIssue("human", "User", "Bug: still broken", "reported by a person", []string{"bug"})
			var posted []string
			var closed bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
					_ = json.NewEncoder(w).Encode(issue)
				case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
					_ = json.NewEncoder(w).Encode(tt.prior)
				case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
					var payload struct {
						Body string `json:"body"`
					}
					_ = json.NewDecoder(r.Body).Decode(&payload)
					posted = append(posted, payload.Body)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
				case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
					closed = true
					_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
				default:
					t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer srv.Close()

			c := testClient(t, srv.URL)
			err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})

			// The verdict must not soften just because the reporter was already asked.
			if !errors.Is(err, ErrReporterConfirmationRequired) {
				t.Fatalf("CloseIssue error = %v, want ErrReporterConfirmationRequired", err)
			}
			if closed {
				t.Fatal("issue was closed despite the reporter-confirmation gate")
			}
			if len(posted) != tt.wantNew {
				t.Fatalf("new comments = %d (%q), want %d", len(posted), posted, tt.wantNew)
			}
		})
	}
}

// An unreadable comment list must not turn a gated close into a silent one.
func TestCloseIssueConfirmationDedupFailsOpen(t *testing.T) {
	issue := closeGateIssue("human", "User", "Bug: still broken", "reported by a person", []string{"bug"})
	var posted []string
	var closed bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7":
			_ = json.NewEncoder(w).Encode(issue)
		case r.Method == "GET" && r.URL.Path == "/repos/o/r/issues/7/comments":
			w.WriteHeader(http.StatusInternalServerError)
		case r.Method == "POST" && r.URL.Path == "/repos/o/r/issues/7/comments":
			var payload struct {
				Body string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&payload)
			posted = append(posted, payload.Body)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 99})
		case r.Method == "PATCH" && r.URL.Path == "/repos/o/r/issues/7":
			closed = true
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c := testClient(t, srv.URL)
	err := c.CloseIssue(context.Background(), "r", 7, IssueCloseOptions{})
	if !errors.Is(err, ErrReporterConfirmationRequired) {
		t.Fatalf("CloseIssue error = %v, want ErrReporterConfirmationRequired", err)
	}
	if closed {
		t.Fatal("issue was closed despite the reporter-confirmation gate")
	}
	if len(posted) != 1 {
		t.Fatalf("new comments = %d, want 1 (dedup must fail open)", len(posted))
	}
}
