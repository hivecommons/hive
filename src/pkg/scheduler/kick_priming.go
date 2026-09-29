package scheduler

import (
	"context"
	"strings"

	"github.com/hivecommons/hive/pkg/agentsmd"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/skillreg"
)

const maxIssuesToPrime = 5

// agentsRepoRoot returns the local filesystem root of repo's checkout, or ""
// when the hive has none — in which case primeAgentsMd is a no-op, exactly as
// before this was wired.
//
// Two sources, in order:
//
//  1. project.checkouts_dir — the explicit one. An operator who mounts checkouts
//     of the monitored repos points at the parent directory and each repo is
//     found at "<dir>/<name>". Hive agents work over the API and keep no clones
//     of their own, so this is how a root gets to exist at all.
//  2. policies.local_dir — the git source LocalDir the original TODO(agentsmd)
//     named. It is a real checkout root (buildPolicyPaths already reads files
//     out of it), but it is a checkout of policies.repo, so it is used ONLY when
//     that repo IS the repo being asked about. Otherwise it would inject the
//     config repo's AGENTS.md into work on an unrelated repo.
//
// Neither is checked for existence here: agentsmd.Parse tolerates a missing
// directory or file and yields "", and primeAgentsMd logs which root came up
// empty. One stat per kick to say the same thing earlier is not worth it.
func (s *Scheduler) agentsRepoRoot(repo string) string {
	if s.cfg == nil {
		return ""
	}
	if root := s.cfg.Project.CheckoutRootFor(repo); root != "" {
		return root
	}
	if local := strings.TrimSpace(s.cfg.Policies.LocalDir); local != "" &&
		sameRepo(s.cfg.Policies.Repo, s.cfg.Project.Org, repo) {
		return local
	}
	return ""
}

// sameRepo reports whether a git source's repo field names org/repo. The source
// field is written as a clone URL in practice ("https://host/org/name(.git)"),
// but a bare "org/name" and a bare "name" are accepted too, since config is
// hand-written and all three forms appear in the wild.
func sameRepo(sourceRepo, org, repo string) bool {
	src := strings.TrimSpace(sourceRepo)
	name := strings.TrimSpace(repo)
	if src == "" || name == "" {
		return false
	}
	if i := strings.LastIndex(name, "/"); i >= 0 {
		name = name[i+1:]
	}
	src = strings.TrimSuffix(strings.TrimSuffix(strings.TrimRight(src, "/"), ".git"), "/")
	parts := strings.Split(src, "/")
	srcName := parts[len(parts)-1]
	srcOwner := ""
	if len(parts) >= 2 {
		srcOwner = parts[len(parts)-2]
	}
	if !strings.EqualFold(srcName, name) {
		return false
	}
	// An owner on both sides must agree; a bare "name" source asserts no owner
	// and matches on name alone.
	if srcOwner != "" && strings.TrimSpace(org) != "" {
		return strings.EqualFold(srcOwner, strings.TrimSpace(org))
	}
	return true
}

// primeAgentsMd reads the repository's AGENTS.md (the cross-tool convention for
// per-repo agent instructions) plus its requested skills and returns text to
// prepend to the kick. It is tolerant: a missing or malformed file yields "".
func (s *Scheduler) primeAgentsMd(repoRoot string) string {
	if repoRoot == "" {
		return ""
	}
	cfg, err := agentsmd.Parse(repoRoot, s.logger)
	if err != nil {
		// Parse is tolerant and should not error; log defensively and skip.
		s.logger.Warn("agentsmd: parse failed, skipping injection", "root", repoRoot, "error", err)
		return ""
	}
	section := cfg.InjectionText(nil)
	if section == "" {
		// The silent half of kubestellar/hive#5227: a wired root holding no
		// AGENTS.md produced exactly the same nothing as an unwired scheduler,
		// so neither state was observable. Say which one this is.
		s.logger.Debug("agentsmd: no repo instructions to inject", "root", repoRoot)
		return ""
	}
	s.logger.Info("agentsmd: injecting repo instructions into kick",
		"root", repoRoot,
		"requested_skills", len(cfg.RequestedSkills),
		"chars", len(section),
	)
	return section
}

