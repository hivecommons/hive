// Package originlabel defines the origin/<source> provenance label family:
// the one label every hive-authored GitHub issue carries to say where it
// came from. It is deliberately dependency-free so every filer — the
// hive-open-issue relay, the review backlog, advisory and recommendations
// reports, self-reports, the hub's feedback and self-report relays, the
// upstream watcher and the convergence publisher — can share one vocabulary
// and one colour.
//
// origin/* is informational only. It is distinct from agent/<lane> (which
// agent works the item; a routing/ownership signal), stage/* (where the item
// is in the pipeline) and author/* (trust tier). No code path reads an
// origin/* label as a gate.
package originlabel

import "strings"

// Prefix is the family prefix; Color is the one colour every member shares
// (the purple the retired from-review label already used).
const (
	Prefix = "origin/"
	Color  = "8250df"
)

// The vocabulary. Each filing path applies exactly one of these at creation.
const (
	// Agent is the fallback for an issue filed through the hive-open-issue
	// relay by an agent whose name is unknown; a known filer gets
	// origin/<agent-name> from ForAgent instead.
	Agent = Prefix + "agent"
	// Review is a follow-up issue the review swarm filed from a backlog
	// finding (review.backlog).
	Review = Prefix + "review"
	// Advisory is the pinned advisory report issue.
	Advisory = Prefix + "advisory"
	// Recommendations is the standing merge-recommendations issue.
	Recommendations = Prefix + "recommendations"
	// SelfReport is a runtime self-report / fleet report, filed directly by a
	// hive or through the hub's self-report relay.
	SelfReport = Prefix + "selfreport"
	// Feedback is user feedback the hub or dashboard filed on a user's behalf.
	Feedback = Prefix + "feedback"
	// Upstream is a port request the upstream watcher filed.
	Upstream = Prefix + "upstream"
	// Audit is a convergence audit finding the publisher filed (public
	// audit-finding issue or private security-disclosure channel).
	Audit = Prefix + "audit"
	// Dashboard is an issue an operator filed from a dashboard action (for
	// example an ACMM criterion gap).
	Dashboard = Prefix + "dashboard"
)

// Retired maps pre-family provenance names to their origin/* replacement.
var Retired = map[string]string{
	"from-review": Review,
}

// All lists the vocabulary in display order.
var All = []string{Agent, Review, Advisory, Recommendations, SelfReport, Feedback, Upstream, Audit, Dashboard}

// Descriptions are the GitHub label descriptions for each member.
var Descriptions = map[string]string{
	Agent:           "Filed through the hive-open-issue relay by a hive agent whose name was not recorded (provenance only)",
	Review:          "Filed by Hive from a review-swarm backlog finding (provenance only, not a routing or hold signal)",
	Advisory:        "Hive's pinned advisory report (provenance only; hive/advisory is the standing-issue signal)",
	Recommendations: "Hive's standing merge-recommendations report (provenance only)",
	SelfReport:      "Hive runtime self-report / fleet report (provenance only; hive/selfreport is the dedupe signal)",
	Feedback:        "User feedback filed by the Hive hub or dashboard on a user's behalf (provenance only)",
	Upstream:        "Upstream change the Hive upstream watcher asked to port (provenance only)",
	Audit:           "Convergence audit finding published by Hive (provenance only)",
	Dashboard:       "Filed by an operator from a Hive dashboard action (provenance only)",
}

// ForAgent returns the origin/<agent-name> label for an issue a named agent
// filed through the hive-open-issue relay (origin/scanner, origin/quality,
// …). The name is lower-cased and reduced to GitHub-safe label characters;
// an empty or unusable name falls back to Agent.
func ForAgent(agent string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(agent)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == ':':
			b.WriteByte('-')
		}
	}
	name := strings.Trim(b.String(), "-._")
	if name == "" {
		return Agent
	}
	return Prefix + name
}

// Is reports whether name belongs to the origin/* family.
func Is(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, Prefix) && len(name) > len(Prefix)
}

// Canonical maps a retired provenance label to its origin/* replacement, or
// returns the trimmed input unchanged. renamed reports whether a mapping
// applied.
func Canonical(name string) (canonical string, renamed bool) {
	name = strings.TrimSpace(name)
	if to, ok := Retired[strings.ToLower(name)]; ok {
		return to, true
	}
	return name, false
}

// Description returns the label description for an origin/* label; unknown
// sources get a generic one so a future filer only has to pick a name.
func Description(name string) string {
	name = strings.TrimSpace(name)
	if d, ok := Descriptions[name]; ok {
		return d
	}
	if Is(name) {
		return "Filed by the hive agent \"" + strings.TrimPrefix(name, Prefix) + "\" through the hive-open-issue relay (provenance only; agent/<lane> says who works it)"
	}
	return "Hive provenance: where this issue came from (origin/<source>; informational only)"
}

// With returns labels carrying exactly one origin/* label, origin. It takes
// the slot of the first origin/* or retired provenance name in labels (so a
// configured "from-review, nit" files as "origin/review, nit") or is appended
// when there was none; every other origin/* and retired name is dropped,
// blanks are skipped and duplicates fold case-insensitively, so no caller can
// double-label an item however its inputs were assembled. An origin that is
// not an origin/* label leaves the input de-duplicated but adds nothing.
func With(labels []string, origin string) []string {
	origin = strings.TrimSpace(origin)
	place := Is(origin)
	out := make([]string, 0, len(labels)+1)
	seen := map[string]bool{}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		_, retired := Canonical(l)
		if Is(l) || retired {
			if place {
				out = append(out, origin)
				place = false
			}
			continue
		}
		key := strings.ToLower(l)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, l)
	}
	if place {
		out = append(out, origin)
	}
	return out
}

// Of returns the origin/* label present in labels, or "" when there is none.
func Of(labels []string) string {
	for _, l := range labels {
		if Is(l) {
			return strings.TrimSpace(l)
		}
	}
	return ""
}
