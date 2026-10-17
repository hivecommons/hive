package config

import (
	"regexp"
	"sort"
	"strings"
)

// origin/<source> is universal: every issue the hive creates carries exactly
// one label from this family saying where it came from. The source is either
// an agent lane (origin/<lane>, derived from HIVE_AGENT) or one of the
// non-agent filers below. Humans filing directly get no origin label — there
// is deliberately no origin/human — so "no origin label" means "a person
// filed this". The family is informational: nothing reads it as a gate, and
// agent/<lane> remains the routing/ownership axis.
const (
	// OriginSourceSelfReport is the fleet runtime self-report filer
	// (pkg/fleetreport, pkg/github/fleet_report.go, the hub relay). The
	// legacy hive/selfreport label is kept alongside for the joiner's
	// dedupe search but is deprecated as a provenance marker.
	OriginSourceSelfReport = "selfreport"
	// OriginSourceGovernor is governor/automation filing on its own behalf:
	// the advisory and recommendations living documents, and any future
	// sweep notice.
	OriginSourceGovernor = "governor"
	// OriginSourceDashboard is an operator action in the SPA, e.g. an ACMM
	// gap issue filed from the evaluation panel.
	OriginSourceDashboard = "dashboard"
	// OriginSourceForge is the forge-neutral issue seam when its caller
	// passed no origin of its own.
	OriginSourceForge = "forge"
	// OriginSourceConvergence is the convergence finding publisher.
	OriginSourceConvergence = "convergence"
)

// OriginSourceReviewer is the review swarm's source: OriginReviewerLabel
// without the prefix. It doubles as the reviewer lane's name.
const OriginSourceReviewer = "reviewer"

// KnownOriginLanes are the agent lanes that file issues through the
// issue-request watcher or the GitHub proxy; each gets origin/<lane>. A lane
// not listed here still gets its label (the source is derived, not looked
// up) with the generic lane description.
var KnownOriginLanes = []string{
	"architect", "ci-maintainer", "dedupe", "guide", "outreach", "quality",
	"reviewer", "scanner", "sec-check", "strategist", "supervisor", "tester",
}

// originSourceDescriptions is the label description per non-lane source.
var originSourceDescriptions = map[string]string{
	OriginSourceReviewer:    "Filed by the Hive reviewer swarm from a backlog finding (provenance only, not a routing or hold signal)",
	OriginSourceSelfReport:  "Filed by Hive from a fleet runtime self-report (provenance only; supersedes hive/selfreport as the origin marker)",
	OriginSourceGovernor:    "Filed by the Hive governor/automation on its own behalf (provenance only, not a routing or hold signal)",
	OriginSourceDashboard:   "Filed by an operator action in the Hive dashboard (provenance only, not a routing or hold signal)",
	OriginSourceForge:       "Filed through the Hive forge issue seam (provenance only, not a routing or hold signal)",
	OriginSourceConvergence: "Filed by the Hive convergence finding publisher (provenance only, not a routing or hold signal)",
}

// OriginLabel returns origin/<source> for a source name. The source is
// trimmed and lower-cased; an empty source yields "".
func OriginLabel(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	source = strings.TrimPrefix(source, OriginLabelPrefix)
	if source == "" {
		return ""
	}
	return OriginLabelPrefix + source
}

// IsOriginLabel reports whether name belongs to the origin/* family.
func IsOriginLabel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, OriginLabelPrefix) && len(name) > len(OriginLabelPrefix)
}

// OriginSourceOf returns the source part of an origin/* label (the
// canonical spelling of a retired name), or "" for any other label.
func OriginSourceOf(label string) string {
	label, _ = CanonicalOriginLabel(label)
	if !IsOriginLabel(label) {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(label))[len(OriginLabelPrefix):]
}

var (
	replicaSuffixRE = regexp.MustCompile(`-\d+$`)
	// originLaneRE is the slug shape a lane name must have to become a
	// label segment; anything else yields no lane rather than a bad label.
	originLaneRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_.-]*$`)
)

// OriginLaneFromAgent returns the lane an agent name belongs to for
// origin/<lane>: the name itself for a declared agent, the base name for a
// materialised replica (scanner-2 → scanner, mirroring replicaAgentName).
// It returns "" for a name that is not a lane slug.
func OriginLaneFromAgent(agent string) string {
	agent = replicaSuffixRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(agent)), "")
	if !originLaneRE.MatchString(agent) {
		return ""
	}
	return agent
}

// OriginLabelForAgent returns origin/<lane> for an agent name, or "" for an
// empty name.
func OriginLabelForAgent(agent string) string {
	return OriginLabel(OriginLaneFromAgent(agent))
}

// FirstOriginLabel returns the first origin/* label in labels (canonical
// spelling) and whether one was present.
func FirstOriginLabel(labels []string) (string, bool) {
	for _, l := range labels {
		if IsOriginLabel(l) {
			canonical, _ := CanonicalOriginLabel(l)
			return canonical, true
		}
	}
	return "", false
}

// WithOriginLabel returns labels with exactly one origin/* label. An origin
// label already present is kept (canonicalised, with any further origin/*
// entries dropped so an issue never carries two); otherwise origin/<source>
// is appended. Blank entries are dropped. An empty source with no origin
// label present returns labels unchanged, so the caller can refuse.
func WithOriginLabel(labels []string, source string) []string {
	out := make([]string, 0, len(labels)+1)
	seen := false
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if IsOriginLabel(l) {
			if seen {
				continue
			}
			seen = true
			l, _ = CanonicalOriginLabel(l)
		}
		out = append(out, l)
	}
	if !seen {
		if label := OriginLabel(source); label != "" {
			out = append(out, label)
		}
	}
	return out
}

// OriginLabelDescription returns the repo label description for an origin/*
// label: a specific text for the known non-lane sources, a lane text for
// everything else (lane names are open-ended). ok is false for a label
// outside the family.
func OriginLabelDescription(label string) (desc string, ok bool) {
	source := OriginSourceOf(label)
	if source == "" {
		return "", false
	}
	if d, known := originSourceDescriptions[source]; known {
		return d, true
	}
	return "Filed by the Hive " + source + " agent lane (provenance only; agent/" + source + " is the routing label)", true
}

// KnownOriginSources lists every origin source the hive documents: the
// lanes plus the non-agent filers, sorted.
func KnownOriginSources() []string {
	set := map[string]bool{}
	for _, lane := range KnownOriginLanes {
		set[lane] = true
	}
	for s := range originSourceDescriptions {
		set[s] = true
	}
	out := make([]string, 0, len(set))
	for s := range set {
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}
