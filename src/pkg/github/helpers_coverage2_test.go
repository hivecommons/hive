package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

func TestChecksForRequired(t *testing.T) {
	app7 := int64(7)
	app9 := int64(9)
	anyApp := int64(-1)
	act := &PRCIActivity{
		Head: "abc",
		Checks: []PRActivityCheck{
			{Name: "unit", State: "completed", Conclusion: "success", Head: "abc", AppID: &app7},
			{Name: "unit", State: "completed", Conclusion: "success", Head: "old", AppID: &app7},
			{Name: "unit", State: "completed", Conclusion: "failure", Head: "ABC", AppID: &app9},
			{Name: "unit", State: "completed", Conclusion: "success"},
			{Name: "lint", State: "completed", Conclusion: "success"},
		},
	}

	if got := act.ChecksForRequired("lint"); len(got) != 1 {
		t.Fatalf("unconstrained lint: got %d checks, want 1", len(got))
	}
	if got := act.ChecksForRequired("unit"); len(got) != 3 {
		t.Fatalf("unconstrained unit skips stale head only: got %d, want 3", len(got))
	}

	act.RequiredChecks = []PRRequiredCheck{{Name: "unit", AppID: &app7, AppKnown: true}}
	got := act.ChecksForRequired("unit")
	if len(got) != 2 {
		t.Fatalf("pinned app: got %d checks, want 2 (match + unknown-producer)", len(got))
	}
	if got[0].AppID == nil || *got[0].AppID != app7 || got[0].Conclusion != "success" {
		t.Fatalf("pinned app first check = %+v", got[0])
	}
	if got[1].State != "unknown" || got[1].Conclusion != "" {
		t.Fatalf("nil-app check under pinned App should be unknown, got %+v", got[1])
	}

	act.RequiredChecks = []PRRequiredCheck{{Name: "unit", AppID: &anyApp, AppKnown: true}}
	if got := act.ChecksForRequired("unit"); len(got) != 3 {
		t.Fatalf("wildcard app: got %d, want 3", len(got))
	}

	act.RequiredChecks = []PRRequiredCheck{{Name: "unit", AppKnown: false}}
	got = act.ChecksForRequired("unit")
	if len(got) != 3 {
		t.Fatalf("unknown app: got %d, want 3", len(got))
	}
	for _, c := range got {
		if c.State != "unknown" {
			t.Fatalf("unknown required App should mark every check unknown, got %+v", c)
		}
	}
}

func TestRequiredChecksSortedKeysAndRepoResolver(t *testing.T) {
	if got := requiredChecksSortedKeys(map[string]bool{"b": true, "a": false}); len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("requiredChecksSortedKeys = %v", got)
	}

	var nilClient *Client
	nilClient.SetRequiredChecksForRepo(nil)
	if set, ok := nilClient.ConfigRequiredChecksForRepo("o/r"); set != nil || ok {
		t.Fatalf("nil client should report no config, got %v %v", set, ok)
	}

	c := &Client{}
	if _, ok := c.ConfigRequiredChecksForRepo("o/r"); ok {
		t.Fatal("empty client should not declare required checks")
	}
	c.SetRequiredChecks(map[string]bool{"unit": true})
	if set, ok := c.ConfigRequiredChecksForRepo("o/r"); !ok || !set["unit"] {
		t.Fatalf("global fallback = %v %v", set, ok)
	}
	c.SetRequiredChecksForRepo(func(repo string) (map[string]bool, bool) {
		if repo == "o/special" {
			return map[string]bool{"lint": true}, true
		}
		return nil, false
	})
	if set, ok := c.ConfigRequiredChecksForRepo("o/special"); !ok || !set["lint"] {
		t.Fatalf("per-repo override = %v %v", set, ok)
	}
	if _, ok := c.ConfigRequiredChecksForRepo("o/other"); ok {
		t.Fatal("resolver's negative answer should win over global fallback")
	}
}

