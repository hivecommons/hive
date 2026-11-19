package customers

import (
	"fmt"
	"sort"
	"time"
)

// Identity resolution. Automatic links happen only on exact keys handed in
// by a trusted source (AutoLink). Everything a person decides is pinned, and
// the automatic paths read the pins first. A display name never links,
// merges or creates a suggestion.

// AddIdentity links an identity to a customer by hand. When the identity
// already belongs to someone else nothing is linked: a "Same person?" hint
// is stored on the customer and returned, for someone who can see both
// profiles to answer. A hive_user identity cannot be added by hand.
func (s *Store) AddIdentity(personID string, raw Identity, by string) (Person, *Hint, error) {
	id, err := cleanIdentity(raw)
	if err != nil {
		return Person{}, nil, err
	}
	if id.Kind == KindHiveUser {
		return Person{}, nil, fmt.Errorf("%w: a hive user identity cannot be added by hand", ErrInvalid)
	}
	var out Person
	var hint *Hint
	err = s.change(func() error {
		p, ok := s.people[personID]
		if !ok {
			return ErrNotFound
		}
		if p.HasIdentity(id.Key()) {
			out = clonePerson(p)
			return nil
		}
		claim := p.identityIndex(id.Key())
		if claim < 0 && len(p.Identities) >= MaxIdentities {
			return fmt.Errorf("%w: too many identities", ErrInvalid)
		}
		now := s.now().UTC()
		if id.Kind != KindName {
			if other, taken := s.owner[id.Key()]; taken {
				claimed := id
				claimed.At, claimed.By = now, by
				h, err := s.addHintLocked(Hint{Person: personID, Other: other, Identity: &claimed, Reason: "claimed identity"})
				if err != nil {
					return err
				}
				hint = h
				out = clonePerson(p)
				return nil
			}
			s.adoptKeyLocked(id.Key())
			s.unpinLocked(PinUnlink, id.Key(), personID)
		}
		if claim >= 0 {
			// Confirming a claim replaces it with the real identity.
			p.dropIdentity(claim)
		}
		id.Source, id.Confidence, id.Manual, id.At, id.By = "manual", 1, true, now, by
		p.Identities = append(p.Identities, id)
		p.UpdatedAt = now
		s.people[personID] = p
		out = clonePerson(p)
		return nil
	})
	return out, hint, err
}

// AddClaim records an identity a read-only user says is theirs. It is shown
// on their profile, marked unconfirmed, and nothing else: it is not a key,
// resolves nobody and produces no edges, whether or not the identity belongs
// to someone else, so the answer tells the claimant nothing about other
// profiles. Someone with the full view confirms it with AddIdentity. A
// hive_user identity cannot be claimed.
func (s *Store) AddClaim(personID string, raw Identity, by string) (Person, error) {
	id, err := cleanIdentity(raw)
	if err != nil {
		return Person{}, err
	}
	if id.Kind == KindHiveUser {
		return Person{}, fmt.Errorf("%w: a hive user identity cannot be added by hand", ErrInvalid)
	}
	return s.changePerson(personID, func(p *Person) error {
		if p.identityIndex(id.Key()) >= 0 {
			return nil
		}
		if len(p.Identities) >= MaxIdentities {
			return fmt.Errorf("%w: too many identities", ErrInvalid)
		}
		id.Source, id.Manual, id.Claimed, id.At, id.By = "claimed", true, id.Kind != KindName, s.now().UTC(), by
		p.Identities = append(p.Identities, id)
		return nil
	})
}

