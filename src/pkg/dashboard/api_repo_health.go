package dashboard

import (
	"net/http"
	"strings"
	"sync"

	"github.com/hivecommons/hive/pkg/search"
)

// repoHealthSource holds the search indexer's per-repo health lookup. The
// score is report-only (hivecommons/hive#14638): it is served here and never
// read by merge decisions, ACMM or Governor priority. Spoke-only: it is not
// part of any hub heartbeat.
type repoHealthSource struct {
	mu sync.RWMutex
	fn func(repo string) (search.Health, bool)
}

// SetRepoHealthSource wires GET /api/repos/{owner}/{repo}/health to the
// search indexer; nil (the default) means search is off.
func (s *Server) SetRepoHealthSource(fn func(repo string) (search.Health, bool)) {
	s.repoHealth.mu.Lock()
	defer s.repoHealth.mu.Unlock()
	s.repoHealth.fn = fn
}

func (s *Server) repoHealthLookup() func(string) (search.Health, bool) {
	s.repoHealth.mu.RLock()
	defer s.repoHealth.mu.RUnlock()
	return s.repoHealth.fn
}

// handleRepoHealth serves GET /api/repos/{owner}/{repo}/health: the repo's
// health score as of the last search index rebuild, with every component's
// inputs and n so the score can be disputed.
func (s *Server) handleRepoHealth(w http.ResponseWriter, r *http.Request) {
	owner := strings.TrimSpace(r.PathValue("owner"))
	repo := strings.TrimSpace(r.PathValue("repo"))
	full := owner + "/" + repo
	if owner == "" || repo == "" || !validPRThroughputRepo(full) {
		jsonError(w, "invalid repository", http.StatusBadRequest)
		return
	}
	lookup := s.repoHealthLookup()
	if lookup == nil {
		jsonError(w, "repo health unavailable: search is not enabled", http.StatusServiceUnavailable)
		return
	}
	h, ok := lookup(full)
	if !ok {
		jsonError(w, "no health computed for "+full+" yet: no review evidence indexed", http.StatusNotFound)
		return
	}
	jsonResponse(w, h)
}
