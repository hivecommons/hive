// Package nous holds the Experiment Lab's campaign store. A campaign is one
// research question plus everything measured while it was active: its
// baseline window, hypotheses and judged results. Several campaigns may run
// at once (hivecommons/hive#12532) as long as their scopes do not overlap:
// two concurrent interventions on the same repo would confound each other's
// outcomes, so the store refuses to activate a campaign whose scope collides
// with another active one (see Conflict). Each campaign's baseline begins at
// zero when it starts, while the hive-state observation history in
// pkg/hivestate is kept untouched — the snapshots are question-independent
// observations, but a campaign's baseline must not include windows perturbed
// by a previous campaign's experiments.
package nous

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// FileVersion is the on-disk schema version of the campaign file. Version 1
// (hivecommons/hive#12501) had one active campaign and no scope or status
// field; version 2 adds Scope, Hypothesis and an explicit Status so several
// campaigns can be active at once. Version 1 files are read and upgraded on
// first write.
const FileVersion = 2

// Campaign statuses.
const (
	StatusActive   = "active"
	StatusPaused   = "paused"
	StatusArchived = "archived"
)

// ScopeHiveWide is the scope label of a campaign with no repo targets: it
// claims every repo, so it conflicts with any other active campaign.
const ScopeHiveWide = "hive-wide"

// DefaultFileName is the campaign file name next to the snapshot directory.
const DefaultFileName = "campaigns.json"

