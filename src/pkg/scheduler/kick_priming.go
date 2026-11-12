package scheduler

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"

	"github.com/hivecommons/hive/crux/agentsmd"
	"github.com/hivecommons/hive/crux/skillreg"
	"github.com/hivecommons/hive/pkg/authorkind"
	"github.com/hivecommons/hive/pkg/claims"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/issueshape"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/stagelabels"
)

const maxIssuesToPrime = 5

// agentsRepoRoot returns the local filesystem root of repo's checkout, or ""
// when the hive has none — in which case primeAgentsMd is a no-op, exactly as
// before this was wired.
//
// Three sources, in order:
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
//  3. the forge — when neither local root is configured and a GitHub client is
//     wired (SetAgentsMdFetcher), AGENTS.md is fetched from the repo's default
//     branch and materialised under remoteAgentsMdDir (#17090). Hosted spokes
//     keep no clones at all, so without this they never injected anything.
//
// Neither local root is checked for existence here: agentsmd.Parse tolerates a missing
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
	return s.remoteAgentsRepoRoot(repo)
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
	capBytes := s.agentsMdMaxBytes()
	section, omitted := cfg.Render(nil, capBytes)
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
	if omitted.Any() {
		// The kick carries its own warning line; this is the operator-side
		// half, so the repo owner learns the file outgrew the cap from the log
		// rather than from an agent that missed a rule (#12129).
		// capBytes 0 is the config "use default" sentinel; log the cap that
		// applied so the operator can act on it (#17318).
		effectiveCap := capBytes
		if effectiveCap <= 0 {
			effectiveCap = agentsmd.DefaultMaxInjectionBytes
		}
		s.logger.Warn("agentsmd: AGENTS.md exceeded the injection cap; sections dropped whole",
			"root", repoRoot, "cap_bytes", effectiveCap, "whole_file", omitted.WholeFile,
			"omitted_sections", omitted.Sections, "omitted_bytes", omitted.Bytes)
	}
	return section
}