// skillsRegistryDir is the host-local directory the skill registry is loaded
// from at kick time. It matches the directory the dashboard reports on, so the
// count shown in the UI and the skills actually injected come from one place.
// It is a var, not a const, only so tests can point it at a temp dir.
var skillsRegistryDir = "/data/skills"

// maxSkillsInjectionBytes caps how much registry skill text may be prepended to
// a single kick. Skill bodies are operator-authored files of unbounded size, and
// the kick prompt shares a context budget with the knowledge primer and the
// issue/PR lists; without a cap one long skill file could crowd out the actual
// work queue. Skills are dropped whole (never truncated mid-body) so an agent
// never receives half an instruction.
const maxSkillsInjectionBytes = 8192

// primeSkills resolves the skills agentName declares in config against the
// host-local skill registry, falling back to repo-local AGENTS.md skills, and
// renders them for injection into the kick. A registry skill wins when both
// sources define the same name.
//
// It is tolerant at every step: no declared skills, missing registry or repo
// files, and names with no matching skill all yield "" rather than an error, so
// a misconfigured skill degrades the kick instead of blocking the agent.
// Loading happens per kick (not once at startup) so an operator editing either
// source sees it take effect on the next kick without a restart.
func (s *Scheduler) primeSkills(agentName, repoRoot string) string {
	if s.cfg == nil {
		return ""
	}
	ac, ok := s.cfg.Agents[agentName]
	if !ok {
		return ""
	}
	requested := ac.Skills
	if ac.AgentSpec != "" {
		requested = nil
		spec, err := skillreg.LoadAgentSpec(ac.AgentSpec)
		if err != nil {
			s.logger.Warn("skillreg: cannot load agent spec skills",
				"agent", agentName, "spec", ac.AgentSpec, "error", err)
		} else {
			requested = spec.Skills
		}
	}
	if len(requested) == 0 {
		return ""
	}

	var repoCfg *agentsmd.AgentsConfig
	if repoRoot != "" {
		var err error
		repoCfg, err = agentsmd.Parse(repoRoot, s.logger)
		if err != nil {
			// Parse is tolerant and should not error; log defensively and keep
			// resolving against the registry.
			s.logger.Warn("skillreg: cannot parse repo skills, continuing with registry",
				"agent", agentName, "root", repoRoot, "error", err)
			repoCfg = nil
		}
	}

	reg := skillreg.NewRegistry()
	loaded, err := reg.Load(skillsRegistryDir, s.logger)
	if err != nil {
		// An unexpected registry failure must not suppress a valid repo-local
		// fallback.
		s.logger.Warn("skillreg: cannot load skills registry, continuing with repo skills",
			"dir", skillsRegistryDir, "error", err)
	}

	resolved := reg.ResolveRequested(repoCfg, requested)
	if len(resolved) == 0 {
		if loaded == 0 && (repoCfg == nil || len(repoCfg.Skills) == 0) {
			s.logger.Debug("skillreg: no skills available, skipping injection",
				"dir", skillsRegistryDir, "repo_root", repoRoot,
				"requested", len(requested))
			return ""
		}
		s.logger.Warn("skillreg: none of the declared skills resolved",
			"agent", agentName, "requested", requested, "registry_size", loaded,
			"repo_root", repoRoot)
		return ""
	}

	kept, dropped := capSkills(resolved, maxSkillsInjectionBytes)
	if len(kept) == 0 {
		s.logger.Warn("skillreg: all resolved skills exceed the injection cap, skipping",
			"agent", agentName, "cap_bytes", maxSkillsInjectionBytes)
		return ""
	}
	section := skillreg.InjectionText(kept)
	s.logger.Info("skillreg: injecting skills into kick",
		"agent", agentName,
		"dir", skillsRegistryDir,
		"repo_root", repoRoot,
		"injected", len(kept),
		"dropped", len(dropped),
		"chars", len(section),
	)
	if len(dropped) > 0 {
		s.logger.Warn("skillreg: dropped skills over the injection cap",
			"agent", agentName, "dropped", dropped, "cap_bytes", maxSkillsInjectionBytes)
	}
	return section
}

