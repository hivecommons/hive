package scheduler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/pkg/agentsmd"
	"github.com/hivecommons/hive/pkg/classify"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/ioscan"
	"github.com/hivecommons/hive/pkg/knowledge"
	"github.com/hivecommons/hive/pkg/policies"
	"github.com/hivecommons/hive/pkg/promptsrc"
	"github.com/hivecommons/hive/pkg/resolve"
	"github.com/hivecommons/hive/pkg/skillreg"
	"github.com/hivecommons/hive/pkg/timeline"
	"github.com/hivecommons/hive/pkg/worksource"
)

type Scheduler struct {
	cfg                  *config.Config
	primer               *knowledge.Primer
	inception            *knowledge.InceptionEngine
	lastActionable       *github.ActionableResult
	logger               *slog.Logger
	promptResolver       *promptsrc.Resolver
	auditFunc            AuditFunc
	advisoryFunc         AdvisoryFunc
	classifier           ioscan.Classifier
	classifierThresholds ioscan.Thresholds
	classifierBudget     int
	inflight             InflightLookup
	lifecycle            timeline.Recorder
	mu                   sync.RWMutex
}

// SetLifecycleRecorder attaches the lifecycle timeline sink. Once set, every
// classifier pass records a KindClassified stage (lane/tier/model) for each
// classified issue — this is the point where lane routing decides an issue's
// lane, so it is the honest producer for the "classified" stage (#5656).
// A nil recorder (or never calling this) keeps classification silent.
func (s *Scheduler) SetLifecycleRecorder(r timeline.Recorder) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lifecycle = r
}

// lifecycleRecorder returns the attached recorder, or nil if none is set.
func (s *Scheduler) lifecycleRecorder() timeline.Recorder {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lifecycle
}

// recordClassified records one KindClassified journey stage per classified
// issue. The timeline store dedupes by (ref, kind), so per-cycle reruns of the
// classifier refresh the stage rather than appending. No I/O beyond the
// store's own throttled persistence; a nil recorder is a no-op.
func (s *Scheduler) recordClassified(issues []github.Issue) {
	rec := s.lifecycleRecorder()
	if rec == nil {
		return
	}
	for _, issue := range issues {
		ref := issueKey(issue)
		if ref == "" {
			continue
		}
		rec.Record(timeline.Event{
			IssueRef: ref,
			Kind:     timeline.KindClassified,
			Attrs: map[string]string{
				"lane":  issue.Lane,
				"tier":  issue.ComplexityTier,
				"model": issue.ModelRec,
			},
		})
	}
}

// registry builds the variable-resolution registry from the current config's
// `variables:` block. It is rebuilt per call (cheap: env/static factories only),
// so a live config reload that changes variable definitions is picked up on the
// next kick without extra wiring.
func (s *Scheduler) registry() *resolve.Registry {
	return s.cfg.ResolveRegistry(s.logger)
}

func New(cfg *config.Config, logger *slog.Logger) *Scheduler {
	return &Scheduler{
		cfg:    cfg,
		logger: logger,
	}
}

// SetGitHubPromptResolver attaches a resolver used to fetch an agent's kick
// prompt from a GitHub repo (agent.prompt_source). When nil, agents with a
// prompt_source silently fall back to their inline kick template.
func (s *Scheduler) SetGitHubPromptResolver(r *promptsrc.Resolver) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptResolver = r
}

// gitHubPromptResolver returns the attached resolver, or nil if none is set.
func (s *Scheduler) gitHubPromptResolver() *promptsrc.Resolver {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.promptResolver
}

// SetPrimer attaches a knowledge primer to the scheduler. When set, kick
// messages include relevant facts from the wiki layers.
func (s *Scheduler) SetPrimer(p *knowledge.Primer) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.primer = p
}

// GetPrimer returns the attached primer, or nil if none is set.
func (s *Scheduler) GetPrimer() *knowledge.Primer {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.primer
}

// SetInception attaches an inception engine so kick templates can inject
// ideation state via ${INCEPTION_*} variables.
func (s *Scheduler) SetInception(ie *knowledge.InceptionEngine) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inception = ie
}

// GetInception returns the attached inception engine, or nil if none is set.
func (s *Scheduler) GetInception() *knowledge.InceptionEngine {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.inception
}

// SetLastActionable caches the latest actionable result so manual kicks
// (via the dashboard API) can prime knowledge from the same issue set.
func (s *Scheduler) SetLastActionable(a *github.ActionableResult) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastActionable = a
}

// GetLastActionable returns the most recently cached actionable result.
func (s *Scheduler) GetLastActionable() *github.ActionableResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.lastActionable
}

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

// loadPromptTemplate searches standard paths for an agent's policy template.
// It checks on-disk paths first, then falls back to embedded default policies.
func (s *Scheduler) loadPromptTemplate(agentName string) string {
	paths := []string{
		fmt.Sprintf("%s/%s/CLAUDE.md", agentHomeDir, agentName),
		// User-saved override from the dashboard prompt editor wins over the
		// git-cloned examples copy and embedded defaults (#3239).
		fmt.Sprintf("%s/%s.md", userSavedPolicyDir, agentName),
		fmt.Sprintf("%s/examples/kubestellar/agents/%s.md", clonedPoliciesDir, agentName),
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
	paths := []string{
		// User-saved override from the dashboard prompt editor wins over the
		// git-cloned examples copy and embedded defaults (#3239). handleAgentPromptSave
		// writes the edited template to /data/policies/<KickTemplate>, so when an
		// agent has a kick_template set (e.g. quality-advisory.md at ACMM L2) the
		// edit lands here and must be picked up on the next kick.
		fmt.Sprintf("%s/%s", userSavedPolicyDir, templateName),
		fmt.Sprintf("%s/examples/kubestellar/agents/%s", clonedPoliciesDir, templateName),
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

// substituteTemplate replaces ${VAR} placeholders in a prompt template.
func (s *Scheduler) substituteTemplate(template string, actionable *github.ActionableResult, agentName string, issues []github.Issue) string {
	msg, _ := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
	return msg
}

func (s *Scheduler) substituteTemplateWithPolicy(template string, actionable *github.ActionableResult, agentName string, issues []github.Issue) (string, bool) {
	baseName := s.cfg.BaseAgentName(agentName)
	if actionable == nil {
		actionable = &github.ActionableResult{}
	}
	now := time.Now().Local()

	var agentIssuesForList []github.Issue
	if baseName == "scanner" {
		agentIssuesForList = issues
	} else {
		agentIssuesForList = filterByLane(issues, baseName)
	}
	agentIssuesForList, heldInflight := s.splitInflight(agentIssuesForList)
	issueList, issueFailClosed := s.formatIssueListWithPolicy(agentIssuesForList)
	prList, prFailClosed := s.formatPRListWithPolicy(actionable)
	if issueFailClosed || prFailClosed {
		s.logger.Warn("ioscan fail-closed blocked kick", "agent", agentName)
		return "", true
	}

	reposList := strings.Join(s.cfg.Project.Repos, ", ")
	primaryRepo := s.cfg.Project.PrimaryRepo
	fullPrimaryRepo := fmt.Sprintf("%s/%s", s.cfg.Project.Org, primaryRepo)

	agentList, agentRoles := s.buildAgentListAndRoles()

	displayName := agentName
	if ac, ok := s.cfg.Agents[agentName]; ok && ac.DisplayName != "" {
		displayName = ac.DisplayName
	}

	agentIssues := filterByLane(issues, baseName)
	if len(agentIssues) == 0 && actionable != nil && len(actionable.Issues.Items) > 0 {
		agentIssues = actionable.Issues.Items
	}
	knowledgeSection := s.primeKnowledge(agentIssues)

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

	mergeEligibleList := s.buildMergeEligibleList()
	ciFailingList := s.buildCIFailingList()

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
		"AUTHORIZED_REPOS":      lit(s.buildReposSection()),
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
	return s.registry().Expand(context.Background(), template, resolve.ScopeTemplate, rt), false
}

func (s *Scheduler) formatIssueList(issues []github.Issue) string {
	out, _ := s.formatIssueListWithPolicy(issues)
	return out
}

// issueFilterNotice renders the operator's project.issue_filter as prompt text,
// or "" when no filter is configured. The filter is ENFORCED upstream at
// enumeration (github.Client.fetchIssues) — filtered issues never reach any
// kick — so this notice is informational: it tells agents WHY the list may
// look smaller than the repo's open issues and not to go hunting for the rest.
// It is prepended to every issue list (${ISSUE_LIST} in kick templates and the
// hardcoded builders alike) so no agent is ever told to look at excluded
// issues.
func (s *Scheduler) issueFilterNotice() string {
	f := s.cfg.Project.IssueFilter
	if f.IsZero() {
		return ""
	}
	var b strings.Builder
	b.WriteString("ISSUE FILTER (operator policy — already enforced; the issue list below reflects it):\n")
	b.WriteString(fmt.Sprintf("  Agents may ONLY work issues carrying at least one of these labels: %s\n",
		strings.Join(f.RequireLabels, ", ")))
	b.WriteString("  ⛔ Do NOT pick up, plan, or open PRs for issues outside this list, even if you find them by listing the repo yourself.\n")
	return b.String()
}

func (s *Scheduler) formatIssueListWithPolicy(issues []github.Issue) (string, bool) {
	notice := s.issueFilterNotice()
	if len(issues) == 0 {
		return notice + "(none)", false
	}
	var b strings.Builder
	b.WriteString(notice)
	shown := 0
	failClosed := false
	for _, issue := range issues {
		if shown >= s.issueCap() {
			break
		}
		// The issue title AND labels are untrusted external text about to be
		// injected into an agent kick, and labels additionally drive classification
		// routing (pkg/classify). Gate both through ioscan (F11): a blocked
		// title/label is redacted/annotated rather than injected raw, and the block
		// is recorded to the dashboard audit log. Disabled → strict no-op
		// passthrough. Default is now ON (fail-safe).
		title, verdict := s.enforceIssueTextVerdict(issue.Title)
		failClosed = failClosed || (s.ioscanFailClosed() && verdict.HasCriticalInjection())
		const maxTitleRunes = 60
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes])
		}
		labels, labelsFailClosed := s.enforceLabelsWithPolicy(issue.Labels)
		failClosed = failClosed || labelsFailClosed
		b.WriteString(fmt.Sprintf("  %dm %s [%s] %s\n",
			issue.AgeMinutes, issueDisplayRef(issue),
			strings.Join(labels, ","), title))
		if issue.ClaimContext != nil && issue.ClaimContext.MergedPR {
			reason := "weakly claimed"
			if issue.ClaimContext.Reference {
				reason = "referenced without a closing keyword"
			} else if issue.ClaimContext.ExternalAuthor {
				reason = "was claimed by an external author"
			}
			prRef := fmt.Sprintf("%s#%d", issue.ClaimContext.PRRepo, issue.ClaimContext.PRNumber)
			b.WriteString(fmt.Sprintf("    ↳ merged PR context: %s %s; verify whether the merged work resolved this issue before implementing", prRef, reason))
			if issue.ClaimContext.PRURL != "" {
				b.WriteString(fmt.Sprintf(" (%s)", issue.ClaimContext.PRURL))
			}
			b.WriteString("\n")
		}
		shown++
	}
	return b.String(), failClosed
}

