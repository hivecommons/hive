package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux/agent/copilottoken"
	"github.com/hivecommons/hive/crux/config"
)

// Running-agent relink after the Copilot CLI's own save (#18671). Every token
// value carries copilotBridgeTestSentinel so assertNoSentinelLogged proves no
// log line leaks token material.

const (
	// copilotBridgeTestCLISaved is what the CLI's save leaves behind: the token
	// it had in memory plus fields it wrote itself.
	copilotBridgeTestCLISaved = `{"copilotTokens":{"https://github.com:fake-user":"gho_older_` + copilotBridgeTestSentinel + `"},"banner":"never","trustedFolders":["/work"]}`
	// copilotBridgeTestNewToken is the token a later dashboard login writes.
	copilotBridgeTestNewToken = "gho_dashboard_" + copilotBridgeTestSentinel
)

func readCopilotBridgeConfig(t *testing.T, path string) map[string]interface{} {
	t.Helper()
	cfg, err := copilottoken.ReadConfig(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return cfg
}

func copilotBridgeTokenAt(t *testing.T, path string) string {
	t.Helper()
	tokens, ok := readCopilotBridgeConfig(t, path)["copilotTokens"].(map[string]interface{})
	if !ok {
		t.Fatalf("%s has no copilotTokens map", path)
	}
	tok, _ := tokens["https://github.com:fake-user"].(string)
	return tok
}

// simulateDashboardLogin writes a new login to the shared file the way the
// dashboard device flow does: a whole-file atomic rewrite of the shared path.
func simulateDashboardLogin(t *testing.T, target, token string) {
	t.Helper()
	cfg := readCopilotBridgeConfig(t, target)
	cfg["copilotTokens"] = map[string]interface{}{"https://github.com:fake-user": token}
	if err := copilottoken.WriteConfig(target, cfg, sharedConfigDesiredMode); err != nil {
		t.Fatal(err)
	}
}

func TestRelinkCopilotConfigAfterCLISave_RelinksAndMergesCLIFields(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestCLISaved)

	// One poll tick.
	if !m.relinkCopilotConfigAfterCLISave(ap) {
		t.Fatal("a private copy left by the CLI's save must be relinked on the next poll")
	}
	assertCopilotLinked(t, link, target)

	cfg := readCopilotBridgeConfig(t, target)
	if cfg["banner"] != "never" {
		t.Fatalf("CLI-written field lost by the relink: banner=%v", cfg["banner"])
	}
	if folders, ok := cfg["trustedFolders"].([]interface{}); !ok || len(folders) != 1 || folders[0] != "/work" {
		t.Fatalf("CLI-written field lost by the relink: trustedFolders=%v", cfg["trustedFolders"])
	}
	// The shared login wins over the token the CLI had in memory.
	if got := copilotBridgeTokenAt(t, target); got != "gho_"+copilotBridgeTestSentinel {
		t.Fatal("the merge must keep the shared login, not the private copy's older token")
	}
	if b := copilotBackups(t, link); len(b) != 1 {
		t.Fatalf("want one backup of the private copy, got %v", b)
	}

	// The next tick, with the link intact, does nothing.
	if m.relinkCopilotConfigAfterCLISave(ap) {
		t.Fatal("an intact link must not be repaired again")
	}
	assertNoSentinelLogged(t, buf)
}

func TestRelinkCopilotConfigAfterCLISave_DashboardLoginReachesRunningAgent(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)

	// The CLI's save seconds after launch, then the poller's relink.
	simulateCopilotSave(t, link, copilotBridgeTestLoggedIn)
	if !m.relinkCopilotConfigAfterCLISave(ap) {
		t.Fatal("private copy must be relinked")
	}

	// A dashboard re-login updates only the shared file. The running agent's
	// effective config (the file at its own path) sees it with no restart.
	simulateDashboardLogin(t, target, copilotBridgeTestNewToken)
	if got := copilotBridgeTokenAt(t, link); got != copilotBridgeTestNewToken {
		t.Fatal("running agent's config.json must see the dashboard login without a restart")
	}

	// The CLI saves again with the token it still holds in memory. The next
	// tick relinks without rolling the fleet back to that older token.
	simulateCopilotSave(t, link, copilotBridgeTestCLISaved)
	if !m.relinkCopilotConfigAfterCLISave(ap) {
		t.Fatal("second CLI save must be relinked too")
	}
	assertCopilotLinked(t, link, target)
	if got := copilotBridgeTokenAt(t, link); got != copilotBridgeTestNewToken {
		t.Fatal("a later CLI save must not overwrite the dashboard login")
	}
	if got := copilotBridgeTokenAt(t, target); got != copilotBridgeTestNewToken {
		t.Fatal("shared login must still be the dashboard login")
	}
	if b := copilotBackups(t, link); len(b) != 1 {
		t.Fatalf("only the newest backup is kept, got %v", b)
	}
	assertNoSentinelLogged(t, buf)
}

