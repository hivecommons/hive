package agent

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
)

// #18732: a revoked Claude grant leaves a credentials file every on-disk
// predicate accepts, so the login-prompt heal restarted agents into the same
// dead login until the 24h budget ran out and then went quiet. Pinned here:
//
//  1. well-formed file + provider 401 → no restart, operator alert raised
//     (BackendAuthRevoked, LastError), budget untouched;
//  2. operator re-login after `giving up` → agents restarted once regardless
//     of the window.

// revokedTestManager builds a manager around one Claude agent whose HOME holds
// a valid-looking credential, with the provider seam answering verdict.
func revokedTestManager(t *testing.T, verdict claude.CredentialVerdict) (*Manager, *AgentProcess, *int) {
	t.Helper()
	dir := t.TempDir()
	agent := claudeHomeAgent(t, dir)
	credDir := filepath.Join(dir, ".claude")
	if err := os.MkdirAll(credDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(credDir, ".credentials.json"), claudeCredJSON(t, time.Now().Add(4*time.Hour)), 0o600); err != nil {
		t.Fatal(err)
	}
	calls := 0
	m := &Manager{
		agents: map[string]*AgentProcess{agent.Name: agent},
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	m.claudeLogin.verify = func(_ context.Context, credPath string) (claude.CredentialVerdict, string) {
		calls++
		if strings.Contains(credPath, "sk-ant") {
			t.Fatalf("credential path must never carry token material: %q", credPath)
		}
		return verdict, "claude usage HTTP 401"
	}
	if !m.AgentHasValidCredential(agent.Name) {
		t.Fatal("precondition: the file-level gate must accept the credential, exactly as in the incident")
	}
	return m, agent, &calls
}

func TestLoginPromptHealRevokedCredentialAlertsAndNeverRestarts(t *testing.T) {
	m, agent, calls := revokedTestManager(t, claude.CredentialRejected)
	now := time.Now()
	gen := m.claudeSignInGeneration()

	for i := 0; i < 5; i++ {
		if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, now.Add(time.Duration(i)*3*time.Second)); got != loginHealOperatorLogin {
			t.Fatalf("poll %d: heal = %v, want operator-login", i, got)
		}
	}
	if *calls != 1 {
		t.Fatalf("provider asked %d times across five polls; the verdict must be cached for %s", *calls, claudeCredentialVerifyInterval)
	}
	// Budget untouched: nothing fired, nothing booked, no give-up latch.
	if n := agent.tokenRestartsInWindow(now); n != 0 || agent.tokenRestartAttempts != 0 || agent.tokenRestartGaveUp {
		t.Fatalf("restart ledger touched: window=%d attempts=%d gaveUp=%v", n, agent.tokenRestartAttempts, agent.tokenRestartGaveUp)
	}
	// Operator alert raised on every surface that reads it.
	if agent.BackendAuth.Status != BackendAuthRevoked {
		t.Fatalf("BackendAuth.Status = %q, want %q", agent.BackendAuth.Status, BackendAuthRevoked)
	}
	if !strings.HasPrefix(agent.LastError, ClaudeLoginRevokedMessage) {
		t.Fatalf("LastError = %q, want the operator one-liner first", agent.LastError)
	}
	if revoked, detail := m.AgentCredentialRevoked(agent.Name); !revoked || !strings.Contains(detail, "HTTP 401") {
		t.Fatalf("AgentCredentialRevoked = %v %q", revoked, detail)
	}
	if refreshable, known := m.AgentCredentialRefreshable(agent.Name); refreshable || !known {
		t.Fatalf("AgentCredentialRefreshable = %v,%v; a revoked grant must not read as refreshable", refreshable, known)
	}
	for _, token := range []string{"sk-ant-oat-test", "sk-ant-ort-test"} {
		if strings.Contains(agent.LastError, token) || strings.Contains(agent.BackendAuth.LastError, token) {
			t.Fatalf("token material leaked into an operator-facing field")
		}
	}
}

func TestLoginPromptHealAcceptedCredentialKeepsTokenRestart(t *testing.T) {
	m, agent, _ := revokedTestManager(t, claude.CredentialAccepted)
	gen := m.claudeSignInGeneration()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, time.Now()); got != loginHealTokenRestart {
		t.Fatalf("heal = %v, want token-restart", got)
	}
	if agent.BackendAuth.Status == BackendAuthRevoked {
		t.Fatal("an accepted credential must not be marked revoked")
	}
}

