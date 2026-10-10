package scheduler

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/resolve"
)

// userSavedPolicyDir is where the dashboard prompt editor
// (PUT /api/config/agent/{name}/prompt → handleAgentPromptSave) writes a
// template a user edited in the UI. It must be searched BEFORE the git-cloned
// policies repo (…/examples/kubestellar/agents/) and the embedded defaults, or
// an edit made in the UI never reaches the kick — the kick keeps rendering the
// stale upstream copy that shadows the override (issue #3239). The dashboard's
// own read path (loadPromptTemplateRaw) already checks this location first; the
// scheduler must agree so a saved edit takes effect on the next kick.
//
// It is a var (not a const) only so tests can point it at a temp dir; production
// always uses the fixed /data/policies path that handleAgentPromptSave writes to.
var userSavedPolicyDir = "/data/policies"

// agentHomeDir is where per-agent homes live on a hive host; the per-agent
// CLAUDE.md (<agentHomeDir>/<name>/CLAUDE.md) is checked first by
// loadPromptTemplate. It is a var (not a const) only so tests can point it at
// a temp dir; production always uses the fixed /data/agents path.
var agentHomeDir = "/data/agents"

// clonedPoliciesDir is the root of the git-cloned policies repo checkout on a
// hive host (…/<root>/examples/kubestellar/agents/). Like userSavedPolicyDir,
// it is a var only so tests can point it at a temp dir; production always
// uses /data/policies.
var clonedPoliciesDir = "/data/policies"

var policyDirMu sync.RWMutex

func policyDirs() (agentHome, userSaved, cloned string) {
	policyDirMu.RLock()
	defer policyDirMu.RUnlock()
	return agentHomeDir, userSavedPolicyDir, clonedPoliciesDir
}

// loadPromptTemplate searches standard paths for an agent's policy template.
// It checks on-disk paths first, then falls back to embedded default policies.
func (s *Scheduler) loadPromptTemplate(agentName string) string {
	agentHome, userSaved, cloned := policyDirs()
	paths := []string{
		fmt.Sprintf("%s/%s/CLAUDE.md", agentHome, agentName),
		// User-saved override from the dashboard prompt editor wins over the
		// git-cloned examples copy and embedded defaults (#3239).
		fmt.Sprintf("%s/%s.md", userSaved, agentName),
		fmt.Sprintf("%s/examples/kubestellar/agents/%s.md", cloned, agentName),
	}
	if s.cfg.Policies.LocalDir != "" {
		paths = append(paths,
			fmt.Sprintf("%s/examples/kubestellar/agents/%s.md", s.cfg.Policies.LocalDir, agentName),
			fmt.Sprintf("%s/%s%s.md", s.cfg.Policies.LocalDir, s.cfg.Policies.Path, agentName),
		)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data)
		}
	}
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + agentName + ".md"); err == nil {
		return string(data)
	}
	return ""
}

// loadNamedTemplate loads a kick template by explicit filename (from config kick_template field).
// It checks on-disk paths first, then falls back to embedded default policies.
func (s *Scheduler) loadNamedTemplate(templateName string) string {
	content, _, _ := s.resolveNamedTemplate(templateName)
	return content
}

// TemplateSourceEmbedded is the source label for a template served from the
// compiled-in defaults (pkg/policies/defaults); every other source is the
// file path that served it.
const TemplateSourceEmbedded = "embedded default"

