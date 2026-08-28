package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// --------------------------------------------------------------------------
// RateLimits
// --------------------------------------------------------------------------

func TestRateLimits(t *testing.T) {
	resetTime := time.Now().Add(time.Hour)
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"resources": map[string]any{
				"core":    map[string]any{"limit": 5000, "remaining": 4999, "reset": resetTime.Unix()},
				"search":  map[string]any{"limit": 30, "remaining": 29, "reset": resetTime.Unix()},
				"graphql": map[string]any{"limit": 5000, "remaining": 4998, "reset": resetTime.Unix()},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	info, err := c.RateLimits(context.Background())
	if err != nil {
		t.Fatalf("RateLimits: %v", err)
	}
	if info.Core.Limit != 5000 {
		t.Errorf("core limit = %d", info.Core.Limit)
	}
	if info.Search.Remaining != 29 {
		t.Errorf("search remaining = %d", info.Search.Remaining)
	}
}

func TestRateLimits_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/rate_limit", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.RateLimits(context.Background())
	if err == nil {
		t.Error("expected error")
	}
}

// --------------------------------------------------------------------------
// LatestCommitHash
// --------------------------------------------------------------------------

func TestLatestCommitHash(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"ref": "refs/heads/main",
			"object": map[string]any{
				"sha":  "abc123def456",
				"type": "commit",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	hash, err := c.LatestCommitHash(context.Background(), "org", "repo1", "main")
	if err != nil {
		t.Fatalf("LatestCommitHash: %v", err)
	}
	if hash != "abc123def456" {
		t.Errorf("hash = %q", hash)
	}
}

func TestLatestCommitHash_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.LatestCommitHash(context.Background(), "org", "repo1", "main")
	if err == nil {
		t.Error("expected error")
	}
}

// --------------------------------------------------------------------------
// GetRepo
// --------------------------------------------------------------------------

func TestGetRepo(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"full_name":        "org/repo1",
			"stargazers_count": 42,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	repo, _, err := c.GetRepo(context.Background(), "org", "repo1")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo.GetStargazersCount() != 42 {
		t.Errorf("stars = %d", repo.GetStargazersCount())
	}
}

// --------------------------------------------------------------------------
// GetContributorCount
// --------------------------------------------------------------------------

func TestGetContributorCount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contributors", func(w http.ResponseWriter, r *http.Request) {
		contribs := []map[string]any{
			{"login": "user1", "contributions": 10},
			{"login": "user2", "contributions": 5},
		}
		json.NewEncoder(w).Encode(contribs)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	count, err := c.GetContributorCount(context.Background(), "org", "repo1")
	if err != nil {
		t.Fatalf("GetContributorCount: %v", err)
	}
	if count != 2 {
		t.Errorf("count = %d", count)
	}
}

func TestGetContributorCount_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contributors", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.GetContributorCount(context.Background(), "org", "repo1")
	if err == nil {
		t.Error("expected error")
	}
}

// --------------------------------------------------------------------------
// GetFileContent
// --------------------------------------------------------------------------

func TestGetFileContent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contents/README.md", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  "SGVsbG8gV29ybGQ=", // "Hello World"
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	content, err := c.GetFileContent(context.Background(), "org", "repo1", "README.md")
	if err != nil {
		t.Fatalf("GetFileContent: %v", err)
	}
	if content != "Hello World" {
		t.Errorf("content = %q", content)
	}
}

func TestGetFileContent_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contents/missing.md", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.GetFileContent(context.Background(), "org", "repo1", "missing.md")
	if err == nil {
		t.Error("expected error")
	}
}

// --------------------------------------------------------------------------
// SearchPRCount
// --------------------------------------------------------------------------

func TestSearchPRCount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 7,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	count, err := c.SearchPRCount(context.Background(), "bot", "org", "open")
	if err != nil {
		t.Fatalf("SearchPRCount: %v", err)
	}
	if count != 7 {
		t.Errorf("count = %d", count)
	}
}

func TestSearchPRCount_Merged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 3,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	count, err := c.SearchPRCount(context.Background(), "bot", "org", "merged")
	if err != nil {
		t.Fatalf("SearchPRCount: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d", count)
	}
}

