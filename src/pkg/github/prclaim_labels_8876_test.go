package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestIssuePRClaimLabels8876_LinkedPRPayloadAndPendingLabels(t *testing.T) {
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "acme/widgets", Issue: 7,
		PRNumber: 44, PRRepo: "acme/widgets", PRURL: "https://github.com/acme/widgets/pull/44",
		PRAuthor: "dev", ExternalAuthor: true, ObservedAt: now, FirstObservedAt: now,
	}}, true)
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "acme/widgets", Number: 7, Title: "fix sleep"}}, Count: 1}}

	if suppressed := FilterClaimedIssues(result, ledger, nil, testLogger()); suppressed != 0 {
		t.Fatalf("suppressed = %d, want 0", suppressed)
	}
	issue := result.Issues.Items[0]
	if !issueHasLabel(issue.Labels, CoveredByPRLabel) || issueHasLabel(issue.Labels, LikelyDoneLabel) {
		t.Fatalf("labels = %v, want covered-by-pr only", issue.Labels)
	}
	if len(issue.LinkedPRs) != 1 || issue.LinkedPRs[0].Number != 44 || issue.LinkedPRs[0].Merged || issue.LinkedPRs[0].Closing {
		t.Fatalf("linked PRs = %+v, want non-closing open #44", issue.LinkedPRs)
	}
	raw, err := json.Marshal(issue)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(raw) || !strings.Contains(string(raw), "linked_prs") {
		t.Fatalf("issue JSON missing linked_prs: %s", raw)
	}
}

func TestIssuePRClaimLabels8876_MergedClaimIsLikelyDone(t *testing.T) {
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "acme/widgets", Issue: 7,
		PRNumber: 45, PRRepo: "acme/widgets", PRURL: "https://github.com/acme/widgets/pull/45",
		PRAuthor: "dev", Closing: true, MergedPR: true, MergedAt: now.Add(-time.Hour), ObservedAt: now, FirstObservedAt: now.Add(-time.Hour),
	}}, true)
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{CoveredByPRLabel}}}, Count: 1}}

	SyncIssuePRClaimLabels(context.Background(), nil, result, ledger, testLogger())
	issue := result.Issues.Items[0]
	if issueHasLabel(issue.Labels, CoveredByPRLabel) || !issueHasLabel(issue.Labels, LikelyDoneLabel) {
		t.Fatalf("labels = %v, want likely-done only", issue.Labels)
	}
	if len(issue.LinkedPRs) != 1 || !issue.LinkedPRs[0].Merged || issue.LinkedPRs[0].State != "merged" || !issue.LinkedPRs[0].Closing {
		t.Fatalf("linked PRs = %+v, want closing merged #45", issue.LinkedPRs)
	}
}

func TestIssuePRClaimLabels8876_RemovesStaleCoveredLabelWhenMerged(t *testing.T) {
	var deletedCovered bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/7/labels"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			if strings.Contains(r.URL.Path, "covered-by-pr") {
				deletedCovered = true
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "acme", []string{"widgets"}, testLogger())
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "acme/widgets", Issue: 7, PRNumber: 45, PRRepo: "acme/widgets",
		MergedPR: true, ObservedAt: now, FirstObservedAt: now,
	}}, true)
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{CoveredByPRLabel}}}, Count: 1}}

	SyncIssuePRClaimLabels(context.Background(), client, result, ledger, testLogger())

	if !deletedCovered {
		t.Fatal("stale covered-by-pr label was not removed after claim became merged")
	}
}
