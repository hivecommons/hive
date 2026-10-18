package github

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIssueActivityKey(t *testing.T) {
	cases := []struct {
		repo   string
		number int
		want   string
	}{
		{"hivecommons/hive", 12517, "hivecommons/hive#12517"},
		{"HiveCommons/Hive", 7, "hivecommons/hive#7"},
		{"hive", 1, "hive#1"},
	}
	for _, c := range cases {
		if got := IssueActivityKey(c.repo, c.number); got != c.want {
			t.Errorf("IssueActivityKey(%q, %d) = %q, want %q", c.repo, c.number, got, c.want)
		}
	}
	// The durable rollup keys by this string, so two spellings of the same
	// repo must collapse to one entry.
	if IssueActivityKey("Org/Repo", 3) != IssueActivityKey("org/repo", 3) {
		t.Fatal("IssueActivityKey is not case-insensitive on repo")
	}
}

func TestIssueActivityAuthorKindClassification(t *testing.T) {
	cases := []struct {
		name, login, accountType, body, want string
	}{
		{"plain human", "alice", "User", "please fix the thing", issueActivityKindHuman},
		{"empty everything", "", "", "", issueActivityKindHuman},
		{"bot by account type", "dependabot", "Bot", "bump deps", issueActivityKindBot},
		{"bot by account type lowercase", "renovate", "bot", "bump deps", issueActivityKindBot},
		{"bot by [bot] suffix only", "some-app[bot]", "User", "hello", issueActivityKindBot},
		{"bot by [BOT] suffix mixed case", "Some-App[BOT]", "", "hello", issueActivityKindBot},
		{
			"agent by attribution trailer wins over human account",
			"alice", "User",
			"Closes #1\n\n— hive: agent=quality backend=copilot model=claude-fable-5.1 copilot=1.0.88",
			issueActivityKindAgent,
		},
		{
			"agent by attribution trailer wins over Bot account",
			"hivecommons-hive[bot]", "Bot",
			"Closes #1\n\n— hive: agent=scanner backend=codex model=gpt-5.6-terra codex=0.5.2",
			issueActivityKindAgent,
		},
		{
			"agent by identity footer",
			"hivecommons-hive[bot]", "Bot",
			"body\n\n---\n🐝 **Hive Agent**: `scanner` | **SHA:** `abc1234`",
			issueActivityKindAgent,
		},
		{
			"agent by identity footer with instance",
			"hivecommons-hive[bot]", "Bot",
			"body\n\n---\n🐝 **Hive Agent**: `sec` | **Instance:** `prod` | **SHA:** `abc1234`\n",
			issueActivityKindAgent,
		},
		{
			"agent by inline Hive Agent marker without footer shape",
			"alice", "User",
			"Reported via 🐝 **Hive Agent** relay",
			issueActivityKindAgent,
		},
		{
			"body mentioning hive without trailer stays human",
			"bob", "User",
			"the hive: agent crashed",
			issueActivityKindHuman,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := IssueActivityAuthorKind(c.login, c.accountType, c.body); got != c.want {
				t.Fatalf("IssueActivityAuthorKind(%q, %q, %q) = %q, want %q", c.login, c.accountType, c.body, got, c.want)
			}
		})
	}
}

func TestIssueActivitySince_NilClient(t *testing.T) {
	var c *Client
	if _, err := c.IssueActivitySince(context.Background(), "org/repo", time.Now()); err == nil {
		t.Fatal("nil *Client: want error, got nil")
	}
	c = &Client{logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))}
	if _, err := c.IssueActivitySince(context.Background(), "org/repo", time.Now()); err == nil {
		t.Fatal("Client without go-github client: want error, got nil")
	}
}

