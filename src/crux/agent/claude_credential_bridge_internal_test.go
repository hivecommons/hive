package agent

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/hivecommons/hive/crux/claude"
	"github.com/hivecommons/hive/crux/config"
)

// TestRepairClaudeCredentialBridge_PromotionKeepsSharedOwner: the promoted
// shared credential is written by the hive process, so without a chown it would
// belong to whoever runs hive. It must keep the owner and group of the file it
// replaces. The chown is a seam so this runs without root or real agent uids.
func TestRepairClaudeCredentialBridge_PromotionKeepsSharedOwner(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	now := time.Now()
	link, target := brokenBridgeFixture(t, shared, "coder",
		fakeClaudeCredential("fake-old-access", now.Add(-time.Hour), "fake-old-refresh"),
		fakeClaudeCredential("fake-new-access", now.Add(8*time.Hour), "fake-new-refresh"))
	info, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	st := info.Sys().(*syscall.Stat_t)

	type chownCall struct {
		path     string
		uid, gid int
	}
	var calls []chownCall
	orig := chownClaudeCredential
	chownClaudeCredential = func(path string, uid, gid int) error {
		calls = append(calls, chownCall{path, uid, gid})
		return nil
	}
	t.Cleanup(func() { chownClaudeCredential = orig })

	rep := m.rebridgeClaudeCredential("coder", link, target, claudeBridgeTriggerWatchdog)
	if rep == nil || rep.kept != "agent" {
		t.Fatalf("repair = %+v, want kept=agent", rep)
	}
	if len(calls) != 1 || calls[0].uid != int(st.Uid) || calls[0].gid != int(st.Gid) {
		t.Fatalf("chown calls = %+v, want one call with the previous shared owner %d:%d", calls, st.Uid, st.Gid)
	}
	if filepath.Dir(calls[0].path) != filepath.Dir(target) {
		t.Errorf("chown applied to %q, want the replacement written beside %q", calls[0].path, target)
	}
}

// TestRepairClaudeCredentialBridge_StaleNamesNeverCollide: two repairs in the
// same second must not overwrite the first moved-aside file.
func TestRepairClaudeCredentialBridge_StaleNamesNeverCollide(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	fixed := time.Date(2026, 11, 7, 14, 54, 0, 0, time.UTC)
	orig := claudeBridgeNow
	claudeBridgeNow = func() time.Time { return fixed }
	t.Cleanup(func() { claudeBridgeNow = orig })

	sharedJSON := fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh")
	link, target := brokenBridgeFixture(t, shared, "tester", sharedJSON,
		fakeClaudeCredential("fake-first-access", time.Now().Add(time.Hour), "fake-first-refresh"))
	m.rebridgeClaudeCredential("tester", link, target, claudeBridgeTriggerWatchdog)

	// The CLI refreshes again within the same second.
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(link, []byte(fakeClaudeCredential("fake-second-access", time.Now().Add(time.Hour), "fake-second-refresh")), 0o600); err != nil {
		t.Fatal(err)
	}
	m.rebridgeClaudeCredential("tester", link, target, claudeBridgeTriggerWatchdog)

	if got := staleFiles(t, filepath.Dir(link)); len(got) != 2 {
		t.Fatalf("want two distinct moved-aside files, got %v", got)
	}
	assertBridgeRestored(t, link, target)
}

// TestAgentSnapshot_ReportsCredentialBridgeBroken: the status the dashboard
// reads names the broken bridge, and stops naming it once it is repaired.
func TestAgentSnapshot_ReportsCredentialBridgeBroken(t *testing.T) {
	shared := withSharedAgentHome(t)
	m, _ := testManagerWithSink(t)
	ap := &AgentProcess{Name: "architect", UID: 2001, Config: config.AgentConfig{Backend: "claude"}}
	m.mu.Lock()
	m.agents["architect"] = ap
	m.mu.Unlock()
	link, _ := brokenBridgeFixture(t, shared, "architect",
		fakeClaudeCredential("fake-shared-access", time.Now().Add(8*time.Hour), "fake-shared-refresh"),
		fakeClaudeCredential("fake-private-access", time.Now().Add(time.Hour), "fake-private-refresh"))

	if snap := ap.snapshot(); !snap.CredentialBridgeBroken {
		t.Fatal("snapshot does not report the private file that replaced the credential link")
	}
	if got := m.RepairClaudeCredentialBridges(claudeBridgeTriggerWatchdog); len(got) != 1 || got[0] != "architect" {
		t.Fatalf("RepairClaudeCredentialBridges = %v, want [architect]", got)
	}
	if snap := ap.snapshot(); snap.CredentialBridgeBroken {
		t.Errorf("snapshot still reports a broken bridge after the repair (link %s)", link)
	}

	// Non-claude agents never report it, even with a stray regular file.
	other := &AgentProcess{Name: "copilot-one", UID: 2006, Config: config.AgentConfig{Backend: "copilot"}}
	brokenBridgeFixture(t, shared, "copilot-one", "", fakeClaudeCredential("fake-x", time.Now(), ""))
	if other.snapshot().CredentialBridgeBroken {
		t.Error("a copilot agent must not report a Claude credential bridge")
	}
}

// TestPreferAgentClaudeCredential pins the "newer AND usable" rule.
func TestPreferAgentClaudeCredential(t *testing.T) {
	now := time.Now()
	tok := func(exp time.Duration, refresh string) *claude.OAuthTokens {
		return &claude.OAuthTokens{AccessToken: "fake", RefreshToken: refresh, ExpiresAt: now.Add(exp).UnixMilli()}
	}
	cases := []struct {
		name          string
		agent, shared *claude.OAuthTokens
		want          bool
	}{
		{"agent newer and live", tok(8*time.Hour, "r"), tok(time.Hour, "r"), true},
		{"shared newer", tok(time.Hour, "r"), tok(8*time.Hour, "r"), false},
		{"tie keeps shared", tok(time.Hour, "r"), tok(time.Hour, "r"), false},
		{"agent expired but refreshable, shared absent", tok(-time.Hour, "r"), nil, true},
		{"agent unusable, shared absent", tok(-time.Hour, ""), nil, false},
		{"agent usable, shared unusable", tok(-time.Minute, "r"), tok(-2*time.Hour, ""), true},
		{"agent absent", nil, tok(time.Hour, "r"), false},
	}
	for _, c := range cases {
		if got := preferAgentClaudeCredential(c.agent, c.shared, now); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
