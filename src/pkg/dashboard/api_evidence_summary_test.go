package dashboard

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/evidence"
	ghpkg "github.com/hivecommons/hive/pkg/github"
)

func evSummaryBundle(repo string, n int, kind string, created time.Time, merged bool, verdicts []evidence.Verdict, sentinel int, ci bool) *evidence.Bundle {
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion, Repo: repo, Number: n, HeadSHA: "abc1234",
		Author: evidence.Author{Login: "x", Kind: kind}, CreatedAt: created, UpdatedAt: created,
		Verdicts: verdicts,
	}
	if merged {
		b.Merge = &evidence.MergeEvent{Actor: "a", Method: "squash", SHA: "m"}
	}
	for i := 0; i < sentinel; i++ {
		b.Sentinel = append(b.Sentinel, evidence.SentinelFinding{Rule: "r"})
	}
	if ci {
		b.CI.Checks = []evidence.Check{{Name: "build", Conclusion: "success"}}
	}
	return b
}

func TestBuildEvidenceSummary(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	recent := now.Add(-2 * time.Hour)
	old := now.Add(-10 * 24 * time.Hour)
	held, notHeld := true, false
	reverted := evSummaryBundle("a/b", 4, evidence.AuthorAgent, recent, true, nil, 0, true)
	reverted.RevertedBy = []string{"a/b#9"}
	reverted.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &held}}
	shadowOnly := evSummaryBundle("a/b", 5, evidence.AuthorHuman, recent, true, nil, 0, true)
	shadowOnly.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &notHeld}}
	bundles := []*evidence.Bundle{
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent.Add(-time.Hour), false, nil, 0, true),
		// newer bundle for the same PR supersedes the one above
		evSummaryBundle("a/b", 1, evidence.AuthorAgent, recent, true, []evidence.Verdict{
			{Perspective: "security", Model: "m1", Verdict: "approve", Confidence: 0.95},
			{Perspective: "security", Model: "", Verdict: "changes_requested", Confidence: 0.3},
		}, 2, true),
		evSummaryBundle("a/b", 2, evidence.AuthorHuman, recent, false, []evidence.Verdict{
			{Perspective: "security", Model: "m1", Verdict: "approve", Confidence: 0.6},
		}, 0, false),
		evSummaryBundle("c/d", 3, evidence.AuthorBot, old, true, nil, 1, true),
		reverted,
		shadowOnly,
	}

	got := buildEvidenceSummary(bundles, now, 24, "", "")
	if got.N != 4 {
		t.Fatalf("n = %d, want 4", got.N)
	}
	if o := got.Outcomes["agent"]; o.N != 2 || o.Merged != 2 || o.Open != 0 || o.RevertedWithin14d != 1 {
		t.Errorf("agent outcomes = %+v", o)
	}
	if o := got.Outcomes["human"]; o.N != 2 || o.Open != 1 || o.Merged != 1 || o.RevertedWithin14d != 0 {
		t.Errorf("human outcomes = %+v", o)
	}
	if got.Missing.VerdictsModelUnknown != 1 || got.Missing.RecordsEmptyCI != 1 {
		t.Errorf("missing = %+v", got.Missing)
	}
	if got.SentinelPerWeek.N != 2 || len(got.SentinelPerWeek.Weeks) != 1 || got.SentinelPerWeek.Weeks[0].Week != "2026-W42" {
		t.Errorf("sentinel = %+v", got.SentinelPerWeek)
	}
	if len(got.Calibration) != 1 || got.Calibration[0].N != 3 {
		t.Fatalf("calibration = %+v", got.Calibration)
	}
	bk := got.Calibration[0].Buckets
	if bk[0].N != 1 || bk[0].Blocks != 1 || bk[1].N != 1 || bk[3].N != 1 || bk[3].Merged != 1 {
		t.Errorf("buckets = %+v", bk)
	}
	if len(got.ReviewerModels) != 2 {
		t.Fatalf("models = %+v", got.ReviewerModels)
	}
	unk := got.ReviewerModels[1]
	if unk.Model != "unknown" || unk.ByAuthor["agent"].MergedAfterBlock != 1 {
		t.Errorf("unknown model = %+v", unk)
	}
	// Two bundles carry a shadow review-gate observation; only one would have held.
	if got.ShadowGate.N != 2 || got.ShadowGate.WouldHaveHeld != 1 {
		t.Errorf("shadow gate = %+v", got.ShadowGate)
	}

	if all := buildEvidenceSummary(bundles, now, 0, "c/d", ""); all.N != 1 || !all.AllTime {
		t.Errorf("all-time repo filter: %+v", all)
	}
	if k := buildEvidenceSummary(bundles, now, 0, "", "human"); k.N != 2 || k.Outcomes["agent"].N != 0 {
		t.Errorf("author_kind filter: %+v", k)
	}
}

func TestHandleEvidenceSummaryValidation(t *testing.T) {
	s := &Server{}
	for _, q := range []string{"hours=-1", "hours=x", "repo=nope", "author_kind=robot"} {
		w := reviewEvidenceRequest(t, s.handleEvidenceSummary, config.RoleMerger, "/api/evidence/summary?"+q)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", q, w.Code)
		}
	}
}