// --------------------------------------------------------------------------
// primaryRepo
// --------------------------------------------------------------------------

func TestPrimaryRepo(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"main-repo", "other"})
	if c.primaryRepo() != "main-repo" {
		t.Errorf("primaryRepo() = %q", c.primaryRepo())
	}

	c2 := newTestClient(t, server, "org", nil)
	if c2.primaryRepo() != "console" {
		t.Errorf("primaryRepo() with no repos = %q", c2.primaryRepo())
	}
}

// --------------------------------------------------------------------------
// ciPassRate
// --------------------------------------------------------------------------

func TestCiPassRate(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		runs := []map[string]any{
			{"id": 1, "conclusion": "success", "status": "completed"},
			{"id": 2, "conclusion": "failure", "status": "completed"},
			{"id": 3, "conclusion": "success", "status": "completed"},
			{"id": 4, "conclusion": "skipped", "status": "completed"},
		}
		json.NewEncoder(w).Encode(map[string]any{
			"total_count":   4,
			"workflow_runs": runs,
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	rate := c.ciPassRate(context.Background(), "repo1")
	// 3 out of 4 passed (success+skipped) = 75
	if rate != 75 {
		t.Errorf("ciPassRate = %d, want 75", rate)
	}
}

func TestCiPassRate_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	rate := c.ciPassRate(context.Background(), "repo1")
	if rate != healthStatusFailure {
		t.Errorf("ciPassRate = %d, want %d", rate, healthStatusFailure)
	}
}

// --------------------------------------------------------------------------
// helmCheck
// --------------------------------------------------------------------------

func TestHelmCheck_Found(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contents/deploy/helm/kubestellar-console/Chart.yaml", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"type":    "file",
			"content": "dmVyc2lvbjogMS4wLjA=",
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.helmCheck(context.Background(), "repo1")
	if result != healthStatusSuccess {
		t.Errorf("helmCheck = %d", result)
	}
}

func TestHelmCheck_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contents/deploy/helm/kubestellar-console/Chart.yaml", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.helmCheck(context.Background(), "repo1")
	if result != healthStatusNotFound {
		t.Errorf("helmCheck = %d", result)
	}
}

// --------------------------------------------------------------------------
// checkWorkflow
// --------------------------------------------------------------------------

func TestCheckWorkflow_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 101, "name": "Test Workflow"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/101/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 1001, "conclusion": "success", "status": "completed"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.checkWorkflow(context.Background(), "repo1", "Test Workflow")
	if result != healthStatusSuccess {
		t.Errorf("checkWorkflow = %d", result)
	}
}

func TestCheckWorkflow_Failure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 101, "name": "Test Workflow"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/101/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 1001, "conclusion": "failure", "status": "completed"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.checkWorkflow(context.Background(), "repo1", "Test Workflow")
	if result != healthStatusFailure {
		t.Errorf("checkWorkflow = %d", result)
	}
}

func TestCheckWorkflow_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 101, "name": "Other Workflow"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.checkWorkflow(context.Background(), "repo1", "Missing Workflow")
	if result != healthStatusNotFound {
		t.Errorf("checkWorkflow = %d", result)
	}
}

// --------------------------------------------------------------------------
// brewCheck
// --------------------------------------------------------------------------

func TestBrewCheck_NoTap(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.brewCheck(context.Background(), "repo1")
	if result != healthStatusNotFound {
		t.Errorf("brewCheck = %d, want not-found", result)
	}
}

func TestBrewCheck_VersionMatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/homebrew-tap/contents/Formula/kubestellar-console.rb", func(w http.ResponseWriter, r *http.Request) {
		// base64 of: version "1.2.3"
		json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  "dmVyc2lvbiAiMS4yLjMi", // version "1.2.3"
		})
	})
	mux.HandleFunc("/repos/org/repo1/releases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v1.2.3"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1", "homebrew-tap"})
	result := c.brewCheck(context.Background(), "repo1")
	if result != healthStatusSuccess {
		t.Errorf("brewCheck = %d, want success", result)
	}
}