// agentsMdMaxBytes is project.agents_md_max_bytes, or zero for the agentsmd
// default when no config is loaded.
func (s *Scheduler) agentsMdMaxBytes() int {
	if s.cfg == nil {
		return 0
	}
	return s.cfg.Project.AgentsMdMaxBytes
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

// knowledgeInjection is what one kick received from the knowledge primer
// (#17049): the rendered "# Relevant Knowledge" section and the numbers
// behind it. Primed is false when the primer did not run at all (knowledge
// disabled, no issues, no keywords), so a zero-byte result from a real query
// is distinguishable from no query.
type knowledgeInjection struct {
	Section       string
	Bytes         int
	Entries       int
	SkippedForCap int
	CapBytes      int
	Primed        bool
	// Keywords is the search query the primer ran (empty when the issues
	// gave none, so nothing was searched). Hits is how many entries the
	// search returned before the byte cap.
	Keywords []string
	Hits     int
}

// primeKnowledge queries the wiki layers for facts relevant to the given issues
// and returns a formatted section for injection into the kick message. The
// facts are restricted to agentName's knowledge.agent_scopes entry, if any.
// It logs the per-kick knowledge line itself; callers that also prepend an
// AGENTS.md or skills block use primeKnowledgeStats and log once with those
// sizes included.
func (s *Scheduler) primeKnowledge(agentName string, issues []github.Issue) string {
	inj := s.primeKnowledgeStats(agentName, issues)
	s.logKnowledgeInjection(agentName, inj, 0, 0)
	return inj.Section
}

// primeKnowledgeStats is primeKnowledge without the summary log line. It is
// the RESIDENT kick's primer: besides the injection it records what this kick
// carried for the agent — a fresh KickSummary (the mid-task advisor's kick
// identity, #17155) and the rendered slugs (KickPrimedSlugs, #17052).
func (s *Scheduler) primeKnowledgeStats(agentName string, issues []github.Issue) knowledgeInjection {
	s.mu.RLock()
	primer := s.primer
	cfg := s.cfg
	s.mu.RUnlock()
	if primer == nil {
		return knowledgeInjection{}
	}
	limit := maxIssuesToPrime
	if len(issues) < limit {
		limit = len(issues)
	}
	s.recordKickSummary(agentName, issues[:limit])
	inj, slugs := s.renderKnowledge(primer, cfg, agentName, issues[:limit])
	if inj.Primed {
		s.primedSlugs.Store(agentName, slugs)
	}
	return inj
}

// primeKnowledgeForTask is the "# Relevant Knowledge" section for a
// contributor's one assigned task in agentName's role (BuildAgentGuidanceForTask):
// the same query, agent scope, kick context and byte cap a resident kick uses,
// primed from that issue alone. It records nothing for the resident agent —
// no KickSummary and no KickPrimedSlugs — because no resident kick was sent:
// writing either would move the resident session's advisor identity and make
// its advisor hold back facts its own kick never carried. It logs one line
// naming the issue, the query and the hits (logTaskKnowledgeSource).
func (s *Scheduler) primeKnowledgeForTask(agentName string, task github.Issue) knowledgeInjection {
	s.mu.RLock()
	primer := s.primer
	cfg := s.cfg
	s.mu.RUnlock()
	if primer == nil {
		return knowledgeInjection{}
	}
	inj, _ := s.renderKnowledge(primer, cfg, agentName, []github.Issue{task})
	s.logTaskKnowledgeSource(agentName, task, inj)
	return inj
}

// logTaskKnowledgeSource says where a role guidance render's "# Relevant
// Knowledge" came from. The shared "knowledge: injected" line names only the
// role, so on its own it reads like a resident kick and cannot tell an
// on-topic section from one primed by generic words. There is no fallback to
// general entries: a task whose issue gives no keywords, or whose search
// matches nothing, gets no section, and this line says which.
func (s *Scheduler) logTaskKnowledgeSource(agentName string, task github.Issue, inj knowledgeInjection) {
	if s == nil || s.logger == nil {
		return
	}
	ref := "(no assigned issue)"
	if task.Repo != "" || task.Number != 0 {
		ref = fmt.Sprintf("issue %s#%d", task.Repo, task.Number)
	}
	query := strings.Join(inj.Keywords, " ")
	var msg, source string
	switch {
	case len(inj.Keywords) == 0:
		source = "none"
		msg = fmt.Sprintf("role guidance knowledge: nothing primed, %s has no title or labels to search on", ref)
	case inj.Hits == 0:
		source = "none"
		msg = fmt.Sprintf("role guidance knowledge: nothing primed, no entry matched %s (query=%q); there is no fallback to general entries", ref, query)
	default:
		source = "issue"
		msg = fmt.Sprintf("role guidance knowledge: primed from %s (query=%q, hits=%d)", ref, query, inj.Hits)
	}
	s.logger.Info(msg,
		"agent", agentName,
		"repo", task.Repo,
		"number", task.Number,
		"source", source,
		"query", query,
		"hits", inj.Hits,
		"entries", inj.Entries,
		"skipped_for_cap", inj.SkippedForCap,
		"injected_bytes", inj.Bytes,
	)
}

// renderKnowledge is the side-effect-free core both primers share: it queries
// the wiki layers for issues and renders the section, returning the slugs it
// rendered for a caller that records them. It writes no scheduler state other
// than the cap-log dedupe key, which only picks the log level.
func (s *Scheduler) renderKnowledge(primer *knowledge.Primer, cfg *config.Config, agentName string, issues []github.Issue) (knowledgeInjection, []string) {
	if len(issues) == 0 {
		return knowledgeInjection{}, nil
	}

	keywords := extractKeywordsFor(cfg, issues)
	if len(keywords) == 0 {
		s.logger.Debug("knowledge primer: no keywords extracted from issues", "issue_count", len(issues))
		return knowledgeInjection{}, nil
	}

	s.logger.Info("knowledge primer: searching", "keywords", len(keywords), "sample", keywordSample(keywords))
	var scope *knowledge.TOCScope
	if sc, ok := cfg.KnowledgeAgentScope(agentName); ok {
		agentScope := knowledge.AgentScope(sc.Layers, sc.Repos, sc.Types, sc.Tags, sc.IncludeStates)
		if knowledge.ListsDraft(sc.IncludeStates) {
			agentScope.DraftAuthors = kickDraftAuthors(cfg, agentName)
		}
		scope = &agentScope
	}
	kc := kickContextFor(cfg, agentName, issues)
	primed := primer.PrimeForKick(context.Background(), scope, kc, keywords)
	// The issue's own terms matched nothing: search again with the terms
	// earlier releases used, so leaving the hive's routing words out never
	// costs a kick the section it got before.
	if len(primed.Facts) == 0 {
		if legacy := legacyKeywords(issues); len(legacy) > 0 && !slices.Equal(legacy, keywords) {
			s.logger.Info("knowledge primer: no entry matched the issue terms, retrying with the full query",
				"sample", keywordSample(keywords), "full_sample", keywordSample(legacy))
			keywords = legacy
			primed = primer.PrimeForKick(context.Background(), scope, kc, keywords)
		}
	}
	result, omitted := primed.Render()
	inj := knowledgeInjection{
		Section:       result,
		Bytes:         len(result),
		Entries:       len(primed.Facts) - omitted.Facts,
		SkippedForCap: omitted.Facts,
		CapBytes:      primer.MaxBytes(),
		Primed:        true,
		Keywords:      keywords,
		Hits:          len(primed.Facts),
	}
	if omitted.Any() {
		// The render carries its own marker; this is the operator-side half so
		// a cap that is too tight for the vault shows up in logs, not only in
		// a kick transcript (#12124).
		// Steady state for a vault larger than the cap: INFO the first time
		// per (agent, cap) and whenever the omitted count changes, DEBUG after.
		lvl := slog.LevelDebug
		key := fmt.Sprintf("%s|%d", agentName, primed.MaxBytes)
		if prev, ok := s.primerCapLogged.Load(key); !ok || prev != omitted.Facts {
			s.primerCapLogged.Store(key, omitted.Facts)
			lvl = slog.LevelInfo
		}
		s.logger.Log(context.Background(), lvl, "knowledge primer: dropped facts over the byte cap",
			"omitted_facts", omitted.Facts, "omitted_hints", omitted.Hints,
			"omitted_bytes", omitted.Bytes, "cap_bytes", primed.MaxBytes)
	}
	return inj, primed.RenderedSlugs()
}

// kickDraftAuthors lists the identities whose draft knowledge a kick for
// agentName may carry once that agent's agent_scopes entry opts into drafts
// (#17076): the agent itself, its base agent for a replica, and the hive's
// owner logins, so an operator's hand-written draft reaches the agents they
// opted in. Owner entries are stored as "alice" or "github:alice"; the
// provider prefix is dropped so either form matches an author: line.
func kickDraftAuthors(cfg *config.Config, agentName string) []string {
	var out []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name == "" {
			return
		}
		for _, have := range out {
			if strings.EqualFold(have, name) {
				return
			}
		}
		out = append(out, name)
	}
	add(agentName)
	if cfg != nil {
		add(cfg.BaseAgentName(agentName))
		for _, login := range cfg.Dashboard.OwnerLogins() {
			if _, bare, ok := strings.Cut(login, ":"); ok {
				login = bare
			}
			add(login)
		}
	}
	return out
}

