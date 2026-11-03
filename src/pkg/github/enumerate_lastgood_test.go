package github

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// twoRepoMux answers issues and PRs for two repos; repo "flaky" fails with
// failStatus whenever failing is set.
func twoRepoMux(t *testing.T, org string, failing *atomic.Bool, failStatus int, failBody string, failPRsOnly bool) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	stableIssues := []wireIssue{{Number: 1, Title: "stable issue", User: wireUser{"alice"}, CreatedAt: hoursAgo(2)}}
	flakyIssues := []wireIssue{
		{Number: 11, Title: "flaky issue a", User: wireUser{"bob"}, CreatedAt: hoursAgo(3)},
		{Number: 12, Title: "flaky issue b", User: wireUser{"carol"}, CreatedAt: hoursAgo(1)},
	}
	flakyPRs := []wirePR{{Number: 20, Title: "flaky pr", User: wireUser{"dave"}, CreatedAt: hoursAgo(1)}}
	fail := func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(failStatus)
		fmt.Fprint(w, failBody)
	}
	mux.HandleFunc(fmt.Sprintf("/repos/%s/stable/issues", org), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, stableIssues))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/stable/pulls", org), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/flaky/issues", org), func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() && !failPRsOnly {
			fail(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, flakyIssues))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/flaky/pulls", org), func(w http.ResponseWriter, r *http.Request) {
		if failing.Load() {
			fail(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, flakyPRs))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[]`))
	})
	return mux
}

// A transient refusal of one repo must not publish that repo as empty: the
// previous successful enumeration is substituted and the repo is marked
// stale (hosted hive, 2026-10-21: TOTAL OPEN ISSUES 0 with 19 open issues).
func TestEnumerateActionable_TransientRepoFailureServesLastGood(t *testing.T) {
	for _, tc := range []struct {
		name       string
		status     int
		body       string
		prsOnly    bool
		wantStale  bool
		wantIssues int
		wantPRs    int
	}{
		{"503 issues", http.StatusServiceUnavailable, `{"message":"unavailable"}`, false, true, 3, 1},
		{"429 issues", http.StatusTooManyRequests, `{"message":"too many requests"}`, false, true, 3, 1},
		{"503 prs only", http.StatusServiceUnavailable, `{"message":"unavailable"}`, true, true, 3, 1},
		// A permission 403 describes the repo, not the weather: omitted as before.
		{"403 permission", http.StatusForbidden, `{"message":"Resource not accessible by integration"}`, false, false, 1, 0},
		{"404", http.StatusNotFound, `{"message":"Not Found"}`, false, false, 1, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var failing atomic.Bool
			srv := httptest.NewServer(twoRepoMux(t, "org", &failing, tc.status, tc.body, tc.prsOnly))
			defer srv.Close()
			c := newTestClient(t, srv, "org", []string{"stable", "flaky"})

			first, err := c.EnumerateActionable(context.Background())
			if err != nil {
				t.Fatalf("first enumerate: %v", err)
			}
			if first.Issues.Count != 3 || first.PRs.Count != 1 || first.Stale() {
				t.Fatalf("first pass = issues %d prs %d stale %v, want 3/1/false", first.Issues.Count, first.PRs.Count, first.Stale())
			}
			if first.TotalByRepo["flaky"] != (RepoCounts{Issues: 2, PRs: 1}) {
				t.Fatalf("first TotalByRepo[flaky] = %+v", first.TotalByRepo["flaky"])
			}

			failing.Store(true)
			second, err := c.EnumerateActionable(context.Background())
			if err != nil {
				t.Fatalf("second enumerate: %v", err)
			}
			if second.Issues.Count != tc.wantIssues || second.PRs.Count != tc.wantPRs {
				t.Fatalf("second pass = issues %d prs %d, want %d/%d", second.Issues.Count, second.PRs.Count, tc.wantIssues, tc.wantPRs)
			}
			if second.Stale() != tc.wantStale {
				t.Fatalf("second pass stale = %v (repos %v), want %v", second.Stale(), second.StaleRepos, tc.wantStale)
			}
			if tc.wantStale {
				if len(second.StaleRepos) != 1 || second.StaleRepos[0] != "flaky" {
					t.Fatalf("StaleRepos = %v, want [flaky]", second.StaleRepos)
				}
				if second.TotalByRepo["flaky"] != (RepoCounts{Issues: 2, PRs: 1}) {
					t.Fatalf("stale TotalByRepo[flaky] = %+v, want last-good 2/1", second.TotalByRepo["flaky"])
				}
			} else if _, ok := second.TotalByRepo["flaky"]; ok {
				t.Fatalf("non-transient failure must omit the repo's totals, got %+v", second.TotalByRepo["flaky"])
			}

			// Recovery: a fresh fetch replaces the substitution and clears stale.
			failing.Store(false)
			third, err := c.EnumerateActionable(context.Background())
			if err != nil {
				t.Fatalf("third enumerate: %v", err)
			}
			if third.Issues.Count != 3 || third.Stale() {
				t.Fatalf("recovered pass = issues %d stale %v, want 3/false", third.Issues.Count, third.Stale())
			}
		})
	}
}

// When every repo fails the pass still errors (the caller keeps its prior
// state), even though last-good enumerations exist — a transient blackout
// is not a new snapshot.
func TestEnumerateActionable_AllReposTransientStillErrors(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(twoRepoMux(t, "org", &failing, http.StatusServiceUnavailable, `{"message":"unavailable"}`, false))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"flaky"})
	if _, err := c.EnumerateActionable(context.Background()); err != nil {
		t.Fatalf("first enumerate: %v", err)
	}
	failing.Store(true)
	if _, err := c.EnumerateActionable(context.Background()); err == nil {
		t.Fatal("all-repos failure must still return an error so the caller keeps prior state")
	}
}

// A last-good enumeration older than the max age is not substituted.
func TestEnumerateActionable_LastGoodExpires(t *testing.T) {
	var failing atomic.Bool
	srv := httptest.NewServer(twoRepoMux(t, "org", &failing, http.StatusServiceUnavailable, `{"message":"unavailable"}`, false))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"stable", "flaky"})
	now := time.Now()
	c.lastGoodEnum.now = func() time.Time { return now }
	if _, err := c.EnumerateActionable(context.Background()); err != nil {
		t.Fatal(err)
	}
	failing.Store(true)
	now = now.Add(lastGoodEnumerationMaxAge + time.Minute)
	res, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res.Stale() || res.Issues.Count != 1 {
		t.Fatalf("expired last-good was substituted: stale %v issues %d", res.Stale(), res.Issues.Count)
	}
}

func TestIsTransientEnumerationError(t *testing.T) {
	resp := func(status int) *http.Response {
		return &http.Response{StatusCode: status, Header: http.Header{}, Request: &http.Request{}}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"abuse (secondary) limit", &gh.AbuseRateLimitError{Response: resp(http.StatusForbidden)}, true},
		{"primary limit / pre-emptive refusal", &gh.RateLimitError{Response: resp(http.StatusForbidden)}, true},
		{"403 rate limit message", &gh.ErrorResponse{Response: resp(http.StatusForbidden), Message: "API rate limit exceeded"}, true},
		{"403 permission", &gh.ErrorResponse{Response: resp(http.StatusForbidden), Message: "Resource not accessible by integration"}, false},
		{"429", &gh.ErrorResponse{Response: resp(http.StatusTooManyRequests)}, true},
		{"502", &gh.ErrorResponse{Response: resp(http.StatusBadGateway)}, true},
		{"404", &gh.ErrorResponse{Response: resp(http.StatusNotFound)}, false},
		{"deadline", context.DeadlineExceeded, true},
		{"canceled", context.Canceled, false},
		{"wrapped", fmt.Errorf("listing issues for o/r: %w", &gh.ErrorResponse{Response: resp(http.StatusServiceUnavailable)}), true},
		{"plain", errors.New("boom"), false},
	}
	for _, tc := range cases {
		if got := isTransientEnumerationError(tc.err); got != tc.want {
			t.Errorf("%s: isTransientEnumerationError = %v, want %v", tc.name, got, tc.want)
		}
	}
}