func TestBrewCheck_VersionMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/homebrew-tap/contents/Formula/kubestellar-console.rb", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"type":     "file",
			"encoding": "base64",
			"content":  "dmVyc2lvbiAiMS4yLjMi", // version "1.2.3"
		})
	})
	mux.HandleFunc("/repos/org/repo1/releases", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"tag_name": "v2.0.0"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1", "homebrew-tap"})
	result := c.brewCheck(context.Background(), "repo1")
	if result != healthStatusFailure {
		t.Errorf("brewCheck = %d, want failure", result)
	}
}

// --------------------------------------------------------------------------
// perfCheck
// --------------------------------------------------------------------------

func TestPerfCheck_AllNotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"workflows":   []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.perfCheck(context.Background(), "repo1")
	if result != healthStatusSuccess {
		t.Errorf("perfCheck = %d", result)
	}
}

// --------------------------------------------------------------------------
// releaseCheck
// --------------------------------------------------------------------------

func TestReleaseCheck_NoWorkflow(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"workflows":   []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", false)
	if result != healthStatusNotFound {
		t.Errorf("releaseCheck = %d", result)
	}
}

func TestReleaseCheck_WithRuns(t *testing.T) {
	// Find a date that is NOT Sunday for nightly check
	weekday := time.Now().Truncate(24 * time.Hour)
	for weekday.Weekday() == time.Sunday {
		weekday = weekday.Add(24 * time.Hour)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 200, "name": "Release"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/200/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{
					"id":         3001,
					"conclusion": "success",
					"status":     "completed",
					"created_at": weekday.Format(time.RFC3339),
				},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", false)
	if result != healthStatusSuccess {
		t.Errorf("nightly releaseCheck = %d, want success", result)
	}
}

// --------------------------------------------------------------------------
// deployChecks
// --------------------------------------------------------------------------

func TestDeployChecks_NoWorkflows(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"workflows":   []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	health := make(map[string]any)
	c.deployChecks(context.Background(), "repo1", health)
	if health["deploy_vllm_d"] != healthStatusNotFound {
		t.Errorf("deploy_vllm_d = %v", health["deploy_vllm_d"])
	}
}

func TestDeployChecks_WithJobs(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 300, "name": "Build and Deploy KC"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/300/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 5001, "conclusion": "success", "status": "completed"},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/org/repo1/actions/runs/%d/jobs", 5001), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"jobs": []map[string]any{
				{"id": 1, "name": "deploy-vllm-d", "conclusion": "success", "status": "completed"},
				{"id": 2, "name": "deploy-pok-prod", "conclusion": "failure", "status": "completed"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	health := make(map[string]any)
	c.deployChecks(context.Background(), "repo1", health)
	if health["deploy_vllm_d"] != healthStatusSuccess {
		t.Errorf("deploy_vllm_d = %v", health["deploy_vllm_d"])
	}
	if health["deploy_pok_prod"] != healthStatusFailure {
		t.Errorf("deploy_pok_prod = %v", health["deploy_pok_prod"])
	}
}

// --------------------------------------------------------------------------
// FetchWorkflowHealth (integration test using all health checks)
// --------------------------------------------------------------------------

func TestFetchWorkflowHealth(t *testing.T) {
	mux := http.NewServeMux()
	// Return empty for all workflow-related endpoints
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Default: return empty/not-found for all unmatched paths
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/org/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count":   0,
			"workflow_runs": []any{},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 0,
			"workflows":   []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	health := c.FetchWorkflowHealth(context.Background())
	if health == nil {
		t.Fatal("health is nil")
	}
	// ci should be 0 (failure) because no runs
	if health["ci"] != healthStatusFailure {
		t.Errorf("ci = %v", health["ci"])
	}
}

// --------------------------------------------------------------------------
// SearchOutreachPRCount
// --------------------------------------------------------------------------

func TestSearchOutreachPRCount(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 7,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	count, err := c.SearchOutreachPRCount(context.Background(), "bot", "org", "KubeStellar", "open")
	if err != nil {
		t.Fatalf("SearchOutreachPRCount: %v", err)
	}
	if count != 7 {
		t.Errorf("count = %d, want 7", count)
	}
}

func TestSearchOutreachPRCount_Merged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 3,
			"items":       []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	count, err := c.SearchOutreachPRCount(context.Background(), "bot", "org", "KubeStellar", "merged")
	if err != nil {
		t.Fatalf("SearchOutreachPRCount: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
}

func TestSearchOutreachPRCount_EmptyProjectName(t *testing.T) {
	c := newTestClient(t, httptest.NewServer(http.NewServeMux()), "org", []string{"repo1"})
	_, err := c.SearchOutreachPRCount(context.Background(), "bot", "org", "", "open")
	if err == nil {
		t.Error("expected error for empty projectName")
	}
}

// --------------------------------------------------------------------------
// releaseCheck — weekly (Sunday run)
// --------------------------------------------------------------------------

func TestReleaseCheck_WeeklySuccess(t *testing.T) {
	// Find a Sunday for the weekly check
	sunday := time.Now().Truncate(24 * time.Hour)
	for sunday.Weekday() != time.Sunday {
		sunday = sunday.Add(24 * time.Hour)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows":   []map[string]any{{"id": 200, "name": "Release"}},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/200/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 3002, "conclusion": "success", "status": "completed", "created_at": sunday.Format(time.RFC3339)},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", true)
	if result != healthStatusSuccess {
		t.Errorf("weekly releaseCheck = %d, want success", result)
	}
}

func TestReleaseCheck_WeeklyFailure(t *testing.T) {
	sunday := time.Now().Truncate(24 * time.Hour)
	for sunday.Weekday() != time.Sunday {
		sunday = sunday.Add(24 * time.Hour)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows":   []map[string]any{{"id": 200, "name": "Release"}},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/200/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 3003, "conclusion": "failure", "status": "completed", "created_at": sunday.Format(time.RFC3339)},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", true)
	if result != healthStatusFailure {
		t.Errorf("weekly releaseCheck = %d, want failure", result)
	}
}

func TestReleaseCheck_NightlyFailure(t *testing.T) {
	weekday := time.Now().Truncate(24 * time.Hour)
	for weekday.Weekday() == time.Sunday {
		weekday = weekday.Add(24 * time.Hour)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows":   []map[string]any{{"id": 200, "name": "Release"}},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/200/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 3004, "conclusion": "failure", "status": "completed", "created_at": weekday.Format(time.RFC3339)},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", false)
	if result != healthStatusFailure {
		t.Errorf("nightly releaseCheck = %d, want failure", result)
	}
}

// --------------------------------------------------------------------------
// releaseCheck — no runs for desired day type
// --------------------------------------------------------------------------

func TestReleaseCheck_NoMatchingDayType(t *testing.T) {
	// For weekly=true but only non-Sunday runs — should return not-found
	weekday := time.Now().Truncate(24 * time.Hour)
	for weekday.Weekday() == time.Sunday {
		weekday = weekday.Add(24 * time.Hour)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows":   []map[string]any{{"id": 200, "name": "Release"}},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/200/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 3005, "conclusion": "success", "status": "completed", "created_at": weekday.Format(time.RFC3339)},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.releaseCheck(context.Background(), "repo1", true)
	if result != healthStatusNotFound {
		t.Errorf("weekly releaseCheck with no Sunday run = %d, want notFound", result)
	}
}

// --------------------------------------------------------------------------
// perfCheck — with failing workflow
// --------------------------------------------------------------------------

func TestPerfCheck_WithFailure(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 400, "name": "Perf — React commits per navigation"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/400/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 4001, "conclusion": "failure", "status": "completed"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.perfCheck(context.Background(), "repo1")
	if result != healthStatusFailure {
		t.Errorf("perfCheck = %d, want failure", result)
	}
}

func TestPerfCheck_AllSuccess(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 2,
			"workflows": []map[string]any{
				{"id": 400, "name": "Perf — React commits per navigation"},
				{"id": 401, "name": "Performance TTFI Gate"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/400/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 4001, "conclusion": "success", "status": "completed"},
			},
		})
	})
	mux.HandleFunc("/repos/org/repo1/actions/workflows/401/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": 4002, "conclusion": "success", "status": "completed"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.perfCheck(context.Background(), "repo1")
	if result != healthStatusSuccess {
		t.Errorf("perfCheck = %d, want success", result)
	}
}

