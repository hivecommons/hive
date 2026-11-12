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
	shared := withSharedAgentHome(t)
	m, sink := testManagerWithSink(t)
	var logs bytes.Buffer
	m.logger = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m.claudeOperatorToken = operatorTokenSentinel
	ap := &AgentProcess{Name: "architect", UID: 2001, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Lock()
	m.agents["architect"] = ap
	m.mu.Unlock()
	link, target := brokenBridgeFixture(t, shared, "architect",
		fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh"),
		fakeClaudeCredential("fake-private-access", time.Now().Add(time.Hour), "fake-private-refresh"))
	return m, sink, ap, link, target, &logs
}

// While the operator token is set the re-bridge does nothing on any trigger:
// the private file stays a private file, the shared login is untouched,
// nothing is audited, and the status no longer flags the bridge as broken.
func TestClaudeOperatorTokenMode_RebridgeIsSuspended(t *testing.T) {
	m, sink, ap, link, target, logs := operatorModeFixture(t)
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
	if rep := m.rebridgeClaudeCredential("architect", link, target, claudeBridgeTriggerLaunch); rep != nil {
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
	if m.snapshotAgent(ap).CredentialBridgeBroken {
		t.Error("status must not flag the unused bridge as broken in operator-token mode")
	}
	if !ap.snapshot().CredentialBridgeBroken {
		t.Error("fixture sanity: the raw snapshot should still see the replaced link")
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
// link, the status reports it until then, and the watchdog probes Claude.
func TestClaudeOperatorTokenMode_UnsetKeepsTodaysBehaviour(t *testing.T) {
	m, sink, ap, _, _, logs := operatorModeFixture(t)
	m.claudeOperatorToken = ""

	if !m.snapshotAgent(ap).CredentialBridgeBroken {
		t.Fatal("status must report the replaced link when no operator token is set")
	}
	if got := m.RepairClaudeCredentialBridges(claudeBridgeTriggerWatchdog); len(got) != 1 || got[0] != "architect" {
		t.Fatalf("RepairClaudeCredentialBridges = %v, want [architect]", got)
	}
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