// resolveNamedTemplate is loadNamedTemplate with provenance: the content, the
// source that served it ("" when nothing did), and every location that was
// tried. The provenance is what lets a dangling kick_template be REPORTED
// rather than silently skipped (hivecommons/hive#7390): the success path
// always logged "using config kick_template", the miss path logged nothing,
// and an operator saw a blank prompt editor with a 404 repo link and no way
// to tell a lost template from one that never existed.
func (s *Scheduler) resolveNamedTemplate(templateName string) (content, source string, tried []string) {
	_, userSaved, cloned := policyDirs()
	paths := []string{
		// User-saved override from the dashboard prompt editor wins over the
		// git-cloned examples copy and embedded defaults (#3239). handleAgentPromptSave
		// writes the edited template to /data/policies/<KickTemplate>, so when an
		// agent has a kick_template set (e.g. quality-advisory.md at ACMM L2) the
		// edit lands here and must be picked up on the next kick.
		fmt.Sprintf("%s/%s", userSaved, templateName),
		fmt.Sprintf("%s/examples/kubestellar/agents/%s", cloned, templateName),
	}
	if s.cfg.Policies.LocalDir != "" {
		paths = append(paths,
			fmt.Sprintf("%s/examples/kubestellar/agents/%s", s.cfg.Policies.LocalDir, templateName),
			fmt.Sprintf("%s/%s%s", s.cfg.Policies.LocalDir, s.cfg.Policies.Path, templateName),
		)
	}
	for _, p := range paths {
		if data, err := os.ReadFile(p); err == nil {
			return string(data), p, paths
		}
	}
	tried = append(paths, "pkg/policies/defaults/"+templateName+" ("+TemplateSourceEmbedded+")")
	if data, err := policies.DefaultPolicies.ReadFile("defaults/" + templateName); err == nil {
		return string(data), TemplateSourceEmbedded, tried
	}
	return "", "", tried
}

// TemplateResolution describes how an agent's kick prompt template resolves,
// for the dashboard prompt editor (hivecommons/hive#7390). It answers the
// questions a blank editor cannot: is a kick_template configured, was it
// found, where, and — when it was not — what the scheduler will use instead.
type TemplateResolution struct {
	// Agent is the base agent name the resolution was computed for.
	Agent string `json:"agent"`
	// KickTemplate is the configured kick_template name ("" when unset).
	KickTemplate string `json:"kickTemplate,omitempty"`
	// Resolved is true when the configured kick_template was found.
	Resolved bool `json:"resolved"`
	// Source is what served the configured template: a file path, or
	// TemplateSourceEmbedded. Empty when unresolved or unset.
	Source string `json:"source,omitempty"`
	// PathsTried lists every location consulted for the configured template,
	// in order, so an operator can see exactly where a missing file was
	// expected. Empty when no kick_template is configured.
	PathsTried []string `json:"pathsTried,omitempty"`
	// Fallback names what a kick will actually use when the configured
	// template is missing (or none is configured): the ACMM pack template,
	// the <agent>.md convention template, or the hardcoded kick.
	Fallback string `json:"fallback"`
	// EmbeddedDefaultExists reports whether pkg/policies/defaults ships a
	// file of the kick_template's name — i.e. whether a repo link to it would
	// resolve. The editor must not render a link to a path that 404s.
	EmbeddedDefaultExists bool `json:"embeddedDefaultExists"`
}

// ResolveTemplate reports how agentName's kick prompt resolves, without
// building a kick. It mirrors BuildAgentMessage's chain (prompt_source is
// excluded: it is resolved live at kick time and has its own status) so the
// prompt editor can show the truth the scheduler would act on.
func (s *Scheduler) ResolveTemplate(agentName string) TemplateResolution {
	res := TemplateResolution{Agent: agentName}
	if s == nil || s.cfg == nil {
		return res
	}
	baseName := s.cfg.BaseAgentName(agentName)
	res.Agent = baseName
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.KickTemplate != "" {
		res.KickTemplate = agentCfg.KickTemplate
		content, source, tried := s.resolveNamedTemplate(agentCfg.KickTemplate)
		res.PathsTried = tried
		res.Resolved = content != ""
		res.Source = source
		_, err := policies.DefaultPolicies.ReadFile("defaults/" + agentCfg.KickTemplate)
		res.EmbeddedDefaultExists = err == nil
	}
	res.Fallback = s.describeTemplateFallback(baseName)
	return res
}

// TemplateExists reports whether a kick_template NAME resolves anywhere the
// scheduler looks (user override dir, cloned examples, configured local dir,
// embedded defaults) and what served it. The dashboard's config write path
// uses it to refuse a newly set dangling name (hivecommons/hive#7390).
func (s *Scheduler) TemplateExists(templateName string) (source string, ok bool) {
	if s == nil || s.cfg == nil || strings.TrimSpace(templateName) == "" {
		return "", false
	}
	content, source, _ := s.resolveNamedTemplate(templateName)
	return source, content != ""
}

