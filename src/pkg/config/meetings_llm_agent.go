package config

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Meetings agent-backend mode (hivecommons/hive#18517): the meeting persona
// and Spektacular live mode answer through one of the agent backends the
// hive's agents are configured with, in that backend's narrow completion
// profile (single turn, no tools, no MCP, no writes, a fixed system prompt,
// usage taken from the CLI's result). Only backends whose CLI supports such
// a profile at a meeting-friendly latency are accepted; the rest are refused
// at validation with MeetingsLLMBackendRefusal.
const (
	// MeetingsCLIBackendClaude is the Claude Code CLI backend.
	MeetingsCLIBackendClaude = "claude"
	// MeetingsOpenAIBackendLiteLLM is the LiteLLM backend, answered through
	// the existing OpenAI-compatible path (governor.litellm).
	MeetingsOpenAIBackendLiteLLM = "litellm"
	// DefaultMeetingsClaudeCLIModel is the model the claude backend answers
	// with when meetings.llm.model is unset: fast enough for a warm turn
	// inside the persona's addressed timeout.
	DefaultMeetingsClaudeCLIModel = "claude-sonnet-5-5"
	// DefaultMeetingsCLIHome is the HOME the completion CLI runs with: the
	// shared agent home that holds the fleet's CLI sign-in. Used only when
	// it exists; otherwise the CLI inherits the hive's own HOME.
	DefaultMeetingsCLIHome = "/data/home"
	// DefaultMeetingsCLIAddressedTimeoutMS, DefaultMeetingsCLIUnpromptedTimeoutMS
	// and DefaultMeetingsCLIRestyleTimeoutMS bound the persona's calls on a
	// CLI backend. A warm Claude CLI turn takes about one to three seconds,
	// so these sit above the API modes' 4 s, 2.5 s and 800 ms.
	DefaultMeetingsCLIAddressedTimeoutMS  = 8000
	DefaultMeetingsCLIUnpromptedTimeoutMS = 6000
	DefaultMeetingsCLIRestyleTimeoutMS    = 3000
	// DefaultMeetingsCLIOneShotTimeoutMS bounds one cold `claude -p` call
	// (Spektacular's `hive meeting-llm`, one process per prompt).
	DefaultMeetingsCLIOneShotTimeoutMS = 30000
	// DefaultMeetingsCLIRecycleTurns is how many turns a warm process
	// answers before it is replaced, which bounds the history every turn
	// re-reads (and pays for).
	DefaultMeetingsCLIRecycleTurns = 8
)

// Meetings CLI env overrides.
const (
	MeetingsCLIHomeEnv                = "HIVE_MEETINGS_CLI_HOME"
	MeetingsCLIBinaryEnv              = "HIVE_MEETINGS_CLI_BINARY"
	MeetingsCLIAddressedTimeoutEnv    = "HIVE_MEETINGS_CLI_ADDRESSED_TIMEOUT_MS"
	MeetingsCLIUnpromptedTimeoutEnv   = "HIVE_MEETINGS_CLI_UNPROMPTED_TIMEOUT_MS"
	MeetingsCLIRestyleTimeoutEnv      = "HIVE_MEETINGS_CLI_RESTYLE_TIMEOUT_MS"
	MeetingsCLIOneShotTimeoutEnv      = "HIVE_MEETINGS_CLI_ONESHOT_TIMEOUT_MS"
	MeetingsCLIRecycleTurnsEnv        = "HIVE_MEETINGS_CLI_RECYCLE_TURNS"
	meetingsCLIDefaultBinaryForClaude = "claude"
)

// MeetingsCompletionBackends are the CLI backends with a meetings
// completion profile. MeetingsOpenAIBackends answer through the
// OpenAI-compatible path.
var (
	MeetingsCompletionBackends = []string{MeetingsCLIBackendClaude}
	MeetingsOpenAIBackends     = []string{MeetingsOpenAIBackendLiteLLM}
)

// ResolvedMeetingsCLI is how the completion CLI is launched.
type ResolvedMeetingsCLI struct {
	// Binary is the CLI executable (a name looked up in PATH, or a path).
	Binary string
	// Home is the HOME the CLI runs with; empty inherits the hive's.
	Home                string
	AddressedTimeoutMS  int
	UnpromptedTimeoutMS int
	RestyleTimeoutMS    int
	OneShotTimeoutMS    int
	RecycleTurns        int
}

// MeetingsLLMBackendSupported reports whether backend can answer for
// meetings in the agent-backend mode.
func MeetingsLLMBackendSupported(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return meetingsContains(MeetingsCompletionBackends, b) || meetingsContains(MeetingsOpenAIBackends, b)
}

// MeetingsLLMSupportedBackends lists the accepted backends, sorted.
func MeetingsLLMSupportedBackends() []string {
	out := append(append([]string{}, MeetingsCompletionBackends...), MeetingsOpenAIBackends...)
	sort.Strings(out)
	return out
}

// MeetingsLLMBackendRefusal explains why backend cannot answer for meetings.
func MeetingsLLMBackendRefusal(backend string) string {
	return fmt.Sprintf("meetings.llm.backend %q has no meetings completion profile yet: hive only answers meetings through backends whose CLI runs a fast single-turn completion with no tools, no MCP servers and no writes (supported: %s); pick one of those, or use meetings.llm.mode openai or anthropic",
		backend, strings.Join(MeetingsLLMSupportedBackends(), ", "))
}

func meetingsContains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// validateMeetingsAgentBackend checks the backend named for the
// agent-backend and claude-cli modes (env included). An empty backend is
// resolved from the hive's agents at boot.
func validateMeetingsAgentBackend(mode, backend string) error {
	b := strings.ToLower(meetingsEnvString(MeetingsLLMBackendEnv, backend, ""))
	if mode == MeetingsLLMModeClaudeCLI {
		if b != "" && b != MeetingsCLIBackendClaude {
			return fmt.Errorf("meetings.llm.mode claude-cli always uses the claude backend; set meetings.llm.mode to agent-backend to use %q", b)
		}
		return nil
	}
	if b != "" && !MeetingsLLMBackendSupported(b) {
		return fmt.Errorf("%s", MeetingsLLMBackendRefusal(b))
	}
	return nil
}

// MeetingsAgentDefault is the backend most of the hive's agents run on
// (ties broken by name, so the choice is stable across boots) and the model
// most of those agents use. Empty when no agent names a backend.
func (c *Config) MeetingsAgentDefault() (backend, model string) {
	if c == nil {
		return "", ""
	}
	backends := map[string]int{}
	for _, a := range c.Agents {
		if b := strings.ToLower(strings.TrimSpace(a.Backend)); b != "" {
			backends[b]++
		}
	}
	backend = meetingsMostCommon(backends)
	if backend == "" {
		return "", ""
	}
	models := map[string]int{}
	for _, a := range c.Agents {
		if strings.EqualFold(strings.TrimSpace(a.Backend), backend) {
			if m := strings.TrimSpace(a.Model); m != "" {
				models[m]++
			}
		}
	}
	return backend, meetingsMostCommon(models)
}

func meetingsMostCommon(counts map[string]int) string {
	best, bestN := "", 0
	for k, n := range counts {
		if n > bestN || (n == bestN && k < best) {
			best, bestN = k, n
		}
	}
	return best
}

// MeetingsDefaultModelFor is the model the meetings backend answers with
// when meetings.llm.model is unset: Sonnet 5.5 on claude, otherwise the
// model the hive's agents on that backend use.
func (c *Config) MeetingsDefaultModelFor(backend string) string {
	if strings.EqualFold(backend, MeetingsCLIBackendClaude) {
		return DefaultMeetingsClaudeCLIModel
	}
	if c == nil {
		return ""
	}
	counts := map[string]int{}
	for _, a := range c.Agents {
		if strings.EqualFold(strings.TrimSpace(a.Backend), backend) {
			if m := strings.TrimSpace(a.Model); m != "" {
				counts[m]++
			}
		}
	}
	return meetingsMostCommon(counts)
}

// resolveMeetingsAgentBackend resolves the agent-backend and claude-cli
// modes.
func (c *Config) resolveMeetingsAgentBackend(ctx context.Context, mode string, r ResolvedMeetingsLLM) ResolvedMeetingsLLM {
	none := ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone}
	l := c.Meetings.LLM
	backend := MeetingsCLIBackendClaude
	if mode == MeetingsLLMModeAgentBackend {
		backend = strings.ToLower(meetingsEnvString(MeetingsLLMBackendEnv, l.Backend, ""))
		if backend == "" {
			backend, _ = c.MeetingsAgentDefault()
		}
	}
	switch {
	case backend == "":
		none.Reason = "rules only, no agent backend configured (set meetings.llm.backend)"
		return none
	case meetingsContains(MeetingsOpenAIBackends, backend):
		def := c.MeetingsDefaultModelFor(backend)
		if def == "" {
			def = strings.TrimSpace(c.Governor.LiteLLM.DefaultModel)
		}
		return c.resolveMeetingsOpenAI(ctx, r, def)
	case backend == MeetingsCLIBackendClaude:
		r.Backend, r.AgentBackend = MeetingsLLMModeAgentBackend, backend
		r.Model = meetingsEnvString(MeetingsLLMModelEnv, l.Model, DefaultMeetingsClaudeCLIModel)
		r.CLI = resolveMeetingsCLI(meetingsCLIDefaultBinaryForClaude)
		return r
	default:
		none.Reason = "rules only, backend " + backend + " has no meetings completion profile yet"
		return none
	}
}

