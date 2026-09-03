package forge

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// newTestGitea builds a giteaForge pointed at a test server.
func newTestGitea(t *testing.T, serverURL, org string) *giteaForge {
	t.Helper()
	f, err := newGiteaForge("test-token", Options{BaseURL: serverURL, Org: org})
	if err != nil {
		t.Fatalf("newGiteaForge: %v", err)
	}
	return f
}

const giteaRepoJSON = `{
	"name": "hive",
	"full_name": "hivecommons/hive",
	"html_url": "https://gitea.example.com/hivecommons/hive",
	"default_branch": "main",
	"description": "Fleet automation",
	"owner": {"login": "hivecommons"}
}`

const giteaIssuesJSON = `[
	{
		"number": 7,
		"title": "Fix the widget",
		"state": "open",
		"labels": [{"name": "kind/bug"}, {"name": "priority/high"}],
		"html_url": "https://gitea.example.com/hivecommons/hive/issues/7",
		"created_at": "2026-07-01T12:00:00Z",
		"user": {"login": "alice"},
		"assignees": [{"login": "bob"}, {"login": "carol"}]
	},
	{
		"number": 8,
		"title": "Add docs",
		"state": "open",
		"labels": [],
		"html_url": "https://gitea.example.com/hivecommons/hive/issues/8",
		"created_at": "2026-07-02T09:30:00Z",
		"user": {"login": "dave"},
		"assignees": []
	},
	{
		"number": 9,
		"title": "A PR masquerading as an issue",
		"state": "open",
		"labels": [],
		"user": {"login": "erin"},
		"pull_request": {"merged": false}
	}
]`

const giteaPullsJSON = `[
	{
		"number": 101,
		"title": "Implement forge abstraction",
		"state": "open",
		"labels": [{"name": "enhancement"}],
		"html_url": "https://gitea.example.com/hivecommons/hive/pulls/101",
		"created_at": "2026-07-05T08:00:00Z",
		"draft": false,
		"user": {"login": "frank"},
		"head": {"ref": "feat-forge", "sha": "abc123def"},
		"base": {"ref": "main", "sha": "base000"}
	},
	{
		"number": 102,
		"title": "WIP refactor",
		"state": "open",
		"labels": [],
		"html_url": "https://gitea.example.com/hivecommons/hive/pulls/102",
		"created_at": "2026-07-06T10:15:00Z",
		"draft": true,
		"user": {"login": "grace"},
		"head": {"ref": "refactor", "sha": "def456"},
		"base": {"ref": "main", "sha": "base111"}
	}
]`

// --- Factory ---

func TestNewForgeGitea(t *testing.T) {
	f, err := NewForge(KindGitea, "token", Options{BaseURL: "https://gitea.example.com", Org: "acme"})
	if err != nil {
		t.Fatalf("NewForge(gitea): %v", err)
	}
	if f.Kind() != KindGitea {
		t.Errorf("Kind() = %q, want %q", f.Kind(), KindGitea)
	}
}

func TestNewForgeGiteaMissingBaseURL(t *testing.T) {
	_, err := NewForge(KindGitea, "token", Options{Org: "acme"})
	if err == nil {
		t.Fatal("expected error for missing Gitea base URL, got nil")
	}
}

// TestNewGiteaForgeBaseURL exercises base-URL handling: required, trailing-slash
// trimming, and invalid/relative URLs.
func TestNewGiteaForgeBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL string
		want    string
		wantErr bool
	}{
		{"empty errors", "", "", true},
		{"absolute ok", "https://gitea.example.com", "https://gitea.example.com", false},
		{"trailing slash trimmed", "https://gitea.example.com/", "https://gitea.example.com", false},
		{"multiple trailing slashes trimmed", "https://gitea.example.com///", "https://gitea.example.com", false},
		{"relative errors (no host)", "gitea.example.com", "", true},
		{"invalid url errors", "://missing-scheme", "", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f, err := newGiteaForge("test-token", Options{BaseURL: tt.baseURL})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error for %q, got nil", tt.baseURL)
				}
				return
			}
			if err != nil {
				t.Fatalf("newGiteaForge(%q): %v", tt.baseURL, err)
			}
			if f.baseURL != tt.want {
				t.Errorf("baseURL = %q, want %q", f.baseURL, tt.want)
			}
		})
	}
}

