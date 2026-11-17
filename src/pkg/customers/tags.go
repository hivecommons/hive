package customers

import (
	"fmt"
	"sort"
	"strings"
)

// CleanTag normalises a tag name: lower case, single spaces, and only
// letters, digits, space, dash and underscore.
func CleanTag(name string) (string, error) {
	name = strings.ToLower(strings.Join(strings.Fields(name), " "))
	if name == "" {
		return "", fmt.Errorf("%w: empty tag", ErrInvalid)
	}
	if len(name) > MaxTagLen {
		return "", fmt.Errorf("%w: tag longer than %d bytes", ErrInvalid, MaxTagLen)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == ' ', r == '-', r == '_':
		default:
			return "", fmt.Errorf("%w: tag may hold letters, digits, space, dash and underscore", ErrInvalid)
		}
	}
	return name, nil
}

func tagIndex(tags []Tag, name string) int {
	for i, t := range tags {
		if t.Name == name {
			return i
		}
	}
	return -1
}

// setTag writes one tag state, recording the previous one in its history.
// It reports whether anything changed.
func setTag(p *Person, name, state string, c Change) (bool, error) {
	i := tagIndex(p.Tags, name)
	if i < 0 {
		if len(p.Tags) >= MaxTags {
			return false, fmt.Errorf("%w: too many tags", ErrInvalid)
		}
		p.Tags = append(p.Tags, Tag{Name: name, State: state, Source: c.Source, Rule: c.Rule, At: c.At, By: c.By})
		return true, nil
	}
	t := p.Tags[i]
	if t.State == state && t.Source == c.Source && t.Rule == c.Rule {
		return false, nil
	}
	t.History = appendHistory(t.History, Change{State: t.State, Source: t.Source, Rule: t.Rule, At: t.At, By: t.By})
	t.State, t.Source, t.Rule, t.At, t.By = state, c.Source, c.Rule, c.At, c.By
	p.Tags[i] = t
	return true, nil
}

// SetTag turns a tag on or off by hand. A manual value always wins: the
// sweep leaves it alone until ResetTag.
func (s *Store) SetTag(personID, name string, on bool, by string) (Person, error) {
	name, err := CleanTag(name)
	if err != nil {
		return Person{}, err
	}
	state := TagOff
	if on {
		state = TagOn
	}
	return s.changePerson(personID, func(p *Person) error {
		_, err := setTag(p, name, state, Change{Source: SourceManual, At: s.now().UTC(), By: by})
		return err
	})
}

// ResetTag hands a tag back to the automatic rules ("Reset to automatic").
// The tag is off until the next sweep decides; a free-form tag, which no
// rule sets, simply stays off.
func (s *Store) ResetTag(personID, name, by string) (Person, error) {
	name, err := CleanTag(name)
	if err != nil {
		return Person{}, err
	}
	return s.changePerson(personID, func(p *Person) error {
		if tagIndex(p.Tags, name) < 0 {
			return ErrNotFound
		}
		_, err := setTag(p, name, TagOff, Change{Source: SourceAuto, Rule: "reset", At: s.now().UTC(), By: by})
		return err
	})
}

// ApplyAutoTags sets the automatic tags for a customer. auto maps a tag
// name to the rule that earned it. A tag with a manual value is never
// touched; an automatic tag that no rule earns any more is turned off. It
// reports whether anything changed, and saves only then.
func (s *Store) ApplyAutoTags(personID string, auto map[string]string) (bool, error) {
	names := make([]string, 0, len(auto))
	for n := range auto {
		names = append(names, n)
	}
	sort.Strings(names)
	s.mu.Lock()
	p, ok := s.people[personID]
	if !ok {
		s.mu.Unlock()
		return false, ErrNotFound
	}
	p = clonePerson(p)
	s.mu.Unlock()
	if !applyAutoTags(&p, names, auto, Change{}) {
		return false, nil
	}
	changed := false
	_, err := s.changePerson(personID, func(p *Person) error {
		changed = applyAutoTags(p, names, auto, Change{Source: SourceAuto, At: s.now().UTC()})
		return nil
	})
	return changed, err
}

// applyAutoTags is ApplyAutoTags on one person. names is auto's keys in a
// stable order.
func applyAutoTags(p *Person, names []string, auto map[string]string, c Change) bool {
	c.Source = SourceAuto
	changed := false
	for _, raw := range names {
		name, err := CleanTag(raw)
		if err != nil {
			continue
		}
		if i := tagIndex(p.Tags, name); i >= 0 && p.Tags[i].Source == SourceManual {
			continue
		}
		c.Rule = auto[raw]
		if ok, err := setTag(p, name, TagOn, c); err == nil && ok {
			changed = true
		}
	}
	for i := range p.Tags {
		t := p.Tags[i]
		if t.Source != SourceAuto || t.State != TagOn {
			continue
		}
		if _, earned := auto[t.Name]; earned {
			continue
		}
		c.Rule = ""
		if ok, err := setTag(p, t.Name, TagOff, c); err == nil && ok {
			changed = true
		}
	}
	return changed
}
