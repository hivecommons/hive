package forge

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// newTestGitLab builds a gitLabForge pointed at a test server.
func newTestGitLab(t *testing.T, serverURL, org string) *gitLabForge {
	t.Helper()
	f, err := newGitLabForge("test-token", Options{BaseURL: serverURL, Org: org})
	if err != nil {
		t.Fatalf("newGitLabForge: %v", err)
	}
	return f
}

const cankedProjectJSON = `{
	"id": 42,
	"path": "hive",
	"path_with_namespace": "hivecommons/hive",
	"web_url": "https://gitlab.com/hivecommons/hive",
	"default_branch": "main",
	"description": "Fleet automation",
	"namespace": {"full_path": "hivecommons", "path": "hivecommons"}
}`

const cannedIssuesJSON = `[
	{
		"iid": 7,
		"title": "Fix the widget",
		"state": "opened",
		"labels": ["kind/bug", "priority/high"],
		"web_url": "https://gitlab.com/hivecommons/hive/-/issues/7",
		"created_at": "2026-07-01T12:00:00Z",
		"author": {"username": "alice"},
		"assignees": [{"username": "bob"}, {"username": "carol"}]
	},
	{
		"iid": 8,
		"title": "Add docs",
		"state": "opened",
		"labels": [],
		"web_url": "https://gitlab.com/hivecommons/hive/-/issues/8",
		"created_at": "2026-07-02T09:30:00Z",
		"author": {"username": "dave"},
		"assignees": []
	}
]`

const cannedMRsJSON = `[
	{
		"iid": 101,
		"title": "Implement forge abstraction",
		"state": "opened",
		"labels": ["enhancement"],
		"web_url": "https://gitlab.com/hivecommons/hive/-/merge_requests/101",
		"created_at": "2026-07-05T08:00:00Z",
		"draft": false,
		"work_in_progress": false,
		"source_branch": "feat-forge",
		"target_branch": "main",
		"sha": "abc123def",
		"author": {"username": "erin"}
	},
	{
		"iid": 102,
		"title": "WIP: refactor",
		"state": "opened",
		"labels": [],
		"web_url": "https://gitlab.com/hivecommons/hive/-/merge_requests/102",
		"created_at": "2026-07-06T10:15:00Z",
		"draft": true,
		"work_in_progress": true,
		"source_branch": "refactor",
		"target_branch": "main",
		"sha": "def456",
		"author": {"username": "frank"}
	}
]`

func TestGitLabGetRepo(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(gitLabTokenHeader)
		// RequestURI preserves the wire-level percent-encoding; r.URL.Path is
		// already decoded by net/http and would hide the %2F.
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cankedProjectJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	repo, err := f.GetRepo(context.Background(), "hive")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}

	if gotAuth != "test-token" {
		t.Errorf("PRIVATE-TOKEN header = %q, want test-token", gotAuth)
	}
	// Bare "hive" should be namespaced with org and URL-encoded as one segment.
	if !strings.Contains(gotPath, "/api/v4/projects/") {
		t.Errorf("path = %q, want /api/v4/projects/...", gotPath)
	}
	if !strings.Contains(gotPath, "hivecommons%2Fhive") {
		t.Errorf("path = %q, want URL-encoded hivecommons%%2Fhive", gotPath)
	}

	if repo.FullName != "hivecommons/hive" {
		t.Errorf("FullName = %q", repo.FullName)
	}
	if repo.Owner != "hivecommons" {
		t.Errorf("Owner = %q", repo.Owner)
	}
	if repo.Name != "hive" {
		t.Errorf("Name = %q", repo.Name)
	}
	if repo.DefaultBranch != "main" {
		t.Errorf("DefaultBranch = %q", repo.DefaultBranch)
	}
	if repo.URL != "https://gitlab.com/hivecommons/hive" {
		t.Errorf("URL = %q", repo.URL)
	}
	if repo.Description != "Fleet automation" {
		t.Errorf("Description = %q", repo.Description)
	}
}

