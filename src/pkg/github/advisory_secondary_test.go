package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// secondaryReportForge is a stateful fake GitHub for per-repo advisory
// reports (#18448). Unlike advisoryRestartForge it honours the list's state
// and labels filters and keeps each issue's body, so the closed-report and
// marker rules are exercised against what GitHub would return.
type secondaryReportForge struct {
	mu          sync.Mutex
	issues      map[string][]map[string]any
	noIssues    map[string]bool
	creates     map[string]int
	edits       map[string]int
	next        int
	lastCreated map[string]any
}

func newSecondaryReportForge() *secondaryReportForge {
	return &secondaryReportForge{
		issues:   map[string][]map[string]any{},
		noIssues: map[string]bool{},
		creates:  map[string]int{},
		edits:    map[string]int{},
		next:     3000,
	}
}

func (f *secondaryReportForge) seed(full string, number int, state, body string, labelled bool) {
	labels := []map[string]any{}
	if labelled {
		labels = append(labels, map[string]any{"name": advisoryLabelName})
	}
	f.issues[full] = append(f.issues[full], map[string]any{
		"number": number, "title": advisoryTitle, "state": state, "body": body, "labels": labels,
	})
}

func secondaryForgeIssueHasLabel(issue map[string]any, name string) bool {
	labels, _ := issue["labels"].([]map[string]any)
	for _, l := range labels {
		if l["name"] == name {
			return true
		}
	}
	return false
}

