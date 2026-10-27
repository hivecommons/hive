package agent

import (
	"regexp"
	"strings"
)

// Command normalization for repeat-failure detection (requirements C7, C9).
// Two failing commands count as "the same" only when their normalized forms
// are equal, so the normalizer removes noise the backend or the clock adds
// (wrapper text, whitespace, scratch paths, timestamps) and nothing else:
// `gh pr view 123` and `gh pr view 456` stay different commands.
//
// This file is independent of the detector: the detector takes the output of
// NormalizeRepeatFailureCommand as its command key and a prefix list for
// RepeatFailureCommandExcluded. It deliberately does not reuse
// fleetreport.NormalizeMessage, which blurs far more than is wanted here.

const (
	repeatFailureTmpPlaceholder  = "<tmp>"
	repeatFailureTimePlaceholder = "<ts>"
)

// defaultRepeatFailureExclusions are commands that fail by design while
// waiting: `gh pr checks` exits non-zero while checks are pending.
var defaultRepeatFailureExclusions = []string{"gh pr checks", "gh run watch"}

var (
	// Claude Code: source <snapshot> && eval '<cmd>' \< /dev/null && pwd -P >| <cwd-file>
	claudeWrapperRe = regexp.MustCompile(`(?s)^source\s+\S+\s+&&\s+(?:\S+=\S+\s+)*eval\s+'(.*)'(?:\s+\\?<\s*/dev/null)?(?:\s+&&\s+pwd\s+-P\s+>\|?\s*\S+)?\s*$`)
	// Gemini CLI: { <cmd>; }; __code=$?; pgrep -g 0 >/tmp/... 2>&1; exit $__code;
	// optionally preceded by a `shopt ...;` preamble.
	geminiWrapperRe = regexp.MustCompile(`(?s)^(?:shopt\s[^;]*;\s*)?\{\s*(.*?);?\s*\};\s*__code=\$\?;.*\bexit\s+\$__code;?\s*$`)

	tmpPathRe = regexp.MustCompile(`(^|[\s='"(:<>|&;,])(?:/var/tmp|/tmp|\$\{TMPDIR\}|\$TMPDIR)(?:/[^\s'"<>|&;)]*)?`)

	isoTimeRe  = regexp.MustCompile(`\b\d{4}-\d{2}-\d{2}T\d{2}:\d{2}(?::\d{2}(?:[.,]\d+)?)?(?:Z|[+-]\d{2}:?\d{2})?`)
	unixTimeRe = regexp.MustCompile(`\b(?:\d{13}|\d{10})\b`)
)

// NormalizeRepeatFailureCommand returns the comparison form of a command as
// recorded from a backend: backend wrapper text stripped, scratch paths under
// /tmp, /var/tmp and $TMPDIR replaced by "<tmp>", ISO-8601 and 10- or 13-digit
// Unix timestamps replaced by "<ts>", and whitespace collapsed. Everything
// else is kept verbatim.
func NormalizeRepeatFailureCommand(raw string) string {
	cmd := strings.TrimSpace(raw)
	if m := claudeWrapperRe.FindStringSubmatch(cmd); m != nil {
		cmd = strings.ReplaceAll(m[1], `'\''`, `'`)
	} else if m := geminiWrapperRe.FindStringSubmatch(cmd); m != nil {
		cmd = m[1]
	}
	cmd = tmpPathRe.ReplaceAllString(cmd, "${1}"+repeatFailureTmpPlaceholder)
	cmd = isoTimeRe.ReplaceAllString(cmd, repeatFailureTimePlaceholder)
	cmd = unixTimeRe.ReplaceAllString(cmd, repeatFailureTimePlaceholder)
	return strings.Join(strings.Fields(cmd), " ")
}

// DefaultRepeatFailureExclusions returns a copy of the default exclusion list
// (`gh pr checks`, `gh run watch`).
func DefaultRepeatFailureExclusions() []string {
	return append([]string(nil), defaultRepeatFailureExclusions...)
}

// RepeatFailureCommandExcluded reports whether an already-normalized command
// starts with any entry of prefixes. Entries are normalized the same way as
// commands (so whitespace differences do not matter); empty entries are
// ignored so a blank config line cannot exclude everything.
func RepeatFailureCommandExcluded(normalized string, prefixes []string) bool {
	for _, p := range prefixes {
		p = NormalizeRepeatFailureCommand(p)
		if p != "" && strings.HasPrefix(normalized, p) {
			return true
		}
	}
	return false
}
