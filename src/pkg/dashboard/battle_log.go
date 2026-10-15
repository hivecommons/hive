package dashboard

import (
	"bytes"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hivecommons/hive/pkg/knowledge"
)

const (
	defaultBattleLogLimit       = 30
	maxBattleLogLimit           = 100
	hiveOfWeekKnowledgeTag      = "hive-of-the-week"
	hiveGourceLogContentType    = "text/plain; charset=utf-8"
	hiveGourceDefaultProject    = "hive"
	battleLogGenericWorkTarget  = "work item"
	battleLogJoinTarget         = "the hive"
	battleLogContributorSubject = "contributor"
	// hiveOfWeekVideoURLEnv / hiveOfWeekPosterURLEnv point the Hive of the Week
	// card at a published render (e.g. the hive-of-the-week GitHub release
	// asset). When neither the env nor the ?video= override is set, the payload
	// carries no media URL and the client renders an empty state instead of a
	// player pointed at a path nothing serves (#12109).
	hiveOfWeekVideoURLEnv  = "HIVE_OF_WEEK_VIDEO_URL"
	hiveOfWeekPosterURLEnv = "HIVE_OF_WEEK_POSTER_URL"
	battleLogKindIssue     = "issue"
	battleLogKindPR        = "pr"
)

var (
	emailLikeRE = regexp.MustCompile(`[A-Za-z0-9._%+\-]+@[A-Za-z0-9.\-]+\.[A-Za-z]{2,}`)
	urlLikeRE   = regexp.MustCompile(`https?://[^\s<>"']+`)
	tokenLikeRE = regexp.MustCompile(`(?i)(token|secret|password|pat|key)=([^,\s]+)`)
	repoRefRE   = regexp.MustCompile(`\b[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+\b`)
	// pathRunRE finds maximal runs of path-ish characters so scrubbing can tell
	// a bare owner/repo token apart from a URL path such as /api/hives/summary
	// (#12109): only a run with exactly one interior slash and no leading slash
	// is treated as a repository reference.
	pathRunRE = regexp.MustCompile(`[A-Za-z0-9_.\-/]+`)
	// taskDescRE matches assignDesc output: "<kind> <owner/repo>#<number>: <title>".
	taskDescRE        = regexp.MustCompile(`^(issue|pr|pull[ -]?request|github)\s+([A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+)#(\d+)(?::\s*(.*))?$`)
	issueRefRE        = regexp.MustCompile(`#\d+`)
	unsafePathCharsRE = regexp.MustCompile(`[^A-Za-z0-9._/\-#]+`)
	gourceProjectName = regexp.MustCompile(`^[A-Za-z0-9_.\-/]{1,120}$`)
)

type battleLogEvent struct {
	Timestamp string `json:"timestamp"`
	Actor     string `json:"actor"`
	Action    string `json:"action"`
	Kind      string `json:"kind"`
	Icon      string `json:"icon"`
	Target    string `json:"target"`
	Repo      string `json:"repo,omitempty"`
	// Ref is the short item reference ("hive#12063" when the repo is public,
	// "#12063" otherwise); Title is the scrubbed item title; URL links to the
	// GitHub item and is only set when the repo is on HIVE_PUBLIC_REPOS (#12109).
	Ref   string `json:"ref,omitempty"`
	Title string `json:"title,omitempty"`
	URL   string `json:"url,omitempty"`
}

// battleLogTask is the structured view of an assignDesc() activity task string.
type battleLogTask struct {
	Kind   string
	Repo   string
	Number string
	Title  string
}

func parseBattleLogTask(task string) (battleLogTask, bool) {
	m := taskDescRE.FindStringSubmatch(strings.TrimSpace(task))
	if m == nil {
		return battleLogTask{}, false
	}
	kind := strings.ToLower(m[1])
	switch kind {
	case "pr", "pull request", "pull-request", "pullrequest":
		kind = battleLogKindPR
	case "issue":
		kind = battleLogKindIssue
	default:
		kind = ""
	}
	return battleLogTask{Kind: kind, Repo: m[2], Number: m[3], Title: strings.TrimSpace(m[4])}, true
}

type hiveOfWeekResponse struct {
	Project        string `json:"project"`
	Week           string `json:"week"`
	ActivityCount  int    `json:"activity_count"`
	GourceLogURL   string `json:"gource_log_url"`
	VideoURL       string `json:"video_url"`
	PosterURL      string `json:"poster_url"`
	LeaderboardURL string `json:"leaderboard_url"`
	EmbedHTML      string `json:"embed_html"`
	ReadmeMarkdown string `json:"readme_markdown"`
}

