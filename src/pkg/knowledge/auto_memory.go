package knowledge

import "strings"

// AutoMemoryTag marks a knowledge entry drained from a coding CLI's own
// memory store (#17033). The drain writes it both as a tag and as the
// entry's `source:` frontmatter.
const AutoMemoryTag = "cli-memory"

// IsAutoMemory reports whether f was drained from a CLI memory store: it
// carries the AutoMemoryTag tag or `source: cli-memory`. Either marker is
// enough, so the agent-facing exclusion fails closed.
func IsAutoMemory(f Fact) bool {
	return strings.EqualFold(strings.TrimSpace(f.Origin), AutoMemoryTag) || hasTagFold(f.Tags, AutoMemoryTag)
}

// hasTagFold reports whether tags holds want, ignoring case and surrounding
// space.
func hasTagFold(tags []string, want string) bool {
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), want) {
			return true
		}
	}
	return false
}

// IsAutoMemoryDraft reports whether f is a drained CLI-memory fact that is
// still a draft. Such a fact is agent-written text nobody has reviewed, so it
// never reaches an agent: not the kick primer, not the knowledge advisor, not
// an agent-identified TOC or entry read, whatever include_states the agent's
// knowledge.agent_scopes entry or the primer config admits (#17058). Only an
// operator approving it through the lifecycle API makes it primable.
func IsAutoMemoryDraft(f Fact) bool {
	return f.EffectiveState() == StateDraft && IsAutoMemory(f)
}
