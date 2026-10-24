package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// loginCodeRequest builds an owner-authenticated POST to the login-code
// endpoint. requireOwnerRole demands both the role header and the server-set
// verified marker (F14); without either the handler never reaches the paths
// these tests exercise — that deny path is already pinned by
// TestV4OwnerOnlyHandlerGapsRejectUnverifiedOwners.
func loginCodeRequest(agent, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/agents/"+agent+"/login-code", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hive-Role", "owner")
	req.Header.Set(ownerRoleVerifiedHeader, "true")
	req.SetPathValue("name", agent)
	return req
}

// Without an agent manager the handler must fail with 503, not panic: the
// endpoint dereferences s.deps.AgentMgr and a dashboard can serve requests
// before its dependencies are wired.
func TestHandleAgentLoginCode_NoAgentManager(t *testing.T) {
	srv := &Server{}
	w := httptest.NewRecorder()
	srv.handleAgentLoginCode(w, loginCodeRequest("scanner", `{"code":"ABCD-1234"}`))

	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("code = %d, want 503 when AgentMgr is nil", w.Code)
	}
}

// A body that is not the expected JSON shape must be a 400, and must be
// rejected before anything is typed toward a pane.
func TestHandleAgentLoginCode_InvalidBody(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleAgentLoginCode(w, loginCodeRequest("scanner", "not json"))

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for a malformed body", w.Code)
	}
}

// The security property of the endpoint: a code carrying a newline would be
// SUBMITTED mid-paste and everything after it typed as a fresh command line.
// crux.Runtime.SubmitLoginCode must refuse it, the handler must surface that as 400,
// and neither the response nor the error may echo the submitted value.
func TestHandleAgentLoginCode_RejectsInjectableCode(t *testing.T) {
	srv := newFullServer(t)
	for _, tc := range []struct{ name, code string }{
		{"newline injection", "ABCD\\nrm -rf /"},
		{"embedded space", "ABCD 1234"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			srv.handleAgentLoginCode(w, loginCodeRequest("scanner", `{"code":"`+tc.code+`"}`))

			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400 — %q must never reach an agent pane", w.Code, tc.code)
			}
			if strings.Contains(w.Body.String(), "rm -rf") {
				t.Error("response echoes the rejected code — errors must describe the shape, never quote the value")
			}
		})
	}
}

// A valid code for an agent the manager does not know must be a 400 with the
// agent named, so the operator learns which side of the request was wrong.
func TestHandleAgentLoginCode_UnknownAgent(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleAgentLoginCode(w, loginCodeRequest("ghost", `{"code":"ABCD-1234"}`))

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 for an unknown agent", w.Code)
	}
	if !strings.Contains(w.Body.String(), "ghost") {
		t.Errorf("body = %q, want the unknown agent named", w.Body.String())
	}
	if strings.Contains(w.Body.String(), "ABCD-1234") {
		t.Error("response echoes the code — it must never be logged or returned")
	}
}

// A known agent with no live terminal session must also be a 400: the code
// has nowhere to go, and the handler must say so rather than pretend success.
func TestHandleAgentLoginCode_KnownAgentWithoutSession(t *testing.T) {
	srv := newFullServer(t)
	w := httptest.NewRecorder()
	srv.handleAgentLoginCode(w, loginCodeRequest("scanner", `{"code":"ABCD-1234"}`))

	if w.Code != http.StatusBadRequest {
		t.Errorf("code = %d, want 400 when the agent has no terminal session", w.Code)
	}
	if strings.Contains(w.Body.String(), "ABCD-1234") {
		t.Error("response echoes the code — it must never be logged or returned")
	}
}