func TestReporterTrustHoldSummaryAndProvenance(t *testing.T) {
	none := ReporterTrust{}
	if none.HoldSummary() != "" || none.HoldProvenance(time.Now()) != nil {
		t.Fatal("unheld trust must yield no summary and no provenance")
	}

	held := ReporterTrust{Held: true, Issue: 15017, Reporter: "Karibhou", Association: " contributor "}
	want := stagelabels.ReporterTrustReason(15017, "Karibhou", "contributor")
	if got := held.HoldSummary(); got != want {
		t.Fatalf("HoldSummary = %q, want %q", got, want)
	}
	since := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	prov := held.HoldProvenance(since)
	if prov == nil || prov.By != stagelabels.HoldByHive || prov.Source != stagelabels.HoldSourceReporterTrust ||
		prov.Reason != want || !prov.Since.Equal(since) || prov.Issue != 15017 || prov.Reporter != "Karibhou" || prov.Association != "CONTRIBUTOR" {
		t.Fatalf("HoldProvenance = %+v", prov)
	}

	blank := ReporterTrust{Held: true, Issue: 1, Reporter: "x"}
	if got := blank.HoldSummary(); got != stagelabels.ReporterTrustReason(1, "x", "association unknown") {
		t.Fatalf("blank association summary = %q", got)
	}
}

func TestIssueTitleState(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/issues/1", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 1, "title": "Fix it", "state": "OPEN"})
	})
	mux.HandleFunc("/repos/acme/widget/issues/2", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "title": "A PR", "state": "open", "pull_request": map[string]any{"url": "x"}})
	})
	mux.HandleFunc("/repos/acme/widget/issues/3", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	ctx := context.Background()

	title, state, isPR, err := c.IssueTitleState(ctx, "acme", "widget", 1)
	if err != nil || title != "Fix it" || state != "open" || isPR {
		t.Fatalf("issue: %q %q %v %v", title, state, isPR, err)
	}
	title, state, isPR, err = c.IssueTitleState(ctx, "acme", "widget", 2)
	if err != nil || title != "" || state != "" || !isPR {
		t.Fatalf("pr: %q %q %v %v", title, state, isPR, err)
	}
	if _, _, _, err := c.IssueTitleState(ctx, "acme", "widget", 3); err == nil {
		t.Fatal("missing issue should error")
	}
	var nilClient *Client
	if _, _, _, err := nilClient.IssueTitleState(ctx, "acme", "widget", 1); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v", err)
	}
}

func TestPRConflictCandidatesAndReviewDeltaDiff(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widget/compare/main...feat", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
			{"filename": "a.go", "additions": 1, "deletions": 2},
			{"filename": "b.go", "previous_filename": "old.go"},
		}})
	})
	mux.HandleFunc("/repos/acme/widget/compare/feat...main", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"files": []map[string]any{
			{"filename": "old.go"}, {"filename": "c.go"},
		}})
	})
	mux.HandleFunc("/repos/acme/widget/compare/main...broken", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	})
	mux.HandleFunc("/repos/acme/widget/compare/broken...main", func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	ctx := context.Background()

	got, err := c.PRConflictCandidates(ctx, "acme/widget", "main", "feat")
	if err != nil || len(got) != 1 || got[0] != "old.go" {
		t.Fatalf("PRConflictCandidates = %v %v", got, err)
	}
	if _, err := c.PRConflictCandidates(ctx, "acme/widget", "main", "broken"); err == nil {
		t.Fatal("compare failure should surface")
	}
	if _, err := c.PRConflictCandidates(ctx, "acme/widget", "broken", "main"); err == nil {
		t.Fatal("reverse compare failure should surface")
	}
	var nilClient *Client
	if _, err := nilClient.PRConflictCandidates(ctx, "acme/widget", "main", "feat"); err != ErrNoGitHubClient {
		t.Fatalf("nil client err = %v", err)
	}

	delta := c.ReviewDeltaDiff(ctx, "acme/widget", "", "main", "feat")
	if !delta.Known || len(delta.Files) != 2 || delta.Files[0].Path != "a.go" || delta.Files[0].Additions != 1 || delta.Files[0].Deletions != 2 {
		t.Fatalf("ReviewDeltaDiff = %+v", delta)
	}
	if d := c.ReviewDeltaDiff(ctx, "acme/widget", "", "main", "broken"); d.Known {
		t.Fatalf("failed compare must be unknown, got %+v", d)
	}
	if d := c.ReviewDeltaDiff(ctx, "acme/widget", "", "same", "same"); d.Known || d.PreviousHead != "same" {
		t.Fatalf("same head must be unknown, got %+v", d)
	}
	if d := nilClient.ReviewDeltaDiff(ctx, "acme/widget", "", "main", "feat"); d.Known {
		t.Fatalf("nil client must be unknown, got %+v", d)
	}
}