// resolveMeetingsCLI resolves the CLI launch settings.
func resolveMeetingsCLI(defaultBinary string) ResolvedMeetingsCLI {
	home := meetingsEnvString(MeetingsCLIHomeEnv, "", "")
	if home == "" {
		if st, err := os.Stat(DefaultMeetingsCLIHome); err == nil && st.IsDir() {
			home = DefaultMeetingsCLIHome
		}
	}
	return ResolvedMeetingsCLI{
		Binary:              meetingsEnvString(MeetingsCLIBinaryEnv, "", defaultBinary),
		Home:                home,
		AddressedTimeoutMS:  meetingsEnvInt(MeetingsCLIAddressedTimeoutEnv, 0, DefaultMeetingsCLIAddressedTimeoutMS),
		UnpromptedTimeoutMS: meetingsEnvInt(MeetingsCLIUnpromptedTimeoutEnv, 0, DefaultMeetingsCLIUnpromptedTimeoutMS),
		RestyleTimeoutMS:    meetingsEnvInt(MeetingsCLIRestyleTimeoutEnv, 0, DefaultMeetingsCLIRestyleTimeoutMS),
		OneShotTimeoutMS:    meetingsEnvInt(MeetingsCLIOneShotTimeoutEnv, 0, DefaultMeetingsCLIOneShotTimeoutMS),
		RecycleTurns:        meetingsEnvInt(MeetingsCLIRecycleTurnsEnv, 0, DefaultMeetingsCLIRecycleTurns),
	}
}

