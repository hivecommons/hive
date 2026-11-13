package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

func verifyTestLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func TestParsePRURL(t *testing.T) {
	cases := []struct {
		name        string
		url         string
		wantErr     bool
		owner, repo string
		number      int
	}{
		{"canonical", "https://github.com/hivecommons/hive/pull/2565", false, "hivecommons", "hive", 2565},
		{"trailing files", "https://github.com/hivecommons/hive/pull/2565/files", false, "hivecommons", "hive", 2565},
		{"with fragment", "https://github.com/o/r/pull/7#discussion_r1", false, "o", "r", 7},
		{"with query", "https://github.com/o/r/pull/7?w=1", false, "o", "r", 7},
		{"ghe host", "https://ghe.example.com/o/r/pull/12", false, "o", "r", 12},
		{"whitespace", "   https://github.com/o/r/pull/9   ", false, "o", "r", 9},
		{"empty", "", true, "", "", 0},
		{"not a pr", "https://github.com/o/r/issues/9", true, "", "", 0},
		{"random text", "the agent finished the work", true, "", "", 0},
		{"zero number", "https://github.com/o/r/pull/0", true, "", "", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ref, err := ParsePRURL(tc.url)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got %+v", tc.url, ref)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.url, err)
			}
			if ref.Owner != tc.owner || ref.Repo != tc.repo || ref.Number != tc.number {
				t.Fatalf("parsed %+v, want %s/%s#%d", ref, tc.owner, tc.repo, tc.number)
			}
		})
	}
}

func TestPRBaseRepoMatches(t *testing.T) {
	cases := []struct {
		base, expected string
		want           bool
	}{
		{"hivecommons/hive", "hive", true},               // bare expected, repo matches
		{"hivecommons/hive", "Hive", true},               // case-insensitive
		{"hivecommons/hive", "hivecommons/hive", true},   // full match
		{"hivecommons/hive", "someone/hive", false},      // owner differs
		{"hivecommons/hive", "kubestellar/other", false}, // repo differs
		{"hivecommons/hive", "other", false},             // bare, repo differs
		{"", "hive", false},                              // empty base
		{"hivecommons/hive", "", false},                  // empty expected
	}
	for _, tc := range cases {
		if got := prBaseRepoMatches(tc.base, tc.expected); got != tc.want {
			t.Errorf("prBaseRepoMatches(%q,%q)=%v want %v", tc.base, tc.expected, got, tc.want)
		}
	}
}

// prJSON builds a minimal go-github PR body with the base repo full name and
// author login the tests care about.
func prJSON(baseFullName, authorLogin string, number int) string {
	owner, repo := splitRepo(baseFullName)
	m := map[string]any{
		"number":   number,
		"html_url": "https://github.com/" + baseFullName + "/pull/" + itoa(number),
		"user":     map[string]any{"login": authorLogin},
		"base": map[string]any{
			"repo": map[string]any{
				"name":      repo,
				"full_name": baseFullName,
				"owner":     map[string]any{"login": owner},
			},
		},
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// verifyMux serves GET /repos/{owner}/{repo}/pulls/{number}. When status != 200
// it returns an error body to exercise the API-error path.
func verifyMux(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if status != http.StatusOK {
			http.Error(w, `{"message":"boom"}`, status)
			return
		}
		_, _ = io.WriteString(w, body)
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func TestVerifyReportedPR_AllChecksPass(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("hivecommons/hive", "alice", 2565))
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if !res.Verified {
		t.Fatalf("expected verified, got reason=%q err=%v", res.Reason, res.Err)
	}
	if res.Author != "alice" || res.BaseRepo != "hivecommons/hive" {
		t.Fatalf("unexpected resolved fields: %+v", res)
	}
}

func TestVerifyReportedPR_CaseInsensitiveAuthorAndRepo(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("HiveCommons/Hive", "Alice", 2565))
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hivecommons/hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if !res.Verified {
		t.Fatalf("expected verified (case-insensitive), reason=%q", res.Reason)
	}
}

func TestVerifyReportedPR_WrongRepo(t *testing.T) {
	// PR base repo is a DIFFERENT repo than the assignment — the "first PR URL
	// mentioned anywhere" fallback risk. Must NOT verify.
	ts := verifyMux(t, http.StatusOK, prJSON("kubestellar/other", "alice", 99))
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/kubestellar/other/pull/99", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for wrong repo")
	}
	if res.Err != nil {
		t.Fatalf("wrong-repo is a clean negative, want no Err, got %v", res.Err)
	}
}

