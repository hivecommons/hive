package agent

import (
	"log/slog"
	"testing"
	"time"
)

// fakeMintIssuer is a minimal AgentMintIssuer for exercising the mint path
// without real key material or WIF exchange.
type fakeMintIssuer struct {
	enabled bool
	token   string
	err     error
}

func (f *fakeMintIssuer) Enabled() bool { return f.enabled }
func (f *fakeMintIssuer) MintAgentToken(agentName, tier string) (string, error) {
	return f.token, f.err
}

// TestIssueAgentMintToken_NoReentrantDeadlock is a regression test for the v4
// crash-loop: Start holds m.mu.Lock() and calls issueAgentMintToken, which used
// to re-acquire m.mu via a locked accessor. Go's sync.RWMutex is NOT reentrant,
// so that self-locked the goroutine forever — wedging every Manager operation
// (SendKick, AllStatuses/heartbeat, ResolveAgent) behind the held write lock,
// stalling the heartbeat, failing liveness, and crash-looping the pod.
//
// The test reproduces the exact condition (mint issuer attached, m.mu.Lock held)
// and asserts the call returns promptly instead of deadlocking.
func TestIssueAgentMintToken_NoReentrantDeadlock(t *testing.T) {
	m := &Manager{
		agents:    make(map[string]*AgentProcess),
		idToName:  make(map[string]string),
		logger:    slog.Default(),
		agentMint: &fakeMintIssuer{enabled: false}, // disabled: returns before any I/O
	}

	done := make(chan struct{})
	go func() {
		// Hold the write lock exactly as Start does, then call the mint path.
		m.mu.Lock()
		defer m.mu.Unlock()
		m.issueAgentMintToken("scanner", "contributor", 0)
		close(done)
	}()

	select {
	case <-done:
		// Returned while holding the write lock — no re-entrant deadlock.
	case <-time.After(3 * time.Second):
		t.Fatal("issueAgentMintToken deadlocked while m.mu.Lock() was held " +
			"(re-entrant RWMutex acquisition regressed)")
	}
}

// TestIssueAgentMintToken_EnabledUnderLock proves the enabled path (which calls
// MintAgentToken) is also lock-safe while m.mu is held. Uses uid 0 so no file is
// written (writeAgentCredFile chowns only for uid>0; here it writes to the cache
// dir, so point it at a temp HOME via t.Setenv is unnecessary — Enabled+empty
// token short-circuits before the write).
func TestIssueAgentMintToken_EnabledUnderLock(t *testing.T) {
	m := &Manager{
		agents:   make(map[string]*AgentProcess),
		idToName: make(map[string]string),
		logger:   slog.Default(),
		// Enabled but returns an empty token → returns before writeAgentCredFile,
		// so no filesystem side effect, while still exercising the locked accessor
		// + MintAgentToken call under the held lock.
		agentMint: &fakeMintIssuer{enabled: true, token: ""},
	}

	done := make(chan struct{})
	go func() {
		m.mu.Lock()
		defer m.mu.Unlock()
		m.issueAgentMintToken("scanner", "contributor", 0)
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("issueAgentMintToken (enabled) deadlocked under m.mu.Lock()")
	}
}
