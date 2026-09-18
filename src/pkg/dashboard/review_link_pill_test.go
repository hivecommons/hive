package dashboard

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/governor"
)

// TestAttachReviewLinks_StampsReviewedPRsOnly proves the queue view can tell a
// PR the hive has reviewed from one it has not, and that the evidence reaches
// the wire under the names the frontend reads.
func TestAttachReviewLinks_StampsReviewedPRsOnly(t *testing.T) {
	reviewed := github.PullRequest{Repo: "repo", Number: 1, Title: "reviewed", Mergeable: github.MergeableYes}
	repeated := github.PullRequest{Repo: "repo", Number: 2, Title: "reviewed thrice", Mergeable: github.MergeableYes}
	untouched := github.PullRequest{Repo: "repo", Number: 3, Title: "never reviewed", Mergeable: github.MergeableYes}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{reviewed, repeated, untouched}}}

	at := time.Date(2026, 9, 18, 5, 51, 0, 0, time.UTC)
	payload := &StatusPayload{Repos: buildRepos(verdictCfg(), actionable, governor.State{})}
	AttachReviewLinks(payload, map[string]github.ReviewLink{
		github.ReviewLinkKey("repo", 1): {URL: "https://example.test/r1", State: "commented", At: at, Count: 1},
		github.ReviewLinkKey("repo", 2): {URL: "https://example.test/r2", State: "changes_requested", At: at, Count: 3},
	})

	raw, err := json.Marshal(payload.Repos)
	if err != nil {
		t.Fatal(err)
	}
	var repos []struct {
		OpenPrs []map[string]any `json:"openPrs"`
	}
	if err := json.Unmarshal(raw, &repos); err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || len(repos[0].OpenPrs) != 3 {
		t.Fatalf("unexpected repo shape: %s", raw)
	}
	byNumber := map[float64]map[string]any{}
	for _, p := range repos[0].OpenPrs {
		byNumber[p["number"].(float64)] = p
	}

	if byNumber[1]["review_url"] != "https://example.test/r1" {
		t.Fatalf("review_url not stamped: %v", byNumber[1])
	}
	if byNumber[1]["review_state"] != "commented" {
		t.Fatalf("review_state not stamped: %v", byNumber[1])
	}
	if byNumber[1]["reviewed_at"] == nil {
		t.Fatalf("reviewed_at missing: %v", byNumber[1])
	}
	// The repeat count is the duplicate-review signal the pill surfaces.
	if byNumber[2]["review_count"] != float64(3) {
		t.Fatalf("review_count = %v, want 3", byNumber[2]["review_count"])
	}
	// A PR with no recorded review carries no review fields at all, so the
	// frontend renders no pill rather than asserting "not reviewed".
	if _, ok := byNumber[3]["review_url"]; ok {
		t.Fatalf("unreviewed PR got a review_url: %v", byNumber[3])
	}
	// The PR's own fields survive the stamping.
	if byNumber[1]["title"] != "reviewed" || byNumber[1]["mergeable"] != "yes" {
		t.Fatalf("PR fields lost: %v", byNumber[1])
	}
}

// TestAttachReviewLinks_IgnoresEmptyAndMissing keeps a link with no URL from
// producing a pill that points nowhere.
func TestAttachReviewLinks_IgnoresEmptyAndMissing(t *testing.T) {
	pr := github.PullRequest{Repo: "repo", Number: 1, Title: "pr", Mergeable: github.MergeableYes}
	actionable := &github.ActionableResult{PRs: github.PRResult{Items: []github.PullRequest{pr}}}
	payload := &StatusPayload{Repos: buildRepos(verdictCfg(), actionable, governor.State{})}

	AttachReviewLinks(payload, map[string]github.ReviewLink{
		github.ReviewLinkKey("repo", 1): {URL: "", State: "commented"},
	})

	fp, ok := payload.Repos[0].OpenPrs[0].(FrontendPR)
	if !ok {
		t.Fatalf("unexpected entry type %T", payload.Repos[0].OpenPrs[0])
	}
	if fp.ReviewURL != "" || fp.ReviewState != "" {
		t.Fatalf("empty link should not stamp anything: %+v", fp)
	}

	// A nil/empty map is a no-op, not a panic.
	AttachReviewLinks(payload, nil)
	AttachReviewLinks(nil, map[string]github.ReviewLink{})
}
