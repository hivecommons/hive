package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

type commandRoleContextKey struct{}

type inceptionSnapshot struct {
	Active    bool                `json:"active"`
	Phase     string              `json:"phase,omitempty"`
	Questions []inceptionQuestion `json:"questions,omitempty"`
	Answers   map[string]string   `json:"answers,omitempty"`
	Proposed  []inceptionFact     `json:"proposed_facts,omitempty"`
}

type inceptionStateResponse struct {
	OK     bool `json:"ok"`
	Active bool `json:"active"`
	State  struct {
		Phase     string              `json:"phase"`
		Mode      string              `json:"mode"`
		IdeaText  string              `json:"idea_text"`
		RepoURL   string              `json:"repo_url,omitempty"`
		Questions []inceptionQuestion `json:"questions"`
		Answers   map[string]string   `json:"answers"`
		FactSlugs []string            `json:"fact_slugs"`
		Proposed  []inceptionFact     `json:"proposed_facts,omitempty"`
	} `json:"state"`
}

type inceptionQuestion struct {
	ID       string `json:"id"`
	Text     string `json:"text"`
	Default  string `json:"default,omitempty"`
	Category string `json:"category,omitempty"`
}

type inceptionFact struct {
	Title     string `json:"title"`
	Body      string `json:"body"`
	Type      string `json:"type"`
	SourceRef string `json:"source_ref,omitempty"`
}

type pendingInterviewKey struct {
	backend string
	author  string
}

type pendingInterview struct {
	Questions []inceptionQuestion
	Answers   map[string]string
}

func (s *Service) registerInceptionCommand() {
	s.RegisterCommand("inception", func(ctx context.Context, args string) (string, error) {
		return s.cmdInception(ctx, args)
	})
}

func (s *Service) cmdInception(ctx context.Context, args string) (string, error) {
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return "❌ Usage: `!inception start <idea>` | `scan <repo-url>` | `state` | `answer <n> <text>` | `facts <json>` | `approve` | `reset`", nil
	}
	sub := strings.ToLower(fields[0])
	rest := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
	switch sub {
	case "start":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		if rest == "" {
			return "❌ Usage: `!inception start <idea>`", nil
		}
		body, _ := json.Marshal(map[string]string{"idea": rest})
		if err := s.dashboardPost(ctx, "/api/inception/start", body); err != nil {
			return fmt.Sprintf("❌ Failed to start inception: %s", err), nil
		}
		return "✅ Inception started. I’ll post clarification questions here when they are ready.", nil
	case "scan":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		if rest == "" {
			return "❌ Usage: `!inception scan <repo-url>`", nil
		}
		body, _ := json.Marshal(map[string]string{"repo_url": rest})
		if err := s.dashboardPost(ctx, "/api/inception/scan", body); err != nil {
			return fmt.Sprintf("❌ Failed to start brownfield scan: %s", err), nil
		}
		return "✅ Brownfield inception scan started. I’ll post clarification questions here when they are ready.", nil
	case "state":
		return s.cmdInceptionState(ctx)
	case "answer":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		return s.cmdInceptionAnswer(ctx, rest)
	case "facts":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		return s.cmdInceptionFacts(ctx, rest)
	case "approve":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		if err := s.dashboardPost(ctx, "/api/inception/approve", nil); err != nil {
			return fmt.Sprintf("❌ Failed to approve inception: %s", err), nil
		}
		s.clearPendingInterviews()
		return "✅ Inception approved and brainstorm re-paused.", nil
	case "reset":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		if err := s.dashboardPost(ctx, "/api/inception/reset", nil); err != nil {
			return fmt.Sprintf("❌ Failed to reset inception: %s", err), nil
		}
		s.clearPendingInterviews()
		return "✅ Inception reset.", nil
	default:
		return "❌ Unknown inception subcommand. Try `!inception state`.", nil
	}
}

func requireCommandOwner(ctx context.Context) error {
	role, _ := ctx.Value(commandRoleContextKey{}).(string)
	if !config.RoleAtLeast(role, config.RoleOwner) {
		return fmt.Errorf("❌ owner role required")
	}
	return nil
}

