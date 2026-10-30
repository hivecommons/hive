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
		KickError string              `json:"kick_error,omitempty"`
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
		return "❌ Usage: `!inception start <idea>` | `scan <repo-url>` | `state` | `answer <n> <text>` | `skip <n>` | `submit` | `facts <json>` | `approve` | `reset`", nil
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
	case "skip":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		return s.cmdInceptionSkip(ctx, rest)
	case "submit":
		if err := requireCommandOwner(ctx); err != nil {
			return err.Error(), nil
		}
		return s.cmdInceptionSubmit(ctx)
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
	if state.State.KickError != "" {
		lines = append(lines, "❌ "+state.State.KickError)
	}
	if len(state.State.Questions) > 0 {
		if inceptionAcceptsAnswers(state.State.Phase) {
			answers, skipped := s.mergedInceptionAnswers(state)
			lines = append(lines, formatInceptionQuestionsWithSkips(state.State.Questions, answers, skipped))
		} else {
			lines = append(lines, formatInceptionAnswerSummary(state.State.Questions, state.State.Answers))
		}
	}
	if len(state.State.FactSlugs) > 0 {
		lines = append(lines, fmt.Sprintf("Facts recorded: %d", len(state.State.FactSlugs)))
	}
	if len(state.State.Proposed) > 0 {
		lines = append(lines, formatInceptionProposals(state.State.Proposed))
	}
	return strings.Join(lines, "\n"), nil
}

// recordInceptionAnswer records an answer to a specific question and returns
// whether the interview is complete, along with the current answer state.
func (s *Service) recordInceptionAnswer(ctx context.Context, state *inceptionStateResponse, qID string, content string) (bool, map[string]string, map[string]bool) {
	key := inceptionRunKey(state)
	s.mu.Lock()
	buf := s.inceptionBufferLocked(key)
	buf.Answers[qID] = content
	delete(buf.Skipped, qID)
	s.mu.Unlock()

	answers, skipped := s.mergedInceptionAnswers(state)
	return inceptionInterviewDone(state.State.Questions, answers, skipped), answers, skipped
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
	if !inceptionAcceptsAnswers(state.State.Phase) {
		return fmt.Sprintf("❌ cannot submit answers in phase %s; remaining questions use their defaults", state.State.Phase), nil
	}

	done, answers, _ := s.recordInceptionAnswer(ctx, state, state.State.Questions[n-1].ID, answer)
	if done {
		return s.submitInceptionBuffer(ctx, state)
	}
	return fmt.Sprintf("✅ Recorded answer %d (%d of %d). Reply `!inception submit` to send them, or keep answering.",
		n, countInceptionAnswered(state.State.Questions, answers), len(state.State.Questions)), nil
}

func (s *Service) cmdInceptionSkip(ctx context.Context, args string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(args))
	if err != nil || n < 1 {
		return "❌ Usage: `!inception skip <n>`", nil
	}
	state, err := s.fetchInceptionState(ctx)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load inception questions: %s", err), nil
	}
	if n > len(state.State.Questions) {
		return fmt.Sprintf("❌ Question %d is not available.", n), nil
	}
	if !inceptionAcceptsAnswers(state.State.Phase) {
		return fmt.Sprintf("❌ cannot skip questions in phase %s", state.State.Phase), nil
	}
	id := state.State.Questions[n-1].ID
	s.mu.Lock()
	buf := s.inceptionBufferLocked(inceptionRunKey(state))
	delete(buf.Answers, id)
	buf.Skipped[id] = true
	s.mu.Unlock()

	answers, skipped := s.mergedInceptionAnswers(state)
	if inceptionInterviewDone(state.State.Questions, answers, skipped) {
		if countInceptionAnswered(state.State.Questions, answers) == 0 {
			return fmt.Sprintf("⏭️ Skipped question %d. %s", n, inceptionNeedsOneAnswer), nil
		}
		return s.submitInceptionBuffer(ctx, state)
	}
	return fmt.Sprintf("⏭️ Skipped question %d; it will use its default. Reply `!inception submit` to send your answers, or keep answering.", n), nil
}