// --- Read path ---

func TestGiteaGetRepo(t *testing.T) {
	var gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get(giteaAuthHeader)
		gotPath = r.RequestURI
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(giteaRepoJSON))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	repo, err := f.GetRepo(context.Background(), "hive")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if gotAuth != giteaTokenScheme+"test-token" {
		t.Errorf("Authorization = %q, want %q", gotAuth, giteaTokenScheme+"test-token")
	}
	if !strings.HasPrefix(gotPath, giteaAPIPath+"/repos/hivecommons/hive") {
		t.Errorf("path = %q, want %s/repos/hivecommons/hive", gotPath, giteaAPIPath)
	}
	if repo.FullName != "hivecommons/hive" || repo.Owner != "hivecommons" || repo.Name != "hive" {
		t.Errorf("repo = %+v", repo)
	}
	if repo.DefaultBranch != "main" || repo.URL == "" || repo.Description != "Fleet automation" {
		t.Errorf("repo fields = %+v", repo)
	}
}

// TestGiteaGetRepoFallbacks covers deriving FullName/Owner/Name from the request
// slug when the response omits them.
func TestGiteaGetRepoFallbacks(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"html_url":"https://gitea.example.com/o/p"}`))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "")
	repo, err := f.GetRepo(context.Background(), "myorg/myrepo")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.FullName != "myorg/myrepo" {
		t.Errorf("FullName fallback = %q", repo.FullName)
	}
	if repo.Owner != "myorg" || repo.Name != "myrepo" {
		t.Errorf("owner/name fallback = %q/%q", repo.Owner, repo.Name)
	}
}

// TestGiteaBareRepoUsesOrg verifies a bare repo name is prefixed with the org.
func TestGiteaBareRepoUsesOrg(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(giteaRepoJSON))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	if _, err := f.GetRepo(context.Background(), "hive"); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if !strings.Contains(gotPath, "/repos/hivecommons/hive") {
		t.Errorf("path = %q, want /repos/hivecommons/hive", gotPath)
	}
}

func TestGiteaListOpenIssues(t *testing.T) {
	var gotState, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/issues") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		gotState = r.URL.Query().Get("state")
		gotType = r.URL.Query().Get("type")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(giteaIssuesJSON))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if gotState != giteaStateOpen {
		t.Errorf("state = %q, want open", gotState)
	}
	if gotType != "issues" {
		t.Errorf("type = %q, want issues", gotType)
	}
	// Three entries in JSON but one is a pull_request => filtered to 2.
	if len(issues) != 2 {
		t.Fatalf("got %d issues, want 2 (PR filtered out)", len(issues))
	}
	i0 := issues[0]
	if i0.Number != 7 || i0.Title != "Fix the widget" || i0.Author != "alice" || i0.State != "open" {
		t.Errorf("issue[0] = %+v", i0)
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
	// Empty labels must map to a non-nil slice.
	if issues[1].Labels == nil {
		t.Error("issue[1].Labels should be non-nil empty slice")
	}
}

func TestGiteaListOpenChangeRequests(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/pulls") {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(giteaPullsJSON))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	crs, err := f.ListOpenChangeRequests(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenChangeRequests: %v", err)
	}
	if len(crs) != 2 {
		t.Fatalf("got %d change requests, want 2", len(crs))
	}
	c0 := crs[0]
	if c0.Number != 101 || c0.Title != "Implement forge abstraction" || c0.Author != "frank" {
		t.Errorf("cr[0] = %+v", c0)
	}
	if c0.Draft {
		t.Error("cr[0].Draft should be false")
	}
	if c0.SourceBranch != "feat-forge" || c0.TargetBranch != "main" || c0.HeadSHA != "abc123def" {
		t.Errorf("cr[0] branches/sha = %q -> %q / %q", c0.SourceBranch, c0.TargetBranch, c0.HeadSHA)
	}
	if !crs[1].Draft {
		t.Error("cr[1] (draft) should map to Draft=true")
	}
}

// TestGiteaPagination verifies the loop pages until a short page arrives.
func TestGiteaPagination(t *testing.T) {
	// Build a full first page (giteaPerPage items) then a short second page.
	fullPage := makeGiteaIssuePage(1, giteaPerPage)
	shortPage := makeGiteaIssuePage(giteaPerPage+1, 3)
	var pages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("limit") != strconv.Itoa(giteaPerPage) {
			t.Errorf("limit = %q, want %d", r.URL.Query().Get("limit"), giteaPerPage)
		}
		pages = append(pages, r.URL.Query().Get("page"))
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("page") {
		case "1":
			_, _ = w.Write([]byte(fullPage))
		case "2":
			_, _ = w.Write([]byte(shortPage))
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	issues, err := f.ListOpenIssues(context.Background(), "hivecommons/hive")
	if err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if len(issues) != giteaPerPage+3 {
		t.Fatalf("got %d issues across pages, want %d", len(issues), giteaPerPage+3)
	}
	if len(pages) != 2 {
		t.Errorf("requested %d pages, want 2 (stop on short page)", len(pages))
	}
}

// TestGiteaPaginationCap asserts the loop stops after giteaMaxPages even when
// the server always returns full pages.
func TestGiteaPaginationCap(t *testing.T) {
	fullPage := makeGiteaIssuePage(1, giteaPerPage)
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(fullPage)) // always full => would loop without the cap
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	if _, err := f.ListOpenIssues(context.Background(), "hivecommons/hive"); err != nil {
		t.Fatalf("ListOpenIssues: %v", err)
	}
	if requests != giteaMaxPages {
		t.Errorf("made %d requests, want giteaMaxPages=%d", requests, giteaMaxPages)
	}
}

// TestGiteaReadErrorStatuses covers non-2xx responses across all read ops.
func TestGiteaReadErrorStatuses(t *testing.T) {
	statuses := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusInternalServerError}
	for _, status := range statuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()

			f := newTestGitea(t, srv.URL, "hivecommons")
			ctx := context.Background()
			if _, err := f.GetRepo(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("GetRepo: expected error for %d", status)
			} else if !strings.Contains(err.Error(), strconv.Itoa(status)) {
				t.Errorf("GetRepo error should mention %d: %v", status, err)
			}
			if _, err := f.ListOpenIssues(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenIssues: expected error for %d", status)
			}
			if _, err := f.ListOpenChangeRequests(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenChangeRequests: expected error for %d", status)
			}
		})
	}
}

// TestGiteaMalformedJSON ensures malformed bodies surface a decode error.
func TestGiteaMalformedJSON(t *testing.T) {
	bodies := map[string]string{
		"garbage":         "not json",
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

			f := newTestGitea(t, srv.URL, "hivecommons")
			ctx := context.Background()
			if name != "wrong type" {
				if _, err := f.GetRepo(ctx, "hivecommons/hive"); err == nil {
					t.Errorf("GetRepo: expected decode error for %q", name)
				}
			}
			if _, err := f.ListOpenIssues(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenIssues: expected decode error for %q", name)
			}
			if _, err := f.ListOpenChangeRequests(ctx, "hivecommons/hive"); err == nil {
				t.Errorf("ListOpenChangeRequests: expected decode error for %q", name)
			}
		})
	}
}

// TestGiteaReadBodyError exercises the getRaw read-error branch via a truncated
// body under an advertised Content-Length.
func TestGiteaReadBodyError(t *testing.T) {
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
		_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 1000\r\n\r\nshort"))
		_ = conn.Close()
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	_, err := f.GetRepo(context.Background(), "hivecommons/hive")
	if err == nil {
		t.Fatal("expected read error for truncated body")
	}
	if !strings.Contains(err.Error(), "reading body") {
		t.Errorf("error should mention reading body: %v", err)
	}
}

// TestGiteaRequestBuildError forces http.NewRequestWithContext to fail.
func TestGiteaRequestBuildError(t *testing.T) {
	f := &giteaForge{
		baseURL: "http://example.com/\x7f",
		http:    &http.Client{},
	}
	if _, err := f.GetRepo(context.Background(), "a/b"); err == nil {
		t.Fatal("expected request-build error, got nil")
	}
}

// --- Write path ---

func TestGiteaCreateIssueComment(t *testing.T) {
	var gotMethod, gotAuth, gotCT, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotAuth = r.Method, r.Header.Get(giteaAuthHeader)
		gotCT, gotPath = r.Header.Get("Content-Type"), r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":1}`))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	if err := f.CreateIssueComment(context.Background(), "hive", 7, "hello"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if gotAuth != giteaTokenScheme+"test-token" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if gotCT != "application/json" {
		t.Errorf("Content-Type = %q", gotCT)
	}
	if !strings.HasSuffix(gotPath, "/repos/hivecommons/hive/issues/7/comments") {
		t.Errorf("path = %q", gotPath)
	}
	if !strings.Contains(gotBody, `"body":"hello"`) {
		t.Errorf("body = %q", gotBody)
	}
}

