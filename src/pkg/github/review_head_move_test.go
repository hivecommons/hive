package github

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/review"
)

// Verdicts across a head move (hivecommons/hive#15823): a review dispatched
// at H1 whose verdict arrives after the PR moved to H2.

const (
	headMoveH1 = "3fde88fa3fde88fa3fde88fa3fde88fa3fde88fa"
	headMoveH2 = "e04062a5e04062a5e04062a5e04062a5e04062a5"
	headMoveH3 = "aaaa1111aaaa1111aaaa1111aaaa1111aaaa1111"
)

// headMoveGitHub serves the PR fetch (current head, base) and the two
// base...head compares the same-patch test reads. compares maps a head to
// the files its compare returns.
func headMoveGitHub(t *testing.T, currentHead string, compares map[string][]map[string]any) (*Client, *bytes.Buffer) {
	t.Helper()
	var logBuf bytes.Buffer
	c := &Client{logger: slog.New(slog.NewTextHandler(&logBuf, nil)), reviewerAgent: func(agent string) bool {
		return agent == "reviewer" || agent == "ci-maintainer" || agent == "adjudicator"
	}}
	c.client = gh.NewClient(&http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		var body any
		switch {
		case r.URL.Path == "/repos/o/r/pulls/1":
			body = map[string]any{"number": 1, "title": "t", "body": "b",
				"head": map[string]any{"sha": currentHead}, "base": map[string]any{"ref": "v5"},
				"user": map[string]any{"login": "someone"}}
		case strings.HasPrefix(r.URL.Path, "/repos/o/r/compare/"):
			spec := strings.TrimPrefix(r.URL.Path, "/repos/o/r/compare/")
			_, head, _ := strings.Cut(spec, "...")
			files, ok := compares[head]
			if !ok {
				return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			}
			body = map[string]any{"files": files}
		default:
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader(`{}`))}, nil
		}
		raw, _ := json.Marshal(body)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(raw))}, nil
	})})
	return c, &logBuf
}

func compareFile(path, status, patch string) map[string]any {
	return map[string]any{"filename": path, "status": status, "patch": patch}
}

func collectedHeads(t *testing.T, reportDir string) []string {
	t.Helper()
	artifact, err := review.Collect(reportDir, review.AggregateOptions{})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	var heads []string
	for _, it := range artifact.Items {
		heads = append(heads, it.HeadSHA)
	}
	return heads
}

// The reviewer read the live PR and reported its current head, while the
// dispatch row still names the head it was kicked for: the verdict is the
// one wanted, recorded under the current head, and the row follows it.
func TestRecordReviewVerdictAcceptsReportedCurrentHeadAcrossHeadMove(t *testing.T) {
	reportDir := filepath.Join(t.TempDir(), "reports")
	c, logBuf := headMoveGitHub(t, headMoveH2, nil)
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "reviewer")
	row.HeadSHA = headMoveH1
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, reportDir)

	if heads := collectedHeads(t, reportDir); len(heads) != 1 || heads[0] != headMoveH2 {
		t.Fatalf("verdict heads = %v, want [%s]", heads, headMoveH2)
	}
	out := logBuf.String()
	if strings.Contains(out, "falling back to advisory") {
		t.Fatalf("verdict for the current head fell back to advisory:\n%s", out)
	}
	for _, want := range []string{"verdict accepted across head move", "h1=" + headMoveH1, "h2=" + headMoveH2, "why=reported-current-head"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	state, err := review.LoadDispatchState("")
	if err != nil {
		t.Fatalf("LoadDispatchState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].HeadSHA != headMoveH2 {
		t.Fatalf("pending row not rewritten to the current head: %+v", state.Pending)
	}
}

// The reviewer reported a head that is neither the dispatched one nor the
// PR's current head (the PR moved on again), but it carries the same patch
// as the dispatched head: accepted for the reported head.
func TestRecordReviewVerdictAcceptsSamePatchAcrossHeadMove(t *testing.T) {
	reportDir := filepath.Join(t.TempDir(), "reports")
	same := []map[string]any{compareFile("a.go", "modified", "@@ -1,3 +1,4 @@\n a\n+b\n c")}
	rebased := []map[string]any{compareFile("a.go", "modified", "@@ -10,3 +10,4 @@\n a\n+b\n c")}
	c, logBuf := headMoveGitHub(t, headMoveH3, map[string][]map[string]any{headMoveH1: same, headMoveH2: rebased})
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "reviewer")
	row.HeadSHA = headMoveH1
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, reportDir)

	if heads := collectedHeads(t, reportDir); len(heads) != 1 || heads[0] != headMoveH2 {
		t.Fatalf("verdict heads = %v, want [%s]", heads, headMoveH2)
	}
	if out := logBuf.String(); !strings.Contains(out, "why=same-patch") || strings.Contains(out, "falling back to advisory") {
		t.Fatalf("same-patch head move not accepted:\n%s", out)
	}
}

// A different patch at the reported head is a different change: the verdict
// does not bind to the dispatch row and takes the advisory path as before.
func TestRecordReviewVerdictRejectsDifferentPatchAcrossHeadMove(t *testing.T) {
	reportDir := filepath.Join(t.TempDir(), "reports")
	h1 := []map[string]any{compareFile("a.go", "modified", "@@ -1,3 +1,4 @@\n a\n+b\n c")}
	h2 := []map[string]any{compareFile("a.go", "modified", "@@ -1,3 +1,4 @@\n a\n+B\n c"), compareFile("d.go", "added", "@@ -0,0 +1 @@\n+d")}
	c, logBuf := headMoveGitHub(t, headMoveH3, map[string][]map[string]any{headMoveH1: h1, headMoveH2: h2})
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "reviewer")
	row.HeadSHA = headMoveH1
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer", Report: raw}, reportDir)

	out := logBuf.String()
	if strings.Contains(out, "verdict accepted across head move") {
		t.Fatalf("different patch accepted across head move:\n%s", out)
	}
	if !strings.Contains(out, "falling back to advisory") {
		t.Fatalf("different-patch verdict should fall back to advisory:\n%s", out)
	}
	state, err := review.LoadDispatchState("")
	if err != nil {
		t.Fatalf("LoadDispatchState: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].HeadSHA != headMoveH1 {
		t.Fatalf("pending row must stay at the dispatched head: %+v", state.Pending)
	}
}