func (s *Scheduler) formatPRList(actionable *github.ActionableResult) string {
	out, _ := s.formatPRListWithPolicy(actionable)
	return out
}

func (s *Scheduler) formatPRListWithPolicy(actionable *github.ActionableResult) (string, bool) {
	if len(actionable.PRs.Items) == 0 {
		return "(none)", false
	}
	var b strings.Builder
	failClosed := false
	limit := s.prCap()
	for i, pr := range actionable.PRs.Items {
		if i >= limit {
			b.WriteString(prListOverflowLine(len(actionable.PRs.Items)-i, limit))
			break
		}
		// The PR title and author login are untrusted external text about to be
		// injected into an agent kick (F11). PR titles in particular drive
		// classification routing, and an attacker controls both the title and their
		// own fork/login. Gate both through ioscan: a blocked value is redacted
		// rather than injected raw. Disabled → strict no-op. Default is now ON.
		title, titleVerdict := s.enforceIssueTextVerdict(pr.Title)
		failClosed = failClosed || (s.ioscanFailClosed() && titleVerdict.HasCriticalInjection())
		const maxPRTitleRunes = 70
		if runes := []rune(title); len(runes) > maxPRTitleRunes {
			title = string(runes[:maxPRTitleRunes])
		}
		author, authorVerdict := s.enforceIssueTextVerdict(pr.Author)
		failClosed = failClosed || (s.ioscanFailClosed() && authorVerdict.HasCriticalInjection())
		b.WriteString(fmt.Sprintf("  %s#%d by @%s%s %s\n", pr.Repo, pr.Number, author, forkAnnotation(pr), title))
	}
	return b.String(), failClosed
}

// forkAnnotation is the inline marker every PR list carries for a PR whose
// head lives in a fork (hivecommons/hive#7386): the agent learns "comment
// only" from the work list, not from a failed push. Empty for same-repo PRs.
func forkAnnotation(pr github.PullRequest) string {
	if !pr.FromFork {
		return ""
	}
	head := pr.HeadRepo
	if head == "" {
		head = "fork deleted"
	}
	return " [fork: " + head + " — comment only, cannot push]"
}

// buildAgentListAndRoles returns a comma-separated agent list and a formatted
// role table derived from the config, so templates stay correct when agents
// are added, removed, or renamed.
func (s *Scheduler) buildAgentListAndRoles() (list, roles string) {
	var names []string
	for name := range s.cfg.EnabledAgents() {
		names = append(names, name)
	}
	list = strings.Join(names, ", ")

	var b strings.Builder
	for name, agentCfg := range s.cfg.EnabledAgents() {
		displayName := agentCfg.DisplayName
		if displayName == "" {
			displayName = name
		}
		model := agentCfg.Model
		if model == "" {
			model = "default"
		}
		b.WriteString(fmt.Sprintf("  - %s (%s, %s)\n", displayName, name, model))
	}
	roles = b.String()
	return list, roles
}

type KickMessage struct {
	Agent     string
	Repo      string
	Message   string
	IssueRefs []string
}

func (s *Scheduler) BuildKickMessages(actionable *github.ActionableResult, agentsDue []string) []KickMessage {
	s.resetClassifierBudget()
	classifiedIssues := classify.ClassifyAll(actionable.Issues.Items)
	s.recordClassified(classifiedIssues)
	reposSection := s.buildReposSection()

	var messages []KickMessage
	for _, targetKey := range agentsDue {
		agentName, repo := config.SplitCadenceTargetKey(targetKey)
		targetActionable := actionableForRepo(actionable, repo)
		targetIssues := classifiedIssues
		if repo != "" {
			targetIssues = filterIssuesByRepo(classifiedIssues, repo)
		}
		msg := s.BuildAgentMessage(agentName, targetIssues, targetActionable)
		if msg != "" {
			includeRepos := true
			if agentCfg, ok := s.cfg.Agents[agentName]; ok {
				includeRepos = agentCfg.ShouldIncludeRepos()
			} else if agentCfg, ok := s.cfg.Agents[s.cfg.BaseAgentName(agentName)]; ok {
				includeRepos = agentCfg.ShouldIncludeRepos()
			} else if s.cfg.BaseAgentName(agentName) == "outreach" {
				includeRepos = false
			}
			if repo != "" {
				msg = fmt.Sprintf("REPO-SCOPED CADENCE TARGET: %s\n\n%s", repo, msg)
			}
			if includeRepos {
				msg += "\n" + reposSection
			}
			msg = s.addCanaryPreamble(agentName, msg)
			messages = append(messages, KickMessage{
				Agent:     agentName,
				Repo:      repo,
				Message:   msg,
				IssueRefs: issueRefsForAgent(agentName, s.freeOfInflight(targetIssues), s.issueCap()),
			})
		}
	}
	return messages
}

func actionableForRepo(actionable *github.ActionableResult, repo string) *github.ActionableResult {
	if actionable == nil || repo == "" {
		return actionable
	}
	out := *actionable
	out.Issues = github.IssueResultFromItems(filterIssuesByRepo(actionable.Issues.Items, repo))
	out.PRs = github.PRResult{
		Items:       filterPRsByRepo(actionable.PRs.Items, repo),
		StaleDrafts: filterPRsByRepo(actionable.PRs.StaleDrafts, repo),
	}
	out.PRs.Count = len(out.PRs.Items)
	out.Hold = filterHoldByRepo(actionable.Hold, repo)
	out.TotalByRepo = map[string]github.RepoCounts{repo: actionable.TotalByRepo[repo]}
	return &out
}

func filterIssuesByRepo(issues []github.Issue, repo string) []github.Issue {
	filtered := make([]github.Issue, 0, len(issues))
	for _, issue := range issues {
		if issue.Repo == repo {
			filtered = append(filtered, issue)
		}
	}
	return filtered
}

func filterPRsByRepo(prs []github.PullRequest, repo string) []github.PullRequest {
	filtered := make([]github.PullRequest, 0, len(prs))
	for _, pr := range prs {
		if pr.Repo == repo {
			filtered = append(filtered, pr)
		}
	}
	return filtered
}

func filterHoldByRepo(hold github.HoldResult, repo string) github.HoldResult {
	filtered := make([]github.HoldItem, 0, len(hold.Items))
	for _, item := range hold.Items {
		if item.Repo == repo {
			filtered = append(filtered, item)
		}
	}
	out := github.HoldResult{Items: filtered}
	for _, item := range filtered {
		switch item.Type {
		case "pr":
			out.PRs++
		default:
			out.Issues++
		}
	}
	out.Total = len(filtered)
	return out
}

func issueRefsForAgent(agentName string, issues []github.Issue, limit int) []string {
	agentIssues := issues
	if agentName != "scanner" {
		agentIssues = filterByLane(issues, agentName)
	}
	if limit <= 0 {
		limit = maxIssuesPerKick
	}
	if len(agentIssues) > limit {
		agentIssues = agentIssues[:limit]
	}
	refs := make([]string, 0, len(agentIssues))
	seen := make(map[string]bool, len(agentIssues))
	for _, issue := range agentIssues {
		// One canonical key implementation (kubestellar/hive#4245). The old
		// `Number <= 0` skip dropped every Linear and Jira item on the floor:
		// they reach here with Number == 0, so no non-GitHub work was ever
		// referenced in an internal-agent kick at all. issueKey keeps
		// GitHub-backed refs byte-identical "repo#number" and gives external
		// work its own "repo!EXT-1" identity instead of a shared "repo#0".
		ref := issueKey(issue)
		if ref == "" || seen[ref] {
			continue
		}
		seen[ref] = true
		refs = append(refs, ref)
	}
	return refs
}

// issueKey is the scheduler's single entry point to the canonical work
// identity. It delegates to pkg/worksource so the scheduler cannot drift into a
// second key format — the parity test in scheduler_worksource_identity_test.go
// pins that it produces exactly what worksource.Ref.Key() does.
func issueKey(issue github.Issue) string {
	return worksource.Ref{
		SourceType: issue.SourceType,
		Repo:       issue.Repo,
		ExternalID: issue.ExternalID,
		Number:     issue.Number,
		URL:        issue.URL,
	}.Key()
}

