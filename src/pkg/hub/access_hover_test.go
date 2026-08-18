package hub

import (
	"strings"
	"testing"
)

// TestAccessForHive covers the ordering and filtering the My Hives hover relies
// on: only users granted this hive, owners first, then alphabetical.
func TestAccessForHive(t *testing.T) {
	users := []SaaSUser{
		{GitHubUsername: "zoe", Hives: map[string]string{"h1": "read"}},
		{GitHubUsername: "adam", Hives: map[string]string{"h1": "read-write"}},
		{GitHubUsername: "owner1", Hives: map[string]string{"h1": "owner"}},
		{GitHubUsername: "elsewhere", Hives: map[string]string{"h2": "owner"}},
		{GitHubUsername: "nohives"},
	}

	// includeNotes is irrelevant here (no user carries notes); false keeps the
	// entries at their zero contact fields so the equality check below is exact.
	got := accessForHive("h1", users, false)
	want := []HiveAccessEntry{
		{Username: "owner1", Role: "owner"},
		{Username: "adam", Role: "read-write"},
		{Username: "zoe", Role: "read"},
	}
	if len(got) != len(want) {
		t.Fatalf("accessForHive returned %d entries, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("entry %d = %+v, want %+v", i, got[i], want[i])
		}
	}

	// A hive nobody is granted must yield an empty (non-nil) slice, so the
	// frontend's `h.access || []` guard and the omitempty tag agree.
	if empty := accessForHive("nosuchhive", users, false); len(empty) != 0 {
		t.Errorf("unknown hive -> %+v, want empty", empty)
	}
}

// TestAccessForHiveContactMetadata covers the avatar-hover enrichment: FullName
// and SlackID always ride so a co-owner can see WHO someone is, but the
// admin-maintained Notes field is delivered ONLY when includeNotes is true (i.e.
// the viewer is a hub admin), so an owner never sees the admin's private notes.
func TestAccessForHiveContactMetadata(t *testing.T) {
	users := []SaaSUser{{
		GitHubUsername: "ja8zyjits",
		Hives:          map[string]string{"h1": "owner"},
		FullName:       "Jane Doe",
		SlackID:        "@jane",
		Notes:          "GPU quota bump; prefers async",
		LoginCount:     42,
		SessionSeconds: 7200,
	}}

	t.Run("admin viewer gets name, slack, notes AND engagement stats", func(t *testing.T) {
		got := accessForHive("h1", users, true)
		if len(got) != 1 {
			t.Fatalf("want 1 entry, got %d", len(got))
		}
		e := got[0]
		if e.FullName != "Jane Doe" || e.SlackID != "@jane" {
			t.Errorf("name/slack = (%q, %q), want (Jane Doe, @jane)", e.FullName, e.SlackID)
		}
		if e.Notes != "GPU quota bump; prefers async" {
			t.Errorf("admin must see notes, got %q", e.Notes)
		}
		if e.LoginCount != 42 || e.SessionSeconds != 7200 {
			t.Errorf("admin must see stats, got login=%d session=%d", e.LoginCount, e.SessionSeconds)
		}
	})

	t.Run("non-admin owner gets name+slack but NEVER notes or stats", func(t *testing.T) {
		got := accessForHive("h1", users, false)
		if len(got) != 1 {
			t.Fatalf("want 1 entry, got %d", len(got))
		}
		e := got[0]
		if e.FullName != "Jane Doe" || e.SlackID != "@jane" {
			t.Errorf("owner should still see name/slack, got (%q, %q)", e.FullName, e.SlackID)
		}
		if e.Notes != "" {
			t.Errorf("owner must NOT see admin notes, but got %q", e.Notes)
		}
		if e.LoginCount != 0 || e.SessionSeconds != 0 {
			t.Errorf("owner must NOT see engagement stats, got login=%d session=%d", e.LoginCount, e.SessionSeconds)
		}
	})
}

// TestAccessAvatarTitleCarriesStats pins that the co-member face's NATIVE tooltip
// (accessAvatarTitle) carries the engagement info — logins, time, task activity,
// verdict — reusing the admin-card helpers, and stays a native title (no custom
// panel; TestInlineAvatarsCarryNoCustomPanel remains the panel-invariant guard).
func TestAccessAvatarTitleCarriesStats(t *testing.T) {
	for _, snippet := range []string{
		"if (a.login_count) lines.push('Logins: ' + a.login_count);",
		"if (a.session_seconds) lines.push('Time in hive: ' + fmtHours(a.session_seconds));",
		"if (a.engaged_seconds) lines.push('Engaged time: ' + fmtHours(a.engaged_seconds));",
		"if (a.last_action_at) lines.push('Last real action: ' + fmtUserTS(a.last_action_at));",
		"var act = userTaskActivity(uname);",
		"userVerdict({login_count: a.login_count || 0, session_seconds: a.session_seconds || 0, engaged_seconds: a.engaged_seconds || 0, last_action_at: a.last_action_at || ''}, act, [])",
		// The "logged in now" line rides in the avatar's own tooltip, not the ring
		// wrapper, so a live user's identity+stats aren't shadowed.
		"if (isUserLive(uname)) title = title + '\\n● Logged into their hive now';",
	} {
		if !strings.Contains(dashboardHTML, snippet) {
			t.Errorf("dashboardHTML missing %q", snippet)
		}
	}
	// The green ring wrapper must NOT carry its own title (it would shadow the
	// enriched tooltip).
	if strings.Contains(dashboardHTML, `'<span title="Logged into their hive now" '`) {
		t.Error("the live-ring wrapper still hard-codes a title that shadows the avatar tooltip")
	}
}
