package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gh "github.com/google/go-github/v72/github"
)

// These tests cover four previously-untested credential/gating paths:
//   - AppAuth.MintInstallationToken — the scoped, cache-bypassing mint used by
//     hub-hosted lite scans (0% covered).
//   - Client.PathExistsAtRef — the advisory digest's stale-path check (#3704).
//   - PullRequest.HasFailingRequiredCheck — the shared "PR is red" predicate
//     gating merge-watcher re-engagement and the stuck-PR reaper.
//   - Client.ProbeIssueWrite — the #2353 write-capability probe.

// mintServer fakes POST /app/installations/{id}/access_tokens, capturing the
// request body so tests can prove the scoped options actually hit the wire.
func mintServer(t *testing.T, capture *gh.InstallationTokenOptions) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.Contains(r.URL.Path, "/app/installations/") {
			http.NotFound(w, r)
			return
		}
		if capture != nil {
			if err := json.NewDecoder(r.Body).Decode(capture); err != nil {
				t.Errorf("decoding mint options: %v", err)
			}
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"ghs_scoped","expires_at":"2030-01-02T15:04:05Z"}`))
	}))
}

func TestMintInstallationToken_PassesScopedOptionsAndBypassesCache(t *testing.T) {
	key, _ := generateTestKey(t)
	var got gh.InstallationTokenOptions
	server := mintServer(t, &got)
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 42,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
		// A live cached token must NOT satisfy a scoped mint: the whole point
		// is a narrower credential than the shared full-permission token.
		cachedToken: "ghs_full_permission_cached",
		tokenExpiry: time.Now().Add(time.Hour),
	}

	opts := &gh.InstallationTokenOptions{
		Repositories: []string{"hive"},
		Permissions:  &gh.InstallationPermissions{Issues: gh.Ptr("read")},
	}
	token, expiry, err := auth.MintInstallationToken(context.Background(), opts)
	if err != nil {
		t.Fatalf("MintInstallationToken: %v", err)
	}
	if token != "ghs_scoped" {
		t.Errorf("token = %q, want the freshly minted ghs_scoped (cache must be bypassed)", token)
	}
	if want := time.Date(2030, 1, 2, 15, 4, 5, 0, time.UTC); !expiry.Equal(want) {
		t.Errorf("expiry = %v, want %v", expiry, want)
	}
	if len(got.Repositories) != 1 || got.Repositories[0] != "hive" {
		t.Errorf("wire Repositories = %v, want [hive]", got.Repositories)
	}
	if got.Permissions == nil || got.Permissions.GetIssues() != "read" {
		t.Errorf("wire Permissions.Issues = %v, want read", got.Permissions)
	}
	if auth.cachedToken != "ghs_full_permission_cached" {
		t.Errorf("cachedToken mutated to %q; scoped mint must not touch the shared cache", auth.cachedToken)
	}
}

func TestMintInstallationToken_NilReceiverReturnsErrNoGitHubClient(t *testing.T) {
	var auth *AppAuth
	_, _, err := auth.MintInstallationToken(context.Background(), nil)
	if err != ErrNoGitHubClient {
		t.Fatalf("err = %v, want ErrNoGitHubClient", err)
	}
}

func TestMintInstallationToken_ServerErrorSurfaces(t *testing.T) {
	key, _ := generateTestKey(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Integration not found"}`, http.StatusNotFound)
	}))
	defer server.Close()

	auth := &AppAuth{
		appID:          1,
		installationID: 42,
		key:            key,
		logger:         quietLogger(),
		apiURL:         server.URL,
		cachePath:      t.TempDir() + "/token-cache",
	}
	_, _, err := auth.MintInstallationToken(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "creating installation token") {
		t.Fatalf("err = %v, want wrapped 'creating installation token' error", err)
	}
}

