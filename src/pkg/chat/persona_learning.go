package chat

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/hivecommons/hive/pkg/agentaudit"
	"github.com/hivecommons/hive/pkg/persona"
)

// Persona learning in the chat spine (hivecommons/hive#8363).
//
// Three explicit signals are derived from what an author does with run
// summaries, counted on the author's persona record, and turned into
// PROPOSALS by pkg/persona once they pass the configured threshold:
//
//   - expanded: `!runs <key> more` from an author whose persona renders the
//     outcomes summary by default;
//   - skipped: approve/reject of a run the author never expanded;
//   - re-asked: `!runs <key>` for a summary the author already saw and did
//     not expand.
//
// A suggestion is inert until `!persona accept <n>`; `!persona reject` drops
// it. Accepting writes an audit row with the evidence. None of this reaches
// ACMM, agent mode, or policy: the persona store is disjoint from autonomy
// and the persona package is stdlib-only (see pkg/persona learning_test.go).

// personaAuditAction is the audit action recorded for accept, reject, and undo.
const personaAuditAction = agentaudit.AuditPersonaAdjusted

const (
	personaAuditOutcomeAccepted = "accepted"
	personaAuditOutcomeRejected = "rejected"
	personaAuditOutcomeUndone   = "undone"
)

type personaRunKey struct {
	backend string
	author  string
	runKey  string
}

func (s *Service) personaRunKey(author, runKey string) personaRunKey {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	return personaRunKey{backend: backend, author: author, runKey: runKey}
}

// learningConfig returns the live learning configuration; zero when unset.
func (s *Service) learningConfig() persona.LearningConfig {
	if s.personaLearning == nil {
		return persona.LearningConfig{}
	}
	return s.personaLearning()
}

// observeSummaryShown marks that the author saw the outcomes summary for a
// run. Seeing it again without expanding in between is the re-asked signal.
func (s *Service) observeSummaryShown(ctx context.Context, author, runKey string) {
	if !s.learningConfig().Enabled {
		return
	}
	key := s.personaRunKey(author, runKey)
	s.mu.Lock()
	_, shown := s.shownSummaries[key]
	_, expanded := s.expandedRuns[key]
	s.shownSummaries[key] = struct{}{}
	s.mu.Unlock()
	if shown && !expanded {
		s.recordPersonaSignal(ctx, author, persona.SignalReAsked)
	}
}

// observeSummaryExpanded records the expanded signal when an author whose
// default rendering is the outcomes summary asks for the technical view.
func (s *Service) observeSummaryExpanded(ctx context.Context, author, runKey string) {
	if !s.learningConfig().Enabled {
		return
	}
	key := s.personaRunKey(author, runKey)
	s.mu.Lock()
	s.expandedRuns[key] = struct{}{}
	s.mu.Unlock()
	record, ok, err := s.getPersona(ctx, author)
	if err != nil || !ok || record.Normalize().Depth == persona.DepthTechnical {
		return
	}
	s.recordPersonaSignal(ctx, author, persona.SignalExpanded)
}

// observeRunDecision records the skipped signal when the deciding author
// acted on a run they never expanded, then forgets the run's marks. It counts
// for every depth: a technical author who never asks for more is the evidence
// that moves depth back toward outcomes.
func (s *Service) observeRunDecision(ctx context.Context, runKey string) {
	author, _ := ctx.Value(commandAuthorContextKey{}).(string)
	if author == "" || !s.learningConfig().Enabled {
		return
	}
	key := s.personaRunKey(author, runKey)
	s.mu.Lock()
	_, expanded := s.expandedRuns[key]
	delete(s.expandedRuns, key)
	delete(s.shownSummaries, key)
	s.mu.Unlock()
	if expanded {
		return
	}
	s.recordPersonaSignal(ctx, author, persona.SignalSkipped)
}

// recordPersonaSignal counts one signal on the author's stored persona and
// announces any suggestion the count produced. Authors without a persona
// record are never counted: there is nothing to learn against.
func (s *Service) recordPersonaSignal(ctx context.Context, author, signal string) {
	record, ok, err := s.getPersona(ctx, author)
	if err != nil || !ok {
		return
	}
	before := len(record.Suggestions())
	updated, err := record.RecordSignal(signal, s.now(), s.learningConfig())
	if err != nil {
		s.logger.Warn("chat: persona signal rejected", "user_id", author, "signal", signal, "error", err)
		return
	}
	if err := s.putPersona(ctx, author, updated); err != nil {
		s.logger.Warn("chat: persona signal not saved", "user_id", author, "signal", signal, "error", err)
		return
	}
	if pending := updated.Suggestions(); len(pending) > before {
		prefix := ""
		if len(s.allowedUsers) > 1 {
			prefix = "For " + author + ": "
		}
		s.enqueue(fmt.Sprintf("💡 %sPersona suggestion: %s. Reply `!persona accept %d` to apply or `!persona reject` to dismiss.",
			prefix, formatSuggestion(pending[len(pending)-1]), len(pending)))
	}
}

