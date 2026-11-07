package agent

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
)

// Mid-task knowledge advisor (hivecommons/hive#17052).
//
// The kick primer injects `# Relevant Knowledge` once, at kick time, from the
// issue titles and labels. Whatever the agent later turns out to touch gets
// nothing. The advisor closes that gap with the two things the runtime already
// has: the scrollback the poller captures every tick, and the typed-line
// primitive every other nudge uses. When the pane first shows a scope (a source
// directory, a `gh` command family) that was not there at the first post-kick
// poll, the hive-side adviser is asked for matching entries and each is typed
// into the live session as one `📚 Knowledge:` line, subject to a per-kick cap,
// a cooldown and the same never-type-over-a-human / never-splice-a-kick gates
// the CI-poll nudge honours. Default off (knowledge.advisor: false).

const (
	// knowledgeAdvisorDefaultMaxPerKick is the per-kick delivery cap when the
	// policy leaves it unset.
	knowledgeAdvisorDefaultMaxPerKick = 3
	// knowledgeAdvisorDefaultCooldown is the minimum gap between two
	// deliveries to one agent when the policy leaves it unset. Each line
	// becomes a queued user turn in the CLI; back-to-back lines would read
	// as an operator talking over the agent.
	knowledgeAdvisorDefaultCooldown = 2 * time.Minute
	// knowledgeAdvisorMaxScopeDepth is how many directory segments identify a
	// path scope: src/crux/agent/manager.go opens "src/crux/agent".
	knowledgeAdvisorMaxScopeDepth = 3
	// knowledgeAdvisorMaxSummary caps the body text in a delivered line.
	knowledgeAdvisorMaxSummary = 300
	// knowledgeAdvisorTrigger is the prompt-history trigger each delivery is
	// recorded under, so the dashboard shows it beside kicks.
	knowledgeAdvisorTrigger  = "knowledge-advisor"
	knowledgeAdvisorKindPath = "path"
	knowledgeAdvisorKindGH   = "gh"
)

// knowledgePathRe matches a relative file path with at least one directory and
// a file extension, as the CLIs print paths they read, edit or run against:
// "⏺ Read(src/crux/agent/manager.go)", "● Edit src/pkg/config/config.go",
// "$ cat src/docs/x.md". The leading class excludes URLs (":" and "//" never
// precede a match) and absolute paths (the segment before the first "/" must
// be a name).
var knowledgePathRe = regexp.MustCompile(`(?:^|[\s("'` + "`" + `=,\[])((?:\./)?(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_-][A-Za-z0-9_.-]*\.[A-Za-z0-9]{1,8})\b`)

// knowledgeGHRe matches the GitHub CLI families worth a scope of their own.
var knowledgeGHRe = regexp.MustCompile(`\bgh (pr|issue|run|api|release|workflow)\b`)

// knowledgeIgnoredDirs are path roots that are tooling, not the agent's work.
var knowledgeIgnoredDirs = map[string]bool{
	"node_modules": true, "vendor": true, ".git": true, "tmp": true, "proc": true,
}

// knowledgeGenericSegments are directory names that carry no topic.
var knowledgeGenericSegments = map[string]bool{
	"src": true, "pkg": true, "cmd": true, "internal": true, "lib": true, "test": true,
	"tests": true, "docs": true, "doc": true, "scripts": true, "static": true, "app": true,
}

// knowledgeGHKeywords are the search terms a gh family contributes.
var knowledgeGHKeywords = map[string][]string{
	"pr":       {"pull request", "pr"},
	"issue":    {"issue", "triage"},
	"run":      {"workflow run", "ci"},
	"api":      {"github api", "rate limit"},
	"release":  {"release", "tag"},
	"workflow": {"workflow", "ci"},
}

// knowledgeToolInFlightRe matches pane text the CLIs render only while a tool
// call is executing: Claude Code's collapsed "Running 1 shell command…", OMP's
// "⏺ Running… (esc to cancel)". Typing then would land inside the tool's
// output window, which is the one moment the advisor must not pick.
var knowledgeToolInFlightRe = regexp.MustCompile(`(?i)\brunning \d+ shell command|Running…|esc to cancel`)

// knowledgeAdvisor holds the adviser and the per-agent, per-kick state.
type knowledgeAdvisor struct {
	adviser crux.KnowledgeAdviser
	now     func() time.Time
	mu      sync.Mutex
	agents  map[string]*knowledgeKickState
}

