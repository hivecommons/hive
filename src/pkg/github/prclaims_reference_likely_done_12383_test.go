package github

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A merged reference-only claim ("Refs #N") must not label the issue
// hive/likely-done: it never asserted it finished the issue, Settled() and
// close-on-merge both exclude it, and the #11931 rule says likely-done needs a
// closing keyword. hivecommons/hive#12316 sat in "Confirm & close" this way
// while half its work was still an open PR (hivecommons/hive#12383).
func TestAnnotateIssueWithClaim_MergedReferenceIsPartiallyAddressed12383(t *testing.T) {
	issue := Issue{Repo: "o/r", Number: 12316, Labels: []string{CoveredByPRLabel, LikelyDoneLabel}}
	annotateIssueWithClaim(&issue, IssueClaim{
		Repo: "o/r", Issue: 12316, PRRepo: "o/r", PRNumber: 12320,
		Reference: true, MergedPR: true, MergedAt: time.Now().Add(-time.Hour),
	})
	if issueHasLabel(issue.Labels, LikelyDoneLabel) {
		t.Fatalf("likely-done applied for a merged reference-only claim: %v", issue.Labels)
	}
	if !issueHasLabel(issue.Labels, PartiallyAddressedLabel) {
		t.Fatalf("partially-addressed missing for a merged reference-only claim: %v", issue.Labels)
	}
	if issueHasLabel(issue.Labels, CoveredByPRLabel) {
		t.Fatalf("covered-by-pr survived the merge: %v", issue.Labels)
	}
	if issue.ClaimContext == nil || !issue.ClaimContext.MergedPR || !issue.ClaimContext.Reference {
		t.Fatalf("claim context = %+v, want merged reference context so the kick still says verify once", issue.ClaimContext)
	}
}

func TestSyncIssuePRClaimLabels_MergedReferenceSwapsLikelyDoneForPartial12383(t *testing.T) {
	var (
		mu      sync.Mutex
		added   []string
		removed []string
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/12316/labels"):
			raw, _ := io.ReadAll(r.Body)
			for _, l := range []string{PartiallyAddressedLabel, LikelyDoneLabel, CoveredByPRLabel} {
				if strings.Contains(string(raw), l) {
					added = append(added, l)
				}
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/12316/labels/"):
			removed = append(removed, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:])
			w.WriteHeader(http.StatusOK)
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "o", []string{"r"}, testLogger())
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "o/r", Issue: 12316, PRNumber: 12320, PRRepo: "o/r",
		Reference: true, MergedPR: true, MergedAt: now.Add(-time.Hour), ObservedAt: now, FirstObservedAt: now.Add(-time.Hour),
	}}, true)
	// The issue already carries the wrong label from an earlier binary.
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "o/r", Number: 12316, Labels: []string{LikelyDoneLabel}}}, Count: 1}}

	SyncIssuePRClaimLabels(context.Background(), client, result, ledger, testLogger())

	mu.Lock()
	defer mu.Unlock()
	if len(added) != 1 || added[0] != PartiallyAddressedLabel {
		t.Fatalf("labels added = %v, want only %q", added, PartiallyAddressedLabel)
	}
	if len(removed) != 1 || !strings.Contains(removed[0], "likely-done") {
		t.Fatalf("labels removed = %v, want the stale likely-done", removed)
	}
	got := result.Issues.Items[0].Labels
	if issueHasLabel(got, LikelyDoneLabel) || !issueHasLabel(got, PartiallyAddressedLabel) {
		t.Fatalf("in-memory labels = %v, want partially-addressed without likely-done", got)
	}
}

// A merged strong claim that already carries hive/likely-done is not re-added
// on every tick; the covered-by-PR branch already made that economy.
func TestSyncIssuePRClaimLabels_LikelyDoneNotReAddedWhenCarried12383(t *testing.T) {
	var posts int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/issues/7/labels"):
			posts++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[]`))
		default:
			t.Errorf("unexpected GitHub API call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	client := NewClientForTest(server.URL, "o", []string{"r"}, testLogger())
	ledger := NewClaimLedger("", testLogger())
	now := time.Now()
	ledger.Reconcile([]IssueClaim{{
		Repo: "o/r", Issue: 7, PRNumber: 45, PRRepo: "o/r",
		Closing: true, MergedPR: true, MergedAt: now.Add(-time.Hour), ObservedAt: now, FirstObservedAt: now.Add(-time.Hour),
	}}, true)
	result := &ActionableResult{Issues: IssueResult{Items: []Issue{{Repo: "o/r", Number: 7, Labels: []string{LikelyDoneLabel}}}, Count: 1}}

	SyncIssuePRClaimLabels(context.Background(), client, result, ledger, testLogger())

	if posts != 0 {
		t.Fatalf("likely-done re-added %d time(s) although already carried", posts)
	}
	if !issueHasLabel(result.Issues.Items[0].Labels, LikelyDoneLabel) {
		t.Fatalf("labels = %v, want likely-done kept", result.Issues.Items[0].Labels)
	}
}