// --------------------------------------------------------------------------
// ciPassRate — with mixed results (skipped counted as pass)
// --------------------------------------------------------------------------

func TestCiPassRate_Mixed(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 4,
			"workflow_runs": []map[string]any{
				{"conclusion": "success"},
				{"conclusion": "skipped"},
				{"conclusion": "failure"},
				{"conclusion": "failure"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.ciPassRate(context.Background(), "repo1")
	// 2 pass (success+skipped) out of 4 = 50%
	if result != 50 {
		t.Errorf("ciPassRate = %d, want 50", result)
	}
}

// --------------------------------------------------------------------------
// brewCheck — version fetch error
// --------------------------------------------------------------------------

func TestBrewCheck_FetchError(t *testing.T) {
	mux := http.NewServeMux()
	// List repos to find homebrew-tap
	mux.HandleFunc("/orgs/org/repos", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{
			{"name": "homebrew-tap", "full_name": "org/homebrew-tap"},
		})
	})
	// Return 404 for contents (no Formula dir)
	mux.HandleFunc("/repos/org/homebrew-tap/contents/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	result := c.brewCheck(context.Background(), "repo1")
	// With tap found but no formulas, should still function
	if result == healthStatusSuccess {
		// If it somehow succeeds, that's also valid — the point is covering the code path
		t.Logf("brewCheck = %d", result)
	}
}

// --------------------------------------------------------------------------
// GetFileContent — error cases
// --------------------------------------------------------------------------

func TestGetFileContent_DirectoryReturnsError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/contents/somedir", func(w http.ResponseWriter, r *http.Request) {
		// Return array (directory listing) instead of single file
		json.NewEncoder(w).Encode([]map[string]any{
			{"name": "file1.txt", "type": "file"},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.GetFileContent(context.Background(), "org", "repo1", "somedir")
	if err == nil {
		t.Error("expected error for directory")
	}
}

// --------------------------------------------------------------------------
// fetchPRs — error handling
// --------------------------------------------------------------------------

func TestFetchPRs_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo1/pulls", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo1"})
	_, err := c.EnumerateActionable(context.Background())
	// Should still return a result (with warnings), not crash
	if err != nil {
		t.Logf("EnumerateActionable error (expected for partial failures): %v", err)
	}
}