func TestRelinkCopilotConfigAfterCLISave_KeepsOnlyLoginWhenSharedEmpty(t *testing.T) {
	m, buf, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestLoggedIn)
	writeCopilotBridgeFile(t, target, copilotBridgeTestNoLogin)

	for i := 0; i < 3; i++ {
		if m.relinkCopilotConfigAfterCLISave(ap) {
			t.Fatal("a private copy holding the only login must not be relinked")
		}
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private login must stay in place: info=%v err=%v", info, err)
	}
	if data, err := os.ReadFile(target); err != nil || string(data) != copilotBridgeTestNoLogin {
		t.Fatalf("shared file must not be touched by a refused repair: err=%v", err)
	}
	if got := strings.Count(buf.String(), "repair not applied"); got != 1 {
		t.Fatalf("refusal must be logged once across polls, got %d:\n%s", got, buf.String())
	}
	assertNoSentinelLogged(t, buf)
}

func TestRelinkCopilotConfigAfterCLISave_WaitsForSharedWriter(t *testing.T) {
	m, _, ap, link, target := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestCLISaved)

	sharedCopilotConfigMu.Lock()
	relinked := m.relinkCopilotConfigAfterCLISave(ap)
	sharedCopilotConfigMu.Unlock()
	if relinked {
		t.Fatal("must not relink while another writer holds the shared config")
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("private copy must stay until the next tick: info=%v err=%v", info, err)
	}
	if b := copilotBackups(t, link); len(b) != 0 {
		t.Fatalf("nothing may be moved aside while busy, got %v", b)
	}

	if !m.relinkCopilotConfigAfterCLISave(ap) {
		t.Fatal("next tick must relink once the writer is done")
	}
	assertCopilotLinked(t, link, target)
	if readCopilotBridgeConfig(t, target)["banner"] != "never" {
		t.Fatal("CLI-written field must be merged on the retry")
	}
}

func TestRelinkCopilotConfigAfterCLISave_IgnoresOtherAgents(t *testing.T) {
	m, _, _, link, _ := copilotBridgeFixture(t)
	simulateCopilotSave(t, link, copilotBridgeTestNoLogin)
	other := &AgentProcess{Name: copilotBridgeTestAgentName, UID: copilotBridgeTestUID, Config: config.AgentConfig{Backend: "claude"}}
	if m.relinkCopilotConfigAfterCLISave(other) {
		t.Fatal("non-copilot backends are not relinked by the copilot hook")
	}
	noHome := &AgentProcess{Name: "planner", Config: config.AgentConfig{Backend: "copilot"}}
	if m.relinkCopilotConfigAfterCLISave(noHome) {
		t.Fatal("agent without a per-agent home has nothing to relink")
	}
	if info, err := os.Lstat(link); err != nil || !info.Mode().IsRegular() {
		t.Fatalf("file must stay in place: info=%v err=%v", info, err)
	}
}

func TestMergeCopilotConfigFields(t *testing.T) {
	shared := map[string]interface{}{
		"copilotTokens":    map[string]interface{}{"k": "shared"},
		"lastLoggedInUser": "https://github.com:shared",
		"model":            "shared-model",
		"sharedOnly":       true,
	}
	private := map[string]interface{}{
		"copilotTokens": map[string]interface{}{"k": "private"},
		"loggedInUsers": []interface{}{"https://github.com:private"},
		"model":         "cli-model",
		"privateOnly":   "x",
	}
	out := mergeCopilotConfigFields(shared, private)

	if tok := out["copilotTokens"].(map[string]interface{})["k"]; tok != "shared" {
		t.Fatalf("copilotTokens must come from shared, got %v", tok)
	}
	if out["lastLoggedInUser"] != "https://github.com:shared" {
		t.Fatalf("identity must come from shared, got %v", out["lastLoggedInUser"])
	}
	if _, ok := out["loggedInUsers"]; ok {
		t.Fatal("a credential field absent from shared must not be imported from private")
	}
	if out["model"] != "cli-model" || out["privateOnly"] != "x" || out["sharedOnly"] != true {
		t.Fatalf("non-credential fields: want private over shared, got %v", out)
	}
	if shared["model"] != "shared-model" || len(shared) != 4 {
		t.Fatal("inputs must not be modified")
	}
}