func TestVerifyReportedPR_WrongAuthor(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("hivecommons/hive", "mallory", 2565))
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for wrong author")
	}
	if res.Err != nil {
		t.Fatalf("wrong-author is a clean negative, want no Err, got %v", res.Err)
	}
}

func TestVerifyReportedPR_DoesNotExist(t *testing.T) {
	ts := verifyMux(t, http.StatusNotFound, "")
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/404", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for nonexistent PR")
	}
	if res.Err == nil {
		t.Fatalf("a 404 lookup should surface Err so the caller can log it")
	}
}

func TestVerifyReportedPR_APIError(t *testing.T) {
	ts := verifyMux(t, http.StatusInternalServerError, "")
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/2565", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified on API error (fail closed on trust)")
	}
	if res.Err == nil {
		t.Fatalf("transient API error should surface Err")
	}
}

func TestVerifyReportedPR_UnparseableURL(t *testing.T) {
	ts := verifyMux(t, http.StatusOK, prJSON("hivecommons/hive", "alice", 1))
	c := NewClientForTest(ts.URL, "hivecommons", []string{"hive"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "hive", "not a url", "alice")
	if res.Verified {
		t.Fatalf("expected NOT verified for unparseable URL")
	}
	if res.Err != nil {
		t.Fatalf("unparseable URL is a clean negative (nothing to look up), got Err %v", res.Err)
	}
}

func TestVerifyReportedPR_NilClient(t *testing.T) {
	var c *Client
	res := c.VerifyReportedPR(context.Background(), "hive", "https://github.com/hivecommons/hive/pull/1", "alice")
	if res.Verified {
		t.Fatalf("nil client must never verify")
	}
	if res.Err == nil {
		t.Fatalf("nil client should report ErrNoGitHubClient")
	}
}

// ---- GraphQL fallback when the REST budget is spent ----

// gqfPR is one pull request the fallback tests serve in both API forms, so a
// REST read and a GraphQL read of it can be compared field by field.
type gqfPR struct {
	base       string // "owner/repo"
	number     int
	login      string // GraphQL login; REST appends "[bot]" for a Bot
	authorType string // GraphQL __typename; "" is a deleted author
	title      string
	state      string // GraphQL form: OPEN, CLOSED or MERGED
	mergedAt   string // RFC 3339; "" when not merged
}

func (p gqfPR) url() string {
	return "https://github.com/" + p.base + "/pull/" + strconv.Itoa(p.number)
}

// restLogin is the login REST reports for the author.
func (p gqfPR) restLogin() string {
	if p.authorType == "Bot" {
		return p.login + "[bot]"
	}
	return p.login
}

func (p gqfPR) restJSON() string {
	owner, repo := splitRepo(p.base)
	state := "closed"
	if p.state == "OPEN" {
		state = "open"
	}
	m := map[string]any{
		"number":   p.number,
		"html_url": p.url(),
		"title":    p.title,
		"state":    state,
		"merged":   p.state == "MERGED",
		"user":     map[string]any{"login": p.restLogin(), "type": p.authorType},
		"base": map[string]any{
			"repo": map[string]any{
				"name":      repo,
				"full_name": p.base,
				"owner":     map[string]any{"login": owner},
			},
		},
	}
	if p.mergedAt != "" {
		m["merged_at"] = p.mergedAt
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func (p gqfPR) graphQLJSON() string {
	pr := map[string]any{
		"state":          p.state,
		"merged":         p.state == "MERGED",
		"mergedAt":       nil,
		"title":          p.title,
		"author":         nil,
		"baseRepository": map[string]any{"nameWithOwner": p.base},
	}
	if p.mergedAt != "" {
		pr["mergedAt"] = p.mergedAt
	}
	if p.authorType != "" {
		pr["author"] = map[string]any{"__typename": p.authorType, "login": p.login}
	}
	b, _ := json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": pr}}})
	return string(b)
}

// gqfServer is a test GitHub that counts the REST and GraphQL requests it got.
type gqfServer struct {
	*httptest.Server
	rest    atomic.Int32
	graphQL atomic.Int32
}

// gqfServe mounts rest on prefix+"repos/" and graphQL on graphQLPath.
func gqfServe(t *testing.T, restPrefix, graphQLPath string, rest, graphQL http.HandlerFunc) *gqfServer {
	t.Helper()
	s := &gqfServer{}
	mux := http.NewServeMux()
	mux.HandleFunc(restPrefix+"repos/", func(w http.ResponseWriter, r *http.Request) {
		s.rest.Add(1)
		rest(w, r)
	})
	mux.HandleFunc(graphQLPath, func(w http.ResponseWriter, r *http.Request) {
		s.graphQL.Add(1)
		graphQL(w, r)
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

// gqfIsolateRateLimitState keeps a test's exhausted-budget responses from
// leaking into the rest of the package: the readings stay out of the shared
// core snapshot, and the pacer's caution window (which a primary-limit 403
// engages process-wide) runs with a 1ms gap and is released afterwards. The
// window stays engaged during the test, so the fallback is exercised through
// the pacer the way it runs on a live hive.
func gqfIsolateRateLimitState(t *testing.T) {
	t.Helper()
	resetRESTAccountingForTest(t, time.Now())
	t.Cleanup(EngageRateLimitCautionForTest(0, time.Millisecond))
}

func gqfRESTOK(pr gqfPR) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, pr.restJSON())
	}
}

// gqfWritePrimaryLimit answers the way GitHub does when a primary (hourly)
// budget is spent: 403, the bucket at zero, a reset in the future.
func gqfWritePrimaryLimit(w http.ResponseWriter, resource string) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("X-RateLimit-Limit", "5000")
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
	h.Set("X-RateLimit-Resource", resource)
	w.WriteHeader(http.StatusForbidden)
	_, _ = io.WriteString(w, `{"message":"API rate limit exceeded for installation ID 1.","documentation_url":"https://docs.github.com/rest/overview/rate-limits-for-the-rest-api"}`)
}

func gqfRESTPrimaryLimited(w http.ResponseWriter, _ *http.Request) {
	gqfWritePrimaryLimit(w, "core")
}

func gqfGraphQLPrimaryLimited(w http.ResponseWriter, _ *http.Request) {
	gqfWritePrimaryLimit(w, "graphql")
}

func gqfRESTStatus(status int) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, `{"message":"boom"}`)
	}
}

