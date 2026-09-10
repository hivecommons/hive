package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// Agent-control mutations are OWNER-only (#6557). Before the gate, the only
// defense was roleEnforcement's read-only write-gate, which blocks role=="read"
// and nothing else — so any read-write or merger user proxied by the hub could
// kick an agent with an arbitrary 10,000-char prompt (typed verbatim into a
// CLI session that executes shell commands with App-scoped credentials),
// switch its backend or model, restart it, or clear its restart counter.
//
// Like TestContributorManagementHandlersAreOwnerGated (audit F14), this
// asserts the INVARIANT on the source rather than one handler's behaviour:
// the dangerous failure mode is a merge silently dropping a gate.

var ownerGatedAgentControlHandlers = []string{
	"handleKick",
	"handleSwitch",
	"handleModelSet",
	"handleRestart",
	"handleResetRestarts",
	"handlePin",
	"handleUnpin",
}

func TestAgentControlHandlersAreOwnerGated(t *testing.T) {
	raw, err := os.ReadFile("api.go")
	if err != nil {
		t.Fatalf("read api.go: %v", err)
	}
	src := string(raw)

	for _, name := range ownerGatedAgentControlHandlers {
		body := handlerBody(t, src, name)
		if !strings.Contains(body, "requireOwnerRole(w, r)") {
			t.Errorf("%s has no requireOwnerRole gate — a read-write member can reach it (#6557). "+
				"If a merge dropped this, restore it rather than relaxing the test.", name)
		}
	}
}

// TestAgentControlEndpointsRejectNonOwner exercises the behaviour end to end:
// a hub-proxied read-write identity must get 403 from every agent-control
// mutation, and the same request marked as a verified owner must get past the
// gate (any non-403 status proves the gate, not the handler, was the decider).
func TestAgentControlEndpointsRejectNonOwner(t *testing.T) {
	s, _ := apiServer(t)

	endpoints := []string{
		"/api/kick/scanner",
		"/api/switch/scanner/claude",
		"/api/model/scanner/some-model",
		"/api/restart/scanner",
		"/api/reset-restarts/scanner",
		"/api/pin/scanner/model",
		"/api/unpin/scanner/model",
	}

	for _, path := range endpoints {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Hive-Role", "read-write")
		req.Header.Set("X-Hive-User", "contributor")
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("POST %s as read-write: got %d, want 403", path, rec.Code)
		}

		req = httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
		req.Header.Set("Content-Type", "application/json")
		markOwnerRequest(req)
		rec = httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		if rec.Code == http.StatusForbidden {
			t.Errorf("POST %s as verified owner: got 403, owner must pass the gate", path)
		}
	}
}
