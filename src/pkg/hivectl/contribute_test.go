package hivectl

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// withoutGitHubToken guards the `gh` subprocess from inheriting the hive's own
// GitHub credentials: gh must authenticate as the OPERATOR (their gh login),
// never as the hive App token that happens to be in the environment. A
// regression here silently acts with the wrong identity — enrolling a spoke, or
// registering a contributor, as somebody else — so the scrub is pinned by
// tests.

func TestWithoutGitHubTokenStripsTokenVars(t *testing.T) {
	env := []string{
		"HOME=/home/op",
		"GITHUB_TOKEN=ghp_secret",
		"PATH=/usr/bin",
		"GH_TOKEN=gho_secret",
		"HIVE_REPO=org/repo",
	}
	got := withoutGitHubToken(env)
	want := []string{"HOME=/home/op", "PATH=/usr/bin", "HIVE_REPO=org/repo"}
	if len(got) != len(want) {
		t.Fatalf("got %d vars %v, want %d %v", len(got), got, len(want), want)
	}
	for i, kv := range want {
		if got[i] != kv {
			t.Fatalf("got[%d] = %q, want %q (order must be preserved)", i, got[i], kv)
		}
	}
	for _, kv := range got {
		if strings.Contains(kv, "secret") {
			t.Fatalf("token leaked through scrub: %q", kv)
		}
	}
}

func TestWithoutGitHubTokenKeepsNonTokenLookalikes(t *testing.T) {
	// Only the exact GITHUB_TOKEN= / GH_TOKEN= keys are stripped; other vars
	// that merely mention tokens (e.g. GH_TOKEN_FILE) must survive.
	env := []string{
		"GH_TOKEN_FILE=/tmp/tok",
		"MY_GITHUB_TOKEN=keep",
		"GITHUB_TOKEN_BACKUP=keep",
	}
	got := withoutGitHubToken(env)
	if len(got) != 3 {
		t.Fatalf("lookalike vars dropped: got %v", got)
	}
}

func TestWithoutGitHubTokenEmptyEnv(t *testing.T) {
	if got := withoutGitHubToken(nil); len(got) != 0 {
		t.Fatalf("nil env: got %v, want empty", got)
	}
	if got := withoutGitHubToken([]string{"GITHUB_TOKEN=x", "GH_TOKEN=y"}); len(got) != 0 {
		t.Fatalf("all-token env: got %v, want empty", got)
	}
}

func TestGitHubLogin(t *testing.T) {
	old := RunGH
	t.Cleanup(func() { RunGH = old })

	RunGH = func(_ context.Context, args ...string) (string, error) {
		if strings.Join(args, " ") != "api user --jq .login" {
			t.Errorf("unexpected gh invocation: %v", args)
		}
		return "octocat\n", nil
	}
	if user, err := GitHubLogin(context.Background()); err != nil || user != "octocat" {
		t.Errorf("GitHubLogin = %q, %v; want octocat", user, err)
	}

	RunGH = func(context.Context, ...string) (string, error) { return "  \n", nil }
	if _, err := GitHubLogin(context.Background()); err == nil || !strings.Contains(err.Error(), "--github-user") {
		t.Errorf("empty login should name the --github-user fallback, got %v", err)
	}

	RunGH = func(context.Context, ...string) (string, error) { return "", errors.New("not logged in") }
	if _, err := GitHubLogin(context.Background()); err == nil || !strings.Contains(err.Error(), "gh auth login") {
		t.Errorf("a gh failure should point at gh auth login, got %v", err)
	}
}

// TestRegisterSendsNoBearerCredential is the #4408 H7/CWE-522 invariant, not a
// smoke test: the hub URL can come from a registry entry, so a token forwarded
// here would be harvestable by a poisoned registry. It fails if any
// Authorization header is ever added.
func TestRegisterSendsNoBearerCredential(t *testing.T) {
	var sawAuth, sawBody string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		body := make([]byte, 256)
		n, _ := r.Body.Read(body)
		sawBody = string(body[:n])
		_ = json.NewEncoder(w).Encode(Registration{RegistrationToken: "tok_1", ContributorID: "contrib_1"})
	}))
	defer server.Close()

	t.Setenv("GITHUB_TOKEN", "ghp_should_never_be_sent")
	reg, err := Register(context.Background(), server.URL, "octocat", 5*time.Second)
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if sawAuth != "" {
		t.Errorf("Authorization header sent to the hub: %q", sawAuth)
	}
	if !strings.Contains(sawBody, `"github_username":"octocat"`) {
		t.Errorf("request body = %q, want the github_username only", sawBody)
	}
	if reg.RegistrationToken != "tok_1" || reg.ContributorID != "contrib_1" {
		t.Errorf("Register = %+v, want the hub's token and id", reg)
	}
}

func TestRegisterSurfacesNonJSONAndHTTPErrors(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusTeapot)
	}))
	defer bad.Close()
	if _, err := Register(context.Background(), bad.URL, "octocat", time.Second); err == nil ||
		!strings.Contains(err.Error(), "418") {
		t.Errorf("a non-2xx should name its status, got %v", err)
	}

	garbage := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html>not json</html>"))
	}))
	defer garbage.Close()
	if _, err := Register(context.Background(), garbage.URL, "octocat", time.Second); err == nil ||
		!strings.Contains(err.Error(), "non-JSON") {
		t.Errorf("a non-JSON body should say so, got %v", err)
	}
}

func TestProbeHubReportsReachability(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/contribute/status" {
			t.Errorf("probed %q, want /api/contribute/status", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer up.Close()

	if !ProbeHub(context.Background(), up.URL+"/contribute", 2*time.Second) {
		t.Error("a hub that answers 200 should probe reachable")
	}

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "down", http.StatusBadGateway)
	}))
	defer down.Close()
	if ProbeHub(context.Background(), down.URL+"/contribute", 2*time.Second) {
		t.Error("a 502 is not reachable")
	}

	// An unparseable hub never reaches the network, and a dead port fails to
	// dial. Both are the same answer, which is the point of the boolean.
	if ProbeHub(context.Background(), "not a url", time.Second) {
		t.Error("an invalid hub URL is not reachable")
	}
	if ProbeHub(context.Background(), "wss://127.0.0.1:1/contribute", time.Second) {
		t.Error("an undialable hub is not reachable")
	}
}

func TestTruncateForMessage(t *testing.T) {
	long := strings.Repeat("x", 250)
	if got := truncateForMessage("  " + long + "  "); len(got) != 200+len("…") || !strings.HasSuffix(got, "…") {
		t.Errorf("long payload not truncated to 200 runes plus ellipsis: %d", len(got))
	}
	if got := truncateForMessage("  short  "); got != "short" {
		t.Errorf("short payload should only be trimmed, got %q", got)
	}
}