// logKnowledgeInjection emits the one knowledge line per kick (#17049):
//
//	knowledge: injected N bytes from M entries (skipped K for cap)
//
// with the primer cap and the sizes of the AGENTS.md and skills blocks that
// share the kick, so the whole knowledge payload is readable from one line
// next to "kick rendered". It is emitted even when N is 0 — a primed kick
// that found nothing is a signal, not silence — but not when the primer did
// not run (knowledge disabled or nothing to prime), which would otherwise
// print a zero line on every kick of a hive that has no knowledge store.
func (s *Scheduler) logKnowledgeInjection(agentName string, inj knowledgeInjection, agentsMdBytes, skillsBytes int) {
	if s == nil || s.logger == nil || (!inj.Primed && agentsMdBytes == 0 && skillsBytes == 0) {
		return
	}
	s.logger.Info(fmt.Sprintf("knowledge: injected %d bytes from %d entries (skipped %d for cap)",
		inj.Bytes, inj.Entries, inj.SkippedForCap),
		"agent", agentName,
		"injected_bytes", inj.Bytes,
		"entries", inj.Entries,
		"skipped_for_cap", inj.SkippedForCap,
		"cap_bytes", inj.CapBytes,
		"primed", inj.Primed,
		"agents_md_bytes", agentsMdBytes,
		"skills_bytes", skillsBytes,
	)
}

// KickPrimedSlugs returns the knowledge slugs the most recent kick message
// built for agentName actually rendered in its `# Relevant Knowledge` block.
// The mid-task advisor (#17052) uses it to never repeat what the kick carried;
// facts the byte cap dropped are not in it and so stay eligible.
func (s *Scheduler) KickPrimedSlugs(agentName string) []string {
	v, ok := s.primedSlugs.Load(agentName)
	if !ok {
		return nil
	}
	slugs, _ := v.([]string)
	return slugs
}

// kickSummaryMaxRunes caps KickSummary.Text, the kick context the mid-task
// advisor's ranking pass hands its model (#17155).
const kickSummaryMaxRunes = 600

// KickSummary is what the mid-task advisor's ranking pass knows about an
// agent's current kick (#17155): an identity that changes every time the
// scheduler primes a kick for the agent, and the primed issue refs, titles and
// labels, capped at kickSummaryMaxRunes. Refs lists the primed issue/PR refs
// that carry a repo ("owner/repo#N"), uncapped, so the advisor's decision
// store can join decisions to beads (#17211).
type KickSummary struct {
	ID   string
	Text string
	Refs []string
}

