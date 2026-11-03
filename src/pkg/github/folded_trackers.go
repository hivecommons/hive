package github

import (
	"strconv"
	"sync"
)

// foldedTrackerSet remembers issues that a same-component fold (#11239) landed
// on, so the scanner work list can name them (#11587). It is in-memory: after a
// restart a tracker reappears the next time a finding folds into it. The zero
// value is ready to use.
type foldedTrackerSet struct {
	mu   sync.Mutex
	refs map[string]bool
}

func foldedTrackerKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

func (s *foldedTrackerSet) add(repo string, number int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refs == nil {
		s.refs = map[string]bool{}
	}
	s.refs[foldedTrackerKey(repo, number)] = true
}

func (s *foldedTrackerSet) has(repo string, number int) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refs[foldedTrackerKey(repo, number)]
}

// annotateFoldedTrackers flags enumerated issues that received a folded finding.
func (c *Client) annotateFoldedTrackers(issues []Issue) {
	for i := range issues {
		if c.foldedTrackers.has(issues[i].Repo, issues[i].Number) {
			issues[i].FoldedTracker = true
		}
	}
}
