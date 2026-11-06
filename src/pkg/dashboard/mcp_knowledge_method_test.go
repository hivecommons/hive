package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/hivecommons/hive/pkg/dashboard/webstatic"
)

// publicKnowledgeServerWithStaticFiles is publicKnowledgeServer plus the
// catch-all the running server registers in Start for the dashboard's static
// files. Without it the mux has no other route for a GET, so a test server
// answers 405 where a running hive does not.
func publicKnowledgeServerWithStaticFiles(t *testing.T) *Server {
	t.Helper()
	s := publicKnowledgeServer(t)
	s.mux.Handle("GET /", webstatic.FileServer(fstest.MapFS{"index.html": {Data: []byte("<html></html>")}}))
	return s
}

// The MCP path took POST only, so on a running hive a GET on it matched the
// static files catch-all and came back 404 from the file server. An MCP client
// that probes for a stream after initialize expects 405 when there is none.
func TestPublicKnowledgeMCPAnswers405ToGetOnTheRunningServer(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "1")
	s := publicKnowledgeServerWithStaticFiles(t)

	for _, method := range []string{http.MethodGet, http.MethodHead} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, publicKnowledgeMCPPath, nil)
		req.Header.Set("Accept", "text/event-stream")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); got != http.MethodPost {
			t.Errorf("%s: Allow = %q, want POST", method, got)
		}
	}

	// The verbs no route names are still the mux's to refuse.
	for _, method := range []string{http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, httptest.NewRequest(method, publicKnowledgeMCPPath, nil))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: status = %d, want 405", method, rec.Code)
		}
		if got := rec.Header().Get("Allow"); !strings.Contains(got, http.MethodPost) {
			t.Errorf("%s: Allow = %q, want it to name POST", method, got)
		}
	}

	// POST still works, and the static files are still served elsewhere.
	if rec := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`); rec.Code != http.StatusOK {
		t.Fatalf("POST tools/list: status = %d", rec.Code)
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Authorization", "Bearer dashboard-secret")
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<html>") {
		t.Fatalf("GET /: status = %d, body = %q, want the static index page", rec.Code, rec.Body.String())
	}
}

// Switched off, the endpoint is not there at all: a caller who is signed in
// (the dashboard token here) gets 404 for a GET as for a POST, not 405.
func TestPublicKnowledgeMCPDisabledIs404ForGetAndPost(t *testing.T) {
	t.Setenv(publicKnowledgeEnabledEnv, "0")
	s := publicKnowledgeServerWithStaticFiles(t)
	for _, method := range []string{http.MethodGet, http.MethodHead, http.MethodPost} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, publicKnowledgeMCPPath, nil)
		req.Header.Set("Authorization", "Bearer dashboard-secret")
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s while switched off: status = %d, want 404", method, rec.Code)
		}
	}
}
