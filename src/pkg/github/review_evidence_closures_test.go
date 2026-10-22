package github

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
	"github.com/hivecommons/hive/pkg/evidence"
)

func TestCaptureReviewEvidenceClosures(t *testing.T) {
	at := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/repos/o/r/issues/1":
			fmt.Fprintf(w, `{"state":"closed","closed_at":%q,"body":"question"}`, at.Format(time.RFC3339))
		case "/repos/o/r/issues/1/comments":
			fmt.Fprintf(w, `[{"body":%q}]`, clarityGateEvidenceMarker)
		case "/repos/o/other/issues/2/comments":
			fmt.Fprint(w, `[]`)
		case "/repos/o/other/issues/2":
			fmt.Fprintf(w, `{"state":"closed","closed_at":%q,"body":"clear request"}`, at.Format(time.RFC3339))
		case "/repos/o/r/issues/3":
			fmt.Fprintf(w, `{"state":"closed","closed_at":%q}`, at.Add(-time.Hour).Format(time.RFC3339))
		case "/repos/o/r/issues/4":
			fmt.Fprint(w, `{"state":"open"}`)
		case "/repos/o/r/issues/5", "/repos/o/r/issues/7/comments":
			w.WriteHeader(http.StatusForbidden)
		case "/repos/o/r/issues/7":
			fmt.Fprintf(w, `{"state":"closed","closed_at":%q}`, at.Format(time.RFC3339))
		default:
			t.Errorf("unexpected fetch %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := newTestClient(t, srv, "o", []string{"o/r"})
	b := &evidence.Bundle{Repo: "o/r"}
	pr := &gh.PullRequest{Body: gh.Ptr("Closes #1, closes #1. Fixes o/other#2. Resolves #3. Fixes #4. Fixes #5. Refs #6. Closes #7."), MergedAt: &gh.Timestamp{Time: at}}
	c.captureReviewEvidenceClosures(context.Background(), b, pr)
	if calls != 9 || len(b.IssueClosures) != 2 {
		t.Fatalf("calls=%d closures=%+v", calls, b.IssueClosures)
	}
	if !b.IssueClosures[0].ClarityGate || b.IssueClosures[0].Repo != "o/r" || b.IssueClosures[1].ClarityGate || b.IssueClosures[1].Repo != "o/other" {
		t.Fatalf("closures = %+v", b.IssueClosures)
	}
}

func TestRecordReviewEvidenceLateReopenForCapturedClosure(t *testing.T) {
	root := withEvidenceRoot(t)
	at := time.Now().UTC().Add(-25 * 24 * time.Hour)
	b := writeMergedEvidence(t, root, 42, "h42", at)
	b.IssueClosures = []evidence.IssueClosure{{Repo: "o/r", Issue: 7, At: at, ClarityGate: true}}
	if err := evidence.Seal(b, nil); err != nil {
		t.Fatal(err)
	}
	if err := writeReviewEvidence(ReviewEvidencePath(root, "o/r", 42, "h42"), b); err != nil {
		t.Fatal(err)
	}
	c := newChangeFailureClient(t)
	c.RecordReviewEvidenceIssueReopen(IssueClaim{Repo: "o/r", Issue: 7, PRNumber: 42, PRRepo: "o/r", MergedAt: at}, at.Add(20*24*time.Hour))
	got := loadEvidence(t, root, 42, "h42")
	if len(got.IssueReopens) != 1 {
		t.Fatalf("late reopen missing: %+v", got.IssueReopens)
	}
	if _, reopened := foldOne(got).ChangeFailure(); reopened {
		t.Fatal("30-day observation must not change the legacy 14-day failure rate")
	}
}