// issueDisplayRef is the human-facing form written into kick message bodies:
// "owner/repo#42" for GitHub-backed work, "owner/repo!ENG-123" for a
// string-keyed source. It exists so no rendering site formats "%s#%d" directly
// and prints "owner/repo#0" for an item that simply has no issue number.
//
// It falls back to the bare repo when an item carries no usable identity at
// all, which keeps a malformed enumeration readable in the message rather than
// rendering a key nothing can match.
func issueDisplayRef(issue github.Issue) string {
	if key := issueKey(issue); key != "" {
		return key
	}
	return issue.Repo
}

// BuildAgentMessageFromLastActionable builds a kick message for the named
// agent from the scheduler's cached actionable snapshot, classifying issues
// exactly like governor-driven kicks do. The dashboard's manual-kick path
// previously called BuildAgentMessage with a nil issue list, so every manual
// kick delivered an EMPTY work list — agents (whose policies forbid running
// gh issue list themselves) then correctly reported "nothing to do" no matter
// how deep the queue was.
func (s *Scheduler) BuildAgentMessageFromLastActionable(agentName string) string {
	s.resetClassifierBudget()
	actionable := s.GetLastActionable()
	var classified []github.Issue
	if actionable != nil {
		classified = classify.ClassifyAll(actionable.Issues.Items)
		s.recordClassified(classified)
	}
	return s.BuildAgentMessage(agentName, classified, actionable)
}

func (s *Scheduler) buildReposSection() string {
	var b strings.Builder
	host := s.cfg.GitHub.ResolvedBaseURL() // always a full URL; github.com or the GHE instance
	b.WriteString(fmt.Sprintf("AUTHORIZED REPOS (all on %s — you may ONLY interact with these):\n", host))
	org := s.cfg.Project.Org
	for _, repo := range s.cfg.Project.Repos {
		full := repo
		if !strings.Contains(repo, "/") {
			full = org + "/" + repo
		}
		// Print the fully-qualified URL so the host is unambiguous in the prompt —
		// a github.ibm.com repo must never be mistaken for a github.com one.
		b.WriteString(fmt.Sprintf("  %s/%s\n", strings.TrimRight(host, "/"), full))
	}
	b.WriteString("⛔ NEVER access, search, list, file issues in, or open PRs on repos not listed above.\n")
	b.WriteString(fmt.Sprintf("⛔ Every repo above is on %s. This hive is single-host — never touch a repo on a different GitHub host.\n", host))
	// SCOPE vs PROVISIONING (#4464). This list is what `include_repos: true`
	// puts in a kick, and agents have read it as a promise that the repos are
	// on disk: a guide agent found its workspace directory empty, concluded
	// "no git worktree has been provisioned despite include_repos=true", and
	// filed it as an infrastructure blocker that then sat in the operator's
	// advisory digest. There is no such provisioning step — nothing in the
	// hive materialises a per-agent worktree from this list — so the kick has
	// to say so, in the same section that produces the impression. Getting a
	// checkout is an ordinary thing an agent does for itself, not a fault.
	b.WriteString(fmt.Sprintf("ℹ️ This list is an AUTHORIZATION SCOPE, not a checkout: it does not put any repo on disk, and nothing provisions a per-agent git worktree from it. If you need files rather than the GitHub API and have no checkout, clone one yourself: git clone %s/<org>/<repo> /tmp/<repo>. An absent checkout is a normal state to handle, NOT an infrastructure fault — do not file a finding about a missing worktree or unprovisioned repo workspace.\n", strings.TrimRight(host, "/")))
	// Multi-repo projects: the agent workdir is never a checkout of anything
	// but the PRIMARY repo, and the shipped templates' examples say --repo
	// "$HIVE_REPO" (primary). Without an explicit rotation instruction agents lock onto
	// the primary repo forever and the other project repos are never
	// touched (root-caused on a live 3-repo hive: sec-check scanned only
	// the primary across every session). The kick is the one place every
	// agent/template combination sees, so the instruction lives here.
	if len(s.cfg.Project.Repos) > 1 {
		primary := s.cfg.Project.PrimaryRepo
		if primary == "" {
			primary = s.cfg.Project.Repos[0]
		}
		b.WriteString(fmt.Sprintf(`🔁 MULTI-REPO COVERAGE — REQUIRED: this project has %d authorized repos; ALL of them are in scope, not just the primary (%s).
Your workdir is, at most, a checkout of the primary repo — never of the others. Each session, pick the authorized repo you have LEAST RECENTLY covered (check your beads and the [<your-role>] issues you previously filed in each repo) and work THAT repo this session:
  - If it is not your workdir repo, clone it first: git clone %s/<org>/<repo> /tmp/<repo> && cd /tmp/<repo>
  - Pass the chosen repo EXPLICITLY to every gh command: --repo "<org>/<repo>" (do not rely on $HIVE_REPO, which always names the primary repo).
  - $HIVE_REPOS lists every authorized repo, comma-separated.
⛔ Do NOT default to the primary repo every session — repos you never visit accumulate unseen problems.
`, len(s.cfg.Project.Repos), org+"/"+primary, strings.TrimRight(host, "/")))
	}
	return b.String()
}

// maxIssuesPerKick / maxPRsPerKick are the DEFAULT list caps; the live values
// come from governor.kick_limits via issueCap/prCap (hivecommons/hive#7368).
// The PR cap is the one that was missing: a spoke with 302 open PRs delivered
// a 69.5 KiB kick — the PR list alone ~36 KiB, over half the prompt and 3x the
// budget documented in pkg/dashboard/prompt_history.go — and every extra
// kilobyte lengthens the terminal-typed delivery window that #7363 hangs on.
const (
	maxIssuesPerKick = config.DefaultMaxIssuesPerKick
	maxPRsPerKick    = config.DefaultMaxPRsPerKick
)

// issueCap is how many issues one kick list may carry (governor.kick_limits
// .max_issues, default maxIssuesPerKick). Nil-safe for bare test schedulers.
func (s *Scheduler) issueCap() int {
	if s == nil || s.cfg == nil {
		return maxIssuesPerKick
	}
	return s.cfg.Governor.KickLimits.IssuesPerKick()
}

// prCap is how many PRs one kick list may carry (governor.kick_limits.max_prs,
// default maxPRsPerKick). Applied to every PR list a kick renders: actionable
// PRs, stale drafts, merge-eligible, CI-failing.
func (s *Scheduler) prCap() int {
	if s == nil || s.cfg == nil {
		return maxPRsPerKick
	}
	return s.cfg.Governor.KickLimits.PRsPerKick()
}

// prListOverflowLine is the explicit marker appended when a PR list was cut
// at the cap, so the agent knows the list is partial rather than complete —
// a silent slice would read as "these are all the PRs".
func prListOverflowLine(omitted, limit int) string {
	return fmt.Sprintf("  … and %d more open PRs not listed (cap %d per kick; they return on later kicks as this list drains)\n", omitted, limit)
}

const (
	holdGatedACMMMinLevel = 3
	holdGatedACMMMaxLevel = 5
)

