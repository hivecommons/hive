package forgestate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	gh "github.com/google/go-github/v72/github"
)

// fakeGitHub serves the REST endpoints GoGitHubFetcher calls.
type fakeGitHub struct {
	mu       sync.Mutex
	srv      *httptest.Server
	failPRs  int // HTTP status for /pulls list; 0 = serve
	twoPages bool
	prsNext  bool // always advertise a next page of pulls
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeGitHub) client(t *testing.T) *gh.Client {
	t.Helper()
	c := gh.NewClient(f.srv.Client())
	u, err := url.Parse(f.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	c.BaseURL = u
	return c
}

func (f *fakeGitHub) set(fn func()) {
	f.mu.Lock()
	fn()
	f.mu.Unlock()
}

func (f *fakeGitHub) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	failPRs, twoPages, prsNext := f.failPRs, f.twoPages, f.prsNext
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	page := r.URL.Query().Get("page")
	switch r.URL.Path {
	case "/repos/o/r/issues":
		if page == "" {
			w.Header().Set("ETag", `"issues-v1"`)
			if twoPages {
				w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/issues?page=2>; rel="next"`, f.srv.URL))
			}
			fmt.Fprint(w, `[{"number":1,"state":"open","updated_at":"2026-10-21T10:00:00Z","labels":[{"name":"bug"},{"name":""}],"assignees":[{"login":"ann"},{"login":""}]},`+
				`{"number":2,"state":"open","pull_request":{"url":"x"}}, null]`)
			return
		}
		fmt.Fprint(w, `[{"number":3,"state":"open","updated_at":"2026-10-21T10:00:00Z"}]`)
	case "/repos/o/r/pulls":
		if failPRs != 0 {
			w.WriteHeader(failPRs)
			fmt.Fprint(w, `{"message":"Resource not accessible by integration"}`)
			return
		}
		if (page == "" && twoPages) || prsNext {
			w.Header().Set("Link", fmt.Sprintf(`<%s/repos/o/r/pulls?page=2>; rel="next"`, f.srv.URL))
		}
		if page == "2" {
			fmt.Fprint(w, `[{"number":4,"state":"open","head":{"sha":"d4"}}]`)
			return
		}
		fmt.Fprint(w, `[{"number":2,"state":"open","draft":true,"updated_at":"2026-10-21T10:00:00Z","head":{"sha":"abc"},"labels":[{"name":"stage/review"}]}, null]`)
	case "/repos/o/r/issues/1":
		fmt.Fprint(w, `{"number":1,"state":"closed","updated_at":"2026-10-21T11:00:00Z"}`)
	case "/repos/o/r/pulls/2":
		fmt.Fprint(w, `{"number":2,"state":"closed","merged":false,"merged_at":"2026-10-21T11:00:00Z","mergeable_state":"clean","head":{"sha":"abc"}}`)
	case "/repos/o/r/issues/404", "/repos/o/r/pulls/404":
		w.WriteHeader(http.StatusNotFound)
		fmt.Fprint(w, `{"message":"Not Found"}`)
	case "/repos/o/r/issues/410":
		w.WriteHeader(http.StatusGone)
		fmt.Fprint(w, `{"message":"This issue was deleted"}`)
	default:
		w.WriteHeader(http.StatusInternalServerError)
		fmt.Fprint(w, `{"message":"boom"}`)
	}
}

func TestGoGitHubFetcherList(t *testing.T) {
	fg := newFakeGitHub(t)
	f := NewGoGitHubFetcher(fg.client(t))
	res, err := f.List(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if res.ETag != `"issues-v1"` || res.NotModified {
		t.Fatalf("ETag=%q NotModified=%v", res.ETag, res.NotModified)
	}
	if len(res.Issues) != 1 || res.Issues[0].Number != 1 {
		t.Fatalf("issues = %+v, want only #1 (PRs filtered from the issues list)", res.Issues)
	}
	is := res.Issues[0]
	if is.Kind != KindIssue || len(is.Labels) != 1 || is.Labels[0] != "bug" || len(is.Assignees) != 1 || is.Assignees[0] != "ann" || is.UpdatedAt.IsZero() || !is.Open() {
		t.Fatalf("issue = %+v", is)
	}
	if len(res.PRs) != 1 || res.PRs[0].HeadSHA != "abc" || !res.PRs[0].Draft || !res.PRs[0].HasLabel("stage/review") || res.PRs[0].Kind != KindPR {
		t.Fatalf("prs = %+v", res.PRs)
	}

	fg.set(func() { fg.twoPages = true })
	res, err = f.List(context.Background(), "o/r", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Issues) != 2 || len(res.PRs) != 2 {
		t.Fatalf("paginated list = %d issues %d prs, want 2/2", len(res.Issues), len(res.PRs))
	}

	f.maxPages = 1
	if _, err := f.List(context.Background(), "o/r", ""); err == nil || !strings.Contains(err.Error(), "pages") {
		t.Fatalf("truncated issues = %v, want an error rather than a short list", err)
	}
}

func TestGoGitHubFetcherListTruncatedPRs(t *testing.T) {
	fg := newFakeGitHub(t)
	f := NewGoGitHubFetcher(fg.client(t))
	f.maxPages = 1
	// Issues fit in one page; PRs advertise a second one.
	fg.set(func() { fg.prsNext = true })
	if _, err := f.List(context.Background(), "o/r", ""); err == nil || !strings.Contains(err.Error(), "pull requests") {
		t.Fatalf("truncated prs = %v", err)
	}
}

func TestGoGitHubFetcherItems(t *testing.T) {
	fg := newFakeGitHub(t)
	f := NewGoGitHubFetcher(fg.client(t))
	ctx := context.Background()

	is, err := f.Issue(ctx, "o/r", 1)
	if err != nil || is.Open() || !is.Closed {
		t.Fatalf("Issue(1) = %+v %v, want closed", is, err)
	}
	p, err := f.PR(ctx, "o/r", 2)
	if err != nil || !p.Merged || p.MergeableState != "clean" || p.Open() {
		t.Fatalf("PR(2) = %+v %v, want merged", p, err)
	}
	for _, get := range []func() error{
		func() error { _, err := f.Issue(ctx, "o/r", 404); return err },
		func() error { _, err := f.PR(ctx, "o/r", 404); return err },
		func() error { _, err := f.Issue(ctx, "o/r", 410); return err },
	} {
		if err := get(); !errors.Is(err, ErrNotFound) {
			t.Fatalf("missing item err = %v, want ErrNotFound", err)
		}
	}
	if _, err := f.PR(ctx, "o/r", 500); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("500 = %v, want a plain error", err)
	}
	if _, err := f.Issue(ctx, "o/r", 500); err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("500 = %v, want a plain error", err)
	}
}

func TestGoGitHubFetcherBadInput(t *testing.T) {
	ctx := context.Background()
	var nilF *GoGitHubFetcher
	if _, err := nilF.List(ctx, "o/r", ""); !errors.Is(err, ErrNoClient) {
		t.Fatalf("nil fetcher = %v", err)
	}
	f := NewGoGitHubFetcher(nil)
	if _, err := f.Issue(ctx, "o/r", 1); !errors.Is(err, ErrNoClient) {
		t.Fatalf("nil client = %v", err)
	}
	fg := newFakeGitHub(t)
	f = NewGoGitHubFetcher(fg.client(t))
	for _, repo := range []string{"", "o", "o/", "/r", "o/r/x"} {
		if _, err := f.List(ctx, repo, ""); err == nil {
			t.Errorf("List(%q) = nil error", repo)
		}
		if _, err := f.Issue(ctx, repo, 1); err == nil {
			t.Errorf("Issue(%q) = nil error", repo)
		}
		if _, err := f.PR(ctx, repo, 1); err == nil {
			t.Errorf("PR(%q) = nil error", repo)
		}
	}
	if _, err := f.List(ctx, "o/missing", ""); err == nil {
		t.Fatal("List of a failing repo must error")
	}
}

// Acceptance: a 403 in the middle of a real reconcile (issues listed, PR
// list refused) keeps the previous snapshot and marks it stale.
func TestReconcile403MidListThroughGoGitHub(t *testing.T) {
	fg := newFakeGitHub(t)
	s, err := New(Options{
		Repos:   []string{"o/r"},
		Fetcher: NewGoGitHubFetcher(fg.client(t)),
		Metrics: NewCounters(),
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Reconcile(ctx, "o/r"); err != nil {
		t.Fatal(err)
	}
	fg.set(func() { fg.failPRs = http.StatusForbidden })
	if err := s.Reconcile(ctx, "o/r"); err == nil {
		t.Fatal("want the 403")
	}
	snap, _ := s.Snapshot("o/r")
	if !snap.Stale || snap.IssueCount() != 1 || snap.PRCount() != 1 {
		t.Fatalf("snapshot after 403 = stale %v, %d issues, %d prs; want stale with the last good 1/1", snap.Stale, snap.IssueCount(), snap.PRCount())
	}
}