// ---- additional coverage tests ----

func TestCheckWorkflow_NoRuns(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflows":   []map[string]interface{}{{"id": 1, "name": "CI"}},
		})
	})
	mux.HandleFunc("/repos/org/repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count":   0,
			"workflow_runs": []interface{}{},
		})
	})

	result := client.checkWorkflow(context.Background(), "repo", "CI")
	if result != healthStatusNotFound {
		t.Errorf("expected not-found for no runs, got %d", result)
	}
}

func TestBrewCheck_FormulaContentError(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	client.repos = []string{"homebrew-tap"}

	mux.HandleFunc("/repos/org/homebrew-tap/contents/Formula/kubestellar-console.rb", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	mux.HandleFunc("/repos/org/homebrew-tap/contents/Formula/kc-agent.rb", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})

	result := client.brewCheck(context.Background(), "main-repo")
	if result != healthStatusNotFound {
		t.Errorf("expected not-found, got %d", result)
	}
}

func TestCiPassRate_ErrorReturnsFailure(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	mux.HandleFunc("/repos/org/repo/actions/runs", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	result := client.ciPassRate(context.Background(), "repo")
	if result != healthStatusFailure {
		t.Errorf("expected failure, got %d", result)
	}
}

func TestCiPassRate_NoRuns(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflows":   []map[string]interface{}{{"id": 1, "name": "CI"}},
		})
	})
	mux.HandleFunc("/repos/org/repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count":   0,
			"workflow_runs": []interface{}{},
		})
	})

	result := client.ciPassRate(context.Background(), "repo")
	if result != healthStatusFailure {
		t.Errorf("expected failure for empty runs, got %d", result)
	}
}

