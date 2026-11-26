package customers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Enrichment holds public facts about confirmed customers that came from
// outside the hive (customers.enrichment, off by default; see
// src/docs/customers-enrichment.md). It lives in its own file beside
// people.json so it can be deleted and rebuilt at any time, and so nothing in
// it is ever mistaken for something a person entered. An affiliation found
// here is only ever offered as a suggestion (SuggestAffiliationWith); it never
// writes the organization field.

// EnrichmentFileName is the enrichment file inside the customers data dir.
const EnrichmentFileName = "enrichment.json"

// Enrichment sources.
const (
	EnrichSourceGitdm    = "gitdm"
	EnrichSourceDevstats = "devstats"
	EnrichSourceCrowdDev = "crowddev"
)

// Bounds on what one person's enrichment may hold, so a hostile or broken
// source cannot grow the file without limit.
const (
	MaxEnrichedAffiliations = 20
	MaxEnrichedActivity     = 50
	maxEnrichedTextLen      = 200
	maxEnrichedURLLen       = 500
	maxEnrichedMatchedByLen = 32
	// enrichmentDateLayout is how affiliation dates are kept: a calendar
	// date, the precision gitdm publishes.
	enrichmentDateLayout = "2006-01-02"
)

// EnrichedAffiliation is one public affiliation with its dates. From and
// Until are calendar dates ("2006-01-02"); an empty Until means current.
type EnrichedAffiliation struct {
	Org    string `json:"org"`
	From   string `json:"from,omitempty"`
	Until  string `json:"until,omitempty"`
	Source string `json:"source"`
	// Confidence is the provider's confidence ("high", "medium", "low"),
	// and MatchedBy the identifier kind that matched ("github_login",
	// "email"), never its value. Both are empty in files written before
	// they were recorded.
	Confidence string `json:"confidence,omitempty"`
	MatchedBy  string `json:"matched_by,omitempty"`
}

// Enrichment confidence and match kinds, as colony's gitdm provider reports
// them.
const (
	EnrichConfidenceHigh   = "high"
	EnrichConfidenceMedium = "medium"
	EnrichConfidenceLow    = "low"
	EnrichMatchedByLogin   = "github_login"
)

// EnrichedActivity is one project's public contribution count.
type EnrichedActivity struct {
	Project       string `json:"project"`
	Contributions int    `json:"contributions"`
	Period        string `json:"period,omitempty"`
	URL           string `json:"url,omitempty"`
	Source        string `json:"source"`
}

// PersonEnrichment is everything enrichment holds about one person.
type PersonEnrichment struct {
	Affiliations []EnrichedAffiliation `json:"affiliations,omitempty"`
	Activity     []EnrichedActivity    `json:"activity,omitempty"`
	// FetchedAt is when each source last answered for this person.
	FetchedAt map[string]time.Time `json:"fetched_at,omitempty"`
}

type enrichmentFile struct {
	People map[string]PersonEnrichment `json:"people"`
}

// EnrichmentStore is /data/customers/enrichment.json (0600, atomic writes),
// keyed by person id.
type EnrichmentStore struct {
	path   string
	mu     sync.Mutex
	people map[string]PersonEnrichment
	saved  []byte
	// colony mirrors every change into colony.db and, in colony mode,
	// answers reads; nil in json mode.
	colony *Colony
}

// SetColony wires the colony mirror (the people store's Colony). Call it
// before the store is shared.
func (e *EnrichmentStore) SetColony(c *Colony) {
	if e == nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.colony = c
}

// OpenEnrichment opens <dir>/enrichment.json. A missing file is empty. A
// damaged one is set aside rather than failing: everything in it can be
// fetched again.
func OpenEnrichment(dir string) (*EnrichmentStore, error) {
	if err := os.MkdirAll(dir, storeDirMode); err != nil {
		return nil, fmt.Errorf("customers: creating dir: %w", err)
	}
	e := &EnrichmentStore{path: filepath.Join(dir, EnrichmentFileName), people: map[string]PersonEnrichment{}}
	data, err := os.ReadFile(e.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return e, nil
	case err != nil:
		return nil, fmt.Errorf("customers: reading %s: %w", EnrichmentFileName, err)
	}
	var f enrichmentFile
	if err := json.Unmarshal(data, &f); err != nil {
		return e, nil
	}
	for id, pe := range f.People {
		if id != "" {
			e.people[id] = pe
		}
	}
	e.saved = data
	return e, nil
}

