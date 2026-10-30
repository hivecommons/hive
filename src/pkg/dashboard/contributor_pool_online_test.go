package dashboard

import (
	"encoding/json"
	"reflect"
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
	if !reflect.DeepEqual(got.OnlineIDs, []string{"id-fresh"}) {
		t.Fatalf("online IDs = %v, want only id-fresh", got.OnlineIDs)
	}

	// A second session for the same contributor does not add another row.
	hub.connections["c3"] = &ContributorConnection{connID: "c3", profile: &ContributorProfile{ContributorID: "id-fresh", GitHubUsername: "fresh"}, lastPong: time.Now()}
	assertPool := func(want []string) {
		t.Helper()
		pool := s.BuildContributorPoolStatus()
		if !reflect.DeepEqual(pool.OnlineIDs, want) || pool.Online != len(want) || pool.Registered != 3 {
			t.Fatalf("online IDs/count = %v/%d, registered %d; want %v/%d, registered 3", pool.OnlineIDs, pool.Online, pool.Registered, want, len(want))
		}
	}
	assertPool([]string{"id-fresh"})
	delete(hub.connections, "c1")
	assertPool([]string{"id-fresh"})
	delete(hub.connections, "c3")
	assertPool([]string{})
	hub.connections["c2"].lastPong = time.Now()
	assertPool([]string{"id-stale"})

	// The zero-state is an explicit array, not unavailable/null.
	empty := (&Server{}).BuildContributorPoolStatus()
	data, err := json.Marshal(empty)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(data, &payload); err != nil || string(payload["online_ids"]) != "[]" {
		t.Fatalf("empty pool must encode online_ids: [], got %s (%v)", data, err)
	}
}

func TestOnlineContributorIDs(t *testing.T) {
	now := time.Now()
	profiles := []ContributorProfile{
		{ContributorID: "clanker-z", GitHubUsername: "Zed"},
		{ContributorID: "clanker-a", GitHubUsername: "Amy"},
		{ContributorID: "clanker-offline", GitHubUsername: "Offline"},
	}
	seen := map[string]time.Time{"zed": now, "amy": now.Add(-15 * time.Minute), "offline": now.Add(-16 * time.Minute), "unknown": now}
	if got, want := onlineContributorIDs(profiles, seen, 15*time.Minute, now), []string{"clanker-a", "clanker-z"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("online IDs = %v, want %v (not GitHub usernames or unknown/offline relays)", got, want)
	}
}