// WarnDanglingKickTemplates checks every enabled agent's configured
// kick_template at startup and logs a WARN for each one that resolves nowhere,
// naming the fallback the kicks will use and the paths tried. Returns the
// offending agents (name → template) so callers and tests can act on the
// list. The per-kick warning in BuildAgentMessage covers the steady state;
// this catches the misconfiguration once, at load, where an operator reading
// the boot log will see it (hivecommons/hive#7390 item 3).
func (s *Scheduler) WarnDanglingKickTemplates() map[string]string {
	if s == nil || s.cfg == nil {
		return nil
	}
	dangling := map[string]string{}
	for name, agentCfg := range s.cfg.Agents {
		if agentCfg.KickTemplate == "" {
			continue
		}
		res := s.ResolveTemplate(name)
		if res.Resolved {
			continue
		}
		dangling[name] = agentCfg.KickTemplate
		if s.logger != nil {
			s.logger.Warn("config: kick_template does not resolve; kicks will use the fallback until the file exists or the field is cleared",
				"agent", name, "template", agentCfg.KickTemplate,
				"fallback", res.Fallback, "paths_tried", strings.Join(res.PathsTried, ", "))
		}
	}
	return dangling
}

// describeTemplateFallback names the template BuildAgentMessage would use for
// baseName when no configured kick_template resolves — the same chain, in the
// same order, described rather than executed.
func (s *Scheduler) describeTemplateFallback(baseName string) string {
	if s.cfg.ACMMLevel != nil && *s.cfg.ACMMLevel > 0 {
		if pack, err := config.ACMMPackByLevel(*s.cfg.ACMMLevel); err == nil {
			for _, pa := range pack.Agents {
				if pa.Name == baseName && pa.KickTemplate != "" {
					if content, _, _ := s.resolveNamedTemplate(pa.KickTemplate); content != "" {
						return fmt.Sprintf("ACMM level %d pack template %s", *s.cfg.ACMMLevel, pa.KickTemplate)
					}
				}
			}
		}
	}
	if template := s.loadPromptTemplate(baseName); template != "" {
		return "convention template " + baseName + ".md"
	}
	return "hardcoded kick for " + baseName
}

// substituteTemplateWithPolicy replaces ${VAR} placeholders in a prompt
// template, reporting whether any substituted value tripped fail-closed policy.
func (s *Scheduler) substituteTemplateWithPolicy(template string, actionable *github.ActionableResult, agentName string, issues []github.Issue) (string, bool) {
	return s.substituteTemplateWithVars(template, actionable, agentName, issues, nil)
}

// inceptionVars extracts template variable values from the inception engine.
// Returns empty strings when no inception is active — templates render cleanly.
func (s *Scheduler) inceptionVars() (idea, phase, mode, answers, slug, repoURL string) {
	s.mu.RLock()
	inception := s.inception
	s.mu.RUnlock()
	if inception == nil {
		return
	}
	state := inception.GetState()
	if state == nil {
		return
	}
	phase = string(state.Phase)
	mode = string(state.Mode)
	slug = state.IdeaSlug
	repoURL = state.RepoURL
	answers = s.inception.FormatAnswersForPrompt()

	idea = state.IdeaText
	if idea == "" && state.Mode == knowledge.InceptionBrownfield {
		idea = state.RepoURL
	}
	return
}

