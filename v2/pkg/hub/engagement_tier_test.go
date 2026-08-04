package hub

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// tierAgo is a real-clock RFC3339 stamp d before now, for tier-window math.
func tierAgo(d time.Duration) string {
	return time.Now().Add(-d).UTC().Format(time.RFC3339)
}

// TestUserStatusTier is the table-driven contract for the Users-table Status
// tier: every tier is reachable, precedence is blocked > never > live >
// active > idle > dormant, and records missing the new engagement fields are
// treated as HAVING NO DATA — never as engaged-now, never as engaged-at-zero.
func TestUserStatusTier(t *testing.T) {
	now := time.Now()
	cases := []struct {
		name       string
		u          SaaSUser
		liveNow    bool
		engagedNow bool
		want       string
	}{
		{
			name:    "blocked overrides everything",
			u:       SaaSUser{Blocked: true, LoginCount: 5, LastLogin: tierAgo(time.Hour), LastActionAt: tierAgo(time.Hour)},
			liveNow: true, engagedNow: true,
			want: tierBlocked,
		},
		{
			name: "never: zero logins ever",
			u:    SaaSUser{},
			want: tierNever,
		},
		{
			name:    "live: connected AND engaged right now",
			u:       SaaSUser{LoginCount: 1, LastLogin: tierAgo(time.Hour)},
			liveNow: true, engagedNow: true,
			want: tierLive,
		},
		{
			name:    "connected but NOT engaged is not live — open tab ranks idle",
			u:       SaaSUser{LoginCount: 1, LastLogin: tierAgo(10 * 24 * time.Hour)},
			liveNow: true, engagedNow: false,
			want: tierIdle,
		},
		{
			name: "active: real audited action within the 7-day window",
			u:    SaaSUser{LoginCount: 2, LastLogin: tierAgo(20 * 24 * time.Hour), LastActionAt: tierAgo(2 * 24 * time.Hour)},
			want: tierActive,
		},
		{
			name: "active: focus-engaged presence within the 7-day window",
			u:    SaaSUser{LoginCount: 2, LastLogin: tierAgo(20 * 24 * time.Hour), LastEngagedAt: tierAgo(3 * 24 * time.Hour)},
			want: tierActive,
		},
		{
			name: "idle: recent login but no real signal in 7 days (open-tab crowd)",
			u:    SaaSUser{LoginCount: 4, LastLogin: tierAgo(2 * 24 * time.Hour), SessionSeconds: 100000},
			want: tierIdle,
		},
		{
			name: "idle: stale real action still counts as seen within 30 days",
			u:    SaaSUser{LoginCount: 1, LastLogin: tierAgo(60 * 24 * time.Hour), LastActionAt: tierAgo(10 * 24 * time.Hour)},
			want: tierIdle,
		},
		{
			name: "dormant: nothing at all within 30 days",
			u:    SaaSUser{LoginCount: 3, LastLogin: tierAgo(45 * 24 * time.Hour), LastActionAt: tierAgo(40 * 24 * time.Hour)},
			want: tierDormant,
		},
		{
			name: "missing new fields are absent, not zero-now: legacy record with old login is dormant",
			u:    SaaSUser{LoginCount: 1, LastLogin: tierAgo(90 * 24 * time.Hour), SessionSeconds: 500000},
			want: tierDormant,
		},
		{
			name: "missing new fields never rank active: fresh login alone is idle",
			u:    SaaSUser{LoginCount: 1, LastLogin: tierAgo(time.Hour)},
			want: tierIdle,
		},
		{
			name: "unparsable timestamps are ignored, not treated as now",
			u:    SaaSUser{LoginCount: 1, LastLogin: tierAgo(45 * 24 * time.Hour), LastActionAt: "garbage", LastEngagedAt: "also-garbage"},
			want: tierDormant,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := userStatusTier(&tc.u, tc.liveNow, tc.engagedNow, now); got != tc.want {
				t.Errorf("userStatusTier() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestCreditEngagedTime verifies the engaged-time gate: session time accrues
// for every live session (back-compat), but engaged time and LastEngagedAt
// accrue ONLY for users the spoke reported as focus-engaged. An idle/hidden
// tab therefore grows session_seconds and never engaged_seconds.
func TestCreditEngagedTime(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	saveSaaSUser(&SaaSUser{GitHubUsername: "worker"})
	saveSaaSUser(&SaaSUser{GitHubUsername: "lurker"})

	prev := &RegistryEntry{LastHeartbeat: realAgo(120 * time.Second)}
	s.creditActiveSessionTime(prev, true, []string{"worker", "lurker"}, []string{"worker"})

	worker := loadSaaSUser("worker")
	lurker := loadSaaSUser("lurker")
	if worker.SessionSeconds < 110 || lurker.SessionSeconds < 110 {
		t.Fatalf("both live users must accrue session time: worker=%d lurker=%d", worker.SessionSeconds, lurker.SessionSeconds)
	}
	if worker.EngagedSeconds < 110 || worker.EngagedSeconds > 130 {
		t.Errorf("engaged user must accrue engaged time, got %d", worker.EngagedSeconds)
	}
	if worker.LastEngagedAt == "" {
		t.Error("engaged user must get LastEngagedAt stamped")
	}
	if lurker.EngagedSeconds != 0 || lurker.LastEngagedAt != "" {
		t.Errorf("idle open tab must NOT accrue engagement: engaged=%d lastEngagedAt=%q", lurker.EngagedSeconds, lurker.LastEngagedAt)
	}
}

// TestApplyUserLastActions verifies the hub-side fold of the spoke audit
// signal: timestamps land on the user record, only ever move FORWARD, and
// unknown users, bad names, and bad timestamps are skipped without error.
func TestApplyUserLastActions(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	saveSaaSUser(&SaaSUser{GitHubUsername: "alice", LastActionAt: tierAgo(time.Hour)})
	saveSaaSUser(&SaaSUser{GitHubUsername: "bob"})

	fresh := tierAgo(time.Minute)
	s.applyUserLastActions(map[string]string{
		"alice":         fresh,
		"bob":           "not-a-timestamp",
		"ghost":         fresh,
		"../etc/passwd": fresh,
	})
	if got := loadSaaSUser("alice").LastActionAt; got != fresh {
		t.Errorf("alice.LastActionAt = %q, want the newer %q", got, fresh)
	}
	if got := loadSaaSUser("bob").LastActionAt; got != "" {
		t.Errorf("a bad timestamp must be skipped, got %q", got)
	}
	if loadSaaSUser("ghost") != nil {
		t.Error("an unknown user must NOT be auto-created")
	}

	// An older report (a second hive with a stale audit log) never regresses.
	s.applyUserLastActions(map[string]string{"alice": tierAgo(48 * time.Hour)})
	if got := loadSaaSUser("alice").LastActionAt; got != fresh {
		t.Errorf("alice.LastActionAt regressed to %q, want %q kept", got, fresh)
	}

	// nil is the old-spoke case: a clean no-op.
	s.applyUserLastActions(nil)
}

// TestEngagedHiveUsernames covers the engaged-now presence set: marked from
// the heartbeat's engaged report, freshness-pruned on read, and queried
// per-user by isUserEngagedNow (the `live` tier gate).
func TestEngagedHiveUsernames(t *testing.T) {
	s := sessionTestHub()

	s.markUsersEngaged([]string{"alice", "../bad", ""})
	if got := s.engagedHiveUsernames(); len(got) != 1 || got[0] != "alice" {
		t.Fatalf("engaged = %v, want [alice] (valid names only)", got)
	}
	if !s.isUserEngagedNow("alice") {
		t.Error("alice must read engaged-now right after a report")
	}
	if s.isUserEngagedNow("bob") {
		t.Error("an unreported user must not read engaged-now")
	}

	// Stale entries drop out and are pruned.
	s.liveHiveUsersMu.Lock()
	s.engagedHiveUsers["alice"] = time.Now().Add(-2 * liveHiveUserStaleness)
	s.liveHiveUsersMu.Unlock()
	if got := s.engagedHiveUsernames(); len(got) != 0 {
		t.Errorf("stale engaged entry must be pruned, got %v", got)
	}
	if s.isUserEngagedNow("alice") {
		t.Error("a stale engaged report must not read engaged-now")
	}

	// nil report (old spoke) is a clean no-op.
	s.markUsersEngaged(nil)
}

// TestAdminUsersCarriesStatusTier verifies the admin Users payload includes
// the hub-computed status_tier for every user, computed from the record plus
// the hub's live/engaged view — including `live` for a user engaged right now.
func TestAdminUsersCarriesStatusTier(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	saveSaaSUser(&SaaSUser{GitHubUsername: "engaged-now", LoginCount: 1, LastLogin: tierAgo(time.Hour)})
	saveSaaSUser(&SaaSUser{GitHubUsername: "blocked-user", Blocked: true})
	saveSaaSUser(&SaaSUser{GitHubUsername: "never-logged-in"})
	s.markUsersLive([]string{"engaged-now"})
	s.markUsersEngaged([]string{"engaged-now"})

	rec := httptest.NewRecorder()
	s.handleAdminUsers(rec, httptest.NewRequest("GET", "/api/saas/admin/users", nil))
	var resp struct {
		Users []struct {
			GitHubUsername string `json:"github_username"`
			StatusTier     string `json:"status_tier"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad admin users payload: %v", err)
	}
	want := map[string]string{
		"engaged-now":     tierLive,
		"blocked-user":    tierBlocked,
		"never-logged-in": tierNever,
	}
	seen := 0
	for _, u := range resp.Users {
		expect, ok := want[u.GitHubUsername]
		if !ok {
			continue
		}
		seen++
		if u.StatusTier != expect {
			t.Errorf("%s: status_tier = %q, want %q", u.GitHubUsername, u.StatusTier, expect)
		}
	}
	if seen != len(want) {
		t.Errorf("payload covered %d of %d expected users", seen, len(want))
	}
	if strings.Contains(rec.Body.String(), "encrypted_token") {
		t.Error("admin users payload must never carry encrypted tokens")
	}
}