// knowledgeKickState is one agent's advisor state for one kick. It is
// replaced whenever LastKick moves, so a new kick starts clean without the
// kick path having to know the advisor exists.
type knowledgeKickState struct {
	kickAt    time.Time
	baselined bool
	seen      map[string]bool // scope keys from the baseline or already handled
	delivered map[string]bool // slugs typed this kick
	count     int
	lastAt    time.Time
	pending   []crux.KnowledgeAdvice
}

// SetKnowledgeAdviser installs the hive-side matcher. A nil adviser turns the
// advisor off. It is an atomic store so the poller never takes m.mu for it.
func (m *Manager) SetKnowledgeAdviser(adviser crux.KnowledgeAdviser) {
	if adviser == nil {
		m.knowledgeAdvisor.Store(nil)
		return
	}
	m.knowledgeAdvisor.Store(&knowledgeAdvisor{
		adviser: adviser,
		now:     time.Now,
		agents:  make(map[string]*knowledgeKickState),
	})
}

// adviseKnowledge is the poll-tick entry point. scrollback is the capture the
// poller already holds; visible is the visible pane, captured once per tick
// and shared with the CI-poll guard.
func (m *Manager) adviseKnowledge(agent *AgentProcess, scrollback, visible string) {
	ka := m.knowledgeAdvisor.Load()
	if ka == nil || agent == nil || agent.kickDelivering.Load() || agent.LastKick == nil {
		return
	}
	policy := ka.adviser.Policy(agent.Name)
	if !policy.Enabled {
		return
	}
	advice, ok := ka.collect(agent.Name, *agent.LastKick, scrollback, visible, policy)
	if !ok {
		return
	}
	if m.tmuxSessionHasAttachedClientForAgent(agent) {
		return
	}
	line := renderKnowledgeAdvice(advice)
	ka.markDelivered(agent.Name, advice.Slug)
	m.logger.Info("knowledge advisor: delivered",
		"agent", agent.Name, "slug", advice.Slug, "title", advice.Title)
	m.recordPrompt(agent.Name, knowledgeAdvisorTrigger, line)
	m.tmuxSendLiteralForAgent(agent, line)
	time.Sleep(textToEnterDelay)
	m.tmuxSendEntersForAgent(agent)
}

// collect folds this tick's scrollback into the agent's kick state, asks the
// adviser for every scope that is new since the baseline, and returns the
// next deliverable advice when the cap, cooldown and pane gates all allow it.
func (ka *knowledgeAdvisor) collect(name string, kickAt time.Time, scrollback, visible string, policy crux.KnowledgeAdvisorPolicy) (crux.KnowledgeAdvice, bool) {
	scopes := observedKnowledgeScopes(scrollback)

	ka.mu.Lock()
	st := ka.agents[name]
	if st == nil || !st.kickAt.Equal(kickAt) {
		st = &knowledgeKickState{kickAt: kickAt, seen: map[string]bool{}, delivered: map[string]bool{}}
		ka.agents[name] = st
	}
	if !st.baselined {
		// Scrollback keeps work from before this kick; what is already on
		// screen at the first post-kick poll was not entered during it.
		st.baselined = true
		for _, sc := range scopes {
			st.seen[sc.Key] = true
		}
		ka.mu.Unlock()
		return crux.KnowledgeAdvice{}, false
	}
	var fresh []crux.KnowledgeScope
	for _, sc := range scopes {
		if !st.seen[sc.Key] {
			st.seen[sc.Key] = true
			fresh = append(fresh, sc)
		}
	}
	maxPerKick := policy.MaxPerKick
	if maxPerKick <= 0 {
		maxPerKick = knowledgeAdvisorDefaultMaxPerKick
	}
	capped := st.count >= maxPerKick
	ka.mu.Unlock()

	if capped {
		return crux.KnowledgeAdvice{}, false
	}
	// The adviser runs outside the lock: it searches the knowledge store.
	var found []crux.KnowledgeAdvice
	for _, sc := range fresh {
		found = append(found, ka.adviser.Advise(name, sc)...)
	}

	ka.mu.Lock()
	defer ka.mu.Unlock()
	for _, adv := range found {
		if adv.Slug == "" || st.delivered[adv.Slug] || knowledgeAdvicePending(st.pending, adv.Slug) {
			continue
		}
		st.pending = append(st.pending, adv)
	}
	if len(st.pending) == 0 {
		return crux.KnowledgeAdvice{}, false
	}
	if knowledgeTurnIdle(visible) {
		// The turn the advice was for is over; the next kick primes afresh.
		st.pending = nil
		return crux.KnowledgeAdvice{}, false
	}
	if knowledgeToolInFlight(visible) {
		return crux.KnowledgeAdvice{}, false
	}
	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = knowledgeAdvisorDefaultCooldown
	}
	if !st.lastAt.IsZero() && ka.now().Sub(st.lastAt) < cooldown {
		return crux.KnowledgeAdvice{}, false
	}
	next := st.pending[0]
	st.pending = st.pending[1:]
	return next, true
}

