package climemory

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/hivecommons/hive/pkg/knowledge"
)

var (
	// agentNameRe bounds the agent names the drain records in tags and in
	// spool paths: no path separators, no leading dot.
	agentNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	repoTagRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`)
	memhashRe   = regexp.MustCompile(`^[0-9a-f]{16,64}$`)
)

// ValidateCreate checks a knowledge create request against the drain's
// provenance rules (#17056). A request without the SourceTag is not a
// drained fact and passes. One that carries it must be created as draft,
// and every tag must be one the drain itself writes: SourceTag,
// agent:<name>, cli:<claude|codex|gemini>, memhash:<hex> and
// repo:<org>/<repo>. That keeps a caller from creating an approved fact
// that claims to come from CLI memory, or from smuggling arbitrary tags
// (which scopes and primers select on) onto one.
func ValidateCreate(state string, tags []string) error {
	drained := false
	for _, t := range tags {
		if strings.EqualFold(strings.TrimSpace(t), SourceTag) {
			drained = true
			break
		}
	}
	if !drained {
		return nil
	}
	st, err := knowledge.ParseLifecycleState(state)
	if err != nil || st != knowledge.StateDraft {
		return fmt.Errorf("a fact tagged %s must be created with state %s", SourceTag, knowledge.StateDraft)
	}
	for _, t := range tags {
		if !allowedDrainTag(strings.TrimSpace(t)) {
			return fmt.Errorf("tag %q is not allowed on a fact tagged %s", t, SourceTag)
		}
	}
	return nil
}

func allowedDrainTag(tag string) bool {
	if strings.EqualFold(tag, SourceTag) {
		return true
	}
	key, val, ok := strings.Cut(tag, ":")
	if !ok {
		return false
	}
	switch key {
	case "agent":
		return agentNameRe.MatchString(val)
	case "cli":
		return val == CLIClaude || val == CLICodex || val == CLIGemini
	case HashKey:
		return memhashRe.MatchString(val)
	case "repo":
		return repoTagRe.MatchString(val)
	}
	return false
}