func (s *Service) cmdInceptionState(ctx context.Context) (string, error) {
	state, err := s.fetchInceptionState(ctx)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load inception state: %s", err), nil
	}
	if !state.Active || state.State.Phase == "" {
		return "ℹ️ No active inception run.", nil
	}
	if state.State.Phase == "clarify" {
		s.seedPendingInterviews(state.State.Questions, state.State.Answers)
	}
	lines := []string{fmt.Sprintf("**Inception:** %s (%s)", state.State.Phase, state.State.Mode)}
	if state.State.IdeaText != "" {
		lines = append(lines, fmt.Sprintf("Idea: %s", state.State.IdeaText))
	}
	if state.State.RepoURL != "" {
		lines = append(lines, fmt.Sprintf("Repo: %s", state.State.RepoURL))
	}
	if len(state.State.Questions) > 0 {
		lines = append(lines, formatInceptionQuestions(state.State.Questions, state.State.Answers))
	}
	if len(state.State.FactSlugs) > 0 {
		lines = append(lines, fmt.Sprintf("Facts recorded: %d", len(state.State.FactSlugs)))
	}
	if len(state.State.Proposed) > 0 {
		lines = append(lines, formatInceptionProposals(state.State.Proposed))
	}
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdInceptionAnswer(ctx context.Context, args string) (string, error) {
	parts := strings.SplitN(strings.TrimSpace(args), " ", 2)
	if len(parts) != 2 {
		return "❌ Usage: `!inception answer <n> <text>`", nil
	}
	n, err := strconv.Atoi(parts[0])
	if err != nil || n < 1 {
		return "❌ Question number must be 1 or greater.", nil
	}
	state, err := s.fetchInceptionState(ctx)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load inception questions: %s", err), nil
	}
	if n > len(state.State.Questions) {
		return fmt.Sprintf("❌ Question %d is not available.", n), nil
	}
	answer := strings.TrimSpace(parts[1])
	body, _ := json.Marshal(map[string]map[string]string{"answers": map[string]string{state.State.Questions[n-1].ID: answer}})
	if err := s.dashboardPost(ctx, "/api/inception/answer", body); err != nil {
		return fmt.Sprintf("❌ Failed to submit answer: %s", err), nil
	}
	s.markPendingAnsweredForRole(state.State.Questions[n-1].ID)
	return fmt.Sprintf("✅ Answered question %d.", n), nil
}

func (s *Service) cmdInceptionFacts(ctx context.Context, args string) (string, error) {
	if strings.TrimSpace(args) == "" {
		return "❌ Usage: `!inception facts <json-array-of-facts>`", nil
	}
	var facts []map[string]any
	if err := json.Unmarshal([]byte(args), &facts); err != nil {
		return fmt.Sprintf("❌ Facts must be a JSON array: %s", err), nil
	}
	body, _ := json.Marshal(map[string]any{"facts": facts})
	if err := s.dashboardPost(ctx, "/api/inception/facts", body); err != nil {
		return fmt.Sprintf("❌ Failed to record facts: %s", err), nil
	}
	return fmt.Sprintf("✅ Recorded %d inception facts.", len(facts)), nil
}