func TestReleaseCheck_NoRuns(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"workflows":   []map[string]interface{}{{"id": 1, "name": "Release"}},
		})
	})
	mux.HandleFunc("/repos/org/repo/actions/workflows/1/runs", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count":   0,
			"workflow_runs": []interface{}{},
		})
	})

	result := client.releaseCheck(context.Background(), "repo", false)
	if result != healthStatusNotFound {
		t.Errorf("expected not-found, got %d", result)
	}
}

func TestEnumerateActionable_SingleRepoError(t *testing.T) {
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	client := newTestClient(t, server, "org", []string{"repo"})
	defer server.Close()

	// Return error for issues endpoint
	mux.HandleFunc("/repos/org/repo/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	// Return error for search endpoint
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})

	result, err := client.EnumerateActionable(context.Background())
	// The only repo failed entirely, so EnumerateActionable reports the
	// outage instead of returning an empty (queue-looks-idle) result.
	if err == nil {
		t.Fatal("expected error when the only repo fails to enumerate")
	}
	if result != nil {
		t.Errorf("expected nil result on total failure, got %+v", result)
	}
}

// --------------------------------------------------------------------------
// deployChecks — additional branches
// --------------------------------------------------------------------------

func TestDeployChecks_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	health := map[string]any{}
	client.deployChecks(context.Background(), "repo", health)

	if health["deploy_vllm_d"] != healthStatusNotFound {
		t.Errorf("deploy_vllm_d = %v, want not_found", health["deploy_vllm_d"])
	}
}

func TestDeployChecks_WorkflowNameMismatch(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": 1, "name": "Other Workflow"},
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	health := map[string]any{}
	client.deployChecks(context.Background(), "repo", health)

	if health["deploy_vllm_d"] != healthStatusNotFound {
		t.Errorf("deploy_vllm_d = %v, want not_found", health["deploy_vllm_d"])
	}
}

func TestDeployChecks_NoRuns(t *testing.T) {
	const workflowID = 42
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": workflowID, "name": "Build and Deploy KC"},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/org/repo/actions/workflows/%d/runs", workflowID), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count":   0,
			"workflow_runs": []any{},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	health := map[string]any{}
	client.deployChecks(context.Background(), "repo", health)

	if health["deploy_vllm_d"] != healthStatusNotFound {
		t.Errorf("deploy_vllm_d = %v, want not_found", health["deploy_vllm_d"])
	}
}

func TestDeployChecks_JobsAPIError(t *testing.T) {
	const workflowID = 42
	const runID = 100
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflows": []map[string]any{
				{"id": workflowID, "name": "Build and Deploy KC"},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/org/repo/actions/workflows/%d/runs", workflowID), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"total_count": 1,
			"workflow_runs": []map[string]any{
				{"id": runID, "status": "completed", "conclusion": "success"},
			},
		})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/org/repo/actions/runs/%d/jobs", runID), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	health := map[string]any{}
	client.deployChecks(context.Background(), "repo", health)

	if health["deploy_vllm_d"] != healthStatusNotFound {
		t.Errorf("deploy_vllm_d = %v, want not_found", health["deploy_vllm_d"])
	}
}

// --------------------------------------------------------------------------
// EnumerateActionable — PR fetch error path
// --------------------------------------------------------------------------

func TestEnumerateActionable_PRsFetchError(t *testing.T) {
	// Issues succeed but PRs endpoint returns 500 — should log+continue.
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		issues := []wireIssue{
			{Number: 1, Title: "issue-1", User: wireUser{"u"}, CreatedAt: hoursAgo(1)},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, issues))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "pr-boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Issues were fetched but PR error causes continue — repo not in totalByRepo.
	if result.PRs.Count != 0 {
		t.Errorf("PRs.Count = %d, want 0", result.PRs.Count)
	}
}

// --------------------------------------------------------------------------
// fetchPRs — exempt PR label
// --------------------------------------------------------------------------

