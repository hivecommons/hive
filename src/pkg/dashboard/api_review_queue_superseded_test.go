package dashboard

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

// useRingSupersessionAudit points the queue's on-disk audit scan at an empty
// temp dir so the in-memory ring is the only record.
func useRingSupersessionAudit(t *testing.T) {
	t.Helper()
	orig := reviewQueueSupersessionAuditPath
	reviewQueueSupersessionAuditPath = filepath.Join(t.TempDir(), "audit.jsonl")
	t.Cleanup(func() { reviewQueueSupersessionAuditPath = orig })
}

func supersessionGraceDetail(repo string, pr int, issue string, closer int) string {
	return "repo=" + repo + ", pr=" + strconv.Itoa(pr) + ", issue=" + issue + ", closer_pr=" + strconv.Itoa(closer) + ", action=commented-grace"
}

func TestSupersededGraceStates(t *testing.T) {
	t0 := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	at := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339) }
	entries := []AuditEntry{
		{Timestamp: at(0), Action: supersessionGraceAuditAction, Repo: "repo1", Target: 2, Detail: supersessionGraceDetail("repo1", 2, "myorg/repo1#3", 9)},
		{Timestamp: at(time.Hour), Action: supersessionGraceAuditAction, Repo: "repo1", Target: 2, Detail: supersessionGraceDetail("repo1", 2, "myorg/repo1#3", 9)},
		{Timestamp: at(0), Action: supersessionGraceAuditAction, Repo: "repo1", Target: 5, Detail: supersessionGraceDetail("repo1", 5, "repo1#4", 10)},
		{Timestamp: at(time.Hour), Action: "supersession-sweep-commented-contributor", Repo: "repo1", Target: 5},
		{Timestamp: at(0), Action: supersessionGraceAuditAction, Repo: "myorg/repo1", Target: 6, Detail: supersessionGraceDetail("myorg/repo1", 6, "repo1#4", 10)},
		{Timestamp: at(time.Hour), Action: supersessionKeepOpenAction, Repo: "myorg/repo1", Target: 6},
		{Timestamp: at(0), Action: supersessionGraceAuditAction, Repo: "repo1", Target: 7, Detail: "repo=repo1, pr=7"},
		{Timestamp: "not-a-time", Action: supersessionGraceAuditAction, Repo: "repo1", Target: 8, Detail: supersessionGraceDetail("repo1", 8, "repo1#4", 10)},
		{Timestamp: at(0), Action: supersessionGraceAuditAction, Repo: "", Target: 9},
		{Timestamp: at(2 * time.Hour), Action: supersessionGraceAuditAction, Repo: "repo1", Target: 5, Detail: supersessionGraceDetail("repo1", 5, "repo1#4", 10)},
	}
	states := supersededGraceStates(entries, "myorg", 24*time.Hour)
	if len(states) != 2 {
		t.Fatalf("states = %+v, want #2 and #5 only", states)
	}
	got, ok := states[ghpkg.ReviewQueueKey("myorg/repo1", 2)]
	if !ok {
		t.Fatalf("#2 missing: %+v", states)
	}
	if got.Reason != ghpkg.ReviewQueueSupersededReason || got.CloserPR != 9 || got.CloserRepo != "myorg/repo1" || got.Issue != "myorg/repo1#3" {
		t.Fatalf("#2 = %+v", got)
	}
	if got.CloserURL != "https://github.com/myorg/repo1/pull/9" {
		t.Fatalf("closer url = %q", got.CloserURL)
	}
	if !got.GraceStartedAt.Equal(t0) || !got.GraceEndsAt.Equal(t0.Add(24*time.Hour)) {
		t.Fatalf("window = %s..%s, want it to start at the first grace entry", got.GraceStartedAt, got.GraceEndsAt)
	}
	restarted := states[ghpkg.ReviewQueueKey("myorg/repo1", 5)]
	if !restarted.GraceStartedAt.Equal(t0.Add(2*time.Hour)) || restarted.CloserRepo != "myorg/repo1" {
		t.Fatalf("#5 should restart after a keep: %+v", restarted)
	}
}

func TestSupersededReason(t *testing.T) {
	ends := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	info := ghpkg.ReviewQueueSuperseded{Reason: ghpkg.ReviewQueueSupersededReason, CloserRepo: "o/r", CloserPR: 9, GraceEndsAt: ends}
	if got := supersededReason(info, ends.Add(-time.Hour)); !strings.HasPrefix(got, "superseded: by o/r#9") || !strings.Contains(got, "unless someone replies") {
		t.Fatalf("pending reason = %q", got)
	}
	if got := supersededReason(info, ends); !strings.Contains(got, "grace window ended") {
		t.Fatalf("ended reason = %q", got)
	}
}

