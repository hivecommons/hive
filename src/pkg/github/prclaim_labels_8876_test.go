package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stageengine/stageenginetest"
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

	SyncIssuePRClaimLabels(context.Background(), nil, result, ledger, testLogger())
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
	if suppressed := FilterClaimedIssues(result, ledger, nil, testLogger()); suppressed != 1 {
		t.Fatalf("suppressed = %d, want 1", suppressed)
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

// A claim that became merged leaves a stale stage/040 on the issue; the sync
// hands the stage engine an Uncovered fact (ADR-0025) instead of deleting the
// label itself. Through the real reconciler the fact takes 040 off: a merged
// claim's issue wears likely-done and leaves the stage pipeline.
func TestIssuePRClaimLabels8876_RemovesStaleCoveredLabelWhenMerged(t *testing.T) {
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
			if strings.Contains(r.URL.Path, "covered") {
				t.Error("the sync must not delete stage/040 itself; the stage engine owns stage labels")
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "acme", []string{"widgets"}, testLogger())
	rec := &stageenginetest.Recorder{}
	client.SetStageFactSink(rec)
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "acme/widgets", Issue: 7, PRNumber: 45, PRRepo: "acme/widgets",
		MergedPR: true, ObservedAt: now, FirstObservedAt: now,
	}}, true)
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "acme/widgets", Number: 7, Labels: []string{CoveredByPRLabel}}}, Count: 1}}

	SyncIssuePRClaimLabels(context.Background(), client, result, ledger, testLogger())

	facts := rec.All()
	if len(facts) != 1 || facts[0].Method != "Uncovered" || facts[0].Ref != client.IssueStageRef("acme/widgets", 7) ||
		facts[0].PR == nil || *facts[0].PR != client.PRStageRef("acme/widgets", 45) {
		t.Fatalf("facts = %+v, want one Uncovered(acme/widgets#7, by PR #45)", facts)
	}
	if issueHasLabel(result.Issues.Items[0].Labels, CoveredByPRLabel) {
		t.Fatalf("payload labels = %v, want stage/040 gone", result.Issues.Items[0].Labels)
	}

	// The same fact through the real reconciler: 040 comes off a merged
	// claim's issue (likely-done takes it out of the pipeline).
	sink, writer := stageenginetest.NewSink(stageenginetest.Options{Issue: IssueDeriver()})
	ref := client.IssueStageRef("acme/widgets", 7)
	writer.Seed(ref, CoveredByPRLabel, LikelyDoneLabel)
	if err := sink.Uncovered(context.Background(), ref, client.PRStageRef("acme/widgets", 45)); err != nil {
		t.Fatal(err)
	}
	if got := writer.LabelsOf(ref); strings.Join(got, ",") != LikelyDoneLabel {
		t.Fatalf("labels after Uncovered = %v, want likely-done only", got)
	}
}
