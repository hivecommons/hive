package customers

import (
	"fmt"
	"strings"
)

// "Same person?" hints. A hint is a question for a person to answer; it
// never links anything by itself. The two rules below only ever compare
// exact, normalised keys: a login to the local part of an address. A display
// name is never compared.

const (
	// minHintLocalLen is the shortest local part or login a hint rule will
	// compare. Shorter ones ("bo", "al") match too many unrelated people.
	minHintLocalLen = 4
	// hintReasonLocal is the reason a hint carries when an address's local
	// part equals a login.
	hintReasonLocal = "the address and the login start the same"
)

// genericLocals are address local parts that name a role, not a person.
var genericLocals = map[string]bool{
	"info": true, "admin": true, "root": true, "support": true, "noreply": true, "no-reply": true,
	"contact": true, "hello": true, "team": true, "mail": true, "sales": true, "office": true,
	"security": true, "postmaster": true, "webmaster": true, "billing": true, "help": true,
}

// localPart is the part of an address before the "@" with any "+tag"
// removed, lower-cased. It is "" for anything that is not an address.
func localPart(addr string) string {
	l, _, ok := strings.Cut(strings.ToLower(strings.TrimSpace(addr)), "@")
	if !ok {
		return ""
	}
	l, _, _ = strings.Cut(l, "+")
	return l
}

func hintableLocal(s string) bool { return len(s) >= minHintLocalLen && !genericLocals[s] }

// personLogins is the logins a customer is known by: GitHub logins and the
// dashboard user name.
func personLogins(p Person) []string {
	var out []string
	for _, i := range p.Identities {
		if i.Claimed {
			continue
		}
		switch i.Kind {
		case KindGitHub, KindHiveUser:
			out = append(out, i.Value)
		}
	}
	return out
}

// personLocals is the local parts of the addresses a customer holds.
func personLocals(p Person) []string {
	var out []string
	for _, i := range p.Identities {
		if i.Claimed {
			continue
		}
		if i.Kind == KindEmail || i.Kind == KindGitEmail {
			if l := localPart(i.Value); l != "" {
				out = append(out, l)
			}
		}
	}
	return out
}

// DeriveHints raises a "Same person?" question between a customer and a
// suggested person when the suggestion's address starts like one of the
// customer's logins, or the suggestion's login is the start of one of the
// customer's addresses. It returns how many new hints were raised. A pair a
// person said is different is not raised again.
func (s *Store) DeriveHints() (int, error) {
	added := 0
	err := s.change(func() error {
		added = 0
		for _, sg := range s.sugg {
			var local string
			switch sg.Kind {
			case KindEmail, KindGitEmail:
				local = localPart(sg.Value)
			case KindGitHub:
				local = strings.ToLower(sg.Value)
			default:
				continue
			}
			if !hintableLocal(local) {
				continue
			}
			for _, p := range s.people {
				match := false
				if sg.Kind == KindGitHub {
					for _, l := range personLocals(p) {
						match = match || l == local
					}
				} else {
					for _, l := range personLogins(p) {
						match = match || strings.ToLower(l) == local
					}
				}
				if !match {
					continue
				}
				before := len(s.hints)
				h, err := s.addHintLocked(Hint{Person: p.ID, Other: sg.ID, Reason: hintReasonLocal})
				if err != nil {
					return err
				}
				if h != nil && len(s.hints) > before {
					added++
				}
			}
		}
		return nil
	})
	return added, err
}

// SuggestionByID returns a suggestion by its id.
func (s *Store) SuggestionByID(id string) (Suggestion, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key, ok := s.suggestionKeyLocked(id)
	if !ok {
		return Suggestion{}, false
	}
	return cloneSuggestion(s.sugg[key]), true
}

// HintByID returns one open hint.
func (s *Store) HintByID(id string) (Hint, bool) {
	for _, h := range s.Hints("") {
		if h.ID == id {
			return h, true
		}
	}
	return Hint{}, false
}

// AbsorbSuggestion answers a hint "yes, the same person": the suggested
// identity becomes an identity of the customer, the suggestion is deleted
// and every hint about it is closed. The next sweep writes the edges. It is
// refused when the identity already belongs to someone.
func (s *Store) AbsorbSuggestion(personID, suggestionID, by string) (Person, error) {
	var out Person
	err := s.change(func() error {
		p, ok := s.people[personID]
		if !ok {
			return ErrNotFound
		}
		key, ok := s.suggestionKeyLocked(suggestionID)
		if !ok {
			return ErrNotFound
		}
		sg := s.sugg[key]
		if owner, taken := s.owner[key]; taken && owner != personID {
			return fmt.Errorf("%w: %s already belongs to another customer", ErrConflict, key)
		}
		if len(p.Identities) >= MaxIdentities {
			return fmt.Errorf("%w: too many identities", ErrInvalid)
		}
		now := s.now().UTC()
		if !p.HasIdentity(key) {
			p.Identities = append(p.Identities, Identity{
				Kind: sg.Kind, Value: sg.Value, Source: "same_person_hint", Confidence: 1, Manual: true, At: now, By: by,
			})
		}
		p.UpdatedAt = now
		s.people[personID] = p
		s.unpinLocked(PinUnlink, key, personID)
		delete(s.sugg, key)
		s.dropRefsLocked(suggestionID)
		out = clonePerson(p)
		return nil
	})
	return out, err
}
