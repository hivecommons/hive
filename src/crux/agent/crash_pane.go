package agent

import "strings"

// crashPaneTailLines bounds the pane excerpt attached to a bare-shell crash
// log line: enough for the CLI's exit message plus the shell prompt that
// replaced it, small enough to stay one log record.
const crashPaneTailLines = 8

// crashPaneTail is the last pane lines attached to a crash log line, with any
// launch credential scrubbed. The pane holds the typed launch command, and
// for Claude that command carries the task MCP launch token in its URL query
// (taskmcp.TokenQueryParam); a log line must not republish it.
func crashPaneTail(pane string) string {
	return redactTokenQueryValues(paneTail(pane, crashPaneTailLines))
}

// crashExitCode reads the CLI's exit status back out of the pane: the value
// bash printed after cliExitMarker when the process returned (withCLIExitEcho).
// The typed launch line also contains the marker, followed by the literal
// `$?`, so only a marker followed by digits counts and the LAST one wins —
// the pane may hold several launches' worth of scrollback. "unknown" when the
// pane shows no status: the launch predates the echo, the operator's
// LaunchCmd swallowed it, or the shell itself is gone.
func crashExitCode(pane string) string {
	rest := pane
	code := "unknown"
	for {
		i := strings.Index(rest, cliExitMarker)
		if i < 0 {
			return code
		}
		rest = rest[i+len(cliExitMarker):]
		// Every marker resets the answer: the latest one is the truth. If
		// it is a typed launch line (literal `$?`), a status printed by an
		// OLDER launch above it must not be reported as this launch's.
		code = "unknown"
		end := 0
		for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
			end++
		}
		if end > 0 {
			code = rest[:end]
		}
	}
}

// redactTokenQueryValues replaces the value of every `token=` URL query
// parameter in s with "<redacted>". The value runs to the next `&`, quote,
// closing brace or whitespace — the characters that can follow a query value
// in a typed launch line.
func redactTokenQueryValues(s string) string {
	const key = "token="
	var b strings.Builder
	for {
		i := strings.Index(s, key)
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		i += len(key)
		b.WriteString(s[:i])
		b.WriteString("<redacted>")
		s = s[i:]
		end := strings.IndexAny(s, "&'\"}` \t\r\n")
		if end < 0 {
			return b.String()
		}
		s = s[end:]
	}
}
