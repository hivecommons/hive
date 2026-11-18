package customers

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
)

const (
	// FileName is the people file inside the customers data directory.
	FileName = "people.json"
	// fileVersion is the people.json format version.
	fileVersion   = 1
	storeDirMode  = 0o700
	storeFileMode = 0o600
	// idBytes is the random part of an id: 6 bytes, 12 hex characters.
	idBytes = 6
	// saltBytes is the per-hive tombstone salt.
	saltBytes = 16
	// maxRejected bounds the remembered rejected-suggestion fingerprints.
	maxRejected = 50
)

// Id prefixes. An id is never derived from personal data.
const (
	personPrefix     = "p_"
	suggestionPrefix = "s_"
	goalPrefix       = "g_"
	hintPrefix       = "h_"
)

type storeFile struct {
	Version     int          `json:"version"`
	Salt        string       `json:"salt"`
	People      []Person     `json:"people"`
	Suggestions []Suggestion `json:"suggestions,omitempty"`
	Pins        []Pin        `json:"pins,omitempty"`
	Hints       []Hint       `json:"hints,omitempty"`
	// Tombstones are salted hashes of forgotten and dismissed identity
	// keys. They hold no readable identity.
	Tombstones []string `json:"tombstones,omitempty"`
}

// Store is the truth for people: one JSON file, rewritten atomically on
// every change. Safe for concurrent use. Every value it returns is a copy.
type Store struct {
	path string
	now  func() time.Time

	mu     sync.Mutex
	salt   string
	people map[string]Person     // by id
	sugg   map[string]Suggestion // by identity key
	pins   []Pin
	hints  []Hint
	tombs  map[string]bool
	owner  map[string]string // identity key -> person id
	// saved is the bytes last written, so an unchanged store is not
	// rewritten.
	saved []byte
}

// Open opens <dir>/people.json, creating dir if needed. A missing file is
// an empty store; an unreadable one is an error, so a damaged file is never
// silently replaced.
func Open(dir string, now func() time.Time) (*Store, error) {
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return nil, fmt.Errorf("customers: creating dir: %w", err)
	}
	s := &Store{
		path:   filepath.Join(dir, FileName),
		now:    now,
		people: map[string]Person{},
		sugg:   map[string]Suggestion{},
		tombs:  map[string]bool{},
		owner:  map[string]string{},
	}
	data, err := os.ReadFile(s.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		salt, serr := randomHex(saltBytes)
		if serr != nil {
			return nil, serr
		}
		s.salt = salt
		return s, nil
	case err != nil:
		return nil, fmt.Errorf("customers: reading %s: %w", FileName, err)
	}
	var f storeFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("customers: decoding %s: %w", FileName, err)
	}
	s.salt = f.Salt
	if s.salt == "" {
		salt, serr := randomHex(saltBytes)
		if serr != nil {
			return nil, serr
		}
		s.salt = salt
	}
	for _, p := range f.People {
		if p.ID != "" {
			s.people[p.ID] = p
		}
	}
	for _, sg := range f.Suggestions {
		if sg.ID != "" {
			s.sugg[IdentityKey(sg.Kind, sg.Value)] = sg
		}
	}
	s.pins = f.Pins
	s.hints = f.Hints
	for _, t := range f.Tombstones {
		s.tombs[t] = true
	}
	s.reindexLocked()
	return s, nil
}

// Path is the people file.
func (s *Store) Path() string { return s.path }

