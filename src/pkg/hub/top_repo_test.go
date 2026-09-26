package hub

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUserTopRepoUsesActivityAndTieBreak(t *testing.T) {
	u := &SaaSUser{
		GitHubUsername: "alice",
		Hives: map[string]string{
			"beta":  "member",
			"alpha": "member",
			"gamma": "member",
		},
	}
	hives := []RegistryEntry{
		{ID: "alpha", Org: "acme", PrimaryRepo: "alpha", Leaderboard: []LeaderboardEntry{{GitHubUsername: "alice", TasksCompleted: 1}}},
		{ID: "beta", Org: "acme", PrimaryRepo: "beta", Leaderboard: []LeaderboardEntry{{GitHubUsername: "alice", TasksCompleted: 3}}},
		{ID: "gamma", Org: "acme", PrimaryRepo: "gamma", Leaderboard: []LeaderboardEntry{{GitHubUsername: "alice", TasksCompleted: 3}}},
	}
	if got, want := userTopRepo(u, hives), "acme/beta"; got != want {
		t.Fatalf("userTopRepo() = %q, want %q", got, want)
	}
}

func TestUserTopRepoFallsBackToOrgAndQualifiedRepo(t *testing.T) {
	u := &SaaSUser{
		GitHubUsername: "alice",
		Hives: map[string]string{
			"org-only":   "member",
			"qualified":  "owner",
			"unassigned": "member",
		},
	}
	hives := []RegistryEntry{
		{ID: "org-only", Org: "acme"},
		{ID: "qualified", Org: "github.ibm.com", PrimaryRepo: "owner/repo"},
	}
	if got, want := userTopRepo(u, hives), "owner/repo"; got != want {
		t.Fatalf("userTopRepo() = %q, want %q", got, want)
	}
}

func TestAdminUsersCarriesTopRepo(t *testing.T) {
	s := sessionTestHub()
	useTempUserDir(t)
	saveSaaSUser(&SaaSUser{
		GitHubUsername: "alice",
		Hives: map[string]string{
			"h1": "member",
			"h2": "member",
		},
	})
	s.registry.Hives = []RegistryEntry{
		{ID: "h1", Org: "acme", PrimaryRepo: "quiet"},
		{ID: "h2", Org: "acme", PrimaryRepo: "busy", Leaderboard: []LeaderboardEntry{{GitHubUsername: "alice", TasksCompleted: 2}}},
	}

	rec := httptest.NewRecorder()
	s.handleAdminUsers(rec, httptest.NewRequest("GET", "/api/saas/admin/users", nil))
	var resp struct {
		Users []struct {
			GitHubUsername string `json:"github_username"`
			TopRepo        string `json:"top_repo"`
		} `json:"users"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("bad admin users payload: %v", err)
	}
	for _, u := range resp.Users {
		if u.GitHubUsername == "alice" {
			if u.TopRepo != "acme/busy" {
				t.Fatalf("top_repo = %q, want acme/busy", u.TopRepo)
			}
			return
		}
	}
	t.Fatal("alice missing from admin users payload")
}

func TestAdminUsersTableTopRepoColumn(t *testing.T) {
	html := dashScript(t)
	if !strings.Contains(html, `sortUsers(\'top_repo\')`) {
		t.Fatal("Top Repo header is not wired into the existing sortUsers mechanism")
	}
	if !strings.Contains(html, "'<td>' + topRepoCell(u) + '</td>'") {
		t.Fatal("admin users rows do not render topRepoCell")
	}
	if strings.Contains(html, `onclick="sortUsers(\'top_repo\')" style=`) {
		t.Fatal("Top Repo header added a new inline style attribute")
	}
}
