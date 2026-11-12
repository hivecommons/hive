package agent

import (
	"bytes"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/config"
)

// operatorModeFixture is a manager in operator-token mode (the token field set
// as NewManager does from HIVE_CLAUDE_OAUTH_TOKEN) with one claude agent whose
// credential link a refresh replaced, plus the log it writes.
func operatorModeFixture(t *testing.T) (*Manager, *fakeAuditSink, *AgentProcess, string, string, *bytes.Buffer) {
	t.Helper()
	m, sink, ap, link, target := bridgeTestFixture(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m.claudeOperatorToken = operatorTokenSentinel
	now := time.Now()
	if err := os.WriteFile(target, bridgeTestCreds(t, "shared", now.Add(10*time.Minute).UnixMilli()), 0o660); err != nil {
		t.Fatal(err)
	}
	simulateClaudeRefresh(t, link, bridgeTestCreds(t, "private", now.Add(8*time.Hour).UnixMilli()))
	return m, sink, ap, link, target, &logs
}

// While the operator token is set the re-bridge does nothing on any trigger:
// the private file stays a private file, the shared login is untouched,
// and nothing is audited.
func TestClaudeOperatorTokenMode_RebridgeIsSuspended(t *testing.T) {
	m, sink, _, link, target, logs := operatorModeFixture(t)
	sharedBefore, err := os.ReadFile(target)
	if err != nil {
		t.Fatal(err)
	}
	agentBefore, err := os.ReadFile(link)
	if err != nil {
		t.Fatal(err)
	}

	for _, trigger := range []string{claudeBridgeTriggerWatchdog, claudeBridgeTriggerSignIn} {
		if got := m.RepairClaudeCredentialBridges(trigger); len(got) != 0 {
			t.Fatalf("trigger %s: RepairClaudeCredentialBridges = %v, want none in operator-token mode", trigger, got)
		}
	}
	if rep := m.rebridgeClaudeCredential("scanner", link, target, claudeBridgeTriggerLaunch); rep != nil {
		t.Fatalf("launch re-bridge ran in operator-token mode: %+v", rep)
	}
	m.rebridgeClaudeCredentialsAfterSignIn()

	if fi, err := os.Lstat(link); err != nil || !fi.Mode().IsRegular() {
		t.Fatalf("agent's private credential must be left exactly where it is; Lstat: %v %v", fi, err)
	}
	if got, _ := os.ReadFile(link); !bytes.Equal(got, agentBefore) {
		t.Error("agent's private credential was rewritten")
	}
	if got, _ := os.ReadFile(target); !bytes.Equal(got, sharedBefore) {
		t.Error("shared credential was rewritten")
	}
	if stale, _ := filepath.Glob(filepath.Join(filepath.Dir(link), "*"+claudeCredentialStaleInfix+"*")); len(stale) != 0 {
		t.Errorf("files were moved aside: %v", stale)
	}
	if n := sink.count(); n != 0 {
		t.Errorf("re-bridge audited %d entries in operator-token mode, want 0", n)
	}
	if n := strings.Count(logs.String(), "watchdog alerts are suspended"); n != 1 {
		t.Errorf("suspension logged %d times across several calls, want exactly once:\n%s", n, logs.String())
	}
	if strings.Contains(logs.String(), operatorTokenSentinel) {
		t.Fatal("operator token reached the log")
	}
}

// While the operator token is set the watchdog never alerts about the Claude
// credentials file — absent, spent or otherwise — but still watches Copilot.
func TestClaudeOperatorTokenMode_WatchdogClaudeProbeIsSuspended(t *testing.T) {
	m, sink, _, _, _, logs := operatorModeFixture(t)
	m.mu.Lock()
	m.agents["reviewer"] = &AgentProcess{Name: "reviewer", UID: 2002, Config: config.AgentConfig{Backend: "copilot"}}
	m.mu.Unlock()

	missing := filepath.Join(t.TempDir(), "missing")
	last := map[string]bool{"claude": true} // a stale observation from before token mode is cleared, not re-alerted
	m.evalCredentialWatch(credentialWatch{backend: "claude", path: missing,
		auditAction: AuditClaudeTokenMissing, probe: claudeTokenUsable}, last)
	if sink.count() != 0 {
		t.Fatalf("claude watch audited %d entries in operator-token mode, want 0", sink.count())
	}
	if _, tracked := last["claude"]; tracked {
		t.Error("suspended claude watch must not keep a transition record")
	}
	if strings.Contains(logs.String(), "durable credential unusable") {
		t.Errorf("claude watch warned in operator-token mode:\n%s", logs.String())
	}

	m.evalCredentialWatch(credentialWatch{backend: "copilot", path: missing,
		auditAction: AuditCopilotTokenMissing, probe: copilotTokenUsable}, last)
	if e := sink.find(AuditCopilotTokenMissing); e == nil {
		t.Fatal("copilot watch must keep alerting in operator-token mode")
	}
}

// Unset, the same fixture behaves exactly as today: the re-bridge repairs the
// link and the watchdog probes Claude.
func TestClaudeOperatorTokenMode_UnsetKeepsTodaysBehaviour(t *testing.T) {
	m, sink, _, link, target, logs := operatorModeFixture(t)
	m.claudeOperatorToken = ""

	if got := m.RepairClaudeCredentialBridges(claudeBridgeTriggerWatchdog); len(got) != 1 || got[0] != "scanner" {
		t.Fatalf("RepairClaudeCredentialBridges = %v, want [scanner]", got)
	}
	assertLinkRestored(t, link, target)
	last := map[string]bool{}
	m.evalCredentialWatch(credentialWatch{backend: "claude", path: filepath.Join(t.TempDir(), "missing"),
		auditAction: AuditClaudeTokenMissing, probe: claudeTokenUsable}, last)
	if e := sink.find(AuditClaudeTokenMissing); e == nil {
		t.Fatal("claude watch must alert when no operator token is set")
	}
	if strings.Contains(logs.String(), "watchdog alerts are suspended") {
		t.Error("suspension must not be logged when the token is unset")
	}
}
