package spoke

import "testing"

func fullPayload() *HeartbeatPayload {
	return &HeartbeatPayload{
		HiveID: "h", Version: "v1", GitHash: "abc", ClusterID: "c",
		Repos: []string{"o/r"}, PrimaryRepo: "o/r",
		RepoActivity:        []RepoActivityWire{{Repo: "o/r"}},
		ActiveSessionUsers:  []string{"alice"},
		EngagedSessionUsers: []string{"alice"},
		UserLastActions:     map[string]string{"alice": "t"},
		Owner:               "alice", AIAuthor: "bot",
		DashboardURL: "https://d", SnapshotURL: "https://s",
		Leaderboard: []LeaderboardEntry{{GitHubUsername: "bob", CurrentTask: "secret title"}},
	}
}

func TestRedactHeartbeatNoopByDefault(t *testing.T) {
	SetHeartbeatOmit(nil)
	p := fullPayload()
	if redactHeartbeat(p) != p {
		t.Fatal("expected same pointer when nothing omitted")
	}
}

func TestRedactHeartbeatClasses(t *testing.T) {
	t.Cleanup(func() { SetHeartbeatOmit(nil) })
	cases := []struct {
		class string
		check func(*HeartbeatPayload) bool
	}{
		{"repos", func(p *HeartbeatPayload) bool {
			return len(p.Repos) == 0 && p.Repos != nil && p.PrimaryRepo == "" && p.RepoActivity == nil && p.Owner == "alice"
		}},
		{"users", func(p *HeartbeatPayload) bool {
			return p.Owner == "" && p.AIAuthor == "" && p.ActiveSessionUsers == nil && p.EngagedSessionUsers == nil &&
				p.UserLastActions == nil && len(p.Leaderboard) == 0 && p.PrimaryRepo == "o/r"
		}},
		{"task_titles", func(p *HeartbeatPayload) bool {
			return len(p.Leaderboard) == 1 && p.Leaderboard[0].CurrentTask == "" && p.Leaderboard[0].GitHubUsername == "bob"
		}},
		{"dashboard_urls", func(p *HeartbeatPayload) bool {
			return p.DashboardURL == "" && p.SnapshotURL == "" && p.Owner == "alice"
		}},
	}
	for _, tc := range cases {
		SetHeartbeatOmit([]string{tc.class})
		orig := fullPayload()
		got := redactHeartbeat(orig)
		if !tc.check(got) {
			t.Errorf("class %s: unexpected result %+v", tc.class, got)
		}
		if got.HiveID != "h" || got.Version != "v1" || got.GitHash != "abc" || got.ClusterID != "c" {
			t.Errorf("class %s: upgrade-delivery fields altered", tc.class)
		}
		if orig.Leaderboard[0].CurrentTask != "secret title" || orig.DashboardURL != "https://d" {
			t.Errorf("class %s: input mutated", tc.class)
		}
	}
}

func TestRedactTaskStatus(t *testing.T) {
	t.Cleanup(func() { SetHeartbeatOmit(nil) })
	p := &TaskStatusPayload{HiveID: "h", Leaderboard: []LeaderboardEntry{{GitHubUsername: "bob", CurrentTask: "t"}}}
	SetHeartbeatOmit([]string{"task_titles"})
	if got := redactTaskStatus(p); got.Leaderboard[0].CurrentTask != "" || p.Leaderboard[0].CurrentTask != "t" {
		t.Fatal("task title not redacted on a copy")
	}
	SetHeartbeatOmit([]string{"users"})
	if got := redactTaskStatus(p); len(got.Leaderboard) != 0 {
		t.Fatal("users not redacted")
	}
}
