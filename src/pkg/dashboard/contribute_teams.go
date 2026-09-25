package dashboard

import (
	"encoding/json"
	"net/http"
	"os"
	"sort"
	"strings"
)

const (
	teamUnknownDistro = "Wildcard"
	teamWeirdKernel   = "Weird Kernel"
	teamUnknownAgent  = "Local Model"
	teamLinux         = "Linux"
	teamMacOS         = "macOS"
	teamWindows       = "Windows"

	teamCatalogEnv = "HIVE_TEAM_CATALOG_JSON"
)

// ContributorTeamMetadata is an opt-in, privacy-bounded contributor runtime
// declaration. It intentionally excludes hostname, username, IP, and device IDs.
type ContributorTeamMetadata struct {
	OSFamily      string   `json:"os_family,omitempty"`
	OSReleaseID   string   `json:"os_release_id,omitempty"`
	OSName        string   `json:"os_name,omitempty"`
	OSVersionID   string   `json:"os_version_id,omitempty"`
	OSIDLike      []string `json:"os_id_like,omitempty"`
	KernelRelease string   `json:"kernel_release,omitempty"`
	AgentBackend  string   `json:"agent_backend,omitempty"`
	DistroTeam    string   `json:"distro_team,omitempty"`
	OSFamilyTeam  string   `json:"os_family_team,omitempty"`
	AgentTeam     string   `json:"agent_team,omitempty"`
	SetupTeam     string   `json:"setup_team,omitempty"`
}

type teamCatalog struct {
	Distros map[string]string `json:"distros"`
	Agents  map[string]string `json:"agents"`
	OS      map[string]string `json:"os"`
}

type TeamLeaderboardResponse struct {
	ByDistro  []TeamLeaderboardEntry `json:"by_distro"`
	ByOS      []TeamLeaderboardEntry `json:"by_os_family"`
	ByAgent   []TeamLeaderboardEntry `json:"by_agent"`
	Rarest    *TeamRarestCallout     `json:"rarest_setup,omitempty"`
	Catalog   teamCatalog            `json:"catalog"`
	Generated string                 `json:"generated_from"`
}

type TeamLeaderboardEntry struct {
	Team            string   `json:"team"`
	Kind            string   `json:"kind"`
	Rank            int      `json:"rank"`
	Members         int      `json:"members"`
	TasksCompleted  int      `json:"tasks_completed"`
	TasksFailed     int      `json:"tasks_failed"`
	Findings        int      `json:"findings,omitempty"`
	TopContributors []string `json:"top_contributors,omitempty"`
}