func TestHandleReviewQueue_Superseded(t *testing.T) {
	useRingSupersessionAudit(t)
	s := reviewQueueTestServer(t, filepath.Join(t.TempDir(), "review-verdicts.json"))
	start := time.Now().Add(-2 * time.Hour).UTC()
	s.audit.LogRecordAt(start.Format(time.RFC3339), "system", supersessionGraceAuditAction, supersessionGraceDetail("repo1", 2, "myorg/repo1#3", 9), "", "repo1", 2)

	rec := httptest.NewRecorder()
	s.handleReviewQueue(rec, httptest.NewRequest(http.MethodGet, "/api/review/queue", nil))
	resp := decodeReviewQueue(t, rec)
	var found bool
	for _, item := range resp.Items {
		if item.Number != 2 {
			if item.Superseded != nil {
				t.Fatalf("#%d wrongly marked superseded", item.Number)
			}
			continue
		}
		found = true
		if item.Superseded == nil || item.Superseded.Reason != "superseded" || item.Superseded.CloserURL != "https://github.com/myorg/repo1/pull/9" {
			t.Fatalf("#2 superseded = %+v", item.Superseded)
		}
		if want := start.Truncate(time.Second).Add(ghpkg.DefaultSupersessionGracePeriod); !item.Superseded.GraceEndsAt.Equal(want) {
			t.Fatalf("grace ends %s, want %s", item.Superseded.GraceEndsAt, want)
		}
		last := item.Reasons[len(item.Reasons)-1]
		if !strings.HasPrefix(last, "superseded: by myorg/repo1#9") {
			t.Fatalf("reasons = %v", item.Reasons)
		}
	}
	if !found {
		t.Fatalf("#2 not in queue: %+v", resp.Items)
	}

	s.deps.Config.SupersessionSweep.GracePeriod = time.Hour
	if got := s.supersessionGracePeriod(); got != time.Hour {
		t.Fatalf("configured grace = %s", got)
	}
}

type supersededGitHubFake struct {
	labels   []string
	comments []string
	closed   int
}

func newSupersededActionServer(t *testing.T) (*Server, *supersededGitHubFake) {
	t.Helper()
	useRingSupersessionAudit(t)
	fake := &supersededGitHubFake{}
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/repos/acme/widget/labels/"):
			json.NewEncoder(w).Encode(map[string]string{"name": "x"})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/labels":
			var in []string
			json.NewDecoder(r.Body).Decode(&in)
			fake.labels = append(fake.labels, in...)
			json.NewEncoder(w).Encode([]map[string]string{})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			out := []map[string]string{}
			for _, c := range fake.comments {
				out = append(out, map[string]string{"body": c})
			}
			json.NewEncoder(w).Encode(out)
		case r.Method == http.MethodPost && r.URL.Path == "/repos/acme/widget/issues/7/comments":
			var in struct {
				Body string `json:"body"`
			}
			json.NewDecoder(r.Body).Decode(&in)
			fake.comments = append(fake.comments, in.Body)
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": len(fake.comments)})
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/widget/issues/7":
			fake.closed++
			json.NewEncoder(w).Encode(map[string]any{"number": 7, "state": "closed"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(api.Close)

	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{
		Config:   &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widget"}}},
		GHClient: ghpkg.NewClient("app-token", "acme", []string{"widget"}, slog.Default(), api.URL),
		Logger:   slog.Default(),
	}
	return s, fake
}

func supersededActionRequest(owner, repo, number, action string, verified bool) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/review/queue/"+owner+"/"+repo+"/"+number+"/superseded/"+action, nil)
	req.SetPathValue("owner", owner)
	req.SetPathValue("repo", repo)
	req.SetPathValue("number", number)
	req.Header.Set("X-Hive-User", "alice")
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	return req
}

func TestReviewQueueSupersededClose(t *testing.T) {
	s, fake := newSupersededActionServer(t)
	s.audit.LogRecord("system", supersessionGraceAuditAction, supersessionGraceDetail("widget", 7, "acme/widget#3", 9), "", "widget", 7)

	merger := supersededActionRequest("acme", "widget", "7", "close", false)
	merger.Header.Set("X-Hive-Role", config.RoleMerger)
	w := httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, merger)
	if w.Code != http.StatusForbidden || fake.closed != 0 {
		t.Fatalf("merger: status %d closed %d", w.Code, fake.closed)
	}
	w = httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, supersededActionRequest("acme", "widget", "7", "close", false))
	if w.Code != http.StatusForbidden {
		t.Fatalf("unverified owner: status %d", w.Code)
	}

	w = httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, supersededActionRequest("acme", "widget", "7", "close", true))
	if w.Code != http.StatusOK {
		t.Fatalf("close: status %d body %s", w.Code, w.Body.String())
	}
	if fake.closed != 1 || len(fake.labels) != 1 || fake.labels[0] != ghpkg.SupersededLabel {
		t.Fatalf("closed %d labels %v", fake.closed, fake.labels)
	}
	if len(fake.comments) != 1 || !strings.Contains(fake.comments[0], ghpkg.SupersessionAutoCloseMarker) || !strings.Contains(fake.comments[0], "acme/widget#9") || !strings.Contains(fake.comments[0], "@alice") {
		t.Fatalf("comments = %v", fake.comments)
	}
	if states := supersededGraceStates(s.supersessionAuditEntries(time.Now()), "acme", time.Hour); len(states) != 0 {
		t.Fatalf("confirmed close should end the grace window: %+v", states)
	}

	w = httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, supersededActionRequest("acme", "widget", "7", "close", true))
	if w.Code != http.StatusOK || len(fake.comments) != 1 || fake.closed != 2 {
		t.Fatalf("repeat close: status %d comments %d closed %d", w.Code, len(fake.comments), fake.closed)
	}
}