func (s *Server) registerBattleLogRoutes() {
	s.mux.HandleFunc("GET /api/leaderboard/battle-log", s.handleBattleLog)
	s.mux.HandleFunc("GET /api/leaderboard/hive-of-week", s.handleHiveOfWeek)
	s.mux.HandleFunc("GET /api/leaderboard/gource-log", s.handleGourceLog)
	s.mux.HandleFunc("POST /api/leaderboard/hive-of-week/archive", s.handleHiveOfWeekArchive)
}

func (s *Server) handleBattleLog(w http.ResponseWriter, r *http.Request) {
	limit := boundedQueryInt(r, "limit", defaultBattleLogLimit, maxBattleLogLimit)
	events := buildBattleLogEvents(s.recentContributionActivity(), limit)
	jsonResponse(w, map[string]any{"events": events, "retained": len(events)})
}

func buildBattleLogEvents(activity []ActivityEntry, limit int) []battleLogEvent {
	if limit <= 0 || limit > maxBattleLogLimit {
		limit = defaultBattleLogLimit
	}
	out := make([]battleLogEvent, 0, len(activity))
	// A contributor relay that reconnects books "joined" again once the hub's
	// 60s debounce has lapsed, and the intervening "left"/"released" rows are
	// not battle-log kinds, so the public feed showed the same actor joining
	// twice (#12109). Keep only the newest join per actor.
	joined := map[string]bool{}
	for i := len(activity) - 1; i >= 0 && len(out) < limit; i-- {
		ev, ok := battleLogFromActivity(activity[i])
		if !ok {
			continue
		}
		if ev.Kind == "joined_hive" {
			if joined[ev.Actor] {
				continue
			}
			joined[ev.Actor] = true
		}
		out = append(out, ev)
	}
	return out
}

func battleLogFromActivity(e ActivityEntry) (battleLogEvent, bool) {
	actor := scrubBattleLogText(e.Username)
	if actor == "" {
		actor = battleLogContributorSubject
	}
	action := strings.ToLower(strings.TrimSpace(e.Action))
	kind, icon, verb := classifyBattleLogAction(action, e)
	if kind == "" {
		return battleLogEvent{}, false
	}
	ev := battleLogEvent{
		Timestamp: e.Timestamp,
		Actor:     actor,
		Action:    verb,
		Kind:      kind,
		Icon:      icon,
		Target:    battleLogTarget(e, kind),
		Repo:      publicRepoFromTask(e.Task),
	}
	if kind == "joined_hive" {
		return ev, true
	}
	if task, ok := parseBattleLogTask(e.Task); ok {
		ev.Action = battleLogVerbForTask(kind, verb, task.Kind)
		ev.Title = scrubBattleLogText(task.Title)
		ev.Ref = "#" + task.Number
		if ev.Repo != "" {
			_, name, _ := strings.Cut(ev.Repo, "/")
			ev.Ref = name + "#" + task.Number
			ev.URL = githubItemURL(ev.Repo, task.Kind, task.Number)
		}
		ev.Target = ev.Ref
		if ev.Title != "" {
			ev.Target += ": " + ev.Title
		}
	}
	return ev, true
}

// battleLogVerbForTask rewords the generic verb for the item kind: a PR is
// merged, an issue is closed, and "picked up" names what was picked up, so the
// row never reads "merged issue" (#12109).
func battleLogVerbForTask(kind, verb, taskKind string) string {
	switch kind {
	case "merged_pr":
		if taskKind == battleLogKindIssue {
			return "closed issue"
		}
		return "merged PR"
	case "review", "local_model_work":
		switch taskKind {
		case battleLogKindIssue:
			return verb + " issue"
		case battleLogKindPR:
			return verb + " PR"
		}
	}
	return verb
}

func githubItemURL(repo, kind, number string) string {
	segment := "issues"
	if kind == battleLogKindPR {
		segment = "pull"
	}
	return "https://github.com/" + repo + "/" + segment + "/" + number
}

