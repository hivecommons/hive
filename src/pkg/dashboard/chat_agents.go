package dashboard

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Hive Chat agent intents (#14409). Before this file every query carrying the
// token `agent`/`agents` — `/agents stuck`, `/agents scanner status`, "Which
// agent completed last?" — dumped the full roster. The frontend forwards
// `/agents <args>` as `agents: <args>` (and bare `/agents` as `agents:
// status`), so the matcher below sees the same shape for slash commands and
// natural language: an ordered, lower-cased token list.

// chatAgentIntentKind names what an agent query is asking for.
type chatAgentIntentKind string

const (
	chatAgentRoster        chatAgentIntentKind = "roster"
	chatAgentFilter        chatAgentIntentKind = "filter"
	chatAgentDetail        chatAgentIntentKind = "detail"
	chatAgentLastCompleted chatAgentIntentKind = "last-completed"
	chatAgentLastRun       chatAgentIntentKind = "last-run"
)

// chatAgentIntent is the parsed form of an agent question.
type chatAgentIntent struct {
	Kind chatAgentIntentKind
	// Filter is the roster filter for Kind == chatAgentFilter: one of the
	// chatAgentFilter* constants.
	Filter string
	// Agent is the resolved agent name for Kind == chatAgentDetail.
	Agent string
	// Aspect is the detail aspect: status (default), why, last, history.
	Aspect string
}

const (
	chatAgentFilterBlocked = "blocked"
	chatAgentFilterPaused  = "paused"
	chatAgentFilterRunning = "running"
	chatAgentFilterIdle    = "idle"
	chatAgentFilterFailed  = "failed"
)

// chatAgentFilterWords maps a query token to a roster filter.
var chatAgentFilterWords = map[string]string{
	"stuck":    chatAgentFilterBlocked,
	"blocked":  chatAgentFilterBlocked,
	"blocker":  chatAgentFilterBlocked,
	"blockers": chatAgentFilterBlocked,
	"paused":   chatAgentFilterPaused,
	"pause":    chatAgentFilterPaused,
	"running":  chatAgentFilterRunning,
	"active":   chatAgentFilterRunning,
	"working":  chatAgentFilterRunning,
	"busy":     chatAgentFilterRunning,
	"idle":     chatAgentFilterIdle,
	"free":     chatAgentFilterIdle,
	"failed":   chatAgentFilterFailed,
	"failing":  chatAgentFilterFailed,
	"crashed":  chatAgentFilterFailed,
	"crashing": chatAgentFilterFailed,
	"errored":  chatAgentFilterFailed,
	"stopped":  chatAgentFilterFailed,
	"down":     chatAgentFilterFailed,
}

// chatAgentAspectWords maps a query token to a detail-card aspect.
var chatAgentAspectWords = map[string]string{
	"status":  "status",
	"state":   "status",
	"doing":   "status",
	"detail":  "status",
	"details": "status",
	"info":    "status",
	"why":     "why",
	"reason":  "why",
	"blocked": "why",
	"stuck":   "why",
	"last":    "last",
	"ran":     "last",
	"run":     "last",
	"when":    "last",
	"history": "history",
	"kicks":   "history",
	"restart": "history",
	"runs":    "history",
}

// chatAgentScopeWords are the tokens that put a query in agent scope even
// when it names no agent.
var chatAgentScopeWords = map[string]bool{"agent": true, "agents": true}

// chatAgentListWords ask for the roster explicitly.
var chatAgentListWords = map[string]bool{"list": true, "all": true, "roster": true, "show": true, "status": true}

func chatIntentFields(query string) []string {
	normalized := strings.ToLower(strings.TrimSpace(query))
	normalized = chatIntentSplitRE.ReplaceAllString(normalized, " ")
	fields := strings.Fields(normalized)
	out := fields[:0]
	for _, f := range fields {
		f = strings.TrimPrefix(f, "/")
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// chatAgentNameIndex resolves tokens to agent names case-insensitively,
// accepting either the config name or the display name's slug.
func chatAgentNameIndex(agents []FrontendAgent) map[string]string {
	idx := make(map[string]string, len(agents)*2)
	for _, a := range agents {
		name := strings.TrimSpace(a.Name)
		if name == "" {
			continue
		}
		idx[strings.ToLower(name)] = name
		if dn := strings.ToLower(strings.TrimSpace(a.DisplayName)); dn != "" {
			idx[strings.ReplaceAll(dn, " ", "-")] = name
		}
	}
	return idx
}

// parseChatAgentIntent decides whether query is an agent question and, if so,
// what it asks. `inScope` is false when the query neither mentions agents nor
// names one, so the caller can fall through to the other local intents.
// `unknown` carries a token that LOOKED like an agent name (position right
// after the `agents` scope word or an aspect word) but matched no agent.
func parseChatAgentIntent(query string, agents []FrontendAgent) (intent chatAgentIntent, inScope bool, unknown string) {
	fields := chatIntentFields(query)
	if len(fields) == 0 {
		return chatAgentIntent{}, false, ""
	}
	names := chatAgentNameIndex(agents)

	scoped := false
	var filter, aspect, agent string
	var others []string
	sawWhich, sawLast, sawCompleted, sawRun := false, false, false, false
	for _, f := range fields {
		switch {
		case chatAgentScopeWords[f]:
			scoped = true
			continue
		case names[f] != "":
			if agent == "" {
				agent = names[f]
			}
			continue
		}
		if filter == "" {
			if fw, ok := chatAgentFilterWords[f]; ok {
				filter = fw
			}
		}
		if aw, ok := chatAgentAspectWords[f]; ok && aspect == "" {
			aspect = aw
		}
		switch f {
		case "which", "who", "what":
			sawWhich = true
		case "last", "latest", "recently", "recent", "most":
			sawLast = true
		case "completed", "complete", "finished", "finish", "done", "ended":
			sawCompleted = true
		case "ran", "run", "kicked", "started":
			sawRun = true
		}
		others = append(others, f)
	}

	if agent != "" {
		if aspect == "" {
			aspect = "status"
		}
		return chatAgentIntent{Kind: chatAgentDetail, Agent: agent, Aspect: aspect}, true, ""
	}
	if !scoped {
		return chatAgentIntent{}, false, ""
	}

	// Scoped, no agent named. Superlative questions first: "which agent
	// completed last" / "which agent ran most recently".
	if sawCompleted && (sawLast || sawWhich) {
		return chatAgentIntent{Kind: chatAgentLastCompleted}, true, ""
	}
	if sawRun && (sawLast || sawWhich) {
		return chatAgentIntent{Kind: chatAgentLastRun}, true, ""
	}
	if filter != "" {
		return chatAgentIntent{Kind: chatAgentFilter, Filter: filter}, true, ""
	}
	if sawLast {
		return chatAgentIntent{Kind: chatAgentLastCompleted}, true, ""
	}
	// `/agents status <name>` / `/agents <name> status` with an unknown name:
	// the leftover non-keyword token is what the operator typed. Only a short
	// query qualifies — a long free-text question with one unrecognised word
	// is a question for the responder, not a typo.
	for _, f := range others {
		if chatAgentListWords[f] || chatAgentAspectWords[f] != "" || chatAgentFilterWords[f] != "" {
			continue
		}
		switch f {
		case "is", "are", "the", "of", "for", "a", "an", "which", "who", "what", "why", "when", "how", "did", "does", "do", "me", "please", "tell", "about", "on", "in", "my", "hive":
			continue
		}
		unknown = f
		break
	}
	if unknown != "" && len(others) <= 2 {
		return chatAgentIntent{Kind: chatAgentDetail, Agent: unknown, Aspect: aspect}, true, unknown
	}
	if len(others) == 0 {
		return chatAgentIntent{Kind: chatAgentRoster}, true, ""
	}
	allList := true
	for _, f := range others {
		if !chatAgentListWords[f] && f != "the" && f != "me" && f != "show" {
			allList = false
			break
		}
	}
	if allList {
		return chatAgentIntent{Kind: chatAgentRoster}, true, ""
	}
	return chatAgentIntent{}, true, ""
}

// chatAgentsAnswer answers an agent-scoped query from the status snapshot.
// ok is false when the query is in agent scope but matched no intent, so the
// caller can hand it to the configured responder with the agent state as
// context instead of dumping the roster.
func (s *Server) chatAgentsAnswer(query string) (answer string, inScope bool, ok bool) {
	status := s.chatStatusSnapshot()
	var agents []FrontendAgent
	if status != nil {
		agents = status.Agents
	}
	intent, inScope, unknown := parseChatAgentIntent(query, agents)
	if !inScope {
		return "", false, false
	}
	if status == nil {
		return "Agent status is still initializing.", true, true
	}
	if len(agents) == 0 {
		return "No agents are currently reported in the hive status.", true, true
	}
	if unknown != "" {
		return renderChatAgentUnknown(unknown, agents), true, true
	}
	now := time.Now()
	switch intent.Kind {
	case chatAgentRoster:
		return renderChatAgentRoster(agents, now), true, true
	case chatAgentFilter:
		return renderChatAgentFilter(agents, intent.Filter, now), true, true
	case chatAgentDetail:
		for _, a := range agents {
			if a.Name == intent.Agent {
				return renderChatAgentDetail(a, intent.Aspect, now), true, true
			}
		}
		return renderChatAgentUnknown(intent.Agent, agents), true, true
	case chatAgentLastCompleted:
		return renderChatAgentLastCompleted(agents, now), true, true
	case chatAgentLastRun:
		return renderChatAgentLastRun(agents, now), true, true
	}
	return "", true, false
}

// chatAgentStateContext is the compact agent-state digest appended to an
// unmatched agent question before it goes to the configured responder.
func (s *Server) chatAgentStateContext() string {
	status := s.chatStatusSnapshot()
	if status == nil || len(status.Agents) == 0 {
		return ""
	}
	return "Hive agent state for reference:\n" + renderChatAgentRoster(status.Agents, time.Now())
}

func sortedChatAgents(agents []FrontendAgent) []FrontendAgent {
	out := append([]FrontendAgent(nil), agents...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func chatAgentNames(agents []FrontendAgent) []string {
	names := make([]string, 0, len(agents))
	for _, a := range sortedChatAgents(agents) {
		names = append(names, a.Name)
	}
	return names
}

func chatAgentBlocked(a FrontendAgent) bool {
	return strings.EqualFold(strings.TrimSpace(a.StructuredStatus), "BLOCKED")
}

func chatAgentReason(a FrontendAgent) string {
	if r := strings.TrimSpace(a.BlockedReason); r != "" {
		return r
	}
	return blockedReasonFromEvidence("BLOCKED", a.StatusEvidence)
}

func chatAgentState(a FrontendAgent) string {
	state := strings.TrimSpace(a.State)
	if state == "" {
		state = "unknown"
	}
	if a.Starting {
		state = "starting"
	}
	if a.Paused {
		state += " (paused)"
	}
	return state
}

// chatAgentMatchesFilter is the single definition of each roster filter.
func chatAgentMatchesFilter(a FrontendAgent, filter string) bool {
	state := strings.ToLower(strings.TrimSpace(a.State))
	switch filter {
	case chatAgentFilterBlocked:
		return chatAgentBlocked(a)
	case chatAgentFilterPaused:
		return a.Paused
	case chatAgentFilterRunning:
		return state == "running" && !a.Paused && strings.EqualFold(a.Busy, "working")
	case chatAgentFilterIdle:
		return state == "running" && !a.Paused && !strings.EqualFold(a.Busy, "working")
	case chatAgentFilterFailed:
		return (state != "running" && !a.Paused && !a.Starting && a.Enabled) || a.Condition != ""
	}
	return false
}

// chatRelative renders an RFC3339 timestamp as "<abs> (<ago> ago)"; empty
// in, empty out. Unparseable input is returned verbatim.
func chatRelative(ts string, now time.Time) string {
	ts = strings.TrimSpace(ts)
	if ts == "" {
		return ""
	}
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ts
	}
	d := now.Sub(t)
	if d < 0 {
		return t.UTC().Format("2006-01-02 15:04 UTC")
	}
	return fmt.Sprintf("%s (%s ago)", t.UTC().Format("2006-01-02 15:04 UTC"), chatHumanDuration(d))
}

func chatHumanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}

func chatAgentRosterLine(a FrontendAgent, now time.Time) string {
	line := fmt.Sprintf("- %s: %s", a.Name, chatAgentState(a))
	if chatAgentBlocked(a) {
		line += " — BLOCKED: " + chatAgentReason(a)
		if since := chatRelative(a.BlockedSince, now); since != "" {
			line += " (since " + since + ")"
		}
		return line
	}
	detail := strings.TrimSpace(a.Doing)
	if detail == "" {
		detail = strings.TrimSpace(a.StructuredStatus)
	}
	if detail != "" {
		line += " — " + detail
	}
	return line
}

func renderChatAgentRoster(agents []FrontendAgent, now time.Time) string {
	sorted := sortedChatAgents(agents)
	lines := make([]string, 0, len(sorted))
	for _, a := range sorted {
		lines = append(lines, chatAgentRosterLine(a, now))
	}
	return fmt.Sprintf("Agents (%d):\n%s", len(sorted), strings.Join(lines, "\n"))
}

func renderChatAgentFilter(agents []FrontendAgent, filter string, now time.Time) string {
	sorted := sortedChatAgents(agents)
	var lines []string
	for _, a := range sorted {
		if !chatAgentMatchesFilter(a, filter) {
			continue
		}
		switch filter {
		case chatAgentFilterBlocked:
			line := fmt.Sprintf("- **%s** (%s) — %s", a.Name, chatAgentState(a), chatAgentReason(a))
			if since := chatRelative(a.BlockedSince, now); since != "" {
				line += "; since " + since
			}
			lines = append(lines, line)
		case chatAgentFilterPaused:
			line := fmt.Sprintf("- **%s** — paused", a.Name)
			if r := strings.TrimSpace(a.PausedReason); r != "" {
				line += ": " + r
			}
			if by := strings.TrimSpace(a.PausedBy); by != "" {
				line += " (by " + by + ")"
			}
			if since := chatRelative(a.PausedAt, now); since != "" {
				line += "; since " + since
			}
			lines = append(lines, line)
		case chatAgentFilterRunning:
			line := fmt.Sprintf("- **%s** — working", a.Name)
			if d := strings.TrimSpace(a.Doing); d != "" {
				line += ": " + d
			}
			if since := chatRelative(a.BusySince, now); since != "" {
				line += "; since " + since
			}
			lines = append(lines, line)
		case chatAgentFilterIdle:
			line := fmt.Sprintf("- **%s** — idle", a.Name)
			if since := chatRelative(a.IdleSince, now); since != "" {
				line += " since " + since
			} else if last := chatRelative(a.KickOutcomeAt, now); last != "" {
				line += "; last turn ended " + last
			}
			if nk := strings.TrimSpace(a.NextKickIn); nk != "" {
				line += "; next kick in " + nk
			}
			lines = append(lines, line)
		default:
			line := fmt.Sprintf("- **%s** — %s", a.Name, chatAgentState(a))
			if c := strings.TrimSpace(a.ConditionMessage); c != "" {
				line += ": " + c
			} else if e := strings.TrimSpace(a.LastError); e != "" {
				line += ": " + e
			}
			if a.Restarts > 0 {
				line += fmt.Sprintf(" (%d restarts)", a.Restarts)
			}
			lines = append(lines, line)
		}
	}
	if len(lines) == 0 {
		return fmt.Sprintf("No %s agents right now (%d agents total).", filter, len(sorted))
	}
	return fmt.Sprintf("%s agents (%d of %d):\n%s", strings.ToUpper(filter[:1])+filter[1:], len(lines), len(sorted), strings.Join(lines, "\n"))
}

func renderChatAgentUnknown(name string, agents []FrontendAgent) string {
	return fmt.Sprintf("No agent named `%s`; agents: %s", name, strings.Join(chatAgentNames(agents), ", "))
}

func chatAgentLastActionLine(a FrontendAgent, now time.Time) string {
	la := a.LastAction
	if la == nil || la.Number == 0 {
		return ""
	}
	ref := fmt.Sprintf("%s#%d", la.Repo, la.Number)
	line := ref
	if la.Action != "" {
		line = la.Action + " " + ref
	}
	if la.Current {
		line += " (current)"
	}
	if at := chatRelative(la.At, now); at != "" {
		line += " at " + at
	}
	return line
}

// renderChatAgentDetail is the single-agent card. Aspect narrows it: "why"
// leads with the block reason, "last" with run timing, "history" with the
// kick/restart record; "status" (default) prints everything.
func renderChatAgentDetail(a FrontendAgent, aspect string, now time.Time) string {
	head := fmt.Sprintf("**%s** — %s", a.Name, chatAgentState(a))
	if chatAgentBlocked(a) {
		head += ", BLOCKED"
	} else if strings.EqualFold(a.Busy, "working") {
		if a.AwaitingCI {
			head += ", waiting on CI"
		} else {
			head += ", working"
		}
	} else if strings.EqualFold(a.State, "running") {
		head += ", idle"
	}
	var lines []string
	add := func(label, value string) {
		if v := strings.TrimSpace(value); v != "" {
			lines = append(lines, fmt.Sprintf("- %s: %s", label, v))
		}
	}

	blocked := func() {
		if !chatAgentBlocked(a) {
			if aspect == "why" {
				lines = append(lines, "- Not blocked.")
			}
			return
		}
		reason := chatAgentReason(a)
		if since := chatRelative(a.BlockedSince, now); since != "" {
			reason += " (since " + since + ")"
		}
		add("Blocked", reason)
		if a.KickOutcome != "" && a.KickOutcomeReason != "" && !strings.Contains(reason, a.KickOutcomeReason) {
			add("Last turn outcome", a.KickOutcome+": "+a.KickOutcomeReason)
		}
	}
	paused := func() {
		if !a.Paused {
			return
		}
		p := "yes"
		if r := strings.TrimSpace(a.PausedReason); r != "" {
			p += " — " + r
		}
		if by := strings.TrimSpace(a.PausedBy); by != "" {
			p += " (by " + by + ")"
		}
		if since := chatRelative(a.PausedAt, now); since != "" {
			p += "; since " + since
		}
		add("Paused", p)
	}
	current := func() {
		doing := strings.TrimSpace(a.Doing)
		if doing == "" {
			doing = strings.TrimSpace(a.LiveSummary)
		}
		if doing == "" && strings.EqualFold(a.Busy, "working") {
			doing = "working"
		}
		if doing != "" {
			if since := chatRelative(a.BusySince, now); since != "" && strings.EqualFold(a.Busy, "working") {
				doing += " (since " + since + ")"
			}
			add("Doing", doing)
		}
		add("Last output", chatRelative(a.LastTranscriptActivity, now))
		if a.Condition != "" && !chatAgentBlocked(a) {
			add("Condition", a.Condition+": "+a.ConditionMessage)
		}
	}
	last := func() {
		if a.KickOutcomeAt != "" {
			out := chatRelative(a.KickOutcomeAt, now)
			if a.KickOutcome != "" {
				out += " — " + a.KickOutcome
				if r := strings.TrimSpace(a.KickOutcomeReason); r != "" {
					out += ": " + r
				}
			}
			add("Last completed turn", out)
		} else if a.LastKickEnd != "" {
			add("Last completed turn", chatRelative(a.LastKickEnd, now))
		}
		kick := chatRelative(a.LastKickAt, now)
		if kick == "" {
			kick = strings.TrimSpace(a.LastKick)
		}
		add("Last kick", kick)
		if nk := strings.TrimSpace(a.NextKickIn); nk != "" {
			add("Next kick", "in "+nk)
		}
		add("Last PR/issue", chatAgentLastActionLine(a, now))
	}
	history := func() {
		add("Restarts", fmt.Sprintf("%d", a.Restarts))
		if a.ContinuousKicks > 0 {
			add("Continuous kicks", fmt.Sprintf("%d", a.ContinuousKicks))
		}
		if a.KicksUndeliverable > 0 {
			add("Undeliverable kicks", fmt.Sprintf("%d", a.KicksUndeliverable))
		}
		nudges := a.StallNudges + a.ActionNudges + a.TransientNudges + a.CIPollNudges
		if nudges > 0 {
			add("Nudges", fmt.Sprintf("%d (stall %d, action %d, transient %d, CI-poll %d)", nudges, a.StallNudges, a.ActionNudges, a.TransientNudges, a.CIPollNudges))
		}
		if a.ProxyViolations > 0 {
			add("Proxy violations", fmt.Sprintf("%d", a.ProxyViolations))
		}
		if e := strings.TrimSpace(a.LastError); e != "" && !strings.Contains(chatAgentReason(a), e) {
			add("Last error", e)
		}
	}

	switch aspect {
	case "why":
		blocked()
		paused()
		current()
	case "last":
		last()
		current()
	case "history":
		history()
		last()
	default:
		blocked()
		paused()
		current()
		last()
		history()
	}
	if len(lines) == 0 {
		return head
	}
	return head + "\n" + strings.Join(lines, "\n")
}

func chatParseTime(ts string) time.Time {
	t, err := time.Parse(time.RFC3339, strings.TrimSpace(ts))
	if err != nil {
		return time.Time{}
	}
	return t
}

// renderChatAgentLastCompleted answers "which agent completed last": the
// agent whose last kicked turn ENDED most recently (KickOutcomeAt), falling
// back to the continuous LastKickEnd for agents the outcome tracker skips.
func renderChatAgentLastCompleted(agents []FrontendAgent, now time.Time) string {
	type cand struct {
		a  FrontendAgent
		at time.Time
	}
	var cands []cand
	for _, a := range agents {
		at := chatParseTime(a.KickOutcomeAt)
		if at.IsZero() {
			at = chatParseTime(a.LastKickEnd)
		}
		if !at.IsZero() {
			cands = append(cands, cand{a, at})
		}
	}
	if len(cands) == 0 {
		return "No agent has a recorded completed run yet."
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].at.Equal(cands[j].at) {
			return cands[i].a.Name < cands[j].a.Name
		}
		return cands[i].at.After(cands[j].at)
	})
	top := cands[0]
	line := fmt.Sprintf("**%s** completed last, at %s", top.a.Name, chatRelative(top.at.Format(time.RFC3339), now))
	if top.a.KickOutcome != "" {
		line += " — outcome " + top.a.KickOutcome
		if r := strings.TrimSpace(top.a.KickOutcomeReason); r != "" {
			line += ": " + r
		}
	}
	if la := chatAgentLastActionLine(top.a, now); la != "" {
		line += "; last PR/issue " + la
	}
	line += "."
	if len(cands) > 1 {
		var rest []string
		for _, c := range cands[1:] {
			if len(rest) == 4 {
				break
			}
			rest = append(rest, fmt.Sprintf("%s (%s)", c.a.Name, chatRelative(c.at.Format(time.RFC3339), now)))
		}
		line += "\nBefore that: " + strings.Join(rest, ", ") + "."
	}
	return line
}

// renderChatAgentLastRun answers "which agent ran last": most recent kick.
func renderChatAgentLastRun(agents []FrontendAgent, now time.Time) string {
	var best FrontendAgent
	var bestAt time.Time
	for _, a := range agents {
		at := chatParseTime(a.LastKickAt)
		if at.IsZero() {
			continue
		}
		if bestAt.IsZero() || at.After(bestAt) || (at.Equal(bestAt) && a.Name < best.Name) {
			best, bestAt = a, at
		}
	}
	if bestAt.IsZero() {
		return "No agent has a recorded kick yet."
	}
	line := fmt.Sprintf("**%s** ran last — kicked %s", best.Name, chatRelative(bestAt.Format(time.RFC3339), now))
	if strings.EqualFold(best.Busy, "working") {
		line += ", still working"
	} else if best.KickOutcome != "" {
		line += ", turn ended with " + best.KickOutcome
	}
	return line + "."
}
