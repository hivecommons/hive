package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query string `json:"query"`
		// Message is an alias for Query (#13223); Query wins when both are set.
		Message string `json:"message"`
		History []any  `json:"history"`
	}
	if err := decodeBody(r, &body); err != nil {
		jsonError(w, "query is required", http.StatusBadRequest)
		return
	}
	query := strings.TrimSpace(body.Query)
	if query == "" {
		query = strings.TrimSpace(body.Message)
	}
	if query == "" {
		jsonError(w, "query is required", http.StatusBadRequest)
		return
	}
	safeQuery, verdict := ioscan.EnforceInput(query)
	if verdict.Blocked {
		s.auditFromRequest(r, "chat.dashboard.refused", auditDetail("reason", "ioscan_blocked"), "")
		jsonError(w, "message refused by safety scanner", http.StatusBadRequest)
		return
	}

	// While an inception waits in clarify, an owner's unprefixed reply is the
	// answer to the next unanswered question: hand it to the chat spine, which
	// owns that routing, before local intents and the fallback claim it (#13215).
	if !strings.HasPrefix(safeQuery, "!") && s.inceptionAwaitingAnswers() &&
		config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleOwner) &&
		s.deps.DashboardChatSubmit != nil {
		s.submitDashboardChat(w, r, safeQuery)
		return
	}

	if answer, ok := s.chatLocalIntentAnswerFor(r, safeQuery); ok {
		jsonResponse(w, map[string]interface{}{
			"answer": scrubDashboardChatAnswer(answer),
			"status": "ok",
		})
		return
	}

	if !strings.HasPrefix(safeQuery, "!") && s.deps != nil && s.deps.ChatResponder != nil {
		answer, err := s.deps.ChatResponder(r.Context(), safeQuery, body.History)
		if err != nil {
			msg := scrubDashboardChatAnswer("The configured chat responder is unavailable: " + strings.TrimSpace(err.Error()))
			jsonResponse(w, map[string]interface{}{
				"answer": msg,
				"error":  msg,
				"status": "responder_unavailable",
			})
			return
		}
		if strings.TrimSpace(answer) != "" {
			jsonResponse(w, map[string]interface{}{
				"answer": scrubDashboardChatAnswer(answer),
				"status": "ok",
			})
			return
		}
	}

	if !strings.HasPrefix(strings.TrimSpace(safeQuery), "!") {
		jsonResponse(w, map[string]interface{}{
			"answer": scrubDashboardChatAnswer(chatUnhandledIntentAnswer(safeQuery)),
			"status": "fallback",
		})
		return
	}

	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		jsonError(w, "read-write access required", http.StatusForbidden)
		return
	}

	if s.deps == nil || s.deps.DashboardChatSubmit == nil {
		jsonError(w, "dashboard chat is not configured", http.StatusServiceUnavailable)
		return
	}
	s.submitDashboardChat(w, r, safeQuery)
}

func (s *Server) submitDashboardChat(w http.ResponseWriter, r *http.Request, text string) {
	seq, err := s.deps.DashboardChatSubmit(requestUser(r), r.Header.Get("X-Hive-Role"), text)
	if err != nil {
		jsonError(w, err.Error(), http.StatusServiceUnavailable)
		return
	}

	jsonResponse(w, map[string]interface{}{
		"accepted": true,
		"seq":      seq,
	})
	s.auditFromRequest(r, "chat.dashboard.message", auditDetail("seq", strconv.FormatUint(seq, 10)), "")
}

// inceptionAwaitingAnswers reports whether the inception engine is in clarify
// with at least one unanswered question.
func (s *Server) inceptionAwaitingAnswers() bool {
	if s.deps == nil || s.deps.Inception == nil {
		return false
	}
	state := s.deps.Inception.GetState()
	if state == nil || state.Phase != knowledge.PhaseClarify {
		return false
	}
	for _, q := range state.Questions {
		if strings.TrimSpace(state.Answers[q.ID]) == "" {
			return true
		}
	}
	return false
}

func scrubDashboardChatAnswer(answer string) string {
	return redactTokens(answer)
}

// handleChatMessages serves the shared dashboard chat channel. Every reader
// with RoleRead sees the same outbox — the spine's bot replies carry no target
// user, exactly as on a Slack or Discord channel — so the response names the
// viewer: the browser uses it to tell its own lines from other operators'
// (hivecommons/hive#9135).
func (s *Server) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleRead) {
		jsonError(w, "read access required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.DashboardChatPoll == nil {
		jsonError(w, "dashboard chat is not configured", http.StatusServiceUnavailable)
		return
	}
	since, err := strconv.ParseUint(r.URL.Query().Get("since"), 10, 64)
	if err != nil && r.URL.Query().Get("since") != "" {
		jsonError(w, "since must be a sequence number", http.StatusBadRequest)
		return
	}
	poll := s.deps.DashboardChatPoll(since)
	if poll.Messages == nil {
		poll.Messages = []ChatOutbound{}
	}
	jsonResponse(w, map[string]interface{}{
		"messages": poll.Messages,
		"next":     poll.Next,
		"epoch":    poll.Epoch,
		"gap":      poll.Gap,
		"viewer":   requestUser(r),
	})
}

