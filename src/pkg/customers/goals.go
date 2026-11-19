package customers

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

// Goals and satisfaction. Both are inferred by the hive and decided by a
// person:
//
//   - inference only ever writes suggestions;
//   - a confirmed or manual value is never changed by inference;
//   - a suggestion a person rejected is remembered and not offered again;
//   - every change keeps who and when in its history.

// maxPendingGoals bounds the goal suggestions waiting on one profile.
const maxPendingGoals = 3

// fingerprintLen is the hex length of a suggestion fingerprint.
const fingerprintLen = 16

// GoalFingerprint identifies a goal by its normalised text, so the same
// suggestion worded with different spacing or case is recognised.
func GoalFingerprint(text string) string {
	norm := strings.ToLower(strings.Join(strings.Fields(text), " "))
	sum := sha256.Sum256([]byte(norm))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

// satisfactionFingerprint identifies a satisfaction suggestion by its level
// and the evidence behind it. New evidence makes a new suggestion.
func satisfactionFingerprint(level Level, evidence []string) string {
	refs := append([]string(nil), evidence...)
	sort.Strings(refs)
	sum := sha256.Sum256([]byte(string(level) + "\x00" + strings.Join(refs, "\x00")))
	return hex.EncodeToString(sum[:])[:fingerprintLen]
}

func goalIndex(goals []Goal, id string) int {
	for i, g := range goals {
		if g.ID == id {
			return i
		}
	}
	return -1
}

func cleanGoal(text string) (string, error) {
	text, err := cleanText(text, MaxGoalLen, false)
	if err != nil {
		return "", err
	}
	if text == "" {
		return "", fmt.Errorf("%w: a goal needs some text", ErrInvalid)
	}
	return text, nil
}

// AddGoal records a goal a person wrote. It is confirmed from the start.
func (s *Store) AddGoal(personID, text, by string) (Person, error) {
	text, err := cleanGoal(text)
	if err != nil {
		return Person{}, err
	}
	return s.changePerson(personID, func(p *Person) error {
		fp := GoalFingerprint(text)
		for _, g := range p.Goals {
			if GoalFingerprint(g.Text) == fp && g.State == StateConfirmed {
				return fmt.Errorf("%w: that goal is already on the profile", ErrConflict)
			}
		}
		if len(p.Goals) >= MaxGoals {
			return fmt.Errorf("%w: too many goals", ErrInvalid)
		}
		id, err := newID(goalPrefix)
		if err != nil {
			return err
		}
		p.Goals = append(p.Goals, Goal{
			ID: id, Text: text, State: StateConfirmed, Source: SourceManual,
			At: s.now().UTC(), By: by,
		})
		return nil
	})
}

// GoalSuggestion is one inferred goal with the artefact references that
// led to it.
type GoalSuggestion struct {
	Text     string
	Evidence []string
}

// SuggestGoals offers inferred goals for a person to decide on. It never
// changes a confirmed or manual goal. A suggestion is skipped when its text
// matches a goal already on the profile or one a person rejected, and the
// pending suggestions are replaced rather than piled up. It returns how many
// suggestions are now waiting that were not before.
func (s *Store) SuggestGoals(personID string, suggestions []GoalSuggestion) (int, error) {
	added := 0
	_, err := s.changePerson(personID, func(p *Person) error {
		standing := map[string]bool{}
		pending := map[string]Goal{}
		var kept []Goal
		for _, g := range p.Goals {
			if g.State == StateSuggested && g.Source == SourceAuto {
				pending[GoalFingerprint(g.Text)] = g
				continue
			}
			standing[GoalFingerprint(g.Text)] = true
			kept = append(kept, g)
		}
		now := s.now().UTC()
		offered := 0
		seen := map[string]bool{}
		for _, sg := range suggestions {
			if offered == maxPendingGoals || len(kept) >= MaxGoals {
				break
			}
			text, err := cleanGoal(sg.Text)
			if err != nil {
				continue
			}
			fp := GoalFingerprint(text)
			if standing[fp] || seen[fp] || contains(p.RejectedGoals, fp) {
				continue
			}
			seen[fp] = true
			if g, ok := pending[fp]; ok {
				// Still suggested: keep its id, refresh its evidence.
				g.Evidence = capEvidence(sg.Evidence)
				kept = append(kept, g)
				offered++
				continue
			}
			id, err := newID(goalPrefix)
			if err != nil {
				return err
			}
			kept = append(kept, Goal{
				ID: id, Text: text, State: StateSuggested, Source: SourceAuto,
				Evidence: capEvidence(sg.Evidence), At: now,
			})
			offered++
			added++
		}
		p.Goals = kept
		return nil
	})
	if err != nil {
		return 0, err
	}
	return added, nil
}

// ConfirmGoal accepts a suggested goal as it stands. Its source stays auto:
// the hive worded it, a person stands behind it.
func (s *Store) ConfirmGoal(personID, goalID, by string) (Person, error) {
	return s.changePerson(personID, func(p *Person) error {
		i := goalIndex(p.Goals, goalID)
		if i < 0 {
			return ErrNotFound
		}
		g := p.Goals[i]
		if g.State == StateConfirmed {
			return nil
		}
		g.History = appendHistory(g.History, Change{State: g.State, Source: g.Source, At: g.At, By: g.By})
		g.State, g.At, g.By = StateConfirmed, s.now().UTC(), by
		p.Goals[i] = g
		return nil
	})
}

// EditGoal rewrites a goal. The result is confirmed and manual, whatever it
// was before.
func (s *Store) EditGoal(personID, goalID, text, by string) (Person, error) {
	text, err := cleanGoal(text)
	if err != nil {
		return Person{}, err
	}
	return s.changePerson(personID, func(p *Person) error {
		i := goalIndex(p.Goals, goalID)
		if i < 0 {
			return ErrNotFound
		}
		g := p.Goals[i]
		g.History = appendHistory(g.History, Change{State: g.State, Source: g.Source, At: g.At, By: g.By})
		g.Text, g.State, g.Source, g.At, g.By = text, StateConfirmed, SourceManual, s.now().UTC(), by
		p.Goals[i] = g
		return nil
	})
}

// RejectGoal removes a goal and remembers its text, so inference does not
// offer it again. It serves both "reject this suggestion" and "remove this
// goal".
func (s *Store) RejectGoal(personID, goalID string) (Person, error) {
	return s.changePerson(personID, func(p *Person) error {
		i := goalIndex(p.Goals, goalID)
		if i < 0 {
			return ErrNotFound
		}
		p.RejectedGoals = remember(p.RejectedGoals, GoalFingerprint(p.Goals[i].Text))
		p.Goals = append(p.Goals[:i:i], p.Goals[i+1:]...)
		return nil
	})
}

// SetSatisfaction sets the level by hand. Any waiting suggestion is cleared:
// the person has answered.
func (s *Store) SetSatisfaction(personID string, level Level, by string) (Person, error) {
	if !ValidLevel(level) {
		return Person{}, fmt.Errorf("%w: unknown satisfaction level %q", ErrInvalid, level)
	}
	return s.changePerson(personID, func(p *Person) error {
		sat := p.Satisfaction
		sat.History = appendHistory(sat.History, Change{State: string(sat.Level), Source: sat.Source, At: sat.At, By: sat.By})
		sat.Level, sat.Source, sat.Evidence, sat.At, sat.By = level, SourceManual, nil, s.now().UTC(), by
		sat.Suggestion = nil
		p.Satisfaction = sat
		return nil
	})
}

// SuggestSatisfaction offers an inferred level with its evidence. It never
// changes the standing level. It is skipped when it only repeats the
// standing level, when a person rejected the same suggestion, or when the
// level is unknown (which says nothing). It reports whether a new suggestion
// is now waiting.
func (s *Store) SuggestSatisfaction(personID string, level Level, evidence []string) (bool, error) {
	if !ValidLevel(level) {
		return false, fmt.Errorf("%w: unknown satisfaction level %q", ErrInvalid, level)
	}
	refs := capEvidence(evidence)
	s.mu.Lock()
	p, ok := s.people[personID]
	s.mu.Unlock()
	if !ok {
		return false, ErrNotFound
	}
	if !satisfactionSuggestable(p.Satisfaction, level, refs) {
		return false, nil
	}
	offered := false
	_, err := s.changePerson(personID, func(p *Person) error {
		offered = satisfactionSuggestable(p.Satisfaction, level, refs)
		if offered {
			p.Satisfaction.Suggestion = &SatisfactionSuggestion{Level: level, Evidence: refs, At: s.now().UTC()}
		}
		return nil
	})
	return offered, err
}

func satisfactionSuggestable(sat Satisfaction, level Level, refs []string) bool {
	if level == LevelUnknown || len(refs) == 0 {
		return false
	}
	standing := sat.Level
	if standing == "" {
		standing = LevelUnknown
	}
	if level == standing {
		return false
	}
	fp := satisfactionFingerprint(level, refs)
	if contains(sat.Rejected, fp) {
		return false
	}
	if sat.Suggestion != nil && satisfactionFingerprint(sat.Suggestion.Level, sat.Suggestion.Evidence) == fp {
		return false
	}
	return true
}

// ConfirmSatisfaction accepts the waiting suggestion as the standing level.
func (s *Store) ConfirmSatisfaction(personID, by string) (Person, error) {
	return s.changePerson(personID, func(p *Person) error {
		sat := p.Satisfaction
		if sat.Suggestion == nil {
			return ErrNotFound
		}
		sat.History = appendHistory(sat.History, Change{State: string(sat.Level), Source: sat.Source, At: sat.At, By: sat.By})
		sat.Level, sat.Source, sat.Evidence = sat.Suggestion.Level, SourceAuto, sat.Suggestion.Evidence
		sat.At, sat.By = s.now().UTC(), by
		sat.Suggestion = nil
		p.Satisfaction = sat
		return nil
	})
}

// RejectSatisfaction turns the waiting suggestion down and remembers it.
func (s *Store) RejectSatisfaction(personID string) (Person, error) {
	return s.changePerson(personID, func(p *Person) error {
		sat := p.Satisfaction
		if sat.Suggestion == nil {
			return ErrNotFound
		}
		sat.Rejected = remember(sat.Rejected, satisfactionFingerprint(sat.Suggestion.Level, sat.Suggestion.Evidence))
		sat.Suggestion = nil
		p.Satisfaction = sat
		return nil
	})
}

// PruneEvidence drops evidence references whose artefact is gone from every
// goal and satisfaction value, so a profile does not keep a pointer to what
// retention deleted. A suggestion left with no evidence is withdrawn. It
// returns how many references were dropped.
func (s *Store) PruneEvidence(exists func(artefact string) bool) (int, error) {
	dropped := 0
	filter := func(refs []string) []string {
		var kept []string
		for _, r := range refs {
			if exists(r) {
				kept = append(kept, r)
			} else {
				dropped++
			}
		}
		return kept
	}
	err := s.change(func() error {
		for id, p := range s.people {
			before := dropped
			var goals []Goal
			for _, g := range p.Goals {
				had := len(g.Evidence) > 0
				g.Evidence = filter(g.Evidence)
				if had && len(g.Evidence) == 0 && g.State == StateSuggested {
					continue
				}
				goals = append(goals, g)
			}
			p.Goals = goals
			p.Satisfaction.Evidence = filter(p.Satisfaction.Evidence)
			if sg := p.Satisfaction.Suggestion; sg != nil {
				next := *sg
				next.Evidence = filter(sg.Evidence)
				p.Satisfaction.Suggestion = &next
				if len(next.Evidence) == 0 {
					p.Satisfaction.Suggestion = nil
				}
			}
			if dropped != before {
				s.people[id] = p
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return dropped, nil
}
