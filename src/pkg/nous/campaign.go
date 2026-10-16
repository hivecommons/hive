// Package nous holds the Experiment Lab's campaign store. A campaign is one
// research question plus everything measured while it was active: its
// baseline window, hypotheses and judged results. Changing the research
// question (or explicitly starting a new experiment) archives the active
// campaign and starts a new one whose baseline begins at zero, while the
// hive-state observation history in pkg/hivestate is kept untouched — the
// snapshots are question-independent observations, but a campaign's baseline
// must not include windows perturbed by a previous campaign's experiments.
package nous

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// FileVersion is the on-disk schema version of the campaign file.
const FileVersion = 1

// DefaultFileName is the campaign file name next to the snapshot directory.
const DefaultFileName = "campaigns.json"

// Campaign is one research question's experiment run.
type Campaign struct {
	ID        int        `json:"id"`
	Question  string     `json:"question"`
	StartedAt time.Time  `json:"started_at"`
	EndedAt   *time.Time `json:"ended_at,omitempty"`
	// Outcome is frozen when the campaign is archived; it is empty for the
	// active campaign, whose counts are computed live.
	Outcome Outcome `json:"outcome"`
}

// Outcome is what a campaign produced, frozen at archive time.
type Outcome struct {
	Snapshots   int                      `json:"snapshots"`
	Experiments int                      `json:"experiments"`
	Principles  int                      `json:"principles"`
	Results     []map[string]interface{} `json:"results,omitempty"`
}

// Archived reports whether the campaign has ended.
func (c Campaign) Archived() bool { return c.EndedAt != nil }

// Status is "active" or "archived".
func (c Campaign) Status() string {
	if c.Archived() {
		return "archived"
	}
	return "active"
}

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

func (s *Store) activeIndex() int {
	for i := len(s.campaigns) - 1; i >= 0; i-- {
		if !s.campaigns[i].Archived() {
			return i
		}
	}
	return -1
}

// Active returns the active campaign, if any.
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
	c := Campaign{ID: s.nextID(), Question: strings.TrimSpace(question), StartedAt: startedAt.UTC()}
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

// Start archives the active campaign (if any) with the given outcome and
// starts a new one for question at now. It returns the archived campaign
// (nil when there was none) and the new active campaign.
func (s *Store) Start(question string, now time.Time, outcome Outcome) (*Campaign, Campaign, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.load(); err != nil {
		return nil, Campaign{}, err
	}
	now = now.UTC()
	var prev *Campaign
	if i := s.activeIndex(); i >= 0 {
		ended := now
		s.campaigns[i].EndedAt = &ended
		s.campaigns[i].Outcome = outcome
		p := s.campaigns[i]
		prev = &p
	}
	next := Campaign{ID: s.nextID(), Question: strings.TrimSpace(question), StartedAt: now}
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