// Campaign is one research question's experiment run.
type Campaign struct {
	ID         int    `json:"id"`
	Question   string `json:"question"`
	Hypothesis string `json:"hypothesis,omitempty"`
	// Scope is what the campaign intervenes on. An empty Repos list means
	// hive-wide.
	Scope Scope `json:"scope"`
	// State is StatusActive, StatusPaused or StatusArchived. It is empty in
	// version-1 files, where EndedAt alone told archived from active; Status()
	// resolves both.
	State     string     `json:"status,omitempty"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Outcome is frozen when the campaign is archived; it is empty for a
	// running campaign, whose counts are computed live.
	Outcome Outcome `json:"outcome"`
}

// Scope is the set of things a campaign may change. Two active campaigns
// must not overlap in Repos (an empty Repos list is hive-wide and overlaps
// everything); Intervention and Tags are descriptive only.
type Scope struct {
	Repos        []string `json:"repos,omitempty"`
	Intervention string   `json:"intervention,omitempty"`
	Tags         []string `json:"tags,omitempty"`
}

// HiveWide reports whether the scope claims every repo.
func (sc Scope) HiveWide() bool { return len(sc.Repos) == 0 }

// Overlaps reports whether two scopes share a repo target (case-insensitive)
// or either is hive-wide.
func (sc Scope) Overlaps(other Scope) ([]string, bool) {
	if sc.HiveWide() || other.HiveWide() {
		return nil, true
	}
	seen := map[string]bool{}
	for _, r := range sc.Repos {
		seen[strings.ToLower(strings.TrimSpace(r))] = true
	}
	var shared []string
	for _, r := range other.Repos {
		if seen[strings.ToLower(strings.TrimSpace(r))] {
			shared = append(shared, r)
		}
	}
	return shared, len(shared) > 0
}

func normalizeScope(sc Scope) Scope {
	out := Scope{Intervention: strings.TrimSpace(sc.Intervention)}
	seen := map[string]bool{}
	for _, r := range sc.Repos {
		r = strings.TrimSpace(r)
		if r == "" || seen[strings.ToLower(r)] {
			continue
		}
		seen[strings.ToLower(r)] = true
		out.Repos = append(out.Repos, r)
	}
	seenTag := map[string]bool{}
	for _, t := range sc.Tags {
		t = strings.TrimSpace(t)
		if t == "" || seenTag[strings.ToLower(t)] {
			continue
		}
		seenTag[strings.ToLower(t)] = true
		out.Tags = append(out.Tags, t)
	}
	return out
}

// Outcome is what a campaign produced, frozen at archive time.
type Outcome struct {
	Snapshots   int                      `json:"snapshots"`
	Experiments int                      `json:"experiments"`
	Principles  int                      `json:"principles"`
	Results     []map[string]interface{} `json:"results,omitempty"`
}

// Archived reports whether the campaign has ended.
func (c Campaign) Archived() bool { return c.EndedAt != nil || c.State == StatusArchived }

// Paused reports whether the campaign is kept but not currently observing
// or proposing experiments.
func (c Campaign) Paused() bool { return !c.Archived() && c.State == StatusPaused }

// Running reports whether the campaign is active: not archived, not paused.
func (c Campaign) Running() bool { return !c.Archived() && !c.Paused() }

// Status is StatusActive, StatusPaused or StatusArchived.
func (c Campaign) Status() string {
	switch {
	case c.Archived():
		return StatusArchived
	case c.Paused():
		return StatusPaused
	}
	return StatusActive
}

// Conflict is returned when activating a campaign would overlap another
// active campaign's scope.
type Conflict struct {
	With   Campaign
	Shared []string
}

func (e *Conflict) Error() string {
	what := ScopeHiveWide
	if len(e.Shared) > 0 {
		what = strings.Join(e.Shared, ", ")
	}
	return "nous: scope conflicts with active experiment #" + strconv.Itoa(e.With.ID) + " (" + what + ")"
}

// ErrNotFound is returned for an unknown campaign id.
var ErrNotFound = errors.New("nous: campaign not found")

// ErrArchived is returned when a mutation targets an archived campaign.
var ErrArchived = errors.New("nous: campaign is archived")

type file struct {
	Version   int        `json:"version"`
	Campaigns []Campaign `json:"campaigns"`
}

// Store persists campaigns to a single JSON file. The zero Store is unusable;
// use NewStore. Methods are safe for concurrent use.
type Store struct {
	path string

	mu        sync.Mutex
	loaded    bool
	campaigns []Campaign
}

// NewStore returns a store backed by path. Nothing is read until first use.
func NewStore(path string) *Store {
	return &Store{path: path}
}

// Path returns the backing file path.
func (s *Store) Path() string { return s.path }

// ErrNoActive is returned when an operation needs an active campaign and
// none exists.
var ErrNoActive = errors.New("nous: no active campaign")

func (s *Store) load() error {
	if s.loaded {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.loaded = true
			return nil
		}
		return err
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		// A corrupt file is never overwritten: every call keeps failing
		// until an operator repairs or removes it.
		return err
	}
	s.loaded = true
	s.campaigns = f.Campaigns
	// Version 1 rows carry no status: EndedAt alone told archived from
	// active. Resolve it so the next save writes an explicit state.
	for i := range s.campaigns {
		if s.campaigns[i].State == "" {
			s.campaigns[i].State = s.campaigns[i].Status()
		}
	}
	sort.SliceStable(s.campaigns, func(i, j int) bool { return s.campaigns[i].ID < s.campaigns[j].ID })
	return nil
}

func (s *Store) save() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(file{Version: FileVersion, Campaigns: s.campaigns}, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// activeIndex is the oldest running campaign: the one legacy single-campaign
// callers (goals PUT, /api/nous/campaign/start) operate on.
func (s *Store) activeIndex() int {
	for i := range s.campaigns {
		if s.campaigns[i].Running() {
			return i
		}
	}
	return -1
}

func (s *Store) indexOf(id int) int {
	for i := range s.campaigns {
		if s.campaigns[i].ID == id {
			return i
		}
	}
	return -1
}

// Active returns the oldest running campaign, if any.
func (s *Store) Active() (Campaign, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, false
	}
	i := s.activeIndex()
	if i < 0 {
		return Campaign{}, false
	}
	return s.campaigns[i], true
}

// Archived returns archived campaigns, newest first.
func (s *Store) Archived() []Campaign {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil
	}
	var out []Campaign
	for i := len(s.campaigns) - 1; i >= 0; i-- {
		if s.campaigns[i].Archived() {
			out = append(out, s.campaigns[i])
		}
	}
	return out
}

// All returns every campaign in ID order.
func (s *Store) All() []Campaign {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil
	}
	return append([]Campaign(nil), s.campaigns...)
}

// Open returns every non-archived campaign (active and paused) in ID order.
func (s *Store) Open() []Campaign {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil
	}
	var out []Campaign
	for _, c := range s.campaigns {
		if !c.Archived() {
			out = append(out, c)
		}
	}
	return out
}

// Get returns the campaign with the given id.
func (s *Store) Get(id int) (Campaign, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, false
	}
	if i := s.indexOf(id); i >= 0 {
		return s.campaigns[i], true
	}
	return Campaign{}, false
}

// conflict returns the first running campaign (other than id) whose scope
// overlaps sc.
func (s *Store) conflict(sc Scope, exclude int) *Conflict {
	for _, c := range s.campaigns {
		if c.ID == exclude || !c.Running() {
			continue
		}
		if shared, ok := c.Scope.Overlaps(sc); ok {
			return &Conflict{With: c, Shared: shared}
		}
	}
	return nil
}

// Conflict reports which running campaign, if any, the given scope would
// collide with. exclude is a campaign id to ignore (0 for none).
func (s *Store) Conflict(sc Scope, exclude int) (Campaign, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, false
	}
	if c := s.conflict(normalizeScope(sc), exclude); c != nil {
		return c.With, true
	}
	return Campaign{}, false
}

// Create adds a new active campaign alongside the existing ones. It fails
// with *Conflict when a running campaign already covers part of the scope
// (hivecommons/hive#12532): concurrent interventions on the same repos would
// confound each other's outcomes. Pause or archive the other campaign, or
// narrow the scope, and retry.
func (s *Store) Create(question, hypothesis string, scope Scope, now time.Time) (Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, err
	}
	scope = normalizeScope(scope)
	if c := s.conflict(scope, 0); c != nil {
		return Campaign{}, c
	}
	next := Campaign{
		ID:         s.nextID(),
		Question:   strings.TrimSpace(question),
		Hypothesis: strings.TrimSpace(hypothesis),
		Scope:      scope,
		State:      StatusActive,
		StartedAt:  now.UTC(),
	}
	s.campaigns = append(s.campaigns, next)
	if err := s.save(); err != nil {
		return Campaign{}, err
	}
	return next, nil
}

// Patch is a partial update applied by Update. Nil fields are left alone.
type Patch struct {
	Question   *string
	Hypothesis *string
	Scope      *Scope
}

// Update edits a non-archived campaign in place. A scope change is subject
// to the same conflict check as Create when the campaign is running.
func (s *Store) Update(id int, p Patch) (Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, err
	}
	i := s.indexOf(id)
	if i < 0 {
		return Campaign{}, ErrNotFound
	}
	c := s.campaigns[i]
	if c.Archived() {
		return Campaign{}, ErrArchived
	}
	if p.Question != nil {
		c.Question = strings.TrimSpace(*p.Question)
	}
	if p.Hypothesis != nil {
		c.Hypothesis = strings.TrimSpace(*p.Hypothesis)
	}
	if p.Scope != nil {
		sc := normalizeScope(*p.Scope)
		if c.Running() {
			if conf := s.conflict(sc, id); conf != nil {
				return Campaign{}, conf
			}
		}
		c.Scope = sc
	}
	s.campaigns[i] = c
	if err := s.save(); err != nil {
		return Campaign{}, err
	}
	return c, nil
}

// Pause keeps the campaign but takes it out of the running set, which also
// releases its scope for other campaigns.
func (s *Store) Pause(id int) (Campaign, error) {
	return s.setState(id, StatusPaused)
}

// Resume reactivates a paused campaign, subject to the conflict check.
func (s *Store) Resume(id int) (Campaign, error) {
	return s.setState(id, StatusActive)
}

func (s *Store) setState(id int, state string) (Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, err
	}
	i := s.indexOf(id)
	if i < 0 {
		return Campaign{}, ErrNotFound
	}
	if s.campaigns[i].Archived() {
		return Campaign{}, ErrArchived
	}
	if state == StatusActive && !s.campaigns[i].Running() {
		if conf := s.conflict(s.campaigns[i].Scope, id); conf != nil {
			return Campaign{}, conf
		}
	}
	s.campaigns[i].State = state
	if err := s.save(); err != nil {
		return Campaign{}, err
	}
	return s.campaigns[i], nil
}

// Archive ends the campaign with the given frozen outcome. Archiving is
// final; the campaign stays readable with its results.
func (s *Store) Archive(id int, now time.Time, outcome Outcome) (Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, err
	}
	i := s.indexOf(id)
	if i < 0 {
		return Campaign{}, ErrNotFound
	}
	if s.campaigns[i].Archived() {
		return Campaign{}, ErrArchived
	}
	ended := now.UTC()
	s.campaigns[i].EndedAt = &ended
	s.campaigns[i].State = StatusArchived
	s.campaigns[i].Outcome = outcome
	if err := s.save(); err != nil {
		return Campaign{}, err
	}
	return s.campaigns[i], nil
}

// Ensure makes sure an active campaign exists. On a store with no campaigns
// (a hive that predates per-campaign baselines) it creates campaign 1 with
// the given question and start time, so existing observation history becomes
// campaign 1 rather than being orphaned. It reports whether a campaign was
// created.
func (s *Store) Ensure(question string, startedAt time.Time) (Campaign, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return Campaign{}, false, err
	}
	if i := s.activeIndex(); i >= 0 {
		return s.campaigns[i], false, nil
	}
	c := Campaign{ID: s.nextID(), Question: strings.TrimSpace(question), State: StatusActive, StartedAt: startedAt.UTC()}
	s.campaigns = append(s.campaigns, c)
	if err := s.save(); err != nil {
		return Campaign{}, false, err
	}
	return c, true, nil
}

func (s *Store) nextID() int {
	max := 0
	for _, c := range s.campaigns {
		if c.ID > max {
			max = c.ID
		}
	}
	return max + 1
}

// Start archives the oldest running campaign (if any) with the given outcome
// and starts a new one for question at now, inheriting the archived
// campaign's scope so the swap never trips the conflict guard. Other running
// campaigns are untouched. It returns the archived campaign (nil when there
// was none) and the new active campaign.
func (s *Store) Start(question string, now time.Time, outcome Outcome) (*Campaign, Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, Campaign{}, err
	}
	now = now.UTC()
	var prev *Campaign
	var scope Scope
	if i := s.activeIndex(); i >= 0 {
		ended := now
		s.campaigns[i].EndedAt = &ended
		s.campaigns[i].State = StatusArchived
		s.campaigns[i].Outcome = outcome
		p := s.campaigns[i]
		prev = &p
		scope = p.Scope
	}
	if prev == nil {
		if c := s.conflict(scope, 0); c != nil {
			return nil, Campaign{}, c
		}
	}
	next := Campaign{ID: s.nextID(), Question: strings.TrimSpace(question), Scope: scope, State: StatusActive, StartedAt: now}
	s.campaigns = append(s.campaigns, next)
	if err := s.save(); err != nil {
		return nil, Campaign{}, err
	}
	return prev, next, nil
}

// SetQuestion updates the active campaign's question in place without
// starting a new campaign (used when a question is first filled in on a
// campaign that has not recorded any observations yet).
func (s *Store) SetQuestion(question string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return err
	}
	i := s.activeIndex()
	if i < 0 {
		return ErrNoActive
	}
	s.campaigns[i].Question = strings.TrimSpace(question)
	return s.save()
}

// QuestionChanged reports whether next is a materially different research
// question from prev: non-empty after whitespace normalisation and not equal
// to prev ignoring case and whitespace. An empty next never counts as a
// change (clearing the field is not a new experiment). Going from no question
// to a question does count: the questionless campaign is the pre-question
// observation period and the first real question starts experiment 2.
func QuestionChanged(prev, next string) bool {
	p := normalizeQuestion(prev)
	n := normalizeQuestion(next)
	if n == "" {
		return false
	}
	return p != n
}

func normalizeQuestion(q string) string {
	return strings.ToLower(strings.Join(strings.Fields(q), " "))
}
