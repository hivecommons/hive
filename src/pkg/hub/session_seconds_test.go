package hub

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// useTempUserDir points the on-disk SaaS user store at a temp dir for one test.
func useTempUserDir(t *testing.T) {
	t.Helper()
	orig := saasUsersDir
	saasUsersDir = filepath.Join(t.TempDir(), "users")
	if err := os.MkdirAll(saasUsersDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { saasUsersDir = orig })
}

func sessionTestHub() *HubServer {
	return &HubServer{logger: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// realAgo is a REAL-clock "d ago" RFC3339 stamp. (The package's rfc3339Ago uses a
// fixed test clock advNow, which would make time.Since() in creditActiveSessionTime
// see a huge elapsed — so this test needs the real wall clock.)
func realAgo(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// TestCreditActiveSessionTime is the per-user "time in hive" accumulation: each
// active user is credited the inter-beat interval, clamped so an outage never
// back-credits, keyed off the previous beat so restarts don't double-count.
func TestCreditActiveSessionTime(t *testing.T) {
	s := sessionTestHub()

	t.Run("credits ~one interval for an active user", func(t *testing.T) {
		useTempUserDir(t)
		if err := saveSaaSUser(&SaaSUser{GitHubUsername: "alice"}); err != nil {
			t.Fatal(err)
		}
		prev := &RegistryEntry{LastHeartbeat: realAgo(120 * time.Second)}
		s.creditActiveSessionTime(prev, true, []string{"alice"}, nil)
		got := loadSaaSUser("alice").SessionSeconds
		if got < 110 || got > 130 {
			t.Errorf("SessionSeconds = %d, want ~120", got)
		}
	})

	t.Run("clamps a giant gap after an outage", func(t *testing.T) {
		useTempUserDir(t)
		saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})
		prev := &RegistryEntry{LastHeartbeat: realAgo(3 * time.Hour)}
		s.creditActiveSessionTime(prev, true, []string{"alice"}, nil)
		got := loadSaaSUser("alice").SessionSeconds
		if got != int64(maxSessionCreditSecs) {
			t.Errorf("SessionSeconds = %d, want clamp %d (not the full 3h)", got, int64(maxSessionCreditSecs))
		}
	})

	t.Run("two active users both credited; unknown user skipped, not created", func(t *testing.T) {
		useTempUserDir(t)
		saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})
		saveSaaSUser(&SaaSUser{GitHubUsername: "bob"})
		prev := &RegistryEntry{LastHeartbeat: realAgo(120 * time.Second)}
		s.creditActiveSessionTime(prev, true, []string{"alice", "bob", "ghost"}, nil)
		if loadSaaSUser("alice").SessionSeconds < 110 || loadSaaSUser("bob").SessionSeconds < 110 {
			t.Error("both known users must be credited")
		}
		if loadSaaSUser("ghost") != nil {
			t.Error("an unknown active user must NOT be auto-created")
		}
	})

	t.Run("first-ever beat credits nothing", func(t *testing.T) {
		useTempUserDir(t)
		saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})
		s.creditActiveSessionTime(nil, false, []string{"alice"}, nil)
		if got := loadSaaSUser("alice").SessionSeconds; got != 0 {
			t.Errorf("no prior beat must credit 0, got %d", got)
		}
	})

	t.Run("a beat at the same instant as the last credits ~0 (restart-safety)", func(t *testing.T) {
		useTempUserDir(t)
		saveSaaSUser(&SaaSUser{GitHubUsername: "alice"})
		prev := &RegistryEntry{LastHeartbeat: time.Now().UTC().Format(time.RFC3339)}
		s.creditActiveSessionTime(prev, true, []string{"alice"}, nil)
		if got := loadSaaSUser("alice").SessionSeconds; got > 1 {
			t.Errorf("elapsed ~0 must credit ~0, got %d", got)
		}
	})

	t.Run("invalid username is ignored", func(t *testing.T) {
		useTempUserDir(t)
		prev := &RegistryEntry{LastHeartbeat: realAgo(120 * time.Second)}
		// Must not panic or create anything for a bad name.
		s.creditActiveSessionTime(prev, true, []string{"../etc/passwd", ""}, nil)
	})
}

// TestLiveHiveUsernames covers the "logged in right now" set that drives the
// green-dashed avatar border: marked on report, pruned by staleness on read.
func TestLiveHiveUsernames(t *testing.T) {
	s := sessionTestHub()

	// Marking goes through creditActiveSessionTime's markUsersLive call as well as
	// directly; exercise markUsersLive here.
	s.markUsersLive([]string{"alice", "bob", "../bad", ""})
	live := s.liveHiveUsernames()
	if len(live) != 2 || live[0] != "alice" || live[1] != "bob" {
		t.Fatalf("live = %v, want [alice bob] (sorted, valid only)", live)
	}

	// A stale entry is pruned on read.
	s.liveHiveUsersMu.Lock()
	s.liveHiveUsers["carol"] = time.Now().Add(-2 * liveHiveUserStaleness)
	s.liveHiveUsersMu.Unlock()
	live = s.liveHiveUsernames()
	for _, u := range live {
		if u == "carol" {
			t.Error("stale live entry must be pruned")
		}
	}
}
