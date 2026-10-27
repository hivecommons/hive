package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent"
)

// fakeProcNetTCP points the UID lookup at a socket table in which local port
// 43210 (0xA8CA) belongs to uid 2003 and nothing else is listed.
func fakeProcNetTCP(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	tcp := filepath.Join(dir, "tcp")
	tcp6 := filepath.Join(dir, "tcp6")
	if err := os.WriteFile(tcp, []byte("  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode\n"+
		"   0: 0100007F:A8CA 0100007F:480E 01 00000000:00000000 00:00000000 00000000  2003        0 1 0000000000000000 100 0 0 10 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tcp6, []byte("header\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldTCP, oldTCP6 := procNetTCPPath, procNetTCP6Path
	procNetTCPPath, procNetTCP6Path = tcp, tcp6
	t.Cleanup(func() { procNetTCPPath, procNetTCP6Path = oldTCP, oldTCP6 })
}

func translatorReq(remoteAddr, claimedAgent string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:18444/v1/messages", strings.NewReader(`{}`))
	req.RemoteAddr = remoteAddr
	if claimedAgent != "" {
		req.Header.Set("x-api-key", inferenceAPIKeyPrefix+claimedAgent)
	}
	return req
}

// TestTranslatorAgentName_SocketUIDWins pins that the inference translator
// names its caller from the socket owner's UID, not from the x-api-key the
// caller chose: "sk-hive-<name>" is the agent's public name, so a header
// claiming another agent must not hand over that agent's inference route.
func TestTranslatorAgentName_SocketUIDWins(t *testing.T) {
	fakeProcNetTCP(t)
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"scanner": 2003, "quality": 2002}}

	// Mapped UID, header claims a different agent → the UID's agent.
	name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "quality"))
	if !ok || name != "scanner" {
		t.Fatalf("mapped uid with foreign claim: got (%q,%v), want (scanner,true)", name, ok)
	}
	// Mapped UID, no header at all → still identified.
	name, ok = p.translatorAgentName(translatorReq("127.0.0.1:43210", ""))
	if !ok || name != "scanner" {
		t.Fatalf("mapped uid without header: got (%q,%v), want (scanner,true)", name, ok)
	}
}

// TestTranslatorAgentName_UnmappedRefusedUnlessAdvisoryOK pins the fail-closed
// half: with a UID map loaded, a socket whose UID is not an agent is refused
// whatever the header says, unless the operator set HIVE_PROXY_ADVISORY_OK.
func TestTranslatorAgentName_UnmappedRefusedUnlessAdvisoryOK(t *testing.T) {
	fakeProcNetTCP(t)
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"quality": 2002}}

	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "quality")); ok || name != "" {
		t.Fatalf("unmapped uid: got (%q,%v), want refused", name, ok)
	}
	// Port absent from the table → unidentified too.
	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:1", "quality")); ok || name != "" {
		t.Fatalf("unknown socket: got (%q,%v), want refused", name, ok)
	}

	p.proxyAdvisoryOK = true
	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "quality")); !ok || name != "quality" {
		t.Fatalf("advisory-ok unmapped uid: got (%q,%v), want (quality,true)", name, ok)
	}
}

// TestTranslatorAgentName_InternalUIDKeepsHeader pins that the hive's own
// control plane (an internal UID) may still name the agent it is acting for:
// it is the party that issues the sk-hive- keys in the first place.
func TestTranslatorAgentName_InternalUIDKeepsHeader(t *testing.T) {
	fakeProcNetTCP(t)
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	// 2003 is the proxy's own UID here and no agent occupies it.
	p.uidMap = &agent.UIDMap{BaseUID: 2001, ProxyUID: 2003, Agents: map[string]int{"quality": 2002}}

	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "supervisor")); !ok || name != "supervisor" {
		t.Fatalf("internal uid: got (%q,%v), want (supervisor,true)", name, ok)
	}
}

// TestTranslatorAgentName_NoUIDMap pins the no-map deployments: the header is
// trusted only under HIVE_PROXY_ADVISORY_OK, matching fallbackAgentName.
func TestTranslatorAgentName_NoUIDMap(t *testing.T) {
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "quality")); ok || name != "" {
		t.Fatalf("no map, no advisory-ok: got (%q,%v), want refused", name, ok)
	}
	p.proxyAdvisoryOK = true
	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "quality")); !ok || name != "quality" {
		t.Fatalf("no map, advisory-ok: got (%q,%v), want (quality,true)", name, ok)
	}
}

// TestInferenceTranslatorHandler_RefusesUnidentified drives the handler itself:
// an unidentifiable caller gets 401 with an Anthropic-shaped error body and no
// route lookup, so a borrowed name never reaches the backend.
func TestInferenceTranslatorHandler_RefusesUnidentified(t *testing.T) {
	fakeProcNetTCP(t)
	p := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	p.uidMap = &agent.UIDMap{BaseUID: 2001, Agents: map[string]int{"quality": 2002}}

	rec := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(rec, translatorReq("127.0.0.1:43210", "quality"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"authentication_error"`) {
		t.Fatalf("body %q lacks Anthropic-shaped authentication_error", rec.Body.String())
	}
}