// RemoveIdentity unlinks an identity from a customer and pins the decision,
// so automatic resolution does not put it back. The removed identity is
// returned; the caller drops the edges it produced. A hive_user identity
// cannot be removed.
func (s *Store) RemoveIdentity(personID, key, by string) (Person, Identity, error) {
	var out Person
	var removed Identity
	err := s.change(func() error {
		p, ok := s.people[personID]
		if !ok {
			return ErrNotFound
		}
		kept := p.Identities[:0:0]
		found := false
		for _, i := range p.Identities {
			if !found && i.Key() == key {
				found, removed = true, i
				continue
			}
			kept = append(kept, i)
		}
		if !found {
			return ErrNotFound
		}
		if removed.Kind == KindHiveUser {
			return fmt.Errorf("%w: a hive user identity cannot be removed", ErrInvalid)
		}
		now := s.now().UTC()
		p.Identities = kept
		p.UpdatedAt = now
		s.people[personID] = p
		if removed.Kind != KindName && !removed.Claimed {
			s.pinLocked(Pin{Kind: PinUnlink, Identity: key, Person: personID, At: now, By: by})
		}
		out = clonePerson(p)
		return nil
	})
	return out, removed, err
}

// AutoLink links an identity to a customer on behalf of a trusted source
// (a proven calendar email, a GitHub sign-in, a DCO match). It reports
// whether the link was made. It is refused, without error, when a person
// pinned the identity off this customer or the identity was forgotten; when
// the identity belongs to another customer it raises a hint instead. A name
// is never linked this way.
func (s *Store) AutoLink(personID string, raw Identity) (bool, error) {
	id, err := cleanIdentity(raw)
	if err != nil {
		return false, err
	}
	if id.Kind == KindName || id.Kind == KindHiveUser {
		return false, nil
	}
	linked := false
	err = s.change(func() error {
		p, ok := s.people[personID]
		if !ok {
			return ErrNotFound
		}
		key := id.Key()
		if p.HasIdentity(key) || s.pinnedLocked(PinUnlink, key, personID) || s.tombs[s.tombstoneHash(key)] {
			return nil
		}
		claim := p.identityIndex(key)
		if claim < 0 && len(p.Identities) >= MaxIdentities {
			return nil
		}
		if other, taken := s.owner[key]; taken {
			_, err := s.addHintLocked(Hint{Person: personID, Other: other, Reason: "shares " + string(id.Kind)})
			return err
		}
		delete(s.sugg, key)
		if claim >= 0 {
			// A trusted source proved what the person claimed.
			p.dropIdentity(claim)
		}
		now := s.now().UTC()
		id.Manual, id.At, id.By = false, now, ""
		if id.Confidence == 0 {
			id.Confidence = 1
		}
		p.Identities = append(p.Identities, id)
		p.UpdatedAt = now
		s.people[personID] = p
		linked = true
		return nil
	})
	return linked, err
}

func (s *Store) pinnedLocked(kind, identity, person string) bool {
	for _, p := range s.pins {
		if p.Kind == kind && p.Identity == identity && p.Person == person {
			return true
		}
	}
	return false
}

func (s *Store) pinLocked(pin Pin) {
	for _, p := range s.pins {
		if p.Kind == pin.Kind && p.Identity == pin.Identity && p.Person == pin.Person && p.Other == pin.Other {
			return
		}
	}
	s.pins = append(s.pins, pin)
}

func (s *Store) unpinLocked(kind, identity, person string) {
	kept := s.pins[:0:0]
	for _, p := range s.pins {
		if p.Kind == kind && p.Identity == identity && p.Person == person {
			continue
		}
		kept = append(kept, p)
	}
	s.pins = kept
}

// distinctLocked reports whether a person said a and b are different.
func (s *Store) distinctLocked(a, b string) bool {
	for _, p := range s.pins {
		if p.Kind != PinDistinct {
			continue
		}
		if (p.Person == a && p.Other == b) || (p.Person == b && p.Other == a) {
			return true
		}
	}
	return false
}

// Pins returns every pinned decision.
func (s *Store) Pins() []Pin {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Pin(nil), s.pins...)
}

