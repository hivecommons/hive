package dashboard

import (
	"encoding/json"
	"testing"
)

func seedTeamProfile(t *testing.T, p ContributorProfile) {
	t.Helper()
	if err := saveContributorProfile(&p); err != nil {
		t.Fatalf("save profile: %v", err)
	}
}

func TestTeamLeaderboardsGroupOptInProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	seedTeamProfile(t, ContributorProfile{
		GitHubUsername: "alice", ContributorID: "c-alice", TrustTier: "trusted", TasksCompleted: 8,
		Team: ptrContributorTeam(ContributorTeamMetadata{OSFamily: "linux", OSReleaseID: "bluefin", OSName: "Bluefin", OSVersionID: "40", OSIDLike: []string{"fedora"}, KernelRelease: "6.9.1", AgentBackend: "pi"}),
	})
	seedTeamProfile(t, ContributorProfile{
		GitHubUsername: "bob", ContributorID: "c-bob", TrustTier: "contributor", TasksCompleted: 4,
		Team: ptrContributorTeam(ContributorTeamMetadata{OSFamily: "darwin", AgentBackend: "copilot"}),
	})
	s := covK2ContribServer(t)

	resp := s.BuildTeamLeaderboards()
	if len(resp.ByDistro) == 0 || resp.ByDistro[0].Team != "Bluefin" {
		t.Fatalf("top distro = %#v, want Bluefin first", resp.ByDistro)
	}
	if len(resp.ByAgent) == 0 || resp.ByAgent[0].Team != "Team Pi" {
		t.Fatalf("top agent = %#v, want Team Pi first", resp.ByAgent)
	}
	if resp.Rarest == nil || resp.Rarest.Kernel != "6.9.1" {
		t.Fatalf("rarest setup = %#v, want kernel callout", resp.Rarest)
	}
}

func TestTeamMetadataSanitizesAndFallsBack(t *testing.T) {
	meta := sanitizeContributorTeam(ContributorTeamMetadata{
		OSFamily:      "linux",
		OSReleaseID:   "plan9\n<script>",
		KernelRelease: "  6.10-weird\tkernel  ",
		AgentBackend:  "unknown-cli",
	}, "")
	if meta.DistroTeam != teamUnknownDistro {
		t.Fatalf("distro team = %q, want %q", meta.DistroTeam, teamUnknownDistro)
	}
	if meta.AgentTeam != teamUnknownAgent {
		t.Fatalf("agent team = %q, want %q", meta.AgentTeam, teamUnknownAgent)
	}
	if meta.KernelRelease != "6.10-weird kernel" {
		t.Fatalf("kernel sanitized to %q", meta.KernelRelease)
	}
}

func TestTeamLeaderboardRouteIsPublicJSON(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	seedTeamProfile(t, ContributorProfile{
		GitHubUsername: "carol", ContributorID: "c-carol", TrustTier: "newcomer", TasksCompleted: 1,
		Team: ptrContributorTeam(ContributorTeamMetadata{OSFamily: "linux", OSReleaseID: "nixos", AgentBackend: "codex"}),
	})
	s := covK2ContribServer(t)
	rec := doGet(s, "/api/leaderboard/teams")
	if rec.Code != 200 {
		t.Fatalf("teams route status = %d", rec.Code)
	}
	var body TeamLeaderboardResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.ByDistro) == 0 || body.ByDistro[0].Team != "NixOS" {
		t.Fatalf("response distros = %#v", body.ByDistro)
	}
}

func TestLeaderboardOmitsTeamForNonOptInProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HIVE_CONTRIBUTORS_DIR", dir)
	seedTeamProfile(t, ContributorProfile{
		GitHubUsername: "dana", ContributorID: "c-dana", TrustTier: "contributor", TasksCompleted: 2,
	})
	s := covK2ContribServer(t)
	rec := doGet(s, "/api/leaderboard")
	var body struct {
		Leaderboard []map[string]any `json:"leaderboard"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode leaderboard: %v", err)
	}
	if len(body.Leaderboard) != 1 {
		t.Fatalf("leaderboard length = %d", len(body.Leaderboard))
	}
	if _, ok := body.Leaderboard[0]["team"]; ok {
		t.Fatalf("non-opt-in profile should not expose an empty team object: %#v", body.Leaderboard[0])
	}
	raw, err := json.Marshal(ContributorProfile{GitHubUsername: "nobody"})
	if err != nil {
		t.Fatalf("marshal profile: %v", err)
	}
	var profile map[string]any
	if err := json.Unmarshal(raw, &profile); err != nil {
		t.Fatalf("decode profile: %v", err)
	}
	if _, ok := profile["team"]; ok {
		t.Fatalf("non-opt-in profile should not serialize an empty team object: %s", raw)
	}
}
