package config

import "strings"

// AgyDefaultEffort is the reasoning effort passed alongside agy's --model
// when the agent has no usable reasoning_effort configured (see
// AgyLaunchEffort). agy requires --effort whenever --model is given and
// otherwise ignores the model entirely; "low" is the effort agy defaults to
// on its own, so this makes the configured model take effect without
// changing behaviour.
const AgyDefaultEffort = "low"

// AgyLaunchEffort returns the --effort agy is launched with: the configured
// reasoning effort when it is one agy accepts (low/medium/high), else the
// effort encoded in agy's model id suffix, else AgyDefaultEffort. The
// rejection mirrors the contributor relay's rule — agy refuses efforts from
// codex's wider vocabulary (e.g. xhigh), and launching with one would make
// agy ignore the model outright, the exact failure --effort exists to prevent.
func AgyLaunchEffort(model, effort string) string {
	for _, v := range ReasoningEffortsByBackend["agy"] {
		if v == effort {
			return effort
		}
	}
	for _, v := range ReasoningEffortsByBackend["agy"] {
		if strings.HasSuffix(model, "-"+v) {
			return v
		}
	}
	return AgyDefaultEffort
}

// ResolveReasoningEffort reports the reasoning effort the runtime actually
// launches a given backend/model pair with, given the agent's configured
// reasoning_effort. The attribution trail is resolved in TWO places —
// Manager.InvocationMetadata for a running agent, and cmd/hive's fallback
// that reads straight from config when the runtime does not know the agent —
// and both must give the same answer. It is pure, so it lives in the
// configuration leaf rather than behind the runtime seam
// (hivecommons/hive#14138, stage 1).
//
// Before this existed the fallback carried its own hardcoded "low", so
// changing AgyDefaultEffort would have left cmd/hive silently stamping PRs
// with an effort agy was no longer being launched with. An attribution trail
// that misreports is worse than one that says nothing.
//
// The rules mirror the launch path exactly:
//   - agy REQUIRES --effort whenever --model is given, so with a model it runs
//     at AgyLaunchEffort(model, configured) and with no model at no effort at all.
//   - codex is launched with `-c model_reasoning_effort` only when an effort
//     is configured; unset means codex's own default, which the hive does not
//     resolve, so the honest answer is the configured value verbatim.
//   - claude is launched with `--effort` only when the configured effort is
//     one Claude Code accepts (the launch path drops anything else), so the
//     answer is the configured value when valid and "" otherwise — never a
//     value the CLI was not actually given (hivecommons/hive#8377).
//   - omp is launched with `--thinking` only when an effort is configured;
//     unset means omp's per-model default, so report the configured value.
//   - every other backend takes its effort from its own config, which the
//     hive does not resolve here, so the honest answer is "".
func ResolveReasoningEffort(backend, model, configured string) string {
	switch backend {
	case "agy":
		if model != "" {
			return AgyLaunchEffort(model, configured)
		}
		return ""
	case "codex", "omp":
		return configured
	case ClaudeBackend:
		if ValidEffort(ClaudeBackend, configured) {
			return configured
		}
		return ""
	}
	return ""
}