// Path is the enrichment file.
func (e *EnrichmentStore) Path() string { return e.path }

// Get returns what enrichment holds for a person.
func (e *EnrichmentStore) Get(id string) (PersonEnrichment, bool) {
	if e == nil {
		return PersonEnrichment{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	pe, ok := e.people[id]
	if !ok {
		return PersonEnrichment{}, false
	}
	return e.colony.enrichmentFromColony(id, clonePersonEnrichment(pe)), true
}

// IDs returns every person id enrichment holds, sorted.
func (e *EnrichmentStore) IDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]string, 0, len(e.people))
	for id := range e.people {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// SetSource replaces one source's facts for a person and records when the
// source answered. Facts from other sources are kept. Values are trimmed,
// capped and validated; an entry that does not fit is dropped.
func (e *EnrichmentStore) SetSource(id, source string, affs []EnrichedAffiliation, acts []EnrichedActivity, at time.Time) error {
	if id == "" || source == "" {
		return fmt.Errorf("%w: enrichment needs a person and a source", ErrInvalid)
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	pe := clonePersonEnrichment(e.people[id])
	keptA := pe.Affiliations[:0:0]
	for _, a := range pe.Affiliations {
		if a.Source != source {
			keptA = append(keptA, a)
		}
	}
	for _, a := range affs {
		if a, ok := cleanEnrichedAffiliation(a, source); ok && len(keptA) < MaxEnrichedAffiliations {
			keptA = append(keptA, a)
		}
	}
	keptT := pe.Activity[:0:0]
	for _, t := range pe.Activity {
		if t.Source != source {
			keptT = append(keptT, t)
		}
	}
	for _, t := range acts {
		if t, ok := cleanEnrichedActivity(t, source); ok && len(keptT) < MaxEnrichedActivity {
			keptT = append(keptT, t)
		}
	}
	pe.Affiliations, pe.Activity = keptA, keptT
	if pe.FetchedAt == nil {
		pe.FetchedAt = map[string]time.Time{}
	}
	pe.FetchedAt[source] = at.UTC()
	e.people[id] = pe
	return e.saveLocked(map[string]PersonEnrichment{id: clonePersonEnrichment(pe)}, nil)
}

// DropSource removes one source's facts from every person, for a source
// that was switched off. It reports how many people changed.
func (e *EnrichmentStore) DropSource(source string) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	changed := map[string]PersonEnrichment{}
	var removed []string
	for id, pe := range e.people {
		_, fetched := pe.FetchedAt[source]
		if !fetched && !holdsSource(pe, source) {
			continue
		}
		pe = clonePersonEnrichment(pe)
		pe.Affiliations = filterAffiliations(pe.Affiliations, source)
		pe.Activity = filterActivity(pe.Activity, source)
		delete(pe.FetchedAt, source)
		if len(pe.Affiliations) == 0 && len(pe.Activity) == 0 && len(pe.FetchedAt) == 0 {
			delete(e.people, id)
			removed = append(removed, id)
		} else {
			e.people[id] = pe
			changed[id] = clonePersonEnrichment(pe)
		}
		n++
	}
	if n == 0 {
		return 0, nil
	}
	return n, e.saveLocked(changed, removed)
}

// Delete removes everything enrichment holds for a person. Forget calls it.
func (e *EnrichmentStore) Delete(id string) error {
	if e == nil {
		return nil
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if _, ok := e.people[id]; !ok {
		return nil
	}
	delete(e.people, id)
	return e.saveLocked(nil, []string{id})
}

// Prune removes the people keep says are gone (forgotten, merged away or
// never confirmed) and reports how many it removed.
func (e *EnrichmentStore) Prune(keep func(id string) bool) (int, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	n := 0
	var removed []string
	for id := range e.people {
		if !keep(id) {
			delete(e.people, id)
			removed = append(removed, id)
			n++
		}
	}
	if n == 0 {
		return 0, nil
	}
	return n, e.saveLocked(nil, removed)
}

// saveLocked writes enrichment.json and then mirrors the people it changed
// (changed holds their new state, removed the ids it dropped) into
// colony.db. The JSON is written first and stays the rollback copy.
func (e *EnrichmentStore) saveLocked(changed map[string]PersonEnrichment, removed []string) error {
	data, err := json.MarshalIndent(enrichmentFile{People: e.people}, "", "  ")
	if err != nil {
		return fmt.Errorf("customers: encoding %s: %w", EnrichmentFileName, err)
	}
	if bytes.Equal(data, e.saved) {
		return nil
	}
	if err := writeFileAtomic(e.path, ".enrichment-*", data); err != nil {
		return err
	}
	e.saved = data
	e.colony.mirrorEnrichment(changed, removed, data)
	return nil
}

// writeFileAtomic writes data to a temp file beside path, sets the store's
// owner-only mode and renames it over path.
func writeFileAtomic(path, pattern string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), pattern)
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
		return fmt.Errorf("customers: writing %s: %w", filepath.Base(path), werr)
	}
	return nil
}

