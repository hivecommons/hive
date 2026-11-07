package crux

import "time"

// KnowledgeScope is one unit of work context the runtime observed an agent
// enter after its kick (hivecommons/hive#17052): a source directory it read
// or edited, or a GitHub CLI family it started using. Key is stable per
// scope and is what the runtime dedupes on within a kick.
type KnowledgeScope struct {
	// Kind is "path" or "gh".
	Kind string
	// Key is the dedupe key: "src/crux/agent" for a path scope, "gh:pr" for
	// a gh family.
	Key string
	// Path is the relative file path that opened a path scope; "" for gh.
	Path string
	// Keywords are the search terms the scope contributes to the matcher.
	Keywords []string
}

// KnowledgeAdvice is one knowledge entry the adviser matched to a scope.
// The runtime renders it as a single line typed into the live session.
type KnowledgeAdvice struct {
	// Slug identifies the entry; the runtime never delivers a slug twice in
	// one kick.
	Slug string
	// Title is the entry title.
	Title string
	// Summary is the body, already collapsed to one line and capped by the
	// adviser.
	Summary string
	// Ref tells the agent where the full entry is: a source URL or the
	// knowledge entry id.
	Ref string
}

// KnowledgeAdvisorPolicy is the delivery policy for one agent, resolved per
// call off the live hive config so a dashboard change applies without a
// restart.
type KnowledgeAdvisorPolicy struct {
	// Enabled false means the runtime neither parses the agent's pane for
	// scopes nor types anything.
	Enabled bool
	// MaxPerKick caps deliveries per kick; non-positive means the default.
	MaxPerKick int
	// Cooldown is the minimum gap between two deliveries to one agent;
	// non-positive means the default.
	Cooldown time.Duration
}

// KnowledgeAdviser is the hive side of the mid-task knowledge advisor.
type KnowledgeAdviser interface {
	// Policy returns the delivery policy for agent.
	Policy(agent string) KnowledgeAdvisorPolicy
	// Advise returns the entries worth surfacing to agent for a scope it has
	// just entered, excluding anything its kick already carried. Order is
	// best first. It runs on the agent's poll goroutine and must be bounded.
	Advise(agent string, scope KnowledgeScope) []KnowledgeAdvice
}
