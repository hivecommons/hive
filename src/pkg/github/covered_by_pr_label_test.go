package github

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

// coveredLabelFixture simulates a repo's label namespace for
// EnsureCoveredByPRLabel: which names exist, plus what was renamed/created.
type coveredLabelFixture struct {
	existing   map[string]bool
	renameFail bool
	renamed    map[string]string
	created    []string
	gets       atomic.Int32
}

func (f *coveredLabelFixture) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		const prefix = "/repos/hivecommons/hive/labels"
		if !strings.HasPrefix(r.URL.Path, prefix) {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		name, _ := url.PathUnescape(strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, prefix), "/"))
		switch r.Method {
		case http.MethodGet:
			f.gets.Add(1)
			if f.existing[name] {
				_ = json.NewEncoder(w).Encode(map[string]any{"name": name})
				return
			}
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case http.MethodPatch:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			if f.renameFail {
				w.WriteHeader(http.StatusForbidden)
				_, _ = w.Write([]byte(`{"message":"nope"}`))
				return
			}
			if f.renamed == nil {
				f.renamed = map[string]string{}
			}
			f.renamed[name] = body["new_name"]
			delete(f.existing, name)
			f.existing[body["new_name"]] = true
			_ = json.NewEncoder(w).Encode(map[string]any{"name": body["new_name"], "color": body["color"]})
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			var body map[string]string
			_ = json.Unmarshal(raw, &body)
			f.created = append(f.created, body["name"])
			f.existing[body["name"]] = true
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"name": body["name"]})
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}
}

func TestEnsureCoveredByPRLabel(t *testing.T) {
	tests := []struct {
		name        string
		existing    []string
		renameFail  bool
		wantRenamed bool
		wantCreated bool
	}{
		{name: "current label present is a no-op", existing: []string{CoveredByPRLabel}},
		{name: "legacy label is renamed in place", existing: []string{LegacyCoveredByPRLabel}, wantRenamed: true},
		{name: "neither present creates the new label", wantCreated: true},
		{name: "failed rename falls back to create", existing: []string{LegacyCoveredByPRLabel}, renameFail: true, wantCreated: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &coveredLabelFixture{existing: map[string]bool{}, renameFail: tt.renameFail}
			for _, n := range tt.existing {
				f.existing[n] = true
			}
			srv := httptest.NewServer(f.handler(t))
			t.Cleanup(srv.Close)
			c := NewClientForTest(srv.URL, "hivecommons", []string{"hive"}, testLogger())
			if err := c.EnsureCoveredByPRLabel(context.Background(), "hivecommons/hive"); err != nil {
				t.Fatalf("EnsureCoveredByPRLabel: %v", err)
			}
			if got := f.renamed[LegacyCoveredByPRLabel] == CoveredByPRLabel; got != tt.wantRenamed {
				t.Fatalf("renamed = %v (%v), want %v", got, f.renamed, tt.wantRenamed)
			}
			if got := len(f.created) == 1 && f.created[0] == CoveredByPRLabel; got != tt.wantCreated {
				t.Fatalf("created = %v, want created=%v", f.created, tt.wantCreated)
			}
			if !f.existing[CoveredByPRLabel] {
				t.Fatalf("current label missing after ensure: %v", f.existing)
			}
			gets := f.gets.Load()
			if err := c.EnsureCoveredByPRLabel(context.Background(), "hivecommons/hive"); err != nil {
				t.Fatalf("second EnsureCoveredByPRLabel: %v", err)
			}
			if f.gets.Load() != gets {
				t.Fatal("second call was not memoised")
			}
		})
	}
}

func TestHasCoveredByPRLabelAcceptsBothSpellings(t *testing.T) {
	if !HasCoveredByPRLabel([]string{"bug", LegacyCoveredByPRLabel}) {
		t.Fatal("legacy spelling not recognised")
	}
	if !HasCoveredByPRLabel([]string{CoveredByPRLabel}) {
		t.Fatal("current spelling not recognised")
	}
	if HasCoveredByPRLabel([]string{"bug"}) {
		t.Fatal("unrelated label recognised")
	}
	got := removeCoveredByPRLabel([]string{"bug", CoveredByPRLabel, LegacyCoveredByPRLabel})
	if len(got) != 1 || got[0] != "bug" {
		t.Fatalf("removeCoveredByPRLabel = %v", got)
	}
}

// TestSyncIssuePRClaimLabels_LegacyLabelIsNotReAdded covers the transition:
// an issue still carrying the legacy spelling (renamed in place server-side)
// must not get a second label write.
func TestSyncIssuePRClaimLabels_LegacyLabelIsNotReAdded(t *testing.T) {
	var adds atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/labels/"):
			_, _ = w.Write([]byte(`{"name":"ok"}`))
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/issues/7/labels"):
			adds.Add(1)
			_, _ = w.Write([]byte(`[]`))
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
	result.Issues.Items = []Issue{{Repo: "hivecommons/hive", Number: 7, Labels: []string{LegacyCoveredByPRLabel}}}
	SyncIssuePRClaimLabels(context.Background(), c, result, ledger, testLogger())
	if adds.Load() != 0 {
		t.Fatalf("label adds = %d, want 0 for an issue already carrying the legacy spelling", adds.Load())
	}
}