func holdsSource(pe PersonEnrichment, source string) bool {
	for _, a := range pe.Affiliations {
		if a.Source == source {
			return true
		}
	}
	for _, t := range pe.Activity {
		if t.Source == source {
			return true
		}
	}
	return false
}

func filterAffiliations(in []EnrichedAffiliation, drop string) []EnrichedAffiliation {
	out := in[:0:0]
	for _, a := range in {
		if a.Source != drop {
			out = append(out, a)
		}
	}
	return out
}

func filterActivity(in []EnrichedActivity, drop string) []EnrichedActivity {
	out := in[:0:0]
	for _, t := range in {
		if t.Source != drop {
			out = append(out, t)
		}
	}
	return out
}

func clonePersonEnrichment(pe PersonEnrichment) PersonEnrichment {
	out := PersonEnrichment{
		Affiliations: append([]EnrichedAffiliation(nil), pe.Affiliations...),
		Activity:     append([]EnrichedActivity(nil), pe.Activity...),
	}
	if pe.FetchedAt != nil {
		out.FetchedAt = make(map[string]time.Time, len(pe.FetchedAt))
		for k, v := range pe.FetchedAt {
			out.FetchedAt[k] = v
		}
	}
	return out
}

// enrichedText trims, drops control characters and refuses text over limit.
func enrichedText(s string, limit int) (string, bool) {
	s = strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, s))
	return s, len(s) <= limit
}

func enrichedDate(s string) (string, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", true
	}
	if _, err := time.Parse(enrichmentDateLayout, s); err != nil {
		return "", false
	}
	return s, true
}

func cleanEnrichedAffiliation(a EnrichedAffiliation, source string) (EnrichedAffiliation, bool) {
	org, ok := enrichedText(a.Org, maxEnrichedTextLen)
	if !ok || org == "" {
		return EnrichedAffiliation{}, false
	}
	from, okF := enrichedDate(a.From)
	until, okU := enrichedDate(a.Until)
	if !okF || !okU {
		return EnrichedAffiliation{}, false
	}
	conf := strings.ToLower(strings.TrimSpace(a.Confidence))
	switch conf {
	case "", EnrichConfidenceHigh, EnrichConfidenceMedium, EnrichConfidenceLow:
	default:
		conf = ""
	}
	by, okB := enrichedText(a.MatchedBy, maxEnrichedMatchedByLen)
	if !okB {
		by = ""
	}
	return EnrichedAffiliation{Org: org, From: from, Until: until, Source: source, Confidence: conf, MatchedBy: by}, true
}

func cleanEnrichedActivity(t EnrichedActivity, source string) (EnrichedActivity, bool) {
	project, ok := enrichedText(t.Project, maxEnrichedTextLen)
	if !ok || project == "" || t.Contributions < 0 {
		return EnrichedActivity{}, false
	}
	period, _ := enrichedText(t.Period, maxEnrichedTextLen)
	u, okU := enrichedText(t.URL, maxEnrichedURLLen)
	if !okU || (u != "" && !strings.HasPrefix(u, "https://")) {
		u = ""
	}
	return EnrichedActivity{Project: project, Contributions: t.Contributions, Period: period, URL: u, Source: source}, true
}

// currentEnrichedAffiliation is the affiliation a source says holds today:
// started (no From, or From not after today), not ended (no Until, or Until
// after today: gitdm's "until" date is the first day the person was no
// longer there), the latest From. Ties keep the first.
func currentEnrichedAffiliation(affs []EnrichedAffiliation, now time.Time) (EnrichedAffiliation, bool) {
	today := now.UTC().Format(enrichmentDateLayout)
	var best EnrichedAffiliation
	found := false
	for _, a := range affs {
		if (a.Until != "" && a.Until <= today) || (a.From != "" && a.From > today) {
			continue
		}
		if !found || a.From > best.From {
			best, found = a, true
		}
	}
	return best, found
}
