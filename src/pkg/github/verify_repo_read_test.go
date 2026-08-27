package github

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestVerifyRepoRead exercises the #2575 repo-access verification gate: a
// verified advisor-scoped Contents read returns nil, an unreadable repo
// returns the error that keeps its finding open, and the minted token must be
// advisor-shaped (contents:read, no write, restricted to the single repo).
func TestVerifyRepoRead(t *testing.T) {
	var mintedPerms map[string]any
	var mintedRepos []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/access_tokens"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if p, ok := body["permissions"].(map[string]any); ok {
				mintedPerms = p
			}
			if rs, ok := body["repositories"].([]any); ok {
				mintedRepos = rs
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"scoped-tok","expires_at":"2099-01-01T00:00:00Z"}`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repos/acme/widgets/contents"):
			_, _ = w.Write([]byte(`[{"name":"README.md","type":"file"}]`))
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/repos/acme/locked/contents"):
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"Resource not accessible by integration"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	auth, err := NewAppAuthFromPEM(1234, 5678, []byte(testAppPrivateKeyPEM(t)), slog.Default(), srv.URL)
	if err != nil {
		t.Fatalf("NewAppAuthFromPEM: %v", err)
	}

	if err := auth.VerifyRepoRead(context.Background(), "acme", "widgets"); err != nil {
		t.Fatalf("VerifyRepoRead(readable repo) = %v, want nil", err)
	}
	// The credential probed with must be the ADVISOR shape — contents:read
	// only, single-repo — or the probe would prove more access than the
	// agents actually receive.
	if got := mintedPerms["contents"]; got != "read" {
		t.Errorf("minted contents permission = %v, want read", got)
	}
	if _, hasIssues := mintedPerms["issues"]; hasIssues {
		t.Errorf("advisor probe token requested issues permission: %v", mintedPerms)
	}
	if len(mintedRepos) != 1 || mintedRepos[0] != "widgets" {
		t.Errorf("minted token repositories = %v, want [widgets]", mintedRepos)
	}

	if err := auth.VerifyRepoRead(context.Background(), "acme", "locked"); err == nil {
		t.Fatal("VerifyRepoRead(unreadable repo) = nil, want error — a real access problem must keep its finding open")
	}

	if err := auth.VerifyRepoRead(context.Background(), "", "widgets"); err == nil {
		t.Fatal("VerifyRepoRead with empty owner = nil, want error")
	}
	if err := auth.VerifyRepoRead(context.Background(), "acme", ""); err == nil {
		t.Fatal("VerifyRepoRead with empty repo = nil, want error")
	}
}