// recordKickSummary stores a fresh KickSummary for agentName. It runs for
// every primed kick, including one with no issues, so the identity always
// moves with the kick.
func (s *Scheduler) recordKickSummary(agentName string, issues []github.Issue) {
	var parts, refs []string
	for _, issue := range issues {
		ref := fmt.Sprintf("#%d", issue.Number)
		if issue.Repo != "" {
			ref = issue.Repo + ref
			refs = append(refs, ref)
		}
		part := ref + " " + strings.Join(strings.Fields(issue.Title), " ")
		if len(issue.Labels) > 0 {
			part += " [" + strings.Join(issue.Labels, ", ") + "]"
		}
		parts = append(parts, part)
	}
	text := strings.Join(parts, "; ")
	if r := []rune(text); len(r) > kickSummaryMaxRunes {
		text = string(r[:kickSummaryMaxRunes])
	}
	id := fmt.Sprintf("%s#%d", agentName, s.kickSeq.Add(1))
	s.kickSummaries.Store(agentName, KickSummary{ID: id, Text: text, Refs: refs})
}

// KickSummary returns the summary of the most recent kick primed for
// agentName; ok is false before the first one.
func (s *Scheduler) KickSummary(agentName string) (KickSummary, bool) {
	v, ok := s.kickSummaries.Load(agentName)
	if !ok {
		return KickSummary{}, false
	}
	sum, ok := v.(KickSummary)
	return sum, ok
}

// kickContextFor is the kick context a scoped knowledge entry is matched
// against (#17045): the base lane name (a replica resolves to the agent it
// replicates) and the union of the primed issues' labels. File paths are not
// known on the kick path, so entries scoped by paths alone wait for a caller
// that has them.
func kickContextFor(cfg *config.Config, agentName string, issues []github.Issue) knowledge.KickContext {
	kc := knowledge.KickContext{Lane: cfg.BaseAgentName(agentName)}
	seen := make(map[string]bool)
	for _, issue := range issues {
		for _, label := range issue.Labels {
			lower := strings.ToLower(strings.TrimSpace(label))
			if lower == "" || seen[lower] {
				continue
			}
			seen[lower] = true
			kc.Labels = append(kc.Labels, lower)
		}
	}
	return kc
}

// extractKeywords is extractKeywordsFor with no hive config: the built-in
// lanes and origin sources are the only filers, and the default never-claim
// labels apply.
func extractKeywords(issues []github.Issue) []string {
	return extractKeywordsFor(nil, issues)
}

// extractKeywordsFor pulls searchable terms from issue labels and titles.
// Title words are included because labels alone are often all noise
// (triage/accepted, kind/bug) and produce zero keywords after filtering.
//
// What the hive itself puts on an item is dropped, because it says where the
// item sits in the hive, not what it is about: its routing labels
// (isRoutingLabel), its complexity tier, the "[<agent>]" filer tag on a title
// it filed, and the "Review backlog" stem. On utah-packages#879 those made up
// most of the query (stage/010-actionable, agent/reviewer, medium, "reviewer
// review backlog") and pulled in review notes from unrelated repos. When
// nothing is left, the terms earlier releases used are returned instead
// (legacyKeywords); renderKnowledge also falls back to them when these terms
// match no entry.
func extractKeywordsFor(cfg *config.Config, issues []github.Issue) []string {
	filers := hiveFilers(cfg)
	neverClaim := neverClaimLabels(cfg)
	keywords := collectKeywords(issues,
		func(label string) bool { return !isRoutingLabel(label) && !neverClaim[label] },
		false,
		func(title string) string { return searchableTitle(filers, title) })
	if len(keywords) == 0 {
		return legacyKeywords(issues)
	}
	return keywords
}

// legacyKeywords is the query earlier releases built: every label outside
// noiseLabels, the complexity tier, and every title word.
func legacyKeywords(issues []github.Issue) []string {
	return collectKeywords(issues,
		func(label string) bool { return !isNoiseLabel(label) },
		true,
		func(title string) string { return title })
}

// collectKeywords is the shared walk: each issue's kept labels, then its tier
// when withTier is set, then the words of title(issue.Title), lower-cased,
// noise words dropped and each term once.
func collectKeywords(issues []github.Issue, keepLabel func(string) bool, withTier bool, title func(string) string) []string {
	seen := make(map[string]bool)
	var keywords []string
	add := func(word string) {
		if !seen[word] {
			keywords = append(keywords, word)
			seen[word] = true
		}
	}

	for _, issue := range issues {
		for _, label := range issue.Labels {
			if lower := strings.ToLower(label); keepLabel(lower) {
				add(lower)
			}
		}

		if withTier && issue.ComplexityTier != "" {
			add(strings.ToLower(issue.ComplexityTier))
		}

		for _, word := range splitTitleWords(title(issue.Title)) {
			if !isNoiseWord(word) {
				add(word)
			}
		}
	}

	return keywords
}

