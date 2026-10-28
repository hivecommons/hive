package worksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

func fakeLinearScopeServer(t *testing.T, description string, fullFetches *int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Variables["id"] != "ONB-3872" {
			t.Errorf("id = %v, want ONB-3872", req.Variables["id"])
		}
		if strings.Contains(req.Query, "description") {
			atomic.AddInt32(fullFetches, 1)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": map[string]interface{}{"issue": map[string]interface{}{
			"identifier":  "ONB-3872",
			"title":       "Onboarding checklist",
			"updatedAt":   "2026-10-27T10:00:00Z",
			"description": description,
			"state":       map[string]interface{}{"name": "In Progress"},
			"comments": map[string]interface{}{"nodes": []map[string]interface{}{
				{"body": "second", "createdAt": "2026-10-27T09:00:00Z", "user": map[string]interface{}{"displayName": "Bo"}},
				{"body": "first", "createdAt": "2026-10-26T09:00:00Z", "user": map[string]interface{}{"displayName": "Al"}},
			}},
		}}})
	}))
}

func TestLinearScopeContractIssue(t *testing.T) {
	var fetches int32
	srv := fakeLinearScopeServer(t, "AC1: the checklist renders", &fetches)
	defer srv.Close()
	src := worksource.NewLinearSource(worksource.LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)

	issue, err := src.ScopeContractIssue(context.Background(), "onb-3872")
	if err != nil {
		t.Fatal(err)
	}
	if issue.Identifier != "ONB-3872" || issue.State != "In Progress" {
		t.Fatalf("issue = %+v", issue)
	}
	for _, want := range []string{"ONB-3872: Onboarding checklist", "State: In Progress", "AC1: the checklist renders", "Al (2026-10-26)"} {
		if !strings.Contains(issue.Text, want) {
			t.Fatalf("text missing %q:\n%s", want, issue.Text)
		}
	}
	if strings.Index(issue.Text, "first") > strings.Index(issue.Text, "second") {
		t.Fatalf("comments not oldest first:\n%s", issue.Text)
	}

	if _, err := src.ScopeContractIssue(context.Background(), "ONB-3872"); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt32(&fetches); got != 1 {
		t.Fatalf("full fetches = %d, want 1 (cache hit must not refetch)", got)
	}
}

func TestLinearScopeContractIssueTruncates(t *testing.T) {
	var fetches int32
	srv := fakeLinearScopeServer(t, strings.Repeat("é", worksource.LinearScopeContractMaxBytes), &fetches)
	defer srv.Close()
	src := worksource.NewLinearSource(worksource.LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)

	issue, err := src.ScopeContractIssue(context.Background(), "ONB-3872")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(issue.Text, "[truncated: the issue text exceeds") {
		t.Fatalf("no truncation note:\n%s", issue.Text[len(issue.Text)-200:])
	}
	if len(issue.Text) > worksource.LinearScopeContractMaxBytes+200 {
		t.Fatalf("text is %d bytes, cap %d", len(issue.Text), worksource.LinearScopeContractMaxBytes)
	}
	if !strings.HasPrefix(issue.Text, "ONB-3872: Onboarding checklist") {
		t.Fatalf("truncation dropped the header:\n%s", issue.Text[:100])
	}
}

func TestLinearScopeContractIssueFetchError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	src := worksource.NewLinearSource(worksource.LinearConfig{APIKey: "k", BaseURL: srv.URL}, nil)
	if _, err := src.ScopeContractIssue(context.Background(), "ONB-3872"); err == nil || !strings.Contains(err.Error(), "ONB-3872") {
		t.Fatalf("err = %v, want an error naming ONB-3872", err)
	}
}
