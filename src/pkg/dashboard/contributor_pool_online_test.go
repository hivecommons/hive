package dashboard

import (
	"testing"
	"time"
)

// TestCountOnlineContributors pins the sidebar's "online/total" numerator
// to the tracker's presence predicate: a heartbeat within the presence
// window (inclusive edge) counts, anything older, missing or for an
// unregistered account does not.
func TestCountOnlineContributors(t *testing.T) {
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	window := 15 * time.Minute
	profiles := []ContributorProfile{
		{ContributorID: "a", GitHubUsername: "Karibhou"},
		{ContributorID: "b", GitHubUsername: "hanthor"},
		{ContributorID: "c", GitHubUsername: "quiet"},
	}
	cases := []struct {
		name     string
		profiles []ContributorProfile
		seen     map[string]time.Time
		want     int
	}{
		{name: "zero contributors", seen: map[string]time.Time{"Karibhou": now}, want: 0},
		{name: "no heartbeats", profiles: profiles, want: 0},
		{name: "all offline", profiles: profiles, seen: map[string]time.Time{
			"Karibhou": now.Add(-window - time.Second),
			"hanthor":  now.Add(-2 * time.Hour),
		}, want: 0},
		{name: "edge of window is online", profiles: profiles, seen: map[string]time.Time{
			"Karibhou": now.Add(-window),
		}, want: 1},
		{name: "case-insensitive login match", profiles: profiles, seen: map[string]time.Time{
			"karibhou": now.Add(-time.Minute),
		}, want: 1},
		{name: "mixed", profiles: profiles, seen: map[string]time.Time{
			"Karibhou": now.Add(-time.Minute),
			"hanthor":  now.Add(-13 * time.Minute),
			"quiet":    now.Add(-16 * time.Minute),
			"stranger": now,
		}, want: 2},
		{name: "zero heartbeat is offline", profiles: profiles, seen: map[string]time.Time{"Karibhou": {}}, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := countOnlineContributors(tc.profiles, tc.seen, window, now); got != tc.want {
				t.Fatalf("countOnlineContributors = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestBuildContributorPoolStatusOnline runs the aggregation through the
// live hub: a registered contributor whose socket's last pong is within
// the window is online; a stale socket is active-but-offline.
func TestBuildContributorPoolStatusOnline(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	for _, u := range []string{"fresh", "stale", "never"} {
		if err := saveContributorProfile(&ContributorProfile{ContributorID: "id-" + u, GitHubUsername: u, TrustTier: contributorTierContributor}); err != nil {
			t.Fatal(err)
		}
	}
	hub := &ContributeWSHub{connections: map[string]*ContributorConnection{}}
	hub.connections["c1"] = &ContributorConnection{connID: "c1", profile: &ContributorProfile{ContributorID: "id-fresh", GitHubUsername: "fresh"}, lastPong: time.Now().Add(-time.Minute)}
	hub.connections["c2"] = &ContributorConnection{connID: "c2", profile: &ContributorProfile{ContributorID: "id-stale", GitHubUsername: "stale"}, lastPong: time.Now().Add(-time.Hour)}
	s := &Server{contributeHub: hub}
	got := s.BuildContributorPoolStatus()
	if got.Online != 1 || got.Registered != 3 || got.Active != 2 {
		t.Fatalf("pool = online %d / active %d / registered %d, want 1 / 2 / 3", got.Online, got.Active, got.Registered)
	}
	if got.PresenceWindow != "15m" {
		t.Fatalf("presence_window = %q, want 15m", got.PresenceWindow)
	}
}
