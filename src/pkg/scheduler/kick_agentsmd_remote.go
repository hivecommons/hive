package scheduler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux/agentsmd"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
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
	owner, name, key, root, ok := s.remoteRepoDir(repo)
	if !ok {
		return ""
	}

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
		if github.IsNotFound(err) {
			// The file was removed upstream: drop the cached copy so a deleted
			// AGENTS.md stops reaching kicks, and so primeSkills, which may
			// still parse this root for skills/ files, does not read stale
			// inline skills from it (#20009).
			//
			// GitHub also answers 404 when it denies access (a revoked
			// installation, a repo made private), so dropping a copy we held
			// is logged at warn: injection stops until the TTL lapses.
			if have && cached.root != "" {
				s.logger.Warn("agentsmd: GitHub returned 404 for a cached AGENTS.md, removing the cached copy",
					"repo", key, "root", root)
			}
			removeCachedFile(filepath.Join(root, agentsmd.AgentsFileName))
			s.remoteAgents[key] = remoteAgentsMd{fetched: now}
			return ""
		}
		if have && cached.root != "" {
			// Last-known-good: a transient API failure must not strip the
			// repo's instructions from a kick. Retry on the next kick.
			s.logger.Warn("agentsmd: refresh from GitHub failed, serving cached copy",
				"repo", key, "root", cached.root, "error", err)
			return cached.root
		}
		// Any other failure with nothing cached is a miss for this kick,
		// said once per TTL.
		s.logger.Info("agentsmd: no AGENTS.md fetched from GitHub", "repo", key, "error", err)
		s.remoteAgents[key] = remoteAgentsMd{fetched: now}
		return ""
	}

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

// remoteRepoDir resolves repo to its owner, name, "owner/name" cache key and
// the local directory its fetched files are materialised in. ok is false when
// no scheduler config is attached or either segment could escape
// remoteAgentsMdDir.
func (s *Scheduler) remoteRepoDir(repo string) (owner, name, key, root string, ok bool) {
	if s.cfg == nil {
		return "", "", "", "", false
	}
	owner, name, cut := strings.Cut(config.QualifyRepo(s.cfg.Project.Org, repo), "/")
	owner, name = strings.TrimSpace(owner), strings.TrimSpace(name)
	if !cut || owner == "" || name == "" || !safeRepoPathSegment(owner) || !safeRepoPathSegment(name) {
		return "", "", "", "", false
	}
	return owner, name, owner + "/" + name, filepath.Join(remoteAgentsMdDir, owner, name), true
}

// isRemoteRepoRoot reports whether root is the GitHub-fetched cache root for
// repo, or empty, meaning no local checkout supplied it. Only then may
// primeSkills fetch the repo's skills/ files from the forge: a local checkout
// is authoritative for its own skills/ directory.
func (s *Scheduler) isRemoteRepoRoot(repo, root string) bool {
	if root == "" {
		return true
	}
	_, _, _, remote, ok := s.remoteRepoDir(repo)
	return ok && root == remote
}

// syncRemoteRepoSkills fetches "skills/<name>.md" from repo's default branch
// for each of names into the remote cache root and returns that root, or ""
// when no fetcher is wired or repo cannot be cached (#20009).
//
// Only the names an agent declares and the registry lacks are passed in, so a
// skill still goes only to an agent that names it, and no directory listing is
// needed. Each file follows the AGENTS.md cache rules: one conditional GET per
// skill per remoteAgentsMdTTL, a 404 deletes the cached copy (a skill removed
// upstream must leave the prompt), and any other failure keeps serving the
// last-known-good copy until the next TTL (so a GitHub outage costs one
// request per skill per TTL, not one per kick). A file larger than
// maxSkillsInjectionBytes is never
// written, because it could never be injected. Skill text comes from the same
// branch as AGENTS.md and is trusted the same way.
func (s *Scheduler) syncRemoteRepoSkills(repo string, names []string) string {
	fetcher := s.agentsMdFetcherOrNil()
	if fetcher == nil || len(names) == 0 {
		return ""
	}
	owner, name, key, root, ok := s.remoteRepoDir(repo)
	if !ok {
		return ""
	}
	skillsDir := filepath.Join(root, agentsmd.SkillsDirName)

	s.remoteAgentsMu.Lock()
	defer s.remoteAgentsMu.Unlock()
	if s.remoteSkills == nil {
		s.remoteSkills = map[string]time.Time{}
	}
	for _, skill := range names {
		skill = strings.TrimSpace(skill)
		if !safeSkillFileName(skill) {
			s.logger.Warn("skillreg: refusing to fetch repo skill with an unsafe name", "repo", key, "skill", skill)
			continue
		}
		cacheKey := key + "/" + skill
		now := time.Now()
		if fetched, have := s.remoteSkills[cacheKey]; have && now.Sub(fetched) < remoteAgentsMdTTL {
			continue
		}
		path := filepath.Join(skillsDir, skill+agentsmd.SkillFileExt)
		ctx, cancel := context.WithTimeout(context.Background(), remoteAgentsMdFetchTimeout)
		body, err := fetcher.GetFileContentRef(ctx, owner, name, agentsmd.SkillsDirName+"/"+skill+agentsmd.SkillFileExt, "")
		cancel()
		switch {
		case err != nil && github.IsNotFound(err):
			if _, statErr := os.Lstat(path); statErr == nil {
				// 404 is also GitHub's answer to denied access, so say so.
				s.logger.Warn("skillreg: GitHub returned 404 for a cached repo skill, removing the cached copy",
					"repo", key, "skill", skill)
			}
			removeCachedFile(path)
			s.remoteSkills[cacheKey] = now
		case err != nil:
			// Last-known-good: keep serving any cached copy and retry after the
			// TTL rather than on every kick.
			if _, statErr := os.Lstat(path); statErr == nil {
				s.logger.Warn("skillreg: refresh of repo skill from GitHub failed, serving cached copy",
					"repo", key, "skill", skill, "error", err)
			} else {
				s.logger.Info("skillreg: repo skill not fetched from GitHub", "repo", key, "skill", skill, "error", err)
			}
			s.remoteSkills[cacheKey] = now
		case strings.TrimSpace(body) == "":
			// An empty file, or a path the contents API did not resolve to a
			// file (a symlink out of the repo has no content): nothing to inject.
			removeCachedFile(path)
			s.remoteSkills[cacheKey] = now
		case len(body) > maxSkillsInjectionBytes:
			removeCachedFile(path)
			s.logger.Warn("skillreg: repo skill exceeds the injection cap, not caching it",
				"repo", key, "skill", skill, "bytes", len(body), "cap_bytes", maxSkillsInjectionBytes)
			s.remoteSkills[cacheKey] = now
		default:
			if wErr := writeFileAtomic(path, []byte(body)); wErr != nil {
				s.logger.Warn("skillreg: cannot materialise fetched repo skill", "repo", key, "skill", skill, "error", wErr)
				continue
			}
			s.logger.Info("skillreg: fetched repo skill from GitHub", "repo", key, "skill", skill, "bytes", len(body))
			s.remoteSkills[cacheKey] = now
		}
	}
	return root
}

// safeSkillFileName accepts a declared skill name only when it is a plain file
// name that stays inside the cache's skills/ directory: no path separators, no
// "." or "..", and no leading dot (which would also hide the file).
func safeSkillFileName(name string) bool {
	return name != "" && safeRepoPathSegment(name) && !strings.HasPrefix(name, ".") &&
		!strings.ContainsRune(name, 0)
}

// removeCachedFile deletes a cached copy; a file that is already gone is fine.
func removeCachedFile(path string) {
	_ = os.Remove(path)
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