// BuildAgentMessage constructs a kick prompt for the named agent using the
// template resolution chain (config kick_template → convention → embedded → hardcoded).
func (s *Scheduler) BuildAgentMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) (message string) {
	// Hold-gated PRs are deliberately absent from actionable.PRs: fetchPRs moves
	// them into actionable.Hold as soon as it sees the hold label. Wrap every
	// resolution path here so config templates, repo-sourced prompts, embedded
	// defaults, hardcoded fallbacks, scheduled kicks, and manual kicks all see
	// the same occupied-ground preflight. A template-only fix would leave stale
	// operator overrides vulnerable indefinitely (kubestellar/hive#4744).
	defer func() {
		message = s.addHeldPRCoordination(agentName, actionable, message)
		// Fix-before-new: an agent with red PRs of its own must see them —
		// with the CI evidence — ahead of any new work. Injected at the same
		// post-resolution seam as the held-PR preflight so no template path
		// can omit it (kubestellar/hive#4744): observed live on
		// kubestellar/console (2026-08-26), ten red split-PRs each had exactly
		// one commit — kicks kept spawning NEW PRs while the reaper's
		// re-engagements aged every red SHA to its cap unfixed.
		message = s.addRedPRFixFirst(agentName, message)
		// Same shape of problem, same seam (hivecommons/hive#7360): a PR this
		// agent opened is stuck behind unresolved external review-bot threads
		// and needs a push + in-thread replies before any new work.
		message = s.addReviewThreadFixFirst(agentName, message)
		// Non-GitHub work source: tell the agent how the tracker half of its
		// policy maps onto Linear (identity, auth, filing, PR linking, hold).
		// Same seam, same reason — a customized template cannot omit it.
		message = s.addWorkTrackerSection(message)
		// Items a live session already holds were dropped from the list
		// above; say so at the same seam so a customized template cannot
		// leave the agent wondering where its delegated issue went.
		message = s.addInflightNote(message, issues)
		// The workflow-push ceiling is a property of the agent's MODE, not of
		// its policy text, and no policy path can state it correctly for every
		// deployment — so it is stated here, at the same seam, for the one mode
		// that has it (#6681).
		message = s.addWorkflowPushCeiling(agentName, message)
	}()

	baseName := s.cfg.BaseAgentName(agentName)
	// 0. GitHub-sourced prompt: if the agent declares a prompt_source, resolve it
	//    live at kick time (with allowlist gating + graceful fallback). A miss
	//    (unset, denied, unreachable with no cache) falls through to the inline
	//    template chain below, so a bad source never blanks or crashes a kick.
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.PromptSource.IsSet() {
		if resolver := s.gitHubPromptResolver(); resolver != nil {
			src := promptsrc.Source{
				Owner: agentCfg.PromptSource.Owner,
				Repo:  agentCfg.PromptSource.Repo,
				Path:  agentCfg.PromptSource.Path,
				Ref:   agentCfg.PromptSource.Ref,
			}
			if res := resolver.Resolve(context.Background(), src); res.Ok && res.Body != "" {
				s.logger.Info("using GitHub-sourced kick prompt", "agent", agentName, "source", res.Source)
				body, failClosed := s.substituteTemplateWithPolicy(res.Body, actionable, agentName, issues)
				if failClosed {
					return ""
				}
				return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
			}
		}
	}

	// 1. Config-driven: use kick_template field if set
	if agentCfg, ok := s.cfg.Agents[baseName]; ok && agentCfg.KickTemplate != "" {
		template, source, tried := s.resolveNamedTemplate(agentCfg.KickTemplate)
		if template != "" {
			s.logger.Info("using config kick_template", "agent", agentName, "template", agentCfg.KickTemplate, "source", source)
			body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
			if failClosed {
				return ""
			}
			return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
		}
		// The configured template does not exist anywhere. Say so — with the
		// same weight the success path gets — naming what the kick falls back
		// to. Silence here is what let a dangling kick_template look like a
		// working one for the life of a spoke (hivecommons/hive#7390).
		s.logger.Warn("config kick_template not found; falling back",
			"agent", agentName, "template", agentCfg.KickTemplate,
			"fallback", s.describeTemplateFallback(baseName),
			"paths_tried", strings.Join(tried, ", "))
	}

	// 2. ACMM pack default: if acmm_level is set, use the pack's template for this agent
	if s.cfg.ACMMLevel != nil && *s.cfg.ACMMLevel > 0 {
		if pack, err := config.ACMMPackByLevel(*s.cfg.ACMMLevel); err == nil {
			for _, pa := range pack.Agents {
				if pa.Name == baseName && pa.KickTemplate != "" {
					if template := s.loadNamedTemplate(pa.KickTemplate); template != "" {
						s.logger.Info("using ACMM pack template", "agent", agentName, "level", *s.cfg.ACMMLevel, "template", pa.KickTemplate)
						body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
						if failClosed {
							return ""
						}
						return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
					}
				}
			}
		}
	}

	// 3. Convention: look for <agent>.md template file
	if template := s.loadPromptTemplate(baseName); template != "" {
		s.logger.Info("using prompt template for kick", "agent", agentName)
		body, failClosed := s.substituteTemplateWithPolicy(template, actionable, agentName, issues)
		if failClosed {
			return ""
		}
		return fmt.Sprintf("[agent:%s]\n\n%s", agentName, body)
	}

	// 3. Legacy hardcoded fallback (removed in Phase 4 when all agents use templates)
	s.logger.Info("no prompt template found, using hardcoded kick", "agent", agentName)
	switch baseName {
	case "scanner":
		return s.buildScannerMessage(issues, actionable)
	case "ci-maintainer":
		return s.buildCIMaintainerMessage(actionable)
	case "supervisor":
		return s.buildSupervisorMessage(actionable)
	case "quality":
		return s.buildQualityMessage(issues, actionable)
	case "architect":
		return s.buildArchitectMessage(issues, actionable)
	case "outreach":
		return s.buildOutreachMessage(actionable)
	case "sec-check":
		return s.buildSecCheckMessage(actionable)
	default:
		return s.buildGenericMessage(agentName, issues, actionable)
	}
}

// addHeldPRCoordination makes open, human-review-gated work visible before a
// PR-capable agent chooses its next target. These PRs cannot ride ${PR_LIST}:
// the hold gate intentionally removes them from the actionable PR population.
// The section is injected outside policy-template resolution so a customized
// or remotely sourced policy cannot accidentally omit the coordination fact.
func (s *Scheduler) addHeldPRCoordination(agentName string, actionable *github.ActionableResult, message string) string {
	if message == "" || !s.isHoldGatedPRAgent(agentName) {
		return message
	}
	claims, failClosed := s.formatHeldPRClaimsWithPolicy(actionable)
	if failClosed {
		if s.logger != nil {
			s.logger.Warn("ioscan fail-closed blocked held-PR coordination kick", "agent", agentName)
		}
		return ""
	}

	section := `## Open hold-gated PR coordination — mandatory preflight

The PRs below are open and awaiting human review. Their files, functions, and
tracking-issue clusters are occupied ground even when they have been waiting
for hours. Review latency is not abandonment.

OPEN HOLD-GATED PRs:
` + claims + `

Before choosing work:
1. Compare your intended files, functions, and tracker cluster with every
   plausibly related PR above.
2. Inspect any plausible overlap with ` + "`gh pr view <number> --repo <repo> --json title,body,files`" + `
   and, when needed, ` + "`gh pr diff <number> --repo <repo>`" + `. The supplied list is the
   authoritative open-PR snapshot; do not run ` + "`gh pr list`" + ` to rebuild it.
3. If another PR already covers any intended ground, choose a disjoint cluster
   or stand down. If the snapshot flags a repo as having omitted PRs, stand
   down for that repo: unseen occupied ground cannot be proven disjoint. Repos
   the snapshot lists in full remain workable. Do not write a second
   implementation and do not remove hold.
4. Make each new PR title and body name the exact files/functions/cluster it
   claims so the next kick can make the same comparison.

`
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
}

func (s *Scheduler) isHoldGatedPRAgent(agentName string) bool {
	if s.cfg == nil || s.cfg.ACMMLevel == nil || *s.cfg.ACMMLevel < holdGatedACMMMinLevel || *s.cfg.ACMMLevel > holdGatedACMMMaxLevel {
		return false
	}
	mode := s.agentEffectiveMode(agentName)
	return mode == "ISSUES_AND_PRS" || mode == "ISSUES_PRS_MERGE"
}

// addWorkflowPushCeiling states the one limit an ISSUES_AND_PRS agent cannot
// discover from its policy: a branch whose diff touches .github/workflows/**
// is unpushable at this mode, whatever the policy says it "can PR".
//
// The mode maps to the `contributor` scoped-token tier, and that tier
// deliberately does not request the Workflows permission (pkg/agent/mode.go
// TokenTier, pkg/github/app.go ScopedToken — only trusted/merger ask for it,
// so the push broker's protected-path rejection of .github/workflows/ stays
// meaningful for sandboxed contributors). GitHub then rejects the ref update
// server-side: "refusing to allow a GitHub App to create or update workflow
// ... without `workflows` permission".
//
// Nothing told the agent. Observed live (#6681): a hold-gated sec-check agent
// found an unsafe pattern in a workflow file, wrote the exact replacement into
// an issue, and filed no PR — the right outcome, reached with no way to say
// why, and indistinguishable to the operator from an agent that simply chose
// not to fix it. ci-maintainer-holdgated.md meanwhile promised
// ".github/workflows/*.yml changes" it could never land.
//
// Injected at the same post-resolution seam as the held-PR preflight and for
// the same reason (kubestellar/hive#4744): a customized or remotely sourced
// policy must not be able to omit it.
func (s *Scheduler) addWorkflowPushCeiling(agentName, message string) string {
	if message == "" || s.agentEffectiveMode(agentName) != "ISSUES_AND_PRS" {
		return message
	}
	section := `## Workflow files are out of reach at this mode — preflight

This agent runs in ISSUES_AND_PRS (hold-gated) mode, whose GitHub App token is
minted at the ` + "`contributor`" + ` tier. That tier does not carry the Workflows
permission, so a push whose diff touches

    .github/workflows/**

is rejected by GitHub server-side ("refusing to allow a GitHub App to create or
update workflow ... without ` + "`workflows`" + ` permission"), no matter what the App
installation grants. When this agent runs sandboxed the push broker refuses the
same diff first ("protected paths changed"), along with the other paths it
protects.

This is a ceiling, not a bug to work around: do not rewrite the file, do not
retry, and never weaken the finding to fit what is pushable.

When a fix belongs in a workflow file:
1. Do not open a PR for it. Nothing you can push will contain the change.
2. File the issue with the exact replacement text, so applying it is mechanical.
3. Say plainly in the issue that the change needs a human or an
   ISSUES_PRS_MERGE agent to land, and why — otherwise "issue, no PR" reads as
   a judgement call you did not make.
4. If part of the fix lives outside ` + "`.github/workflows/`" + `, PR that part and say
   in both the issue and the PR which part is still waiting.

Everything else is pushable as normal, including composite actions under
` + "`.github/actions/`" + ` — GitHub's restriction covers the workflows directory only.

`
	if newline := strings.IndexByte(message, '\n'); newline >= 0 {
		return message[:newline+1] + "\n" + section + message[newline+1:]
	}
	return section + message
}

// agentEffectiveMode returns the agent's configured mode, preferring the
// tools-derived effective mode when one is set. Empty when the agent is not
// configured. This is the resolution isHoldGatedPRAgent and isPRCapableAgent
// both did inline; it is one place now so a new caller cannot get it subtly
// different.
func (s *Scheduler) agentEffectiveMode(agentName string) string {
	if s.cfg == nil {
		return ""
	}
	agentCfg, ok := s.cfg.Agents[agentName]
	if !ok {
		agentCfg, ok = s.cfg.Agents[s.cfg.BaseAgentName(agentName)]
	}
	if !ok {
		return ""
	}
	mode := agentCfg.Mode
	if agentCfg.Tools != nil {
		if effective := agentCfg.Tools.EffectiveMode(); effective != "" {
			mode = effective
		}
	}
	return mode
}

// isPRCapableAgent reports whether the agent's effective mode lets it push
// branches / open PRs at all. Advisory-only agents can never repair a red PR,
// so the fix-before-new section would be noise for them.
func (s *Scheduler) isPRCapableAgent(agentName string) bool {
	if s.cfg == nil {
		return false
	}
	mode := s.agentEffectiveMode(agentName)
	return mode == "ISSUES_AND_PRS" || mode == "ISSUES_PRS_MERGE"
}