func TestHandleEvidenceSummaryRoleGate(t *testing.T) {
	reviewEvidenceTestEnv(t)
	s := &Server{}
	for _, role := range []string{"", config.RoleRead, config.RoleReadWrite} {
		w := reviewEvidenceRequest(t, s.handleEvidenceSummary, role, "/api/evidence/summary")
		if w.Code != http.StatusForbidden {
			t.Errorf("role %q: status %d, want 403", role, w.Code)
		}
	}
	// An owner role without the verified header is refused too.
	req := httptest.NewRequest(http.MethodGet, "/api/evidence/summary", nil)
	req.Header.Set("X-Hive-Role", config.RoleOwner)
	w := httptest.NewRecorder()
	s.handleEvidenceSummary(w, req)
	if w.Code != http.StatusForbidden {
		t.Errorf("unverified owner: status %d, want 403", w.Code)
	}
	for _, role := range []string{config.RoleMerger, config.RoleOwner} {
		if w := reviewEvidenceRequest(t, s.handleEvidenceSummary, role, "/api/evidence/summary"); w.Code != http.StatusOK {
			t.Errorf("role %q: status %d, want 200: %s", role, w.Code, w.Body)
		}
	}
}

func TestHandleEvidenceSummaryEmpty(t *testing.T) {
	reviewEvidenceTestEnv(t)
	s := &Server{}
	w := reviewEvidenceRequest(t, s.handleEvidenceSummary, config.RoleMerger, "/api/evidence/summary")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.N != 0 || resp.Hours != 24 {
		t.Errorf("resp = %+v", resp)
	}
}

// writeEvidenceFixture writes raw bytes where the loader expects a bundle for
// repo#number@head.
func writeEvidenceFixture(t *testing.T, repo string, number int, head string, data []byte) string {
	t.Helper()
	path := ghpkg.ReviewEvidencePath("", repo, number, head)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestHandleEvidenceSummaryFixtures(t *testing.T) {
	reviewEvidenceTestEnv(t)
	now := time.Now().UTC()
	created := now.Add(-time.Hour)

	// Two valid heads for one PR: the newer one (merged, reverted, shadow-held) wins.
	seedReviewEvidenceBundle(t, "aaaaaaa1", "", created.Add(-time.Minute), false)
	newer := seedReviewEvidenceBundle(t, "bbbbbbb2", "", created, true)
	held := true
	newer.RevertedBy = []string{"acme/widget#8"}
	newer.Shadow = &evidence.Shadow{ReviewGate: &evidence.ShadowGate{WouldHaveHeld: &held, EvaluatedAt: created}}
	data, _ := json.MarshalIndent(newer, "", "  ")
	writeEvidenceFixture(t, "acme/widget", 7, "bbbbbbb2", data)

	// A second valid PR by an agent, open, with an empty-CI record.
	agent := evSummaryBundle("acme/widget", 9, evidence.AuthorAgent, created, false, []evidence.Verdict{
		{Perspective: "security", Model: "m2", Verdict: "approve", Confidence: 0.8},
	}, 1, false)
	data, _ = json.MarshalIndent(agent, "", "  ")
	writeEvidenceFixture(t, "acme/widget", 9, "ccccccc3", data)

	// Wrong schema version, corrupt JSON and a non-JSON sibling: the first two
	// count as unreadable, the third is ignored.
	wrong := evSummaryBundle("acme/widget", 10, evidence.AuthorHuman, created, true, nil, 0, true)
	wrong.SchemaVersion = "v0"
	data, _ = json.MarshalIndent(wrong, "", "  ")
	writeEvidenceFixture(t, "acme/widget", 10, "ddddddd4", data)
	writeEvidenceFixture(t, "acme/widget", 11, "eeeeeee5", []byte("{not json"))
	if err := os.WriteFile(filepath.Join(ghpkg.ReviewEvidenceRoot, "README.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	bundles, unreadable, err := loadEvidenceBundles(ghpkg.ReviewEvidenceRoot)
	if err != nil {
		t.Fatal(err)
	}
	if len(bundles) != 3 || unreadable != 2 {
		t.Fatalf("loaded %d bundles, %d unreadable; want 3 and 2", len(bundles), unreadable)
	}

	s := &Server{}
	w := reviewEvidenceRequest(t, s.handleEvidenceSummary, config.RoleOwner, "/api/evidence/summary?repo=acme/widget")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body)
	}
	var resp EvidenceSummary
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.N != 2 || resp.Repo != "acme/widget" {
		t.Errorf("n = %d repo = %q, want 2 acme/widget", resp.N, resp.Repo)
	}
	if o := resp.Outcomes["human"]; o.N != 1 || o.Merged != 1 || o.RevertedWithin14d != 1 {
		t.Errorf("human outcomes = %+v", o)
	}
	if o := resp.Outcomes["agent"]; o.N != 1 || o.Open != 1 || o.RevertedWithin14d != 0 {
		t.Errorf("agent outcomes = %+v", o)
	}
	if resp.ShadowGate.N != 1 || resp.ShadowGate.WouldHaveHeld != 1 {
		t.Errorf("shadow gate = %+v", resp.ShadowGate)
	}
	if resp.Missing.RecordsUnreadable != 2 || resp.Missing.RecordsEmptyCI != 1 {
		t.Errorf("missing = %+v", resp.Missing)
	}
	if resp.SentinelPerWeek.N != 1 {
		t.Errorf("sentinel = %+v", resp.SentinelPerWeek)
	}
}
