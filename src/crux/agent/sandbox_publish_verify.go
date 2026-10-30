package agent

import (
	"regexp"
	"strings"
)

// publishVerifyDirective matches an HTML-comment block of the form
//
//	<!-- hive:publish.verify
//	uv run pytest --collect-only -q
//	-->
//
// One shell command per line; blank lines and `#` comments are ignored.
//
// The block is recognised so the manager can say it saw one, but it is never
// executed: a rendered kick message interpolates issue and PR titles,
// knowledge facts and delegated prompts, none of which may choose the
// commands that run against the agent's tree before it is pushed. Verify
// commands are read from sandbox.publish.verify config only.
var publishVerifyDirective = regexp.MustCompile(`(?s)<!--\s*hive:publish\.verify\s*\n(.*?)-->`)

// parsePublishVerifyDirective extracts the commands a directive lists. ok is
// true when a directive is present, even if it lists no commands.
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