func (s *Service) cmdInceptionSubmit(ctx context.Context) (string, error) {
	state, err := s.fetchInceptionState(ctx)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load inception questions: %s", err), nil
	}
	return s.submitInceptionBuffer(ctx, state)
}

// submitInceptionBuffer sends every buffered answer in one request, so the
// engine leaves clarify once, after the operator is done answering.
func (s *Service) submitInceptionBuffer(ctx context.Context, state *inceptionStateResponse) (string, error) {
	key := inceptionRunKey(state)
	s.mu.Lock()
	var batch map[string]string
	skipped := 0
	if buf := s.inceptionBuffers[key]; buf != nil {
		batch = make(map[string]string, len(buf.Answers))
		for id, v := range buf.Answers {
			batch[id] = v
		}
		skipped = len(buf.Skipped)
	}
	s.mu.Unlock()
	if len(batch) == 0 {
		if skipped > 0 {
			return "❌ " + inceptionNeedsOneAnswer, nil
		}
		return "❌ No buffered answers to submit. Use `!inception answer <n> <text>` first.", nil
	}
	body, _ := json.Marshal(map[string]map[string]string{"answers": batch})
	if err := s.dashboardPost(ctx, "/api/inception/answer", body); err != nil {
		return fmt.Sprintf("❌ Failed to submit answers: %s", inceptionErrorText(err)), nil
	}
	s.mu.Lock()
	delete(s.inceptionBuffers, key)
	s.mu.Unlock()
	for id := range batch {
		s.markPendingAnsweredForRole(id)
	}
	answers := make(map[string]string, len(state.State.Answers)+len(batch))
	for id, v := range state.State.Answers {
		answers[id] = v
	}
	for id, v := range batch {
		answers[id] = v
	}
	reply := fmt.Sprintf("✅ Submitted %d answers.", len(batch))
	if missing := len(state.State.Questions) - countInceptionAnswered(state.State.Questions, answers); missing > 0 {
		reply += fmt.Sprintf(" The remaining %d questions use their defaults.", missing)
	}
	return reply, nil
}

type inceptionBuffer struct {
	Answers map[string]string
	Skipped map[string]bool
}

// inceptionNeedsOneAnswer mirrors the engine rule that a clarify submission
// must carry at least one non-empty answer; skips alone cannot leave clarify.
const inceptionNeedsOneAnswer = "Inception needs at least one answer to leave clarify — skips alone cannot be submitted. Answer any question with `!inception answer <n> <text>`."

// inceptionRunKey identifies an inception run so buffered answers never leak
// into a later run.
func inceptionRunKey(state *inceptionStateResponse) string {
	return state.State.Mode + "\x00" + state.State.IdeaText + "\x00" + state.State.RepoURL
}

func (s *Service) inceptionBufferLocked(key string) *inceptionBuffer {
	if s.inceptionBuffers == nil {
		s.inceptionBuffers = make(map[string]*inceptionBuffer)
	}
	buf := s.inceptionBuffers[key]
	if buf == nil {
		buf = &inceptionBuffer{Answers: map[string]string{}, Skipped: map[string]bool{}}
		s.inceptionBuffers[key] = buf
	}
	return buf
}

// mergedInceptionAnswers overlays the buffered answers on the server's.
func (s *Service) mergedInceptionAnswers(state *inceptionStateResponse) (map[string]string, map[string]bool) {
	answers := make(map[string]string, len(state.State.Answers))
	for id, v := range state.State.Answers {
		answers[id] = v
	}
	skipped := map[string]bool{}
	s.mu.Lock()
	defer s.mu.Unlock()
	if buf := s.inceptionBuffers[inceptionRunKey(state)]; buf != nil {
		for id, v := range buf.Answers {
			answers[id] = v
		}
		for id := range buf.Skipped {
			skipped[id] = true
		}
	}
	return answers, skipped
}

