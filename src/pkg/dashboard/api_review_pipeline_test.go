package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	ghpkg "github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/review"
	"github.com/hivecommons/hive/pkg/review/pipeline"
)

// reviewPipelineTestServer reuses the review-queue snapshot (#1 agent
// coverage, #2 contributor fix, #3 held docs) and points the verdict,
// dispatch-state and review-links files at dir.
func reviewPipelineTestServer(t *testing.T, dir string) *Server {
	t.Helper()
	s := reviewQueueTestServer(t, filepath.Join(dir, "review-verdicts.json"))

	origDispatch, origLegacy, origLinks := review.ReviewDispatchStatePath, review.LegacyReviewDispatchStatePath, ghpkg.ReviewLinksPath
	review.ReviewDispatchStatePath = filepath.Join(dir, "review-dispatch-state.json")
	review.LegacyReviewDispatchStatePath = filepath.Join(dir, "legacy-review-dispatch-state.json")
	ghpkg.ReviewLinksPath = filepath.Join(dir, "review-links.json")
	t.Cleanup(func() {
		review.ReviewDispatchStatePath, review.LegacyReviewDispatchStatePath, ghpkg.ReviewLinksPath = origDispatch, origLegacy, origLinks
	})
	return s
}

func seedReviewPipeline(t *testing.T, dir string) {
	t.Helper()
	if err := review.WriteArtifact(filepath.Join(dir, "review-verdicts.json"), review.Artifact{Items: []review.Aggregate{{
		Repo: "myorg/repo1", Number: 1, HeadSHA: "h1", Verdict: review.VerdictChangesRequested,
		Perspectives: map[review.Perspective]review.Verdict{"security": review.VerdictChangesRequested},
		ReviewModel:  "model-x",
	}}}); err != nil {
		t.Fatalf("WriteArtifact: %v", err)
	}
	if err := review.WriteDispatchState(review.ReviewDispatchStatePath, review.DispatchState{
		Fixes: []review.PendingFix{{Repo: "myorg/repo1", Number: 1, HeadSHA: "h1", Agent: "fixer", Attempts: 1}},
	}); err != nil {
		t.Fatalf("WriteDispatchState: %v", err)
	}
	// Recorded under the bare name to exercise lookupReviewLink's fallback.
	if err := ghpkg.RecordReviewLink(ghpkg.ReviewLinksPath, "repo1", 2, ghpkg.ReviewLink{
		URL: "https://github.com/myorg/repo1/pull/2#pullrequestreview-9", State: "changes_requested", HeadSHA: "h2",
	}); err != nil {
		t.Fatalf("RecordReviewLink: %v", err)
	}
}

func decodeReviewPipeline(t *testing.T, rec *httptest.ResponseRecorder) reviewPipelineResponse {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp reviewPipelineResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestHandleReviewPipeline_DerivesStagesFiltersAndPages(t *testing.T) {
	dir := t.TempDir()
	s := reviewPipelineTestServer(t, dir)
	seedReviewPipeline(t, dir)

	all := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline"))
	if all.Total != 3 || all.Limit != reviewQueueDefaultLimit || all.Offset != 0 || all.HasMore || len(all.Cards) != 3 {
		t.Fatalf("all = %+v", all)
	}
	want := []struct {
		number int
		stage  pipeline.Stage
	}{
		{2, pipeline.StageChangesRequested},
		{3, pipeline.StageHumanHold},
		{1, pipeline.StageFixing},
	}
	for i, w := range want {
		c := all.Cards[i]
		if c.Number != w.number || c.Stage != w.stage || c.Repo != "myorg/repo1" {
			t.Fatalf("card %d = %+v, want #%d %s", i, c, w.number, w.stage)
		}
	}
	if got := all.Cards[0].NextAction; got.Kind != pipeline.ActionFix || got.URL != "https://github.com/myorg/repo1/pull/2#pullrequestreview-9" {
		t.Fatalf("contributor fix next action = %+v", got)
	}
	if got := all.Cards[2]; got.LoopCount != 1 || len(got.Reviewers) != 1 || got.Reviewers[0].Model != "model-x" {
		t.Fatalf("fixing card = %+v", got)
	}

	fixing := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline?stage=FIXING"))
	if fixing.Total != 1 || len(fixing.Cards) != 1 || fixing.Cards[0].Number != 1 {
		t.Fatalf("stage filter = %+v", fixing)
	}

	page := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline?repo=repo1&limit=1&offset=1"))
	if page.Total != 3 || page.Limit != 1 || page.Offset != 1 || !page.HasMore || len(page.Cards) != 1 || page.Cards[0].Number != 3 {
		t.Fatalf("repo page = %+v", page)
	}

	none := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline?repo=myorg/other"))
	if none.Total != 0 || none.HasMore || none.Cards == nil || len(none.Cards) != 0 {
		t.Fatalf("other repo = %+v", none)
	}

	beyond := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline?offset=10"))
	if beyond.Total != 3 || beyond.HasMore || len(beyond.Cards) != 0 {
		t.Fatalf("offset past the end = %+v", beyond)
	}
}

func TestHandleReviewPipeline_MissingEvidenceIsUnreviewed(t *testing.T) {
	s := reviewPipelineTestServer(t, t.TempDir())
	resp := decodeReviewPipeline(t, doOwnerGet(s, "/api/review/pipeline?stage=unreviewed"))
	if resp.Total != 2 || len(resp.Cards) != 2 {
		t.Fatalf("unreviewed = %+v", resp)
	}
	for _, c := range resp.Cards {
		if c.Stage != pipeline.StageUnreviewed || c.NextAction.Kind != pipeline.ActionReview {
			t.Fatalf("card = %+v", c)
		}
	}
}

func TestHandleReviewPipeline_RejectsBadQuery(t *testing.T) {
	s := reviewPipelineTestServer(t, t.TempDir())
	for _, q := range []string{"stage=bogus", "limit=0", "limit=201", "offset=-1"} {
		rec := httptest.NewRecorder()
		s.handleReviewPipeline(rec, httptest.NewRequest(http.MethodGet, "/api/review/pipeline?"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s: status %d, want 400", q, rec.Code)
		}
	}
}

func TestHandleReviewPipeline_UnreadableEvidence(t *testing.T) {
	for _, name := range []string{"review-verdicts.json", "review-dispatch-state.json", "review-links.json"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s := reviewPipelineTestServer(t, dir)
			if err := os.WriteFile(filepath.Join(dir, name), []byte("{not json"), 0o600); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			s.handleReviewPipeline(rec, httptest.NewRequest(http.MethodGet, "/api/review/pipeline", nil))
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("status %d, want 500: %s", rec.Code, rec.Body.String())
			}
		})
	}
}