// markDelivered records a typed slug against the agent's current kick.
func (ka *knowledgeAdvisor) markDelivered(name, slug string) {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	st := ka.agents[name]
	if st == nil {
		return
	}
	st.delivered[slug] = true
	st.count++
	st.lastAt = ka.now()
}

func knowledgeAdvicePending(pending []crux.KnowledgeAdvice, slug string) bool {
	for _, p := range pending {
		if p.Slug == slug {
			return true
		}
	}
	return false
}

// knowledgeTurnIdle reports whether the agent is parked at an empty prompt
// with no work in flight — the turn is over.
func knowledgeTurnIdle(visible string) bool {
	return !paneShowsActiveWork(visible) && paneShowsEmptyInputPrompt(visible)
}

// knowledgeToolInFlight reports whether a tool call is executing right now.
func knowledgeToolInFlight(visible string) bool {
	return knowledgeToolInFlightRe.MatchString(visible)
}

// observedKnowledgeScopes extracts the scopes a pane capture shows the agent
// working in, in first-seen order, one entry per key.
func observedKnowledgeScopes(pane string) []crux.KnowledgeScope {
	pane = stripExplainLines(pane)
	var out []crux.KnowledgeScope
	seen := map[string]bool{}
	add := func(sc crux.KnowledgeScope) {
		if sc.Key == "" || seen[sc.Key] {
			return
		}
		seen[sc.Key] = true
		out = append(out, sc)
	}
	for _, match := range knowledgePathRe.FindAllStringSubmatch(pane, -1) {
		add(knowledgePathScope(match[1]))
	}
	for _, match := range knowledgeGHRe.FindAllStringSubmatch(pane, -1) {
		add(crux.KnowledgeScope{
			Kind:     knowledgeAdvisorKindGH,
			Key:      "gh:" + match[1],
			Keywords: knowledgeGHKeywords[match[1]],
		})
	}
	return out
}

// knowledgePathScope turns one observed relative path into its scope, or a
// zero scope for root-level files and tooling directories.
func knowledgePathScope(path string) crux.KnowledgeScope {
	path = strings.TrimPrefix(path, "./")
	segments := strings.Split(path, "/")
	dirs := segments[:len(segments)-1]
	if len(dirs) == 0 || knowledgeIgnoredDirs[dirs[0]] {
		return crux.KnowledgeScope{}
	}
	for _, d := range dirs {
		if d == "" || d == "." || d == ".." {
			return crux.KnowledgeScope{}
		}
	}
	if len(dirs) > knowledgeAdvisorMaxScopeDepth {
		dirs = dirs[:knowledgeAdvisorMaxScopeDepth]
	}
	var keywords []string
	for _, d := range dirs {
		if !knowledgeGenericSegments[strings.ToLower(d)] {
			keywords = append(keywords, d)
		}
	}
	return crux.KnowledgeScope{
		Kind:     knowledgeAdvisorKindPath,
		Key:      strings.Join(dirs, "/"),
		Path:     path,
		Keywords: keywords,
	}
}

// renderKnowledgeAdvice is the single line typed into the session.
func renderKnowledgeAdvice(adv crux.KnowledgeAdvice) string {
	var b strings.Builder
	b.WriteString("📚 Knowledge: ")
	b.WriteString(oneLine(adv.Title))
	if summary := truncateStr(oneLine(adv.Summary), knowledgeAdvisorMaxSummary); summary != "" {
		b.WriteString(" — ")
		b.WriteString(summary)
	}
	if ref := strings.TrimSpace(adv.Ref); ref != "" {
		b.WriteString(" (full: ")
		b.WriteString(ref)
		b.WriteString(")")
	}
	return b.String()
}

// oneLine collapses whitespace runs, including newlines, to single spaces.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
