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

// bootUIDMap writes a uid-map.json holding "quality" at 2002 with proxy UID
// 1001, points agent.UIDMapPath at it, and builds the proxy the way boot does,
// so the proxy's copy is the one loaded at construction.
func bootUIDMap(t *testing.T, logOut io.Writer) (*GitHubProxy, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "uid-map.json")
	m := agent.NewUIDMap()
	m.Agents["quality"] = 2002
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	old := agent.UIDMapPath
	agent.UIDMapPath = path
	t.Cleanup(func() { agent.UIDMapPath = old })

	p, err := NewGitHubProxyEphemeral(slog.New(slog.NewTextHandler(logOut, nil)), "org", []string{"repo"})
	if err != nil {
		t.Fatal(err)
	}
	if p.loadedUIDMap() == nil {
		t.Fatal("proxy did not load the boot UID map")
	}
	return p, path
}

// addAgentAtRuntime does what Manager.AddAgent / ReconcileAgents do for a new
// agent after boot: allocate the next UID and save the map to disk.
func addAgentAtRuntime(t *testing.T, path, name string) int {
	t.Helper()
	m, err := agent.LoadUIDMap(path)
	if err != nil {
		t.Fatal(err)
	}
	uid := m.AllocateUID(name)
	if err := m.Save(path); err != nil {
		t.Fatal(err)
	}
	return uid
}

// TestUIDMap_RuntimeAddedAgentOwnSocketAccepted pins #18184: an agent the
// manager adds after the proxy was built is identified from its own socket
// without a restart, instead of being refused as uid_not_mapped.
func TestUIDMap_RuntimeAddedAgentOwnSocketAccepted(t *testing.T) {
	var buf strings.Builder
	p, path := bootUIDMap(t, &buf)
	uid := addAgentAtRuntime(t, path, "scanner")
	if uid != 2003 {
		t.Fatalf("allocated uid = %d, want 2003", uid)
	}
	fakeProcNetTCPUID(t, uid)

	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "")); !ok || name != "scanner" {
		t.Fatalf("runtime-added agent's own socket: got (%q,%v), want (scanner,true)", name, ok)
	}
	if got := p.identifyAgentByUID("127.0.0.1:43210"); got != "scanner" {
		t.Fatalf("identifyAgentByUID = %q, want scanner", got)
	}
	if strings.Contains(buf.String(), "uid_not_mapped") {
		t.Fatalf("runtime-added agent refused as uid_not_mapped: %s", buf.String())
	}
}

// TestUIDMap_ProxyUIDClaimOnRuntimeAddedAgentRefused pins #18184 against the
// #18133 invariant: once an agent added at runtime owns a UID, a proxy-UID
// caller claiming its name is refused with proxy_uid_claims_isolated_agent
// rather than accepted because the proxy's map was stale.
func TestUIDMap_ProxyUIDClaimOnRuntimeAddedAgentRefused(t *testing.T) {
	var buf strings.Builder
	p, path := bootUIDMap(t, &buf)
	p.inference = newInferenceRouter()
	addAgentAtRuntime(t, path, "scanner")
	p.inference.Set("scanner", &InferenceRoute{Backend: "vllm", Endpoint: "http://127.0.0.1:1", Model: "m", APIKey: "victim-secret"})

	// The name check alone must see the new agent, independent of any UID
	// lookup having refreshed the map first.
	if !p.sharedUIDClaimsIsolatedAgent(1001, "scanner") {
		t.Fatal("sharedUIDClaimsIsolatedAgent did not see the runtime-added agent")
	}

	fakeProcNetTCPUID(t, 1001)
	if name, ok := p.translatorAgentName(translatorReq("127.0.0.1:43210", "scanner")); ok || name != "" {
		t.Fatalf("proxy uid claiming runtime-added agent: got (%q,%v), want refused", name, ok)
	}
	rec := httptest.NewRecorder()
	p.inferenceTranslatorHandler().ServeHTTP(rec, translatorReq("127.0.0.1:43210", "scanner"))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(buf.String(), "reason=proxy_uid_claims_isolated_agent") {
		t.Errorf("refusal log %q missing reason=proxy_uid_claims_isolated_agent", buf.String())
	}
	if strings.Contains(buf.String(), "victim-secret") {
		t.Errorf("log %q leaked the victim's route key", buf.String())
	}
}

// TestUIDMap_RefreshOnlyWhenFileChanges pins the reload discipline: an
// unchanged file is not re-read, a malformed rewrite keeps the previous map,
// and a map absent at construction never appears mid-run.
func TestUIDMap_RefreshOnlyWhenFileChanges(t *testing.T) {
	p, path := bootUIDMap(t, io.Discard)
	before := p.loadedUIDMap()
	if p.refreshUIDMap() != nil || p.loadedUIDMap() != before {
		t.Fatal("unchanged uid-map.json was reloaded")
	}

	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p.refreshUIDMap() != nil || p.loadedUIDMap() != before {
		t.Fatal("malformed uid-map.json replaced the loaded map")
	}
	if got := p.lookupUIDByName("quality"); got != 2002 {
		t.Fatalf("after malformed rewrite, quality uid = %d, want 2002", got)
	}

	noMap := &GitHubProxy{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if noMap.refreshUIDMap() != nil || noMap.lookupAgentByUID(2002) != "" || noMap.loadedUIDMap() != nil {
		t.Fatal("proxy with no boot UID map loaded one at runtime")
	}
}