// capSkills keeps skills in request order until adding the next one would push
// the rendered body past capBytes. It returns the kept skills and the names of
// those dropped. A single skill larger than capBytes is dropped rather than
// truncated, so an agent never receives a partial instruction. Later, smaller
// skills are still considered after a large one is dropped, so one oversized
// file does not silently suppress everything declared after it.
func capSkills(skills []skillreg.Skill, capBytes int) (kept []skillreg.Skill, dropped []string) {
	used := 0
	for _, sk := range skills {
		size := len(sk.Body)
		if used+size > capBytes {
			dropped = append(dropped, sk.Name)
			continue
		}
		used += size
		kept = append(kept, sk)
	}
	return kept, dropped
}

// primeKnowledge queries the wiki layers for facts relevant to the given issues
// and returns a formatted section for injection into the kick message.
func (s *Scheduler) primeKnowledge(issues []github.Issue) string {
	s.mu.RLock()
	primer := s.primer
	s.mu.RUnlock()
	if primer == nil || len(issues) == 0 {
		return ""
	}

	limit := maxIssuesToPrime
	if len(issues) < limit {
		limit = len(issues)
	}

	keywords := extractKeywords(issues[:limit])
	if len(keywords) == 0 {
		s.logger.Debug("knowledge primer: no keywords extracted from issues", "issue_count", len(issues))
		return ""
	}

	s.logger.Info("knowledge primer: searching", "keywords", len(keywords), "sample", keywordSample(keywords))
	primed := primer.Prime(context.Background(), nil, keywords)
	result := primed.FormatForPrompt()
	if result != "" {
		s.logger.Info("knowledge primer: injecting facts into kick", "facts", len(primed.Facts), "chars", len(result))
	}
	return result
}

// extractKeywords pulls searchable terms from issue labels and titles.
// Title words are included because labels alone are often all noise
// (triage/accepted, kind/bug) and produce zero keywords after filtering.
func extractKeywords(issues []github.Issue) []string {
	seen := make(map[string]bool)
	var keywords []string

	for _, issue := range issues {
		for _, label := range issue.Labels {
			lower := strings.ToLower(label)
			if !seen[lower] && !isNoiseLabel(lower) {
				keywords = append(keywords, lower)
				seen[lower] = true
			}
		}

		if issue.ComplexityTier != "" {
			tier := strings.ToLower(issue.ComplexityTier)
			if !seen[tier] {
				keywords = append(keywords, tier)
				seen[tier] = true
			}
		}

		for _, word := range splitTitleWords(issue.Title) {
			if !seen[word] && !isNoiseWord(word) {
				keywords = append(keywords, word)
				seen[word] = true
			}
		}
	}

	return keywords
}

// splitTitleWords extracts lowercase words from an issue title, dropping
// short words and punctuation.
func splitTitleWords(title string) []string {
	const minWordLen = 3
	var words []string
	for _, word := range strings.Fields(strings.ToLower(title)) {
		clean := strings.TrimFunc(word, func(r rune) bool {
			return (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' && r != '_'
		})
		if len(clean) >= minWordLen {
			words = append(words, clean)
		}
	}
	return words
}

var noiseWords = map[string]bool{
	"the": true, "and": true, "for": true, "not": true,
	"are": true, "but": true, "with": true, "this": true,
	"that": true, "from": true, "have": true, "has": true,
	"was": true, "were": true, "been": true, "being": true,
	"does": true, "did": true, "will": true, "would": true,
	"should": true, "could": true, "can": true, "may": true,
	"add": true, "fix": true, "update": true, "remove": true,
	"issue": true, "bug": true, "error": true, "when": true,
	"after": true, "before": true, "into": true, "about": true,
}

func isNoiseWord(word string) bool {
	return noiseWords[word]
}

func keywordSample(keywords []string) string {
	const maxSampleKeywords = 8
	n := len(keywords)
	if n > maxSampleKeywords {
		n = maxSampleKeywords
	}
	return strings.Join(keywords[:n], ", ")
}

var noiseLabels = map[string]bool{
	"triage/accepted":  true,
	"ai-fix-requested": true,
	"kind/bug":         true,
	"kind/feature":     true,
	"kind/task":        true,
	"good first issue": true,
	"help wanted":      true,
	"hold":             true,
}

func isNoiseLabel(label string) bool {
	return noiseLabels[label]
}