func (f *secondaryReportForge) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		items := []map[string]any{}
		for _, field := range strings.Fields(r.URL.Query().Get("q")) {
			if full, ok := strings.CutPrefix(field, "repo:"); ok {
				items = append(items, f.issues[full]...)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": len(items), "items": items})
	})
	mux.HandleFunc("/repos/{owner}/{repo}", func(w http.ResponseWriter, r *http.Request) {
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		_ = json.NewEncoder(w).Encode(map[string]any{"full_name": full, "has_issues": !f.noIssues[full]})
	})
	mux.HandleFunc("/repos/{owner}/{repo}/issues", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		if r.Method == http.MethodPost {
			var req struct {
				Title  string   `json:"title"`
				Body   string   `json:"body"`
				Labels []string `json:"labels"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			f.next++
			labels := []map[string]any{}
			for _, l := range req.Labels {
				labels = append(labels, map[string]any{"name": l})
			}
			issue := map[string]any{"number": f.next, "title": req.Title, "state": "open", "body": req.Body, "labels": labels}
			f.issues[full] = append(f.issues[full], issue)
			f.creates[full]++
			f.lastCreated = issue
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(issue)
			return
		}
		state := r.URL.Query().Get("state")
		if state == "" {
			state = "open"
		}
		label := r.URL.Query().Get("labels")
		items := []map[string]any{}
		for _, issue := range f.issues[full] {
			if state != "all" && issue["state"] != state {
				continue
			}
			if label != "" && !secondaryForgeIssueHasLabel(issue, label) {
				continue
			}
			items = append(items, issue)
		}
		_ = json.NewEncoder(w).Encode(items)
	})
	mux.HandleFunc("/repos/{owner}/{repo}/issues/{number}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		full := r.PathValue("owner") + "/" + r.PathValue("repo")
		if r.Method == http.MethodPatch {
			f.edits[full]++
		}
		for _, issue := range f.issues[full] {
			if jsonNumberString(issue["number"]) == r.PathValue("number") {
				_ = json.NewEncoder(w).Encode(issue)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "Not Found"})
	})
	mux.HandleFunc("/repos/{owner}/{repo}/labels", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
	return mux
}

const secondaryTestOrg = "hivecommons"

// TestEnsureSecondaryAdvisoryIssue_CreatesOnceWithMarkerThenReuses: the first
// ensure files one report carrying the hive marker; restarted clients find it.
func TestEnsureSecondaryAdvisoryIssue_CreatesOnceWithMarkerThenReuses(t *testing.T) {
	forge := newSecondaryReportForge()
	server := httptest.NewServer(forge.handler())
	defer server.Close()
	full := secondaryTestOrg + "/crux"

	first, err := newTestClient(t, server, secondaryTestOrg, []string{"hive", "crux"}).EnsureSecondaryAdvisoryIssue(context.Background(), "crux")
	if err != nil || first <= 0 {
		t.Fatalf("first ensure = (%d, %v), want a created report", first, err)
	}
	if body, _ := forge.lastCreated["body"].(string); !strings.Contains(body, AdvisoryReportMarker) {
		t.Fatalf("created report body lacks the hive marker: %q", body)
	}
	const restarts = 3
	for i := 0; i < restarts; i++ {
		got, err := newTestClient(t, server, secondaryTestOrg, []string{"hive", "crux"}).EnsureSecondaryAdvisoryIssue(context.Background(), "crux")
		if err != nil || got != first {
			t.Fatalf("restart %d: ensure = (%d, %v), want #%d", i, got, err, first)
		}
	}
	if forge.creates[full] != 1 {
		t.Fatalf("%d reports created, want exactly 1", forge.creates[full])
	}
}

// TestEnsureSecondaryAdvisoryIssue_HumanClosedIsNotReopened: a closed report
// returns AdvisoryReportClosedError; nothing is reopened (no PATCH) and
// nothing is created, on every restart.
func TestEnsureSecondaryAdvisoryIssue_HumanClosedIsNotReopened(t *testing.T) {
	for name, labelled := range map[string]bool{"labelled": true, "marker only": false} {
		t.Run(name, func(t *testing.T) {
			forge := newSecondaryReportForge()
			full := secondaryTestOrg + "/crux"
			forge.seed(full, 2400, "closed", "old report\n"+AdvisoryReportMarker, labelled)
			server := httptest.NewServer(forge.handler())
			defer server.Close()
			for i := 0; i < 2; i++ {
				n, err := newTestClient(t, server, secondaryTestOrg, []string{"crux"}).EnsureSecondaryAdvisoryIssue(context.Background(), "crux")
				var closed *AdvisoryReportClosedError
				if !errors.As(err, &closed) || closed.Number != 2400 || n != 0 {
					t.Fatalf("restart %d: ensure = (%d, %v), want AdvisoryReportClosedError for #2400", i, n, err)
				}
			}
			if forge.creates[full] != 0 || forge.edits[full] != 0 {
				t.Fatalf("closed report: %d creates, %d edits; want none", forge.creates[full], forge.edits[full])
			}
		})
	}
}

// TestEnsureSecondaryAdvisoryIssue_OpenReportWinsOverClosed: with an open and
// a closed report, the open one is reused.
func TestEnsureSecondaryAdvisoryIssue_OpenReportWinsOverClosed(t *testing.T) {
	forge := newSecondaryReportForge()
	full := secondaryTestOrg + "/crux"
	forge.seed(full, 10, "closed", "", true)
	forge.seed(full, 20, "open", "", true)
	server := httptest.NewServer(forge.handler())
	defer server.Close()
	n, err := newTestClient(t, server, secondaryTestOrg, []string{"crux"}).EnsureSecondaryAdvisoryIssue(context.Background(), "crux")
	if err != nil || n != 20 {
		t.Fatalf("ensure = (%d, %v), want open #20", n, err)
	}
	if forge.creates[full] != 0 || forge.edits[full] != 0 {
		t.Fatal("reusing an open report must not create or edit")
	}
}

// TestEnsureSecondaryAdvisoryIssue_IssuesDisabledSkipped: a repo with Issues
// disabled yields IssuesDisabledError and no create attempt.
func TestEnsureSecondaryAdvisoryIssue_IssuesDisabledSkipped(t *testing.T) {
	forge := newSecondaryReportForge()
	full := secondaryTestOrg + "/fork"
	forge.noIssues[full] = true
	server := httptest.NewServer(forge.handler())
	defer server.Close()
	_, err := newTestClient(t, server, secondaryTestOrg, []string{"fork"}).EnsureSecondaryAdvisoryIssue(context.Background(), "fork")
	var disabled *IssuesDisabledError
	if !errors.As(err, &disabled) {
		t.Fatalf("ensure error = %v, want IssuesDisabledError", err)
	}
	if forge.creates[full] != 0 {
		t.Fatal("created a report in a repo with Issues disabled")
	}
}

// TestAdvisoryIssueOpen_ReportsClosed: the post path's open check sees a
// human close.
func TestAdvisoryIssueOpen_ReportsClosed(t *testing.T) {
	forge := newSecondaryReportForge()
	full := secondaryTestOrg + "/crux"
	forge.seed(full, 1, "open", "", true)
	forge.seed(full, 2, "closed", "", true)
	server := httptest.NewServer(forge.handler())
	defer server.Close()
	c := newTestClient(t, server, secondaryTestOrg, []string{"crux"})
	if open, err := c.AdvisoryIssueOpen(context.Background(), "crux", 1); err != nil || !open {
		t.Fatalf("open report: (%v, %v), want open", open, err)
	}
	if open, err := c.AdvisoryIssueOpen(context.Background(), "crux", 2); err != nil || open {
		t.Fatalf("closed report: (%v, %v), want closed", open, err)
	}
	if open, err := c.AdvisoryIssueOpen(context.Background(), "crux", 99); err != nil || open {
		t.Fatalf("deleted report: (%v, %v), want not open", open, err)
	}
}