// The perspective was dispatched to one reviewer-role agent and a sibling
// reviewer answered it (ci-maintainer for adjudicator's row): the row binds.
func TestRecordReviewVerdictBindsReviewerRoleAgentToAnotherReviewersRow(t *testing.T) {
	reportDir := filepath.Join(t.TempDir(), "reports")
	c, logBuf := headMoveGitHub(t, headMoveH2, nil)
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "adjudicator")
	row.HeadSHA = headMoveH2
	row.HiveAuthored = true
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	recorded := c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "ci-maintainer", Report: raw}, reportDir)

	if len(recorded) != 1 || !recorded[0].HiveAuthored {
		t.Fatalf("verdict did not bind to the sibling reviewer's dispatch row: %+v", recorded)
	}
	if strings.Contains(logBuf.String(), "falling back to advisory") {
		t.Fatalf("reviewer-role verdict fell back to advisory:\n%s", logBuf.String())
	}
}

// A lane that is not a reviewer still binds only to its own row.
func TestRecordReviewVerdictNonReviewerDoesNotBindToAnotherAgentsRow(t *testing.T) {
	reportDir := filepath.Join(t.TempDir(), "reports")
	c, _ := headMoveGitHub(t, headMoveH2, nil)
	row := pendingVerdict("o/r", 1, review.PerspectiveCorrectness, "adjudicator")
	row.HeadSHA = headMoveH2
	withVerdictDispatchState(t, review.DispatchState{Pending: []review.PendingReview{row}})
	raw := validVerdictJSONWithHead(t, "o/r", 1, "correctness", "approve", headMoveH2)

	c.recordReviewVerdict(context.Background(), ReviewRequest{Repo: "o/r", Number: 1, Agent: "docs-writer", Report: raw}, reportDir)

	if matches, _ := filepath.Glob(filepath.Join(reportDir, "*")); len(matches) != 0 {
		t.Fatalf("non-reviewer verdict bound to another agent's row: %v", matches)
	}
}

// An exact row (same agent, same head) wins over a sibling's row and over a
// row for an earlier head, whatever their order in the state.
func TestVerdictDispatchRowTierPrefersExactRow(t *testing.T) {
	report := review.PerspectiveReport{Repo: "o/r", Number: 1, Perspective: review.PerspectiveCorrectness, HeadSHA: headMoveH2}
	req := ReviewRequest{Repo: "o/r", Number: 1, Agent: "reviewer"}
	moves := func(rowHead string) *verdictHeadMove {
		return &verdictHeadMove{From: rowHead, To: headMoveH2, Why: headMoveReportedCurrent}
	}
	cases := []struct {
		agent, head string
		want        int
	}{
		{"reviewer", headMoveH2, rowTierExact},
		{"adjudicator", headMoveH2, rowTierReviewerRole},
		{"reviewer", headMoveH1, rowTierHeadMove},
		{"adjudicator", headMoveH1, rowTierHeadMoveRole},
	}
	for _, tc := range cases {
		got, _ := reviewDispatchRowTier("o/r", 1, tc.head, review.PerspectiveCorrectness, tc.agent, report, req, true, moves)
		if got != tc.want {
			t.Errorf("tier(agent=%s head=%s) = %d, want %d", tc.agent, tc.head[:8], got, tc.want)
		}
	}
	if got, _ := reviewDispatchRowTier("o/r", 1, headMoveH1, review.PerspectiveCorrectness, "adjudicator", report, req, false, moves); got != -1 {
		t.Errorf("non-reviewer bound to another agent's row: tier %d", got)
	}
	if got, _ := reviewDispatchRowTier("o/r", 1, headMoveH1, review.PerspectiveSecurity, "reviewer", report, req, true, moves); got != -1 {
		t.Errorf("row for another perspective bound: tier %d", got)
	}
}

func TestPatchIDEqualIgnoresHunkHeadersOnly(t *testing.T) {
	a := []*gh.CommitFile{{Filename: gh.Ptr("a.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -1,2 +1,3 @@\n x\n+y")}}
	b := []*gh.CommitFile{{Filename: gh.Ptr("a.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -40,2 +41,3 @@\n x\n+y")}}
	if !patchIDEqual(a, b) {
		t.Fatal("rebased hunk read as a different patch")
	}
	c := []*gh.CommitFile{{Filename: gh.Ptr("a.go"), Status: gh.Ptr("modified"), Patch: gh.Ptr("@@ -1,2 +1,3 @@\n x\n+z")}}
	if patchIDEqual(a, c) {
		t.Fatal("different hunk body read as the same patch")
	}
	if patchIDEqual(a, append(b, &gh.CommitFile{Filename: gh.Ptr("b.go"), Status: gh.Ptr("added")})) {
		t.Fatal("extra file read as the same patch")
	}
	if patchIDEqual(a, nil) || !patchIDEqual(nil, nil) {
		t.Fatal(fmt.Sprintf("empty handling wrong: %v %v", patchIDEqual(a, nil), patchIDEqual(nil, nil)))
	}
}