type TeamRarestCallout struct {
	Team        string `json:"team"`
	OSFamily    string `json:"os_family,omitempty"`
	Distro      string `json:"distro,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Kernel      string `json:"kernel_release,omitempty"`
	Member      string `json:"member,omitempty"`
	Description string `json:"description"`
}

func defaultTeamCatalog() teamCatalog {
	return teamCatalog{
		Distros: map[string]string{
			"alpine": "Alpine", "arch": "Arch", "archlinux": "Arch",
			"aurora": "Aurora", "bazzite": "Bazzite", "bluefin": "Bluefin",
			"debian": "Debian", "fedora": "Fedora", "gentoo": "Gentoo",
			"nixos": "NixOS", "opensuse": "openSUSE", "opensuse-leap": "openSUSE",
			"opensuse-tumbleweed": "openSUSE", "ubuntu": "Ubuntu",
		},
		Agents: map[string]string{
			"aider": "Aider", "agy": "Agy", "claude": "Claude", "codex": "Codex",
			"copilot": "Copilot", "gemini": "Gemini", "goose": "Goose",
			"kilo": "Kilo", "litellm": "LiteLLM", "muse": "Muse", "omp": "OMP",
			"opencode": "OpenCode", "pi": "Team Pi",
		},
		OS: map[string]string{"darwin": teamMacOS, "linux": teamLinux, "macos": teamMacOS, "windows": teamWindows, "win32": teamWindows},
	}
}

func configuredTeamCatalog() teamCatalog {
	c := defaultTeamCatalog()
	raw := strings.TrimSpace(os.Getenv(teamCatalogEnv))
	if raw == "" {
		return c
	}
	var override teamCatalog
	if err := json.Unmarshal([]byte(raw), &override); err != nil {
		return c
	}
	mergeTeamMap(c.Distros, override.Distros)
	mergeTeamMap(c.Agents, override.Agents)
	mergeTeamMap(c.OS, override.OS)
	return c
}

func mergeTeamMap(dst, src map[string]string) {
	for k, v := range src {
		key := strings.ToLower(strings.TrimSpace(k))
		val := sanitizeTeamField(v)
		if key != "" && val != "" {
			dst[key] = val
		}
	}
}

func sanitizeContributorTeam(in ContributorTeamMetadata, fallbackBackend string) ContributorTeamMetadata {
	out := ContributorTeamMetadata{
		OSFamily:      normalizeOSFamily(in.OSFamily),
		OSReleaseID:   sanitizeTeamToken(in.OSReleaseID),
		OSName:        sanitizeTeamField(in.OSName),
		OSVersionID:   sanitizeTeamToken(in.OSVersionID),
		KernelRelease: sanitizeTeamField(in.KernelRelease),
		AgentBackend:  sanitizeTeamToken(firstNonEmptyTeam(in.AgentBackend, fallbackBackend)),
	}
	for _, raw := range in.OSIDLike {
		if tok := sanitizeTeamToken(raw); tok != "" {
			out.OSIDLike = append(out.OSIDLike, tok)
		}
	}
	return hydrateContributorTeam(out, fallbackBackend)
}

func hydrateContributorTeam(in ContributorTeamMetadata, fallbackBackend string) ContributorTeamMetadata {
	out := sanitizeContributorTeamNoHydrate(in, fallbackBackend)
	catalog := configuredTeamCatalog()
	out.OSFamilyTeam = catalog.OS[strings.ToLower(out.OSFamily)]
	if out.OSFamilyTeam == "" && out.OSFamily != "" {
		out.OSFamilyTeam = sanitizeTeamField(out.OSFamily)
	}
	out.DistroTeam = distroTeam(out, catalog)
	out.AgentTeam = catalog.Agents[strings.ToLower(out.AgentBackend)]
	if out.AgentTeam == "" {
		out.AgentTeam = teamUnknownAgent
	}
	out.SetupTeam = out.DistroTeam + " / " + out.AgentTeam
	return out
}

func sanitizeContributorTeamNoHydrate(in ContributorTeamMetadata, fallbackBackend string) ContributorTeamMetadata {
	out := ContributorTeamMetadata{
		OSFamily:      normalizeOSFamily(in.OSFamily),
		OSReleaseID:   sanitizeTeamToken(in.OSReleaseID),
		OSName:        sanitizeTeamField(in.OSName),
		OSVersionID:   sanitizeTeamToken(in.OSVersionID),
		KernelRelease: sanitizeTeamField(in.KernelRelease),
		AgentBackend:  sanitizeTeamToken(firstNonEmptyTeam(in.AgentBackend, fallbackBackend)),
	}
	for _, raw := range in.OSIDLike {
		if tok := sanitizeTeamToken(raw); tok != "" {
			out.OSIDLike = append(out.OSIDLike, tok)
		}
	}
	return out
}

func distroTeam(meta ContributorTeamMetadata, catalog teamCatalog) string {
	for _, key := range append([]string{meta.OSReleaseID}, meta.OSIDLike...) {
		if team := catalog.Distros[strings.ToLower(key)]; team != "" {
			return team
		}
	}
	switch meta.OSFamily {
	case "macos":
		return teamMacOS
	case "windows":
		return "Windows / WSL"
	case "linux":
		if meta.OSReleaseID == "" && len(meta.OSIDLike) == 0 {
			return teamWeirdKernel
		}
		return teamUnknownDistro
	default:
		return teamUnknownDistro
	}
}

func normalizeOSFamily(raw string) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "darwin", "mac", "macos":
		return "macos"
	case "win32", "windows":
		return "windows"
	case "linux":
		return "linux"
	default:
		return sanitizeTeamToken(raw)
	}
}

func sanitizeTeamToken(raw string) string {
	return strings.ToLower(sanitizeTeamField(raw))
}

func sanitizeTeamField(raw string) string {
	return sanitizeCapabilityField(raw)
}

func firstNonEmptyTeam(vals ...string) string {
	for _, val := range vals {
		if strings.TrimSpace(val) != "" {
			return val
		}
	}
	return ""
}

func ptrContributorTeam(team ContributorTeamMetadata) *ContributorTeamMetadata {
	return &team
}

func (s *Server) handleTeamLeaderboardAPI(w http.ResponseWriter, _ *http.Request) {
	jsonResponse(w, s.BuildTeamLeaderboards())
}

func (s *Server) BuildTeamLeaderboards() TeamLeaderboardResponse {
	entries := buildLeaderboard()
	return TeamLeaderboardResponse{
		ByDistro:  buildTeamLeaderboard(entries, "distro"),
		ByOS:      buildTeamLeaderboard(entries, "os_family"),
		ByAgent:   buildTeamLeaderboard(entries, "agent"),
		Rarest:    rarestSetup(entries),
		Catalog:   configuredTeamCatalog(),
		Generated: "contributor_profiles",
	}
}

func buildTeamLeaderboard(entries []LeaderboardEntry, kind string) []TeamLeaderboardEntry {
	groups := map[string]*TeamLeaderboardEntry{}
	for _, entry := range entries {
		if entry.Team == nil || !teamMetadataDeclared(*entry.Team) {
			continue
		}
		team := teamNameForKind(*entry.Team, kind)
		if team == "" {
			continue
		}
		group := groups[team]
		if group == nil {
			group = &TeamLeaderboardEntry{Team: team, Kind: kind}
			groups[team] = group
		}
		group.Members++
		group.TasksCompleted += entry.TasksCompleted
		group.TasksFailed += entry.TasksFailed
		group.Findings += entry.Findings
		if entry.GitHubUsername != "" && len(group.TopContributors) < 5 {
			group.TopContributors = append(group.TopContributors, entry.GitHubUsername)
		}
	}
	out := make([]TeamLeaderboardEntry, 0, len(groups))
	for _, group := range groups {
		out = append(out, *group)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].TasksCompleted != out[j].TasksCompleted {
			return out[i].TasksCompleted > out[j].TasksCompleted
		}
		if out[i].Members != out[j].Members {
			return out[i].Members > out[j].Members
		}
		return out[i].Team < out[j].Team
	})
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func teamNameForKind(meta ContributorTeamMetadata, kind string) string {
	meta = hydrateContributorTeam(meta, meta.AgentBackend)
	switch kind {
	case "distro":
		return meta.DistroTeam
	case "os_family":
		return meta.OSFamilyTeam
	case "agent":
		return meta.AgentTeam
	default:
		return ""
	}
}

func rarestSetup(entries []LeaderboardEntry) *TeamRarestCallout {
	counts := map[string]int{}
	for _, entry := range entries {
		if entry.Team == nil || !teamMetadataDeclared(*entry.Team) {
			continue
		}
		meta := hydrateContributorTeam(*entry.Team, entry.Team.AgentBackend)
		key := meta.DistroTeam + "|" + meta.OSFamilyTeam + "|" + meta.AgentTeam
		counts[key]++
	}
	var chosen *LeaderboardEntry
	chosenCount := 0
	for i := range entries {
		if entries[i].Team == nil || !teamMetadataDeclared(*entries[i].Team) {
			continue
		}
		meta := hydrateContributorTeam(*entries[i].Team, entries[i].Team.AgentBackend)
		key := meta.DistroTeam + "|" + meta.OSFamilyTeam + "|" + meta.AgentTeam
		count := counts[key]
		if chosen == nil || count < chosenCount || (count == chosenCount && entries[i].TasksCompleted > chosen.TasksCompleted) {
			chosen = &entries[i]
			chosenCount = count
		}
	}
	if chosen == nil {
		return nil
	}
	meta := hydrateContributorTeam(*chosen.Team, chosen.Team.AgentBackend)
	return &TeamRarestCallout{
		Team:        meta.SetupTeam,
		OSFamily:    meta.OSFamilyTeam,
		Distro:      meta.DistroTeam,
		Agent:       meta.AgentTeam,
		Kernel:      meta.KernelRelease,
		Member:      chosen.GitHubUsername,
		Description: "Rarest opt-in setup on this hive — Weird Kernel energy welcome.",
	}
}

func teamMetadataDeclared(meta ContributorTeamMetadata) bool {
	return meta.OSFamily != "" || meta.OSReleaseID != "" || meta.OSName != "" ||
		meta.OSVersionID != "" || len(meta.OSIDLike) > 0 || meta.KernelRelease != "" ||
		meta.AgentBackend != ""
}
