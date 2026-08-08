package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// --------------------------------------------------------------------------
// CommitMessage
// --------------------------------------------------------------------------

func TestCommitMessage_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/commits/abc123", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sha": "abc123",
			"commit": map[string]any{
				"message": "fix: resolve crash on startup\n\nThis is the body of the commit.",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	msg, err := c.CommitMessage(context.Background(), "org", "repo", "abc123")
	if err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	if msg != "fix: resolve crash on startup" {
		t.Errorf("msg = %q, want first line only", msg)
	}
}

func TestCommitMessage_SingleLine(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/commits/def456", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"sha": "def456",
			"commit": map[string]any{
				"message": "chore: update deps",
			},
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	msg, err := c.CommitMessage(context.Background(), "org", "repo", "def456")
	if err != nil {
		t.Fatalf("CommitMessage: %v", err)
	}
	if msg != "chore: update deps" {
		t.Errorf("msg = %q", msg)
	}
}

func TestCommitMessage_Error(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/org/repo/commits/bad", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, "org", []string{"repo"})
	_, err := c.CommitMessage(context.Background(), "org", "repo", "bad")
	if err == nil {
		t.Error("expected error for missing commit")
	}
}

// --------------------------------------------------------------------------
// setBaseURL
// --------------------------------------------------------------------------

func TestSetBaseURL_Empty(t *testing.T) {
	client := gh.NewClient(nil)
	original := client.BaseURL.String()
	setBaseURL(client, "")
	if client.BaseURL.String() != original {
		t.Error("empty URL should not change BaseURL")
	}
}

func TestSetBaseURL_Default(t *testing.T) {
	client := gh.NewClient(nil)
	original := client.BaseURL.String()
	setBaseURL(client, "https://api.github.com")
	if client.BaseURL.String() != original {
		t.Error("default URL should not change BaseURL")
	}
}

func TestSetBaseURL_Custom(t *testing.T) {
	client := gh.NewClient(nil)
	setBaseURL(client, "https://ghe.example.com/api/v3")
	if client.BaseURL.String() != "https://ghe.example.com/api/v3/" {
		t.Errorf("BaseURL = %q", client.BaseURL.String())
	}
	if client.UploadURL.String() != "https://ghe.example.com/api/v3/" {
		t.Errorf("UploadURL = %q", client.UploadURL.String())
	}
}

func TestSetBaseURL_CustomWithTrailingSlash(t *testing.T) {
	client := gh.NewClient(nil)
	setBaseURL(client, "https://ghe.example.com/api/v3/")
	if client.BaseURL.String() != "https://ghe.example.com/api/v3/" {
		t.Errorf("BaseURL = %q", client.BaseURL.String())
	}
}

func TestSetBaseURL_InvalidURL(t *testing.T) {
	client := gh.NewClient(nil)
	original := client.BaseURL.String()
	// url.Parse rarely fails, but ctl chars can trigger it
	setBaseURL(client, "://bad\x00url")
	// Should silently return without changing BaseURL
	if client.BaseURL.String() != original {
		t.Error("invalid URL should not change BaseURL")
	}
}

// --------------------------------------------------------------------------
// hold / unhold — error paths
// --------------------------------------------------------------------------

func TestHold_AddLabelError(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"id": 1})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	// Should not panic — logs warning
	c.hold(context.Background(), org, repo, 1)
}

func TestHold_CommentError(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"name": "hold"}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	// Should not panic — logs warning
	c.hold(context.Background(), org, repo, 1)
}

func TestUnhold_RemoveLabelError(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/labels/hold", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	// Should not panic — logs warning
	c.unhold(context.Background(), org, repo, 1)
}

// --------------------------------------------------------------------------
// deviceFlowURLs — GHE paths
// --------------------------------------------------------------------------

func TestDeviceFlowURLs_Defaults(t *testing.T) {
	codeURL, tokURL, uURL := deviceFlowURLs("", "")
	if codeURL != defaultDeviceCodeURL {
		t.Errorf("codeURL = %q", codeURL)
	}
	if tokURL != defaultTokenURL {
		t.Errorf("tokURL = %q", tokURL)
	}
	if uURL != defaultUserURL {
		t.Errorf("uURL = %q", uURL)
	}
}

func TestDeviceFlowURLs_DefaultGitHubCom(t *testing.T) {
	codeURL, tokURL, uURL := deviceFlowURLs("https://github.com", "https://api.github.com")
	if codeURL != defaultDeviceCodeURL {
		t.Errorf("codeURL = %q", codeURL)
	}
	if tokURL != defaultTokenURL {
		t.Errorf("tokURL = %q", tokURL)
	}
	if uURL != defaultUserURL {
		t.Errorf("uURL = %q", uURL)
	}
}