func TestGitLabListOpenIssues(t *testing.T) {
	var gotState string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotState = r.URL.Query().Get("state")
		w.Header().Set("Content-Type", "application/json")
		// No X-Next-Page header => single page.
		_, _ = w.Write([]byte(cannedIssuesJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}

	if gotState != "opened" {
		t.Errorf("state query = %q, want opened", gotState)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2", len(issues))
	}

	i0 := issues[0]
	if i0.Number != 7 || i0.Title != "Fix the widget" || i0.Author != "alice" {
		t.Errorf("issue[0] = %+v", i0)
	}
	if i0.State != "opened" {
		t.Errorf("issue[0].State = %q", i0.State)
	}
	if len(i0.Labels) != 2 || i0.Labels[0] != "kind/bug" {
		t.Errorf("issue[0].Labels = %v", i0.Labels)
	}
	if len(i0.Assignees) != 2 || i0.Assignees[0] != "bob" {
		t.Errorf("issue[0].Assignees = %v", i0.Assignees)
	}
	if i0.Repo != "hivecommons/hive" {
		t.Errorf("issue[0].Repo = %q", i0.Repo)
	}
	if i0.CreatedAt.IsZero() {
		t.Error("issue[0].CreatedAt is zero")
	}

	// Empty labels must map to a non-nil slice (guard against nil range/join).
	if issues[1].Labels == nil {
		t.Error("issue[1].Labels should be non-nil empty slice")
	}
	if len(issues[1].Assignees) != 0 {
		t.Errorf("issue[1].Assignees = %v, want empty", issues[1].Assignees)
	}
}

func TestGitLabListOpenChangeRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/merge_requests") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cannedMRsJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	crs, err := f.ListOpenChangeRequests(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("got %d change requests, want 2", len(crs))
	}

	c0 := crs[0]
	if c0.Number != 101 || c0.Title != "Implement forge abstraction" || c0.Author != "erin" {
		t.Errorf("cr[0] = %+v", c0)
	}
	if c0.Draft {
		t.Error("cr[0].Draft should be false")
	}
	if c0.SourceBranch != "feat-forge" || c0.TargetBranch != "main" {
		t.Errorf("cr[0] branches = %q -> %q", c0.SourceBranch, c0.TargetBranch)
	}
	if c0.HeadSHA != "abc123def" {
		t.Errorf("cr[0].HeadSHA = %q", c0.HeadSHA)
	}

	// work_in_progress must be treated as draft.
	if !crs[1].Draft {
		t.Error("cr[1] (work_in_progress) should map to Draft=true")
	}
}

func TestGitLabPagination(t *testing.T) {
	// Two pages: first returns one issue with X-Next-Page: 2; second returns
	// one issue with no next page.
	page1 := `[{"iid":1,"title":"one","state":"opened","author":{"username":"a"}}]`
	page2 := `[{"iid":2,"title":"two","state":"opened","author":{"username":"b"}}]`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write([]byte(page1))
		case "2":
			// no X-Next-Page => last page
			_, _ = w.Write([]byte(page2))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != 2 {
		t.Fatalf("got %d issues across pages, want 2", len(issues))
	}
	if issues[0].Number != 1 || issues[1].Number != 2 {
		t.Errorf("paginated issues = %d, %d", issues[0].Number, issues[1].Number)
	}
}

func TestGitLabErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	_, err := f.GetRepo(context.Background(), "kubestellar/missing")
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error should mention status: %v", err)
	}
}

// TestGitLabNewForgeBaseURL exercises base-URL normalization: the public
// default, self-managed with and without a trailing slash, and an invalid URL.
func TestGitLabNewForgeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr bool
	}{
		{"empty defaults to gitlab.com", "", defaultGitLabBaseURL, false},
		{"self-managed no trailing slash", "https://gitlab.example.com", "https://gitlab.example.com", false},
		{"self-managed trailing slash trimmed", "https://gitlab.example.com/", "https://gitlab.example.com", false},
		{"self-managed multiple trailing slashes trimmed", "https://gitlab.example.com///", "https://gitlab.example.com", false},
		{"gitlab.com explicit", "https://gitlab.com", "https://gitlab.com", false},
		{"invalid url errors", "://missing-scheme", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := newGitLabForge("test-token", Options{BaseURL: tt.baseURL})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for base URL %q, got nil", tt.baseURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("newGitLabForge(%q): %v", tt.baseURL, err)
			}
			if f.baseURL != tt.want {
				t.Errorf("baseURL = %q, want %q", f.baseURL, tt.want)
			}
		})
	}
}

