package dashboard

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

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
	bang := strings.HasPrefix(safeQuery, "!")

	// While an inception waits in clarify, an owner's unprefixed reply is the
	// answer to the next unanswered question: hand it to the chat spine, which
	// owns that routing, before local intents and the fallback claim it (#13215).
	if !bang && s.inceptionAwaitingAnswers() &&
		config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleOwner) &&
		s.deps.DashboardChatSubmit != nil {
		s.submitDashboardChat(w, r, safeQuery)
		return
	}

	if bang {
		// `!` commands belong to the shared chat spine, which enforces the
		// allowlist and owner role.
		s.handleChatBang(w, r, safeQuery)
		return
	}

	// Explicit slash commands with a v6 local handler (`/governor`,
	// `/knowledge ...`, `/spek ...`, `/who`) answer before the shared router
	// so they keep their command answers.
	if answer, ok := s.chatCommandHintAnswer(safeQuery); ok {
		jsonResponse(w, map[string]interface{}{
			"answer": scrubDashboardChatAnswer(answer),
			"status": "ok",
		})
		return
	}

	route := chatParseRoute(safeQuery)
	if answer, ok := s.chatRouteLocalAnswer(r, route); ok {
		jsonResponse(w, map[string]interface{}{
			"answer": scrubDashboardChatAnswer(answer),
			"status": "ok",
		})
		return
	}
	// An agent-scoped question the deterministic matcher (#14409) did not
	// understand is grounded against the agent snapshot.
	agentUnmatched := false
	if _, inScope, ok := s.chatAgentsAnswer(safeQuery); inScope && !ok {
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
				"answer":   scrubDashboardChatAnswer(answer),
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
			query := safeQuery
			if ctxText := s.chatAgentStateContext(); ctxText != "" {
				query += "\n\n" + ctxText
			}
			if answer, err := s.deps.ChatResponder(r.Context(), query, body.History); err == nil && strings.TrimSpace(answer) != "" {
				jsonResponse(w, map[string]interface{}{"answer": scrubDashboardChatAnswer(answer), "status": "ok"})
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

	msg := "The guide agent is not available for free-text chat right now. Try `help`, `beads`, `agents`, `prs`, or `status` for local hive answers."
	if groundErr != nil {
		msg += " Grounded answers: " + chatGroundingReason(groundErr) + "."
	}
	jsonResponse(w, map[string]interface{}{
		"answer": scrubDashboardChatAnswer(msg),
		"status": "responder_unavailable",
	})
}

// handleChatBang forwards a `!` command to the shared dashboard chat spine.
func (s *Server) handleChatBang(w http.ResponseWriter, r *http.Request, text string) {
	if !config.RoleAtLeast(r.Header.Get("X-Hive-Role"), config.RoleReadWrite) {
		jsonError(w, "read-write access required", http.StatusForbidden)
		return
	}
	if s.deps == nil || s.deps.DashboardChatSubmit == nil {
		jsonError(w, "dashboard chat is not configured", http.StatusServiceUnavailable)
		return
	}
	s.submitDashboardChat(w, r, text)
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
	if answer, inScope, ok := s.chatAgentsAnswer(route.Raw); inScope && ok {
		return answer, true
	}
	// A PR question made only of the filters parseChatPRFilter knows is
	// answered from the snapshot without a model turn (hivecommons/hive#14408);
	// anything with words it cannot place goes to the grounded path.
	if route.Scope == "prs" {
		if f := parseChatPRFilter(route.Raw, viewer); route.Kind == chatRouteBare || len(f.Unknown) == 0 {
			return s.chatPRsAnswer(route.Raw, viewer), true
		}
	}
	return "", false
}

const chatHelpAnswer = "Try `beads` for bead counts, `agents` for the agent roster, `prs` for the open pull requests (filters: " + chatPRFilterHint + "), or `status` for a hive summary. Agent questions: `/agents stuck|paused|running|idle|failed`, `/agents <name> [status|why|last|history]`, or ask \"which agent completed last?\", \"why is scanner blocked?\". Any other arguments or plain-words question (`/prs waiting on review`, `what's waiting on my decision`) are answered from the live hive state. Commands beginning with `!` or `/` still use their command handlers."

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
		return s.chatPRsAnswer("", ""), true
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

// chatPRsAnswer lists the snapshot's open pull requests, narrowed by the
// filters in the query remainder (parseChatPRFilter). It reads the same
// cached FrontendPR rows /api/prs serves — no GitHub call (hivecommons/hive#14408).
func (s *Server) chatPRsAnswer(query, viewer string) string {
	status := s.chatStatusSnapshot()
	if status == nil {
		return "Pull request data is still initializing."
	}
	return renderChatPRList(status.Repos, parseChatPRFilter(query, viewer), time.Now())
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
