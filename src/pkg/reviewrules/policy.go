package reviewrules

import (
	"os"
	"regexp"
	"strings"
)

// Policy is what an operator tunes about rule capture without a code change
// (hivecommons/hive#19138): whether it runs, whose rules are trusted, extra
// phrases that mark a comment as a rule, and built-in signals to ignore. The
// zero value is the built-in behaviour with capture on.
type Policy struct {
	// Off turns capture off.
	Off bool
	// Trusted are the upper-case author associations that may state a rule
	// on their own authority. Nil keeps DefaultTrustedAssociations.
	Trusted map[string]bool
	// ExtraPhrases are matched case-insensitively on word boundaries.
	ExtraPhrases []string
	// DisabledSignals are built-in signal names that no longer count.
	DisabledSignals map[string]bool

	extraRe *regexp.Regexp
}

// extraRegexp is the compiled alternation of ExtraPhrases, nil when there are
// none. Resolve compiles it once; a Policy built by hand compiles on use.
// Phrases are quoted, so one can never act as a pattern.
func (p Policy) extraRegexp() *regexp.Regexp {
	if p.extraRe != nil {
		return p.extraRe
	}
	var parts []string
	for _, ph := range p.ExtraPhrases {
		ph = strings.Join(strings.Fields(ph), " ")
		if ph == "" {
			continue
		}
		parts = append(parts, strings.ReplaceAll(regexp.QuoteMeta(ph), " ", `\s+`))
	}
	if len(parts) == 0 {
		return nil
	}
	return regexp.MustCompile(`(?i)(?:^|[^\pL\pN_])(?:` + strings.Join(parts, "|") + `)(?:[^\pL\pN_]|$)`)
}

// Settings is the operator's preference set as hive.yaml gives it
// (review_feedback.rules.*). It is a plain struct so this package does not
// import pkg/config.
type Settings struct {
	Enabled             *bool
	TrustedAssociations []string
	ExtraPhrases        []string
	DisabledSignals     []string
}

// Resolved is the effective policy and where each part came from.
type Resolved struct {
	Policy Policy
	// EnabledFromEnv and TrustedFromEnv say the environment variable decided
	// the value, so the yaml key has no effect while it is set.
	EnabledFromEnv bool
	TrustedFromEnv bool
}

// Resolve merges hive.yaml with the environment. The environment variables
// keep working and win when they are set: HIVE_REVIEW_FEEDBACK_RULES set to a
// false-like value stops capture, and a non-empty
// HIVE_REVIEW_FEEDBACK_RULE_TRUSTED_ASSOCIATIONS is the trusted list. Without
// them, the yaml keys apply, then the defaults. Extra phrases and disabled
// signals have no environment variable.
func Resolve(set Settings) Resolved {
	var out Resolved
	out.Policy.Off = set.Enabled != nil && !*set.Enabled
	if v := strings.TrimSpace(os.Getenv(DisableEnvVar)); v != "" {
		switch strings.ToLower(v) {
		case "0", "false", "off", "no":
			out.Policy.Off = true
			out.EnabledFromEnv = true
		default:
			out.Policy.Off = false
			out.EnabledFromEnv = true
		}
	}
	switch list := strings.TrimSpace(os.Getenv(TrustedAssociationsEnvVar)); {
	case list != "":
		out.Policy.Trusted = ParseAssociations(list)
		out.TrustedFromEnv = true
	case len(set.TrustedAssociations) > 0:
		out.Policy.Trusted = ParseAssociations(strings.Join(set.TrustedAssociations, ","))
	default:
		out.Policy.Trusted = ParseAssociations(DefaultTrustedAssociations)
	}
	out.Policy.ExtraPhrases = append([]string(nil), set.ExtraPhrases...)
	if len(set.DisabledSignals) > 0 {
		out.Policy.DisabledSignals = map[string]bool{}
		for _, s := range set.DisabledSignals {
			out.Policy.DisabledSignals[strings.ToLower(strings.TrimSpace(s))] = true
		}
	}
	out.Policy.extraRe = out.Policy.extraRegexp()
	return out
}

// SignalNames lists the built-in signal names in table order.
func SignalNames() []string {
	names := make([]string, 0, len(ruleSignals))
	for _, s := range ruleSignals {
		names = append(names, s.name)
	}
	return names
}
