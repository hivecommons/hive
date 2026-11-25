// Package stalehold notices pull requests that sit behind a hold label after
// every one of their checks went green, and decides when the hive may release
// a hold that its setter explicitly asked to be released once CI is green.
//
// A hold is often set as "hold until CI is green, then release", by a person
// or by a background session that then stops before it removes the label. The
// PR then waits, fully green, for nobody. Before this package nothing measured
// that wait: the dashboard showed the hold, but nothing escalated as it aged.
//
// The package is pure bookkeeping. It never writes to a forge itself: Sweep
// returns the actions due this pass, and the caller performs them and reports
// a delivered notification back (MarkNotified). A release that fails is simply
// proposed again on the next pass. Two rules are load-bearing:
//
//   - A notification is raised once per held head. A new push re-arms it; a
//     check going red pauses the clock without re-arming it.
//   - A release is only ever proposed for a PR that carries the opt-in
//     release label. A plain hold is never released by this package.
package stalehold

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// PruneAfter is how long an entry survives without being observed. A PR
// drops out of one pass when its repository's enumeration fails; keeping the
// entry means the next pass neither restarts the clock nor re-notifies.
const PruneAfter = 7 * 24 * time.Hour

// ledger file permissions, matching the hive's other PVC ledgers.
const (
	ledgerDirMode  = 0o755
	ledgerFileMode = 0o644
)

// CI verdicts an Observation may carry.
const (
	// CIGreen means every check on the head completed successfully.
	CIGreen = "green"
	// CIRed means at least one check on the head failed or did not finish
	// cleanly (cancelled, timed out).
	CIRed = "red"
	// CIUnknown means this pass could not conclude: checks still running,
	// none reported, or the read failed. It changes nothing in the ledger.
	CIUnknown = ""
)

// Observation is one open PR from the current enumeration pass.
type Observation struct {
	Repo    string
	Number  int
	HeadSHA string
	Labels  []string
	// CI is CIGreen, CIRed or CIUnknown for HeadSHA.
	CI string
}

// Options configures a sweep.
type Options struct {
	// After is how long a held PR must stay green before it is raised.
	// Zero or negative disables the notification.
	After time.Duration
	// HoldLabels are the labels that count as a hold, matched exactly and
	// case-insensitively.
	HoldLabels []string
	// ReleaseLabel is the opt-in "release when green" label. Empty
	// disables releases.
	ReleaseLabel string
}

// ActionKind says what the caller should do for a PR.
type ActionKind string

const (
	// ActionNotify raises a held, green PR to a person, once.
	ActionNotify ActionKind = "notify"
	// ActionRelease removes the hold labels and the opt-in release label
	// from a PR whose checks are all green.
	ActionRelease ActionKind = "release"
)

// Action is one thing due this pass.
type Action struct {
	Kind    ActionKind
	Repo    string
	Number  int
	HeadSHA string
	// HoldLabels are the hold labels on the PR, as spelled on the forge.
	// For ActionRelease they are the labels to remove before ReleaseLabel.
	HoldLabels []string
	// ReleaseLabel is the opt-in label as spelled on the forge
	// (ActionRelease only).
	ReleaseLabel string
	// GreenFor is how long the head has been held and green.
	GreenFor time.Duration
}

// Entry is the ledger's record of one held PR.
type Entry struct {
	HeadSHA string `json:"head_sha"`
	// GreenSince is when the head was first seen held and green; zero while
	// a check is red.
	GreenSince time.Time `json:"green_since,omitempty"`
	// NotifiedSHA is the head a notification was raised for.
	NotifiedSHA string    `json:"notified_sha,omitempty"`
	LastSeen    time.Time `json:"last_seen"`
}

// Store is the on-disk ledger. All methods are safe for concurrent use.
type Store struct {
	mu      sync.Mutex
	path    string
	entries map[string]*Entry
}

// Load reads the ledger at path. A missing or corrupt file yields an empty,
// usable store: losing the ledger only delays a notification. An empty path
// keeps the store in memory.
func Load(path string) *Store {
	s := &Store{path: path, entries: map[string]*Entry{}}
	if path == "" {
		return s
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return s
	}
	var m map[string]*Entry
	if json.Unmarshal(data, &m) == nil && m != nil {
		for k, e := range m {
			if e == nil {
				delete(m, k)
			}
		}
		s.entries = m
	}
	return s
}