func (s *Service) cmdPersonaSuggestions(ctx context.Context, author string) (string, error) {
	if !s.learningConfig().Enabled {
		return "ℹ️ Persona learning is off (persona.learning.enabled). No suggestions are collected.", nil
	}
	record, ok, err := s.getPersona(ctx, author)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
	}
	if !ok {
		return "ℹ️ No persona recorded. Run `!persona setup`.", nil
	}
	pending := record.Suggestions()
	if len(pending) == 0 {
		return "ℹ️ No persona suggestions pending.", nil
	}
	lines := []string{"**Persona suggestions**"}
	for i, suggestion := range pending {
		lines = append(lines, fmt.Sprintf("%d. %s", i+1, formatSuggestion(suggestion)))
	}
	lines = append(lines, "Reply `!persona accept <n>` to apply one or `!persona reject` to dismiss them all.")
	return strings.Join(lines, "\n"), nil
}

func (s *Service) cmdPersonaAccept(ctx context.Context, author, arg string) (string, error) {
	n, err := strconv.Atoi(strings.TrimSpace(arg))
	if err != nil {
		return "❌ Usage: `!persona accept <n>`", nil
	}
	record, ok, err := s.getPersona(ctx, author)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
	}
	if !ok {
		return "ℹ️ No persona recorded. Run `!persona setup`.", nil
	}
	updated, adj, err := record.AcceptSuggestion(n, s.now())
	if err != nil {
		return "❌ " + err.Error(), nil
	}
	if err := s.putPersona(ctx, author, updated); err != nil {
		return fmt.Sprintf("❌ Failed to save persona: %s", err), nil
	}
	s.auditPersona(author, personaAuditOutcomeAccepted, adj)
	return "✅ Persona adjusted: " + formatAdjustment(adj) + ".\n" + formatPersona(updated), nil
}

func (s *Service) cmdPersonaReject(ctx context.Context, author string) (string, error) {
	record, ok, err := s.getPersona(ctx, author)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
	}
	if !ok {
		return "ℹ️ No persona recorded. Run `!persona setup`.", nil
	}
	pending := record.Suggestions()
	if len(pending) == 0 {
		return "ℹ️ No persona suggestions pending.", nil
	}
	if err := s.putPersona(ctx, author, record.RejectSuggestions(s.now())); err != nil {
		return fmt.Sprintf("❌ Failed to save persona: %s", err), nil
	}
	for _, suggestion := range pending {
		s.auditPersona(author, personaAuditOutcomeRejected, persona.Adjustment{
			Key: suggestion.Key, From: suggestion.From, To: suggestion.To, Evidence: suggestion.Evidence,
		})
	}
	return fmt.Sprintf("✅ Dismissed %d persona suggestion(s). Nothing changed.", len(pending)), nil
}

func (s *Service) cmdPersonaUndo(ctx context.Context, author string) (string, error) {
	record, ok, err := s.getPersona(ctx, author)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
	}
	if !ok {
		return "ℹ️ No persona recorded. Run `!persona setup`.", nil
	}
	updated, last, err := record.UndoAdjustment(s.now())
	if err != nil {
		return "❌ " + err.Error(), nil
	}
	if err := s.putPersona(ctx, author, updated); err != nil {
		return fmt.Sprintf("❌ Failed to save persona: %s", err), nil
	}
	s.auditPersona(author, personaAuditOutcomeUndone, last)
	return fmt.Sprintf("✅ Reverted %s to %s and pinned the persona. `!persona unpin` resumes learning.\n%s",
		last.Key, last.From, formatPersona(updated)), nil
}

func (s *Service) cmdPersonaPin(ctx context.Context, author string, pinned bool) (string, error) {
	record, ok, err := s.getPersona(ctx, author)
	if err != nil {
		return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
	}
	if !ok {
		return "ℹ️ No persona recorded. Run `!persona setup`.", nil
	}
	if err := s.putPersona(ctx, author, record.SetPinned(pinned, s.now())); err != nil {
		return fmt.Sprintf("❌ Failed to save persona: %s", err), nil
	}
	if pinned {
		return "✅ Persona pinned. Learning will not propose changes until `!persona unpin`.", nil
	}
	return "✅ Persona unpinned. Learning resumes from an empty evidence window.", nil
}

// auditPersona records one persona adjustment event with its evidence. The
// row names the author as actor and carries no agent name: no agent's
// permissions are involved.
func (s *Service) auditPersona(author, outcome string, adj persona.Adjustment) {
	if s.audit == nil {
		return
	}
	s.audit.Record(author, personaAuditAction, "", agentaudit.Fields(
		"outcome", outcome,
		"key", adj.Key,
		"from", adj.From,
		"to", adj.To,
		"evidence", adj.Evidence,
	))
}

func formatSuggestion(s persona.Suggestion) string {
	return fmt.Sprintf("set %s from %s to %s (evidence: %s)", s.Key, s.From, s.To, s.Evidence)
}

func formatAdjustment(a persona.Adjustment) string {
	return fmt.Sprintf("%s %s to %s, evidence: %s", a.Key, a.From, a.To, a.Evidence)
}