// substituteTemplateWithVars is substituteTemplateWithPolicy plus caller-supplied
// ${VAR}s, for a template whose values only one call site can compute.
//
// The reviewer lane (#5617 item 2) is the first such caller: its work list is
// read from ci-failing.json and then GATED on — buildReviewerMessage refuses to
// send a contract at all when that list is empty — so the list the template
// renders must be the same one the gate saw. Recomputing it inside the
// substitution would reopen the window where a rewrite between the two reads
// renders a full adjudication contract over an empty list.
//
// The built-ins still WIN on a name collision, so an extra var can never shadow
// ${GH_AUTH} or ${AGENT_NAME}.
func (s *Scheduler) substituteTemplateWithVars(template string, actionable *github.ActionableResult, agentName string, issues []github.Issue, extra map[string]func() string) (string, bool) {
	baseName := s.cfg.BaseAgentName(agentName)
	if actionable == nil {
		actionable = &github.ActionableResult{}
	}
	now := time.Now().Local()

	// Per-repo agent scope (#6204): everything below describes ONE agent's
	// world, so narrow that world to the repos this agent serves before any of
	// it is computed. Cadence stays hive-wide — a scoped agent still wakes on
	// its schedule — but it wakes to its own repos' work instead of a backlog
	// it has to read through and discard. That task filtering is the cheap half
	// of the cost problem: an agent shown a schema-migration issue in a repo
	// with no database will look at it.
	//
	// A nil scope (every agent on every hive that does not use the feature)
	// skips all of this, and the values below are byte-identical to before.
	if s.cfg.AgentRepoScope(agentName) != nil {
		keep := func(repo string) bool { return s.cfg.AgentServesRepo(agentName, repo) }
		actionable = github.FilterActionableForRepos(actionable, keep)
		issues = filterIssuesForRepos(issues, keep)
	}

	var agentIssuesForList []github.Issue
	if baseName == "scanner" {
		agentIssuesForList = issues
	} else {
		agentIssuesForList = filterByLane(issues, baseName)
	}
	agentIssuesForList, heldInflight := s.splitInflight(agentIssuesForList)
	issueList, issueFailClosed := s.formatIssueListWithPolicy(agentIssuesForList)
	prList, prFailClosed := s.formatPRListWithPolicyForAgent(actionable, baseName)
	if issueFailClosed || prFailClosed {
		s.logger.Warn("ioscan fail-closed blocked kick", "agent", agentName)
		return "", true
	}

	// Both narrowings apply (#6203/#6204). ${PROJECT_REPOS_LIST} is what a
	// template tells an agent to work through: a repo it does not serve is not
	// its work, and a paused repo is not work at all. ${PROJECT_PRIMARY_REPO}
	// is the repo its examples target, so a specialist scoped away from the
	// hive primary must not be handed the hive primary in either.
	agentActiveRepos, _ := s.activeReposForAgent(agentName)
	reposList := strings.Join(agentActiveRepos, ", ")
	primaryRepo := s.cfg.PrimaryRepoForAgent(agentName)
	fullPrimaryRepo := config.QualifyRepo(s.cfg.Project.Org, primaryRepo)

	agentList, agentRoles := s.buildAgentListAndRoles()

	displayName := agentName
	if ac, ok := s.cfg.Agents[agentName]; ok && ac.DisplayName != "" {
		displayName = ac.DisplayName
	}

	agentIssues := filterByLane(issues, baseName)
	if len(agentIssues) == 0 && actionable != nil && len(actionable.Issues.Items) > 0 {
		agentIssues = actionable.Issues.Items
	}
	knowledgeSection := s.primeKnowledge(agentName, agentIssues)

	repoRoot := s.agentsRepoRoot(primaryRepo)

	// Additive: prepend the repo's AGENTS.md instructions + requested skills to
	// the injected knowledge, when a local checkout root is available. This is a
	// guarded, single call point — it returns "" (and never errors) when no
	// AGENTS.md exists, so it is a no-op for repos that don't use the convention.
	//
	// The root is resolved for the PRIMARY repo, which is the repo this kick's
	// instructions are about — the same repo ${PROJECT_PRIMARY_REPO} names here
	// and HIVE_REPO names in the agent's environment. A multi-repo hive gets the
	// primary repo's AGENTS.md, never a different repo's: resolution is keyed by
	// repo name, so it cannot silently pick the wrong one.
	//
	// TODO(agentsmd): once file-level targeting exists, prefer
	// agentsmd.ParseNearest for closest-wins nested AGENTS.md. That still has no
	// caller — nothing on the kick path knows which FILE an agent will touch —
	// so it stays deferred, unlike the checkout root, which is now threaded.
	if agentsSection := s.primeAgentsMd(repoRoot); agentsSection != "" {
		knowledgeSection = agentsSection + "\n" + knowledgeSection
	}

	// Agent-declared skills prefer the hive-host-local registry, then fall back
	// to definitions in the primary repo's AGENTS.md or adjacent skills/
	// directory. The registry remains independently useful without a checkout;
	// the fallback activates only when agentsRepoRoot found one above.
	if skillsSection := s.primeSkills(agentName, repoRoot); skillsSection != "" {
		knowledgeSection = skillsSection + "\n" + knowledgeSection
	}

	inceptionIdea, inceptionPhase, inceptionMode, inceptionAnswers, inceptionSlug, inceptionRepoURL := s.inceptionVars()

	// The merge-eligible and CI-failing lists are read from hive-wide metrics
	// files, so they need the same narrowing: a scoped agent asked to fix red CI
	// must not be handed a red PR on a repo it cannot push to (#6204).
	repoInScope := func(repo string) bool { return s.cfg.AgentServesRepo(agentName, repo) }
	mergeEligibleList := s.buildMergeEligibleListFor(repoInScope)
	ciFailingList := s.buildCIFailingListFor(repoInScope)

	// The built-in per-kick variables. Each value is already computed above, so
	// the thunks just return it — but wrapping them as resolve.RuntimeContext
	// producers routes this through the same pluggable engine as config
	// substitution, letting operators add their own ${VAR}s (via the config
	// `variables:` block) while these built-ins always win. With no operator
	// variables configured, Expand reproduces the previous strings.NewReplacer
	// output exactly (unknown ${VAR} left literal; no env fallback in template
	// scope).
	lit := func(v string) func() string { return func() string { return v } }
	rt := &resolve.RuntimeContext{Vars: map[string]func() string{
		"AGENT_NAME":            lit(agentName),
		"AGENT_DISPLAY_NAME":    lit(displayName),
		"TIMESTAMP":             lit(now.Format("1/2 3:04 PM MST")),
		"QUEUE_ISSUES":          lit(fmt.Sprintf("%d", actionable.Issues.Count)),
		"QUEUE_PRS":             lit(fmt.Sprintf("%d", actionable.PRs.Count)),
		"QUEUE_HOLD":            lit(fmt.Sprintf("%d", actionable.Hold.Total)),
		"SLA_VIOLATIONS":        lit(fmt.Sprintf("%d", actionable.Issues.SLAViolations)),
		"ISSUE_LIST":            lit(issueList),
		"PR_LIST":               lit(prList),
		"REVIEW_PERSPECTIVES":   lit(s.reviewPerspectivesSection()),
		"AUTHORIZED_REPOS":      lit(s.buildReposSectionFor(agentName)),
		"GH_AUTH":               lit(s.ghAuthInstructions()),
		"WORK_TRACKER":          lit(s.workTrackerSection()),
		"IN_FLIGHT":             lit(inflightNote(heldInflight)),
		"PROJECT_ORG":           lit(s.cfg.Project.Org),
		"PROJECT_NAME":          lit(s.cfg.Project.Name),
		"PROJECT_PRIMARY_REPO":  lit(fullPrimaryRepo),
		"PROJECT_AI_AUTHOR":     lit(s.cfg.EffectiveAIAuthor()),
		"PROJECT_REPOS_LIST":    lit(reposList),
		"PROJECT_HOMEBREW_REPO": lit(fmt.Sprintf("%s/homebrew-tap", s.cfg.Project.Org)),
		"PROJECT_OBSERVABILITY": lit(s.cfg.Governor.ProjectObservability.PromptSection()),
		"WRITING_GUIDE":         lit(s.cfg.Project.WritingGuideSection()),
		"HIVE_REPO":             lit(fmt.Sprintf("%s/hive", s.cfg.Project.Org)),
		"HIVE_ID":               lit(s.cfg.HiveID),
		"AGENT_LIST":            lit(agentList),
		"AGENT_ROLES":           lit(agentRoles),
		"ENABLED_AGENTS":        lit(agentList),
		"KNOWLEDGE":             lit(knowledgeSection),
		"INCEPTION_IDEA":        lit(inceptionIdea),
		"INCEPTION_PHASE":       lit(inceptionPhase),
		"INCEPTION_MODE":        lit(inceptionMode),
		"INCEPTION_ANSWERS":     lit(inceptionAnswers),
		"INCEPTION_SLUG":        lit(inceptionSlug),
		"INCEPTION_REPO_URL":    lit(inceptionRepoURL),
		"MERGE_ELIGIBLE":        lit(mergeEligibleList),
		"CI_FAILING":            lit(ciFailingList),
	}}
	// Caller-supplied vars lose a name clash: one already claimed by a built-in
	// above is left alone, so no call site can redefine ${GH_AUTH}.
	for name, fn := range extra {
		if _, taken := rt.Vars[name]; taken {
			continue
		}
		rt.Vars[name] = fn
	}
	return s.registry().Expand(context.Background(), template, resolve.ScopeTemplate, rt), false
}
