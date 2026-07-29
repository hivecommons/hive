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

	got := accessForHive("h1", users)
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
	if empty := accessForHive("nosuchhive", users); len(empty) != 0 {
		t.Errorf("unknown hive -> %+v, want empty", empty)
	}
}
