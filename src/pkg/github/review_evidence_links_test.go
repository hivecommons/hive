package github

// Tests for the change-failure links (review_evidence_links.go) and the
// author launch metadata stamped on agent-authored bundles.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

func TestReviewEvidenceChangeRefs(t *testing.T) {
	tests := []struct {
		name          string
		text          string
		wantReverts   []int
		wantFollowups []int
	}{
		{name: "github revert button body", text: "Revert \"✨ thing (#5)\"\nReverts o/r#5", wantReverts: []int{5}},
		{name: "revert of", text: "This is a revert of PR #5.", wantReverts: []int{5}},
		{name: "follow-up and regression", text: "Follow-up to #6; regression from pull request #8", wantFollowups: []int{6, 8}},
		{name: "fixes keyword", text: "Fixes #9, fixed: #10", wantFollowups: []int{9, 10}},
		{name: "revert wins over fix", text: "Reverts #5\nFixes #5", wantReverts: []int{5}},
		{name: "other repo and self dropped", text: "Reverts x/y#5\nfixes #7\nfollowup for O/R#11", wantFollowups: []int{11}},
		{name: "duplicates once", text: "fixes #6 and follow up to #6", wantFollowups: []int{6}},
		{name: "bare mention is not a reference", text: "see #5 and (#6)"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			reverts, followups := reviewEvidenceChangeRefs("o/r", 7, tt.text)
			if !reflect.DeepEqual(reverts, tt.wantReverts) || !reflect.DeepEqual(followups, tt.wantFollowups) {
				t.Fatalf("refs = %v / %v, want %v / %v", reverts, followups, tt.wantReverts, tt.wantFollowups)
			}
		})
	}
}

func TestParseReviewEvidenceID(t *testing.T) {
	repo, number, head, ok := parseReviewEvidenceID("o/r#12@abc")
	if !ok || repo != "o/r" || number != 12 || head != "abc" {
		t.Fatalf("parse = %q %d %q %v", repo, number, head, ok)
	}
	for _, bad := range []string{"", "o/r", "o/r#x@abc", "o/r#0@abc", "o/r#3@", "o/r#3"} {
		if _, _, _, ok := parseReviewEvidenceID(bad); ok {
			t.Fatalf("parse(%q) ok, want failure", bad)
		}
	}
}

func TestEvidenceAuthorStampsLaunchMeta(t *testing.T) {
	withEvidenceRoot(t)
	ts := time.Now().UTC().Format(time.RFC3339)
	lines := `{"ts":"` + ts + `","action":"agent_pr_created","detail":"repo=o/r, number=9, author=hive[bot], url=https://x/9, agent=coder, backend=copilot, model=gpt-5.6-luna, effort=high","agent":"coder"}` + "\n" +
		`{"ts":"` + ts + `","action":"agent_pr_created","detail":"repo=o/r, number=10","agent":"scanner"}` + "\n"
	if err := os.WriteFile(reviewVerdictAuditPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("write audit: %v", err)
	}
	app := &gh.User{Login: gh.Ptr("hive[bot]"), Type: gh.Ptr("Bot")}
	tests := []struct {
		name   string
		number int
		user   *gh.User
		want   evidence.Author
	}{
		{name: "agent with launch metadata", number: 9, user: app,
			want: evidence.Author{Login: "hive[bot]", Kind: evidence.AuthorAgent, Agent: "coder", Backend: "copilot", Model: "gpt-5.6-luna"}},
		{name: "agent without recorded model", number: 10, user: app,
			want: evidence.Author{Login: "hive[bot]", Kind: evidence.AuthorAgent, Agent: "scanner"}},
		{name: "human carries no model", number: 7, user: &gh.User{Login: gh.Ptr("alice"), Type: gh.Ptr("User")},
			want: evidence.Author{Login: "alice", Kind: evidence.AuthorHuman}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := evidenceAuthor("o/r", tt.number, tt.user); got != tt.want {
				t.Fatalf("author = %+v, want %+v", got, tt.want)
			}
		})
	}
	if got := auditAuthorAgentForPR("o/r", 9, reviewVerdictAuditPath, time.Time{}); got != "coder" {
		t.Fatalf("auditAuthorAgentForPR = %q, want coder", got)
	}
}

// writeMergedEvidence writes a bundle for o/r#number@head, merged at mergedAt
// when it is non-zero.
func writeMergedEvidence(t *testing.T, root string, number int, head string, mergedAt time.Time) *evidence.Bundle {
	t.Helper()
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	b := &evidence.Bundle{
		SchemaVersion: evidence.SchemaVersion,
		ID:            evidence.BundleID("o/r", number, head),
		Repo:          "o/r",
		Number:        number,
		Author:        evidence.Author{Login: "alice", Kind: evidence.AuthorHuman},
		BaseSHA:       "base000",
		HeadSHA:       head,
		CreatedAt:     created,
		UpdatedAt:     created,
	}
	if !mergedAt.IsZero() {
		b.Merge = &evidence.MergeEvent{Actor: "alice", Method: "squash", At: mergedAt, SHA: "m-" + head}
		b.UpdatedAt = mergedAt
	}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", number, head), b); err != nil {
		t.Fatal(err)
	}
	return b
}

