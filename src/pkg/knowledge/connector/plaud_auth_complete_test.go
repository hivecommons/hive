package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestPlaudCompleteAuthInstallsPastedTokens(t *testing.T) {
	srv := newPlaudTestServer()
	pc := newTestPlaud(t, srv, nil)
	raw := `{"access_token":"at","refresh_token":"rt","token_type":"Bearer","expires_at":1}`
	st, err := pc.CompleteAuth(context.Background(), AuthCompletion{Tokens: raw})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(pc.tokenPath())
	if err != nil {
		t.Fatalf("token file not written: %v", err)
	}
	if strings.TrimSpace(string(data)) != raw {
		t.Fatalf("token file = %q", data)
	}
	if !strings.HasSuffix(pc.tokenPath(), ".plaud/tokens-mcp.json") || !strings.HasPrefix(pc.tokenPath(), pc.stateDir) {
		t.Fatalf("token path %q should live under the connector home", pc.tokenPath())
	}
	if !st.Connected || st.Account != "andy@example.com" {
		t.Fatalf("expected refreshed connected state, got %+v", st)
	}
}

func TestPlaudCompleteAuthRejectsBadInput(t *testing.T) {
	pc := newTestPlaud(t, newPlaudTestServer(), nil)
	ctx := context.Background()
	if _, err := pc.CompleteAuth(ctx, AuthCompletion{}); err == nil {
		t.Fatal("empty completion must fail")
	}
	if _, err := pc.CompleteAuth(ctx, AuthCompletion{Tokens: "not json"}); err == nil {
		t.Fatal("non-JSON tokens must fail")
	}
	if _, err := pc.CompleteAuth(ctx, AuthCompletion{Tokens: `{"token_type":"Bearer"}`}); err == nil {
		t.Fatal("tokens without access/refresh token must fail")
	}
	if _, err := pc.CompleteAuth(ctx, AuthCompletion{RedirectURL: "http://localhost:8199/auth/callback"}); err == nil {
		t.Fatal("redirect without query must fail")
	}
	if _, err := pc.CompleteAuth(ctx, AuthCompletion{RedirectURL: "http://localhost:8199/auth/callback?code=x&state=y"}); err != ErrNoPendingLogin {
		t.Fatalf("redirect without pending login: got %v, want ErrNoPendingLogin", err)
	}
}

func TestPlaudCompleteAuthForwardsRedirectToPendingLogin(t *testing.T) {
	// A live login session: the fake callback listener plays the login
	// goroutine, which finishes (closes done) once the callback lands.
	l := &plaudLogin{done: make(chan struct{}), cancel: func() {}}
	got := make(chan string, 1)
	cb := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.URL.Path + "?" + r.URL.RawQuery
		close(l.done)
		w.WriteHeader(http.StatusOK)
	}))
	defer cb.Close()
	pc := newTestPlaud(t, newPlaudTestServer(), nil)
	pc.callbackBase = cb.URL
	pc.mu.Lock()
	pc.login = l
	pc.auth = AuthState{Pending: true}
	pc.mu.Unlock()
	_, err := pc.CompleteAuth(context.Background(), AuthCompletion{RedirectURL: "http://localhost:8199/auth/callback?code=abc&state=xyz"})
	if err != nil {
		t.Fatal(err)
	}
	if fwd := <-got; fwd != "/auth/callback?code=abc&state=xyz" {
		t.Fatalf("forwarded %q", fwd)
	}
}