// gqfRESTSecondaryLimited answers with GitHub's secondary (abuse) refusal.
// With zeroRemaining it also reports an empty primary bucket, which go-github
// types as *RateLimitError rather than *AbuseRateLimitError.
func gqfRESTSecondaryLimited(zeroRemaining bool) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		h := w.Header()
		h.Set("Content-Type", "application/json")
		h.Set("Retry-After", "60")
		if zeroRemaining {
			h.Set("X-RateLimit-Limit", "5000")
			h.Set("X-RateLimit-Remaining", "0")
			h.Set("X-RateLimit-Reset", strconv.FormatInt(time.Now().Add(30*time.Minute).Unix(), 10))
		}
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"message":"You have exceeded a secondary rate limit.","documentation_url":"https://docs.github.com/en/rest/using-the-rest-api/rate-limits-for-the-rest-api#about-secondary-rate-limits"}`)
	}
}

// gqfGraphQLOK answers the pull request, after checking that owner, name and
// number arrived as GraphQL variables and are not written into the query.
func gqfGraphQLOK(t *testing.T, pr gqfPR) http.HandlerFunc {
	owner, name := splitRepo(pr.base)
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decoding graphql request: %v", err)
		}
		if r.Method != http.MethodPost {
			t.Errorf("graphql method = %s, want POST", r.Method)
		}
		if req.Variables["owner"] != owner || req.Variables["name"] != name || req.Variables["number"] != float64(pr.number) {
			t.Errorf("graphql variables = %v, want owner=%s name=%s number=%d", req.Variables, owner, name, pr.number)
		}
		if strings.Contains(req.Query, owner) || strings.Contains(req.Query, name) {
			t.Errorf("owner/name were written into the query text: %s", req.Query)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-RateLimit-Limit", "5000")
		w.Header().Set("X-RateLimit-Remaining", "4990")
		w.Header().Set("X-RateLimit-Resource", "graphql")
		_, _ = io.WriteString(w, pr.graphQLJSON())
	}
}

func gqfGraphQLBody(status int, body string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// gqfGraphQLForbidden fails the test if a GraphQL request arrives at all.
func gqfGraphQLForbidden(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GraphQL request %s %s: this failure must not use the fallback", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	}
}

// gqfAssertSame compares every PRVerification field of a fallback result with
// the REST result for the same pull request.
func gqfAssertSame(t *testing.T, got, want PRVerification) {
	t.Helper()
	if got.Err != nil || want.Err != nil {
		t.Errorf("Err: fallback=%v rest=%v, want both nil", got.Err, want.Err)
	}
	if got.Verified != want.Verified {
		t.Errorf("Verified: fallback=%v rest=%v", got.Verified, want.Verified)
	}
	if got.Reason != want.Reason {
		t.Errorf("Reason: fallback=%q rest=%q", got.Reason, want.Reason)
	}
	if got.Author != want.Author {
		t.Errorf("Author: fallback=%q rest=%q", got.Author, want.Author)
	}
	if got.BaseRepo != want.BaseRepo {
		t.Errorf("BaseRepo: fallback=%q rest=%q", got.BaseRepo, want.BaseRepo)
	}
	if got.Merged != want.Merged {
		t.Errorf("Merged: fallback=%v rest=%v", got.Merged, want.Merged)
	}
	if got.State != want.State {
		t.Errorf("State: fallback=%q rest=%q", got.State, want.State)
	}
	if !got.MergedAt.Equal(want.MergedAt) {
		t.Errorf("MergedAt: fallback=%v rest=%v", got.MergedAt, want.MergedAt)
	}
	if got.Title != want.Title {
		t.Errorf("Title: fallback=%q rest=%q", got.Title, want.Title)
	}
}

// gqfAssertRESTFailure checks that res is exactly what a rate-limited REST
// read returned before the fallback existed: the lookup-failed reason, the
// REST request's own rate-limit error, and nothing else filled in.
func gqfAssertRESTFailure(t *testing.T, res PRVerification, restPathSuffix string) {
	t.Helper()
	if res.Verified || res.Reason != "github lookup failed" {
		t.Fatalf("Verified=%v Reason=%q, want the REST failure (unverified, \"github lookup failed\")", res.Verified, res.Reason)
	}
	var rl *gh.RateLimitError
	if !errors.As(res.Err, &rl) {
		t.Fatalf("Err = %T %v, want the REST read's *RateLimitError", res.Err, res.Err)
	}
	if got := rl.Response.Request.URL.Path; !strings.HasSuffix(got, restPathSuffix) {
		t.Fatalf("Err is for %s, want the REST read (%s), not the fallback's request", got, restPathSuffix)
	}
	if res.Author != "" || res.BaseRepo != "" || res.Merged || res.State != "" || !res.MergedAt.IsZero() || res.Title != "" {
		t.Fatalf("a failed lookup must carry no pull request fields: %+v", res)
	}
}

// The reported incident: the REST budget is spent, the pull request is real.
// Through the fallback every field must equal what the REST read gives for the
// same pull request, including the clean negatives and their reason text.
func TestVerifyReportedPR_GraphQLFallbackMatchesREST(t *testing.T) {
	gqfIsolateRateLimitState(t)
	const mergedAt = "2026-11-10T15:04:05Z"
	cases := []struct {
		name         string
		pr           gqfPR
		expectedRepo string
		author       string
		wantVerified bool
		wantReason   string
	}{
		{"open", gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "OPEN", ""}, "acme/widgets", "alice", true, ""},
		{"merged", gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "MERGED", mergedAt}, "acme/widgets", "alice", true, ""},
		{"closed unmerged", gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "CLOSED", ""}, "widgets", "alice", true, ""},
		{"case-insensitive", gqfPR{"Acme/Widgets", 7, "Alice", "User", "Fix the widget", "OPEN", ""}, "acme/widgets", "alice", true, ""},
		{"another author", gqfPR{"acme/widgets", 7, "mallory", "User", "Fix the widget", "OPEN", ""}, "acme/widgets", "alice", false,
			`PR author "mallory" does not match contributor "alice"`},
		{"another base repository", gqfPR{"acme/gadgets", 7, "alice", "User", "Fix the gadget", "OPEN", ""}, "acme/widgets", "alice", false,
			`PR base repo "acme/gadgets" does not match assigned repo "acme/widgets"`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			restOnly := gqfServe(t, "/", "/graphql", gqfRESTOK(tc.pr), gqfGraphQLForbidden(t))
			want := NewClientForTest(restOnly.URL, "acme", []string{"widgets"}, verifyTestLogger()).
				VerifyReportedPR(context.Background(), tc.expectedRepo, tc.pr.url(), tc.author)

			var logs bytes.Buffer
			limited := gqfServe(t, "/", "/graphql", gqfRESTPrimaryLimited, gqfGraphQLOK(t, tc.pr))
			got := NewClientForTest(limited.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(&logs, nil))).
				VerifyReportedPR(context.Background(), tc.expectedRepo, tc.pr.url(), tc.author)

			gqfAssertSame(t, got, want)
			if got.Verified != tc.wantVerified || got.Reason != tc.wantReason {
				t.Errorf("fallback Verified=%v Reason=%q, want %v %q", got.Verified, got.Reason, tc.wantVerified, tc.wantReason)
			}
			if rest, gql := limited.rest.Load(), limited.graphQL.Load(); rest != 1 || gql != 1 {
				t.Errorf("requests: rest=%d graphql=%d, want one of each", rest, gql)
			}
			if n := strings.Count(logs.String(), "pull request read through GraphQL instead"); n != 1 || strings.Contains(logs.String(), "level=WARN") {
				t.Errorf("want exactly one INFO line for a fallback that settled the lookup, got: %s", logs.String())
			}
			if tc.pr.state == "MERGED" {
				wantAt, _ := time.Parse(time.RFC3339, mergedAt)
				if !got.Merged || got.State != "closed" || !got.MergedAt.Equal(wantAt) {
					t.Errorf("merged PR: Merged=%v State=%q MergedAt=%v, want true, closed, %v", got.Merged, got.State, got.MergedAt, wantAt)
				}
			}
		})
	}
}

// After the first 403 go-github refuses REST requests itself until the reset
// ("not making remote request"), which is the error the incident logged. That
// refusal is per category, so the GraphQL read is still sent.
func TestVerifyReportedPR_GraphQLFallbackWhileLibraryRefusesREST(t *testing.T) {
	gqfIsolateRateLimitState(t)
	pr := gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "OPEN", ""}
	srv := gqfServe(t, "/", "/graphql", gqfRESTPrimaryLimited, gqfGraphQLOK(t, pr))
	c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, verifyTestLogger())

	for lookup := 1; lookup <= 2; lookup++ {
		res := c.VerifyReportedPR(context.Background(), "acme/widgets", pr.url(), "alice")
		if !res.Verified || res.Err != nil {
			t.Fatalf("lookup %d: Verified=%v Reason=%q Err=%v, want verified through GraphQL", lookup, res.Verified, res.Reason, res.Err)
		}
	}
	if got := srv.rest.Load(); got != 1 {
		t.Fatalf("REST requests that reached the server = %d, want 1: the second must be refused locally by the library", got)
	}
	if got := srv.graphQL.Load(); got != 2 {
		t.Fatalf("GraphQL requests = %d, want one per lookup", got)
	}
}

// When the fallback cannot settle the lookup, callers must see exactly the
// REST failure they see today; the fallback's own error is only logged.
func TestVerifyReportedPR_GraphQLFallbackFailureKeepsRESTFailure(t *testing.T) {
	gqfIsolateRateLimitState(t)
	okBody := func(pr gqfPR) http.HandlerFunc { return gqfGraphQLBody(http.StatusOK, pr.graphQLJSON()) }
	cases := []struct {
		name    string
		graphQL http.HandlerFunc
	}{
		{"graphql budget spent too", gqfGraphQLPrimaryLimited},
		{"graphql NOT_FOUND", gqfGraphQLBody(http.StatusOK,
			`{"data":{"repository":{"pullRequest":null}},"errors":[{"type":"NOT_FOUND","path":["repository","pullRequest"],"message":"Could not resolve to a PullRequest with the number of 7."}]}`)},
		{"graphql null pull request", gqfGraphQLBody(http.StatusOK, `{"data":{"repository":{"pullRequest":null}}}`)},
		{"graphql null repository", gqfGraphQLBody(http.StatusOK, `{"data":{"repository":null}}`)},
		{"graphql null data", gqfGraphQLBody(http.StatusOK, `{"data":null}`)},
		{"graphql bad gateway", gqfGraphQLBody(http.StatusBadGateway, `{"message":"bad gateway"}`)},
		{"unrecognized state", okBody(gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "DRAFTED", ""})},
		{"deleted author", okBody(gqfPR{"acme/widgets", 7, "", "", "Fix the widget", "OPEN", ""})},
		{"mannequin author", okBody(gqfPR{"acme/widgets", 7, "alice", "Mannequin", "Fix the widget", "OPEN", ""})},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			srv := gqfServe(t, "/", "/graphql", gqfRESTPrimaryLimited, tc.graphQL)
			c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, slog.New(slog.NewTextHandler(&logs, nil)))

			res := c.VerifyReportedPR(context.Background(), "acme/widgets", "https://github.com/acme/widgets/pull/7", "alice")

			gqfAssertRESTFailure(t, res, "/repos/acme/widgets/pulls/7")
			if got := srv.graphQL.Load(); got != 1 {
				t.Errorf("GraphQL requests = %d, want exactly one attempt and no retry", got)
			}
			out := logs.String()
			if strings.Count(out, "GraphQL fallback did not settle it") != 1 || !strings.Contains(out, "level=WARN") || strings.Contains(out, "level=INFO") {
				t.Errorf("want exactly one WARN line for a failed fallback, got: %s", out)
			}
			if !strings.Contains(out, "API rate limit exceeded") || !strings.Contains(out, "fallback_error=") {
				t.Errorf("WARN must carry the REST rate-limit error and the fallback's own error, got: %s", out)
			}
		})
	}
}

// GraphQL reports a bot author's login without the "[bot]" suffix REST uses,
// and the REST login cannot be rebuilt from it with certainty. So a
// bot-authored pull request that REST verifies does NOT verify through the
// fallback: it stays the REST failure, as before the fallback existed.
func TestVerifyReportedPR_GraphQLFallbackDoesNotVerifyBotAuthor(t *testing.T) {
	gqfIsolateRateLimitState(t)
	pr := gqfPR{"acme/widgets", 7, "alice-app", "Bot", "Fix the widget", "OPEN", ""}

	restOnly := gqfServe(t, "/", "/graphql", gqfRESTOK(pr), gqfGraphQLForbidden(t))
	viaREST := NewClientForTest(restOnly.URL, "acme", []string{"widgets"}, verifyTestLogger()).
		VerifyReportedPR(context.Background(), "acme/widgets", pr.url(), "alice-app[bot]")
	if !viaREST.Verified || viaREST.Author != "alice-app[bot]" {
		t.Fatalf("REST read: Verified=%v Author=%q, want the bot verified under its REST login", viaREST.Verified, viaREST.Author)
	}

	limited := gqfServe(t, "/", "/graphql", gqfRESTPrimaryLimited, gqfGraphQLOK(t, pr))
	c := NewClientForTest(limited.URL, "acme", []string{"widgets"}, verifyTestLogger())
	for _, expected := range []string{"alice-app[bot]", "alice-app"} {
		res := c.VerifyReportedPR(context.Background(), "acme/widgets", pr.url(), expected)
		gqfAssertRESTFailure(t, res, "/repos/acme/widgets/pulls/7")
	}
	if got := limited.graphQL.Load(); got != 2 {
		t.Fatalf("GraphQL requests = %d, want one per lookup", got)
	}
}

// Only the primary rate limit is answered with a GraphQL read. A missing pull
// request, a server error and a secondary (abuse) limit return what they
// return today, without a second request.
func TestVerifyReportedPR_NoGraphQLFallbackForOtherFailures(t *testing.T) {
	gqfIsolateRateLimitState(t)
	cases := []struct {
		name string
		rest http.HandlerFunc
		// check inspects the unchanged REST error.
		check func(t *testing.T, err error)
	}{
		{"404", gqfRESTStatus(http.StatusNotFound), func(t *testing.T, err error) {
			var er *gh.ErrorResponse
			if !errors.As(err, &er) || er.Response.StatusCode != http.StatusNotFound {
				t.Errorf("Err = %T %v, want the 404 ErrorResponse", err, err)
			}
		}},
		{"500", gqfRESTStatus(http.StatusInternalServerError), func(t *testing.T, err error) {
			var er *gh.ErrorResponse
			if !errors.As(err, &er) || er.Response.StatusCode != http.StatusInternalServerError {
				t.Errorf("Err = %T %v, want the 500 ErrorResponse", err, err)
			}
		}},
		{"abuse rate limit", gqfRESTSecondaryLimited(false), func(t *testing.T, err error) {
			var abuse *gh.AbuseRateLimitError
			if !errors.As(err, &abuse) {
				t.Errorf("Err = %T %v, want *AbuseRateLimitError", err, err)
			}
		}},
		{"secondary limit reporting an empty bucket", gqfRESTSecondaryLimited(true), func(t *testing.T, err error) {
			var rl *gh.RateLimitError
			if !errors.As(err, &rl) {
				t.Errorf("Err = %T %v, want go-github's *RateLimitError for a 403 with Remaining 0", err, err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := gqfServe(t, "/", "/graphql", tc.rest, gqfGraphQLForbidden(t))
			c := NewClientForTest(srv.URL, "acme", []string{"widgets"}, verifyTestLogger())

			res := c.VerifyReportedPR(context.Background(), "acme/widgets", "https://github.com/acme/widgets/pull/7", "alice")

			if res.Verified || res.Reason != "github lookup failed" || res.Err == nil {
				t.Fatalf("Verified=%v Reason=%q Err=%v, want today's lookup failure", res.Verified, res.Reason, res.Err)
			}
			tc.check(t, res.Err)
			if got := srv.graphQL.Load(); got != 0 {
				t.Fatalf("GraphQL requests = %d, want none", got)
			}
		})
	}
}

func TestPRVerifyPrimaryRateLimited(t *testing.T) {
	resp := func(retryAfter string) *http.Response {
		h := http.Header{}
		if retryAfter != "" {
			h.Set("Retry-After", retryAfter)
		}
		return &http.Response{StatusCode: http.StatusForbidden, Header: h}
	}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"github answered 403 with the budget at zero", &gh.RateLimitError{Response: resp(""), Message: "API rate limit exceeded for installation ID 1."}, true},
		{"library refused locally", &gh.RateLimitError{Response: resp(""), Message: "API rate limit of 5000 still exceeded until 2026-11-10 15:04:05 +0000 UTC, not making remote request."}, true},
		{"wrapped primary", fmt.Errorf("lookup: %w", &gh.RateLimitError{Response: resp("")}), true},
		{"primary without a response", &gh.RateLimitError{}, true},
		{"abuse", &gh.AbuseRateLimitError{Response: resp("60")}, false},
		{"wrapped abuse", fmt.Errorf("lookup: %w", &gh.AbuseRateLimitError{Response: resp("")}), false},
		{"empty bucket with Retry-After", &gh.RateLimitError{Response: resp("60"), Message: "API rate limit exceeded"}, false},
		{"empty bucket with the secondary phrase", &gh.RateLimitError{Response: resp(""), Message: "You have exceeded a secondary rate limit."}, false},
		{"404", &gh.ErrorResponse{Response: &http.Response{StatusCode: http.StatusNotFound}}, false},
		{"network", errors.New("connection reset by peer"), false},
		{"nil", nil, false},
	}
	for _, tc := range cases {
		if got := prVerifyPrimaryRateLimited(tc.err); got != tc.want {
			t.Errorf("%s: prVerifyPrimaryRateLimited = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The limit of the fallback on a host whose GraphQL path is not "/graphql".
// graphQLEndpoint maps a ".../api/v3/" REST base to "/api/graphql"; go-github
// files only the exact path "/graphql" under its GraphQL category, so on such
// a host the library's local refusal for the spent core bucket covers the
// fallback request too. The result is then the REST failure, as before. This
// pins that behaviour; it passes with or without the fallback.
func TestVerifyReportedPR_GraphQLFallbackEnterprisePathStaysRESTFailure(t *testing.T) {
	if got := gh.GetRateLimitCategory(http.MethodPost, "/graphql"); got != gh.GraphqlCategory {
		t.Fatalf("category of /graphql = %v, want the GraphQL category", got)
	}
	if got := gh.GetRateLimitCategory(http.MethodPost, "/api/graphql"); got != gh.CoreCategory {
		t.Fatalf("category of /api/graphql = %v, want core", got)
	}

	gqfIsolateRateLimitState(t)
	pr := gqfPR{"acme/widgets", 7, "alice", "User", "Fix the widget", "OPEN", ""}
	srv := gqfServe(t, "/api/v3/", "/api/graphql", gqfRESTPrimaryLimited, gqfGraphQLOK(t, pr))
	c := NewClientForTest(srv.URL+"/api/v3", "acme", []string{"widgets"}, verifyTestLogger())

	res := c.VerifyReportedPR(context.Background(), "acme/widgets", pr.url(), "alice")

	gqfAssertRESTFailure(t, res, "/api/v3/repos/acme/widgets/pulls/7")
	if got := srv.graphQL.Load(); got != 0 {
		t.Fatalf("GraphQL requests that reached the server = %d, want 0: the library refuses them under the core category", got)
	}
}
