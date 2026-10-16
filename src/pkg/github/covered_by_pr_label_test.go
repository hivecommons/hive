package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestEnsureCoveredByPRLabelCreatesStageLabelOnce(t *testing.T) {
	var gets, creates atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			gets.Add(1)
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			if body["name"] != CoveredByPRLabel || body["color"] != stagelabels.StageColor {
				t.Fatalf("created %v, want %s in %s", body, CoveredByPRLabel, stagelabels.StageColor)
			}
			creates.Add(1)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": body["name"]})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	for i := 0; i < 2; i++ {
		if err := c.EnsureCoveredByPRLabel(context.Background(), "hivecommons/hive"); err != nil {
			t.Fatalf("EnsureCoveredByPRLabel #%d: %v", i, err)
		}
	}
	if gets.Load() != 1 || creates.Load() != 1 {
		t.Fatalf("gets=%d creates=%d, want 1/1 (memoised)", gets.Load(), creates.Load())
	}
}

func TestCoveredByPRLabelIsTheStageLabel(t *testing.T) {
	if CoveredByPRLabel != stagelabels.CoveredByPR.Label() {
		t.Fatalf("CoveredByPRLabel = %q, stage table says %q", CoveredByPRLabel, stagelabels.CoveredByPR.Label())
	}
}

func TestHasCoveredByPRLabelAcceptsStageAndRetiredSpellings(t *testing.T) {
	for _, labels := range [][]string{
		{"bug", stagelabels.LegacyCoveredByPR},
		{stagelabels.LegacyCoveredByPRASCII},
		{CoveredByPRLabel},
		{"bug", stagelabels.MergeQueue.Label()}, // mirrored PR stage
	} {
		if !HasCoveredByPRLabel(labels) {
			t.Errorf("%v not recognised as covered", labels)
		}
	}
	for _, labels := range [][]string{{"bug"}, {stagelabels.Claimed.Label()}, {stagelabels.Actionable.Label()}} {
		if HasCoveredByPRLabel(labels) {
			t.Errorf("%v wrongly recognised as covered", labels)
		}
	}
	got := removeCoveredByPRLabel([]string{"bug", CoveredByPRLabel, stagelabels.LegacyCoveredByPR, stagelabels.LegacyCoveredByPRASCII})
	if len(got) != 1 || got[0] != "bug" {
		t.Fatalf("removeCoveredByPRLabel = %v", got)
	}
}

// TestSyncIssuePRClaimLabels_StripsRetiredLabelOnly covers the migration: the
// claim sync no longer writes the covered-by-PR label (the issue stage
// reconciler owns stage/040), and an issue still carrying the retired bee
// label loses it in one write.
func TestSyncIssuePRClaimLabels_StripsRetiredLabelOnly(t *testing.T) {
	var adds atomic.Int32
	var removed []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/7/labels"):
			adds.Add(1)
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/issues/7/labels/"):
			removed = append(removed, strings.TrimPrefix(r.URL.Path, "/repos/hivecommons/hive/issues/7/labels/"))
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(r.URL.Path, "/graphql"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"repository": map[string]any{
				"defaultBranchRef": map[string]any{"name": "v5"},
				"pullRequest":      map[string]any{"id": "PR_1", "state": "OPEN", "baseRefName": "v5", "closingIssuesReferences": map[string]any{"nodes": []any{}}},
				"issue":            map[string]any{"id": "I_7", "state": "OPEN"},
			}}})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)
	c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
	ledger := NewClaimLedger("", testLogger())
	ledger.Reconcile([]IssueClaim{{Repo: "hivecommons/hive", Issue: 7, PRNumber: 1, PRRepo: "hivecommons/hive", PRState: "open", Closing: true}}, true)
	result := &ActionableResult{}
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 7, Labels: []string{stagelabels.LegacyCoveredByPR}}}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if adds.Load() != 0 {
		t.Fatalf("label adds = %d, want 0: the stage reconciler writes stage/040", adds.Load())
	}
	if len(removed) != 1 {
		t.Fatalf("removed = %v, want exactly the retired bee label", removed)
	}
	if got := result.Issues.Items[0].Labels; !issueHasLabel(got, CoveredByPRLabel) || issueHasLabel(got, stagelabels.LegacyCoveredByPR) {
		t.Fatalf("in-memory labels = %v, want stage/040 without the retired spelling", got)
	}
}
