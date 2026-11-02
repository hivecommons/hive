package dashboard

import (
	"encoding/json"
	"net/http/httptest"
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
		{name: "freshest case-variant session wins", profiles: profiles, seen: map[string]time.Time{
			"Karibhou": now.Add(-time.Hour),
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
	if !reflect.DeepEqual(got.OnlineLogins, []string{"fresh"}) {
		t.Fatalf("online logins = %v, want only fresh", got.OnlineLogins)
	}

	w := httptest.NewRecorder()
	s.handleContributorsList(w, httptest.NewRequest("GET", "/api/contributors", nil))
	var roster struct {
		Contributors []ContributorProfile  `json:"contributors"`
		Pool         ContributorPoolStatus `json:"contributorPool"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &roster); err != nil {
		t.Fatalf("decode roster: %v\n%s", err, w.Body.String())
	}
	var online int
	for _, profile := range roster.Contributors {
		if profile.Online {
			online++
		}
	}
	if online != 1 || roster.Pool.Online != online || roster.Pool.Registered != len(roster.Contributors) ||
		!reflect.DeepEqual(roster.Pool.OnlineLogins, []string{"fresh"}) || roster.Pool.PresenceWindow != got.PresenceWindow {
		t.Fatalf("roster and pool must share presence: %+v", roster)
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
	if string(payload["online_logins"]) != "[]" {
		t.Fatalf("empty pool must encode online_logins: [], got %s", data)
	}
}

func TestContributorPoolOnlineLogins(t *testing.T) {
	profiles := []ContributorProfile{
		{ContributorID: "relay-z", GitHubUsername: "Zed", Online: true},
		{ContributorID: "relay-a", GitHubUsername: " Amy ", Online: true},
		{ContributorID: "relay-a2", GitHubUsername: "AMY", Online: true},
		{ContributorID: "relay-offline", GitHubUsername: "offline"},
	}
	pool := (&Server{}).contributorPoolStatus(profiles, 15*time.Minute)
	if pool.Online != 3 || pool.Registered != 4 ||
		!reflect.DeepEqual(pool.OnlineIDs, []string{"relay-a", "relay-a2", "relay-z"}) ||
		!reflect.DeepEqual(pool.OnlineLogins, []string{"amy", "zed"}) {
		t.Fatalf("pool must count online rows but list each person once: %+v", pool)
	}
}

func TestContributorsListEmptyRoster(t *testing.T) {
	t.Setenv("HIVE_CONTRIBUTORS_DIR", t.TempDir())
	w := httptest.NewRecorder()
	(&Server{}).handleContributorsList(w, httptest.NewRequest("GET", "/api/contributors", nil))
	var body struct {
		Contributors json.RawMessage       `json:"contributors"`
		Pool         ContributorPoolStatus `json:"contributorPool"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if string(body.Contributors) != "[]" || body.Pool.Registered != 0 || body.Pool.Online != 0 ||
		body.Pool.OnlineLogins == nil {
		t.Fatalf("empty roster must serialize as an array with known zero counts: %s", w.Body.String())
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