func (s *Service) fetchInceptionState(ctx context.Context) (*inceptionStateResponse, error) {
	data, err := s.dashboardGet(ctx, "/api/inception/state")
	if err != nil {
		return nil, err
	}
	var state inceptionStateResponse
	if err := json.Unmarshal(data, &state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (s *Service) handlePendingInterviewReply(ctx context.Context, msg Message, content string) {
	if len(s.allowedUsers) == 0 {
		return
	}
	role, ok := s.allowedUsers[msg.AuthorID]
	if !ok || !config.RoleAtLeast(role, config.RoleOwner) {
		return
	}
	key := s.pendingKey(msg.AuthorID)
	s.mu.Lock()
	pending := s.pendingInterviews[key]
	if pending == nil {
		s.mu.Unlock()
		return
	}
	q, ok := pending.lowestUnanswered()
	if !ok {
		delete(s.pendingInterviews, key)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	body, _ := json.Marshal(map[string]map[string]string{"answers": map[string]string{q.ID: content}})
	reply := fmt.Sprintf("✅ Answered question %s.", q.ID)
	if err := s.dashboardPost(ctx, "/api/inception/answer", body); err != nil {
		reply = fmt.Sprintf("❌ Failed to submit inception answer: %s", err)
	} else {
		s.mu.Lock()
		if pending := s.pendingInterviews[key]; pending != nil {
			pending.Answers[q.ID] = content
		}
		s.mu.Unlock()
	}
	s.enqueue(reply)
}

func (p *pendingInterview) lowestUnanswered() (inceptionQuestion, bool) {
	for _, q := range p.Questions {
		if strings.TrimSpace(p.Answers[q.ID]) == "" {
			return q, true
		}
	}
	return inceptionQuestion{}, false
}

func (s *Service) pendingKey(author string) pendingInterviewKey {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	return pendingInterviewKey{backend: backend, author: author}
}

func (s *Service) diffInception(prev, cur *statusSnapshot) {
	if cur.Inception.Phase == "" || !cur.Inception.Active {
		s.clearPendingInterviews()
		return
	}
	if cur.Inception.Phase != "clarify" && cur.Inception.Phase != "structure" {
		s.clearPendingInterviews()
		return
	}
	if cur.Inception.Phase != "clarify" {
		s.clearCompletedPendingInterviews()
		return
	}
	s.seedPendingInterviews(cur.Inception.Questions, cur.Inception.Answers)
	if prev.Inception.Phase != "clarify" {
		s.enqueue(formatInceptionQuestions(cur.Inception.Questions, cur.Inception.Answers))
	}
}

func (s *Service) seedPendingInterviews(questions []inceptionQuestion, answers map[string]string) {
	if len(questions) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for author, role := range s.allowedUsers {
		if !config.RoleAtLeast(role, config.RoleOwner) {
			continue
		}
		cpAnswers := make(map[string]string, len(answers))
		for k, v := range answers {
			cpAnswers[k] = v
		}
		s.pendingInterviews[s.pendingKey(author)] = &pendingInterview{
			Questions: append([]inceptionQuestion(nil), questions...),
			Answers:   cpAnswers,
		}
	}
}

func (s *Service) clearPendingInterviews() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.pendingInterviews {
		if s.backend != nil && key.backend != s.backend.Name() {
			continue
		}
		delete(s.pendingInterviews, key)
	}
}

func (s *Service) clearCompletedPendingInterviews() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key, pending := range s.pendingInterviews {
		if s.backend != nil && key.backend != s.backend.Name() {
			continue
		}
		if _, ok := pending.lowestUnanswered(); !ok {
			delete(s.pendingInterviews, key)
		}
	}
}

func (s *Service) markPendingAnsweredForRole(questionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pending := range s.pendingInterviews {
		if pending.Answers == nil {
			pending.Answers = map[string]string{}
		}
		pending.Answers[questionID] = "answered"
	}
}

func formatInceptionProposals(facts []inceptionFact) string {
	lines := []string{"Transcript proposals pending confirmation:"}
	for i, f := range facts {
		ref := ""
		if f.SourceRef != "" {
			ref = " — " + f.SourceRef
		}
		lines = append(lines, fmt.Sprintf("%d. [%s] %s%s", i+1, f.Type, f.Title, ref))
	}
	lines = append(lines, "Confirm selected items with `!inception facts` JSON including `confirmed:true`; unconfirmed proposals are discarded on approve.")
	return strings.Join(lines, "\n")
}

func formatInceptionQuestions(questions []inceptionQuestion, answers map[string]string) string {
	if len(questions) == 0 {
		return "No pending inception questions."
	}

	lines := []string{"🧭 **Inception clarification questions**"}
	for i, q := range questions {
		suffix := ""
		if strings.TrimSpace(answers[q.ID]) != "" {
			suffix = " ✅"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s", i+1, q.Text, suffix))
	}
	lines = append(lines, "Reply in plain language to answer the next unanswered question, or use `!inception answer <n> <text>`.")
	return strings.Join(lines, "\n")
}
