package hub

import "testing"

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
	}}

	t.Run("admin viewer gets name, slack, AND notes", func(t *testing.T) {
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
	})

	t.Run("non-admin owner gets name+slack but NEVER notes", func(t *testing.T) {
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
	})
}