// redPRFixMaxDetailed bounds how many red PRs get a full evidence entry in one
// kick; the rest are summarized so a pathological backlog cannot flood the
// prompt. redPRFixExcerptRunes bounds the per-PR CI evidence excerpt.
const (
	redPRFixMaxDetailed  = 5
	redPRFixExcerptRunes = 400
)

// addRedPRFixFirst prepends a fix-before-new section listing the agent's OWN
// red-CI PRs, with the failing checks and the raw CI evidence, right below the
// kick header. It reads ci-failing.json (written by writeMergeEligible each
// eval tick), which attributes each PR to the agent whose relay request opened
// it. Unattributed rows default to scanner — the fleet's primary PR creator.
// Escalated (needs-human) PRs are never listed: they belong to a human.
func (s *Scheduler) addRedPRFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		return message
	}
	section := formatRedPRFixData(data, s.cfg.BaseAgentName(agentName))
	if section == "" {
		return message
	}
	// Insert directly after the "[agent:x]" header line when present, so the
	// section is the first thing the agent reads; otherwise prefix.
	if idx := strings.Index(message, "\n"); idx >= 0 && strings.HasPrefix(message, "[agent:") {
		return message[:idx+1] + section + message[idx+1:]
	}
	return section + message
}

// heldRedPRNote is appended to a held PR's entry in the fix-before-new
// block: the hold is a merge checkpoint, not a repair checkpoint
// (hivecommons/hive#7438), and the agent's own policy says never to touch a
// held item — this line is the explicit, narrow exception.
const heldRedPRNote = "held for human review — fix CI, do not remove the hold"

// heldRedPRExemptAgent is the one agent whose held PRs are NOT routed back
// for repair: the level-hold comment tells humans that outreach PRs are
// always held for their review, so an outreach PR must not be edited after a
// human may have started reading it.
const heldRedPRExemptAgent = "outreach"

// formatRedPRFixData renders the fix-before-new section for one agent from
// raw ci-failing.json bytes. Empty result means the agent has no open,
// non-escalated red PRs. A held red PR (hivecommons/hive#7438) is listed like
// any other — with heldRedPRNote — except for outreach's.
func formatRedPRFixData(data []byte, agent string) string {
	type ciFailingRow struct {
		Number        int      `json:"number"`
		Repo          string   `json:"repo"`
		Title         string   `json:"title"`
		Agent         string   `json:"agent"`
		FailingChecks []string `json:"failing_checks"`
		Excerpt       string   `json:"excerpt"`
		Escalated     bool     `json:"escalated"`
		FromFork      bool     `json:"from_fork"`
		HeadRepo      string   `json:"head_repo"`
		Held          bool     `json:"held"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return ""
	}
	var mine []ciFailingRow
	forks := 0
	for _, pr := range payload.Items {
		if pr.Escalated {
			continue // needs-human: hands off for agents
		}
		if pr.FromFork {
			// A fork PR can never be "yours": the hive pushes only to the base
			// repository, so it did not open this PR and cannot repair it.
			// Unattributed rows default to scanner below, which is exactly how
			// 66 contributor PRs from forks became one scanner's FIX-BEFORE-NEW
			// gate on the projectbluefin spoke (hivecommons/hive#7386).
			forks++
			continue
		}
		owner := pr.Agent
		if owner == "" {
			owner = "scanner"
		}
		if owner != agent {
			continue
		}
		if pr.Held && owner == heldRedPRExemptAgent {
			continue // a human may already be reading it; leave it alone
		}
		mine = append(mine, pr)
	}
	if len(mine) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n## 🔴 FIX-BEFORE-NEW — your open PRs with failing CI (%d)\n\n", len(mine)))
	if forks > 0 {
		b.WriteString(fmt.Sprintf("(%d red PR(s) from forks are NOT listed here: you cannot push to a fork — they appear under CI_FAILING as comment-only.)\n", forks))
	}
	b.WriteString("These PRs are YOURS and they are red. Repairing them comes BEFORE claiming\n")
	b.WriteString("new issues or opening ANY new PR. For each one:\n")
	b.WriteString("  gh pr checkout <number> → fix using the evidence below → commit -s → git push\n")
	b.WriteString("Push to the SAME branch. Do NOT open a replacement PR. Do NOT leave these\n")
	b.WriteString("for a later cycle — every kick will re-list them until they are green.\n\n")
	for i, pr := range mine {
		if i >= redPRFixMaxDetailed {
			b.WriteString(fmt.Sprintf("  … and %d more (full list: %s)\n", len(mine)-i, ciFailingPath))
			break
		}
		b.WriteString(fmt.Sprintf("  #%d %s — %s\n", pr.Number, pr.Repo, pr.Title))
		if pr.Held {
			b.WriteString("    " + heldRedPRNote + "\n")
		}
		if len(pr.FailingChecks) > 0 {
			b.WriteString(fmt.Sprintf("    failing: %s\n", strings.Join(pr.FailingChecks, ", ")))
		}
		if excerpt := strings.TrimSpace(pr.Excerpt); excerpt != "" {
			if runes := []rune(excerpt); len(runes) > redPRFixExcerptRunes {
				excerpt = string(runes[:redPRFixExcerptRunes]) + "…"
			}
			b.WriteString("    evidence: " + strings.ReplaceAll(excerpt, "\n", "\n              ") + "\n")
		}
	}
	b.WriteString("\n")
	return b.String()
}

// reviewThreadsPath is the review-thread monitor's artifact
// (github.ReviewThreadsPath); a var here, like ciFailingPath, so tests can
// point the kick builder at a fixture.
var reviewThreadsPath = github.ReviewThreadsPath

// addReviewThreadFixFirst prepends a fix-before-new section listing the
// agent's OWN open PRs that carry unresolved external review-bot threads
// (hivecommons/hive#7360), with each thread's path, line, and an excerpt of
// the bot's finding. It reads review-threads.json (written by the governor's
// eval tick), which — exactly like ci-failing.json — attributes each PR to the
// agent whose relay request opened it; unattributed rows default to scanner.
// Escalated (needs-human) PRs are never listed. Inserted at the same
// below-the-header seam as the red-CI block (which runs first, so this block
// lands directly above it) so the two stuck-PR lists sit together at the top
// of the kick, ahead of the work list.
func (s *Scheduler) addReviewThreadFixFirst(agentName string, message string) string {
	if message == "" || !s.isPRCapableAgent(agentName) {
		return message
	}
	data, err := os.ReadFile(reviewThreadsPath)
	if err != nil {
		return message
	}
	section := formatReviewThreadFixData(data, s.cfg.BaseAgentName(agentName), s.reviewBotsResolveAfterFix())
	if section == "" {
		return message
	}
	if idx := strings.Index(message, "\n"); idx >= 0 && strings.HasPrefix(message, "[agent:") {
		return message[:idx+1] + section + message[idx+1:]
	}
	return section + message
}

// reviewBotsResolveAfterFix reads classification.review_bots.resolve_after_fix
// from the loaded config (default true). The project-file fallback is not
// consulted here: the kick text only decides whether to ALSO write a
// resolve_thread request, and the watcher's guard is what actually gates
// resolution, so a stale answer here costs at most one denied request.
func (s *Scheduler) reviewBotsResolveAfterFix() bool {
	if s.cfg == nil {
		return true
	}
	return s.cfg.Classification.ReviewBots.ResolveAfterFixEnabled()
}

// formatReviewThreadFixData renders the review-thread fix-before-new section
// for one agent from raw review-threads.json bytes. Empty result means the
// agent has no open, non-escalated PR with an actionable bot thread.
func formatReviewThreadFixData(data []byte, agent string, resolveAfterFix bool) string {
	var report github.ReviewThreadsReport
	if json.Unmarshal(data, &report) != nil || !report.Enabled {
		return ""
	}
	var mine []github.ReviewThreadPR
	threads := 0
	for _, pr := range report.PRs {
		if pr.Escalated || len(pr.Threads) == 0 {
			continue // needs-human, or nothing left to address on this PR
		}
		owner := pr.Agent
		if owner == "" {
			owner = "scanner"
		}
		if owner != agent {
			continue
		}
		mine = append(mine, pr)
		threads += len(pr.Threads)
	}
	if len(mine) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("\n## 💬 FIX-BEFORE-NEW — your open PRs with unresolved review-bot threads (%d PRs, %d threads)\n\n", len(mine), threads))
	b.WriteString("An external review bot left inline threads on PRs that are YOURS. Until every\n")
	b.WriteString("thread is resolved these PRs cannot merge. Addressing them comes BEFORE claiming\n")
	b.WriteString("new issues or opening ANY new PR. ONE pass per kick, for each PR:\n")
	b.WriteString("  1. gh pr checkout <number> (branch: head_ref below) → address each thread's\n")
	b.WriteString("     finding → git commit -s → git push to the SAME branch. No replacement PR.\n")
	b.WriteString("  2. For EACH thread, reply in-thread with ONE line saying what changed, or why\n")
	b.WriteString("     nothing needed to (the reply goes into the thread, not a new review):\n")
	b.WriteString("       hive-review <number> --repo <owner/repo> --comment --thread <thread_id> --body \"<one line>\"\n")
	if resolveAfterFix {
		b.WriteString("  3. Then resolve it:\n")
		b.WriteString("       hive-review <number> --repo <owner/repo> --resolve-thread <thread_id>\n")
	} else {
		b.WriteString("  3. Do NOT resolve the thread — a human closes it on this hive (resolve_after_fix: false).\n")
	}
	b.WriteString("Never reply twice in the same thread: threads you already answered are not listed\n")
	b.WriteString("here; if one still appears, skip it. Only threads a review bot opened are listed\n")
	b.WriteString("and only those can be resolved this way — a human's thread is never yours to close.\n\n")
	shown := 0
	for _, pr := range mine {
		if shown >= redPRFixMaxDetailed {
			b.WriteString(fmt.Sprintf("  … and %d more PRs (full list: %s)\n", len(mine)-shown, reviewThreadsPath))
			break
		}
		shown++
		title := strings.TrimSpace(pr.Title)
		if title != "" {
			title = " — " + title
		}
		b.WriteString(fmt.Sprintf("  #%d %s%s\n", pr.Number, pr.Repo, title))
		b.WriteString(fmt.Sprintf("    head_ref: %s\n", pr.HeadRef))
		for _, t := range pr.Threads {
			loc := t.Path
			if t.Line > 0 {
				loc = fmt.Sprintf("%s:%d", t.Path, t.Line)
			}
			b.WriteString(fmt.Sprintf("    thread %s (%s, by %s)\n", t.ThreadID, loc, t.Author))
			if excerpt := strings.TrimSpace(t.Body); excerpt != "" {
				if runes := []rune(excerpt); len(runes) > redPRFixExcerptRunes {
					excerpt = string(runes[:redPRFixExcerptRunes]) + "…"
				}
				b.WriteString("      finding: " + strings.ReplaceAll(excerpt, "\n", "\n               ") + "\n")
			}
		}
	}
	b.WriteString("\n")
	return b.String()
}

// maxHeldPRsPerRepoPerKick bounds the held-PR snapshot per repository instead
// of across the whole kick.
//
// Disjointness is only ever evaluated against PRs in the repo the agent is
// about to touch, so a repo-scoped cap preserves the fail-closed guarantee
// exactly — the agent still refuses to act wherever the snapshot is
// incomplete — while preventing one crowded repo from blanking the snapshot
// for every other repo in the sweep.
//
// The previous global cap reused maxIssuesPerKick, which coupled two unrelated
// limits and made the stand-down fire on render-cap overflow rather than on
// real contention: a spoke tracking 16 repos stood down across all of them
// because a single repo pushed the combined list one item past 100, and could
// then never open the PRs that would drain the backlog causing the overflow.
const maxHeldPRsPerRepoPerKick = 100

func (s *Scheduler) formatHeldPRClaimsWithPolicy(actionable *github.ActionableResult) (string, bool) {
	if actionable == nil {
		return "  (none)", false
	}
	var b strings.Builder
	shown := 0
	failClosed := false

	repoOrder := make([]string, 0, 8)
	byRepo := make(map[string][]github.HoldItem)
	for _, item := range actionable.Hold.Items {
		if item.Type != "pr" {
			continue
		}
		if _, seen := byRepo[item.Repo]; !seen {
			repoOrder = append(repoOrder, item.Repo)
		}
		byRepo[item.Repo] = append(byRepo[item.Repo], item)
	}

	for _, repo := range repoOrder {
		items := byRepo[repo]
		limit := len(items)
		if limit > maxHeldPRsPerRepoPerKick {
			limit = maxHeldPRsPerRepoPerKick
		}
		for _, item := range items[:limit] {
			title, verdict := s.enforceIssueTextVerdict(item.Title)
			failClosed = failClosed || (s.ioscanFailClosed() && verdict.HasCriticalInjection())
			const maxHeldPRTitleRunes = 70
			if runes := []rune(title); len(runes) > maxHeldPRTitleRunes {
				title = string(runes[:maxHeldPRTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s#%d %s\n", item.Repo, item.Number, title))
			shown++
		}
		if omitted := len(items) - limit; omitted > 0 {
			b.WriteString(fmt.Sprintf("  ... %d additional open held PRs omitted in %s; STAND DOWN for %s this kick\n", omitted, repo, repo))
		}
	}

	if shown == 0 {
		return "  (none)", failClosed
	}
	return strings.TrimSuffix(b.String(), "\n"), failClosed
}

func (s *Scheduler) buildScannerMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder

	b.WriteString("[agent:scanner]\n")
	b.WriteString("YOUR WORK LIST (pre-filtered — hold/ADOPTERS/drafts excluded, classified):\n")
	b.WriteString(s.issueFilterNotice())

	scannerIssues := issues

	b.WriteString(fmt.Sprintf("ACTIONABLE ISSUES (%d, oldest first):\n", len(scannerIssues)))
	shown := 0
	for _, issue := range scannerIssues {
		if shown >= s.issueCap() {
			break
		}
		tier := string(issue.ComplexityTier)
		if len(tier) > 0 {
			tier = tier[:1]
		}
		tracker := ""
		if issue.IsTracker {
			tracker = " [TRACKER]"
		}
		title := issue.Title
		const maxTitleRunes = 60
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes])
		}
		b.WriteString(fmt.Sprintf("  %dm %s [%s/%s] [%s] %s%s\n",
			issue.AgeMinutes, issueDisplayRef(issue),
			tier, issue.ModelRec,
			strings.Join(issue.Labels, ","),
			title, tracker))
		shown++
	}

	b.WriteString(fmt.Sprintf("ACTIONABLE PRs (%d):\n", actionable.PRs.Count))
	prLimit := s.prCap()
	for i, pr := range actionable.PRs.Items {
		if i >= prLimit {
			b.WriteString(prListOverflowLine(len(actionable.PRs.Items)-i, prLimit))
			break
		}
		title := pr.Title
		const maxPRTitleRunes = 70
		if runes := []rune(title); len(runes) > maxPRTitleRunes {
			title = string(runes[:maxPRTitleRunes])
		}
		b.WriteString(fmt.Sprintf("  %s#%d by @%s%s %s\n", pr.Repo, pr.Number, pr.Author, forkAnnotation(pr), title))
	}

	if actionable.Issues.SLAViolations > 0 {
		b.WriteString(fmt.Sprintf("\n⚠️ %d SLA VIOLATIONS (>30 min)\n", actionable.Issues.SLAViolations))
	}

	// Surfaces exactly the work step 2 already asks for ("Close stale drafts
	// (>48h, needs-rebase + dco-no, or fix already merged)") — which had
	// nothing to act on before, since fetchPRs drops every draft before this
	// prompt is ever built. See kubestellar/hive#3963.
	if len(actionable.PRs.StaleDrafts) > 0 {
		b.WriteString(fmt.Sprintf("\nYOUR STALE DRAFT PRs (%d, >48h old — finish, mark ready, or close):\n", len(actionable.PRs.StaleDrafts)))
		for i, d := range actionable.PRs.StaleDrafts {
			if i >= prLimit {
				b.WriteString(prListOverflowLine(len(actionable.PRs.StaleDrafts)-i, prLimit))
				break
			}
			title := d.Title
			const maxDraftTitleRunes = 70
			if runes := []rune(title); len(runes) > maxDraftTitleRunes {
				title = string(runes[:maxDraftTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s#%d %s\n", d.Repo, d.Number, title))
		}
	}

	if knowledgeSection := s.primeKnowledge(scannerIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	b.WriteString("\nWORKFLOW:\n")
	b.WriteString("  1. Check beads (`bd list --status open`) for context from previous cycles\n")
	b.WriteString("  2. Quick merges + cleanup (10 min cap) — merge PRs whose required checks are GREEN using a squash merge via your App token (MCP `merge_pull_request` with `merge_method: \"squash\"`, or `gh pr merge --squash`). Do NOT use `--admin` — never force-merge past pending or failing CI; wait for the required checks to pass. Ensure the PR body cites the issue it addresses: ask does merging this PR leave anything for that issue to track? If nothing, write `Closes #<issue>` — the default, auto-closes on merge. Use `Refs #<issue>` or `Part of #<issue>` (non-closing) only for an epic/multi-phase tracker or a deliberately partial fix, and say on the same line what remains and why. Close stale drafts (>48h, needs-rebase + dco-no, or fix already merged). `@dependabot rebase` stale ones. Move on after 10 min.\n")
	b.WriteString("  3. Fix blockers — find the ONE fix that unblocks the most PRs/issues. Clone, fix, push, merge.\n")
	b.WriteString("  4. Crank quick fixes — launch background agents using the Agent tool (run_in_background: true) to fix remaining issues in parallel. One PR per issue, move fast.\n")

	return b.String()
}

