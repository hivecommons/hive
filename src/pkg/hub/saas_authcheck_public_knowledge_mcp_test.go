package hub

import (
	"net/http"
	"testing"
)

// The hub cannot see a spoke's HIVE_PUBLIC_KNOWLEDGE, so it waves
// /mcp/knowledge through the nginx auth_request gate and lets the spoke
// fail closed (#10615) — the same arrangement as knowledgeExportPath (#8294).
func TestSaaSAuthCheckPublicKnowledgeMCPPathBypassesGate(t *testing.T) {
	cleanup := helperSetupTempDirs(t)
	defer cleanup()
	s := newHandlerHub()
	demotedOwnerFixture(t, s, "spoke-owner", "owned-hive")

	if !isSaaSPublicPath(publicKnowledgeMCPPath) || !isSaaSPublicPath(publicKnowledgeMCPPath+"?x=1") {
		t.Fatalf("%s must be a SaaS public path", publicKnowledgeMCPPath)
	}
	// Only the exact path — nothing else under /mcp/ is public.
	for _, p := range []string{"/mcp", "/mcp/", "/mcp/knowledge/admin", "/mcp/other"} {
		if isSaaSPublicPath(p) {
			t.Errorf("%s must NOT be public", p)
		}
	}

	rec := publicPathAuthCheck(t, s, "owned-hive", publicKnowledgeMCPPath, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("%s anonymous: status = %d, want 200 (a 401 becomes an auth-signin redirect and an external agent never reaches the spoke's own switch)", publicKnowledgeMCPPath, rec.Code)
	}
	for _, h := range []string{"X-Hive-User", "X-Hive-Role", proxyAuthHeader} {
		if got := rec.Header().Get(h); got != "" {
			t.Errorf("%s anonymous: %s = %q, want unset", publicKnowledgeMCPPath, h, got)
		}
	}
}