// TestGitLabAPIPathAppended asserts the /api/v4 path is appended to the instance
// root exactly once, regardless of a trailing slash on the base URL.
func TestGitLabAPIPathAppended(t *testing.T) {
	for _, base := range []string{"", "/"} {
		var gotURI string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			gotURI = r.RequestURI
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(cankedProjectJSON))
		}))
		// Point the adapter at the test server, optionally with a trailing slash.
		f := newTestGitLab(t, srv.URL+base, "hivecommons")
		if _, err := f.GetRepo(context.Background(), "hive"); err != nil {
			srv.Close()
			t.Fatalf("GetRepo (base suffix %q): %v", base, err)
		}
		srv.Close()
		if !strings.HasPrefix(gotURI, gitLabAPIPath+"/projects/") {
			t.Errorf("base suffix %q: request URI = %q, want prefix %q/projects/", base, gotURI, gitLabAPIPath)
		}
		if strings.Contains(gotURI, gitLabAPIPath+gitLabAPIPath) {
			t.Errorf("base suffix %q: /api/v4 doubled in %q", base, gotURI)
		}
	}
}

// TestGitLabNestedNamespaceEncoding verifies a nested-namespace slug
// (group/subgroup/repo) is URL-encoded as a single path segment on the wire.
func TestGitLabNestedNamespaceEncoding(t *testing.T) {
	var gotURI string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotURI = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cankedProjectJSON))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "")
	if _, err := f.GetRepo(context.Background(), "group/subgroup/repo"); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	// All slashes in the slug must be percent-encoded (%2F), leaving exactly one
	// literal slash separating /projects/ from the encoded segment.
	if !strings.Contains(gotURI, "/projects/group%2Fsubgroup%2Frepo") {
		t.Errorf("request URI = %q, want encoded group%%2Fsubgroup%%2Frepo", gotURI)
	}
}

// TestGitLabErrorStatuses covers the range of non-2xx responses across all read
// operations: each must return an error mentioning the status, never panic.
func TestGitLabErrorStatuses(t *testing.T) {
	statuses := []int{
		http.StatusUnauthorized,        // 401
		http.StatusForbidden,           // 403
		http.StatusNotFound,            // 404
		http.StatusInternalServerError, // 500
	}
	for _, status := range statuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()

			f := newTestGitLab(t, srv.URL, "hivecommons")
			ctx := context.Background()

			if _, err := f.GetRepo(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("GetRepo: expected error for status %d", status)
			} else if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("GetRepo error should mention %d: %v", status, err)
			}
			if _, err := f.ListOpenIssues(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenIssues: expected error for status %d", status)
			}
			if _, err := f.ListOpenChangeRequests(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenChangeRequests: expected error for status %d", status)
			}
		})
	}
}

// TestGitLabMalformedJSON ensures a malformed or empty response body surfaces a
// decode error rather than panicking, on every read operation.
func TestGitLabMalformedJSON(t *testing.T) {
	bodies := map[string]string{
		"garbage":         "not json at all",
		"empty":           "",
		"truncated array": "[{",
		"wrong type":      `{"not":"an array"}`,
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(body))
			}))
			defer srv.Close()

			f := newTestGitLab(t, srv.URL, "hivecommons")
			ctx := context.Background()

			// GetRepo decodes an object; list ops decode arrays. "wrong type"
			// (a valid object) only breaks the array decoders, so skip GetRepo
			// there. Every other body must break all three.
			if name != "wrong type" {
				if _, err := f.GetRepo(ctx, "hivecommons/hive"); err == nil {
					t.Errorf("GetRepo: expected decode error for %q body", name)
				}
			}
			if _, err := f.ListOpenIssues(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenIssues: expected decode error for %q body", name)
			}
			if _, err := f.ListOpenChangeRequests(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenChangeRequests: expected decode error for %q body", name)
			}
		})
	}
}

