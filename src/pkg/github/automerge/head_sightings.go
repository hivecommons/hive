package automerge

// Durable first sighting of each PR head (Doug's decision on hivecommons/hive#16817,
// the adjudicator's backdating finding).
//
// An approval reaches a later base-merge head by the date fallback only when
// the content it approved existed when it was given. A commit's date is
// whatever the pusher's git wrote, so it is trusted only together with the
// hive's own clock: the time the hive first listed that commit as the PR's
// head. HeadFirstSeenTracker holds a similar time in memory, pruned after a
// week and lost on restart; this record survives both, so a carry-over can
// be judged days later. A head with no record (seen before this existed, or
// never seen as a head) is unknown and carries nothing by date.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/stagelabels"
)

// headSightingsRetention drops sightings older than this: a PR head the
// hive first saw half a year ago is not going to carry an approval forward.
const headSightingsRetention = 180 * 24 * time.Hour

// HeadSightings is the persisted (repo#pr@sha → first seen) record.
type HeadSightings struct {
	mu     sync.Mutex
	path   string
	loaded bool
	first  map[string]time.Time
}

var sharedHeadSightings = NewHeadSightings(filepath.Join(stagelabels.DefaultStateDir, "pr-head-sightings.json"))

// NewHeadSightings returns a record persisted at path ("" keeps it in memory).
func NewHeadSightings(path string) *HeadSightings {
	return &HeadSightings{path: path, first: map[string]time.Time{}}
}

func headSightingKey(fullRepo string, number int, sha string) string {
	return strings.ToLower(strings.TrimSpace(fullRepo)) + "#" + strconv.Itoa(number) + "@" + strings.TrimSpace(sha)
}

func (s *HeadSightings) loadLocked() {
	if s.loaded {
		return
	}
	s.loaded = true
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var file map[string]time.Time
	if json.Unmarshal(data, &file) == nil {
		for k, v := range file {
			s.first[k] = v
		}
	}
}

func (s *HeadSightings) saveLocked() {
	if s.path == "" {
		return
	}
	data, err := json.Marshal(s.first)
	if err != nil {
		return
	}
	if os.MkdirAll(filepath.Dir(s.path), 0o755) != nil {
		return
	}
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o644) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

// Sight records now for each head not seen before and saves once.
func (s *HeadSightings) Sight(fullRepo string, heads map[int]string, now time.Time) {
	if s == nil || len(heads) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	changed := false
	for number, sha := range heads {
		if strings.TrimSpace(sha) == "" || number <= 0 {
			continue
		}
		k := headSightingKey(fullRepo, number, sha)
		if _, ok := s.first[k]; !ok {
			s.first[k] = now
			changed = true
		}
	}
	if !changed {
		return
	}
	for k, at := range s.first {
		if now.Sub(at) > headSightingsRetention {
			delete(s.first, k)
		}
	}
	s.saveLocked()
}

// FirstSeen is when the hive first listed sha as the PR's head, false when
// it has no record.
func (s *HeadSightings) FirstSeen(fullRepo string, number int, sha string) (time.Time, bool) {
	if s == nil {
		return time.Time{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loadLocked()
	at, ok := s.first[headSightingKey(fullRepo, number, sha)]
	return at, ok
}
