package worksource_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hivecommons/hive/pkg/worksource"
)

// rawGQLServer captures the full GraphQL request bodies so tests can assert
// on the query document itself, not just the typed variables.
type rawGQLServer struct {
	*httptest.Server
	mu       sync.Mutex
	requests []struct {
		Query     string                 `json:"query"`
		Variables map[string]interface{} `json:"variables"`
	}
}

func newRawGQLServer(t *testing.T) *rawGQLServer {
	t.Helper()
	s := &rawGQLServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query     string                 `json:"query"`
			Variables map[string]interface{} `json:"variables"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		s.mu.Lock()
		s.requests = append(s.requests, req)
		s.mu.Unlock()
		json.NewEncoder(w).Encode(gqlResponse(false, "", issueNode("ENG-1", "Mine", 1, "Todo")))
	}))
	return s
}

func (s *rawGQLServer) last(t *testing.T) (string, map[string]interface{}) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		t.Fatal("no GraphQL requests captured")
	}
	r := s.requests[len(s.requests)-1]
	return r.Query, r.Variables
}

// TestLinearAssignedFilter: with ViewerID set the query must narrow to issues
// assigned to OR delegated to the app user (Linear sets `delegate`, not
// `assignee`, when an issue is delegated to an agent).
func TestLinearAssignedFilter(t *testing.T) {
	srv := newRawGQLServer(t)
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:   "key",
		BaseURL:  srv.URL,
		ViewerID: "viewer-123",
		Teams:    []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
	}, nil)

	issues, err := src.ListIssues(context.Background())
	if err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	if len(issues) != 1 || issues[0].ExternalID != "ENG-1" {
		t.Fatalf("issues = %+v", issues)
	}

	query, vars := srv.last(t)
	for _, want := range []string{"$viewerID: ID", "assignee: { id: { eq: $viewerID } }", "delegate: { id: { eq: $viewerID } }", "or:"} {
		if !strings.Contains(query, want) {
			t.Errorf("query missing %q:\n%s", want, query)
		}
	}
	if vars["viewerID"] != "viewer-123" {
		t.Errorf("viewerID var = %v", vars["viewerID"])
	}
}

// TestLinearNoViewerIDUsesOriginalQuery: without ViewerID the original
// unfiltered query is sent and no viewerID variable leaks in.
func TestLinearNoViewerIDUsesOriginalQuery(t *testing.T) {
	srv := newRawGQLServer(t)
	defer srv.Close()

	src := worksource.NewLinearSource(worksource.LinearConfig{
		APIKey:  "key",
		BaseURL: srv.URL,
		Teams:   []worksource.LinearTeamConfig{{Key: "ENG", Repo: "acme/app"}},
	}, nil)

	if _, err := src.ListIssues(context.Background()); err != nil {
		t.Fatalf("ListIssues: %v", err)
	}
	query, vars := srv.last(t)
	if strings.Contains(query, "viewerID") || strings.Contains(query, "delegate") {
		t.Errorf("unfiltered query mentions viewer filter:\n%s", query)
	}
	if _, ok := vars["viewerID"]; ok {
		t.Error("viewerID variable sent without config")
	}
}
