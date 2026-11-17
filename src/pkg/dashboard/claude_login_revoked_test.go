package dashboard

import (
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

func TestReconcileClaudeLoginRevokedAlertRaisesAndClears(t *testing.T) {
	s := &Server{}
	statuses := map[string]*crux.Process{
		"scanner": {BackendAuth: crux.BackendAuthState{Status: crux.BackendAuthRevoked, LastError: "provider rejected the stored Claude credential (claude usage HTTP 401)"}},
		"quality": {BackendAuth: crux.BackendAuthState{Status: crux.BackendAuthRevoked}},
		"guide":   {BackendAuth: crux.BackendAuthState{Status: crux.BackendAuthOK}},
		"nilproc": nil,
	}
	s.ReconcileClaudeLoginRevokedAlert(statuses)

	s.systemAlertsMu.Lock()
	alerts := append([]SystemAlert(nil), s.systemAlerts...)
	s.systemAlertsMu.Unlock()
	if len(alerts) != 1 || alerts[0].ID != ClaudeLoginRevokedAlertID {
		t.Fatalf("alerts = %+v, want one %s", alerts, ClaudeLoginRevokedAlertID)
	}
	msg := alerts[0].Message
	for _, want := range []string{"Claude login revoked: sign in again", "2 agents are", "quality, scanner", "will not be restarted", "restarted once after the sign-in"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("banner missing %q:\n%s", want, msg)
		}
	}
	if strings.Contains(msg, "guide") {
		t.Fatalf("healthy agent named in the banner:\n%s", msg)
	}
	if alerts[0].Severity != claudeLoginRevokedAlertSeverity {
		t.Fatalf("severity = %q", alerts[0].Severity)
	}

	// Operator signed in: marks lifted, banner gone.
	for _, p := range statuses {
		if p != nil {
			p.BackendAuth.Status = crux.BackendAuthOK
		}
	}
	s.ReconcileClaudeLoginRevokedAlert(statuses)
	s.systemAlertsMu.Lock()
	remaining := len(s.systemAlerts)
	s.systemAlertsMu.Unlock()
	if remaining != 0 {
		t.Fatalf("banner must clear once no agent is marked revoked; %d alert(s) remain", remaining)
	}
}

func TestClaudeLoginRevokedAlertMessageSingular(t *testing.T) {
	msg := ClaudeLoginRevokedAlertMessage([]string{"scanner"})
	if !strings.Contains(msg, "1 agent is waiting") || !strings.Contains(msg, ": scanner.") {
		t.Fatalf("msg = %q", msg)
	}
}