func TestGiteaAddLabels(t *testing.T) {
	var requests int
	var gotMethod, gotBody, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		gotMethod, gotPath = r.Method, r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")

	// Empty labels => no-op.
	if err := f.AddLabels(context.Background(), "hive", 7, nil); err != nil {
		t.Fatalf("AddLabels(empty): %v", err)
	}
	if requests != 0 {
		t.Errorf("empty AddLabels made %d requests, want 0", requests)
	}

	if err := f.AddLabels(context.Background(), "hive", 7, []string{"kind/bug", "hold"}); err != nil {
		t.Fatalf("AddLabels: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/repos/hivecommons/hive/issues/7/labels") {
		t.Errorf("path = %q", gotPath)
	}
	// Verify the labels array round-trips.
	var payload struct {
		Labels []string `json:"labels"`
	}
	if err := json.Unmarshal([]byte(gotBody), &payload); err != nil {
		t.Fatalf("body not valid JSON: %v (%q)", err, gotBody)
	}
	if len(payload.Labels) != 2 || payload.Labels[0] != "kind/bug" {
		t.Errorf("labels = %v", payload.Labels)
	}
}

func TestGiteaRemoveLabel(t *testing.T) {
	var gotMethod, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	if err := f.RemoveLabel(context.Background(), "hive", 7, "hold"); err != nil {
		t.Fatalf("RemoveLabel: %v", err)
	}
	if gotMethod != http.MethodDelete {
		t.Errorf("method = %q, want DELETE", gotMethod)
	}
	if !strings.HasSuffix(gotPath, "/repos/hivecommons/hive/issues/7/labels/hold") {
		t.Errorf("path = %q", gotPath)
	}
}

// TestGiteaRemoveLabelNotFoundIsOK verifies a 404 on remove is treated as
// success (idempotent-remove contract).
func TestGiteaRemoveLabelNotFoundIsOK(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"label not found"}`))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")
	if err := f.RemoveLabel(context.Background(), "hive", 7, "absent"); err != nil {
		t.Errorf("RemoveLabel on 404 should be nil, got %v", err)
	}
}

func TestGiteaSetHold(t *testing.T) {
	var lastMethod, lastBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lastMethod = r.Method
		b, _ := io.ReadAll(r.Body)
		lastBody = string(b)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	f := newTestGitea(t, srv.URL, "hivecommons")

	if err := f.SetHold(context.Background(), "hive", 7, true); err != nil {
		t.Fatalf("SetHold(true): %v", err)
	}
	if lastMethod != http.MethodPost || !strings.Contains(lastBody, holdLabel) {
		t.Errorf("SetHold(true) = %s body=%q", lastMethod, lastBody)
	}

	if err := f.SetHold(context.Background(), "hive", 7, false); err != nil {
		t.Fatalf("SetHold(false): %v", err)
	}
	if lastMethod != http.MethodDelete {
		t.Errorf("SetHold(false) method = %q, want DELETE", lastMethod)
	}
}

// TestGiteaWriteErrorStatuses covers non-2xx (excluding the tolerated 404 on
// remove) on the write ops.
func TestGiteaWriteErrorStatuses(t *testing.T) {
	statuses := []int{http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError}
	for _, status := range statuses {
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, _ = w.Write([]byte(`{"message":"boom"}`))
			}))
			defer srv.Close()

			f := newTestGitea(t, srv.URL, "hivecommons")
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

// TestGiteaWriteRequestBuildError covers the doWrite build-error branch.
func TestGiteaWriteRequestBuildError(t *testing.T) {
	f := &giteaForge{baseURL: "http://example.com/\x7f", http: &http.Client{}}
	if err := f.CreateIssueComment(context.Background(), "a/b", 1, "x"); err == nil {
		t.Fatal("expected request-build error, got nil")
	}
}

// TestGiteaNoTokenNoAuthHeader verifies the Authorization header is omitted when
// no token is configured, on both read and write.
func TestGiteaNoTokenNoAuthHeader(t *testing.T) {
	var sawAuth bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(giteaAuthHeader) != "" {
			sawAuth = true
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(giteaRepoJSON))
	}))
	defer srv.Close()

	f, err := newGiteaForge("", Options{BaseURL: srv.URL, Org: "hivecommons"})
	if err != nil {
		t.Fatalf("newGiteaForge: %v", err)
	}
	if _, err := f.GetRepo(context.Background(), "hive"); err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if err := f.CreateIssueComment(context.Background(), "hive", 7, "x"); err != nil {
		t.Fatalf("CreateIssueComment: %v", err)
	}
	if sawAuth {
		t.Error("Authorization header should be absent with empty token")
	}
}

// --- Helper / guard tests ---

func TestGiteaOwnerRepo(t *testing.T) {
	tests := []struct {
		name, org, repo, wantOwner, wantName string
	}{
		{"bare with org", "acme", "widget", "acme", "widget"},
		{"already qualified", "acme", "other/widget", "other", "widget"},
		{"bare no org", "", "widget", "", "widget"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &giteaForge{org: tt.org}
			owner, name := f.ownerRepo(tt.repo)
			if owner != tt.wantOwner || name != tt.wantName {
				t.Errorf("ownerRepo(%q) = %q,%q want %q,%q", tt.repo, owner, name, tt.wantOwner, tt.wantName)
			}
		})
	}
}

func TestLabelNames(t *testing.T) {
	// nil => non-nil empty slice.
	if got := labelNames(nil); got == nil || len(got) != 0 {
		t.Errorf("labelNames(nil) = %v, want non-nil empty", got)
	}
	in := []gtLabel{{Name: "a"}, {Name: ""}, {Name: "b"}}
	got := labelNames(in)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("labelNames(%v) = %v, want [a b] (empty skipped)", in, got)
	}
}

func TestLoginNames(t *testing.T) {
	if got := loginNames(nil); got == nil || len(got) != 0 {
		t.Errorf("loginNames(nil) = %v, want non-nil empty", got)
	}
	in := []gtUser{{Login: "x"}, {Login: ""}, {Login: "y"}}
	got := loginNames(in)
	if len(got) != 2 || got[0] != "x" || got[1] != "y" {
		t.Errorf("loginNames(%v) = %v, want [x y]", in, got)
	}
}

// makeGiteaIssuePage builds a JSON array of n plain issues starting at issue
// number `start`, for pagination tests.
func makeGiteaIssuePage(start, n int) string {
	var b strings.Builder
	b.WriteByte('[')
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"number":%d,"title":"t","state":"open","user":{"login":"a"}}`, start+i)
	}
	b.WriteByte(']')
	return b.String()
}
