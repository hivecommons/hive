package agent

import (
	"regexp"
	"strings"
)

// publishVerifyDirective matches an HTML-comment block a kick template uses
// to declare its publish preflight inline:
//
//	<!-- hive:publish.verify
//	uv run pytest --collect-only -q
//	sh -c '[ -z "$HIVE_CHANGED_TESTS" ] || uv run pytest $HIVE_CHANGED_TESTS -q'
//	-->
//
// One shell command per line; blank lines and `#` comments are ignored. The
// comment is invisible to the agent-facing markdown yet travels with the
// policy, so the template that tells the agent "run on card first" is the
// same artifact that tells hive how to check it did.
var publishVerifyDirective = regexp.MustCompile(`(?s)<!--\s*hive:publish\.verify\s*\n(.*?)-->`)

// parsePublishVerifyDirective extracts the verify commands from a rendered
// kick message. ok is true when a directive is present, even if it lists no
// commands (an explicit empty block turns the configured preflight off for
// that kick).
func parsePublishVerifyDirective(message string) (cmds []string, ok bool) {
	m := publishVerifyDirective.FindStringSubmatch(message)
	if m == nil {
		return nil, false
	}
	for _, line := range strings.Split(m[1], "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		cmds = append(cmds, line)
	}
	return cmds, true
}