func countInceptionAnswered(questions []inceptionQuestion, answers map[string]string) int {
	n := 0
	for _, q := range questions {
		if strings.TrimSpace(answers[q.ID]) != "" {
			n++
		}
	}
	return n
}

func inceptionInterviewDone(questions []inceptionQuestion, answers map[string]string, skipped map[string]bool) bool {
	_, open := nextOpenInceptionQuestion(questions, answers, skipped)
	return !open
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
	// Same identity rule as commands (router.go, F8): a server-verified
	// dashboard role stands in only while no allowlist is configured.
	var role string
	var ok bool
	if s.allowedUserCount() == 0 {
		role, ok = trustedMessageRole(msg)
	} else {
		role, ok = s.allowedUserRole(msg.AuthorID)
	}
	if !ok || !config.RoleAtLeast(role, config.RoleOwner) {
		return
	}
	key := s.pendingKey(msg.AuthorID)
	s.mu.Lock()
	_, hasPending := s.pendingInterviews[key]
	s.mu.Unlock()
	if !hasPending && msg.TrustedRole == "" {
		return
	}
	// The live state supplies the run key the answer buffer is scoped by and
	// confirms the engine still accepts answers.
	state, err := s.fetchInceptionState(ctx)
	if err != nil {
		if hasPending {
			s.enqueue(fmt.Sprintf("❌ Failed to load inception questions: %s", err))
		}
		return
	}
	if !hasPending {
		s.seedPendingInterviewFromState(state, key)
	}
	s.mu.Lock()
	pending := s.pendingInterviews[key]
	if pending == nil {
		s.mu.Unlock()
		return
	}
	if _, ok := pending.lowestUnanswered(); !ok {
		delete(s.pendingInterviews, key)
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()
	if !inceptionAcceptsAnswers(state.State.Phase) {
		s.enqueue(fmt.Sprintf("❌ cannot submit answers in phase %s; remaining questions use their defaults", state.State.Phase))
		return
	}

	answers, skipped := s.mergedInceptionAnswers(state)
	q, ok := nextOpenInceptionQuestion(state.State.Questions, answers, skipped)
	if !ok {
		return
	}
	s.mu.Lock()
	if pending := s.pendingInterviews[key]; pending != nil {
		pending.Answers[q.ID] = content
	}
	s.mu.Unlock()

	// Same one-call rule as `!inception answer`: the engine leaves clarify on
	// the first submission, so the reply is buffered until the interview is
	// complete (or `!inception submit`) rather than posted on its own.
	done, answers, _ := s.recordInceptionAnswer(ctx, state, q.ID, content)
	if done {
		reply, _ := s.submitInceptionBuffer(ctx, state)
		s.enqueue(reply)
		return
	}
	s.enqueue(fmt.Sprintf("✅ Recorded answer %d (%d of %d). Reply to the next question, or use `!inception submit` to send them now.",
		inceptionQuestionNumber(state.State.Questions, q.ID), countInceptionAnswered(state.State.Questions, answers), len(state.State.Questions)))
}

// nextOpenInceptionQuestion returns the first question that is neither
// answered (on the server or in the buffer) nor skipped.
func nextOpenInceptionQuestion(questions []inceptionQuestion, answers map[string]string, skipped map[string]bool) (inceptionQuestion, bool) {
	for _, q := range questions {
		if strings.TrimSpace(answers[q.ID]) == "" && !skipped[q.ID] {
			return q, true
		}
	}
	return inceptionQuestion{}, false
}

func inceptionQuestionNumber(questions []inceptionQuestion, id string) int {
	for i, q := range questions {
		if q.ID == id {
			return i + 1
		}
	}
	return 0
}

// seedPendingInterviewFromState seeds key's interview from the live
// inception state when none is pending yet. The dashboard only forwards an
// unprefixed reply (TrustedRole set) while the engine is in clarify, which can
// be before the next status snapshot reaches diffInception (#13215).
func (s *Service) seedPendingInterviewFromState(state *inceptionStateResponse, key pendingInterviewKey) {
	if !state.Active || state.State.Phase != "clarify" || len(state.State.Questions) == 0 {
		return
	}
	answers := make(map[string]string, len(state.State.Answers))
	for k, v := range state.State.Answers {
		answers[k] = v
	}
	s.mu.Lock()
	if _, exists := s.pendingInterviews[key]; !exists {
		s.pendingInterviews[key] = &pendingInterview{
			Questions: append([]inceptionQuestion(nil), state.State.Questions...),
			Answers:   answers,
		}
	}
	s.mu.Unlock()
}

func (p *pendingInterview) lowestUnanswered() (inceptionQuestion, bool) {
	for _, q := range p.Questions {
		if strings.TrimSpace(p.Answers[q.ID]) == "" {
			return q, true
		}
	}
	return inceptionQuestion{}, false
}

// pendingKey keys a pending interview by config.IdentityMatchKey(author):
// seeding walks the configured allowlist identities while replies carry the
// transport's author ID, and the two must meet the same way the allowlist
// lookup (allowedUserRole) matches them.
func (s *Service) pendingKey(author string) pendingInterviewKey {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	return pendingInterviewKey{backend: backend, author: config.IdentityMatchKey(author)}
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
	for author, role := range s.allowedUsersSnapshot() {
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
	s.inceptionBuffers = nil
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

func inceptionAcceptsAnswers(phase string) bool {
	return phase == "clarify" || phase == "structure"
}

// formatInceptionAnswerSummary reports the answers recorded once inception has
// moved past the phases that accept answers, instead of listing the
// unanswered questions as open.
func formatInceptionAnswerSummary(questions []inceptionQuestion, answers map[string]string) string {
	answered := 0
	for _, q := range questions {
		if strings.TrimSpace(answers[q.ID]) != "" {
			answered++
		}
	}
	line := fmt.Sprintf("Moving on with %d of %d answers", answered, len(questions))
	if answered < len(questions) {
		line += "; remaining questions use their defaults"
	}
	return line + "."
}

// inceptionErrorText reduces a dashboard error of the form
// "HTTP <code>: <json with an error field>" to the plain error message,
// falling back to the original text.
func inceptionErrorText(err error) string {
	msg := err.Error()
	if !strings.HasPrefix(msg, "HTTP ") {
		return msg
	}
	_, body, ok := strings.Cut(msg, ": ")
	if !ok {
		return msg
	}
	var payload struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(body), &payload) != nil || strings.TrimSpace(payload.Error) == "" {
		return msg
	}
	text := payload.Error
	if strings.HasPrefix(text, "cannot submit answers in phase ") {
		text += "; remaining questions use their defaults"
	}
	return text
}

func formatInceptionQuestions(questions []inceptionQuestion, answers map[string]string) string {
	return formatInceptionQuestionsWithSkips(questions, answers, nil)
}

func formatInceptionQuestionsWithSkips(questions []inceptionQuestion, answers map[string]string, skipped map[string]bool) string {
	if len(questions) == 0 {
		return "No pending inception questions."
	}

	lines := []string{"🧭 **Inception clarification questions**"}
	for i, q := range questions {
		suffix := ""
		if strings.TrimSpace(answers[q.ID]) != "" {
			suffix = " ✅"
		} else if skipped[q.ID] {
			suffix = " ⏭️ skipped"
		}
		lines = append(lines, fmt.Sprintf("%d. %s%s", i+1, q.Text, suffix))
	}
	lines = append(lines, "Reply in plain language to answer the next unanswered question, or use `!inception answer <n> <text>`, `!inception skip <n>` and `!inception submit`.")
	return strings.Join(lines, "\n")
}
