package github

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// patVerifyServer fakes the three endpoints VerifyPAT calls. goodToken is
// the only token /user accepts; readable lists the "owner/name" repos the
// token may read; scopes is the X-OAuth-Scopes header on /rate_limit.
type patVerifyServer struct {
	goodToken string
	readable  map[string]bool
	scopes    string

	mu   sync.Mutex
	auth []string
}

func (f *patVerifyServer) start(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.mu.Unlock()
		authorized := strings.HasSuffix(r.Header.Get("Authorization"), " "+f.goodToken)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/user":
			if !authorized {
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"message":"Bad credentials"}`))
				return
			}
			_, _ = w.Write([]byte(`{"login":"rotator","id":7}`))
		case r.URL.Path == "/rate_limit":
			w.Header().Set(oauthScopesHeader, f.scopes)
			_, _ = w.Write([]byte(`{"resources":{}}`))
		case strings.HasPrefix(r.URL.Path, "/repos/"):
			slug := strings.TrimPrefix(r.URL.Path, "/repos/")
			if !authorized || !f.readable[slug] {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"message":"Not Found"}`))
				return
			}
			_, _ = w.Write([]byte(`{"full_name":"` + slug + `"}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func patVerifyLogger() *slog.Logger { return slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)) }

func TestVerifyPAT_AcceptsAWorkingToken(t *testing.T) {
	f := &patVerifyServer{goodToken: "ghp_new", readable: map[string]bool{"acme/widgets": true, "other/tools": true}, scopes: "repo, read:org, workflow"}
	api := f.start(t)

	got, err := VerifyPAT(context.Background(), " ghp_new\n", api, "acme", []string{"widgets", "other/tools"}, 6, patVerifyLogger())
	if err != nil {
		t.Fatalf("VerifyPAT: %v", err)
	}
	if got.Login != "rotator" {
		t.Fatalf("login = %q, want rotator", got.Login)
	}
	if got.Scopes.Status != ScopeStatusOK {
		t.Fatalf("scope status = %v, want OK", got.Scopes.Status)
	}
	// Every request carried the candidate, never some other credential.
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.auth {
		if !strings.HasSuffix(a, " ghp_new") {
			t.Fatalf("request sent with Authorization %q, want the candidate token", a)
		}
	}
}

func TestVerifyPAT_Rejects(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		readable map[string]bool
		scopes   string
		wantErr  string
	}{
		{name: "empty token", token: "  ", wantErr: "empty"},
		{name: "bad credentials", token: "ghp_revoked", readable: map[string]bool{"acme/widgets": true}, scopes: "repo", wantErr: "GET /user"},
		{name: "repo not readable", token: "ghp_new", readable: map[string]bool{}, scopes: "repo, read:org, workflow", wantErr: "cannot read acme/widgets"},
		{name: "missing scope for level", token: "ghp_new", readable: map[string]bool{"acme/widgets": true}, scopes: "read:org", wantErr: "missing scopes"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &patVerifyServer{goodToken: "ghp_new", readable: tt.readable, scopes: tt.scopes}
			api := f.start(t)
			_, err := VerifyPAT(context.Background(), tt.token, api, "acme", []string{"widgets"}, 6, patVerifyLogger())
			if err == nil {
				t.Fatal("VerifyPAT accepted a token that must be refused")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("error %q does not name %q", err, tt.wantErr)
			}
			if strings.TrimSpace(tt.token) != "" && strings.Contains(err.Error(), tt.token) {
				t.Fatalf("error leaks the token: %q", err)
			}
		})
	}
}

// A fine-grained PAT reports no scopes; that is accepted at boot and must be
// accepted on rotation too.
func TestVerifyPAT_AcceptsFineGrainedToken(t *testing.T) {
	f := &patVerifyServer{goodToken: "github_pat_new", readable: map[string]bool{"acme/widgets": true}, scopes: ""}
	api := f.start(t)
	got, err := VerifyPAT(context.Background(), "github_pat_new", api, "acme", []string{"widgets"}, 6, patVerifyLogger())
	if err != nil {
		t.Fatalf("VerifyPAT refused a fine-grained PAT: %v", err)
	}
	if got.Scopes.Status != ScopeStatusUndetermined {
		t.Fatalf("scope status = %v, want Undetermined", got.Scopes.Status)
	}
}