var chatIntentSplitRE = regexp.MustCompile(`[^a-z0-9#/-]+`)

func chatIntentTokens(query string) map[string]bool {
	normalized := strings.ToLower(strings.TrimSpace(query))
	normalized = chatIntentSplitRE.ReplaceAllString(normalized, " ")
	fields := strings.Fields(normalized)
	tokens := make(map[string]bool, len(fields))
	for _, field := range fields {
		tokens[field] = true
	}
	return tokens
}

// chatLocalIntentAnswerFor answers intents that need the caller's identity
// (presence) before falling back to the identity-free local intents.
func (s *Server) chatLocalIntentAnswerFor(r *http.Request, query string) (string, bool) {
	// Explicit slash/bang commands (v6 chat spine) win over the softer presence
	// intent so `/jam who is online?` keeps its command answer.
	if answer, ok := s.chatCommandHintAnswer(strings.TrimSpace(query)); ok {
		return answer, true
	}
	tokens := chatIntentTokens(query)
	if chatPresenceIntent(tokens) {
		return s.chatPresenceAnswer(chatViewer(s, r)), true
	}
	return s.chatLocalIntentAnswer(query)
}

// chatPresenceIntent is true for `/jam who is online?`-style questions: the
// jam scope prefix, or an explicit online/presence word alongside "who".
func chatPresenceIntent(tokens map[string]bool) bool {
	if tokens["jam"] || tokens["presence"] || tokens["online"] {
		return true
	}
	return tokens["who"] && (tokens["here"] || tokens["active"] || tokens["idle"] || tokens["free"] || tokens["around"])
}

func chatViewer(s *Server, r *http.Request) string {
	viewer := strings.TrimSpace(r.Header.Get("X-Hive-User"))
	if sess := s.sessionFromRequest(r); sess != nil {
		viewer = strings.TrimSpace(sess.Username)
	}
	return viewer
}

func (s *Server) chatPresenceAnswer(viewer string) string {
	if viewer == "" {
		return "No authenticated users are visible. Local dashboards report only `local`. Use `/who` after signing in to see the live roster."
	}
	users := s.presenceRoster(viewer)
	if len(users) == 0 {
		return "Nobody is online in this hive right now."
	}
	active := 0
	lines := make([]string, 0, len(users))
	for _, u := range users {
		name := u.DisplayName
		if name == "" {
			name = u.Username
		}
		marker, state := "⚪", "idle"
		if u.Active {
			marker, state = "🟢", "active"
			active++
		}
		line := fmt.Sprintf("%s **%s**", marker, name)
		if u.You {
			line += " (you)"
		}
		line += " — " + state
		if u.LastAction != "" {
			line += ", last action " + u.LastAction
		}
		lines = append(lines, line)
	}
	return fmt.Sprintf("%d online (%d active, %d idle):\n%s", len(users), active, len(users)-active, strings.Join(lines, "\n"))
}

func (s *Server) chatLocalIntentAnswer(query string) (string, bool) {
	trimmed := strings.TrimSpace(query)
	if answer, ok := s.chatCommandHintAnswer(trimmed); ok {
		return answer, true
	}
	if strings.HasPrefix(trimmed, "!") || strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	tokens := chatIntentTokens(query)
	switch {
	case tokens["spek"] || tokens["spectacular"] || tokens["spec"]:
		return s.chatSpekAnswer(), true
	case tokens["governor"] || tokens["kick"] || tokens["kicks"] || tokens["failure"] || tokens["failures"]:
		return s.chatGovernorAnswer(), true
	case tokens["help"]:
		return "Try `beads` for bead counts, `agents` for agent status, `prs` for open pull requests, or `status` for a hive summary. Commands beginning with `!` or `/` still use their command handlers.", true
	case tokens["bead"] || tokens["beads"]:
		return s.chatBeadsAnswer(), true
	case tokens["agent"] || tokens["agents"]:
		return s.chatAgentsAnswer(), true
	case tokens["pr"] || tokens["prs"] || tokens["pull"] || tokens["pulls"]:
		return s.chatPRsAnswer(), true
	case tokens["status"] || tokens["health"]:
		return s.chatStatusAnswer(), true
	default:
		return "", false
	}
}

