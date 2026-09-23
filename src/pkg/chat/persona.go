package chat

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/persona"
)

type pendingPersonaKey struct {
	backend string
	author  string
}

type pendingPersonaSetup struct {
	Answers []string
}

type localPersonaStore struct {
	mu      sync.RWMutex
	records map[string]persona.Record
}

func newLocalPersonaStore() *localPersonaStore {
	return &localPersonaStore{records: map[string]persona.Record{}}
}

func (s *localPersonaStore) GetPersona(_ context.Context, author string) (persona.Record, bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	record, ok := s.records[author]
	return record, ok, nil
}

func (s *localPersonaStore) PutPersona(_ context.Context, author string, record persona.Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[author] = record.Normalize()
	return nil
}

func (s *Service) registerPersonaCommand() {
	s.RegisterCommand("persona", func(ctx context.Context, args string) (string, error) {
		return s.cmdPersona(ctx, args)
	})
}

func (s *Service) cmdPersona(ctx context.Context, args string) (string, error) {
	author, _ := ctx.Value(commandAuthorContextKey{}).(string)
	if author == "" {
		return "❌ Persona commands require an authenticated chat author.", nil
	}
	fields := strings.Fields(strings.TrimSpace(args))
	if len(fields) == 0 {
		return "❌ " + personaUsage, nil
	}
	switch strings.ToLower(fields[0]) {
	case "setup":
		s.mu.Lock()
		s.pendingPersonas[s.pendingPersonaKey(author)] = &pendingPersonaSetup{}
		s.mu.Unlock()
		return personaSetupPrompt(0), nil
	case "show":
		record, ok, err := s.getPersona(ctx, author)
		if err != nil {
			return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
		}
		if !ok {
			return "ℹ️ No persona recorded. Run `!persona setup`.", nil
		}
		return formatPersona(record), nil
	case "set":
		if len(fields) < 3 {
			return "❌ Usage: `!persona set <depth|summary_length|notes> <value>`", nil
		}
		key := fields[1]
		value := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(args), fields[0]))
		value = strings.TrimSpace(strings.TrimPrefix(value, key))
		record, _, err := s.getPersona(ctx, author)
		if err != nil {
			return fmt.Sprintf("❌ Failed to load persona: %s", err), nil
		}
		record, err = record.Set(key, value)
		if err != nil {
			return "❌ " + err.Error(), nil
		}
		if err := s.putPersona(ctx, author, record); err != nil {
			return fmt.Sprintf("❌ Failed to save persona: %s", err), nil
		}
		return "✅ Persona updated.\n" + formatPersona(record), nil
	case "suggestions":
		return s.cmdPersonaSuggestions(ctx, author)
	case "accept":
		if len(fields) < 2 {
			return "❌ Usage: `!persona accept <n>`", nil
		}
		return s.cmdPersonaAccept(ctx, author, fields[1])
	case "reject":
		return s.cmdPersonaReject(ctx, author)
	case "undo":
		return s.cmdPersonaUndo(ctx, author)
	case "pin":
		return s.cmdPersonaPin(ctx, author, true)
	case "unpin":
		return s.cmdPersonaPin(ctx, author, false)
	default:
		return "❌ Unknown persona subcommand. " + personaUsage, nil
	}
}

const personaUsage = "Usage: `!persona setup` | `show` | `set <depth|summary_length|notes> <value>` | `suggestions` | `accept <n>` | `reject` | `undo` | `pin` | `unpin`"

func (s *Service) handlePendingPersonaReply(ctx context.Context, msg Message, content string) bool {
	if len(s.allowedUsers) == 0 {
		return false
	}
	if _, ok := s.allowedUsers[msg.AuthorID]; !ok {
		return false
	}
	key := s.pendingPersonaKey(msg.AuthorID)
	s.mu.Lock()
	pending := s.pendingPersonas[key]
	if pending == nil {
		s.mu.Unlock()
		return false
	}
	pending.Answers = append(pending.Answers, strings.TrimSpace(content))
	answers := append([]string(nil), pending.Answers...)
	if len(answers) >= 3 {
		delete(s.pendingPersonas, key)
	}
	s.mu.Unlock()

	if len(answers) < 3 {
		s.enqueue("✅ Recorded.\n" + personaSetupPrompt(len(answers)))
		return true
	}
	record := persona.Record{
		Depth:         persona.NormalizeDepth(answers[0]),
		SummaryLength: persona.NormalizeSummaryLength(answers[1]),
		Notes:         answers[2],
	}.Normalize()
	if err := s.putPersona(ctx, msg.AuthorID, record); err != nil {
		s.enqueue(fmt.Sprintf("❌ Failed to save persona: %s", err))
		return true
	}
	s.enqueue("✅ Persona saved.\n" + formatPersona(record))
	return true
}

func (s *Service) getPersona(ctx context.Context, author string) (persona.Record, bool, error) {
	if s.personaStore == nil {
		return persona.Record{}, false, nil
	}
	return s.personaStore.GetPersona(ctx, author)
}

func (s *Service) putPersona(ctx context.Context, author string, record persona.Record) error {
	if s.personaStore == nil {
		return fmt.Errorf("persona store unavailable")
	}
	return s.personaStore.PutPersona(ctx, author, record.Normalize())
}

func (s *Service) pendingPersonaKey(author string) pendingPersonaKey {
	backend := ""
	if s.backend != nil {
		backend = s.backend.Name()
	}
	return pendingPersonaKey{backend: backend, author: author}
}

func personaSetupPrompt(index int) string {
	switch index {
	case 0:
		return "Persona setup 1/3: prefer `outcomes` or `technical` run summaries?"
	case 1:
		return "Persona setup 2/3: summary length `short`, `standard`, or `detailed`?"
	default:
		return "Persona setup 3/3: any free-text communication notes?"
	}
}

func formatPersona(record persona.Record) string {
	record = record.Normalize()
	lines := []string{
		"**Persona**",
		"- depth: " + record.Depth,
		"- summary_length: " + record.SummaryLength,
	}
	if record.Notes != "" {
		lines = append(lines, "- notes: "+record.Notes)
	}
	if record.Pinned {
		lines = append(lines, "- pinned: yes (learning paused; `!persona unpin` to resume)")
	}
	if record.Learning != nil && record.Learning.LastAdjustment != nil {
		lines = append(lines, "- last adjustment: "+formatAdjustment(*record.Learning.LastAdjustment)+" (`!persona undo` reverts and pins)")
	}
	if pending := record.Suggestions(); len(pending) > 0 {
		lines = append(lines, fmt.Sprintf("- suggestions: %d pending (`!persona suggestions`)", len(pending)))
	}
	return strings.Join(lines, "\n")
}