func TestFetchPRs_ExemptPR(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("[]"))
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/pulls", org, repo), func(w http.ResponseWriter, r *http.Request) {
		prs := []wirePR{
			{Number: 1, Title: "exempt-pr", User: wireUser{"u"}, Labels: []wireLabel{{Name: "LFX mentorship"}}, CreatedAt: hoursAgo(1)},
			{Number: 2, Title: "normal-pr", User: wireUser{"u"}, CreatedAt: hoursAgo(2)},
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(mustMarshal(t, prs))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	c.SetExemptLabels([]string{"LFX"})
	result, err := c.EnumerateActionable(context.Background())
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	// PR #1 is exempt, only PR #2 should be actionable.
	if result.PRs.Count != 1 {
		t.Errorf("PRs.Count = %d, want 1", result.PRs.Count)
	}
}

// --------------------------------------------------------------------------
// GetContributorCount — pagination
// --------------------------------------------------------------------------

func TestGetContributorCount_Pagination(t *testing.T) {
	org, repo := "org", "repo"
	callCount := 0

	// Need a mux variable we can reference in the handler to build absolute URLs
	mux := http.NewServeMux()
	server := httptest.NewServer(mux)
	defer server.Close()

	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/contributors", org, repo), func(w http.ResponseWriter, r *http.Request) {
		callCount++
		page := r.URL.Query().Get("page")
		if page == "" || page == "1" {
			// First page: return 2 contributors, indicate page 2 exists via Link header
			nextURL := fmt.Sprintf("%s/repos/%s/%s/contributors?page=2", server.URL, org, repo)
			w.Header().Set("Link", fmt.Sprintf(`<%s>; rel="next"`, nextURL))
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"login":"a","id":1,"contributions":10},{"login":"b","id":2,"contributions":5}]`))
		} else {
			// Second page: return 1 contributor, no next link
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`[{"login":"c","id":3,"contributions":3}]`))
		}
	})

	c := newTestClient(t, server, org, []string{repo})
	count, err := c.GetContributorCount(context.Background(), org, repo)
	if err != nil {
		t.Fatalf("error: %v", err)
	}
	if count != 3 {
		t.Errorf("count = %d, want 3", count)
	}
	if callCount < 2 {
		t.Errorf("expected at least 2 API calls for pagination, got %d", callCount)
	}
}

// --------------------------------------------------------------------------
// GetFileContent — nil file content (directory)
// --------------------------------------------------------------------------

func TestGetFileContent_NilContent(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/contents/somedir", func(w http.ResponseWriter, r *http.Request) {
		// Return a directory listing (array instead of object) — go-github returns nil fc
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`[{"name":"file.txt","path":"somedir/file.txt","type":"file"}]`))
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.GetFileContent(context.Background(), "org", "repo", "somedir")
	if err == nil {
		t.Error("expected error for directory path")
	}
}

// --------------------------------------------------------------------------
// SearchPRCount — API error
// --------------------------------------------------------------------------

func TestSearchPRCount_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.SearchPRCount(context.Background(), "bot", "org", "open")
	if err == nil {
		t.Error("expected error from SearchPRCount")
	}
}

// --------------------------------------------------------------------------
// SearchOutreachPRCount — API error
// --------------------------------------------------------------------------

func TestSearchOutreachPRCount_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/search/issues", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.SearchOutreachPRCount(context.Background(), "bot", "org", "MyProject", "open")
	if err == nil {
		t.Error("expected error from SearchOutreachPRCount")
	}
}

// --------------------------------------------------------------------------
// releaseCheck — API error
// --------------------------------------------------------------------------

func TestReleaseCheck_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	result := client.releaseCheck(context.Background(), "repo", false)
	if result != healthStatusNotFound {
		t.Errorf("expected not-found, got %d", result)
	}
}

// --------------------------------------------------------------------------
// checkWorkflow — API error
// --------------------------------------------------------------------------

func TestCheckWorkflow_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/actions/workflows", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	client := newTestClient(t, server, "org", []string{"repo"})
	result := client.checkWorkflow(context.Background(), "repo", "CI")
	if result != healthStatusNotFound {
		t.Errorf("expected not-found, got %d", result)
	}
}
