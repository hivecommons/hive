package loginscan

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// revokedFakeManager is the loop-test fake plus the optional #18732 extension:
// the manager verified the credential against the provider and was refused.
type revokedFakeManager struct {
	fakeLoginScanManager
	revoked map[string]string
}

func (m *revokedFakeManager) AgentCredentialRevoked(name string) (bool, string) {
	detail, ok := m.revoked[name]
	return ok, detail
}

// #18732: a login prompt on an agent whose credential FILE reads valid but
// whose provider said 401 must neither be left to the token-restart heal
// ("credential is valid — leaving it to the token-restart heal") nor paused.
// It pages the operator once and leaves the agent running so the sign-in
// restart can reach it.
func TestScanRoutesRevokedCredentialToOperatorWithoutPauseOrRestart(t *testing.T) {
	mgr := &revokedFakeManager{
		fakeLoginScanManager: fakeLoginScanManager{
			statuses: map[string]*crux.Process{
				"quality": {State: crux.StateRunning},
				"scanner": {State: crux.StateRunning},
			},
			outputs: map[string][]string{
				"quality": {"Please run /login"},
				"scanner": {"Please run /login"},
			},
			// Exactly the incident shape: the file still proves "valid".
			validCreds: map[string]bool{"quality": true, "scanner": true},
		},
		revoked: map[string]string{"quality": "claude usage HTTP 401"},
	}
	notifier := &fakeLoginScanNotifier{}
	auditor := &fakeLoginScanAuditor{}
	sightings := NewSightingTracker()
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, nil))
	cfg := loginScanLoopConfig([]string{"please run /login"}, map[string]string{
		"quality": "claude",
		"scanner": "claude",
	})

	for cycle := 0; cycle < 3; cycle++ {
		Scan(context.Background(), cfg, mgr, notifier, auditor, logger, sightings)
	}

	if len(mgr.pauses) != 0 {
		t.Fatalf("revoked credential must not pause the agent (it would not self-heal after sign-in): pauses=%v", mgr.pauses)
	}
	if len(mgr.refreshes) != 0 {
		t.Fatalf("no per-agent token refresh is attempted on the operator path: %v", mgr.refreshes)
	}
	if len(notifier.sent) != 1 {
		t.Fatalf("operator paged %d times over three cycles, want exactly once: %v", len(notifier.sent), notifier.sent)
	}
	if !strings.Contains(notifier.sent[0], "Claude login revoked: quality") || !strings.Contains(notifier.sent[0], "claude usage HTTP 401") {
		t.Fatalf("notification = %q", notifier.sent[0])
	}
	logs := logBuf.String()
	if !strings.Contains(logs, "login revoked detected") || !strings.Contains(logs, "agent=quality") {
		t.Fatalf("missing the operator-facing WARN:\n%s", logs)
	}
	// The sibling agent whose credential was NOT reported revoked keeps the
	// pre-#18732 routing: file valid → token-restart heal's case.
	if !strings.Contains(logs, "leaving it to the token-restart heal") || !strings.Contains(logs, "agent=scanner") {
		t.Fatalf("non-revoked agent must keep the authenticated-defer path:\n%s", logs)
	}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "leaving it to the token-restart heal") && strings.Contains(line, "agent=quality") {
			t.Fatalf("revoked agent must not be deferred to the restart heal:\n%s", line)
		}
	}
	if strings.Contains(logs, "sk-ant") || strings.Contains(notifier.sent[0], "sk-ant") {
		t.Fatal("token material leaked")
	}
}

// Once the manager stops reporting the credential revoked (operator signed
// in), the once-per-episode page re-arms for a later episode.
func TestScanRevokedAlertRearmsAfterRecovery(t *testing.T) {
	mgr := &revokedFakeManager{
		fakeLoginScanManager: fakeLoginScanManager{
			statuses:   map[string]*crux.Process{"quality": {State: crux.StateRunning}},
			outputs:    map[string][]string{"quality": {"Please run /login"}},
			validCreds: map[string]bool{"quality": true},
		},
		revoked: map[string]string{"quality": "claude usage HTTP 401"},
	}
	notifier := &fakeLoginScanNotifier{}
	sightings := NewSightingTracker()
	cfg := loginScanLoopConfig([]string{"please run /login"}, map[string]string{"quality": "claude"})

	Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, testLogger(), sightings)
	Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, testLogger(), sightings)
	if len(notifier.sent) != 1 {
		t.Fatalf("want one page, got %d", len(notifier.sent))
	}

	// Operator signed in: the manager no longer reports revocation.
	mgr.revoked = map[string]string{}
	mgr.outputs["quality"] = []string{"❯ ready"}
	Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, testLogger(), sightings)

	// A later, separate revocation pages again.
	mgr.revoked = map[string]string{"quality": "claude usage HTTP 401"}
	mgr.outputs["quality"] = []string{"Please run /login"}
	Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, testLogger(), sightings)
	if len(notifier.sent) != 2 {
		t.Fatalf("a new episode must page again: got %d pages", len(notifier.sent))
	}
	if len(mgr.pauses) != 0 {
		t.Fatalf("unexpected pauses: %v", mgr.pauses)
	}
}

// A runtime without the #18732 extension keeps every pre-existing verdict.
func TestScanWithoutRevocationReporterIsUnchanged(t *testing.T) {
	mgr := &fakeLoginScanManager{
		statuses:   map[string]*crux.Process{"valid": {State: crux.StateRunning}},
		outputs:    map[string][]string{"valid": {"Please run /login"}},
		validCreds: map[string]bool{"valid": true},
	}
	notifier := &fakeLoginScanNotifier{}
	cfg := loginScanLoopConfig([]string{"please run /login"}, map[string]string{"valid": "claude"})
	Scan(context.Background(), cfg, mgr, notifier, &fakeLoginScanAuditor{}, testLogger(), NewSightingTracker())
	if len(notifier.sent) != 0 || len(mgr.pauses) != 0 {
		t.Fatalf("authenticated defer must stay silent: sent=%v pauses=%v", notifier.sent, mgr.pauses)
	}
}
