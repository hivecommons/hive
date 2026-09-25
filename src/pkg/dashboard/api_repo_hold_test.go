package dashboard

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/github"
)

type repoHoldMock struct {
	permission  map[string]string
	issueLabels []string
	added       []string
	created     []string
	removed     []string
}

func newRepoHoldMockServer(t *testing.T, m *repoHoldMock) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/writer/permission"):
			_, _ = io.WriteString(w, `{"permission":"`+m.permission["writer"]+`"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/collaborators/reader/permission"):
			_, _ = io.WriteString(w, `{"permission":"read"}`)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/issues/7"):
			labels := make([]map[string]string, 0, len(m.issueLabels))
			for _, label := range m.issueLabels {
				labels = append(labels, map[string]string{"name": label})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"number": 7, "labels": labels})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			http.NotFound(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/labels"):
			var labels []string
			_ = json.NewDecoder(r.Body).Decode(&labels)
			m.added = append(m.added, labels...)
			_, _ = io.WriteString(w, `[]`)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/labels"):
			var label map[string]any
			_ = json.NewDecoder(r.Body).Decode(&label)
			if name, _ := label["name"].(string); name != "" {
				m.created = append(m.created, name)
			}
			_ = json.NewEncoder(w).Encode(label)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			part := strings.TrimPrefix(r.URL.EscapedPath(), "/repos/testorg/testrepo/issues/7/labels/")
			first, _ := url.PathUnescape(part)
			second, _ := url.PathUnescape(first)
			m.removed = append(m.removed, second)
			_, _ = io.WriteString(w, `{}`)
		default:
			t.Logf("unexpected GitHub API request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
}

func newRepoHoldServer(t *testing.T, mock *repoHoldMock) *Server {
	t.Helper()
	ghSrv := newRepoHoldMockServer(t, mock)
	t.Cleanup(ghSrv.Close)
	srv := newFullServer(t)
	srv.deps.Config.HiveID = "h1"
	srv.deps.GHClient = github.NewClient("token", "testorg", []string{"testrepo"}, slog.Default(), ghSrv.URL)
	srv.deps.GHClient.SetHoldLabels([]string{github.CanonicalHiveHoldLabel("h1")})
	return srv
}

func postRepoHold(t *testing.T, srv *Server, user string, owner bool, held bool) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"held":` + map[bool]string{true: "true", false: "false"}[held] + `}`
	req := httptest.NewRequest(http.MethodPost, "/api/repos/testorg/testrepo/items/7/hold", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if user != "" {
		req.Header.Set("X-Hive-User", user)
	}
	if owner {
		markOwnerRequest(req)
	}
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	return w
}

func TestRepoItemHoldToggleAuthorizationAndLabels(t *testing.T) {
	mock := &repoHoldMock{
		permission:  map[string]string{"writer": "maintain"},
		issueLabels: []string{"bug", "hold", "hive/h1", "needs-human"},
	}
	srv := newRepoHoldServer(t, mock)

	if w := postRepoHold(t, srv, "", true, true); w.Code != http.StatusOK {
		t.Fatalf("owner add: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.added) != 1 || mock.added[0] != "hive-pause/h1" {
		t.Fatalf("owner add labels = %v, want canonical hive label", mock.added)
	}
	if len(mock.created) != 1 || mock.created[0] != "hive-pause/h1" {
		t.Fatalf("created labels = %v, want canonical hive label ensured", mock.created)
	}

	if w := postRepoHold(t, srv, "writer", false, true); w.Code != http.StatusOK {
		t.Fatalf("writer add: code=%d body=%s", w.Code, w.Body.String())
	}
	if len(mock.added) != 2 || mock.added[1] != "hive-pause/h1" {
		t.Fatalf("writer add labels = %v, want second canonical hive label", mock.added)
	}

	w := postRepoHold(t, srv, "reader", false, true)
	if w.Code != http.StatusForbidden {
		t.Fatalf("reader add: code=%d want 403 body=%s", w.Code, w.Body.String())
	}
	w = postRepoHold(t, srv, "", false, true)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous add: code=%d want 401 body=%s", w.Code, w.Body.String())
	}

	w = postRepoHold(t, srv, "writer", false, false)
	if w.Code != http.StatusOK {
		t.Fatalf("writer remove: code=%d body=%s", w.Code, w.Body.String())
	}
	wantRemoved := "hold"
	if got := strings.Join(mock.removed, ","); got != wantRemoved {
		t.Fatalf("removed labels = %q, want %q", got, wantRemoved)
	}

	entries := srv.GetAudit().Recent(10)
	var addAudit, removeAudit bool
	for _, entry := range entries {
		addAudit = addAudit || entry.Action == "repo_item_hold_add"
		removeAudit = removeAudit || entry.Action == "repo_item_hold_remove"
	}
	if !addAudit || !removeAudit {
		t.Fatalf("audit entries missing add=%v remove=%v entries=%+v", addAudit, removeAudit, entries)
	}
}

func TestRepoItemHoldToggleUpdatesStatusSnapshotAndReturnsFloor(t *testing.T) {
	mock := &repoHoldMock{permission: map[string]string{"writer": "maintain"}}
	srv := newRepoHoldServer(t, mock)
	srv.status = &StatusPayload{
		StatusSeq: 3,
		Repos: []FrontendRepo{{
			Name: "testrepo",
			Full: "testorg/testrepo",
			WorkBreakdown: &github.RepoWorkBreakdown{
				Issues: github.RepoIssueBreakdown{Actionable: 1},
			},
			ActionableIssues: []any{github.Issue{
				Repo:   "testorg/testrepo",
				Number: 7,
				Title:  "fix stale hold pill",
				URL:    "https://github.test/testorg/testrepo/issues/7",
				Labels: []string{"bug"},
			}},
			HeldIssues: []any{},
			OpenPrs:    []any{},
			HeldPrs:    []any{},
		}},
	}
	srv.statusSeq = 3

	w := postRepoHold(t, srv, "writer", false, true)
	if w.Code != http.StatusOK {
		t.Fatalf("writer add: code=%d body=%s", w.Code, w.Body.String())
	}
	var resp repoHoldResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MinStatusSeq != 4 {
		t.Fatalf("minStatusSeq = %d, want 4", resp.MinStatusSeq)
	}
	if srv.status.StatusSeq != 4 {
		t.Fatalf("statusSeq = %d, want bumped snapshot seq 4", srv.status.StatusSeq)
	}
	repo := srv.status.Repos[0]
	if len(repo.ActionableIssues) != 0 || len(repo.HeldIssues) != 1 {
		t.Fatalf("repo lists after add: actionable=%d held=%d", len(repo.ActionableIssues), len(repo.HeldIssues))
	}
	if repo.WorkBreakdown.Issues.Actionable != 0 || repo.WorkBreakdown.Issues.Hold != 1 {
		t.Fatalf("breakdown after add = %+v", repo.WorkBreakdown.Issues)
	}
	if srv.status.Hold.Total != 1 || srv.status.Hold.Issues != 1 || len(srv.status.Hold.Items) != 1 {
		t.Fatalf("hold summary after add = %+v", srv.status.Hold)
	}

	srv.status.Hold.Items[0] = github.HoldItem{Repo: "testrepo", Number: 7, Type: "issue", Labels: []string{"bug", "hive-pause/h1"}}
	mock.issueLabels = []string{"bug", "hive-pause/h1"}
	w = postRepoHold(t, srv, "writer", false, false)
	if w.Code != http.StatusOK {
		t.Fatalf("writer remove: code=%d body=%s", w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.MinStatusSeq != 5 {
		t.Fatalf("remove minStatusSeq = %d, want 5", resp.MinStatusSeq)
	}
	repo = srv.status.Repos[0]
	if len(repo.ActionableIssues) != 1 || len(repo.HeldIssues) != 0 {
		t.Fatalf("repo lists after remove: actionable=%d held=%d", len(repo.ActionableIssues), len(repo.HeldIssues))
	}
	if repo.WorkBreakdown.Issues.Actionable != 1 || repo.WorkBreakdown.Issues.Hold != 0 {
		t.Fatalf("breakdown after remove = %+v", repo.WorkBreakdown.Issues)
	}
	if srv.status.Hold.Total != 0 || srv.status.Hold.Issues != 0 || len(srv.status.Hold.Items) != 0 {
		t.Fatalf("hold summary after remove = %+v", srv.status.Hold)
	}
}

func TestRepoHoldPermissionEndpointUsesCachedGitHubPermission(t *testing.T) {
	mock := &repoHoldMock{permission: map[string]string{"writer": "write"}}
	srv := newRepoHoldServer(t, mock)
	req := httptest.NewRequest(http.MethodGet, "/api/repos/testorg/testrepo/hold-permission", nil)
	req.Header.Set("X-Hive-User", "writer")
	w := httptest.NewRecorder()
	srv.mux.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"allowed":true`) {
		t.Fatalf("permission response: code=%d body=%s", w.Code, w.Body.String())
	}
}
