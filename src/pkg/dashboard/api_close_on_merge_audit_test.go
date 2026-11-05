package dashboard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func closeOnMergeAuditGet(s *Server, path, role string, verified bool) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if role != "" {
		req.Header.Set("X-Hive-Role", role)
	}
	if verified {
		req.Header.Set(ownerRoleVerifiedHeader, "true")
	}
	s.mux.ServeHTTP(rec, req)
	return rec
}

func stubCloseOnMergeAudit(t *testing.T, audit ghpkg.CloseOnMergeAudit) (*int, *ghpkg.CloseOnMergeAuditOptions) {
	t.Helper()
	prev := closeOnMergeWrongClosesFunc
	closeOnMergeAuditCache.Lock()
	closeOnMergeAuditCache.entries = map[int]closeOnMergeAuditEntry{}
	closeOnMergeAuditCache.Unlock()
	t.Cleanup(func() {
		closeOnMergeWrongClosesFunc = prev
		closeOnMergeAuditCache.Lock()
		closeOnMergeAuditCache.entries = map[int]closeOnMergeAuditEntry{}
		closeOnMergeAuditCache.Unlock()
	})
	calls := 0
	got := &ghpkg.CloseOnMergeAuditOptions{}
	closeOnMergeWrongClosesFunc = func(ctx context.Context, _ *ghpkg.Client, opts ghpkg.CloseOnMergeAuditOptions) (ghpkg.CloseOnMergeAudit, error) {
		calls++
		*got = opts
		if dl, ok := ctx.Deadline(); !ok || time.Until(dl) > closeOnMergeAuditDeadline {
			t.Errorf("scan context deadline = %v (set %v), want within %v", dl, ok, closeOnMergeAuditDeadline)
		}
		return audit, nil
	}
	return &calls, got
}

func TestCloseOnMergeWrongClosesEndpoint(t *testing.T) {
	calls, opts := stubCloseOnMergeAudit(t, ghpkg.CloseOnMergeAudit{Inspected: 3, Items: []ghpkg.CloseOnMergeWrongClose{{Repo: "projectbluefin/utah-packages", Issue: 490, PR: 860, Reason: "pr_leaves_issue_open"}}})
	s, deps := apiServer(t)
	deps.GHClient = &ghpkg.Client{}

	// The scan spends the installation's API budget: owner only.
	for _, tc := range []struct {
		role     string
		verified bool
	}{{"read", false}, {"read-write", false}, {"owner", false}} {
		if rec := closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes", tc.role, tc.verified); rec.Code != http.StatusForbidden {
			t.Fatalf("role %q verified=%v: code = %d, want 403", tc.role, tc.verified, rec.Code)
		}
	}
	if *calls != 0 {
		t.Fatalf("a refused request scanned GitHub %d times", *calls)
	}

	rec := closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes?days=7", "owner", true)
	if rec.Code != http.StatusOK {
		t.Fatalf("code = %d body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Days      int                            `json:"days"`
		Complete  bool                           `json:"complete"`
		Inspected int                            `json:"inspected"`
		Items     []ghpkg.CloseOnMergeWrongClose `json:"items"`
		CachedAt  string                         `json:"cached_at"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Days != 7 || !resp.Complete || resp.Inspected != 3 || len(resp.Items) != 1 || resp.Items[0].Issue != 490 || resp.CachedAt != "" {
		t.Fatalf("resp = %+v", resp)
	}
	if opts.MaxIssues != closeOnMergeAuditMaxIssues {
		t.Fatalf("MaxIssues = %d, want %d", opts.MaxIssues, closeOnMergeAuditMaxIssues)
	}
	if d := time.Since(opts.Since); d < 7*24*time.Hour-time.Minute || d > 7*24*time.Hour+time.Minute {
		t.Fatalf("since = %v, want 7 days ago", opts.Since)
	}

	// A repeat within the TTL is served from the cache: no GitHub calls.
	rec = closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes?days=7", "owner", true)
	resp.CachedAt = ""
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if *calls != 1 || resp.CachedAt == "" || len(resp.Items) != 1 {
		t.Fatalf("repeat: scans = %d cached_at = %q items = %d, want 1 scan and a cached answer", *calls, resp.CachedAt, len(resp.Items))
	}
	// Another window is its own entry.
	closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes?days=8", "owner", true)
	if *calls != 2 {
		t.Fatalf("days=8 scans = %d, want 2", *calls)
	}

	for _, bad := range []string{"0", "91", "x"} {
		if rec := closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes?days="+bad, "owner", true); rec.Code != http.StatusBadRequest {
			t.Errorf("days=%s: code = %d, want 400", bad, rec.Code)
		}
	}
}

// A capped scan says so and is not reported complete.
func TestCloseOnMergeWrongClosesEndpointTruncated(t *testing.T) {
	stubCloseOnMergeAudit(t, ghpkg.CloseOnMergeAudit{Inspected: closeOnMergeAuditMaxIssues, Truncated: true})
	s, deps := apiServer(t)
	deps.GHClient = &ghpkg.Client{}
	rec := closeOnMergeAuditGet(s, "/api/close-on-merge/wrong-closes", "owner", true)
	var resp struct {
		Complete  bool   `json:"complete"`
		Truncated bool   `json:"truncated"`
		Note      string `json:"note"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.Complete || !resp.Truncated || resp.Note == "" {
		t.Fatalf("code=%d resp=%+v, want truncated with a note and complete=false", rec.Code, resp)
	}
}
