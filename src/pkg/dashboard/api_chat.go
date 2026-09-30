package dashboard

import (
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"

	"github.com/hivecommons/hive/pkg/beads"
)

func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Query   string `json:"query"`
		History []any  `json:"history"`
	}
	if err := decodeBody(r, &body); err != nil || strings.TrimSpace(body.Query) == "" {
		jsonError(w, "message is required", http.StatusBadRequest)
		return
	}

	if answer, ok := s.chatLocalIntentAnswerFor(r, body.Query); ok {
		jsonResponse(w, map[string]interface{}{
			"answer": answer,
			"status": "ok",
		})
		return
	}

	if s.deps != nil && s.deps.ChatResponder != nil {
		answer, err := s.deps.ChatResponder(r.Context(), body.Query, body.History)
		if err != nil {
			msg := "The configured chat responder is unavailable: " + strings.TrimSpace(err.Error())
			jsonResponse(w, map[string]interface{}{
				"answer": msg,
				"error":  msg,
				"status": "responder_unavailable",
			})
			return
		}
		if strings.TrimSpace(answer) != "" {
			jsonResponse(w, map[string]interface{}{
				"answer": answer,
				"status": "ok",
			})
			return
		}
	}

	jsonResponse(w, map[string]interface{}{
		"answer": "The guide agent is not available for free-text chat right now. Try `help`, `beads`, `agents`, `prs`, or `status` for local hive answers.",
		"status": "responder_unavailable",
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
	tokens := chatIntentTokens(query)
	switch {
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
