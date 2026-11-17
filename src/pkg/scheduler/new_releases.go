package scheduler

// NewReleasesSource renders the ${NEW_RELEASES} block for an agent: the
// upstream releases the release-feed watcher (hivecommons/hive#18494) found
// and has not yet delivered. "" means none.
type NewReleasesSource func(agentName string) string

// newReleasesHolder wraps the installed source so atomic.Pointer can hold a
// func value.
type newReleasesHolder struct{ fn NewReleasesSource }

// SetNewReleasesSource installs (or with nil, removes) the ${NEW_RELEASES}
// source. It is an atomic swap, not s.mu, because template substitution may
// run while s.mu is held.
func (s *Scheduler) SetNewReleasesSource(fn NewReleasesSource) {
	if fn == nil {
		s.newReleases.Store(nil)
		return
	}
	s.newReleases.Store(&newReleasesHolder{fn: fn})
}

// newReleasesBlock is the ${NEW_RELEASES} value for agentName.
func (s *Scheduler) newReleasesBlock(agentName string) string {
	if s == nil {
		return ""
	}
	h := s.newReleases.Load()
	if h == nil || h.fn == nil {
		return ""
	}
	return h.fn(agentName)
}