// TestIssueActivitySince_ShedsUnderBudgetPressure pins the REST-reserve
// contract: once the shared core snapshot is at or below the reserve floor the
// fetch is skipped with an error (so the caller keeps its last rollup) and no
// request reaches GitHub. The shed check exempts loopback hosts, so the client
// must point at a non-local base URL for the pressure path to engage.
func TestIssueActivitySince_ShedsUnderBudgetPressure(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	h := http.Header{}
	h.Set("X-RateLimit-Resource", githubCoreRateLimitResource)
	h.Set("X-RateLimit-Remaining", strconv.Itoa(lowValueRESTBudgetFloor-1))
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10))
	RecordRESTRequest("hive", http.MethodGet, "/repos/org/repo/issues", http.StatusOK, h)
	if LowValueRESTWorkAllowed() {
		t.Fatal("precondition: low-value REST work should be disallowed below the floor")
	}

	// Non-loopback host so shouldShedMTTRIssueFetches consults the budget.
	// The transport never gets to dial it: shedding happens first, and an
	// .invalid TLD would fail loudly if it did not.
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClient("fake-token", "org", []string{"repo"}, logger, "")
	base, err := url.Parse("https://api.example.invalid/")
	if err != nil {
		t.Fatal(err)
	}
	c.client.BaseURL = base

	got, err := c.IssueActivitySince(context.Background(), "org/repo", now.Add(-time.Hour))
	if err == nil {
		t.Fatal("want shed error, got nil")
	}
	if !strings.Contains(err.Error(), "budget below reserve") || !strings.Contains(err.Error(), "org/repo") {
		t.Fatalf("shed error = %q, want it to name the reserve and the repo", err)
	}
	if got != nil {
		t.Fatalf("shed result = %v, want nil", got)
	}
}

func TestIssueActivitySince_LoopbackIgnoresBudgetPressure(t *testing.T) {
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	resetRESTAccountingForTest(t, now)
	h := http.Header{}
	h.Set("X-RateLimit-Resource", githubCoreRateLimitResource)
	h.Set("X-RateLimit-Remaining", "0")
	h.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(30*time.Minute).Unix(), 10))
	RecordRESTRequest("hive", http.MethodGet, "/repos/org/repo/issues", http.StatusOK, h)

	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})

	got, err := c.IssueActivitySince(context.Background(), "org/repo", now.Add(-time.Hour))
	if err != nil {
		t.Fatalf("loopback fetch: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d activities, want 0 from empty list", len(got))
	}
	if hits.Load() != 1 {
		t.Fatalf("server hits = %d, want 1 (loopback must not shed)", hits.Load())
	}
}

