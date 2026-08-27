package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// #4329: a fork with has_issues=false has no Issues tab. The ensure path must
// return a DISTINCT, actionable error naming the repo-settings remedy — not
// fall through to a create whose 410 reads like an App-auth failure.

// muxNoAdvisoryIssue wires the find fallbacks so findAdvisoryIssue resolves
// nothing: search and every list return empty.
func muxNoAdvisoryIssue(mux *http.ServeMux, org, repo string, onCreate func()) {
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"total_count": 0, "items": []any{}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if onCreate != nil {
				onCreate()
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"number": 7, "title": advisoryTitle})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"name": advisoryLabelName})
	})
}

func TestEnsureAdvisoryIssue_IssuesDisabledOnFork(t *testing.T) {
	org, repo := "jeejz", "incubator-kie-drools"
	var createAttempted bool
	mux := http.NewServeMux()
	muxNoAdvisoryIssue(mux, org, repo, func() { createAttempted = true })
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"full_name":  org + "/" + repo,
			"has_issues": false,
			"fork":       true,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	_, err := c.EnsureAdvisoryIssue(context.Background(), org+"/"+repo)
	if err == nil {
		t.Fatal("expected an error for a repo with Issues disabled")
	}
	var disabled *IssuesDisabledError
	if !errors.As(err, &disabled) {
		t.Fatalf("error must be an *IssuesDisabledError, got %T: %v", err, err)
	}
	if disabled.Repo != org+"/"+repo {
		t.Errorf("error repo = %q, want %q", disabled.Repo, org+"/"+repo)
	}
	if !disabled.Fork {
		t.Error("error must record that the repo is a fork")
	}
	for _, want := range []string{"Issues are disabled", org + "/" + repo, "Settings > General > Features", "upstream repo", "fork"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error message must contain %q, got %q", want, err.Error())
		}
	}
	if createAttempted {
		t.Error("no create must be attempted against a repo with Issues disabled")
	}
}

// Happy path: has_issues=true — the probe answers, and the create proceeds
// exactly as before the check existed.
func TestEnsureAdvisoryIssue_HasIssuesTrueStillCreates(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var createAttempted bool
	mux := http.NewServeMux()
	muxNoAdvisoryIssue(mux, org, repo, func() { createAttempted = true })
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"full_name":  org + "/" + repo,
			"has_issues": true,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 7 {
		t.Errorf("issue number = %d, want 7", num)
	}
	if !createAttempted {
		t.Error("the create must proceed when has_issues=true")
	}
}

// The has_issues probe fails OPEN: a metadata fetch error (rate limit, 5xx)
// must not block the create, whose own error is the real, classifiable one.
func TestEnsureAdvisoryIssue_HasIssuesProbeFailureFailsOpen(t *testing.T) {
	org, repo := "testorg", "testrepo"
	var createAttempted bool
	mux := http.NewServeMux()
	muxNoAdvisoryIssue(mux, org, repo, func() { createAttempted = true })
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	num, err := c.EnsureAdvisoryIssue(context.Background(), repo)
	if err != nil {
		t.Fatalf("EnsureAdvisoryIssue: %v", err)
	}
	if num != 7 {
		t.Errorf("issue number = %d, want 7", num)
	}
	if !createAttempted {
		t.Error("a failed has_issues probe must not block the create")
	}
}

func TestIssuesDisabledError_MessageNonFork(t *testing.T) {
	err := &IssuesDisabledError{Repo: "org/repo"}
	msg := err.Error()
	if !strings.Contains(msg, "common on forks") {
		t.Errorf("non-fork message should hedge with %q, got %q", "common on forks", msg)
	}
	if !strings.Contains(msg, "Settings > General > Features") {
		t.Errorf("message must name the settings remedy, got %q", msg)
	}
}
