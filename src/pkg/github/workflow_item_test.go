package github

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// WorkflowItemSnapshot reads one issue GET; a PR adds one pulls GET and
// the head's checks; closed/merged PRs skip the checks; errors surface.
func TestWorkflowItemSnapshot(t *testing.T) {
	var checkReads int
	mux := http.NewServeMux()
	issue := func(number int, pr bool, state string, labels ...string) map[string]any {
		out := map[string]any{"number": number, "state": state, "user": map[string]any{"login": "nate"}}
		ls := []map[string]any{}
		for _, l := range labels {
			ls = append(ls, map[string]any{"name": l})
		}
		out["labels"] = ls
		if pr {
			out["pull_request"] = map[string]any{"url": "x"}
		}
		return out
	}
	mux.HandleFunc("/repos/acme/widget/issues/1", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(issue(1, false, "open", "bug", "", "hold"))
	})
	mux.HandleFunc("/repos/acme/widget/issues/2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(issue(2, true, "open", "role/untrusted"))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/2", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 2, "state": "open", "draft": true, "mergeable_state": "dirty",
			"head": map[string]any{"sha": "abc"}, "base": map[string]any{"ref": "v5"}, "user": map[string]any{"login": "nate"}})
	})
	mux.HandleFunc("/repos/acme/widget/commits/abc/status", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"state": "success", "statuses": []map[string]any{{"context": "DCO", "state": "success"}}})
	})
	mux.HandleFunc("/repos/acme/widget/commits/abc/check-runs", func(w http.ResponseWriter, r *http.Request) {
		checkReads++
		_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 2, "check_runs": []map[string]any{
			{"id": 1, "name": "unit", "status": "completed", "conclusion": "success"},
			{"id": 2, "name": "lint", "status": "completed", "conclusion": "failure"},
		}})
	})
	mux.HandleFunc("/repos/acme/widget/issues/3", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(issue(3, true, "closed"))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/3", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 3, "state": "closed", "merged_at": "2026-10-20T20:00:00Z", "mergeable_state": "unknown",
			"head": map[string]any{"sha": "def"}, "user": map[string]any{"login": "nate"}})
	})
	mux.HandleFunc("/repos/acme/widget/issues/4", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(issue(4, true, "open"))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/4", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"gone"}`, http.StatusNotFound)
	})
	mux.HandleFunc("/repos/acme/widget/issues/5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(issue(5, true, "open"))
	})
	mux.HandleFunc("/repos/acme/widget/pulls/5", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"number": 5, "state": "open", "mergeable_state": "clean",
			"head": map[string]any{"sha": "nochecks"}, "base": map[string]any{"ref": "main"}})
	})
	mux.HandleFunc("/repos/acme/widget/commits/nochecks/status", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"boom"}`, http.StatusBadGateway)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := newTestClient(t, srv, "acme", []string{"widget"})
	ctx := context.Background()

	snap, err := c.WorkflowItemSnapshot(ctx, "acme/widget", 1)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != "issue" || !snap.Open || snap.Author != "nate" || strings.Join(snap.Labels, ",") != "bug,hold" || snap.HeadSHA != "" || snap.ChecksKnown || snap.ConflictKnown {
		t.Fatalf("issue: %+v", snap)
	}

	// Bare repo name resolves against the client's org.
	snap, err = c.WorkflowItemSnapshot(ctx, "widget", 2)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != "pr" || !snap.Open || !snap.Draft || snap.Merged || snap.HeadSHA != "abc" || snap.BaseRef != "v5" || !snap.ConflictKnown || !snap.Conflicting {
		t.Fatalf("pr: %+v", snap)
	}
	if !snap.ChecksKnown || snap.Checks.Total != 2 || snap.Checks.Green != 1 || len(snap.Checks.Red) != 1 || snap.Checks.Red[0] != "lint" || snap.Checks.DCO != 1 || checkReads != 1 {
		t.Fatalf("checks: %+v reads=%d", snap.Checks, checkReads)
	}
	if strings.Join(snap.Labels, ",") != "role/untrusted" {
		t.Fatalf("labels: %v", snap.Labels)
	}

	snap, err = c.WorkflowItemSnapshot(ctx, "acme/widget", 3)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Kind != "pr" || snap.Open || !snap.Merged || snap.ConflictKnown || snap.ChecksKnown || snap.BaseRef != "" || checkReads != 1 {
		t.Fatalf("merged: %+v reads=%d", snap, checkReads)
	}

	snap, err = c.WorkflowItemSnapshot(ctx, "acme/widget", 5)
	if err != nil {
		t.Fatal(err)
	}
	if snap.ChecksKnown || !snap.ConflictKnown || snap.Conflicting || snap.BaseRef != "main" {
		t.Fatalf("unreadable head: %+v", snap)
	}

	if _, err := c.WorkflowItemSnapshot(ctx, "acme/widget", 4); err == nil || !strings.Contains(err.Error(), "reading PR acme/widget#4") {
		t.Fatalf("pr error: %v", err)
	}
	if _, err := c.WorkflowItemSnapshot(ctx, "acme/widget", 404); err == nil || !strings.Contains(err.Error(), "reading acme/widget#404") {
		t.Fatalf("issue error: %v", err)
	}

	var nilClient *Client
	if _, err := nilClient.WorkflowItemSnapshot(ctx, "acme/widget", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("nil client: %v", err)
	}
	if _, err := (&Client{}).WorkflowItemSnapshot(ctx, "acme/widget", 1); !errors.Is(err, ErrNoGitHubClient) {
		t.Fatalf("empty client: %v", err)
	}
}
