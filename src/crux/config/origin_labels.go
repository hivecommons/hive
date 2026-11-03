package config

import (
	"regexp"
	"sort"
	"strings"
)

// DefaultReviewBacklogLabel is the label applied when review.backlog.labels
// is unset; reviewer is an agent lane, so agent/reviewer carries routing,
// ownership and provenance.
const DefaultReviewBacklogLabel = "agent/reviewer"

// Origin provenance labels. Non-lane hive issue filers use origin/<source>.
// Agent lanes use agent/<lane> instead.
const (
	OriginLabelPrefix = "origin/"
	// OriginReviewerLabel is retired; reviewer is an agent lane and now maps
	// to agent/reviewer.
	OriginReviewerLabel = OriginLabelPrefix + "reviewer"
	// LegacyOriginReviewLabel is the first origin/* spelling of the reviewer
	// lane provenance label. Config still accepts it; the hive maps it to
	// agent/reviewer when filing and migrates open items once at startup.
	LegacyOriginReviewLabel = OriginLabelPrefix + "review"
	// LegacyReviewBacklogLabel is the pre-origin/* name of the reviewer lane
	// provenance label. Config still accepts it; the hive maps it to
	// agent/reviewer when filing and migrates open items once at startup.
	LegacyReviewBacklogLabel = "from-review"
)

// OriginLabelRenames maps retired reviewer provenance label names to the
// canonical reviewer lane label. Readers use CanonicalOriginLabel; the startup
// reconciler swaps the old name for the new one on open items.
var OriginLabelRenames = map[string]string{
	LegacyReviewBacklogLabel: DefaultReviewBacklogLabel,
	LegacyOriginReviewLabel:  DefaultReviewBacklogLabel,
	OriginReviewerLabel:      DefaultReviewBacklogLabel,
}

// CanonicalOriginLabel returns the canonical replacement for a retired
// provenance label, maps legacy origin/<lane> labels to agent/<lane>, or
// returns the trimmed input unchanged when it is not renamed. renamed reports
// whether a mapping applied.
func CanonicalOriginLabel(name string) (canonical string, renamed bool) {
	name = strings.TrimSpace(name)
	if to, ok := OriginLabelRenames[strings.ToLower(name)]; ok {
		return to, true
	}
	if IsOriginLabel(name) {
		if to, ok := CanonicalAgentLaneLabel(name); ok {
			return to, true
		}
	}
	return name, false
}

// origin/<source> is the provenance family for hive issue filers that do not
// have an agent lane label. Agent-filed issues use agent/<lane> as their
// single lane label: that label carries routing, ownership and provenance.
// Humans filing directly get no origin label — there is deliberately no
// origin/human — so "no origin label and no agent label" means "a person
// filed this".
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

// OriginSourceReviewer is the reviewer lane's name. It is no longer an origin
// source; reviewer-filed issues use agent/reviewer.
const OriginSourceReviewer = "reviewer"

const agentLabelPrefix = "agent/"

// KnownOriginLanes are the agent lanes that file issues through the
// issue-request watcher or the GitHub proxy; each gets agent/<lane>, not
// origin/<lane>.
var KnownOriginLanes = []string{
	"architect", "ci-maintainer", "dedupe", "guide", "outreach", "quality",
	"reviewer", "scanner", "sec-check", "strategist", "supervisor", "tester",
}

// GitHubLabelDescriptionMax is GitHub's cap on a label description; a
// longer one makes the labels API answer 422 "description is too long".
const GitHubLabelDescriptionMax = 100

// originSourceDescriptions is the label description per non-lane source.
// Every entry must fit GitHubLabelDescriptionMax.
var originSourceDescriptions = map[string]string{
	OriginSourceSelfReport:  "Filed by Hive from a fleet runtime self-report (provenance only; supersedes hive/selfreport)",
	OriginSourceGovernor:    "Filed by the Hive governor/automation on its own behalf (provenance only, not routing or hold)",
	OriginSourceDashboard:   "Filed by an operator action in the Hive dashboard (provenance only, not a routing or hold signal)",
	OriginSourceForge:       "Filed through the Hive forge issue seam (provenance only, not a routing or hold signal)",
	OriginSourceConvergence: "Filed by the Hive convergence finding publisher (provenance only, not a routing or hold signal)",
}

// OriginLabel returns origin/<source> for a source name. The source is
// trimmed and lower-cased; an empty source yields "".
func OriginLabel(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	source = strings.TrimPrefix(source, OriginLabelPrefix)
	if IsKnownOriginLane(source) {
		return ""
	}
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
	if IsAgentLaneLabel(label) {
		return strings.ToLower(strings.TrimSpace(label))[len(agentLabelPrefix):]
	}
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

// OriginLaneFromAgent returns the lane an agent name belongs to: the name
// itself for a declared agent, the base name for a materialised replica
// (scanner-2 → scanner, mirroring replicaAgentName).
// It returns "" for a name that is not a lane slug.
func OriginLaneFromAgent(agent string) string {
	agent = replicaSuffixRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(agent)), "")
	if !originLaneRE.MatchString(agent) {
		return ""
	}
	return agent
}

// AgentLaneLabel returns agent/<lane> for a lane source.
func AgentLaneLabel(source string) string {
	source = strings.ToLower(strings.TrimSpace(source))
	source = strings.TrimPrefix(source, agentLabelPrefix)
	if source == "" {
		return ""
	}
	return agentLabelPrefix + source
}