// reviewBacklogTitleRE matches the stem the hive puts on its review backlog
// items, "[reviewer] Review backlog (<perspective>): <finding>", with or
// without the filer tag (the same shape as pkg/github's
// reviewBacklogTitleCategoryRE).
var reviewBacklogTitleRE = regexp.MustCompile(`^(?:\[[^\]\s]+\]\s*)?Review backlog \(([^)]+)\):\s*`)

// searchableTitle is the part of an issue title that says what the issue is
// about. A review backlog title is cut down to its perspective, which names
// the kind of finding ("security", "style"), and the finding; the words
// "review backlog" match every review-related entry in the vault whatever the
// repo. Otherwise a leading "[<tag>]" is dropped only when it names a hive
// filer (hiveFilers): any lowercase word in brackets has the filer shape, and
// a person's "[nvidia] driver fails to build" keeps "nvidia".
func searchableTitle(filers map[string]bool, title string) string {
	title = strings.TrimSpace(title)
	if m := reviewBacklogTitleRE.FindStringSubmatch(title); m != nil {
		return m[1] + " " + title[len(m[0]):]
	}
	if tag := issueshape.AgentPrefix(title); tag != "" && filers[strings.ToLower(tag)] {
		return issueshape.StripAgentPrefix(title)
	}
	return title
}

// hiveFilers is the set of names that file issues for the hive: the built-in
// lanes (scanner, reviewer, …), the other origin sources (selfreport,
// governor, …) and the agents configured on this hive.
func hiveFilers(cfg *config.Config) map[string]bool {
	filers := make(map[string]bool)
	for _, lane := range config.KnownOriginLanes {
		filers[lane] = true
	}
	for _, source := range config.KnownOriginSources() {
		filers[source] = true
	}
	if cfg != nil {
		for name := range cfg.Agents {
			filers[strings.ToLower(name)] = true
			filers[strings.ToLower(cfg.BaseAgentName(name))] = true
		}
	}
	return filers
}

// neverClaimLabels is the hive's never-claim set (claims.never_claim_labels,
// default "epic"), lower-cased. Those labels mark trackers the hive will not
// work, so like routing labels they say nothing about the work.
func neverClaimLabels(cfg *config.Config) map[string]bool {
	labels := claims.DefaultNeverClaimLabels
	if cfg != nil {
		labels = cfg.Governor.Claims.EffectiveNeverClaimLabels()
	}
	out := make(map[string]bool, len(labels))
	for _, l := range labels {
		out[strings.ToLower(strings.TrimSpace(l))] = true
	}
	return out
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

// routingLabelPrefixes are the label families the hive, Prow and the review
// bots use to route and gate work: where an item sits in the pipeline
// (stage/, needs-), who holds or filed it (agent/ and origin/, see
// isRoutingLabel; hive/claimed-by-…; hive: churn-triaged), a hive's hold
// (hive-pause/<id>), the author class (author/human, …), and review, test or
// merge state (tide/, do-not-merge, release-note, cncf-cla:). They say nothing
// about what the issue is about, and they are on most items, so as search
// terms they match the same review and scanning entries for every task.
var routingLabelPrefixes = []string{
	stagelabels.Prefix, "hive/", "hive:", github.HiveHoldLabelPrefix, authorkind.Prefix,
	"triage/", "size/", "lifecycle/", "do-not-merge", "needs-", "dco-signoff",
	"sentinel-", "tide/", "release-note", "cncf-cla:",
}

// routingLabels are single hive, Prow or review-bot labels outside those
// families.
var routingLabels = map[string]bool{
	"approved-direction": true,
	"hive-review":        true,
	"lgtm":               true,
	"approved":           true,
	"claimed":            true,
	"ok-to-test":         true,
}

// isRoutingLabel reports whether a lower-cased label is one the hive's search
// leaves out: noiseLabels, and every routing label above.
func isRoutingLabel(label string) bool {
	if noiseLabels[label] || routingLabels[label] {
		return true
	}
	if config.IsAgentLaneLabel(label) || config.IsOriginLabel(label) {
		return true
	}
	for _, prefix := range routingLabelPrefixes {
		if strings.HasPrefix(label, prefix) {
			return true
		}
	}
	return false
}