func TestLoginPromptHealInconclusiveKeepsTokenRestart(t *testing.T) {
	m, agent, _ := revokedTestManager(t, claude.CredentialInconclusive)
	gen := m.claudeSignInGeneration()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, time.Now()); got != loginHealTokenRestart {
		t.Fatalf("heal = %v, want token-restart (pre-#18732 behaviour)", got)
	}
}

func TestLoginPromptHealNonClaudeBackendUntouched(t *testing.T) {
	m, agent, calls := revokedTestManager(t, claude.CredentialRejected)
	agent.BackendOverride = "copilot"
	gen := m.claudeSignInGeneration()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, time.Now()); got != loginHealTokenRestart {
		t.Fatalf("heal = %v, want token-restart for a non-Claude backend", got)
	}
	if *calls != 0 {
		t.Fatal("the Claude verifier must not run for other backends")
	}
}

// Acceptance 2: the operator's sign-in is a new signal, not another retry.
func TestOperatorSignInRestartsOnceAfterGivingUp(t *testing.T) {
	m, agent, _ := revokedTestManager(t, claude.CredentialRejected)
	now := time.Now()
	cooldown := time.Duration(tokenRestartCooldownSec) * time.Second

	// Exhaust the 24h window the old way, then confirm the heal gave up.
	for i := 0; i < tokenRestartWindowMax; i++ {
		if got := agent.decideTokenRestart(now); got != tokenRestartFire {
			t.Fatalf("attempt %d: got %v, want fire", i, got)
		}
		now = now.Add(cooldown)
	}
	if got := agent.decideTokenRestart(now); got != tokenRestartGiveUp {
		t.Fatalf("budget spent: got %v, want giveUp", got)
	}
	agent.tokenRestartGaveUp = true

	// Mark it revoked, as the live check would have.
	gen := m.claudeSignInGeneration()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, now); got != loginHealOperatorLogin {
		t.Fatalf("heal = %v, want operator-login", got)
	}

	// Dashboard sign-in.
	m.mu.Lock()
	m.noteClaudeOperatorSignInLocked(now)
	m.mu.Unlock()
	if agent.BackendAuth.Status == BackendAuthRevoked {
		t.Fatal("sign-in must lift the revoked mark")
	}

	// The agent is still at the prompt: restart once, window exhausted or not.
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, now); got != loginHealSignInRestart {
		t.Fatalf("after sign-in: heal = %v, want sign-in restart", got)
	}
	if n := agent.tokenRestartsInWindow(now); n != 1 || agent.tokenRestartGaveUp {
		t.Fatalf("after sign-in restart: ledger=%d gaveUp=%v, want a fresh ledger holding this one restart", n, agent.tokenRestartGaveUp)
	}

	// Once. The same sign-in never restarts the same agent twice, and with the
	// provider still rejecting (the fake never changes its mind) the next poll
	// routes back to the operator path rather than burning the fresh budget.
	m.mu.Lock()
	m.claudeLogin.verified = nil
	m.mu.Unlock()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, now.Add(3*time.Second)); got != loginHealOperatorLogin {
		t.Fatalf("second poll after sign-in: heal = %v, want operator-login (not a second sign-in restart)", got)
	}
	if n := agent.tokenRestartsInWindow(now); n != 1 {
		t.Fatalf("ledger = %d after the second poll, want 1", n)
	}
}

func TestOperatorSignInSignalExpiresAndIgnoresOlderGenerations(t *testing.T) {
	m, agent, _ := revokedTestManager(t, claude.CredentialAccepted)
	now := time.Now()
	m.mu.Lock()
	m.noteClaudeOperatorSignInLocked(now)
	m.mu.Unlock()

	// A poller started AFTER the sign-in begins at the current generation and
	// does not replay it.
	gen := m.claudeSignInGeneration()
	if got := m.decideLoginPromptHeal(context.Background(), agent, &gen, now); got != loginHealTokenRestart {
		t.Fatalf("poller born after sign-in: heal = %v, want token-restart", got)
	}

	// A poller that predates it does act on it, but only inside the window.
	stale := gen - 1
	late := now.Add(operatorSignInRestartWindow + time.Minute)
	if got := m.decideLoginPromptHeal(context.Background(), agent, &stale, late); got != loginHealTokenRestart {
		t.Fatalf("expired sign-in: heal = %v, want token-restart", got)
	}
	if got := m.decideLoginPromptHeal(context.Background(), agent, &stale, now.Add(time.Minute)); got != loginHealSignInRestart {
		t.Fatalf("fresh sign-in: heal = %v, want sign-in restart", got)
	}
	if stale != gen {
		t.Fatalf("poller must record the generation it consumed: got %d want %d", stale, gen)
	}
}