// addHintLocked stores a hint unless the pair was pinned distinct or the
// same hint is already waiting. It returns the stored (or existing) hint,
// or nil when the pair is pinned.
func (s *Store) addHintLocked(h Hint) (*Hint, error) {
	if h.Other != "" && s.distinctLocked(h.Person, h.Other) {
		return nil, nil
	}
	for i := range s.hints {
		e := s.hints[i]
		if e.Person != h.Person || e.Other != h.Other {
			continue
		}
		if (e.Identity == nil) != (h.Identity == nil) {
			continue
		}
		if e.Identity != nil && e.Identity.Key() != h.Identity.Key() {
			continue
		}
		out := e
		return &out, nil
	}
	id, err := newID(hintPrefix)
	if err != nil {
		return nil, err
	}
	h.ID = id
	if h.At.IsZero() {
		h.At = s.now().UTC()
	}
	s.hints = append(s.hints, h)
	out := h
	return &out, nil
}

// AddHint raises a "Same person?" question between a customer and another
// customer or a suggestion. It links nothing.
func (s *Store) AddHint(person, other, reason string) error {
	return s.change(func() error {
		if _, ok := s.people[person]; !ok {
			return ErrNotFound
		}
		_, err := s.addHintLocked(Hint{Person: person, Other: other, Reason: reason})
		return err
	})
}

