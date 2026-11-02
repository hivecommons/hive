package config

import (
	"regexp"
	"strings"
)

// ModelFlag renders the ` --model <model>` argument pair for a CLI launch
// command, or "" when model is empty or whitespace so the backend's own
// default applies.
//
// Every launch builder must go through this (or ModelArgs) rather than
// formatting `--model %s` directly. Observed live on a production spoke
// (Claude Code 2.1.316): an agent whose configured model was "" launched as
//
//	claude --model --dangerously-skip-permissions --bare ...
//
// so `--model` consumed `--dangerously-skip-permissions` as its VALUE. Claude
// then ran in permission-prompting mode and blocked on a "Read outside the
// working directories?" dialog that no human was attached to answer, and
// every kick failed with "CLI did not reach input prompt" for 40+ minutes.
func ModelFlag(model string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	return " --model " + model
}

// ModelArgs is ModelFlag for argv-style builders: {"--model", model} or nil
// when model is empty or whitespace.
func ModelArgs(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" {
		return nil
	}
	return []string{"--model", model}
}

// DropDanglingModelFlag is the launch-time belt-and-braces check behind
// ModelFlag: if the rendered command contains a `--model` whose value slot is
// another `--flag` (or nothing at all), that `--model` is removed and the
// cleaned command returned with dropped=true. A well-formed command is
// returned verbatim with dropped=false.
//
// A model id never starts with "--" and a quoted value such as
// `--model 'x y'` keeps its first character free of the "--" prefix, so a
// `--model` followed by "--" is always the dangling case. The rest of the
// command — quoting, spacing — is left byte-for-byte intact.
func DropDanglingModelFlag(cmd string) (cleaned string, dropped bool) {
	cleaned = cmd
	for {
		next := danglingModelFlagRe.ReplaceAllString(cleaned, "${1}${2}")
		if next == cleaned {
			if dropped {
				cleaned = strings.TrimRight(cleaned, " \t")
			}
			return cleaned, dropped
		}
		cleaned, dropped = next, true
	}
}

// danglingModelFlagRe matches a `--model` token whose value is another
// `--flag` or the end of the command. Group 1 is the whitespace (or start)
// before it, group 2 the "--" of the flag that followed, both preserved.
var danglingModelFlagRe = regexp.MustCompile(`(^|\s)--model(?:\s+(--)|\s*$)`)
