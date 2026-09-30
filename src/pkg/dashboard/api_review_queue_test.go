package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
)

// reviewQueueTestServer serves a snapshot with an agent coverage PR (reviewed,
// safe, green), a contributor fix (unreviewed), and a held docs PR.
func reviewQueueTestServer(t *testing.T, verdictsFile string) *Server {
	t.Helper()
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = verdictsFile
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })

	s := covApiServer(t)
	opened := time.Now().Add(-3 * time.Hour)
	s.deps.Scheduler = metricsSchedulerStub{actionable: &ghpkg.ActionableResult{
		GeneratedAt: opened,
		PRs: ghpkg.PRResult{
			Items: []ghpkg.PullRequest{
				{Repo: "repo1", Number: 1, Title: "[quality] test: cover rescan", Author: "bot", Labels: []string{"agent/quality"}, HeadSHA: "h1", CIStatus: "success", CreatedAt: opened},
				{Repo: "repo1", Number: 2, Title: "fix: crash on empty config", Author: "alice", HeadSHA: "h2", CIStatus: "failure", CreatedAt: opened},
			},
			Held: []ghpkg.PullRequest{
				{Repo: "repo1", Number: 3, Title: "docs: explain the queue", Author: "bot", Labels: []string{"hold"}, HeadSHA: "h3", CreatedAt: opened},
			},
		},
	}}
	return s
}

func decodeReviewQueue(t *testing.T, rec *httptest.ResponseRecorder) reviewQueueResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp reviewQueueResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestHandleReviewQueue_RanksAndPages(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	if err := review.WriteArtifact(path, review.Artifact{Items: []review.Aggregate{{
		Repo: "myorg/repo1", Number: 1, HeadSHA: "h1", Verdict: review.VerdictApprove,
		Confidence: review.Confidence{Score: review.ConfidenceMax},
	}}}); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	s := reviewQueueTestServer(t, path)

	// Through the mux, so the route registration is covered too.
	page1 := decodeReviewQueue(t, doOwnerGet(s, "/api/review/queue?limit=2"))
	if page1.Total != 3 || page1.Limit != 2 || page1.Offset != 0 || !page1.HasMore || len(page1.Items) != 2 {
		t.Fatalf("page 1 = %+v", page1)
	}
	first := page1.Items[0]
	if first.Repo != "myorg/repo1" || first.Number != 2 || first.Position != 1 || first.HiveAuthored || first.Priority != ghpkg.ReviewPriorityHigh {
		t.Fatalf("contributor fix should lead the queue: %+v", first)
	}
	if first.Reviewed || first.ConfidenceBand != ghpkg.ReviewQueueBandNeedsAttention || len(first.Reasons) == 0 {
		t.Fatalf("unreviewed fix = %+v", first)
	}
	if held := page1.Items[1]; held.Number != 3 || !held.Held {
		t.Fatalf("second entry = %+v, want the held docs PR", held)
	}

	page2 := decodeReviewQueue(t, doOwnerGet(s, "/api/review/queue?limit=2&offset=2"))
	if page2.HasMore || len(page2.Items) != 1 {
		t.Fatalf("page 2 = %+v", page2)
	}
	last := page2.Items[0]
	if last.Number != 1 || last.Position != 3 || !last.Reviewed || last.ConfidenceScore == nil || *last.ConfidenceScore != review.ConfidenceMax {
		t.Fatalf("agent coverage entry = %+v", last)
	}

	beyond := decodeReviewQueue(t, doOwnerGet(s, "/api/review/queue?offset=10"))
	if beyond.Total != 3 || beyond.HasMore || beyond.Items == nil || len(beyond.Items) != 0 {
		t.Fatalf("offset past the end = %+v", beyond)
	}
	if def := decodeReviewQueue(t, doOwnerGet(s, "/api/review/queue")); def.Limit != reviewQueueDefaultLimit || len(def.Items) != 3 {
		t.Fatalf("default page = %+v", def)
	}
}

func TestHandleReviewQueue_RejectsBadPaging(t *testing.T) {
	s := reviewQueueTestServer(t, filepath.Join(t.TempDir(), "missing.json"))
	for _, q := range []string{"limit=0", "limit=-1", "limit=abc", "limit=201", "offset=-1", "offset=x"} {
		rec := httptest.NewRecorder()
		s.handleReviewQueue(rec, httptest.NewRequest(http.MethodGet, "/api/review/queue?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", q, rec.Code)
		}
	}
}

func TestHandleReviewQueue_MissingVerdictsRanksUnreviewed(t *testing.T) {
	s := reviewQueueTestServer(t, filepath.Join(t.TempDir(), "missing.json"))
	rec := httptest.NewRecorder()
	s.handleReviewQueue(rec, httptest.NewRequest(http.MethodGet, "/api/review/queue", nil))
	resp := decodeReviewQueue(t, rec)
	if resp.Total != 3 {
		t.Fatalf("total = %d", resp.Total)
	}
	for _, e := range resp.Items {
		if e.Reviewed || e.ConfidenceBand == ghpkg.ReviewQueueBandSafe {
			t.Fatalf("no verdicts on disk, yet %s#%d reads as reviewed/safe: %+v", e.Repo, e.Number, e)
		}
	}
}

func TestHandleReviewQueue_UnreadableVerdictsIs500(t *testing.T) {
	path := filepath.Join(t.TempDir(), "review-verdicts.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := reviewQueueTestServer(t, path)
	rec := httptest.NewRecorder()
	s.handleReviewQueue(rec, httptest.NewRequest(http.MethodGet, "/api/review/queue", nil))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status %d, want 500", rec.Code)
	}
}

func TestHandleReviewQueue_NoSnapshotIsEmpty(t *testing.T) {
	orig := review.ReviewVerdictsPath
	review.ReviewVerdictsPath = filepath.Join(t.TempDir(), "missing.json")
	t.Cleanup(func() { review.ReviewVerdictsPath = orig })
	s := covApiServer(t)
	s.deps.Scheduler = nil
	rec := httptest.NewRecorder()
	s.handleReviewQueue(rec, httptest.NewRequest(http.MethodGet, "/api/review/queue", nil))
	resp := decodeReviewQueue(t, rec)
	if resp.Total != 0 || resp.HasMore || resp.Items == nil {
		t.Fatalf("empty queue = %+v", resp)
	}
}

func TestReviewConfigPut_PriorityLabels(t *testing.T) {
	s := covApiServer(t)
	if s.deps.Config.Review.PriorityLabels {
		t.Fatal("priority_labels must default off")
	}
	if rec := doPut(s, "/api/config/review", map[string]any{"priority_labels": true}); rec.Code != http.StatusOK {
		t.Fatalf("put: %d %s", rec.Code, rec.Body.String())
	}
	if !s.deps.Config.Review.PriorityLabels {
		t.Fatal("priority_labels not applied")
	}
	if rec := doPut(s, "/api/config/review", map[string]any{}); rec.Code != http.StatusOK || !s.deps.Config.Review.PriorityLabels {
		t.Fatal("an absent key must leave priority_labels untouched")
	}
}
