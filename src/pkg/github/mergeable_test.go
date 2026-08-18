package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The bug these tests pin down:
//
// listActionablePRs used to set Mergeable from pr.GetMergeable() on a
// PullRequests.List response. GitHub never populates "mergeable" on the list
// endpoint — it is computed per-PR and returned only by the single-PR GET —
// so GetMergeable() read a nil pointer and returned false for every PR. The
// field was a bool, so "never populated" and "GitHub says no" were the same
// value, and merge-eligible.json reported every PR as unmergeable forever.

// TestMergeableZeroValueIsUnknown is the guard that makes the failure loud.
// If Mergeable is ever reverted to a bool, its zero value becomes false —
// a definitive "not mergeable" — and this test fails.
func TestMergeableZeroValueIsUnknown(t *testing.T) {
	var pr PullRequest
	if pr.Mergeable != MergeableUnknown {
		t.Fatalf("zero-value Mergeable = %q, want MergeableUnknown", pr.Mergeable)
	}
	if pr.Mergeable == MergeableNo {
		t.Fatal("zero-value Mergeable must never equal MergeableNo: an unpopulated field would silently gate every PR shut")
	}
}

// TestMergeableFromState covers the mapping, most importantly that UNSTABLE
// counts as mergeable — this project ignores non-required checks (Playwright,
// tide), and GitHub reports "unstable" exactly when the PR is cleanly
// mergeable but such a check is outstanding.
func TestMergeableFromState(t *testing.T) {
	boolPtr := func(b bool) *bool { return &b }

	tests := []struct {
		name  string
		state string
		raw   *bool
		want  Mergeable
	}{
		{"unstable counts as mergeable", "unstable", boolPtr(true), MergeableYes},
		{"unstable mergeable even if raw bool absent", "unstable", nil, MergeableYes},
		{"clean is mergeable", "clean", boolPtr(true), MergeableYes},
		{"has_hooks is mergeable", "has_hooks", boolPtr(true), MergeableYes},
		{"dirty is not mergeable", "dirty", boolPtr(false), MergeableNo},
		{"blocked is not mergeable", "blocked", boolPtr(false), MergeableNo},
		{"behind is not mergeable", "behind", nil, MergeableNo},
		{"unknown state with no raw bool is unknown", "unknown", nil, MergeableUnknown},
		{"empty state with no raw bool is unknown", "", nil, MergeableUnknown},
		{"unknown state falls back to raw true", "unknown", boolPtr(true), MergeableYes},
		{"unknown state falls back to raw false", "unknown", boolPtr(false), MergeableNo},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := mergeableFromState(tt.state, tt.raw); got != tt.want {
				t.Errorf("mergeableFromState(%q, %v) = %q, want %q", tt.state, tt.raw, got, tt.want)
			}
		})
	}
}

// TestEnrichCIStatus_PopulatesMergeableFromSinglePRGet is the regression test
// that would have caught the original bug. The fake list endpoint omits
// "mergeable" exactly as GitHub does; only the single-PR GET supplies it. A
// build that reads mergeability from the list response leaves this at
// MergeableUnknown and the test fails.
func TestEnrichCIStatus_PopulatesMergeableFromSinglePRGet(t *testing.T) {
	var singleGetCalls int

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		// Single-PR GET: the ONLY endpoint that returns mergeable state.
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			singleGetCalls++
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":          1,
				"mergeable":       true,
				"mergeable_state": "unstable",
			})
		case strings.Contains(r.URL.Path, "/check-runs"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 1,
				"check_runs": []map[string]interface{}{
					{"name": "build", "status": "completed", "conclusion": "success"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)

	if singleGetCalls == 0 {
		t.Fatal("EnrichCIStatus never issued the single-PR GET; mergeability cannot come from the list endpoint")
	}
	if prs[0].Mergeable != MergeableYes {
		t.Errorf("Mergeable = %q, want %q (mergeable_state=unstable must count as mergeable)", prs[0].Mergeable, MergeableYes)
	}
	if prs[0].CIStatus != "success" {
		t.Errorf("CIStatus = %q, want success", prs[0].CIStatus)
	}
}

// A dirty PR must come back as a definitive "no".
func TestEnrichCIStatus_MergeableDirtyIsNo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls/1"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 1, "mergeable": false, "mergeable_state": "dirty",
			})
		case strings.Contains(r.URL.Path, "/check-runs"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 1,
				"check_runs": []map[string]interface{}{
					{"name": "build", "status": "completed", "conclusion": "success"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)

	if prs[0].Mergeable != MergeableNo {
		t.Errorf("Mergeable = %q, want %q for a dirty PR", prs[0].Mergeable, MergeableNo)
	}
}

// When the single-PR GET fails we must stay at Unknown, never fabricate "no".
func TestEnrichCIStatus_MergeableUnknownOnFetchError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/check-runs") {
			json.NewEncoder(w).Encode(map[string]interface{}{
				"total_count": 1,
				"check_runs": []map[string]interface{}{
					{"name": "build", "status": "completed", "conclusion": "success"},
				},
			})
			return
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)

	if prs[0].Mergeable != MergeableUnknown {
		t.Errorf("Mergeable = %q, want %q when the mergeability fetch fails", prs[0].Mergeable, MergeableUnknown)
	}
}

// The JSON wire form must round-trip as a string, not a bool. This pins the
// marker contract so a consumer can distinguish "unknown" from "no".
func TestMergeableJSONWireFormat(t *testing.T) {
	data, err := json.Marshal(PullRequest{Number: 1, Mergeable: MergeableYes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(data), `"mergeable":"yes"`) {
		t.Errorf("marshalled PR = %s, want mergeable as the string \"yes\"", data)
	}
	if strings.Contains(string(data), `"mergeable":false`) {
		t.Error("mergeable serialised as a bool: unknown and no are indistinguishable again")
	}
}