func classifyBattleLogAction(action string, e ActivityEntry) (kind, icon, verb string) {
	switch {
	case action == "completed":
		return "merged_pr", "🔀", "merged"
	case action == "picked up" && strings.Contains(strings.ToLower(e.CLI+" "+e.Model), "local"):
		return "local_model_work", "🖥️", "started local-model work on"
	case action == "picked up":
		return "review", "🔧", "picked up"
	case action == "joined":
		return "joined_hive", "🐝", "joined"
	case action == "promoted" || strings.Contains(action, "achievement"):
		return "achievement_unlocked", "🏅", "unlocked"
	case strings.Contains(action, "approved"):
		return "spec_approved", "✅", "approved"
	case strings.Contains(action, "swarm"):
		return "swarm_joined", "🍯", "joined swarm"
	default:
		return "", "", ""
	}
}

func battleLogTarget(e ActivityEntry, kind string) string {
	if kind == "joined_hive" {
		return battleLogJoinTarget
	}
	if kind == "achievement_unlocked" && strings.TrimSpace(e.Task) == "" {
		return "achievement"
	}
	task := scrubBattleLogText(e.Task)
	if task == "" {
		return battleLogGenericWorkTarget
	}
	if scrubRepoRefs(task) != task {
		return battleLogGenericWorkTarget
	}
	return task
}

func scrubBattleLogText(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	s = emailLikeRE.ReplaceAllString(s, "[email]")
	s = urlLikeRE.ReplaceAllString(s, "[link]")
	s = tokenLikeRE.ReplaceAllString(s, "$1=[redacted]")
	s = scrubRepoRefs(s)
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	if len([]rune(s)) > 120 {
		return string([]rune(s)[:120]) + "…"
	}
	return s
}

// scrubRepoRefs replaces bare owner/repo tokens with "[project]" while leaving
// URL paths (leading slash or more than one slash) alone (#12109).
func scrubRepoRefs(s string) string {
	return pathRunRE.ReplaceAllStringFunc(s, func(run string) string {
		if strings.HasPrefix(run, "/") || strings.Count(run, "/") != 1 {
			return run
		}
		if !repoRefRE.MatchString(run) {
			return run
		}
		return "[project]"
	})
}

func (s *Server) handleHiveOfWeek(w http.ResponseWriter, r *http.Request) {
	project, count := busiestProject(s.recentContributionActivity(), time.Now().UTC())
	weekStart := weekStartUTC(time.Now().UTC())
	resp := hiveOfWeekPayload(project, weekStart, count, r)
	jsonResponse(w, resp)
}

func (s *Server) handleGourceLog(w http.ResponseWriter, r *http.Request) {
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		project, _ = busiestProject(s.recentContributionActivity(), time.Now().UTC())
	}
	if !validPublicGourceProject(project) {
		http.Error(w, "invalid or non-public project", http.StatusBadRequest)
		return
	}
	weekStart := parseWeekQuery(r.URL.Query().Get("week"), time.Now().UTC())
	lines := gourceLogForProject(s.recentContributionActivity(), project, weekStart)
	w.Header().Set("Content-Type", hiveGourceLogContentType)
	_, _ = w.Write([]byte(strings.Join(lines, "\n")))
	if len(lines) > 0 {
		_, _ = w.Write([]byte("\n"))
	}
}

func (s *Server) handleHiveOfWeekArchive(w http.ResponseWriter, r *http.Request) {
	if !requireOwnerRole(w, r) {
		return
	}
	project := strings.TrimSpace(r.URL.Query().Get("project"))
	if project == "" {
		project, _ = busiestProject(s.recentContributionActivity(), time.Now().UTC())
	}
	if !validPublicGourceProject(project) {
		http.Error(w, "invalid or non-public project", http.StatusBadRequest)
		return
	}
	weekStart := parseWeekQuery(r.URL.Query().Get("week"), time.Now().UTC())
	lines := gourceLogForProject(s.recentContributionActivity(), project, weekStart)
	if err := s.archiveHiveOfWeekLog(project, weekStart, strings.Join(lines, "\n")); err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	jsonResponse(w, map[string]any{"ok": true, "project": project, "week": formatHiveWeek(weekStart), "lines": len(lines)})
}