func randomHex(n int) (string, error) {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("customers: random id: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func newID(prefix string) (string, error) {
	h, err := randomHex(idBytes)
	if err != nil {
		return "", err
	}
	return prefix + h, nil
}

// reindexLocked rebuilds the identity-key index; s.mu is held (or the store
// is not yet shared).
func (s *Store) reindexLocked() {
	s.owner = map[string]string{}
	for id, p := range s.people {
		for _, i := range p.Identities {
			if i.Kind == KindName || i.Claimed {
				continue
			}
			s.owner[i.Key()] = id
		}
	}
}

// snapshot is the mutable state a failed save rolls back to.
type snapshot struct {
	people map[string]Person
	sugg   map[string]Suggestion
	pins   []Pin
	hints  []Hint
	tombs  map[string]bool
}

func (s *Store) snapshotLocked() snapshot {
	sn := snapshot{
		people: make(map[string]Person, len(s.people)),
		sugg:   make(map[string]Suggestion, len(s.sugg)),
		pins:   append([]Pin(nil), s.pins...),
		hints:  append([]Hint(nil), s.hints...),
		tombs:  make(map[string]bool, len(s.tombs)),
	}
	for k, v := range s.people {
		sn.people[k] = clonePerson(v)
	}
	for k, v := range s.sugg {
		sn.sugg[k] = cloneSuggestion(v)
	}
	for k, v := range s.tombs {
		sn.tombs[k] = v
	}
	return sn
}

func (s *Store) restoreLocked(sn snapshot) {
	s.people, s.sugg, s.pins, s.hints, s.tombs = sn.people, sn.sugg, sn.pins, sn.hints, sn.tombs
	s.reindexLocked()
}

// change runs fn under the lock and saves. If fn or the save fails, the
// store is left exactly as it was.
func (s *Store) change(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	sn := s.snapshotLocked()
	if err := fn(); err != nil {
		s.restoreLocked(sn)
		return err
	}
	s.reindexLocked()
	if err := s.saveLocked(); err != nil {
		s.restoreLocked(sn)
		return err
	}
	return nil
}

// changePerson applies fn to one person and saves.
func (s *Store) changePerson(id string, fn func(p *Person) error) (Person, error) {
	var out Person
	err := s.change(func() error {
		p, ok := s.people[id]
		if !ok {
			return ErrNotFound
		}
		if err := fn(&p); err != nil {
			return err
		}
		p.UpdatedAt = s.now().UTC()
		s.people[id] = p
		out = clonePerson(p)
		return nil
	})
	return out, err
}

func (s *Store) saveLocked() error {
	f := storeFile{Version: fileVersion, Salt: s.salt, Pins: s.pins, Hints: s.hints}
	for _, p := range s.people {
		f.People = append(f.People, p)
	}
	sort.Slice(f.People, func(i, j int) bool { return f.People[i].ID < f.People[j].ID })
	for _, sg := range s.sugg {
		f.Suggestions = append(f.Suggestions, sg)
	}
	sort.Slice(f.Suggestions, func(i, j int) bool { return f.Suggestions[i].ID < f.Suggestions[j].ID })
	for t := range s.tombs {
		f.Tombstones = append(f.Tombstones, t)
	}
	sort.Strings(f.Tombstones)
	data, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	if bytes.Equal(data, s.saved) {
		// Nothing changed: a sweep that found nothing new must not
		// rewrite the file.
		return nil
	}
	if err := writeAtomic(s.path, data); err != nil {
		return err
	}
	s.saved = data
	return nil
}

// writeAtomic writes data to a temp file beside path and renames it over.
func writeAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".people-*")
	if err != nil {
		return fmt.Errorf("customers: temp file: %w", err)
	}
	name := tmp.Name()
	_, werr := tmp.Write(data)
	if werr == nil {
		werr = tmp.Sync()
	}
	if cerr := tmp.Close(); werr == nil {
		werr = cerr
	}
	if werr == nil {
		werr = os.Chmod(name, storeFileMode)
	}
	if werr == nil {
		werr = os.Rename(name, path)
	}
	if werr != nil {
		_ = os.Remove(name)
		return fmt.Errorf("customers: writing %s: %w", FileName, werr)
	}
	return nil
}

func clonePerson(p Person) Person {
	var out Person
	data, err := json.Marshal(p)
	if err != nil {
		return p
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return p
	}
	return out
}

func cloneSuggestion(sg Suggestion) Suggestion {
	out := sg
	out.Evidence = append([]string(nil), sg.Evidence...)
	if sg.Counts != nil {
		out.Counts = make(map[string]int, len(sg.Counts))
		for k, v := range sg.Counts {
			out.Counts[k] = v
		}
	}
	return out
}

// cleanText trims s, refuses control characters and enforces a length cap.
func cleanText(s string, maxLen int, multiline bool) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) > maxLen {
		return "", fmt.Errorf("%w: longer than %d bytes", ErrInvalid, maxLen)
	}
	for _, r := range s {
		if multiline && (r == '\n' || r == '\t') {
			continue
		}
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%w: control character", ErrInvalid)
		}
	}
	return s, nil
}

// cleanIdentity validates and normalises one identity.
func cleanIdentity(i Identity) (Identity, error) {
	if !identityKinds[i.Kind] {
		return i, fmt.Errorf("%w: unknown identity kind %q", ErrInvalid, i.Kind)
	}
	v, err := cleanText(i.Value, MaxIdentityLen, false)
	if err != nil {
		return i, err
	}
	v = NormalizeIdentity(i.Kind, v)
	if v == "" {
		return i, fmt.Errorf("%w: empty identity", ErrInvalid)
	}
	if (i.Kind == KindEmail || i.Kind == KindGitEmail) && !strings.Contains(v, "@") {
		return i, fmt.Errorf("%w: not an email address", ErrInvalid)
	}
	i.Value = v
	return i, nil
}

