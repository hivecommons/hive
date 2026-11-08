package agent

import (
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/logscrub"
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
//
// Advise runs off the poll goroutine (hivecommons/hive#17153): at most one call
// per agent is in flight, new scopes queue newest-first behind it, and a result
// is held in the kick state until a later tick passes the delivery gates. A
// result that comes back after the agent's kick moved is dropped.

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
	// knowledgeAdvisorDefaultPaneLines caps KnowledgeScope.PaneTail when the
	// policy asks for it without a size.
	knowledgeAdvisorDefaultPaneLines = 80
)

// knowledgeAdvisorAdviseGrace is how long a poll tick waits for an Advise call
// it started before leaving the result for a later tick. A bounded local
// matcher answers well inside it, so its advice still lands on the tick that
// saw the scope; a model-backed adviser does not hold the tick past it.
var knowledgeAdvisorAdviseGrace = 500 * time.Millisecond

// knowledgePathRe matches a relative file path with at least one directory and
// a file extension, as the CLIs print paths they read, edit or run against:
// "⏺ Read(src/crux/agent/manager.go)", "● Edit src/pkg/config/config.go",
// "$ cat src/docs/x.md". The leading class excludes URLs (":" and "//" never
// precede a match) and absolute paths (the segment before the first "/" must
// be a name).
var knowledgePathRe = regexp.MustCompile(`(?:^|[\s("'` + "`" + `=,\[])((?:\./)?(?:[A-Za-z0-9_.-]+/)+[A-Za-z0-9_-][A-Za-z0-9_.-]*\.[A-Za-z0-9]{1,8})\b`)

// knowledgeGHRe matches the GitHub CLI families worth a scope of their own.
// The optional second group is the command within the family ("checks" in
// "gh pr checks"); a new command is what makes a gh scope eligible again.
var knowledgeGHRe = regexp.MustCompile(`\bgh (pr|issue|run|api|release|workflow)\b(?:[ \t]+([a-z][a-z-]*))?`)

// knowledgeEditLineRe matches the tool-call lines the CLIs print for a file
// write: "● Edit src/x.go", "⏺ Update(src/x.go)", "⏺ Write(src/x.go)".
var knowledgeEditLineRe = regexp.MustCompile(`^\s*[⏺●]\s*(?:Edit|MultiEdit|Write|Update|Create)\b`)

// knowledgeShellLineRe matches a shell command line: "$ cat src/x.md".
var knowledgeShellLineRe = regexp.MustCompile(`^\s*\$ `)

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
	// inFlight holds, per agent, the done channel of the Advise call running
	// now. It is per agent, not per kick, so a call left over from the
	// previous kick still blocks a second concurrent one.
	inFlight map[string]chan struct{}
}

// knowledgeKickState is one agent's advisor state for one kick. It is
// replaced whenever LastKick moves, so a new kick starts clean without the
// kick path having to know the advisor exists.
type knowledgeKickState struct {
	kickAt    time.Time
	baselined bool
	seen      map[string]*knowledgeScopeSeen // scope keys from the baseline or already handled
	delivered map[string]bool                // slugs typed this kick
	count     int
	lastAt    time.Time
	queue     []crux.KnowledgeScope // scopes waiting for Advise, newest first
	pending   []crux.KnowledgeAdvice
}

// knowledgeScopeSeen is what one kick has seen of one scope. A scope first
// seen without an edit (or a gh family with only the commands seen so far) is
// eligible for exactly one more Advise call: on its first edit, or on the next
// command in its gh family. Elapsed time never makes it eligible again.
type knowledgeScopeSeen struct {
	edited    bool
	commands  map[string]bool
	readvised bool
}

// knowledgeObservation is one scope a pane shows, with the gh commands seen
// in its family.
type knowledgeObservation struct {
	scope    crux.KnowledgeScope
	commands []string
}

