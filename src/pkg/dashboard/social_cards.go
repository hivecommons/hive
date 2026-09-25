package dashboard

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	socialCardWidth        = 1200
	socialCardHeight       = 630
	socialCardCacheTTL     = 5 * time.Minute
	socialCardCacheMax     = 256
	socialCardMaxTextRunes = 96

	socialCardPlayerPrefix         = "/cards/player/"
	socialCardAchievementPrefix    = "/cards/achievement/"
	socialCardLeaderboardPrefix    = "/cards/leaderboard/"
	socialCardAPIPlayerPrefix      = "/api/cards/player/"
	socialCardAPIAchievementPrefix = "/api/cards/achievement/"
	socialCardAPILeaderboardPrefix = "/api/cards/leaderboard/"
	socialSharePlayerPrefix        = "/share/player/"
	socialShareAchievementPrefix   = "/share/achievement/"
	socialShareLeaderboardPrefix   = "/share/leaderboard/"
)

type socialCardCacheEntry struct {
	body     []byte
	etag     string
	storedAt time.Time
}

var socialCardCache = struct {
	sync.Mutex
	entries map[string]socialCardCacheEntry
}{entries: map[string]socialCardCacheEntry{}}

type socialCardData struct {
	Kind        string
	Title       string
	Subtitle    string
	Player      string
	Hive        string
	Metal       achievementMetal
	Stats       []socialCardStat
	Highlights  []string
	Footer      string
	LandingPath string
}

type socialCardStat struct {
	Label string
	Value string
}

type achievementMetal struct {
	Name      string
	Primary   string
	Secondary string
	Glow      string
}

var achievementMetals = map[string]achievementMetal{
	achievementTierSolo:     {Name: "Copper", Primary: "#b87333", Secondary: "#f0b27a", Glow: "rgba(184,115,51,.28)"},
	achievementTierDual:     {Name: "Silver", Primary: "#c0c7d1", Secondary: "#f8fafc", Glow: "rgba(192,199,209,.30)"},
	achievementTierFireteam: {Name: "Gold", Primary: "#d4af37", Secondary: "#fff3b0", Glow: "rgba(212,175,55,.32)"},
	achievementTierRaid:     {Name: "Platinum", Primary: "#d6f5ff", Secondary: "#8bd3ff", Glow: "rgba(139,211,255,.34)"},
}

var defaultSocialMetal = achievementMetal{Name: "Bronze", Primary: "#a97142", Secondary: "#f0b27a", Glow: "rgba(169,113,66,.26)"}

func achievementTierMetal(tier string) achievementMetal {
	if metal, ok := achievementMetals[strings.ToLower(strings.TrimSpace(tier))]; ok {
		return metal
	}
	return defaultSocialMetal
}

func validAchievementID(id string) bool {
	if id == "" || len(id) > 80 {
		return false
	}
	for _, c := range id {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-' || c == '_' || c == ':' {
			continue
		}
		return false
	}
	return true
}

func validLeaderboardCardBoard(board string) bool {
	switch board {
	case "contributors", "swarm", "teams":
		return true
	default:
		return false
	}
}

func (s *Server) registerSocialCardRoutes() {
	s.mux.HandleFunc("GET /cards/player/{asset}", s.handlePlayerSocialCard)
	s.mux.HandleFunc("GET /cards/achievement/{login}/{asset}", s.handleAchievementSocialCard)
	s.mux.HandleFunc("GET /cards/leaderboard/{asset}", s.handleLeaderboardSocialCard)
	s.mux.HandleFunc("GET /api/cards/player/{asset}", s.handlePlayerSocialCard)
	s.mux.HandleFunc("GET /api/cards/achievement/{login}/{asset}", s.handleAchievementSocialCard)
	s.mux.HandleFunc("GET /api/cards/leaderboard/{asset}", s.handleLeaderboardSocialCard)
	s.mux.HandleFunc("GET /share/player/{login}", s.handlePlayerSharePage)
	s.mux.HandleFunc("GET /share/achievement/{login}/{achievementID}", s.handleAchievementSharePage)
	s.mux.HandleFunc("GET /share/leaderboard/{board}", s.handleLeaderboardSharePage)
}

func (s *Server) handlePlayerSocialCard(w http.ResponseWriter, r *http.Request) {
	login, ok := parseSuffixedCardPathAny(r.URL.Path, socialCardPlayerPrefix, socialCardAPIPlayerPrefix)
	if !ok || !validGitHubUsername(login) {
		http.NotFound(w, r)
		return
	}
	data, found := s.playerCardData(login, r)
	if !found {
		http.NotFound(w, r)
		return
	}
	s.writeCachedSVG(w, r, data)
}

