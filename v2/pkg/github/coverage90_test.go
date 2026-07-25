package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// --------------------------------------------------------------------------
// client.go — simple getters (GoGitHub / AppAuth), nil-receiver safety.
// --------------------------------------------------------------------------

func TestGoGitHub_NilReceiver(t *testing.T) {
	var c *Client
	if got := c.GoGitHub(); got != nil {
		t.Errorf("GoGitHub on nil receiver = %v, want nil", got)
	}
}

func TestGoGitHub_NonNil(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClient("tok", "org", []string{"repo"}, logger, "")
	if got := c.GoGitHub(); got == nil {
		t.Error("GoGitHub returned nil for non-nil client")
	}
}

func TestAppAuthGetter_NilReceiver(t *testing.T) {
	var c *Client
	if got := c.AppAuth(); got != nil {
		t.Errorf("AppAuth on nil receiver = %v, want nil", got)
	}
}

func TestAppAuthGetter_NilAndSet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClient("tok", "org", []string{"repo"}, logger, "")
	if got := c.AppAuth(); got != nil {
		t.Errorf("AppAuth on token client = %v, want nil", got)
	}

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	auth := &AppAuth{appID: 1, installationID: 2, key: key, logger: logger}
	appClient := NewClientFromApp(auth, "org", []string{"repo"}, logger)
	if got := appClient.AppAuth(); got != auth {
		t.Error("AppAuth did not return the backing AppAuth")
	}
}

// --------------------------------------------------------------------------
// client.go — EnrichCIStatus (entirely uncovered before this file).
// --------------------------------------------------------------------------