func (s *Scheduler) buildCIMaintainerMessage(actionable *github.ActionableResult) string {
	var b strings.Builder
	b.WriteString("[agent:ci-maintainer]\n")
	b.WriteString("Post-merge health check. Review CI status, GA4 errors, workflow health.\n")
	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold\n",
		actionable.Issues.Count, actionable.PRs.Count, actionable.Hold.Total))
	return b.String()
}

func (s *Scheduler) buildSupervisorMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:supervisor]\n")
	b.WriteString(fmt.Sprintf("MONITORING PASS %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())
	b.WriteString(s.reposSection())

	b.WriteString("ROLE: You are the SUPERVISOR. Your job is to MONITOR other agents, NOT to fix issues yourself.\n")
	b.WriteString("⛔ NEVER work on issues directly — that is scanner's job.\n")
	b.WriteString("⛔ NEVER open PRs or commit code — that is scanner's and architect's job.\n")
	b.WriteString("⛔ NEVER merge PRs — that is scanner's job.\n")
	b.WriteString("⛔ NEVER launch background fix agents — that is scanner's job.\n\n")

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Check all agent tmux panes — are they working or stuck at a prompt?\n")
	b.WriteString("  2. Check if agents are idle when they should be working (queue > 0 but agent idle)\n")
	b.WriteString("  3. Report agent health: running/stuck/crashed/idle/rate-limited\n")
	b.WriteString("  4. Flag stale agents that haven't produced output in > 1 cadence cycle\n")
	b.WriteString("  5. Summarize current state: what each agent is doing, what's stuck, what needs attention\n\n")

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold, %d SLA violations\n",
		actionable.Issues.Count, actionable.PRs.Count,
		actionable.Hold.Total, actionable.Issues.SLAViolations))

	b.WriteString("\nBeads: ~/supervisor-beads\n")
	return b.String()
}