// SetKnowledgeAdviser installs the hive-side matcher. A nil adviser turns the
// advisor off. It is an atomic store so the poller never takes m.mu for it.
func (m *Manager) SetKnowledgeAdviser(adviser crux.KnowledgeAdviser) {
	if adviser == nil {
		m.knowledgeAdvisor.Store(nil)
		return
	}
	m.knowledgeAdvisor.Store(&knowledgeAdvisor{
		adviser:  adviser,
		now:      time.Now,
		agents:   make(map[string]*knowledgeKickState),
		inFlight: make(map[string]chan struct{}),
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
		// Held, not dropped: a later tick delivers it once the human leaves.
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

// collect folds this tick's scrollback into the agent's kick state, queues
// every scope that is new (or newly eligible) since the baseline for the
// adviser, and returns the next deliverable advice when the cap, cooldown and
// pane gates all allow it. The advice stays pending until markDelivered.
func (ka *knowledgeAdvisor) collect(name string, kickAt time.Time, scrollback, visible string, policy crux.KnowledgeAdvisorPolicy) (crux.KnowledgeAdvice, bool) {
	observed := observeKnowledge(scrollback)

	ka.mu.Lock()
	st := ka.agents[name]
	if st == nil || !st.kickAt.Equal(kickAt) {
		st = &knowledgeKickState{kickAt: kickAt, seen: map[string]*knowledgeScopeSeen{}, delivered: map[string]bool{}}
		ka.agents[name] = st
	}
	if !st.baselined {
		// Scrollback keeps work from before this kick; what is already on
		// screen at the first post-kick poll was not entered during it.
		st.baselined = true
		for _, ob := range observed {
			st.seen[ob.scope.Key] = newKnowledgeScopeSeen(ob)
		}
		ka.mu.Unlock()
		return crux.KnowledgeAdvice{}, false
	}
	var fresh []crux.KnowledgeScope
	for _, ob := range observed {
		if sc, ok := st.observe(ob); ok {
			fresh = append(fresh, sc)
		}
	}
	maxPerKick := policy.MaxPerKick
	if maxPerKick <= 0 {
		maxPerKick = knowledgeAdvisorDefaultMaxPerKick
	}
	st.enqueue(fresh, maxPerKick-st.count)
	ka.mu.Unlock()

	ka.dispatch(name, st, scrollback, policy)

	ka.mu.Lock()
	defer ka.mu.Unlock()
	if len(st.pending) == 0 {
		return crux.KnowledgeAdvice{}, false
	}
	if knowledgeTurnIdle(visible) {
		// The turn the advice was for is over; the next kick primes afresh.
		st.pending = nil
		st.queue = nil
		return crux.KnowledgeAdvice{}, false
	}
	if knowledgeToolInFlight(visible) {
		return crux.KnowledgeAdvice{}, false
	}
	if st.count >= maxPerKick {
		return crux.KnowledgeAdvice{}, false
	}
	cooldown := policy.Cooldown
	if cooldown <= 0 {
		cooldown = knowledgeAdvisorDefaultCooldown
	}
	if !st.lastAt.IsZero() && ka.now().Sub(st.lastAt) < cooldown {
		return crux.KnowledgeAdvice{}, false
	}
	return st.pending[0], true
}

func newKnowledgeScopeSeen(ob knowledgeObservation) *knowledgeScopeSeen {
	seen := &knowledgeScopeSeen{edited: ob.scope.Action == crux.KnowledgeActionEdit, commands: map[string]bool{}}
	for _, c := range ob.commands {
		seen.commands[c] = true
	}
	return seen
}

// observe records one observation against the kick and reports the scope to
// ask the adviser about: on first sight, and once more on the scope's first
// edit or the next command in its gh family.
func (st *knowledgeKickState) observe(ob knowledgeObservation) (crux.KnowledgeScope, bool) {
	seen := st.seen[ob.scope.Key]
	if seen == nil {
		st.seen[ob.scope.Key] = newKnowledgeScopeSeen(ob)
		return ob.scope, true
	}
	again := false
	if ob.scope.Action == crux.KnowledgeActionEdit && !seen.edited {
		seen.edited = true
		again = true
	}
	for _, c := range ob.commands {
		if !seen.commands[c] {
			seen.commands[c] = true
			again = true
		}
	}
	if !again || seen.readvised {
		return crux.KnowledgeScope{}, false
	}
	seen.readvised = true
	return ob.scope, true
}

// enqueue puts scopes (in pane order, so the last is the newest) at the head
// of the queue newest-first and caps the queue at the remaining per-kick
// allowance, dropping the oldest.
func (st *knowledgeKickState) enqueue(scopes []crux.KnowledgeScope, remaining int) {
	if remaining <= 0 {
		st.queue = nil
		return
	}
	queue := make([]crux.KnowledgeScope, 0, len(scopes)+len(st.queue))
	for i := len(scopes) - 1; i >= 0; i-- {
		queue = append(queue, scopes[i])
	}
	queue = append(queue, st.queue...)
	if len(queue) > remaining {
		queue = queue[:remaining]
	}
	st.queue = queue
}

// dispatch starts the next queued Advise call when none is in flight for the
// agent, and waits up to knowledgeAdvisorAdviseGrace for calls to finish so a
// fast adviser's results land on this tick. It never runs two calls for one
// agent at once and never holds the tick past the grace period.
func (ka *knowledgeAdvisor) dispatch(name string, st *knowledgeKickState, scrollback string, policy crux.KnowledgeAdvisorPolicy) {
	deadline := time.Now().Add(knowledgeAdvisorAdviseGrace)
	for {
		ka.mu.Lock()
		done := ka.inFlight[name]
		if done == nil {
			if len(st.queue) == 0 {
				ka.mu.Unlock()
				return
			}
			scope := st.queue[0]
			st.queue = st.queue[1:]
			if policy.WantsPaneTail {
				scope.PaneTail = knowledgePaneTail(scrollback, policy.PaneLines)
			}
			done = make(chan struct{})
			ka.inFlight[name] = done
			go ka.advise(name, st.kickAt, scope, done)
		}
		ka.mu.Unlock()

		wait := time.Until(deadline)
		if wait <= 0 {
			return
		}
		timer := time.NewTimer(wait)
		select {
		case <-done:
			timer.Stop()
		case <-timer.C:
			return
		}
	}
}

// advise runs one Advise call off the poll goroutine and folds its result
// into the agent's kick state, unless the kick captured when the call was
// made (kickAt, from agent.LastKick) is no longer the current one.
func (ka *knowledgeAdvisor) advise(name string, kickAt time.Time, scope crux.KnowledgeScope, done chan struct{}) {
	defer close(done)
	found := ka.adviser.Advise(name, scope)

	ka.mu.Lock()
	defer ka.mu.Unlock()
	if ka.inFlight[name] == done {
		delete(ka.inFlight, name)
	}
	st := ka.agents[name]
	if st == nil || !st.kickAt.Equal(kickAt) {
		return
	}
	for _, adv := range found {
		if adv.Slug == "" || st.delivered[adv.Slug] || knowledgeAdvicePending(st.pending, adv.Slug) {
			continue
		}
		st.pending = append(st.pending, adv)
	}
}

// markDelivered records a typed slug against the agent's current kick and
// takes it off the pending list.
func (ka *knowledgeAdvisor) markDelivered(name, slug string) {
	ka.mu.Lock()
	defer ka.mu.Unlock()
	st := ka.agents[name]
	if st == nil {
		return
	}
	for i, p := range st.pending {
		if p.Slug == slug {
			st.pending = append(st.pending[:i:i], st.pending[i+1:]...)
			break
		}
	}
	st.delivered[slug] = true
	st.count++
	st.lastAt = ka.now()
}

// knowledgePaneTail is the last lines of scrollback with tokens redacted, the
// only pane text that may cross the adviser seam.
func knowledgePaneTail(scrollback string, lines int) string {
	if lines <= 0 {
		lines = knowledgeAdvisorDefaultPaneLines
	}
	all := strings.Split(strings.TrimRight(scrollback, "\n"), "\n")
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	return logscrub.RedactPaneTokens(strings.Join(all, "\n"))
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
// working in, in first-seen order, one entry per key. A path scope's Action is
// "edit" if any line edits in it, otherwise the action of its first line.
func observedKnowledgeScopes(pane string) []crux.KnowledgeScope {
	var out []crux.KnowledgeScope
	for _, ob := range observeKnowledge(pane) {
		out = append(out, ob.scope)
	}
	return out
}

// observeKnowledge is observedKnowledgeScopes plus, for each gh family, the
// commands seen in it.
func observeKnowledge(pane string) []knowledgeObservation {
	pane = stripExplainLines(pane)
	var out []knowledgeObservation
	index := map[string]int{}
	add := func(sc crux.KnowledgeScope, command string) {
		if sc.Key == "" {
			return
		}
		i, ok := index[sc.Key]
		if !ok {
			i = len(out)
			index[sc.Key] = i
			out = append(out, knowledgeObservation{scope: sc})
		} else if sc.Action == crux.KnowledgeActionEdit {
			out[i].scope.Action = crux.KnowledgeActionEdit
		}
		if command != "" && !containsString(out[i].commands, command) {
			out[i].commands = append(out[i].commands, command)
		}
	}
	var gh [][]string
	for _, line := range strings.Split(pane, "\n") {
		action := knowledgeLineAction(line)
		for _, match := range knowledgePathRe.FindAllStringSubmatch(line, -1) {
			sc := knowledgePathScope(match[1])
			sc.Action = action
			add(sc, "")
		}
		gh = append(gh, knowledgeGHRe.FindAllStringSubmatch(line, -1)...)
	}
	// gh families follow the path scopes, as they always have.
	for _, match := range gh {
		add(crux.KnowledgeScope{
			Kind:     knowledgeAdvisorKindGH,
			Key:      "gh:" + match[1],
			Keywords: knowledgeGHKeywords[match[1]],
			Action:   crux.KnowledgeActionRun,
		}, match[2])
	}
	return out
}

// knowledgeLineAction tags the pane line an observation came from: a CLI
// write tool is an edit, a shell command is a run, anything else a read.
func knowledgeLineAction(line string) string {
	switch {
	case knowledgeEditLineRe.MatchString(line):
		return crux.KnowledgeActionEdit
	case knowledgeShellLineRe.MatchString(line):
		return crux.KnowledgeActionRun
	default:
		return crux.KnowledgeActionRead
	}
}

func containsString(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
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
	if summary := headRunes(oneLine(adv.Summary), knowledgeAdvisorMaxSummary); summary != "" {
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

// headRunes keeps s within n runes, ellipsis included, so the typed line's
// body never exceeds the documented cap.
func headRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
