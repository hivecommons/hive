package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/review"
)

// permissionsTighteningServer serves a PR whose only change is the given
// workflow patch, so the relay can prove (or fail to prove) a tightening.
func permissionsTighteningServer(t *testing.T, patch string, changedFiles int) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/o/r/pulls/7", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"number": 7, "changed_files": changedFiles,
			"head": map[string]any{"sha": "abc"},
		})
	})
	mux.HandleFunc("/repos/o/r/pulls/7/files", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]map[string]any{{
			"filename": ".github/workflows/publish.yml", "status": "modified", "patch": patch,
		}})
	})
	return httptest.NewServer(mux)
}

const tighteningPatch = "@@ -7,4 +7,3 @@\n permissions:\n-  contents: write\n-  id-token: write\n+  contents: read\n jobs:"
const wideningPatch = "@@ -7,3 +7,3 @@\n permissions:\n-  contents: read\n+  contents: write\n jobs:"

func TestRecordReviewVerdictFlagsPermissionsTightening(t *testing.T) {
	cases := []struct {
		name         string
		patch        string
		changedFiles int
		want         bool
	}{
		{"pure tightening", tighteningPatch, 1, true},
		{"widening", wideningPatch, 1, false},
		{"truncated file list fails closed", tighteningPatch, 2, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := permissionsTighteningServer(t, tc.patch, tc.changedFiles)
			defer srv.Close()
			c := testClient(t, srv.URL)
			dir := t.TempDir()
			withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
				pendingVerdict("o/r", 7, "security", "reviewer"),
			}})
			raw := validVerdictJSON(t, "o/r", 7, "security", "requires_human")

			recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer", Report: raw}, dir)

			if len(recorded) != 1 || recorded[0].PermissionsTighteningOnly != tc.want {
				t.Fatalf("recorded = %+v, want PermissionsTighteningOnly=%v", recorded, tc.want)
			}
			matches, _ := filepath.Glob(filepath.Join(dir, review.ReviewReportFilePrefix+"*"+review.ReviewReportFileSuffix))
			if len(matches) != 1 {
				t.Fatalf("want one verdict file, got %v", matches)
			}
			b, _ := os.ReadFile(matches[0])
			if got := strings.Contains(string(b), `"permissions_tightening_only":true`); got != tc.want {
				t.Fatalf("stored verdict %s, want flag=%v", b, tc.want)
			}
		})
	}
}

// An approve with no decision question has no human hold to lift, so the
// relay must not spend a files fetch on it.
func TestRecordReviewVerdictSkipsDiffFetchWithoutHumanHold(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected GitHub call %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := testClient(t, srv.URL)
	dir := t.TempDir()
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{
		pendingVerdict("o/r", 7, "security", "reviewer"),
	}})
	raw := validVerdictJSON(t, "o/r", 7, "security", "approve")

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 7, Agent: "reviewer", Report: raw}, dir)

	if len(recorded) != 1 || recorded[0].PermissionsTighteningOnly {
		t.Fatalf("recorded = %+v, want one unflagged approve", recorded)
	}
}

func TestWorkflowPermissionsTighteningOnlyWithoutClient(t *testing.T) {
	c := &Client{logger: testLogger()}
	if c.workflowPermissionsTighteningOnly(context.Background(), ReviewRequest{Repo: "o/r", Number: 7}) {
		t.Fatal("nil client must fail closed")
	}
}