func (s *Server) handleAchievementSocialCard(w http.ResponseWriter, r *http.Request) {
	login, achievementID, ok := parseAchievementCardPathAny(r.URL.Path, socialCardAchievementPrefix, socialCardAPIAchievementPrefix)
	if !ok || !validGitHubUsername(login) || !validAchievementID(achievementID) {
		http.NotFound(w, r)
		return
	}
	data, found := s.achievementCardData(login, achievementID, r)
	if !found {
		http.NotFound(w, r)
		return
	}
	s.writeCachedSVG(w, r, data)
}

func (s *Server) handleLeaderboardSocialCard(w http.ResponseWriter, r *http.Request) {
	board, ok := parseSuffixedCardPathAny(r.URL.Path, socialCardLeaderboardPrefix, socialCardAPILeaderboardPrefix)
	if !ok || !validLeaderboardCardBoard(board) {
		http.NotFound(w, r)
		return
	}
	data, found := s.leaderboardCardData(board, r)
	if !found {
		http.NotFound(w, r)
		return
	}
	s.writeCachedSVG(w, r, data)
}

func parseSuffixedCardPath(path, prefix string) (string, bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || !strings.HasSuffix(rest, ".svg") {
		return "", false
	}
	value := strings.TrimSuffix(rest, ".svg")
	if value == "" || strings.Contains(value, "/") {
		return "", false
	}
	return value, true
}

func parseSuffixedCardPathAny(path string, prefixes ...string) (string, bool) {
	for _, prefix := range prefixes {
		if value, ok := parseSuffixedCardPath(path, prefix); ok {
			return value, true
		}
	}
	return "", false
}

