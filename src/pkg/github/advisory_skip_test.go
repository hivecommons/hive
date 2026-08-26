package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// Tests for the skip-if-unchanged guard on PostAdvisoryDigest (#4818): a
// byte-identical digest must not be rewritten to the forge every ~60s, but
// the guard must never mask a changed body, a failed write, or a permission
// regression (periodic write-through).

// digestCountingServer serves the advisory-digest comment endpoints and
// counts every request class the guard is supposed to eliminate.
type digestCountingServer struct {
	*httptest.Server
	listCalls  int // GET  /issues/N/comments (findDigestComment)
	editCalls  int // PATCH /issues/comments/ID
	editStatus int // response code for PATCH; 200 by default
}

func newDigestCountingServer(t *testing.T, org, repo string) *digestCountingServer {
	t.Helper()
	s := &digestCountingServer{editStatus: http.StatusOK}
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/10/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			s.listCalls++
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 555, "body": advisoryDigestPrefix + " — old content"},
			})
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/555", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			s.editCalls++
			if s.editStatus != http.StatusOK {
				w.WriteHeader(s.editStatus)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"id": 555})
		}
	})
	s.Server = httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestPostAdvisoryDigest_UnchangedSkipsForgeWrite(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	digest := advisoryDigestPrefix + "\nsteady-state content"
	// First post after process start must always write.
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
		t.Fatalf("first post: %v", err)
	}
	if srv.editCalls != 1 {
		t.Fatalf("first post: editCalls = %d, want 1", srv.editCalls)
	}

	// Byte-identical repeats: no edit AND no comment-list read — the whole
	// forge round-trip is skipped. Must still return nil (a skipped cycle is
	// a healthy cycle: the caller's success path advances RecordAdvisoryPost,
	// the advisory-staleness freshness record — see cmd/hive/main.go).
	for i := 0; i < 5; i++ {
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
			t.Fatalf("skipped cycle %d must return nil (staleness gate), got: %v", i, err)
		}
	}
	if srv.editCalls != 1 {
		t.Errorf("after 5 unchanged cycles: editCalls = %d, want 1 (no rewrites)", srv.editCalls)
	}
	if srv.listCalls != 1 {
		t.Errorf("after 5 unchanged cycles: listCalls = %d, want 1 (skip avoids the read too)", srv.listCalls)
	}
}

func TestPostAdvisoryDigest_ChangedBodyWritesExactlyOnce(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+"\nversion A"); err != nil {
		t.Fatalf("post A: %v", err)
	}
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+"\nversion B"); err != nil {
		t.Fatalf("post B: %v", err)
	}
	if srv.editCalls != 2 {
		t.Errorf("changed body: editCalls = %d, want 2 (exactly one edit per distinct body)", srv.editCalls)
	}
	// And the new body immediately becomes the skip baseline.
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, advisoryDigestPrefix+"\nversion B"); err != nil {
		t.Fatalf("repeat B: %v", err)
	}
	if srv.editCalls != 2 {
		t.Errorf("repeat of B: editCalls = %d, want 2 (unchanged repeat skipped)", srv.editCalls)
	}
}

func TestPostAdvisoryDigest_WriteThroughFiresOnNthCycle(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	c := newTestClient(t, srv.Server, org, []string{repo})

	digest := advisoryDigestPrefix + "\nquiet hive"
	// Cycle 1 writes; cycles 2..N-1 skip; cycle N (interval-th consecutive
	// unchanged cycle) must write through so a 403/permission regression
	// still surfaces within the interval.
	total := advisoryDigestWriteThroughInterval + 1
	for i := 0; i < total; i++ {
		if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
			t.Fatalf("cycle %d: %v", i+1, err)
		}
	}
	if srv.editCalls != 2 {
		t.Errorf("after %d unchanged cycles: editCalls = %d, want 2 (initial + one write-through)", total, srv.editCalls)
	}
	// The write-through resets the streak: the very next unchanged cycle
	// skips again.
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
		t.Fatalf("post-write-through cycle: %v", err)
	}
	if srv.editCalls != 2 {
		t.Errorf("cycle after write-through: editCalls = %d, want 2 (streak reset, skip resumes)", srv.editCalls)
	}
}

func TestPostAdvisoryDigest_FailedWriteIsNotRecordedAsPosted(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	srv.editStatus = http.StatusForbidden
	c := newTestClient(t, srv.Server, org, []string{repo})

	digest := advisoryDigestPrefix + "\nnever landed"
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err == nil {
		t.Fatal("expected error from 403 edit")
	}
	// The hash is recorded only on SUCCESS, so the retry must hit the forge
	// again rather than skipping a body that never landed.
	srv.editStatus = http.StatusOK
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if srv.editCalls != 2 {
		t.Errorf("editCalls = %d, want 2 (failed write must be retried, not skipped)", srv.editCalls)
	}
}

func TestPostAdvisoryDigest_SkipKeyedPerIssue(t *testing.T) {
	org, repo := "testorg", "testrepo"
	srv := newDigestCountingServer(t, org, repo)
	// Second issue (#11) with its own comment endpoints.
	var issue11Edits int
	srv.Config.Handler.(*http.ServeMux).HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/11/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			json.NewEncoder(w).Encode([]map[string]any{
				{"id": 777, "body": advisoryDigestPrefix + " — old"},
			})
		}
	})
	srv.Config.Handler.(*http.ServeMux).HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/comments/777", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PATCH" {
			issue11Edits++
			json.NewEncoder(w).Encode(map[string]any{"id": 777})
		}
	})
	c := newTestClient(t, srv.Server, org, []string{repo})

	digest := advisoryDigestPrefix + "\nsame body, two issues"
	if err := c.PostAdvisoryDigest(context.Background(), repo, 10, digest); err != nil {
		t.Fatalf("issue 10: %v", err)
	}
	// Same body, DIFFERENT issue: the hash is keyed per owner/repo#issue, so
	// this must be a first-write for #11, not a skip inherited from #10.
	if err := c.PostAdvisoryDigest(context.Background(), repo, 11, digest); err != nil {
		t.Fatalf("issue 11: %v", err)
	}
	if issue11Edits != 1 {
		t.Errorf("issue 11 editCalls = %d, want 1 (hash must not be shared across issues)", issue11Edits)
	}
}