var mergeEligiblePath = "/var/run/hive-metrics/merge-eligible.json"
var ciFailingPath = "/var/run/hive-metrics/ci-failing.json"

func (s *Scheduler) buildMergeEligibleList() string {
	data, err := os.ReadFile(mergeEligiblePath)
	if err != nil {
		return "(none)\n"
	}
	return formatMergeEligibleData(data, s.prCap())
}

func formatMergeEligibleData(data []byte, limit int) string {
	var payload struct {
		Items []struct {
			Number int    `json:"number"`
			Repo   string `json:"repo"`
			Title  string `json:"title"`
			Queued bool   `json:"queued"`
		} `json:"merge_eligible"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Items) == 0 {
		return "(none)\n"
	}
	var b strings.Builder
	for i, pr := range payload.Items {
		if limit > 0 && i >= limit {
			b.WriteString(prListOverflowLine(len(payload.Items)-i, limit))
			break
		}
		queued := ""
		if pr.Queued {
			queued = " [queued for auto-merge]"
		}
		b.WriteString(fmt.Sprintf("  #%d %s%s — %s\n", pr.Number, pr.Repo, queued, pr.Title))
	}
	return b.String()
}

// heldMarker annotates a red PR that is under hold. Held PRs entered this list
// with hivecommons/hive#7438 so they can be repaired; the marker is what keeps
// an agent from reading their presence as permission to merge or unhold them.
func heldMarker(held bool) string {
	if !held {
		return ""
	}
	return " [" + heldRedPRNote + "]"
}

func (s *Scheduler) buildCIFailingList() string {
	data, err := os.ReadFile(ciFailingPath)
	if err != nil {
		return "(none)\n"
	}
	type ciFailingRow struct {
		Number   int    `json:"number"`
		Repo     string `json:"repo"`
		Title    string `json:"title"`
		Author   string `json:"author"`
		HeadSHA  string `json:"head_sha"`
		HeadRef  string `json:"head_ref"`
		HeadRepo string `json:"head_repo"`
		FromFork bool   `json:"from_fork"`
		Held     bool   `json:"held"`
	}
	var payload struct {
		Items []ciFailingRow `json:"ci_failing"`
	}
	if json.Unmarshal(data, &payload) != nil || len(payload.Items) == 0 {
		return "(none)\n"
	}
	// Held red PRs ride ci-failing.json since hivecommons/hive#7438 so their
	// AUTHOR can repair them, but they are not this shared queue's business:
	// every policy says never touch a held item, and only the owning agent's
	// fix-before-new block carries the narrow exception. Keep them out here
	// so a repair agent does not push to a PR a human is reviewing.
	held := 0
	kept := payload.Items[:0]
	for _, pr := range payload.Items {
		if pr.Held {
			held++
			continue
		}
		kept = append(kept, pr)
	}
	payload.Items = kept
	// Two queues, not one (hivecommons/hive#7386): a red PR whose head lives
	// in a fork is comment-only for every agent — the App token pushes to the
	// base repository and nowhere else. Rendering the two together as one
	// "repair queue" sent a scanner through 107 PRs of which 66 were forks;
	// it found out by pushing, and the push landed a stray branch on the base
	// repo under the fork's head-ref name. The split keeps the push queue
	// honest about its size and takes the discovery cost off the agent.
	var pushable, forks []ciFailingRow
	for _, pr := range payload.Items {
		if pr.FromFork {
			forks = append(forks, pr)
		} else {
			pushable = append(pushable, pr)
		}
	}
	var b strings.Builder
	limit := s.prCap()
	if len(pushable) == 0 {
		b.WriteString("  (none you can push to)\n")
	}
	if held > 0 {
		b.WriteString(fmt.Sprintf("  (%d held red PR(s) are not listed: a held PR is repaired only by the agent that opened it, via its own FIX-BEFORE-NEW block)\n", held))
	}
	for i, pr := range pushable {
		if i >= limit {
			b.WriteString(prListOverflowLine(len(pushable)-i, limit))
			break
		}
		b.WriteString(fmt.Sprintf("  #%d %s by @%s (sha:%s)%s — %s\n", pr.Number, pr.Repo, pr.Author, pr.HeadSHA, heldMarker(pr.Held), pr.Title))
	}
	if len(forks) > 0 {
		b.WriteString(fmt.Sprintf("FORK PRs (%d — review/comment only, you CANNOT push to these):\n", len(forks)))
		b.WriteString("  Their head branch lives in the contributor's fork, not in this repo. Do NOT\n")
		b.WriteString("  `gh pr checkout` + push, and NEVER `git push origin HEAD:<head_ref>` — that creates\n")
		b.WriteString("  a stray branch on the base repo under a name you do not own. Leave a review\n")
		b.WriteString("  comment with the fix, or skip.\n")
		for i, pr := range forks {
			if i >= limit {
				b.WriteString(prListOverflowLine(len(forks)-i, limit))
				break
			}
			head := pr.HeadRepo
			if head == "" {
				head = "(fork deleted)"
			}
			if pr.HeadRef != "" {
				head += ":" + pr.HeadRef
			}
			b.WriteString(fmt.Sprintf("  #%d %s by @%s [fork: %s — comment only]%s — %s\n", pr.Number, pr.Repo, pr.Author, head, heldMarker(pr.Held), pr.Title))
		}
	}
	return b.String()
}

// ghAuthInstructions tells the agent how to authenticate each tool class.
//
// The answer for every class is THE AGENT DOES NOTHING (#1861): git is served
// by the credential helper, and gh by the wrapper the image installs AS `+"`gh`"+`
// (src/Dockerfile: COPY bin/gh-wrapper.sh /usr/local/bin/gh), which reads
// HIVE_AGENT_TOKEN_CACHE and exports the agent's tier-scoped App token itself,
// per invocation. No token material has to reach the agent for either tool.
//
// This block used to instruct every agent to run
// `+"`export GH_TOKEN=$(cat .../agent-tokens/gh-token-<agent>.cache)`"+`, which was
// wrong three ways:
//
//   - It put a live App installation token into the agent's OWN reasoning and
//     transcript — the exact exposure #3842/#3889 removed from the native-install
//     prompt, differing only in blast radius (tier-scoped here, fleet-wide there).
//     A token in the transcript is one prompt injection from exfiltration, and
//     #1861's whole goal is that agents hold no token material.
//   - It was redundant. The wrapper had already injected the same token before
//     the agent's command ran.
//   - It could BREAK the session. bin/agent-launch.sh deliberately leaves
//     GH_TOKEN unset because "Copilot CLI uses GH_TOKEN for its own Copilot API
//     auth, which rejects GitHub App server-to-server tokens" — so a Copilot-
//     backed agent following this instruction could lose model auth. The final
//     bullet below already said the Copilot CLI owns that variable, contradicting
//     the instruction four lines above it.
//
// Keep this block free of any token path or GH_TOKEN assignment. Both hive
// tools authenticate the agent without its participation; anything that tells
// an agent to fetch, read, echo or export a credential is a regression, and
// TestGHAuthInstructions_NeverHandsTheAgentAToken pins that.
// The agent name is no longer a parameter: nothing in this block is per-agent
// any more, which is the point — there is no per-agent path for the agent to
// read, because the hive applies the per-agent token on its behalf.
func (s *Scheduler) ghAuthInstructions() string {
	return `## Project Authentication

- The GitHub App is the WRITE GATE. Every write to GitHub — opening or updating
  an issue or PR, commenting, and merging — goes through this hive's GitHub App
  (github.com or GitHub Enterprise, per the primary repo). If the App is not
  installed you have NO write credential: stay advisory (read, KB, beads) and do
  not attempt to write. Never substitute a personal user token to work around a
  missing App. Login/identity is a separate concern and is always github.com.
- Writes are authored by the App bot identity, not a personal account. Do not
  set git user.name/user.email to a human, and do not pass 'gh pr create' or
  'git commit' an explicit --author: let the App identity stand.
- To OPEN A PULL REQUEST, use ` + "`hive-open-pr`" + ` — the hive opens it with the
  App token so it is authored by the App bot ("<slug>[bot]"), never the login user:
    hive-open-pr --repo <org>/<repo> --head <your-branch> --title "<title>" \
      --body "<body citing the issue>" --issues <N>
  --body-file <path> is accepted in place of --body, exactly as gh accepts it.
  Pass --issues <N> naming the issue this PR is for whenever the work started
  from an issue: the hive verifies the body actually references that issue and
  refuses the request otherwise, and it always refuses an empty body.
  Cite the issue correctly: write ` + "`Closes #N`" + ` whenever this PR resolves
  issue N — that is the NORMAL case, and GitHub then closes the issue on
  merge. An issue the PR fixed but never closes stays open as standing noise
  for the maintainers. Write ` + "`Refs #N`" + ` (non-closing) ONLY when part of
  issue N is deliberately left open — an epic/multi-phase tracker, or a
  partial fix — and say on the SAME line what is left and why. Never use Refs
  as the cautious default.
  Do NOT open PRs with the GitHub MCP (create_pull_request / create_pull_request_with_copilot)
  or raw 'gh pr create' — those author the PR as the Copilot login user. 'gh pr create'
  is auto-redirected to hive-open-pr, but prefer calling hive-open-pr directly.
  (Push your branch first; hive-open-pr requests the PR, the hive opens it within ~10s.)
- git push / git fetch: run them normally. A credential helper supplies the
  App-scoped push token automatically. Do NOT export GH_TOKEN for git and do
  NOT use HIVE_GITHUB_TOKEN (it is read-only; overriding breaks pushes).
- gh CLI: just run ` + "`gh`" + `. Authentication is already handled for you — the hive
  wraps every gh call and applies YOUR tier-scoped App token to it. You do not
  need, and must not set up, any credential of your own: do NOT export GH_TOKEN,
  and do NOT go looking for, read, or echo a token file — not one of your own,
  not another agent's, not a shared one. There is no token file you are meant
  to open. A token you put on a command line is a token in your transcript, and
  exporting GH_TOKEN can break the Copilot CLI's own auth, which owns that
  variable. If gh reports an auth problem, report it — do not go find a token.
- A missing GH_TOKEN at session start is therefore expected and is never a
  blocker — it is what "already handled for you" looks like from inside the
  session. All GitHub traffic flows through the hive proxy either way.

`
}

func (s *Scheduler) reposSection() string {
	var b strings.Builder
	host := s.cfg.GitHub.ResolvedBaseURL()
	b.WriteString(fmt.Sprintf("## Project Repositories\n\nYour role covers these repositories, all on **%s** (this hive is single-host):\n", host))
	for _, repo := range s.cfg.Project.Repos {
		full := repo
		if !strings.Contains(repo, "/") {
			full = s.cfg.Project.Org + "/" + repo
		}
		b.WriteString(fmt.Sprintf("  %s/%s\n", strings.TrimRight(host, "/"), full))
	}
	b.WriteString(fmt.Sprintf("\nAll work should be scoped to these repos on %s.\n\n", host))
	return b.String()
}

func (s *Scheduler) buildGenericMessage(agentName string, issues []github.Issue, actionable *github.ActionableResult) string {
	baseName := s.cfg.BaseAgentName(agentName)
	var b strings.Builder
	b.WriteString(fmt.Sprintf("[agent:%s]\n", agentName))
	b.WriteString(s.issueFilterNotice())

	agentIssues := filterByLane(issues, baseName)
	if len(agentIssues) > 0 {
		b.WriteString(fmt.Sprintf("Work items (%d):\n", len(agentIssues)))
		for _, issue := range agentIssues {
			b.WriteString(fmt.Sprintf("  %s %s\n", issueDisplayRef(issue), issue.Title))
		}
	}

	if knowledgeSection := s.primeKnowledge(agentIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	return b.String()
}

const defaultCoverageTargetPct = 91.0

func (s *Scheduler) buildQualityMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder

	b.WriteString("[agent:quality]\n")
	b.WriteString("TEST STRATEGIST — build test coverage from current level toward target.\n\n")

	b.WriteString(fmt.Sprintf("COVERAGE TARGET: %.0f%%\n", defaultCoverageTargetPct))

	qualityIssues := filterByLane(issues, "quality")
	if len(qualityIssues) > 0 {
		b.WriteString(fmt.Sprintf("\nTEST-RELATED ISSUES (%d):\n", len(qualityIssues)))
		shown := 0
		for _, issue := range qualityIssues {
			if shown >= s.issueCap() {
				break
			}
			title := issue.Title
			const maxTitleRunes = 60
			if runes := []rune(title); len(runes) > maxTitleRunes {
				title = string(runes[:maxTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s [%s] %s\n",
				issueDisplayRef(issue),
				strings.Join(issue.Labels, ","),
				title))
			shown++
		}
	}

	b.WriteString("\nMATURITY-ADAPTIVE INSTRUCTIONS:\n")
	b.WriteString("  If project has NO tests or CI (Level 1-2, mode=suggest):\n")
	b.WriteString("    - Propose test scaffolding. Create stub files with TODO bodies.\n")
	b.WriteString("    - Suggest which test framework to adopt. Open draft PRs.\n")
	b.WriteString("    - Create shared test utilities (factories, fixtures, helpers).\n")
	b.WriteString("  If project has CI but coverage is below target (Level 3, mode=gate):\n")
	b.WriteString("    - Identify the highest-impact untested code paths.\n")
	b.WriteString("    - Create test PRs that raise coverage above the CI threshold.\n")
	b.WriteString("    - Focus on integration tests for critical paths.\n")
	b.WriteString("  If project has full CI + TDD markers (Level 4, mode=tdd):\n")
	b.WriteString("    - Identify modules without red-green discipline.\n")
	b.WriteString("    - Create regression tests for recent bug fixes missing them.\n")
	b.WriteString("    - Enforce test-first for new features.\n")

	if knowledgeSection := s.primeKnowledge(qualityIssues); knowledgeSection != "" {
		b.WriteString("\n")
		b.WriteString(knowledgeSection)
	}

	b.WriteString("\nWORKFLOW:\n")
	b.WriteString("  1. Analyze coverage reports and identify untested modules.\n")
	b.WriteString("  2. Prioritize: regression-prone code > new features > utilities.\n")
	b.WriteString("  3. Create test PRs in batches (max 3 concurrent).\n")
	b.WriteString("  4. Each PR must include: test file, required mocks/factories, coverage delta estimate.\n")
	b.WriteString("  5. Write test_scaffold and pattern facts to the knowledge wiki for future agents.\n")
	b.WriteString("⛔ NEVER run gh issue list, gh pr list, gh search issues — the work list above is your ONLY source.\n")

	return b.String()
}

func (s *Scheduler) buildArchitectMessage(issues []github.Issue, actionable *github.ActionableResult) string {
	var b strings.Builder
	b.WriteString("[agent:architect]\n")
	b.WriteString("Full architect pass — refactor/perf scan across all repos.\n\n")

	b.WriteString(s.ghAuthInstructions())

	architectIssues := filterByLane(issues, "architect")
	if len(architectIssues) > 0 {
		b.WriteString(fmt.Sprintf("ARCHITECTURE-RELATED ISSUES (%d):\n", len(architectIssues)))
		shown := 0
		for _, issue := range architectIssues {
			if shown >= s.issueCap() {
				break
			}
			title := issue.Title
			const maxTitleRunes = 60
			if runes := []rune(title); len(runes) > maxTitleRunes {
				title = string(runes[:maxTitleRunes])
			}
			b.WriteString(fmt.Sprintf("  %s [%s] %s\n",
				issueDisplayRef(issue),
				strings.Join(issue.Labels, ","),
				title))
			shown++
		}
		b.WriteString("\n")
	}

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs, %d on hold\n\n",
		actionable.Issues.Count, actionable.PRs.Count, actionable.Hold.Total))

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Scan repos for refactoring opportunities (dead code, duplication, tech debt)\n")
	b.WriteString("  2. Identify performance bottlenecks and propose improvements\n")
	b.WriteString("  3. Review architecture decisions and flag inconsistencies\n")
	b.WriteString("  4. Create RFC-style issues for large changes that need discussion\n")
	b.WriteString("  5. Open PRs for small refactors that improve maintainability\n\n")

	b.WriteString("AUTONOMY RULES:\n")
	b.WriteString("  ✅ May do without approval: refactoring PRs, perf improvements, dead code removal\n")
	b.WriteString("  ❌ Needs human approval: API changes, dependency upgrades, schema migrations\n\n")

	if knowledgeSection := s.primeKnowledge(architectIssues); knowledgeSection != "" {
		b.WriteString(knowledgeSection)
		b.WriteString("\n")
	}

	b.WriteString("Beads: ~/architect-beads\n")

	return b.String()
}

func (s *Scheduler) buildOutreachMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:outreach]\n")
	b.WriteString(fmt.Sprintf("Full outreach pass. Time: %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Open PRs on external repos to promote adoption (awesome-lists, adopters files, install guides)\n")
	b.WriteString("  2. Check blocked_orgs before opening new PRs — one PR per org at a time\n")
	b.WriteString("  3. Monitor open outreach PRs for review feedback and address comments\n")
	b.WriteString("  4. Track placement progress toward target\n\n")

	b.WriteString("RULES:\n")
	b.WriteString("  ⛔ NEVER re-query PR counts with gh search — use pre-computed metrics\n")
	b.WriteString("  ⛔ NEVER open a second PR on an org that already has an open outreach PR\n")
	b.WriteString("  ⛔ NEVER open PRs on repos without verifying a matching mission exists first\n")
	b.WriteString("  ✅ Check ADOPTERS.MD before proposing cold outreach to any org\n\n")

	b.WriteString("Beads: ~/outreach-beads\n")

	return b.String()
}

func (s *Scheduler) buildSecCheckMessage(actionable *github.ActionableResult) string {
	now := time.Now().Local()
	var b strings.Builder
	b.WriteString("[agent:sec-check]\n")
	b.WriteString(fmt.Sprintf("Security review pass. Time: %s\n\n", now.Format("1/2 3:04 PM MST")))

	b.WriteString(s.ghAuthInstructions())

	b.WriteString("YOUR RESPONSIBILITIES:\n")
	b.WriteString("  1. Scan repos for security vulnerabilities (OWASP top 10, dependency CVEs)\n")
	b.WriteString("  2. Review recent PRs for security implications\n")
	b.WriteString("  3. Check for exposed secrets, hardcoded credentials, insecure defaults\n")
	b.WriteString("  4. Verify security headers, CSP policies, and auth middleware\n")
	b.WriteString("  5. Open issues or PRs for any findings\n\n")

	b.WriteString(fmt.Sprintf("Queue: %d issues, %d PRs\n",
		actionable.Issues.Count, actionable.PRs.Count))

	return b.String()
}

func filterByLane(issues []github.Issue, lane string) []github.Issue {
	var result []github.Issue
	for _, issue := range issues {
		if issue.Lane == lane || issue.Lane == "" {
			result = append(result, issue)
		}
	}
	return result
}

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
	if !ok || len(ac.Skills) == 0 {
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

	resolved := reg.ResolveRequested(repoCfg, ac.Skills)
	if len(resolved) == 0 {
		if loaded == 0 && (repoCfg == nil || len(repoCfg.Skills) == 0) {
			s.logger.Debug("skillreg: no skills available, skipping injection",
				"dir", skillsRegistryDir, "repo_root", repoRoot,
				"requested", len(ac.Skills))
			return ""
		}
		s.logger.Warn("skillreg: none of the declared skills resolved",
			"agent", agentName, "requested", ac.Skills, "registry_size", loaded,
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
