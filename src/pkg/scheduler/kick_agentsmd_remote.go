package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agentsmd"
	"github.com/hivecommons/hive/pkg/config"
)

// AgentsMdFetcher is the one GitHub call the remote AGENTS.md fallback needs.
// *github.Client satisfies it; the interface exists so tests can fake it and so
// a token-less boot can leave it unset.
type AgentsMdFetcher interface {
	GetFileContentRef(ctx context.Context, owner, repo, path, ref string) (string, error)
}

const (
	// remoteAgentsMdTTL is how long one fetched AGENTS.md serves kicks before
	// the scheduler asks GitHub again. The shared transport sends
	// If-None-Match, so a refresh of an unchanged file is a free 304 — the
	// TTL only bounds how often that round trip happens, not quota.
	remoteAgentsMdTTL = 15 * time.Minute
	// remoteAgentsMdFetchTimeout bounds one fetch so a slow API cannot stall
	// a kick; on timeout the last-known-good copy (if any) is served.
	remoteAgentsMdFetchTimeout = 10 * time.Second
)

// remoteAgentsMdDir is where fetched AGENTS.md files are materialised, as
// "<dir>/<owner>/<repo>/AGENTS.md", so agentsmd.Parse and primeSkills read it
// exactly as they would a checkout. A var only so tests can point it at a
// temp dir.
var remoteAgentsMdDir = "/data/agentsmd-cache"

// remoteAgentsMd is one repo's fetch state. root is "" when the last fetch
// found no AGENTS.md (negative cache: a repo without the file is not asked
// about again until the TTL lapses).
type remoteAgentsMd struct {
	root    string
	fetched time.Time
}

// SetAgentsMdFetcher attaches the GitHub client the scheduler falls back to
// when a hive has no local checkout of its primary repo (hivecommons/hive#17090).
// Nil leaves the fallback off, which is the pre-existing behaviour.
func (s *Scheduler) SetAgentsMdFetcher(f AgentsMdFetcher) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.agentsMdFetcher = f
}

func (s *Scheduler) agentsMdFetcherOrNil() AgentsMdFetcher {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.agentsMdFetcher
}

// remoteAgentsRepoRoot returns a local root holding repo's AGENTS.md fetched
// from the forge at its default branch, or "" when no fetcher is wired, the
// repo has no AGENTS.md, or the fetch failed with nothing cached.
//
// Hosted spokes keep no clone of the repos they work on — agents go through
// the API — so without this the two local roots agentsRepoRoot knows are
// always empty and the repo's instructions never reach a kick. The file is
// written under remoteAgentsMdDir so the rest of the priming path (Parse,
// Render, the injection cap, primeSkills' repo-local fallback) is unchanged
// and the log line's root= says where the text came from.
func (s *Scheduler) remoteAgentsRepoRoot(repo string) string {
	fetcher := s.agentsMdFetcherOrNil()
	if fetcher == nil || s.cfg == nil {
		return ""
	}
	owner, name, ok := strings.Cut(config.QualifyRepo(s.cfg.Project.Org, repo), "/")
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !ok || owner == "" || name == "" || !safeRepoPathSegment(owner) || !safeRepoPathSegment(name) {
		return ""
	}
	key := owner + "/" + name

	s.remoteAgentsMu.Lock()
	defer s.remoteAgentsMu.Unlock()
	if s.remoteAgents == nil {
		s.remoteAgents = map[string]remoteAgentsMd{}
	}
	now := time.Now()
	cached, have := s.remoteAgents[key]
	if have && now.Sub(cached.fetched) < remoteAgentsMdTTL {
		return cached.root
	}

	ctx, cancel := context.WithTimeout(context.Background(), remoteAgentsMdFetchTimeout)
	defer cancel()
	body, err := fetcher.GetFileContentRef(ctx, owner, name, agentsmd.AgentsFileName, "")
	if err != nil {
		if have && cached.root != "" {
			// Last-known-good: a transient API failure must not strip the
			// repo's instructions from a kick. Retry on the next kick.
			s.logger.Warn("agentsmd: refresh from GitHub failed, serving cached copy",
				"repo", key, "root", cached.root, "error", err)
			return cached.root
		}
		// A 404 is the normal "repo has no AGENTS.md" answer; everything else
		// is still just a miss for this kick, said once per TTL.
		s.logger.Info("agentsmd: no AGENTS.md fetched from GitHub", "repo", key, "error", err)
		s.remoteAgents[key] = remoteAgentsMd{fetched: now}
		return ""
	}

	root := filepath.Join(remoteAgentsMdDir, owner, name)
	if err := writeFileAtomic(filepath.Join(root, agentsmd.AgentsFileName), []byte(body)); err != nil {
		s.logger.Warn("agentsmd: cannot materialise fetched AGENTS.md, skipping injection",
			"repo", key, "root", root, "error", err)
		if have && cached.root != "" {
			return cached.root
		}
		return ""
	}
	s.logger.Info("agentsmd: fetched AGENTS.md from GitHub", "repo", key, "bytes", len(body), "root", root)
	s.remoteAgents[key] = remoteAgentsMd{root: root, fetched: now}
	return root
}

// safeRepoPathSegment refuses an owner or repo name that could escape
// remoteAgentsMdDir when joined into a path. Names come from config, which
// is operator-written, so this is belt-and-braces rather than a trust gate.
func safeRepoPathSegment(seg string) bool {
	return seg != "." && seg != ".." && !strings.ContainsAny(seg, `/\`)
}

// writeFileAtomic writes data to path via a sibling temp file and rename so a
// kick reading AGENTS.md concurrently never sees a half-written file.
func writeFileAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentsmd-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}