func newEvidenceLinkServer(t *testing.T, title, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/pulls/7":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"number": 7, "title": title, "body": body, "head": map[string]any{"sha": "h7"},
				"merged_by": map[string]any{"login": "bob"},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/repos/o/r/commits/h7/check-runs":
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "check_runs": []any{}})
		case r.Method == http.MethodGet:
			_ = json.NewEncoder(w).Encode([]any{})
		case r.Method == http.MethodPost && r.URL.Path == "/repos/o/r/issues/7/comments":
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": 1})
		default:
			t.Logf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRecordReviewEvidenceMergeLinksRevertsAndFollowups(t *testing.T) {
	root := withEvidenceRoot(t)
	now := time.Now().UTC()
	writeMergedEvidence(t, root, 7, "h7", time.Time{})
	writeMergedEvidence(t, root, 5, "h5", now.Add(-24*time.Hour))
	writeMergedEvidence(t, root, 6, "h6", now.Add(-48*time.Hour))
	stale := writeMergedEvidence(t, root, 4, "h4", now.Add(-20*24*time.Hour))
	open := writeMergedEvidence(t, root, 3, "h3", time.Time{})

	body := "Reverts o/r#5\n\nFollow-up to #6. Also fixes #4, fixes #3 and fixes #99."
	c := newTestClient(t, newEvidenceLinkServer(t, `Revert "thing"`, body), "o", []string{"o/r"})
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })

	c.RecordPRMergedAudit("o/r", 7, "squash", "m7", PRAuditPathSweep)

	merged := mustLoadBundle(t, root, "h7")
	if merged.Merge == nil {
		t.Fatal("merge not recorded")
	}
	if want := []string{evidence.BundleID("o/r", 5, "h5")}; !reflect.DeepEqual(merged.RevertOf, want) {
		t.Fatalf("revert_of = %v, want %v", merged.RevertOf, want)
	}
	if want := []string{evidence.BundleID("o/r", 6, "h6")}; !reflect.DeepEqual(merged.FollowupOf, want) {
		t.Fatalf("followup_of = %v, want %v", merged.FollowupOf, want)
	}
	if h, _ := evidence.Hash(merged); h != merged.Hash {
		t.Fatalf("merged bundle not resealed")
	}

	loadPR := func(number int, head string) *evidence.Bundle {
		t.Helper()
		b, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", number, head))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	reverted := loadPR(5, "h5")
	if want := []string{merged.ID}; !reflect.DeepEqual(reverted.RevertedBy, want) || len(reverted.FollowedUpBy) != 0 {
		t.Fatalf("PR 5 reverted_by = %v followed_up_by = %v, want %v", reverted.RevertedBy, reverted.FollowedUpBy, want)
	}
	if err := reverted.Validate(); err != nil {
		t.Fatalf("reverted bundle invalid: %v", err)
	}
	if h, _ := evidence.Hash(reverted); h != reverted.Hash {
		t.Fatalf("reverted bundle not resealed")
	}
	fixed := loadPR(6, "h6")
	if want := []string{merged.ID}; !reflect.DeepEqual(fixed.FollowedUpBy, want) || len(fixed.RevertedBy) != 0 {
		t.Fatalf("PR 6 followed_up_by = %v reverted_by = %v, want %v", fixed.FollowedUpBy, fixed.RevertedBy, want)
	}
	if got := loadPR(4, "h4"); got.Hash != stale.Hash {
		t.Fatalf("PR merged outside the window was linked: %+v", got)
	}
	if got := loadPR(3, "h3"); got.Hash != open.Hash {
		t.Fatalf("unmerged PR was linked: %+v", got)
	}

	// A replayed merge adds no duplicate links and changes no bundle.
	c.RecordPRMergedAudit("o/r", 7, "squash", "m7", PRAuditPathRelay)
	for _, b := range []*evidence.Bundle{merged, reverted, fixed} {
		if again := loadPR(b.Number, b.HeadSHA); again.Hash != b.Hash {
			t.Fatalf("replayed merge changed %s", b.ID)
		}
	}
}

func TestRecordReviewEvidenceMergeWithoutReferencesLinksNothing(t *testing.T) {
	root := withEvidenceRoot(t)
	writeMergedEvidence(t, root, 7, "h7", time.Time{})
	prior := writeMergedEvidence(t, root, 5, "h5", time.Now().UTC().Add(-time.Hour))
	c := newTestClient(t, newEvidenceLinkServer(t, "✨ unrelated", "Mentions #5 in passing."), "o", []string{"o/r"})
	c.SetReviewEvidence(func(string) ReviewEvidenceSettings { return ReviewEvidenceSettings{Enabled: true} })

	c.RecordPRMergedAudit("o/r", 7, "squash", "m7", PRAuditPathSweep)

	merged := mustLoadBundle(t, root, "h7")
	if merged.Merge == nil || len(merged.RevertOf)+len(merged.FollowupOf) != 0 {
		t.Fatalf("merged bundle = merge %+v revert_of %v followup_of %v", merged.Merge, merged.RevertOf, merged.FollowupOf)
	}
	got, err := LoadReviewEvidence(ReviewEvidencePath(root, "o/r", 5, "h5"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Hash != prior.Hash {
		t.Fatal("unreferenced bundle changed")
	}
}