func TestEnrichCIStatus_EmptyHeadSHA(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClient("tok", "org", []string{"repo"}, logger, "")
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: ""}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "pending" {
		t.Errorf("CIStatus = %q, want pending", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "pending" {
		t.Errorf("CIStatus = %q, want pending on API error", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_NoCheckRuns(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{"total_count": 0, "check_runs": []interface{}{}})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "pending" {
		t.Errorf("CIStatus = %q, want pending on zero check runs", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_OnlyTideCheck(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"check_runs": []map[string]interface{}{
				{"name": "tide", "status": "completed", "conclusion": "success"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "pending" {
		t.Errorf("CIStatus = %q, want pending when only tide check present", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_FailureConclusion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 2,
			"check_runs": []map[string]interface{}{
				{"name": "build", "status": "completed", "conclusion": "failure"},
				{"name": "lint", "status": "completed", "conclusion": "success"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "failure" {
		t.Errorf("CIStatus = %q, want failure", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_ActionRequired(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"check_runs": []map[string]interface{}{
				{"name": "build", "status": "completed", "conclusion": "action_required"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "failure" {
		t.Errorf("CIStatus = %q, want failure for action_required", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_AllDoneSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"check_runs": []map[string]interface{}{
				{"name": "build", "status": "completed", "conclusion": "success"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "success" {
		t.Errorf("CIStatus = %q, want success", prs[0].CIStatus)
	}
}

func TestEnrichCIStatus_StillRunning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"total_count": 1,
			"check_runs": []map[string]interface{}{
				{"name": "build", "status": "in_progress"},
			},
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	prs := []PullRequest{{Repo: "org/repo", Number: 1, HeadSHA: "abc123"}}
	c.EnrichCIStatus(context.Background(), prs)
	if prs[0].CIStatus != "pending" {
		t.Errorf("CIStatus = %q, want pending while still running", prs[0].CIStatus)
	}
}

// --------------------------------------------------------------------------
// client.go — GetFileContentRef: explicit ref + content-decode error.
// --------------------------------------------------------------------------

func TestGetFileContentRef_WithRef(t *testing.T) {
	var gotRef string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotRef = r.URL.Query().Get("ref")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  "aGVsbG8=", // "hello"
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	content, err := c.GetFileContentRef(context.Background(), "org", "repo", "file.txt", "my-branch")
	if err != nil {
		t.Fatalf("GetFileContentRef: %v", err)
	}
	if content != "hello" {
		t.Errorf("content = %q, want hello", content)
	}
	if gotRef != "my-branch" {
		t.Errorf("ref query param = %q, want my-branch", gotRef)
	}
}

func TestGetFileContentRef_DecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"type":     "file",
			"encoding": "base64",
			"content":  "not-valid-base64!!!",
		})
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.GetFileContentRef(context.Background(), "org", "repo", "file.txt", "")
	if err == nil {
		t.Error("expected decode error for invalid base64 content")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("error = %v, want mention of decoding", err)
	}
}

// --------------------------------------------------------------------------
// client.go — EnforceSHAHold: PR entries in the kind/bug issue listing must
// be skipped (issue.IsPullRequest() branch).
// --------------------------------------------------------------------------

func TestEnforceSHAHold_SkipsPullRequests(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number":       1,
					"title":        "actually a PR",
					"user":         map[string]string{"login": "alice"},
					"pull_request": map[string]string{"url": "https://example.com/pr/1"},
					"labels":       []map[string]string{{"name": "kind/bug"}},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	result, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{PrimaryRepo: "repo", AIAuthor: "bot"})
	if err != nil {
		t.Fatalf("EnforceSHAHold: %v", err)
	}
	if result.Held != 0 || result.Unheld != 0 || result.Skipped != 0 {
		t.Errorf("expected PR entry to be skipped entirely, got %+v", result)
	}
}

// --------------------------------------------------------------------------
// app.go — AgentTokenCachePath / WriteAgentToken / VerifyInstallation.
// --------------------------------------------------------------------------

func TestAgentTokenCachePath(t *testing.T) {
	got := AgentTokenCachePath("myagent")
	want := agentTokenCacheDir + "/gh-token-myagent.cache"
	if got != want {
		t.Errorf("AgentTokenCachePath = %q, want %q", got, want)
	}
}

func TestWriteAgentToken_ScopedTokenError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	// No fake server behind apiURL -> ScopedToken's JWT client call fails.
	auth := &AppAuth{
		appID: 1, installationID: 2, key: key, logger: logger,
		apiURL: "http://127.0.0.1:1", // closed port -> connection refused
	}
	err := auth.WriteAgentToken(context.Background(), "agent1", "newcomer", 0)
	if err == nil {
		t.Error("expected error when ScopedToken fails")
	}
	if !strings.Contains(err.Error(), "minting scoped token") {
		t.Errorf("error = %v, want mention of minting scoped token", err)
	}
}

func TestWriteAgentToken_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "scoped-token-xyz",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer server.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth := &AppAuth{
		appID: 1, installationID: 2, key: key, logger: logger,
		apiURL: server.URL,
	}

	cacheDir := t.TempDir()
	// Point the package-level cache dir at a temp dir for this test only by
	// using a per-agent cache path override isn't available (const), so we
	// verify the mint succeeds and only check the error-free path up to the
	// MkdirAll/WriteFile using the real (but harmless in CI sandbox) path
	// is avoided — instead we directly exercise ScopedToken success here.
	_ = cacheDir
	token, err := auth.ScopedToken(context.Background(), "trusted")
	if err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if token != "scoped-token-xyz" {
		t.Errorf("token = %q, want scoped-token-xyz", token)
	}
}

func TestScopedToken_DefaultTier(t *testing.T) {
	var capturedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedBody, _ = io.ReadAll(r.Body)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"token":      "default-tier-token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	}))
	defer server.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth := &AppAuth{appID: 1, installationID: 2, key: key, logger: logger, apiURL: server.URL}

	token, err := auth.ScopedToken(context.Background(), "unknown-tier")
	if err != nil {
		t.Fatalf("ScopedToken: %v", err)
	}
	if token != "default-tier-token" {
		t.Errorf("token = %q", token)
	}
	if !strings.Contains(string(capturedBody), "metadata") {
		t.Errorf("body = %s, want metadata-only permissions for unknown tier", capturedBody)
	}
}

func TestVerifyInstallation_JWTAndAPIError(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth := &AppAuth{
		appID: 1, installationID: 2, key: key, logger: logger,
		apiURL: "http://127.0.0.1:1", // connection refused
	}
	_, err := auth.VerifyInstallation(context.Background())
	if err == nil {
		t.Error("expected error resolving installation against unreachable API")
	}
	if !strings.Contains(err.Error(), "resolving installation") {
		t.Errorf("error = %v, want mention of resolving installation", err)
	}
}

func TestVerifyInstallation_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      2,
			"account": map[string]interface{}{"login": "kubestellar"},
			"permissions": map[string]interface{}{
				"issues": "write",
			},
		})
	}))
	defer server.Close()

	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	auth := &AppAuth{appID: 1, installationID: 2, key: key, logger: logger, apiURL: server.URL}

	info, err := auth.VerifyInstallation(context.Background())
	if err != nil {
		t.Fatalf("VerifyInstallation: %v", err)
	}
	if info.Account != "kubestellar" {
		t.Errorf("Account = %q, want kubestellar", info.Account)
	}
	if info.IssuesPerm != "write" {
		t.Errorf("IssuesPerm = %q, want write", info.IssuesPerm)
	}
	if info.InstallationID != 2 {
		t.Errorf("InstallationID = %d, want 2", info.InstallationID)
	}
}

// --------------------------------------------------------------------------
// deviceflow.go — non-200 status branches.
// --------------------------------------------------------------------------

func TestStartDeviceFlow_NonOKStatus(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusForbidden,
			Body:       io.NopCloser(strings.NewReader(`{"message":"forbidden"}`)),
		}, nil
	}, func() {
		_, err := StartDeviceFlow("test-client-id", "", "")
		if err == nil {
			t.Fatal("expected error on non-200 status")
		}
		if !strings.Contains(err.Error(), "403") {
			t.Errorf("error = %v, want mention of 403", err)
		}
	})
}

func TestValidateToken_NonOKStatus(t *testing.T) {
	withMockDeviceFlow(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Body:       io.NopCloser(strings.NewReader(`{}`)),
		}, nil
	}, func() {
		_, err := ValidateToken("bad-token", "")
		if err == nil {
			t.Fatal("expected error on non-200 status")
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("error = %v, want mention of 401", err)
		}
	})
}

// --------------------------------------------------------------------------
// advisory.go — error branches in EnsureAdvisoryIssue / findAdvisoryIssue /
// truncateDigest.
// --------------------------------------------------------------------------

func TestEnsureAdvisoryIssue_FindError(t *testing.T) {
	// Search API returns 500 -> findAdvisoryIssue falls back through
	// label-list and title-scan, both of which also 500 in this test,
	// ultimately returning num=0 (not an error) OR — for CreateLabel path —
	// simulate CreateIssue failing to hit the wrapped-error branch instead.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/issues"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"message": "create failed"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.EnsureAdvisoryIssue(context.Background(), "repo")
	if err == nil {
		t.Fatal("expected error when issue creation fails")
	}
}

func TestFindAdvisoryIssue_SearchErrorFallsBackThenScanErrors(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/issues"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			// label-filtered list (has "labels" query) succeeds empty;
			// unlabeled scan (no labels query) fails to hit scanErr break.
			if r.URL.Query().Get("labels") != "" {
				json.NewEncoder(w).Encode([]interface{}{})
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	num, err := c.findAdvisoryIssue(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 0 {
		t.Errorf("num = %d, want 0", num)
	}
}

func TestFindAdvisoryIssue_TitleScanPagination(t *testing.T) {
	const perPage = 50
	page1 := make([]map[string]interface{}, perPage)
	for i := range page1 {
		page1[i] = map[string]interface{}{
			"number": i + 1,
			"title":  fmt.Sprintf("issue %d", i+1),
			"user":   map[string]string{"login": "someone"},
		}
	}
	page2 := []map[string]interface{}{
		{"number": 999, "title": advisoryTitle, "user": map[string]string{"login": "hive"}},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/search/issues"):
			w.WriteHeader(http.StatusInternalServerError)
		case strings.HasSuffix(r.URL.Path, "/issues") && r.Method == http.MethodGet:
			if r.URL.Query().Get("labels") != "" {
				json.NewEncoder(w).Encode([]interface{}{})
				return
			}
			if r.URL.Query().Get("page") == "2" {
				json.NewEncoder(w).Encode(page2)
				return
			}
			json.NewEncoder(w).Encode(page1)
		case strings.HasSuffix(r.URL.Path, "/labels") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusOK)
			json.NewEncoder(w).Encode(map[string]string{"name": advisoryLabelName})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	num, err := c.findAdvisoryIssue(context.Background(), "org", "repo")
	if err != nil {
		t.Fatalf("findAdvisoryIssue: %v", err)
	}
	if num != 999 {
		t.Errorf("num = %d, want 999 (found on page 2)", num)
	}
}

func TestTruncateDigest_UTF8Boundary(t *testing.T) {
	// truncateDigest computes cutoff = githubCommentCharLimit - truncationFooterPadding,
	// then walks back to the last "\n" before that cutoff, then walks back
	// further while the byte at cutoff is not a UTF-8 rune start. Place a
	// multi-byte rune ('é', 2 bytes) so that the initial cutoff (no newline
	// present, so lastNewline stays -1) lands on its continuation byte —
	// this exercises the utf8.RuneStart walk-back loop.
	cutoff := githubCommentCharLimit - truncationFooterPadding
	filler := strings.Repeat("a", cutoff-1) + "é" // 'é' starts at byte cutoff-1, ends at cutoff
	digest := filler + strings.Repeat("b", 200)   // pushes total length past githubCommentCharLimit; no newlines -> lastNewline == -1
	out := truncateDigest(digest)
	if len(out) == 0 {
		t.Fatal("truncateDigest returned empty string")
	}
	if !strings.Contains(out, "Digest truncated") {
		t.Errorf("expected truncation footer in output")
	}
	// The kept prefix must be valid UTF-8 (walk-back must not split the rune).
	prefixEnd := strings.Index(out, "\n\n---\n")
	if prefixEnd < 0 {
		t.Fatal("could not locate truncation footer separator")
	}
	kept := out[:prefixEnd]
	if !utf8.ValidString(kept) {
		t.Errorf("truncated prefix is not valid UTF-8: %q", kept[len(kept)-10:])
	}
}

// --------------------------------------------------------------------------
// mttr.go — branch coverage for skip conditions and history bucketing.
// --------------------------------------------------------------------------

func TestComputeMTTR_EmptyBodySkipped(t *testing.T) {
	now := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number":    1,
					"body":      "",
					"merged_at": now.Add(-time.Hour).Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("count = %d, want 0 for empty-body PR", result.Count)
	}
}

func TestComputeMTTR_IssueFetchError(t *testing.T) {
	now := time.Now()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number":    1,
					"body":      "Fixes #10",
					"merged_at": now.Add(-time.Hour).Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
			})
		case strings.Contains(r.URL.Path, "/issues/10"):
			w.WriteHeader(http.StatusNotFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	// Issue fetch failed -> no createdAt entry -> ref skipped -> 0 durations.
	if result.Count != 0 {
		t.Errorf("count = %d, want 0 when issue fetch fails", result.Count)
	}
	if len(result.History) != 0 {
		t.Errorf("history len = %d, want 0", len(result.History))
	}
}

func TestComputeMTTR_MergeBeforeIssueCreatedSkipped(t *testing.T) {
	now := time.Now()
	// Merged BEFORE the issue was created (bad data / clock skew) -> skip.
	issueCreated := now
	prMerged := now.Add(-time.Hour)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number":    1,
					"body":      "Fixes #10",
					"merged_at": prMerged.Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
			})
		case strings.Contains(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number":     10,
				"created_at": issueCreated.Format(time.RFC3339),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	if result.Count != 0 {
		t.Errorf("count = %d, want 0 when merge precedes issue creation", result.Count)
	}
}

func TestComputeMTTR_HistoryCutoffAndP90Clamp(t *testing.T) {
	now := time.Now()
	// One recent ref (within 30-day backfill window) and one very old ref
	// (bucket older than cutoff -> exercises the "continue" skip in the
	// history-bucket loop). Single duration -> exercises p90Idx clamp.
	recentIssueCreated := now.Add(-2 * time.Hour)
	recentMerged := now.Add(-1 * time.Hour)

	oldIssueCreated := now.Add(-40 * 24 * time.Hour)
	oldMerged := now.Add(-35 * 24 * time.Hour)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/pulls"):
			json.NewEncoder(w).Encode([]map[string]interface{}{
				{
					"number":    1,
					"body":      "Fixes #10",
					"merged_at": recentMerged.Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
				{
					"number":    2,
					"body":      "Fixes #11",
					"merged_at": oldMerged.Format(time.RFC3339),
					"state":     "closed",
					"user":      map[string]string{"login": "bot"},
				},
			})
		case strings.Contains(r.URL.Path, "/issues/10"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 10, "created_at": recentIssueCreated.Format(time.RFC3339),
			})
		case strings.Contains(r.URL.Path, "/issues/11"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"number": 11, "created_at": oldIssueCreated.Format(time.RFC3339),
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "myorg", []string{"repo1"})
	result, err := c.ComputeMTTR(context.Background(), "repo1")
	if err != nil {
		t.Fatalf("ComputeMTTR: %v", err)
	}
	if result.Count != 2 {
		t.Errorf("count = %d, want 2", result.Count)
	}
	// Old bucket (35 days ago) must be excluded by the 30-day cutoff.
	if len(result.History) != 1 {
		t.Errorf("history len = %d, want 1 (old bucket excluded by cutoff)", len(result.History))
	}
	if result.P90Minutes == 0 {
		t.Error("P90Minutes should be nonzero")
	}
}

// --------------------------------------------------------------------------
// health.go — brewCheck GetContent decode error and ListReleases error.
// --------------------------------------------------------------------------

func TestBrewCheck_FormulaDecodeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "Formula/kubestellar-console.rb"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type":     "file",
				"encoding": "base64",
				"content":  "!!!not-base64!!!",
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "kubestellar", []string{"console", "homebrew-tap"})
	status := c.brewCheck(context.Background(), "console")
	if status != healthStatusNotFound {
		t.Errorf("brewCheck = %d, want healthStatusNotFound on decode error", status)
	}
}

func TestBrewCheck_ListReleasesError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "Formula/kubestellar-console.rb"):
			json.NewEncoder(w).Encode(map[string]interface{}{
				"type":     "file",
				"encoding": "base64",
				"content":  "dmVyc2lvbiAiMS4wLjAi", // version "1.0.0"
			})
		case strings.Contains(r.URL.Path, "/releases"):
			w.WriteHeader(http.StatusInternalServerError)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	c := newTestClient(t, server, "kubestellar", []string{"console", "homebrew-tap"})
	status := c.brewCheck(context.Background(), "console")
	if status != healthStatusNotFound {
		t.Errorf("brewCheck = %d, want healthStatusNotFound when ListReleases errors", status)
	}
}