// Hints returns the open hints that involve a customer, oldest first. An
// empty id returns every hint.
func (s *Store) Hints(person string) []Hint {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []Hint
	for _, h := range s.hints {
		if person == "" || h.Person == person || h.Other == person {
			c := h
			if h.Identity != nil {
				i := *h.Identity
				c.Identity = &i
			}
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

// ResolveHint closes a hint and returns it. With same false the pair is
// pinned distinct so the question is not asked again. With same true the
// caller acts on the answer (Merge, or AddIdentity after the other profile
// gave the identity up); the store links nothing by itself.
func (s *Store) ResolveHint(id string, same bool, by string) (Hint, error) {
	var out Hint
	err := s.change(func() error {
		kept := s.hints[:0:0]
		found := false
		for _, h := range s.hints {
			if h.ID == id {
				found, out = true, h
				continue
			}
			kept = append(kept, h)
		}
		if !found {
			return ErrNotFound
		}
		s.hints = kept
		if !same && out.Other != "" {
			s.pinLocked(Pin{Kind: PinDistinct, Person: out.Person, Other: out.Other, At: s.now().UTC(), By: by})
		}
		return nil
	})
	return out, err
}

// Observation is one sighting of an identity next to an artefact.
type Observation struct {
	Kind      IdentityKind
	Value     string
	Predicate string
	// Artefact is a reference, never content.
	Artefact string
	At       time.Time
}

// Observe takes one sweep's sightings in one save. For each observation
// whose identity belongs to a customer it returns that customer's id, keyed
// by the observation's index, so the caller can write the edge. An identity
// nobody holds becomes (or updates) a suggestion: a key, counters and the
// MaxSuggestionRef most recent evidence references. A forgotten identity
// and a display name produce nothing.
//
// A suggestion's counters and evidence are what this batch saw, not a
// running total: a sweep hands in everything it currently sees, so seeing
// the same artefact on every sweep does not inflate a count. A suggestion
// the batch does not mention is left as it is; ExpireSuggestions removes it
// once its evidence is gone.
func (s *Store) Observe(obs []Observation) (map[int]string, error) {
	owners := map[int]string{}
	type sighting struct {
		artefact string
		at       time.Time
	}
	type seen struct {
		kind      IdentityKind
		value     string
		counts    map[string]int
		sightings []sighting
		dedupe    map[string]bool
		first     time.Time
		last      time.Time
	}
	err := s.change(func() error {
		batch := map[string]*seen{}
		var order []string
		for n, o := range obs {
			if o.Kind == KindName || !identityKinds[o.Kind] {
				continue
			}
			value := NormalizeIdentity(o.Kind, o.Value)
			if value == "" || len(value) > MaxIdentityLen {
				continue
			}
			key := IdentityKey(o.Kind, value)
			if pid, ok := s.owner[key]; ok {
				owners[n] = pid
				continue
			}
			if o.Kind == KindHiveUser {
				// A dashboard user is a customer through the access list,
				// never a suggestion.
				continue
			}
			if s.tombs[s.tombstoneHash(key)] {
				continue
			}
			at := o.At.UTC()
			if o.At.IsZero() {
				at = s.now().UTC()
			}
			b, ok := batch[key]
			if !ok {
				b = &seen{kind: o.Kind, value: value, counts: map[string]int{}, dedupe: map[string]bool{}, first: at, last: at}
				batch[key] = b
				order = append(order, key)
			}
			dk := o.Predicate + keyDelim + o.Artefact
			if b.dedupe[dk] {
				continue
			}
			b.dedupe[dk] = true
			if o.Predicate != "" {
				b.counts[o.Predicate]++
			}
			if o.Artefact != "" {
				b.sightings = append(b.sightings, sighting{artefact: o.Artefact, at: at})
			}
			if at.Before(b.first) {
				b.first = at
			}
			if at.After(b.last) {
				b.last = at
			}
		}
		for _, key := range order {
			b := batch[key]
			sg, ok := s.sugg[key]
			if !ok {
				id, err := newID(suggestionPrefix)
				if err != nil {
					return err
				}
				sg = Suggestion{ID: id, Kind: b.kind, Value: b.value, FirstSeen: b.first}
			}
			if b.first.Before(sg.FirstSeen) {
				sg.FirstSeen = b.first
			}
			sg.LastSeen = b.last
			sg.Counts = b.counts
			sort.SliceStable(b.sightings, func(i, j int) bool { return b.sightings[i].at.After(b.sightings[j].at) })
			sg.Evidence = nil
			for _, st := range b.sightings {
				if len(sg.Evidence) == MaxSuggestionRef {
					break
				}
				if !contains(sg.Evidence, st.artefact) {
					sg.Evidence = append(sg.Evidence, st.artefact)
				}
			}
			s.sugg[key] = sg
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return owners, nil
}

// Suggestions returns the suggested people, most recently seen first.
func (s *Store) Suggestions() []Suggestion {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Suggestion, 0, len(s.sugg))
	for _, sg := range s.sugg {
		out = append(out, cloneSuggestion(sg))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].LastSeen.Equal(out[j].LastSeen) {
			return out[i].LastSeen.After(out[j].LastSeen)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Store) suggestionKeyLocked(id string) (string, bool) {
	for key, sg := range s.sugg {
		if sg.ID == id {
			return key, true
		}
	}
	return "", false
}

// Accept turns a suggestion into a customer. This is the human decision
// that lets the hive build a profile of someone who never signed up.
func (s *Store) Accept(suggestionID, by string) (Person, error) {
	var out Person
	err := s.change(func() error {
		key, ok := s.suggestionKeyLocked(suggestionID)
		if !ok {
			return ErrNotFound
		}
		sg := s.sugg[key]
		pid, err := newID(personPrefix)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		p := Person{
			ID: pid, Name: sg.Value, Status: StatusCustomer, Origin: OriginAccepted,
			Identities: []Identity{{
				Kind: sg.Kind, Value: sg.Value, Source: "accepted", Confidence: 1,
				Manual: true, At: now, By: by,
			}},
			Satisfaction: Satisfaction{Level: LevelUnknown},
			CreatedAt:    now, UpdatedAt: now,
		}
		delete(s.sugg, key)
		s.dropRefsLocked(suggestionID)
		s.people[pid] = p
		out = clonePerson(p)
		return nil
	})
	return out, err
}

// Dismiss turns a suggestion down. The suggestion is deleted and its key is
// tombstoned, so it is not offered again and the hive keeps no readable
// trace of the person.
func (s *Store) Dismiss(suggestionID string) error {
	return s.change(func() error {
		key, ok := s.suggestionKeyLocked(suggestionID)
		if !ok {
			return ErrNotFound
		}
		delete(s.sugg, key)
		s.tombs[s.tombstoneHash(key)] = true
		s.dropRefsLocked(suggestionID)
		return nil
	})
}

// ExpireSuggestions drops evidence whose artefact is gone and deletes every
// suggestion left with none: a suggestion does not outlive its evidence. It
// returns how many suggestions were deleted.
func (s *Store) ExpireSuggestions(exists func(artefact string) bool) (int, error) {
	removed := 0
	err := s.change(func() error {
		for key, sg := range s.sugg {
			kept := sg.Evidence[:0:0]
			for _, ref := range sg.Evidence {
				if exists(ref) {
					kept = append(kept, ref)
				}
			}
			if len(kept) == 0 {
				delete(s.sugg, key)
				s.dropRefsLocked(sg.ID)
				removed++
				continue
			}
			sg.Evidence = kept
			s.sugg[key] = sg
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// ExpireSuggestionsWhere deletes the suggestions drop names, and every
// suggestion whose identity a person owns: a suggestion is for someone
// nobody holds. Nothing is tombstoned, so a suggestion a later rule allows
// can come back. It returns how many were deleted.
func (s *Store) ExpireSuggestionsWhere(drop func(Suggestion) bool) (int, error) {
	removed := 0
	err := s.change(func() error {
		for key, sg := range s.sugg {
			if _, owned := s.owner[key]; !owned && (drop == nil || !drop(sg)) {
				continue
			}
			delete(s.sugg, key)
			s.dropRefsLocked(sg.ID)
			removed++
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

// Merge folds src into dst and deletes src. The merge is recorded on dst so
// Unmerge can undo it exactly. Two dashboard users are never merged: each
// keeps their own "me".
func (s *Store) Merge(dstID, srcID, by string) (Person, error) {
	if dstID == srcID {
		return Person{}, fmt.Errorf("%w: cannot merge a customer into itself", ErrInvalid)
	}
	var out Person
	err := s.change(func() error {
		dst, ok := s.people[dstID]
		if !ok {
			return ErrNotFound
		}
		src, ok := s.people[srcID]
		if !ok {
			return ErrNotFound
		}
		if dst.Identity(KindHiveUser) != "" && src.Identity(KindHiveUser) != "" {
			return fmt.Errorf("%w: two dashboard users cannot be merged", ErrConflict)
		}
		now := s.now().UTC()
		rec := MergeRecord{Person: clonePerson(src), At: now, By: by}
		for _, i := range src.Identities {
			if dst.identityIndex(i.Key()) < 0 && len(dst.Identities) < MaxIdentities {
				dst.Identities = append(dst.Identities, i)
				rec.Identities = append(rec.Identities, i.Key())
			}
		}
		for _, t := range src.Tags {
			if tagIndex(dst.Tags, t.Name) < 0 && len(dst.Tags) < MaxTags {
				dst.Tags = append(dst.Tags, t)
				rec.Tags = append(rec.Tags, t.Name)
			}
		}
		for _, g := range src.Goals {
			if goalIndex(dst.Goals, g.ID) < 0 && len(dst.Goals) < MaxGoals {
				dst.Goals = append(dst.Goals, g)
				rec.Goals = append(rec.Goals, g.ID)
			}
		}
		dst.MergedFrom = append(dst.MergedFrom, rec)
		if dst.Origin != OriginHiveUser && src.Origin == OriginHiveUser {
			dst.Origin = OriginHiveUser
		}
		dst.UpdatedAt = now
		s.people[dstID] = dst
		delete(s.people, srcID)
		s.dropRefsLocked(srcID)
		out = clonePerson(dst)
		return nil
	})
	return out, err
}

// Unmerge undoes a merge: the merged customer comes back as it stood, and
// dst gives up exactly the identities, tags and goals it gained. It returns
// dst and the restored customer; the caller moves the restored customer's
// edges back by their via identity.
func (s *Store) Unmerge(dstID, srcID string) (Person, Person, error) {
	var outDst, outSrc Person
	err := s.change(func() error {
		dst, ok := s.people[dstID]
		if !ok {
			return ErrNotFound
		}
		at := -1
		for i, m := range dst.MergedFrom {
			if m.Person.ID == srcID {
				at = i
			}
		}
		if at < 0 {
			return ErrNotFound
		}
		if _, exists := s.people[srcID]; exists {
			return fmt.Errorf("%w: %s exists again", ErrConflict, srcID)
		}
		rec := dst.MergedFrom[at]
		src := rec.Person
		kept := dst.Identities[:0:0]
		for _, i := range dst.Identities {
			if !contains(rec.Identities, i.Key()) {
				kept = append(kept, i)
			}
		}
		dst.Identities = kept
		tags := dst.Tags[:0:0]
		for _, t := range dst.Tags {
			if !contains(rec.Tags, t.Name) {
				tags = append(tags, t)
			}
		}
		dst.Tags = tags
		goals := dst.Goals[:0:0]
		for _, g := range dst.Goals {
			if !contains(rec.Goals, g.ID) {
				goals = append(goals, g)
			}
		}
		dst.Goals = goals
		dst.MergedFrom = append(dst.MergedFrom[:at:at], dst.MergedFrom[at+1:]...)
		if dst.Identity(KindHiveUser) == "" && dst.Origin == OriginHiveUser {
			dst.Origin = OriginManual
		}
		now := s.now().UTC()
		dst.UpdatedAt, src.UpdatedAt = now, now
		s.people[dstID] = dst
		s.people[srcID] = src
		s.pinLocked(Pin{Kind: PinDistinct, Person: dstID, Other: srcID, At: now})
		outDst, outSrc = clonePerson(dst), clonePerson(src)
		return nil
	})
	return outDst, outSrc, err
}

// Split moves the named identities off a customer onto a new one and pins
// the two as distinct. It returns the original and the new customer; the
// caller moves the edges those identities produced. A hive_user identity
// stays where it is.
func (s *Store) Split(personID string, keys []string, name, by string) (Person, Person, error) {
	name, err := cleanText(name, MaxNameLen, false)
	if err != nil {
		return Person{}, Person{}, err
	}
	var outOrig, outNew Person
	err = s.change(func() error {
		p, ok := s.people[personID]
		if !ok {
			return ErrNotFound
		}
		want := map[string]bool{}
		for _, k := range keys {
			want[k] = true
		}
		var moved, kept []Identity
		for _, i := range p.Identities {
			if want[i.Key()] {
				if i.Kind == KindHiveUser {
					return fmt.Errorf("%w: a hive user identity cannot be split off", ErrInvalid)
				}
				moved = append(moved, i)
				continue
			}
			kept = append(kept, i)
		}
		if len(moved) == 0 {
			return fmt.Errorf("%w: no matching identity to split off", ErrInvalid)
		}
		if len(kept) == 0 {
			return fmt.Errorf("%w: a split must leave an identity behind", ErrInvalid)
		}
		pid, err := newID(personPrefix)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		if name == "" {
			name = moved[0].Value
		}
		np := Person{
			ID: pid, Name: name, Status: StatusCustomer, Origin: OriginManual,
			Identities:   moved,
			Satisfaction: Satisfaction{Level: LevelUnknown},
			CreatedAt:    now, UpdatedAt: now,
		}
		p.Identities = kept
		p.UpdatedAt = now
		s.people[personID] = p
		s.people[pid] = np
		for _, i := range moved {
			if i.Kind != KindName && !i.Claimed {
				s.pinLocked(Pin{Kind: PinUnlink, Identity: i.Key(), Person: personID, At: now, By: by})
			}
		}
		s.pinLocked(Pin{Kind: PinDistinct, Person: personID, Other: pid, At: now, By: by})
		outOrig, outNew = clonePerson(p), clonePerson(np)
		return nil
	})
	return outOrig, outNew, err
}