// TestGitLabPaginationCap asserts the pagination loop stops after
// gitLabMaxPages even when the server always advertises a further page, and
// that per_page is set on every request.
func TestGitLabPaginationCap(t *testing.T) {
	var requests int
	var maxPageSeen int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Query().Get("per_page") != strconv.Itoa(gitLabPerPage) {
			t.Errorf("per_page = %q, want %d", r.URL.Query().Get("per_page"), gitLabPerPage)
		}
		if p, _ := strconv.Atoi(r.URL.Query().Get("page")); p > maxPageSeen {
			maxPageSeen = p
		}
		w.Header().Set("Content-Type", "application/json")
		// Always advertise another page: without the cap this would loop forever.
		w.Header().Set("X-Next-Page", strconv.Itoa(1000))
		_, _ = w.Write([]byte(`[{"iid":1,"title":"t","state":"opened","author":{"username":"a"}}]`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if requests != gitLabMaxPages {
		t.Errorf("made %d requests, want exactly gitLabMaxPages=%d", requests, gitLabMaxPages)
	}
	if len(issues) != gitLabMaxPages {
		t.Errorf("collected %d issues, want %d (one per capped page)", len(issues), gitLabMaxPages)
	}
}

// TestGitLabPaginationBadNextPage ensures a non-numeric X-Next-Page header is
// treated as "no more pages" (nextPage stays 0) rather than erroring.
func TestGitLabPaginationBadNextPage(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Next-Page", "not-a-number")
		_, _ = w.Write([]byte(`[{"iid":1,"title":"t","state":"opened","author":{"username":"a"}}]`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if requests != 1 {
		t.Errorf("made %d requests, want 1 (bad X-Next-Page => stop)", requests)
	}
	if len(issues) != 1 {
		t.Errorf("collected %d issues, want 1", len(issues))
	}
}

// TestGitLabGetRepoFallbacks covers the GetRepo field-derivation fallbacks used
// when the project JSON omits path_with_namespace / path / namespace.full_path:
// the code derives them from the requested slug instead.
func TestGitLabGetRepoFallbacks(t *testing.T) {
	// Minimal project JSON: only id + web_url, everything else empty.
	const sparse = `{"id":1,"web_url":"https://gitlab.example.com/group/sub/proj"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(sparse))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "")
	repo, err := f.GetRepo(context.Background(), "group/sub/proj")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.FullName != "group/sub/proj" {
		t.Errorf("FullName fallback = %q, want group/sub/proj", repo.FullName)
	}
	if repo.Name != "proj" {
		t.Errorf("Name fallback = %q, want proj (last segment)", repo.Name)
	}
	if repo.Owner != "group/sub" {
		t.Errorf("Owner fallback = %q, want group/sub (everything before last slash)", repo.Owner)
	}
}

// TestGitLabReadBodyError exercises the getRaw read-error branch: a 2xx status
// whose body is truncated below its advertised Content-Length, so io.ReadAll
// fails with an unexpected EOF.
func TestGitLabReadBodyError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		// Advertise 1000 bytes but send only a few, then slam the connection.
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort"))
		_ = conn.Close()
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	_, err := f.GetRepo(context.Background(), "hivecommons/hive")
	if err == nil {
		t.Fatal("expected read error for truncated body, got nil")
	}
	if !strings.Contains(err.Error(), "reading body") {
		t.Errorf("error should mention reading body: %v", err)
	}
}

// TestGitLabRequestBuildError forces http.NewRequestWithContext to fail by
// giving the adapter a base URL containing a control character, which is
// invalid in a request target.
func TestGitLabRequestBuildError(t *testing.T) {
	f := &gitLabForge{
		baseURL: "http://example.com/\x7f", // DEL control char is not a valid URL byte
		http:    &http.Client{},
	}
	_, err := f.GetRepo(context.Background(), "a/b")
	if err == nil {
		t.Fatal("expected request-build error, got nil")
	}
}

// TestGitLabNoTokenNoHeader confirms an empty token means the PRIVATE-TOKEN
// header is not sent (unauthenticated public reads), while a token is sent.
func TestGitLabTokenHeaderPresence(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		wantHeader bool
	}{
		{"with token", "test-token", true},
		{"empty token", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var sawHeader bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// http.Header canonicalizes keys, so use Get which handles that.
				sawHeader = r.Header.Get(gitLabTokenHeader) != ""
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(cankedProjectJSON))
			}))
			defer srv.Close()

			f, err := newGitLabForge(tt.token, Options{BaseURL: srv.URL, Org: "hivecommons"})
			if err != nil {
				t.Fatalf("newGitLabForge: %v", err)
			}
			if _, err := f.GetRepo(context.Background(), "hive"); err != nil {
				t.Fatalf("GetRepo: %v", err)
			}
			if sawHeader != tt.wantHeader {
				t.Errorf("PRIVATE-TOKEN header present = %v, want %v", sawHeader, tt.wantHeader)
			}
		})
	}
}

// TestSplitRepoLastAndOwnerLast covers the slug-splitting helpers, including the
// no-slash edge case.
func TestSplitRepoLastAndOwnerLast(t *testing.T) {
	tests := []struct {
		full      string
		wantOwner string
		wantName  string
	}{
		{"group/sub/proj", "group/sub", "proj"},
		{"owner/repo", "owner", "repo"},
		{"noslash", "", "noslash"},
		{"", "", ""},
		{"trailing/", "trailing", ""},
	}
	for _, tt := range tests {
		t.Run(tt.full, func(t *testing.T) {
			owner, name := splitRepoLast(tt.full)
			if owner != tt.wantOwner || name != tt.wantName {
				t.Errorf("splitRepoLast(%q) = %q,%q want %q,%q", tt.full, owner, name, tt.wantOwner, tt.wantName)
			}
			// splitOwnerLast delegates to splitRepoLast; owner must agree.
			gotOwner, _ := splitOwnerLast(tt.full)
			if gotOwner != tt.wantOwner {
				t.Errorf("splitOwnerLast(%q) owner = %q, want %q", tt.full, gotOwner, tt.wantOwner)
			}
		})
	}
}

// TestTruncate covers both branches of truncate: short strings pass through
// unchanged, long strings are cut with an ellipsis.
func TestTruncate(t *testing.T) {
	if got := truncate("short", 200); got != "short" {
		t.Errorf("truncate short = %q, want unchanged", got)
	}
	long := strings.Repeat("x", 250)
	got := truncate(long, 200)
	if len(got) != 203 { // 200 chars + "..."
		t.Errorf("truncated length = %d, want 203", len(got))
	}
	if !strings.HasSuffix(got, "...") {
		t.Errorf("truncated string should end with ..., got %q", got[len(got)-5:])
	}
}

// TestNormalizeLabels covers the nil and non-nil label guard paths directly.
func TestNormalizeLabels(t *testing.T) {
	if got := normalizeLabels(nil); got == nil || len(got) != 0 {
		t.Errorf("normalizeLabels(nil) = %v, want non-nil empty slice", got)
	}
	in := []string{"a", "b"}
	if got := normalizeLabels(in); len(got) != 2 || got[0] != "a" {
		t.Errorf("normalizeLabels(%v) = %v, want passthrough", in, got)
	}
}

// TestGitLabTruncatedErrorBody ensures a very long error body is truncated in
// the returned error message (exercises truncate via getRaw's error path).
func TestGitLabTruncatedErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(strings.Repeat("E", 500)))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	_, err := f.GetRepo(context.Background(), "hivecommons/hive")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("long error body should be truncated with ...: %v", err)
	}
}

func TestGitLabProjectSlug(t *testing.T) {
	tests := []struct {
		name string
		org  string
		repo string
		want string
	}{
		{"bare name with org", "hivecommons", "hive", "hivecommons/hive"},
		{"already namespaced", "hivecommons", "other/hive", "other/hive"},
		{"nested namespace", "hivecommons", "group/sub/proj", "group/sub/proj"},
		{"bare name no org", "", "hive", "hive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &gitLabForge{org: tt.org}
			if got := f.projectSlug(tt.repo); got != tt.want {
				t.Errorf("projectSlug(%q) = %q, want %q", tt.repo, got, tt.want)
			}
		})
	}
}

// --- Write-path tests ---

// TestGitLabCreateIssueComment verifies the note-create request: method, path,
// PRIVATE-TOKEN header, JSON body, and success on 201.
func TestGitLabCreateIssueComment(t *testing.T) {
	var gotMethod, gotAuth, gotCT, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotAuth = r.Header.Get(gitLabTokenHeader)
		gotCT = r.Header.Get("Content-Type")
		gotPath = r.RequestURI
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	if err := f.CreateIssueComment(context.Background(), "hive", 7, "hello world"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != "test-token" {
		t.Errorf("PRIVATE-TOKEN = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if !strings.Contains(gotPath, "hivecommons%2Fhive/issues/7/notes") {
		t.Errorf("path = %q, want .../issues/7/notes", gotPath)
	}
	if !strings.Contains(gotBody, `"body":"hello world"`) {
		t.Errorf("body = %q, want body field", gotBody)
	}
}

// TestGitLabAddLabels verifies the PUT with add_labels, and the empty-slice
// no-op (no request made).
func TestGitLabAddLabels(t *testing.T) {
	var requests int
	var gotMethod, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotMethod = r.Method
		gotPath = r.RequestURI
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"iid":7}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")

	// Empty labels => no-op, no HTTP call.
	if err := f.AddLabels(context.Background(), "hive", 7, nil); err != nil {
		t.Fatalf("AddLabels(empty): %v", err)
	}
	if requests != 0 {
		t.Errorf("empty AddLabels made %d requests, want 0", requests)
	}

	if err := f.AddLabels(context.Background(), "hive", 7, []string{"triage/accepted", "kind/bug"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if !strings.Contains(gotPath, "hivecommons%2Fhive/issues/7") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"add_labels":"triage/accepted,kind/bug"`) {
		t.Errorf("body = %q, want comma-joined add_labels", gotBody)
	}
}

// TestGitLabRemoveLabel verifies the PUT with remove_labels.
func TestGitLabRemoveLabel(t *testing.T) {
	var gotMethod, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"iid":7}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")
	if err := f.RemoveLabel(context.Background(), "hive", 7, "hold"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if gotMethod != http.MethodPut {
		t.Errorf("method = %q, want PUT", gotMethod)
	}
	if !strings.Contains(gotBody, `"remove_labels":"hold"`) {
		t.Errorf("body = %q, want remove_labels", gotBody)
	}
}

// TestGitLabSetHold verifies SetHold(true) adds the hold label and SetHold(false)
// removes it, hitting the right verbs/bodies.
func TestGitLabSetHold(t *testing.T) {
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"iid":7}`))
	}))
	defer srv.Close()

	f := newTestGitLab(t, srv.URL, "hivecommons")

	if err := f.SetHold(context.Background(), "hive", 7, true); err != nil {
		t.Fatalf("SetHold(true): %v", err)
	}
	if !strings.Contains(gotBody, `"add_labels":"hold"`) {
		t.Errorf("SetHold(true) body = %q, want add_labels hold", gotBody)
	}

	if err := f.SetHold(context.Background(), "hive", 7, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}
	if !strings.Contains(gotBody, `"remove_labels":"hold"`) {
		t.Errorf("SetHold(false) body = %q, want remove_labels hold", gotBody)
	}
}

// TestGitLabWriteErrorStatus verifies non-2xx responses on each write op surface
// an error mentioning the status.
func TestGitLabWriteErrorStatus(t *testing.T) {
	statuses := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}
	for _, status := range statuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()

			f := newTestGitLab(t, srv.URL, "hivecommons")
			ctx := context.Background()

			if err := f.CreateIssueComment(ctx, "hive", 7, "x"); err == nil {
				t.Errorf("CreateIssueComment: expected error for %d", status)
			} else if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("error should mention %d: %v", status, err)
			}
			if err := f.AddLabels(ctx, "hive", 7, []string{"x"}); err == nil {
				t.Errorf("AddLabels: expected error for %d", status)
			}
			if err := f.RemoveLabel(ctx, "hive", 7, "x"); err == nil {
				t.Errorf("RemoveLabel: expected error for %d", status)
			}
		})
	}
}

// TestGitLabWriteRequestBuildError forces http.NewRequestWithContext to fail via
// a control character in the base URL, covering the doWrite build-error branch.
func TestGitLabWriteRequestBuildError(t *testing.T) {
	f := &gitLabForge{
		baseURL: "http://example.com/\x7f",
		http:    &http.Client{},
	}
	if err := f.CreateIssueComment(context.Background(), "a/b", 1, "x"); err == nil {
		t.Fatal("expected request-build error, got nil")
	}
}

// TestGitLabWriteNoToken verifies the PRIVATE-TOKEN header is omitted on writes
// when no token is configured.
func TestGitLabWriteNoToken(t *testing.T) {
	var sawHeader bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHeader = r.Header.Get(gitLabTokenHeader) != ""
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()

	f, err := newGitLabForge("", Options{BaseURL: srv.URL, Org: "hivecommons"})
	if err != nil {
		t.Fatalf("newGitLabForge: %v", err)
	}
	if err := f.CreateIssueComment(context.Background(), "hive", 7, "x"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if sawHeader {
		t.Error("PRIVATE-TOKEN header should be absent with empty token")
	}
}