func chatUnhandledIntentAnswer(query string) string {
	query = strings.TrimSpace(query)
	if query == "" {
		query = "that request"
	}
	return fmt.Sprintf("I don't know how to handle `%s` yet. Try `/help`, `/agents`, `/beads`, `/prs`, `/governor`, `/spek`, or `!help`.", query)
}

func (s *Server) chatStatusSnapshot() *StatusPayload {
	s.statusMu.RLock()
	defer s.statusMu.RUnlock()
	return s.status
}

func (s *Server) chatBeadsAnswer() string {
	if s.deps != nil && s.deps.BeadStores != nil {
		names := make([]string, 0, len(s.deps.BeadStores))
		for name := range s.deps.BeadStores {
			names = append(names, name)
		}
		sort.Strings(names)
		total := 0
		lines := make([]string, 0, len(names)+1)
		for _, name := range names {
			list := s.deps.BeadStores[name].List(beads.ListFilter{})
			total += len(list)
			lines = append(lines, fmt.Sprintf("- %s: %d bead(s)", name, len(list)))
		}
		if len(lines) == 0 {
			return "No bead stores are configured."
		}
		return fmt.Sprintf("There are %d bead(s) across %d agent store(s):\n%s", total, len(names), strings.Join(lines, "\n"))
	}
	if status := s.chatStatusSnapshot(); status != nil {
		return fmt.Sprintf("Bead summary: %d worker bead(s), %d supervisor bead(s).", status.Beads.Workers, status.Beads.Supervisor)
	}
	return "Bead data is not available yet."
}

func (s *Server) chatAgentsAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Agent status is still initializing."
	}
	if len(status.Agents) == 0 {
		return "No agents are currently reported in the hive status."
	}
	agents := append([]FrontendAgent(nil), status.Agents...)
	sort.Slice(agents, func(i, j int) bool { return agents[i].Name < agents[j].Name })
	lines := make([]string, 0, len(agents)+1)
	for _, a := range agents {
		state := strings.TrimSpace(a.State)
		if state == "" {
			state = "unknown"
		}
		if a.Paused {
			state += " (paused)"
		}
		detail := strings.TrimSpace(a.Doing)
		if detail == "" {
			detail = strings.TrimSpace(a.StructuredStatus)
		}
		if detail != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s — %s", a.Name, state, detail))
		} else {
			lines = append(lines, fmt.Sprintf("- %s: %s", a.Name, state))
		}
	}
	return fmt.Sprintf("Agents (%d):\n%s", len(agents), strings.Join(lines, "\n"))
}

func (s *Server) chatPRsAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Pull request data is still initializing."
	}
	total := 0
	lines := []string{}
	for _, repo := range status.Repos {
		count := len(repo.OpenPrs)
		total += count
		if count > 0 {
			name := repo.Full
			if name == "" {
				name = repo.Name
			}
			lines = append(lines, fmt.Sprintf("- %s: %d open PR(s)", name, count))
		}
	}
	if total == 0 {
		return "No open pull requests are currently reported."
	}
	sort.Strings(lines)
	return fmt.Sprintf("There are %d open pull request(s):\n%s", total, strings.Join(lines, "\n"))
}

func (s *Server) chatStatusAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Hive status is still initializing."
	}
	paused := 0
	for _, a := range status.Agents {
		if a.Paused {
			paused++
		}
	}
	openPRs := 0
	for _, repo := range status.Repos {
		openPRs += len(repo.OpenPrs)
	}
	health := "unknown"
	if state, ok := status.DeepHealth["status"].(string); ok && strings.TrimSpace(state) != "" {
		health = strings.TrimSpace(state)
	} else if ready, ok := status.DeepHealth["ready"].(bool); ok {
		if ready {
			health = "ready"
		} else {
			health = "not ready"
		}
	}
	return fmt.Sprintf("Hive status: %s. Agents: %d total, %d paused. Beads: %d worker, %d supervisor. Open PRs: %d.", health, len(status.Agents), paused, status.Beads.Workers, status.Beads.Supervisor, openPRs)
}

func (s *Server) chatGovernorAnswer() string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Governor status is still initializing; no recent kick failure details are available yet."
	}
	health := "unknown"
	if state, ok := status.DeepHealth["status"].(string); ok && strings.TrimSpace(state) != "" {
		health = strings.TrimSpace(state)
	} else if ready, ok := status.DeepHealth["ready"].(bool); ok && ready {
		health = "ready"
	}
	return fmt.Sprintf("Governor/kick status: hive health is %s. For detailed kick history, check the Governor and activity panels.", health)
}

func (s *Server) chatSpekAnswer() string {
	if status := s.SpektacularStatus(); status != nil && status.Present {
		return s.chatSpecRunsAnswer()
	}
	return "Spektacular/spec-run data is not available yet on this dashboard. Try the Inception/Spektacular panels or `!runs` if the chat bot is connected."
}