func hiveOfWeekPayload(project string, weekStart time.Time, count int, r *http.Request) hiveOfWeekResponse {
	if project == "" {
		project = hiveGourceDefaultProject
	}
	week := formatHiveWeek(weekStart)
	q := "?project=" + urlQueryEscape(project) + "&week=" + urlQueryEscape(week)
	logURL := "/api/leaderboard/gource-log" + q
	videoURL := strings.TrimSpace(r.URL.Query().Get("video"))
	if videoURL == "" {
		videoURL = strings.TrimSpace(os.Getenv(hiveOfWeekVideoURLEnv))
	}
	posterURL := strings.TrimSpace(r.URL.Query().Get("poster"))
	if posterURL == "" {
		posterURL = strings.TrimSpace(os.Getenv(hiveOfWeekPosterURLEnv))
	}
	leaderboardURL := "/contribute/leaderboard"
	embedHTML := ""
	readme := fmt.Sprintf(`Powered by Hive Battle Log · [Leaderboard](%s) · [Gource source log](%s)`, leaderboardURL, logURL)
	if videoURL != "" {
		embedHTML = fmt.Sprintf(`<video controls preload="metadata" poster="%s"><source src="%s" type="video/mp4"><a href="%s">Watch Hive of the Week</a></video>`, htmlAttr(posterURL), htmlAttr(videoURL), htmlAttr(videoURL))
		readme = fmt.Sprintf(`[![Hive of the Week: %s](%s)](%s)`+"\n\n", project, posterURL, videoURL) + readme
	}
	return hiveOfWeekResponse{Project: project, Week: week, ActivityCount: count, GourceLogURL: logURL, VideoURL: videoURL, PosterURL: posterURL, LeaderboardURL: leaderboardURL, EmbedHTML: embedHTML, ReadmeMarkdown: readme}
}

func busiestProject(activity []ActivityEntry, now time.Time) (string, int) {
	start := weekStartUTC(now)
	counts := map[string]int{}
	for _, e := range activity {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(start) || !t.Before(start.AddDate(0, 0, 7)) {
			continue
		}
		project := publicRepoFromTask(e.Task)
		if project == "" {
			continue
		}
		counts[project]++
	}
	if len(counts) == 0 {
		return hiveGourceDefaultProject, 0
	}
	names := make([]string, 0, len(counts))
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	best := names[0]
	for _, name := range names[1:] {
		if counts[name] > counts[best] {
			best = name
		}
	}
	return best, counts[best]
}

func gourceLogForProject(activity []ActivityEntry, project string, weekStart time.Time) []string {
	project = strings.TrimSpace(project)
	var lines []string
	for _, e := range activity {
		t, err := time.Parse(time.RFC3339, e.Timestamp)
		if err != nil || t.Before(weekStart) || !t.Before(weekStart.AddDate(0, 0, 7)) {
			continue
		}
		repo := publicRepoFromTask(e.Task)
		if repo == "" {
			continue
		}
		if repo != project {
			continue
		}
		user := sanitizeGourceUser(e.Username)
		typ, area, colour := gourceMapping(e.Action)
		path := fmt.Sprintf("%s/%s/%s", sanitizeGourceProjectPath(repo), area, sanitizeGourcePathPart(gourceLeaf(e)))
		lines = append(lines, fmt.Sprintf("%d|%s|%s|%s|%s", t.Unix(), user, typ, path, colour))
	}
	sort.Strings(lines)
	return lines
}

func gourceMapping(action string) (typ, area, colour string) {
	action = strings.ToLower(strings.TrimSpace(action))
	switch {
	case action == "completed":
		return "M", "merged-prs", "FFC857"
	case action == "picked up":
		return "A", "work-in-flight", "FFB000"
	case action == "joined":
		return "A", "contributors", "F5D76E"
	case action == "failed" || strings.HasPrefix(action, "released"):
		return "D", "setbacks", "C46A00"
	case action == "promoted" || strings.Contains(action, "achievement"):
		return "A", "achievements", "FFE8A3"
	default:
		return "M", "activity", "E6A700"
	}
}

func gourceLeaf(e ActivityEntry) string {
	if strings.TrimSpace(e.Task) != "" {
		return e.Task
	}
	if strings.TrimSpace(e.Username) != "" {
		return e.Username
	}
	return "event"
}

func weekStartUTC(t time.Time) time.Time {
	t = t.UTC()
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
	offset := (int(day.Weekday()) + 6) % 7
	return day.AddDate(0, 0, -offset)
}

func parseWeekQuery(raw string, now time.Time) time.Time {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return weekStartUTC(now)
	}
	if t, err := time.Parse("2006-01-02", raw); err == nil {
		return weekStartUTC(t)
	}
	var y, w int
	if _, err := fmt.Sscanf(raw, "%d-W%d", &y, &w); err == nil && w >= 1 && w <= 53 {
		jan4 := time.Date(y, 1, 4, 0, 0, 0, 0, time.UTC)
		return weekStartUTC(jan4).AddDate(0, 0, (w-1)*7)
	}
	return weekStartUTC(now)
}

func formatHiveWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%04d-W%02d", y, w)
}

func publicRepoFromTask(task string) string {
	m := repoRefRE.FindString(task)
	if !validGourceProject(m) || !publicRepoAllowed(m) {
		return ""
	}
	return m
}

func validPublicGourceProject(project string) bool {
	if project == hiveGourceDefaultProject {
		return true
	}
	return validGourceProject(project) && publicRepoAllowed(project)
}

func publicRepoAllowed(repo string) bool {
	repo = strings.ToLower(strings.TrimSpace(repo))
	if repo == "" {
		return false
	}
	for _, part := range strings.Split(os.Getenv("HIVE_PUBLIC_REPOS"), ",") {
		if strings.ToLower(strings.TrimSpace(part)) == repo {
			return true
		}
	}
	return false
}

func validGourceProject(project string) bool {
	return project != "" && gourceProjectName.MatchString(project) && !strings.Contains(project, "..")
}

func sanitizeGourceUser(user string) string {
	user = strings.TrimSpace(user)
	if user == "" {
		return battleLogContributorSubject
	}
	user = emailLikeRE.ReplaceAllString(user, "[email]")
	user = urlLikeRE.ReplaceAllString(user, "[link]")
	user = tokenLikeRE.ReplaceAllString(user, "$1-[redacted]")
	user = strings.ReplaceAll(user, "|", "-")
	if len([]rune(user)) > 60 {
		user = string([]rune(user)[:60])
	}
	return user
}

func sanitizeGourcePathPart(s string) string {
	if ref := issueRefRE.FindString(s); ref != "" {
		s = "issue-" + strings.TrimPrefix(ref, "#")
	}
	s = scrubBattleLogText(s)
	s = strings.ReplaceAll(s, "|", "-")
	s = strings.ReplaceAll(s, "..", ".")
	s = unsafePathCharsRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "/.-")
	if s == "" {
		return "event"
	}
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	return s
}

func sanitizeGourceProjectPath(s string) string {
	s = strings.ReplaceAll(s, "|", "-")
	s = strings.ReplaceAll(s, "..", ".")
	s = unsafePathCharsRE.ReplaceAllString(s, "-")
	s = strings.Trim(s, "/.-")
	if s == "" {
		return hiveGourceDefaultProject
	}
	return s
}

func boundedQueryInt(r *http.Request, key string, fallback, max int) int {
	n := fallback
	if raw := strings.TrimSpace(r.URL.Query().Get(key)); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > max {
		return max
	}
	return n
}

func (s *Server) archiveHiveOfWeekLog(project string, weekStart time.Time, body string) error {
	if s == nil || s.deps == nil || s.deps.Knowledge == nil {
		return fmt.Errorf("knowledge store is not configured")
	}
	stores := s.deps.Knowledge.FileStores()
	if len(stores) == 0 {
		return fmt.Errorf("knowledge store has no writable file vault")
	}
	title := fmt.Sprintf("Hive of the Week gource source %s %s", project, formatHiveWeek(weekStart))
	factBody := fmt.Sprintf("Archived gource custom log source for `%s` during `%s`.\n\n```gource\n%s\n```\n", project, formatHiveWeek(weekStart), strings.TrimSpace(body))
	fact := knowledge.ExtractedFact{
		Title:      title,
		Type:       knowledge.FactReference,
		Body:       factBody,
		Confidence: 1,
		Tags:       []string{hiveOfWeekKnowledgeTag, "gource", sanitizeGourceProjectPath(project), formatHiveWeek(weekStart)},
		SourceDate: weekStart,
	}
	if err := stores[0].WriteFacts([]knowledge.ExtractedFact{fact}); err != nil {
		return err
	}
	stores[0].Reindex()
	return nil
}

func htmlAttr(s string) string {
	var buf bytes.Buffer
	for _, r := range s {
		switch r {
		case '&':
			buf.WriteString("&amp;")
		case '<':
			buf.WriteString("&lt;")
		case '>':
			buf.WriteString("&gt;")
		case '"':
			buf.WriteString("&quot;")
		case '\'':
			buf.WriteString("&#39;")
		default:
			buf.WriteRune(r)
		}
	}
	return buf.String()
}

func urlQueryEscape(s string) string {
	repl := strings.NewReplacer(" ", "%20", "#", "%23", "&", "%26", "?", "%3F", "+", "%2B")
	return repl.Replace(s)
}