func TestReviewQueueSupersededKeepOpen(t *testing.T) {
	s, fake := newSupersededActionServer(t)
	s.audit.LogRecord("system", supersessionGraceAuditAction, supersessionGraceDetail("widget", 7, "acme/widget#3", 9), "", "widget", 7)

	w := httptest.NewRecorder()
	s.handleReviewQueueSupersededKeepOpen(w, supersededActionRequest("acme", "widget", "7", "keep-open", true))
	if w.Code != http.StatusOK {
		t.Fatalf("keep-open: status %d body %s", w.Code, w.Body.String())
	}
	if len(fake.labels) != 1 || fake.labels[0] != ghpkg.SupersessionKeepOpenLabel || fake.closed != 0 || len(fake.comments) != 0 {
		t.Fatalf("labels %v closed %d comments %v", fake.labels, fake.closed, fake.comments)
	}
	if states := supersededGraceStates(s.supersessionAuditEntries(time.Now()), "acme", time.Hour); len(states) != 0 {
		t.Fatalf("keep-open should end the grace window: %+v", states)
	}
}

func TestReviewQueueSupersededActionValidation(t *testing.T) {
	s, fake := newSupersededActionServer(t)
	cases := []struct {
		name                string
		owner, repo, number string
		want                int
	}{
		{"bad number", "acme", "widget", "x", http.StatusBadRequest},
		{"zero number", "acme", "widget", "0", http.StatusBadRequest},
		{"empty repo", "acme", "", "7", http.StatusBadRequest},
		{"unmanaged repo", "acme", "other", "7", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			s.handleReviewQueueSupersededKeepOpen(w, supersededActionRequest(tc.owner, tc.repo, tc.number, "keep-open", true))
			if w.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	if len(fake.labels) != 0 {
		t.Fatalf("invalid requests labeled: %v", fake.labels)
	}

	s.deps.GHClient = nil
	w := httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, supersededActionRequest("acme", "widget", "7", "close", true))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no client: status %d", w.Code)
	}
}

func TestReviewQueueSupersededActionGitHubErrors(t *testing.T) {
	useRingSupersessionAudit(t)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer api.Close()
	s := NewServer(0, slog.Default())
	s.deps = &Dependencies{
		Config:   &config.Config{Project: config.ProjectConfig{Org: "acme", Repos: []string{"widget"}}},
		GHClient: ghpkg.NewClient("app-token", "acme", []string{"widget"}, slog.Default(), api.URL),
		Logger:   slog.Default(),
	}
	w := httptest.NewRecorder()
	s.handleReviewQueueSupersededClose(w, supersededActionRequest("acme", "widget", "7", "close", true))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("close: status %d", w.Code)
	}
	w = httptest.NewRecorder()
	s.handleReviewQueueSupersededKeepOpen(w, supersededActionRequest("acme", "widget", "7", "keep-open", true))
	if w.Code != http.StatusBadGateway {
		t.Fatalf("keep-open: status %d", w.Code)
	}
}
