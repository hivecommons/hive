package scheduler

import (
	"fmt"
	"os"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/policies"
)

// templates.go holds the agent prompt-template resolution chain: locating a
// per-agent template on disk (dashboard-saved override → git-cloned policies
// repo → per-agent CLAUDE.md), falling back to embedded defaults, and
// reporting which source a kick actually used. Split out of scheduler.go
// (hivecommons/hive#9262) — a self-contained concern with no dependency on
// message building, triage, or policy overlays.

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