func TestIssueActivitySince_QueryAndMapping(t *testing.T) {
	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.FixedZone("EST", -5*3600))
	created := time.Date(2026, 10, 2, 9, 30, 0, 0, time.UTC)
	closed := time.Date(2026, 10, 3, 10, 0, 0, 0, time.UTC)

	var gotPath string
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		issues := []map[string]any{
			{ // open human issue: no closed_at
				"number":     10,
				"state":      "open",
				"body":       "please fix",
				"created_at": created.Format(time.RFC3339),
				"user":       map[string]any{"login": "alice", "type": "User"},
			},
			{ // closed bot issue
				"number":     11,
				"state":      "closed",
				"body":       "bump",
				"created_at": created.Format(time.RFC3339),
				"closed_at":  closed.Format(time.RFC3339),
				"user":       map[string]any{"login": "dependabot[bot]", "type": "Bot"},
			},
			{ // agent issue filed by the hive's own App account
				"number":     12,
				"state":      "closed",
				"body":       "Found a gap\n\n— hive: agent=quality backend=copilot model=claude-fable-5.1 copilot=1.0.88",
				"created_at": created.Format(time.RFC3339),
				"closed_at":  closed.Format(time.RFC3339),
				"user":       map[string]any{"login": "hivecommons-hive[bot]", "type": "Bot"},
			},
			{ // pull request: must be skipped
				"number":       13,
				"state":        "open",
				"created_at":   created.Format(time.RFC3339),
				"user":         map[string]any{"login": "alice", "type": "User"},
				"pull_request": map[string]any{"url": "https://example.invalid/pulls/13"},
			},
			{ // closed_at set but state not closed (reopened race): ClosedAt stays zero
				"number":     14,
				"state":      "open",
				"created_at": created.Format(time.RFC3339),
				"closed_at":  closed.Format(time.RFC3339),
				"user":       map[string]any{"login": "bob", "type": "User"},
			},
			{ // number missing: skipped
				"state":      "open",
				"created_at": created.Format(time.RFC3339),
				"user":       map[string]any{"login": "carol", "type": "User"},
			},
		}
		_ = json.NewEncoder(w).Encode(issues)
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "myorg", []string{"repo1"})

	got, err := c.IssueActivitySince(context.Background(), "myorg/repo1", since)
	if err != nil {
		t.Fatalf("IssueActivitySince: %v", err)
	}

	if gotPath != "/repos/myorg/repo1/issues" {
		t.Errorf("path = %q, want /repos/myorg/repo1/issues", gotPath)
	}
	wantQuery := map[string]string{
		"state":     "all",
		"sort":      "updated",
		"direction": "desc",
		"per_page":  strconv.Itoa(issueActivityPerPage),
		"since":     since.UTC().Format(time.RFC3339),
	}
	for k, want := range wantQuery {
		if gotQuery.Get(k) != want {
			t.Errorf("query %s = %q, want %q", k, gotQuery.Get(k), want)
		}
	}

	if len(got) != 4 {
		t.Fatalf("got %d activities, want 4 (PR and number-less entries skipped): %+v", len(got), got)
	}
	byNum := map[int]IssueActivity{}
	for _, a := range got {
		byNum[a.Number] = a
		if a.Repo != "myorg/repo1" {
			t.Errorf("#%d repo = %q, want the caller's spelling myorg/repo1", a.Number, a.Repo)
		}
		if !a.CreatedAt.Equal(created) {
			t.Errorf("#%d created_at = %v, want %v", a.Number, a.CreatedAt, created)
		}
		if a.CreatedAt.Location() != time.UTC {
			t.Errorf("#%d created_at location = %v, want UTC", a.Number, a.CreatedAt.Location())
		}
	}
	if _, ok := byNum[13]; ok {
		t.Error("pull request #13 was not skipped")
	}

	open := byNum[10]
	if open.Author != "alice" || open.Kind != issueActivityKindHuman || !open.ClosedAt.IsZero() {
		t.Errorf("#10 = %+v, want open human issue with zero ClosedAt", open)
	}
	bot := byNum[11]
	if bot.Author != "dependabot[bot]" || bot.Kind != issueActivityKindBot || !bot.ClosedAt.Equal(closed) {
		t.Errorf("#11 = %+v, want closed bot issue closed at %v", bot, closed)
	}
	agent := byNum[12]
	if agent.Author != "hivecommons-hive[bot]" || agent.Kind != issueActivityKindAgent || !agent.ClosedAt.Equal(closed) {
		t.Errorf("#12 = %+v, want closed agent issue (trailer beats Bot account)", agent)
	}
	reopened := byNum[14]
	if !reopened.ClosedAt.IsZero() {
		t.Errorf("#14 ClosedAt = %v, want zero while state is open", reopened.ClosedAt)
	}
}

func TestIssueActivitySince_BareRepoUsesClientOrg(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewEncoder(w).Encode([]any{})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "myorg", []string{"repo1"})
	if _, err := c.IssueActivitySince(context.Background(), "repo1", time.Now()); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/repos/myorg/repo1/issues" {
		t.Fatalf("path = %q, want client org prepended", gotPath)
	}
}