func parseAchievementCardPath(path, prefix string) (string, string, bool) {
	rest := strings.TrimPrefix(path, prefix)
	if rest == path || !strings.HasSuffix(rest, ".svg") {
		return "", "", false
	}
	rest = strings.TrimSuffix(rest, ".svg")
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func parseAchievementCardPathAny(path string, prefixes ...string) (string, string, bool) {
	for _, prefix := range prefixes {
		if login, achievementID, ok := parseAchievementCardPath(path, prefix); ok {
			return login, achievementID, true
		}
	}
	return "", "", false
}

func (s *Server) writeCachedSVG(w http.ResponseWriter, r *http.Request, data socialCardData) {
	key := r.URL.Path
	now := time.Now()
	socialCardCache.Lock()
	if ent, ok := socialCardCache.entries[key]; ok && now.Sub(ent.storedAt) < socialCardCacheTTL {
		socialCardCache.Unlock()
		writeSocialSVGResponse(w, r, ent.body, ent.etag)
		return
	}
	socialCardCache.Unlock()

	body := []byte(renderSocialCardSVG(data, data.LandingPath))
	sum := sha256.Sum256(body)
	etag := `"` + hex.EncodeToString(sum[:]) + `"`
	socialCardCache.Lock()
	if len(socialCardCache.entries) >= socialCardCacheMax {
		oldestKey := ""
		oldestAt := now
		for k, ent := range socialCardCache.entries {
			if oldestKey == "" || ent.storedAt.Before(oldestAt) {
				oldestKey = k
				oldestAt = ent.storedAt
			}
		}
		delete(socialCardCache.entries, oldestKey)
	}
	socialCardCache.entries[key] = socialCardCacheEntry{body: body, etag: etag, storedAt: now}
	socialCardCache.Unlock()
	writeSocialSVGResponse(w, r, body, etag)
}

func writeSocialSVGResponse(w http.ResponseWriter, r *http.Request, body []byte, etag string) {
	w.Header().Set("Content-Type", "image/svg+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300, stale-while-revalidate=600")
	w.Header().Set("ETag", etag)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; img-src https: data:; style-src 'unsafe-inline'; base-uri 'none'; form-action 'none'")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func (s *Server) playerCardData(login string, r *http.Request) (socialCardData, bool) {
	profile := s.BuildContributorProfile(login)
	if !profile.Found {
		return socialCardData{}, false
	}
	achievements := topAttainedAchievementLabels(profile.Achievements2, 3)
	if len(achievements) == 0 {
		achievements = attainedMilestoneLabels(profile.Milestones, 3)
	}
	return socialCardData{
		Kind:     "Contributor card",
		Title:    profile.GitHubUsername,
		Subtitle: "Hive contributor dossier",
		Player:   profile.GitHubUsername,
		Hive:     hiveNameFromProfile(profile),
		Metal:    achievementTierMetal(profile.Achievement2.TopTier),
		Stats: []socialCardStat{
			{Label: "Rank", Value: rankValue(profile.Rank, profile.Total)},
			{Label: "Score", Value: strconv.Itoa(profile.TasksCompleted)},
			{Label: "PR tasks", Value: strconv.Itoa(profile.TasksWithPR)},
		},
		Highlights:  achievements,
		Footer:      "Public stats from this hive's leaderboard",
		LandingPath: socialSharePlayerPrefix + profile.GitHubUsername,
	}, true
}

func (s *Server) achievementCardData(login, achievementID string, r *http.Request) (socialCardData, bool) {
	profile := s.BuildContributorProfile(login)
	if !profile.Found {
		return socialCardData{}, false
	}
	achievement, ok := findAttainedAchievement(profile, achievementID)
	if !ok {
		return socialCardData{}, false
	}
	metal := achievementTierMetal(achievement.Tier)
	rarity := s.achievementRarity(achievementID)
	highlights := []string{achievement.Detail}
	if rarity != "" {
		highlights = append(highlights, rarity)
	}
	date := displayDate(profile.RegisteredAt)
	stats := []socialCardStat{
		{Label: "Tier", Value: metal.Name},
		{Label: "Track", Value: strings.TrimSpace(achievement.Track)},
	}
	if date != "" {
		stats = append(stats, socialCardStat{Label: "Joined", Value: date})
	}
	return socialCardData{
		Kind:        "Achievement unlocked",
		Title:       achievement.Label,
		Subtitle:    metal.Name + " tier",
		Player:      profile.GitHubUsername,
		Hive:        hiveNameFromProfile(profile),
		Metal:       metal,
		Stats:       stats,
		Highlights:  highlights,
		Footer:      "Achievement System 2.0 · " + achievement.ID,
		LandingPath: socialShareAchievementPrefix + profile.GitHubUsername + "/" + achievement.ID,
	}, true
}

func (s *Server) leaderboardCardData(board string, r *http.Request) (socialCardData, bool) {
	switch board {
	case "contributors":
		entries := buildLeaderboardWithInputs(s.achievement2BaseInputs())
		top := make([]string, 0, 3)
		totalTasks := 0
		for _, entry := range entries {
			totalTasks += entry.TasksCompleted
			if len(top) < 3 {
				top = append(top, fmt.Sprintf("#%d %s · %d tasks", entry.Rank, entry.GitHubUsername, entry.TasksCompleted))
			}
		}
		return socialCardData{Kind: "Leaderboard", Title: "Contributor standings", Subtitle: "Public Hive leaderboard", Hive: s.publicHiveName(), Metal: achievementTierMetal(achievementTierFireteam), Stats: []socialCardStat{{Label: "Contributors", Value: strconv.Itoa(len(entries))}, {Label: "Tasks", Value: strconv.Itoa(totalTasks)}}, Highlights: top, Footer: "Ranked by completed tasks", LandingPath: socialShareLeaderboardPrefix + board}, true
	case "teams":
		teams := s.BuildTeamLeaderboards()
		top := topTeamHighlights(teams)
		total := len(teams.ByDistro) + len(teams.ByOS) + len(teams.ByAgent)
		return socialCardData{Kind: "Leaderboard", Title: "Team leagues", Subtitle: "Opt-in public contributor setups", Hive: s.publicHiveName(), Metal: achievementTierMetal(achievementTierDual), Stats: []socialCardStat{{Label: "Leagues", Value: strconv.Itoa(total)}, {Label: "Rarest", Value: emptyDash(rarestTeamName(teams.Rarest))}}, Highlights: top, Footer: "Distro, OS, and agent metadata only", LandingPath: socialShareLeaderboardPrefix + board}, true
	case "swarm":
		history, err := s.swarmStore().history(r.Context())
		if err != nil {
			return socialCardData{}, false
		}
		top := make([]string, 0, 3)
		for i, leader := range history.Leaderboard {
			if i >= 3 {
				break
			}
			top = append(top, fmt.Sprintf("#%d %s · %d pts", leader.Rank, leader.Repo, leader.Score))
		}
		return socialCardData{Kind: "Leaderboard", Title: "Swarm leaderboard", Subtitle: history.Name, Hive: s.publicHiveName(), Metal: achievementTierMetal(achievementTierRaid), Stats: []socialCardStat{{Label: "Swarms", Value: strconv.Itoa(len(history.History))}, {Label: "Repos", Value: strconv.Itoa(len(history.Leaderboard))}}, Highlights: top, Footer: "Public swarm history", LandingPath: socialShareLeaderboardPrefix + board}, true
	default:
		return socialCardData{}, false
	}
}

func topAttainedAchievementLabels(achievements []ContributorAchievement, limit int) []string {
	attained := make([]ContributorAchievement, 0, len(achievements))
	for _, achievement := range achievements {
		if achievement.Attained {
			attained = append(attained, achievement)
		}
	}
	sort.SliceStable(attained, func(i, j int) bool {
		return tierWeight(attained[i].Tier) > tierWeight(attained[j].Tier)
	})
	out := make([]string, 0, limit)
	for _, achievement := range attained {
		if len(out) >= limit {
			break
		}
		out = append(out, achievement.Label)
	}
	return out
}

func attainedMilestoneLabels(milestones []ContributorMilestone, limit int) []string {
	out := make([]string, 0, limit)
	for _, milestone := range milestones {
		if milestone.Attained {
			out = append(out, milestone.Label)
			if len(out) >= limit {
				break
			}
		}
	}
	return out
}

func findAttainedAchievement(profile ContributorProfileResponse, id string) (ContributorAchievement, bool) {
	for _, achievement := range profile.Achievements2 {
		if achievement.Attained && achievement.ID == id {
			return achievement, true
		}
	}
	return ContributorAchievement{}, false
}

func tierWeight(tier string) int {
	switch tier {
	case achievementTierRaid:
		return 4
	case achievementTierFireteam:
		return 3
	case achievementTierDual:
		return 2
	case achievementTierSolo:
		return 1
	default:
		return 0
	}
}

func (s *Server) achievementRarity(id string) string {
	profiles := listContributorProfiles()
	if len(profiles) == 0 {
		return ""
	}
	total := 0
	unlocked := 0
	inputs := s.achievement2BaseInputs()
	localHiveID := s.localHiveIdentity()
	for i := range profiles {
		p := &profiles[i]
		if p.TrustTier == "revoked" {
			continue
		}
		total++
		profileInputs := inputs
		profileInputs.LocalHiveID = localHiveID
		profileInputs.Hives = buildContributorHives(p.GitHubUsername, localHiveID)
		achievements, _ := buildAchievements2WithInputs(p, profileInputs)
		for _, achievement := range achievements {
			if achievement.ID == id && achievement.Attained {
				unlocked++
				break
			}
		}
	}
	if total == 0 {
		return ""
	}
	return fmt.Sprintf("Unlocked by %d of %d contributors", unlocked, total)
}

func rankValue(rank, total int) string {
	if rank <= 0 || total <= 0 {
		return "—"
	}
	return fmt.Sprintf("#%d/%d", rank, total)
}

func hiveNameFromProfile(profile ContributorProfileResponse) string {
	for _, hive := range profile.Hives {
		if strings.TrimSpace(hive.ProjectName) != "" {
			return hive.ProjectName
		}
		if strings.TrimSpace(hive.Org) != "" {
			return hive.Org
		}
		if strings.TrimSpace(hive.ID) != "" {
			return hive.ID
		}
	}
	return "Hive"
}

func (s *Server) publicHiveName() string {
	if s != nil && s.deps != nil && s.deps.Config != nil && strings.TrimSpace(s.deps.Config.Project.Name) != "" {
		return strings.TrimSpace(s.deps.Config.Project.Name)
	}
	return "Hive"
}

func displayDate(raw string) string {
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(time.DateOnly)
	}
	return ""
}

