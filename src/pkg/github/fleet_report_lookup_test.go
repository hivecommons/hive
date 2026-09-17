package github

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// isAlreadyExists decides whether a failed +1 reaction still counts as
// "reaction sent" in EnsureFleetReport. Misclassifying a real error as
// already-exists hides write failures; the reverse spams warnings for the
// benign duplicate case GitHub reports as 422.
func TestIsAlreadyExists(t *testing.T) {
	resp422 := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusUnprocessableEntity}}
	resp500 := &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusInternalServerError}}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil error", nil, false},
		{"github 422 unprocessable", resp422, true},
		{"github 500", resp500, false},
		{"github error without response", &gh.ErrorResponse{Message: "reaction already exists"}, true},
		{"plain already-exists text", errors.New("Reaction Already exists"), true},
		{"unrelated error", errors.New("connection refused"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isAlreadyExists(tc.err); got != tc.want {
				t.Fatalf("isAlreadyExists(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func fleetLookupClient(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return NewClientForTest(srv.URL, "hivecommons", nil, slog.New(slog.DiscardHandler))
}

func TestFleetReportIssueNilClient(t *testing.T) {
	var c *Client
	if _, _, err := c.FleetReportIssue(context.Background(), "fp"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil receiver: err = %v, want ErrNoGitHubClient", err)
	}
	if _, _, err := (&Client{}).FleetReportIssue(context.Background(), "fp"); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil inner client: err = %v, want ErrNoGitHubClient", err)
	}
}

func TestFleetReportIssueFound(t *testing.T) {
	c := fleetLookupClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/issues" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		q := r.URL.Query().Get("q")
		if !strings.Contains(q, `"hive-fleet-fingerprint:fp-1"`) {
			t.Fatalf("search query does not quote the fingerprint marker: %q", q)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"number": 12, "html_url": "https://github.com/hivecommons/hive/issues/12"},
		}})
	})
	res, found, err := c.FleetReportIssue(context.Background(), "fp-1")
	if err != nil {
		t.Fatal(err)
	}
	if !found || res.Number != 12 || res.URL != "https://github.com/hivecommons/hive/issues/12" {
		t.Fatalf("result = %+v found=%v, want issue 12", res, found)
	}
}

// The fingerprint search runs over issues+PRs; a PR that quotes the marker in
// its body must not be mistaken for the standing fleet-report issue.
func TestFleetReportIssueSkipsPullRequests(t *testing.T) {
	c := fleetLookupClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{
			{"number": 7, "pull_request": map[string]any{"url": "https://api.github.com/repos/hivecommons/hive/pulls/7"}},
			{"number": 9, "html_url": "https://github.com/hivecommons/hive/issues/9"},
		}})
	})
	res, found, err := c.FleetReportIssue(context.Background(), "fp-2")
	if err != nil {
		t.Fatal(err)
	}
	if !found || res.Number != 9 {
		t.Fatalf("result = %+v found=%v, want the non-PR issue 9", res, found)
	}
}

func TestFleetReportIssueNotFound(t *testing.T) {
	c := fleetLookupClient(t, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	})
	res, found, err := c.FleetReportIssue(context.Background(), "fp-3")
	if err != nil {
		t.Fatal(err)
	}
	if found || res.Number != 0 {
		t.Fatalf("result = %+v found=%v, want not found", res, found)
	}
}

func TestFleetReportIssueSearchError(t *testing.T) {
	c := fleetLookupClient(t, func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	_, found, err := c.FleetReportIssue(context.Background(), "fp-4")
	if err == nil || found {
		t.Fatalf("err = %v found = %v, want search error surfaced and found=false", err, found)
	}
}