// CanonicalAgentLaneLabel maps the accepted lane label spellings for a known
// agent lane to the single writer/reader form, agent/<lane>. During the
// origin/<lane> transition readers use this to understand legacy labels while
// writers emit only the returned agent/* label.
func CanonicalAgentLaneLabel(name string) (string, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	source := name
	switch {
	case strings.HasPrefix(name, agentLabelPrefix):
		source = strings.TrimPrefix(name, agentLabelPrefix)
	case strings.HasPrefix(name, OriginLabelPrefix):
		source = strings.TrimPrefix(name, OriginLabelPrefix)
	}
	source = OriginLaneFromAgent(source)
	if !IsKnownOriginLane(source) {
		return "", false
	}
	return AgentLaneLabel(source), true
}

// OriginLabelForAgent returns the provenance label for an agent name. Agent
// lanes use agent/<lane>; non-agent sources still use origin/<source>.
func OriginLabelForAgent(agent string) string {
	if label, ok := CanonicalAgentLaneLabel(agent); ok {
		return label
	}
	lane := OriginLaneFromAgent(agent)
	return OriginLabel(lane)
}

// IsKnownOriginLane reports whether source is one of the documented agent
// lanes that file issues.
func IsKnownOriginLane(source string) bool {
	source = strings.ToLower(strings.TrimSpace(source))
	for _, lane := range KnownOriginLanes {
		if source == lane {
			return true
		}
	}
	return false
}

// IsAgentLaneLabel reports whether name belongs to the agent/* lane family.
func IsAgentLaneLabel(name string) bool {
	name = strings.ToLower(strings.TrimSpace(name))
	return strings.HasPrefix(name, agentLabelPrefix) && len(name) > len(agentLabelPrefix)
}

// FirstOriginLabel returns the first origin/* label in labels after canonical
// legacy mapping (so origin/<lane> returns agent/<lane>) and whether one was
// present.
func FirstOriginLabel(labels []string) (string, bool) {
	for _, l := range labels {
		if IsOriginLabel(l) {
			canonical, _ := CanonicalOriginLabel(l)
			return canonical, true
		}
	}
	return "", false
}

// WithOriginLabel returns labels with exactly one non-lane origin/* label. An
// existing non-lane origin is kept (canonicalised, with further origin/*
// entries dropped); retired lane-origin spellings are mapped to their agent
// label and active origin/<lane> labels are dropped. When no non-lane origin is
// present, origin/<source> is appended. Blank entries are dropped. An empty
// source with no origin label present returns labels unchanged, so the caller
// can refuse.
func WithOriginLabel(labels []string, source string) []string {
	out := make([]string, 0, len(labels)+1)
	seen := false
	seenLabels := map[string]bool{}
	appendOut := func(label string) {
		key := strings.ToLower(strings.TrimSpace(label))
		if key == "" || seenLabels[key] {
			return
		}
		seenLabels[key] = true
		out = append(out, label)
	}
	for _, l := range labels {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		if IsOriginLabel(l) {
			canonical, renamed := CanonicalOriginLabel(l)
			if IsKnownOriginLane(OriginSourceOf(canonical)) {
				if renamed {
					appendOut(canonical)
				}
				continue
			}
			if seen {
				continue
			}
			seen = true
			l = canonical
		}
		appendOut(l)
	}
	if !seen {
		if label := OriginLabel(source); label != "" {
			appendOut(label)
		}
	}
	return out
}

// WithIssueOriginLabel returns an issue label list with exactly one
// provenance source for this filer. Known agent lanes get agent/<lane> and any
// redundant origin/<lane> or bare <lane> alias is removed. Non-lane sources use
// origin/<source> and leave unrelated labels untouched.
func WithIssueOriginLabel(labels []string, source string) []string {
	lane := OriginLaneFromAgent(source)
	if IsKnownOriginLane(lane) {
		return WithAgentLaneLabel(labels, lane)
	}
	originSource := source
	if existing, ok := FirstOriginLabel(labels); ok {
		if existingSource := OriginSourceOf(existing); !IsKnownOriginLane(existingSource) {
			originSource = existingSource
		}
	}
	return WithOriginLabel(labels, originSource)
}

func WithAgentLaneLabel(labels []string, lane string) []string {
	lane = OriginLaneFromAgent(lane)
	if !IsKnownOriginLane(lane) {
		return WithOriginLabel(labels, lane)
	}
	want := AgentLaneLabel(lane)
	out := make([]string, 0, len(labels)+1)
	seenAgent := false
	seenOrigin := false
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		name := strings.ToLower(strings.TrimSpace(label))
		if IsKnownOriginLane(name) {
			continue
		}
		if IsOriginLabel(name) {
			if IsKnownOriginLane(OriginSourceOf(name)) || seenOrigin {
				continue
			}
			seenOrigin = true
			label, _ = CanonicalOriginLabel(label)
		}
		if IsAgentLaneLabel(name) {
			if seenAgent {
				continue
			}
			seenAgent = true
			label = want
		}
		out = append(out, label)
	}
	if !seenAgent {
		out = append(out, want)
	}
	return out
}

// OriginLabelDescription returns the repo label description for a known
// non-lane origin/* label. ok is false for lane-origin labels and anything
// outside the origin family.
func OriginLabelDescription(label string) (desc string, ok bool) {
	source := OriginSourceOf(label)
	if source == "" {
		return "", false
	}
	if d, known := originSourceDescriptions[source]; known {
		return d, true
	}
	return "", false
}

// KnownOriginSources lists every origin source the hive documents: the
// non-agent filers, sorted.
func KnownOriginSources() []string {
	set := map[string]bool{}
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