// MeetingsLLMModes are the meetings.llm.mode values the Settings picker
// offers (claude-cli is accepted too, as agent-backend with claude).
var MeetingsLLMModes = []string{MeetingsLLMModeAuto, MeetingsLLMModeOpenAI, MeetingsLLMModeAnthropic, MeetingsLLMModeAgentBackend, MeetingsLLMModeNone}

// ValidateMeetingsLLMChoice checks a mode and backend picked in Settings,
// without the environment.
func ValidateMeetingsLLMChoice(mode, backend string) error {
	mode = strings.ToLower(strings.TrimSpace(mode))
	backend = strings.ToLower(strings.TrimSpace(backend))
	if mode == "" {
		mode = MeetingsLLMModeAuto
	}
	if !meetingsContains(MeetingsLLMModes, mode) && mode != MeetingsLLMModeClaudeCLI {
		return fmt.Errorf("meetings.llm.mode %q is not one of %s or %s", mode, strings.Join(MeetingsLLMModes, ", "), MeetingsLLMModeClaudeCLI)
	}
	if mode == MeetingsLLMModeClaudeCLI && backend != "" && backend != MeetingsCLIBackendClaude {
		return fmt.Errorf("meetings.llm.mode claude-cli always uses the claude backend; set meetings.llm.mode to agent-backend to use %q", backend)
	}
	if mode == MeetingsLLMModeAgentBackend && backend != "" && !MeetingsLLMBackendSupported(backend) {
		return fmt.Errorf("%s", MeetingsLLMBackendRefusal(backend))
	}
	return nil
}