func TestIssueActivitySince_FollowsLinkPagination(t *testing.T) {
	const pages = 3
	var pagesSeen []string
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		pagesSeen = append(pagesSeen, page)
		n := 1
		if page != "" {
			n, _ = strconv.Atoi(page)
		}
		if n < pages {
			w.Header().Set("Link", fmt.Sprintf("<%s%s?per_page=%d&page=%d>; rel=\"next\"",
				srv.URL, r.URL.Path, issueActivityPerPage, n+1))
		}
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number":     n,
			"state":      "open",
			"created_at": time.Date(2026, 10, n, 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
			"user":       map[string]any{"login": "alice", "type": "User"},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})

	got, err := c.IssueActivitySince(context.Background(), "org/repo", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IssueActivitySince: %v", err)
	}
	if len(got) != pages {
		t.Fatalf("got %d activities, want %d (one per page)", len(got), pages)
	}
	if len(pagesSeen) != pages {
		t.Fatalf("requests = %v, want %d pages", pagesSeen, pages)
	}
	// The first request carries no page param; later ones follow Link.
	if pagesSeen[0] != "" || pagesSeen[1] != "2" || pagesSeen[2] != "3" {
		t.Fatalf("pages requested = %v, want [\"\" 2 3]", pagesSeen)
	}
	for i, a := range got {
		if a.Number != i+1 {
			t.Errorf("got[%d].Number = %d, want %d (page order preserved)", i, a.Number, i+1)
		}
	}
}

// TestIssueActivitySince_StopsAtMaxPages guards the runaway-pagination cap: a
// server that always advertises a next page must be cut off at
// issueActivityMaxPages requests rather than looping forever.
func TestIssueActivitySince_StopsAtMaxPages(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := int(hits.Add(1))
		w.Header().Set("Link", fmt.Sprintf("<%s%s?per_page=%d&page=%d>; rel=\"next\"",
			srv.URL, r.URL.Path, issueActivityPerPage, n+1))
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number":     n,
			"state":      "open",
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"user":       map[string]any{"login": "alice", "type": "User"},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})

	got, err := c.IssueActivitySince(context.Background(), "org/repo", time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("IssueActivitySince: %v", err)
	}
	if hits.Load() != issueActivityMaxPages {
		t.Fatalf("requests = %d, want exactly %d", hits.Load(), issueActivityMaxPages)
	}
	if len(got) != issueActivityMaxPages {
		t.Fatalf("got %d activities, want %d", len(got), issueActivityMaxPages)
	}
}

func TestIssueActivitySince_APIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})

	since := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	got, err := c.IssueActivitySince(context.Background(), "org/repo", since)
	if err == nil {
		t.Fatal("want error from 500, got nil")
	}
	if got != nil {
		t.Fatalf("result on error = %v, want nil", got)
	}
	for _, want := range []string{"listing issues for org/repo", since.Format(time.RFC3339)} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

// TestIssueActivitySince_ErrorMidPaginationDropsPartial pins that a failure on
// a later page returns an error rather than a silently truncated slice — the
// rollup caller treats an error as "keep the last good rollup".
func TestIssueActivitySince_ErrorMidPaginationDropsPartial(t *testing.T) {
	var hits atomic.Int32
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) > 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Link", fmt.Sprintf("<%s%s?per_page=%d&page=2>; rel=\"next\"",
			srv.URL, r.URL.Path, issueActivityPerPage))
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"number":     1,
			"state":      "open",
			"created_at": time.Now().UTC().Format(time.RFC3339),
			"user":       map[string]any{"login": "alice", "type": "User"},
		}})
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "org", []string{"repo"})

	got, err := c.IssueActivitySince(context.Background(), "org/repo", time.Now().Add(-time.Hour))
	if err == nil {
		t.Fatal("want error from page 2 failure, got nil")
	}
	if got != nil {
		t.Fatalf("partial result = %v, want nil on error", got)
	}
	if hits.Load() != 2 {
		t.Fatalf("requests = %d, want 2", hits.Load())
	}
}