func TestDeviceFlowURLs_CustomBase(t *testing.T) {
	codeURL, tokURL, uURL := deviceFlowURLs("https://ghe.example.com", "")
	if codeURL != "https://ghe.example.com/login/device/code" {
		t.Errorf("codeURL = %q", codeURL)
	}
	if tokURL != "https://ghe.example.com/login/oauth/access_token" {
		t.Errorf("tokURL = %q", tokURL)
	}
	if uURL != defaultUserURL {
		t.Errorf("uURL = %q (should remain default when apiURL is empty)", uURL)
	}
}

func TestDeviceFlowURLs_CustomAPI(t *testing.T) {
	codeURL, tokURL, uURL := deviceFlowURLs("", "https://ghe.example.com/api/v3")
	if codeURL != defaultDeviceCodeURL {
		t.Errorf("codeURL = %q (should remain default when baseURL is empty)", codeURL)
	}
	if tokURL != defaultTokenURL {
		t.Errorf("tokURL = %q", tokURL)
	}
	if uURL != "https://ghe.example.com/api/v3/user" {
		t.Errorf("uURL = %q", uURL)
	}
}

func TestDeviceFlowURLs_BothCustom(t *testing.T) {
	codeURL, tokURL, uURL := deviceFlowURLs("https://ghe.corp.com", "https://ghe.corp.com/api/v3")
	if codeURL != "https://ghe.corp.com/login/device/code" {
		t.Errorf("codeURL = %q", codeURL)
	}
	if tokURL != "https://ghe.corp.com/login/oauth/access_token" {
		t.Errorf("tokURL = %q", tokURL)
	}
	if uURL != "https://ghe.corp.com/api/v3/user" {
		t.Errorf("uURL = %q", uURL)
	}
}

// --------------------------------------------------------------------------
// ScopedToken — all tiers
// --------------------------------------------------------------------------

func generateTestKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generating key: %v", err)
	}
	keyBytes := x509.MarshalPKCS1PrivateKey(key)
	pemBlock := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: keyBytes}
	tmpFile, err := os.CreateTemp(t.TempDir(), "test-key-*.pem")
	if err != nil {
		t.Fatalf("creating temp file: %v", err)
	}
	if err := pem.Encode(tmpFile, pemBlock); err != nil {
		t.Fatalf("writing PEM: %v", err)
	}
	tmpFile.Close()
	return key, tmpFile.Name()
}

func TestScopedToken_AllTiers(t *testing.T) {
	tiers := []string{"newcomer", "contributor", "trusted", "merger", "advisor", "unknown-tier", ""}

	for _, tier := range tiers {
		t.Run("tier_"+tier, func(t *testing.T) {
			key, _ := generateTestKey(t)

			// Mock server that returns a fake installation token
			mux := http.NewServeMux()
			mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
				json.NewEncoder(w).Encode(map[string]any{
					"token":      "scoped-token-" + tier,
					"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
				})
			})
			server := httptest.NewServer(mux)
			defer server.Close()

			auth := &AppAuth{
				appID:          1,
				installationID: 2,
				key:            key,
				logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
				apiURL:         server.URL,
			}

			token, err := auth.ScopedToken(context.Background(), tier)
			if err != nil {
				t.Fatalf("ScopedToken(%q): %v", tier, err)
			}
			expected := "scoped-token-" + tier
			if token != expected {
				t.Errorf("token = %q, want %q", token, expected)
			}
		})
	}
}

func TestScopedToken_APIError(t *testing.T) {
	key, _ := generateTestKey(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		apiURL:         server.URL,
	}

	_, err := auth.ScopedToken(context.Background(), "contributor")
	if err == nil {
		t.Error("expected error for API failure")
	}
}

// --------------------------------------------------------------------------
// Token — with httptest mock for the installation token API
// --------------------------------------------------------------------------

func TestToken_RefreshesExpiredCache(t *testing.T) {
	key, _ := generateTestKey(t)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{
			"token":      "refreshed-token",
			"expires_at": time.Now().Add(time.Hour).Format(time.RFC3339),
		})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		cachedToken:    "old-expired-token",
		tokenExpiry:    time.Now().Add(-time.Hour),
	}

	token, err := auth.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "refreshed-token" {
		t.Errorf("token = %q, want refreshed-token", token)
	}
}