func topTeamHighlights(teams TeamLeaderboardResponse) []string {
	out := make([]string, 0, 3)
	appendTop := func(rows []TeamLeaderboardEntry) {
		for _, row := range rows {
			if len(out) >= 3 {
				return
			}
			out = append(out, fmt.Sprintf("%s · %d tasks", row.Team, row.TasksCompleted))
		}
	}
	appendTop(teams.ByDistro)
	appendTop(teams.ByOS)
	appendTop(teams.ByAgent)
	return out
}

func rarestTeamName(team *TeamRarestCallout) string {
	if team == nil {
		return ""
	}
	return team.Team
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "—"
	}
	return s
}

func renderSocialCardSVG(data socialCardData, landingURL string) string {
	metal := data.Metal
	if metal.Name == "" {
		metal = defaultSocialMetal
	}
	stats := renderSocialStats(data.Stats)
	highlights := renderSocialHighlights(data.Highlights)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" role="img" aria-labelledby="title desc">
<title id="title">%s</title><desc id="desc">%s</desc>
<a href="%s" target="_top">
<rect width="1200" height="630" rx="0" fill="#07111f"/>
<circle cx="1030" cy="100" r="250" fill="%s" opacity="0.55"/>
<circle cx="190" cy="540" r="250" fill="%s" opacity="0.26"/>
<path d="M80 90h1040v450H80z" fill="#0f172a" stroke="%s" stroke-width="4" rx="34"/>
<path d="M110 120h980v390H110z" fill="#111c31" stroke="rgba(255,255,255,.12)" stroke-width="2" rx="24"/>
<text x="150" y="168" fill="%s" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="26" font-weight="700" letter-spacing="5">%s</text>
<text x="150" y="248" fill="#f8fafc" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="66" font-weight="800">%s</text>
<text x="152" y="302" fill="#cbd5e1" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="31">%s</text>
<rect x="820" y="145" width="210" height="210" rx="105" fill="%s" opacity="0.24" stroke="%s" stroke-width="8"/>
<text x="925" y="238" text-anchor="middle" fill="%s" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="34" font-weight="800">%s</text>
<text x="925" y="286" text-anchor="middle" fill="#e2e8f0" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="24">tier</text>
%s
%s
<text x="150" y="498" fill="#94a3b8" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="24">%s</text>
<text x="1050" y="498" text-anchor="end" fill="#94a3b8" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="24">%s</text>
</a>
</svg>`, socialCardWidth, socialCardHeight, socialCardWidth, socialCardHeight,
		svgEscape(data.Title), svgEscape(data.Subtitle), svgEscape(landingURL), metal.Glow, metal.Primary, metal.Primary,
		metal.Secondary, svgEscape(strings.ToUpper(data.Kind)), svgEscape(truncateSocialRunes(data.Title, socialCardMaxTextRunes)), svgEscape(truncateSocialRunes(data.Subtitle, socialCardMaxTextRunes)),
		metal.Primary, metal.Secondary, metal.Secondary, svgEscape(metal.Name), stats, highlights, svgEscape(data.Footer), svgEscape(data.Hive))
}

func renderSocialStats(stats []socialCardStat) string {
	var b strings.Builder
	for i, stat := range stats {
		if i >= 4 {
			break
		}
		x := 150 + (i * 165)
		b.WriteString(fmt.Sprintf(`<g><text x="%d" y="380" fill="#f8fafc" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="42" font-weight="800">%s</text><text x="%d" y="414" fill="#94a3b8" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="20" letter-spacing="2">%s</text></g>`, x, svgEscape(truncateSocialRunes(stat.Value, 18)), x, svgEscape(strings.ToUpper(stat.Label))))
	}
	return b.String()
}

func renderSocialHighlights(highlights []string) string {
	if len(highlights) == 0 {
		return ""
	}
	var b strings.Builder
	for i, highlight := range highlights {
		if i >= 3 {
			break
		}
		y := 382 + (i * 36)
		b.WriteString(fmt.Sprintf(`<text x="690" y="%d" fill="#cbd5e1" font-family="Inter,Segoe UI,Arial,sans-serif" font-size="24">• %s</text>`, y, svgEscape(truncateSocialRunes(highlight, 42))))
	}
	return b.String()
}

func svgEscape(s string) string { return html.EscapeString(s) }

func truncateSocialRunes(s string, max int) string {
	r := []rune(strings.TrimSpace(s))
	if len(r) <= max {
		return string(r)
	}
	if max <= 1 {
		return string(r[:max])
	}
	return string(r[:max-1]) + "…"
}

func absoluteURL(r *http.Request, path string) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	host := r.Host
	if forwarded := r.Header.Get("X-Forwarded-Host"); forwarded != "" {
		host = forwarded
	}
	if host == "" {
		host = "localhost"
	}
	return scheme + "://" + host + path
}

func (s *Server) handlePlayerSharePage(w http.ResponseWriter, r *http.Request) {
	login := strings.TrimPrefix(r.URL.Path, socialSharePlayerPrefix)
	if login == "" || strings.Contains(login, "/") || !validGitHubUsername(login) {
		http.NotFound(w, r)
		return
	}
	profile := s.BuildContributorProfile(login)
	if !profile.Found {
		http.NotFound(w, r)
		return
	}
	card := socialCardPlayerPrefix + profile.GitHubUsername + ".svg"
	body := `<h1>` + html.EscapeString(profile.GitHubUsername) + `</h1><p>Rank ` + html.EscapeString(rankValue(profile.Rank, profile.Total)) + ` · ` + strconv.Itoa(profile.TasksCompleted) + ` tasks shipped · ` + html.EscapeString(profile.TrustTier) + ` tier.</p><p><a href="/contribute/dossier/` + html.EscapeString(profile.GitHubUsername) + `">Open the public dossier</a></p>`
	s.writeSharePage(w, r, "Hive contributor: "+profile.GitHubUsername, body, card)
}

func (s *Server) handleAchievementSharePage(w http.ResponseWriter, r *http.Request) {
	login, achievementID, ok := parseShareAchievementPath(r.URL.Path)
	if !ok || !validGitHubUsername(login) || !validAchievementID(achievementID) {
		http.NotFound(w, r)
		return
	}
	profile := s.BuildContributorProfile(login)
	if !profile.Found {
		http.NotFound(w, r)
		return
	}
	achievement, ok := findAttainedAchievement(profile, achievementID)
	if !ok {
		http.NotFound(w, r)
		return
	}
	card := socialCardAchievementPrefix + profile.GitHubUsername + "/" + achievement.ID + ".svg"
	metal := achievementTierMetal(achievement.Tier)
	body := `<h1>` + html.EscapeString(achievement.Label) + `</h1><p>` + html.EscapeString(profile.GitHubUsername) + ` unlocked a ` + html.EscapeString(metal.Name) + ` achievement on ` + html.EscapeString(hiveNameFromProfile(profile)) + `.</p><p>` + html.EscapeString(achievement.Detail) + `</p><p><a href="/contribute/dossier/` + html.EscapeString(profile.GitHubUsername) + `">Explore the public dossier</a></p>`
	s.writeSharePage(w, r, achievement.Label+" · "+profile.GitHubUsername, body, card)
}

func (s *Server) handleLeaderboardSharePage(w http.ResponseWriter, r *http.Request) {
	board := strings.TrimPrefix(r.URL.Path, socialShareLeaderboardPrefix)
	if board == "" || strings.Contains(board, "/") || !validLeaderboardCardBoard(board) {
		http.NotFound(w, r)
		return
	}
	data, ok := s.leaderboardCardData(board, r)
	if !ok {
		http.NotFound(w, r)
		return
	}
	items := ""
	for _, h := range data.Highlights {
		items += "<li>" + html.EscapeString(h) + "</li>"
	}
	body := `<h1>` + html.EscapeString(data.Title) + `</h1><p>` + html.EscapeString(data.Subtitle) + `</p><ul>` + items + `</ul><p><a href="/contribute/leaderboard">Open the live leaderboard</a></p>`
	s.writeSharePage(w, r, data.Title, body, socialCardLeaderboardPrefix+board+".svg")
}

func parseShareAchievementPath(path string) (string, string, bool) {
	rest := strings.TrimPrefix(path, socialShareAchievementPrefix)
	if rest == path {
		return "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

func (s *Server) writeSharePage(w http.ResponseWriter, r *http.Request, title, body, cardPath string) {
	cardURL := absoluteURL(r, cardPath)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	_, _ = w.Write([]byte(`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>` + html.EscapeString(title) + `</title><meta property="og:title" content="` + html.EscapeString(title) + `"><meta property="og:type" content="website"><meta property="og:image" content="` + html.EscapeString(cardURL) + `"><meta name="twitter:card" content="summary_large_image"><meta name="twitter:image" content="` + html.EscapeString(cardURL) + `"><style>body{margin:0;font-family:Inter,Segoe UI,Arial,sans-serif;background:#0f172a;color:#e2e8f0}.wrap{max-width:860px;margin:0 auto;padding:48px 24px}a{color:#93c5fd}.card{border:1px solid rgba(255,255,255,.14);border-radius:18px;padding:28px;background:#111827}.cta{margin-top:32px;color:#94a3b8;font-size:.95rem}</style></head><body><main class="wrap"><section class="card">` + body + `</section><p class="cta">Curious how this was made? <a href="/contribute">Contribute to this hive</a> or run your own when you are ready.</p></main></body></html>`))
}