// Key builds the ledger key for a PR.
func Key(repo string, number int) string {
	return fmt.Sprintf("%s#%d", strings.ToLower(strings.TrimSpace(repo)), number)
}

// Entry returns a copy of the ledger entry for a PR.
func (s *Store) Entry(repo string, number int) (Entry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[Key(repo, number)]
	if !ok {
		return Entry{}, false
	}
	return *e, true
}

// matching returns the labels from have that appear in want, compared
// exactly and case-insensitively, in have's order and spelling.
func matching(have, want []string) []string {
	var out []string
	for _, l := range have {
		t := strings.TrimSpace(l)
		if t == "" {
			continue
		}
		if slices.ContainsFunc(want, func(w string) bool { return strings.EqualFold(strings.TrimSpace(w), t) }) {
			out = append(out, l)
		}
	}
	return out
}

// Sweep folds one enumeration pass into the ledger and returns the actions
// due now. obs should be every open PR the pass saw that might be held.
func (s *Store) Sweep(obs []Observation, now time.Time, opts Options) []Action {
	s.mu.Lock()
	defer s.mu.Unlock()
	var actions []Action
	changed := false
	for _, o := range obs {
		if o.Number <= 0 || strings.TrimSpace(o.Repo) == "" {
			continue
		}
		key := Key(o.Repo, o.Number)
		holds := matching(o.Labels, opts.HoldLabels)
		var release []string
		if opts.ReleaseLabel != "" {
			release = matching(o.Labels, []string{opts.ReleaseLabel})
		}
		if len(release) > 0 && o.CI == CIGreen && o.HeadSHA != "" {
			// The setter asked for exactly this: drop the hold now that CI
			// is green. A ledger entry is not needed for it.
			actions = append(actions, Action{
				Kind: ActionRelease, Repo: o.Repo, Number: o.Number, HeadSHA: o.HeadSHA,
				HoldLabels: holds, ReleaseLabel: release[0],
			})
			if _, ok := s.entries[key]; ok {
				delete(s.entries, key)
				changed = true
			}
			continue
		}
		if len(holds) == 0 {
			// Not held (any more): a later hold is a new incident.
			if _, ok := s.entries[key]; ok {
				delete(s.entries, key)
				changed = true
			}
			continue
		}
		e := s.entries[key]
		if e == nil || (o.HeadSHA != "" && e.HeadSHA != o.HeadSHA) {
			// A new head re-arms the notification.
			e = &Entry{HeadSHA: o.HeadSHA}
			s.entries[key] = e
		}
		e.LastSeen = now
		changed = true
		switch o.CI {
		case CIGreen:
			if e.GreenSince.IsZero() {
				e.GreenSince = now
			}
		case CIRed:
			e.GreenSince = time.Time{}
			continue
		default:
			continue
		}
		greenFor := now.Sub(e.GreenSince)
		if opts.After > 0 && greenFor >= opts.After && e.NotifiedSHA != e.HeadSHA {
			actions = append(actions, Action{
				Kind: ActionNotify, Repo: o.Repo, Number: o.Number, HeadSHA: e.HeadSHA,
				HoldLabels: holds, GreenFor: greenFor,
			})
		}
	}
	for k, e := range s.entries {
		if now.Sub(e.LastSeen) > PruneAfter {
			delete(s.entries, k)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
	return actions
}

// MarkNotified records that the notification for head was delivered, so the
// PR is not raised again until a new head is held and green.
func (s *Store) MarkNotified(repo string, number int, head string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e := s.entries[Key(repo, number)]
	if e == nil || e.HeadSHA != head {
		return
	}
	e.NotifiedSHA = head
	s.saveLocked()
}

func (s *Store) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.MarshalIndent(s.entries, "", "  ")
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(s.path), ledgerDirMode) != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, ledgerFileMode) == nil {
		_ = os.Rename(tmp, s.path)
	}
}