// People returns every customer, by name then id.
func (s *Store) People() []Person {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Person, 0, len(s.people))
	for _, p := range s.people {
		out = append(out, clonePerson(p))
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one customer.
func (s *Store) Get(id string) (Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[id]
	if !ok {
		return Person{}, ErrNotFound
	}
	return clonePerson(p), nil
}

// Lookup returns the customer that holds an identity. A name is never a
// key, so a name never finds anyone.
func (s *Store) Lookup(kind IdentityKind, value string) (Person, bool) {
	if kind == KindName {
		return Person{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	id, ok := s.owner[IdentityKey(kind, value)]
	if !ok {
		return Person{}, false
	}
	return clonePerson(s.people[id]), true
}

// EnsureHiveUser returns the profile of a dashboard user, creating it on
// first use. This is the only way a hive_user identity comes to exist.
func (s *Store) EnsureHiveUser(user string) (Person, error) {
	id, err := cleanIdentity(Identity{Kind: KindHiveUser, Value: user})
	if err != nil {
		return Person{}, err
	}
	s.mu.Lock()
	if pid, ok := s.owner[id.Key()]; ok {
		p := clonePerson(s.people[pid])
		s.mu.Unlock()
		return p, nil
	}
	s.mu.Unlock()

	var out Person
	err = s.change(func() error {
		if pid, ok := s.owner[id.Key()]; ok {
			out = clonePerson(s.people[pid])
			return nil
		}
		pid, err := newID(personPrefix)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		id.Source, id.Confidence, id.At = "access_list", 1, now
		p := Person{
			ID: pid, Name: id.Value, Status: StatusCustomer, Origin: OriginHiveUser,
			Identities:   []Identity{id},
			Satisfaction: Satisfaction{Level: LevelUnknown},
			CreatedAt:    now, UpdatedAt: now,
		}
		s.people[pid] = p
		out = clonePerson(p)
		return nil
	})
	return out, err
}

// Add creates a customer by hand. Identities that already belong to someone
// else are refused with ErrConflict; a hive_user identity cannot be added by
// hand.
func (s *Store) Add(name string, ids []Identity, by string) (Person, error) {
	name, err := cleanText(name, MaxNameLen, false)
	if err != nil {
		return Person{}, err
	}
	if name == "" {
		return Person{}, fmt.Errorf("%w: a name is required", ErrInvalid)
	}
	if len(ids) > MaxIdentities {
		return Person{}, fmt.Errorf("%w: too many identities", ErrInvalid)
	}
	var out Person
	err = s.change(func() error {
		pid, err := newID(personPrefix)
		if err != nil {
			return err
		}
		now := s.now().UTC()
		p := Person{
			ID: pid, Name: name, Status: StatusCustomer, Origin: OriginManual,
			Satisfaction: Satisfaction{Level: LevelUnknown},
			CreatedAt:    now, UpdatedAt: now,
		}
		for _, raw := range ids {
			id, err := cleanIdentity(raw)
			if err != nil {
				return err
			}
			if id.Kind == KindHiveUser {
				return fmt.Errorf("%w: a hive user identity cannot be added by hand", ErrInvalid)
			}
			if p.HasIdentity(id.Key()) {
				continue
			}
			if id.Kind != KindName {
				if _, taken := s.owner[id.Key()]; taken {
					return fmt.Errorf("%w: %s already belongs to another customer", ErrConflict, id.Key())
				}
				s.adoptKeyLocked(id.Key())
			}
			id.Source, id.Confidence, id.Manual, id.At, id.By = "manual", 1, true, now, by
			p.Identities = append(p.Identities, id)
		}
		s.people[pid] = p
		out = clonePerson(p)
		return nil
	})
	return out, err
}

// adoptKeyLocked clears what stood in the way of a person deliberately
// taking an identity: a pending suggestion for the key and a tombstone. A
// manual decision outranks both.
func (s *Store) adoptKeyLocked(key string) {
	delete(s.sugg, key)
	delete(s.tombs, s.tombstoneHash(key))
}

// SetName renames a customer.
func (s *Store) SetName(id, name string) (Person, error) {
	name, err := cleanText(name, MaxNameLen, false)
	if err != nil {
		return Person{}, err
	}
	if name == "" {
		return Person{}, fmt.Errorf("%w: a name is required", ErrInvalid)
	}
	return s.changePerson(id, func(p *Person) error {
		p.Name = name
		return nil
	})
}

// SetNotes replaces a customer's notes.
func (s *Store) SetNotes(id, notes string) (Person, error) {
	notes, err := cleanText(notes, MaxNotesLen, true)
	if err != nil {
		return Person{}, err
	}
	return s.changePerson(id, func(p *Person) error {
		p.Notes = notes
		return nil
	})
}

// SetProfileField sets one profile field by hand. An empty value clears it
// and keeps the manual mark, so the automatic source does not refill what a
// person removed.
func (s *Store) SetProfileField(id, field, value, by string) (Person, error) {
	value, err := cleanText(value, MaxProfileFieldLen, false)
	if err != nil {
		return Person{}, err
	}
	return s.changePerson(id, func(p *Person) error {
		f := p.Profile.Field(field)
		if f == nil {
			return fmt.Errorf("%w: unknown profile field", ErrInvalid)
		}
		*f = ProfileField{Value: value, Source: SourceManual, At: s.now(), By: by}
		return nil
	})
}

// ResetProfileField hands a field back to the automatic source: it is
// emptied and the next accept-time fetch may fill it.
func (s *Store) ResetProfileField(id, field string) (Person, error) {
	return s.changePerson(id, func(p *Person) error {
		f := p.Profile.Field(field)
		if f == nil {
			return fmt.Errorf("%w: unknown profile field", ErrInvalid)
		}
		*f = ProfileField{}
		return nil
	})
}

// ApplyAutoProfile fills profile fields from a public GitHub profile. It
// only writes a field a person has not set (manual) and only with a
// non-empty value, and it reports whether anything changed.
func (s *Store) ApplyAutoProfile(id string, values map[string]string) (Person, bool, error) {
	changed := false
	p, err := s.changePerson(id, func(p *Person) error {
		for _, name := range ProfileFields {
			f := p.Profile.Field(name)
			v, err := cleanText(values[name], MaxProfileFieldLen, false)
			if err != nil || v == "" || f.Source == SourceManual || f.Value == v {
				continue
			}
			*f = ProfileField{Value: v, Source: SourceAuto, Rule: ProfileSourceGitHub, At: s.now()}
			changed = true
		}
		return nil
	})
	return p, changed, err
}

// Forget deletes a customer and leaves a salted-hash tombstone for each of
// their confirmed identities, so a later sweep does not rebuild them. An
// unconfirmed claim is not tombstoned: it may be someone else's. The returned
// person is what was deleted; the caller removes their edges. Hive-user
// identities and names are not tombstoned: a dashboard user who signs in
// again gets a fresh, empty profile.
func (s *Store) Forget(id string) (Person, error) {
	var out Person
	err := s.change(func() error {
		p, ok := s.people[id]
		if !ok {
			return ErrNotFound
		}
		out = clonePerson(p)
		for _, i := range p.Identities {
			if i.Kind == KindName || i.Kind == KindHiveUser || i.Claimed {
				continue
			}
			s.tombs[s.tombstoneHash(i.Key())] = true
		}
		for _, m := range p.MergedFrom {
			for _, i := range m.Person.Identities {
				if i.Kind == KindName || i.Kind == KindHiveUser || i.Claimed {
					continue
				}
				s.tombs[s.tombstoneHash(i.Key())] = true
			}
		}
		delete(s.people, id)
		s.dropRefsLocked(id)
		return nil
	})
	return out, err
}

// dropRefsLocked removes the pins and hints that mention an id.
func (s *Store) dropRefsLocked(id string) {
	pins := s.pins[:0:0]
	for _, p := range s.pins {
		if p.Person != id && p.Other != id {
			pins = append(pins, p)
		}
	}
	s.pins = pins
	hints := s.hints[:0:0]
	for _, h := range s.hints {
		if h.Person != id && h.Other != id {
			hints = append(hints, h)
		}
	}
	s.hints = hints
}

// appendHistory adds c and keeps the newest MaxHistory entries.
func appendHistory(h []Change, c Change) []Change {
	h = append(h, c)
	if len(h) > MaxHistory {
		h = h[len(h)-MaxHistory:]
	}
	return h
}

// capEvidence dedupes refs and keeps the first MaxEvidence.
func capEvidence(refs []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, r := range refs {
		r = strings.TrimSpace(r)
		if r == "" || seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
		if len(out) == MaxEvidence {
			break
		}
	}
	return out
}

// remember adds fp to a bounded list of rejected fingerprints.
func remember(list []string, fp string) []string {
	for _, v := range list {
		if v == fp {
			return list
		}
	}
	list = append(list, fp)
	if len(list) > maxRejected {
		list = list[len(list)-maxRejected:]
	}
	return list
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