func TestPathExistsAtRef(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/contents/docs/live.md"):
			if got := r.URL.Query().Get("ref"); got != "abc123" {
				t.Errorf("ref query = %q, want abc123", got)
			}
			_, _ = w.Write([]byte(`{"type":"file","name":"live.md","path":"docs/live.md"}`))
		case strings.HasSuffix(r.URL.Path, "/contents/docs"):
			_, _ = w.Write([]byte(`[{"type":"file","name":"live.md","path":"docs/live.md"}]`))
		case strings.HasSuffix(r.URL.Path, "/contents/docs/removed.md"):
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		default:
			http.Error(w, `{"message":"boom"}`, http.StatusInternalServerError)
		}
	}))
	defer server.Close()
	c := newTestClient(t, server, "hivecommons", []string{"hive"})
	ctx := context.Background()

	exists, err := c.PathExistsAtRef(ctx, "hivecommons", "hive", "docs/live.md", "abc123")
	if err != nil || !exists {
		t.Errorf("file at ref: exists=%v err=%v, want true,nil", exists, err)
	}
	exists, err = c.PathExistsAtRef(ctx, "hivecommons", "hive", "docs", "")
	if err != nil || !exists {
		t.Errorf("directory: exists=%v err=%v, want true,nil", exists, err)
	}
	// 404 is a definitive "does not exist", NOT an error: the digest must be
	// able to drop the stale citation rather than treat the check as broken.
	exists, err = c.PathExistsAtRef(ctx, "hivecommons", "hive", "docs/removed.md", "abc123")
	if err != nil || exists {
		t.Errorf("404: exists=%v err=%v, want false,nil", exists, err)
	}
	// Any non-404 failure is inconclusive and must surface as an error.
	_, err = c.PathExistsAtRef(ctx, "hivecommons", "hive", "docs/error.md", "abc123")
	if err == nil {
		t.Error("500: want error for inconclusive check, got nil")
	}

	var nilClient *Client
	if _, err := nilClient.PathExistsAtRef(ctx, "o", "r", "p", ""); err != ErrNoGitHubClient {
		t.Errorf("nil client err = %v, want ErrNoGitHubClient", err)
	}
}

func TestHasFailingRequiredCheck(t *testing.T) {
	cases := []struct {
		name          string
		ciStatus      string
		failingChecks []string
		want          bool
	}{
		{"failure with named checks", "failure", []string{"go-test"}, true},
		{"failure but no named checks", "failure", nil, false},
		{"success with stale check list", "success", []string{"go-test"}, false},
		{"pending", "pending", nil, false},
		{"empty status", "", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := PullRequest{CIStatus: tc.ciStatus, FailingChecks: tc.failingChecks}
			if got := pr.HasFailingRequiredCheck(); got != tc.want {
				t.Errorf("HasFailingRequiredCheck() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestProbeIssueWrite(t *testing.T) {
	const body = "advisory digest body"
	t.Run("no-op edit writes the same body back", func(t *testing.T) {
		var editedBody string
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch r.Method {
			case http.MethodGet:
				_, _ = w.Write([]byte(`{"number":7,"body":"` + body + `"}`))
			case http.MethodPatch:
				var req struct {
					Body *string `json:"body"`
				}
				_ = json.NewDecoder(r.Body).Decode(&req)
				if req.Body != nil {
					editedBody = *req.Body
				}
				_, _ = w.Write([]byte(`{"number":7}`))
			}
		}))
		defer server.Close()
		c := newTestClient(t, server, "hivecommons", []string{"hive"})
		if err := c.ProbeIssueWrite(context.Background(), "hivecommons/hive", 7); err != nil {
			t.Fatalf("ProbeIssueWrite: %v", err)
		}
		if editedBody != body {
			t.Errorf("probe wrote body %q, want the unchanged original %q", editedBody, body)
		}
	})

	t.Run("read failure is reported as a read, not a write, problem", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"Not Found"}`, http.StatusNotFound)
		}))
		defer server.Close()
		c := newTestClient(t, server, "hivecommons", []string{"hive"})
		err := c.ProbeIssueWrite(context.Background(), "hivecommons/hive", 7)
		if err == nil || !strings.Contains(err.Error(), "for write probe") {
			t.Fatalf("err = %v, want wrapped read-phase error", err)
		}
	})

	t.Run("integration 403 on the edit surfaces as a write-probe failure", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodGet {
				_, _ = w.Write([]byte(`{"number":7,"body":"x"}`))
				return
			}
			http.Error(w, `{"message":"Resource not accessible by integration"}`, http.StatusForbidden)
		}))
		defer server.Close()
		c := newTestClient(t, server, "hivecommons", []string{"hive"})
		err := c.ProbeIssueWrite(context.Background(), "hivecommons/hive", 7)
		if err == nil || !strings.Contains(err.Error(), "write probe on advisory issue") {
			t.Fatalf("err = %v, want wrapped write-probe error", err)
		}
		if !strings.Contains(err.Error(), "Resource not accessible by integration") {
			t.Errorf("err = %v, want the underlying 403 preserved for caller classification", err)
		}
	})

	t.Run("nil client", func(t *testing.T) {
		var c *Client
		if err := c.ProbeIssueWrite(context.Background(), "o/r", 1); err != ErrNoGitHubClient {
			t.Errorf("err = %v, want ErrNoGitHubClient", err)
		}
	})
}
