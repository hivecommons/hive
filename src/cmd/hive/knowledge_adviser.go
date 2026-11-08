package main

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/pkg/config"
	"github.com/hivecommons/hive/pkg/knowledge"
)

// Mid-task knowledge adviser (hivecommons/hive#17052): the hive side of the
// runtime's knowledge advisor. The runtime says which scope an agent just
// entered; this asks the same Primer the kick used, under the same
// knowledge.agent_scopes restriction, and hands back entries the kick did
// not already carry.

const (
	// knowledgeAdviseTimeout bounds one adviser query. The runtime calls it off
	// the poll goroutine, one call per agent at a time, so a slow wiki layer
	// only holds back that agent's next scope.
	knowledgeAdviseTimeout = 5 * time.Second
	// knowledgeAdviseMaxPerScope caps how many entries one scope may queue;
	// the runtime's per-kick cap is the real bound, this keeps the queue short.
	knowledgeAdviseMaxPerScope = 2
	// knowledgeAdviseSummaryRunes caps the body text handed to the runtime.
	knowledgeAdviseSummaryRunes = 300
)

type knowledgeAdviser struct {
	cfg    func() *config.Config
	primer func() *knowledge.Primer
	primed func(agent string) []string
	logger *slog.Logger
}

func newKnowledgeAdviser(b *boot) *knowledgeAdviser {
	return &knowledgeAdviser{
		cfg: func() *config.Config { return b.cfg },
		primer: func() *knowledge.Primer {
			if b.sched == nil {
				return nil
			}
			return b.sched.GetPrimer()
		},
		primed: func(agent string) []string {
			if b.sched == nil {
				return nil
			}
			return b.sched.KickPrimedSlugs(agent)
		},
		logger: b.logger,
	}
}

// Policy reads the live config every call so a dashboard change applies to
// the next poll tick without a restart, like the other host resolvers.
func (a *knowledgeAdviser) Policy(agent string) crux.KnowledgeAdvisorPolicy {
	cfg := a.cfg()
	if cfg == nil || !cfg.KnowledgeAdvisorEnabledFor(agent) {
		return crux.KnowledgeAdvisorPolicy{}
	}
	// The matcher works from the scope alone; it never asks for pane text.
	return crux.KnowledgeAdvisorPolicy{Enabled: true, MaxPerKick: cfg.Knowledge.AdvisorMaxPerKick, WantsPaneTail: false}
}

// Advise searches the knowledge store for scope. Until #17045 lands the
// Primer has no kick context for paths, so the observed file goes in as the
// primer's file path and the scope's segments as keywords.
func (a *knowledgeAdviser) Advise(agent string, scope crux.KnowledgeScope) []crux.KnowledgeAdvice {
	return a.adviseUpTo(agent, scope, knowledgeAdviseMaxPerScope)
}

// adviseUpTo is Advise with the per-scope cap as a parameter, so the ranking
// pass (#17155) can ask for its larger candidate set without changing what
// phase 1 hands the runtime.
func (a *knowledgeAdviser) adviseUpTo(agent string, scope crux.KnowledgeScope, limit int) []crux.KnowledgeAdvice {
	primer := a.primer()
	cfg := a.cfg()
	if primer == nil || cfg == nil {
		return nil
	}
	var paths []string
	if scope.Path != "" {
		paths = []string{scope.Path}
	}
	var agentScope *knowledge.TOCScope
	if sc, ok := cfg.KnowledgeAgentScope(agent); ok {
		s := knowledge.AgentScope(sc.Layers, sc.Repos, sc.Types, sc.Tags, sc.IncludeStates)
		agentScope = &s
	}
	ctx, cancel := context.WithTimeout(context.Background(), knowledgeAdviseTimeout)
	defer cancel()
	primed := primer.PrimeScoped(ctx, agentScope, paths, scope.Keywords)
	advice := selectKnowledgeAdvice(primed.Facts, a.primed(agent), limit)
	a.logger.Debug("knowledge advisor: scope entered",
		"agent", agent, "scope", scope.Key, "candidates", len(primed.Facts), "advice", len(advice))
	return advice
}

// selectKnowledgeAdvice keeps the best-ranked facts not in exclude, up to
// limit, rendered for the runtime's one-line delivery.
func selectKnowledgeAdvice(facts []knowledge.Fact, exclude []string, limit int) []crux.KnowledgeAdvice {
	skip := make(map[string]bool, len(exclude))
	for _, slug := range exclude {
		skip[slug] = true
	}
	var out []crux.KnowledgeAdvice
	for _, f := range facts {
		if f.Slug == "" || skip[f.Slug] || strings.TrimSpace(f.Title) == "" {
			continue
		}
		skip[f.Slug] = true
		out = append(out, crux.KnowledgeAdvice{
			Slug:    f.Slug,
			Title:   f.Title,
			Summary: truncateRunes(strings.Join(strings.Fields(f.Body), " "), knowledgeAdviseSummaryRunes),
			Ref:     knowledgeAdviceRef(f),
		})
		if len(out) >= limit {
			break
		}
	}
	return out
}

// knowledgeAdviceRef is where the agent can read the whole entry: the first
// source document URL, else the knowledge entry id the dashboard serves.
func knowledgeAdviceRef(f knowledge.Fact) string {
	for _, s := range f.Sources {
		if s.DocURL != "" {
			return s.DocURL
		}
	}
	return "knowledge entry " + f.Slug
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