func TestToken_DoubleCheckAfterLock(t *testing.T) {
	key, _ := generateTestKey(t)

	// Token that is still valid when double-checked under write lock
	auth := &AppAuth{
		appID:          1,
		installationID: 2,
		key:            key,
		logger:         slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError})),
		cachedToken:    "still-valid",
		tokenExpiry:    time.Now().Add(time.Hour),
	}

	token, err := auth.Token(context.Background())
	if err != nil {
		t.Fatalf("Token: %v", err)
	}
	if token != "still-valid" {
		t.Errorf("token = %q, want still-valid", token)
	}
}

// --------------------------------------------------------------------------
// NewClient with GHE URL
// --------------------------------------------------------------------------

func TestNewClient_WithAPIURL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	c := NewClient("tok", "myorg", []string{"repo"}, logger, "https://ghe.example.com/api/v3")
	if c.client.BaseURL.String() != "https://ghe.example.com/api/v3/" {
		t.Errorf("BaseURL = %q", c.client.BaseURL.String())
	}
}

// --------------------------------------------------------------------------
// EnforceSHAHold — SHA in issue body
// --------------------------------------------------------------------------

func TestEnforceSHAHold_SHAInBody(t *testing.T) {
	org, repo := "testorg", "console"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			// Return issue with SHA in body
			resp := []map[string]any{
				{
					"number":     400,
					"title":      "Bug with SHA in body",
					"body":       "See commit abc1234 for details",
					"user":       map[string]string{"login": "external-user"},
					"labels":     []map[string]string{{"name": "kind/bug"}},
					"created_at": hoursAgo(1),
				},
			}
			json.NewEncoder(w).Encode(resp)
		}
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/400/labels", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{{"name": "hold"}})
	})
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/400/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"id": 1})
			return
		}
		json.NewEncoder(w).Encode([]map[string]any{})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{
		PrimaryRepo: repo,
		AIAuthor:    "hive-bot",
	})
	if err != nil {
		t.Fatalf("EnforceSHAHold: %v", err)
	}
	// Has SHA in body and not held — should be skipped (no action needed)
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (SHA in body, not held)", result.Skipped)
	}
}

func TestEnforceSHAHold_BotAuthor(t *testing.T) {
	org, repo := "testorg", "console"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		resp := []map[string]any{
			{
				"number":     500,
				"title":      "Bug from bot",
				"body":       "no sha here",
				"user":       map[string]string{"login": "github-actions[bot]"},
				"labels":     []map[string]string{{"name": "kind/bug"}},
				"created_at": hoursAgo(1),
			},
		}
		json.NewEncoder(w).Encode(resp)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{
		PrimaryRepo: repo,
		AIAuthor:    "hive-bot",
	})
	if err != nil {
		t.Fatalf("EnforceSHAHold: %v", err)
	}
	if result.Skipped != 1 {
		t.Errorf("Skipped = %d, want 1 (bot author)", result.Skipped)
	}
}

func TestEnforceSHAHold_WithOrgPrefix(t *testing.T) {
	org, repo := "testorg", "console"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode([]map[string]any{})
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	result, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{
		PrimaryRepo: "testorg/console",
		AIAuthor:    "hive-bot",
	})
	if err != nil {
		t.Fatalf("EnforceSHAHold: %v", err)
	}
	if result.Held != 0 && result.Unheld != 0 {
		t.Error("expected no actions on empty issues")
	}
}

// --------------------------------------------------------------------------
// checkCommentsForSHA — error path
// --------------------------------------------------------------------------

func TestCheckCommentsForSHA_Error(t *testing.T) {
	org, repo := "org", "repo"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues/1/comments", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	got := c.checkCommentsForSHA(context.Background(), org, repo, 1, "user")
	if got {
		t.Error("expected false when API errors")
	}
}

// --------------------------------------------------------------------------
// EnforceSHAHold — API error
// --------------------------------------------------------------------------

func TestEnforceSHAHold_APIError(t *testing.T) {
	org, repo := "testorg", "console"
	mux := http.NewServeMux()
	mux.HandleFunc(fmt.Sprintf("/repos/%s/%s/issues", org, repo), func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	})
	server := httptest.NewServer(mux)
	defer server.Close()

	c := newTestClient(t, server, org, []string{repo})
	_, err := c.EnforceSHAHold(context.Background(), SHAHoldConfig{
		PrimaryRepo: repo,
		AIAuthor:    "hive-bot",
	})
	if err == nil {
		t.Error("expected error for API failure")
	}
}

// --------------------------------------------------------------------------
// Suppress unused import
// --------------------------------------------------------------------------

var _ = fmt.Sprintf
