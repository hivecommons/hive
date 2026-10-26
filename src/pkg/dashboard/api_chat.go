package dashboard

import (
	"context"
	"errors"
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

	route := chatParseRoute(body.Query)
	if answer, ok := s.chatRouteLocalAnswer(r, route); ok {
		jsonResponse(w, map[string]interface{}{
			"answer": answer,
			"status": "ok",
		})
		return
	}
	// An agent-scoped question the deterministic matcher (#14409) did not
	// understand is grounded against the agent snapshot.
	agentUnmatched := false
	if _, inScope, ok := s.chatAgentsAnswer(body.Query); inScope && !ok {
		agentUnmatched = true
		route.Kind, route.Scope = chatRouteGrounded, "agents"
		if route.Question == "" {
			route.Question = route.Raw
		}
	}

	// Grounded path: the question (with its arguments intact) against a
	// scoped snapshot of the hive state. Read-only; audited; falls through
	// to the usage reply / configured responder when it cannot run.
	var groundErr error
	if route.Kind == chatRouteGrounded {
		answer, items, err := s.chatGroundedAnswer(r.Context(), route)
		s.chatAuditGrounded(r, route, items, err)
		if err == nil {
			jsonResponse(w, map[string]interface{}{
				"answer":   answer,
				"status":   "ok",
				"grounded": true,
				"scope":    firstNonEmpty(route.Scope, "all"),
			})
			return
		}
		groundErr = err
		if s.logger != nil {
			s.logger.Debug("chat grounded answer unavailable", "scope", route.Scope, "err", err)
		}
		if agentUnmatched && s.deps != nil && s.deps.ChatResponder != nil {
			// Hand it to the responder WITH the live agent state (#14409).
			query := strings.TrimSpace(body.Query)
			if ctxText := s.chatAgentStateContext(); ctxText != "" {
				query += "\n\n" + ctxText
			}
			if answer, err := s.deps.ChatResponder(r.Context(), query, body.History); err == nil && strings.TrimSpace(answer) != "" {
				jsonResponse(w, map[string]interface{}{"answer": answer, "status": "ok"})
				return
			}
		}
		if agentUnmatched {
			jsonResponse(w, map[string]interface{}{
				"answer": "I couldn't match that agent question locally (" + chatGroundingReason(err) + "). Try `/agents stuck`, `/agents <name> status`, `/agents <name> why`, \"which agent completed last?\" or \"which agents are idle?\" — or bare `/agents` for the roster.",
				"status": "grounding_unavailable",
				"scope":  "agents",
			})
			return
		}
		if route.Scope != "" {
			// A scoped question must never collapse into the bare dump.
			jsonResponse(w, map[string]interface{}{
				"answer": chatScopeUsage(route.Scope, route.Question, chatGroundingReason(err)),
				"status": "grounding_unavailable",
				"scope":  route.Scope,
			})
			return
		}
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

	msg := "The guide agent is not available for free-text chat right now. Try `help`, `beads`, `agents`, `prs`, or `status` for local hive answers."
	if groundErr != nil {
		msg += " Grounded answers: " + chatGroundingReason(groundErr) + "."
	}
	jsonResponse(w, map[string]interface{}{
		"answer": msg,
		"status": "responder_unavailable",
	})
}

// chatGroundingReason is the operator-facing reason a grounded turn did not
// produce an answer.
func chatGroundingReason(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, errChatGroundingDisabled):
		return "grounded answers are disabled by `chat.grounded_answers.enabled: false`"
	case errors.Is(err, errChatGroundingUnavailable):
		return "no inference backend is wired for chat"
	case errors.Is(err, errChatGroundingBusy):
		return "too many chat questions are in flight; try again in a moment"
	case errors.Is(err, errChatGroundingNoStatus):
		return "hive status is still initializing"
	case errors.Is(err, context.DeadlineExceeded):
		return "the inference backend did not answer in time"
	}
	return "inference backend error: " + strings.TrimSpace(err.Error())
}

// chatRouteLocalAnswer answers the routes that need no inference: help,
// presence and the bare command dumps. Grounded routes return false.
func (s *Server) chatRouteLocalAnswer(r *http.Request, route chatRoute) (string, bool) {
	switch route.Kind {
	case chatRouteHelp:
		return chatHelpAnswer, true
	case chatRoutePresence:
		return s.chatPresenceAnswer(chatViewer(s, r)), true
	case chatRouteBare, chatRouteGrounded:
		// Deterministic argument handlers first: "scanner status" names an
		// agent and must reach its card before the bare `status` summary.
		if answer, ok := s.chatScopedLocalAnswer(route, chatViewer(s, r)); ok {
			return answer, true
		}
		if route.Kind == chatRouteBare {
			return s.chatBareAnswer(route.Scope)
		}
	}
	return "", false
}

// chatScopedLocalAnswer is where deterministic handlers for a command WITH
// arguments answer before the grounded path runs: the `/prs <filters>`
// list (hivecommons/hive#14408) and the `/agents <filter|name>` cards
// (hivecommons/hive#14409) recognise their own argument vocabularies here.
// Anything they do not recognise returns false and is grounded instead of
// collapsing to the bare dump.
func (s *Server) chatScopedLocalAnswer(route chatRoute, viewer string) (string, bool) {
	_ = viewer
	if answer, inScope, ok := s.chatAgentsAnswer(route.Raw); inScope && ok {
		return answer, true
	}
	return "", false
}

const chatHelpAnswer = "Try `beads` for bead counts, `agents` for the agent roster, `prs` for open pull requests, or `status` for a hive summary. Agent questions: `/agents stuck|paused|running|idle|failed`, `/agents <name> [status|why|last|history]`, or ask \"which agent completed last?\", \"why is scanner blocked?\". Any other arguments or plain-words question (`/prs waiting on review`, `what's waiting on my decision`) are answered from the live hive state. Commands beginning with `!` or `/` still use their command handlers."

// chatBareAnswer is the deterministic summary for a bare command scope.
func (s *Server) chatBareAnswer(scope string) (string, bool) {
	switch scope {
	case "beads":
		return s.chatBeadsAnswer(), true
	case "agents":
		if answer, _, ok := s.chatAgentsAnswer("agents: status"); ok {
			return answer, true
		}
		return "Agent status is still initializing.", true
	case "prs":
		return s.chatPRsAnswer(), true
	case "status":
		return s.chatStatusAnswer(), true
	}
	return "", false
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
