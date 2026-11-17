package config

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
)

// Meetings agent-backend mode (hivecommons/hive#18517, #18549): the meeting
// persona and Spektacular live mode answer through one of the agent backends
// the hive's agents are configured with. A CLI backend answers in its narrow
// completion profile (single turn, no tools, no MCP, no writes, a fixed
// system prompt, usage taken from the CLI's result); a model-gateway backend
// (LiteLLM, vllm, llm-d or a named gateway) answers through the
// OpenAI-compatible path. Every other backend the agents' picker offers is
// refused at validation and in the picker with the reason in
// meetingsRefusedBackends.
const (
	// MeetingsCLIBackendClaude is the Claude Code CLI backend.
	MeetingsCLIBackendClaude = "claude"
	// MeetingsCLIBackendCopilot is the GitHub Copilot CLI backend.
	MeetingsCLIBackendCopilot = "copilot"
	// MeetingsCLIBackendOMP is the Oh My Pi CLI backend.
	MeetingsCLIBackendOMP = "omp"
	// MeetingsOpenAIBackendLiteLLM is the LiteLLM backend, answered through
	// the existing OpenAI-compatible path (governor.litellm).
	MeetingsOpenAIBackendLiteLLM = "litellm"
	// MeetingsOpenAIBackendVLLM and MeetingsOpenAIBackendLLMD are the
	// self-hosted inference backends, answered through the OpenAI-compatible
	// path at the endpoints the agents use (HIVE_VLLM_ENDPOINT,
	// HIVE_LLMD_ENDPOINT).
	MeetingsOpenAIBackendVLLM = "vllm"
	MeetingsOpenAIBackendLLMD = "llm-d"
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
	// DefaultMeetingsColdCLIAddressedTimeoutMS,
	// DefaultMeetingsColdCLIUnpromptedTimeoutMS and
	// DefaultMeetingsColdCLIRestyleTimeoutMS are the same bounds on a CLI
	// backend with no warm mode (copilot, omp): every call starts the CLI,
	// which costs about four to five seconds before the model answers.
	DefaultMeetingsColdCLIAddressedTimeoutMS  = 20000
	DefaultMeetingsColdCLIUnpromptedTimeoutMS = 15000
	DefaultMeetingsColdCLIRestyleTimeoutMS    = 10000
	// DefaultMeetingsCLIOneShotTimeoutMS bounds one cold CLI call
	// (Spektacular's `hive meeting-llm`, one process per prompt).
	DefaultMeetingsCLIOneShotTimeoutMS = 30000
	// DefaultMeetingsCLIRecycleTurns is how many turns a warm process
	// answers before it is replaced, which bounds the history every turn
	// re-reads (and pays for).
	DefaultMeetingsCLIRecycleTurns = 8
	// DefaultLLMDEndpoint is the in-cluster llm-d endpoint picker Service
	// the agents use when HIVE_LLMD_ENDPOINT is unset.
	DefaultLLMDEndpoint = "http://hive-llm-d-epp.hive-inference.svc.cluster.local:8000"
)

// Meetings CLI env overrides.
const (
	MeetingsCLIHomeEnv              = "HIVE_MEETINGS_CLI_HOME"
	MeetingsCLIBinaryEnv            = "HIVE_MEETINGS_CLI_BINARY"
	MeetingsCLIAddressedTimeoutEnv  = "HIVE_MEETINGS_CLI_ADDRESSED_TIMEOUT_MS"
	MeetingsCLIUnpromptedTimeoutEnv = "HIVE_MEETINGS_CLI_UNPROMPTED_TIMEOUT_MS"
	MeetingsCLIRestyleTimeoutEnv    = "HIVE_MEETINGS_CLI_RESTYLE_TIMEOUT_MS"
	MeetingsCLIOneShotTimeoutEnv    = "HIVE_MEETINGS_CLI_ONESHOT_TIMEOUT_MS"
	MeetingsCLIRecycleTurnsEnv      = "HIVE_MEETINGS_CLI_RECYCLE_TURNS"
	// VLLMEndpointEnv and LLMDEndpointEnv are the agents' inference
	// endpoints (comma-separated lists; meetings use the first).
	VLLMEndpointEnv = "HIVE_VLLM_ENDPOINT"
	LLMDEndpointEnv = "HIVE_LLMD_ENDPOINT"
)

// MeetingsCompletionBackends are the CLI backends with a meetings
// completion profile; MeetingsWarmCLIBackends are those that also keep one
// warm process per meeting. MeetingsOpenAIBackends answer through the
// OpenAI-compatible path, as does every named model gateway
// (Config.MeetingsGatewayBackends).
var (
	MeetingsCompletionBackends = []string{MeetingsCLIBackendClaude, MeetingsCLIBackendCopilot, MeetingsCLIBackendOMP}
	MeetingsWarmCLIBackends    = []string{MeetingsCLIBackendClaude}
	MeetingsOpenAIBackends     = []string{MeetingsOpenAIBackendLiteLLM, MeetingsOpenAIBackendLLMD, MeetingsOpenAIBackendVLLM}
)

// meetingsNoProfileReason is the refusal of an agentic CLI hive has not
// fitted with a completion profile.
const meetingsNoProfileReason = "hive has no verified launch of this CLI that answers one prompt with every tool, MCP server and file write turned off"

// meetingsRefusedBackends are the agent backends the agents' picker offers
// that cannot answer for meetings, each with the reason. Tested against
// the agents' backend list so a new backend gets a decision here.
var meetingsRefusedBackends = map[string]string{
	"codex": "codex has no flag that removes its shell tool: even with a read-only sandbox, approval policy never and an empty workspace, " +
		"`codex exec` lets the model run read-only commands, which can read the CLI's sign-in and other files on the hive and repeat them into a meeting",
	"gemini": "the gemini CLI is not installed in the hive image (src/Dockerfile pins no version), has no flag that removes every tool " +
		"(its non-interactive mode still runs read-only shell and file tools), and stopped serving personal and Google AI Pro accounts on 2026-06-18",
	"agy":       "agy has no flag that turns its tools off (the agent launcher cannot pass it tool rules either)",
	"bob":       "bob has no flag that turns its tools off (the agent launcher cannot pass it tool rules either)",
	"goose":     meetingsNoProfileReason,
	"aider":     meetingsNoProfileReason,
	"cline":     meetingsNoProfileReason,
	"pi":        meetingsNoProfileReason,
	"opencode":  meetingsNoProfileReason,
	"kilo":      meetingsNoProfileReason,
	"muse":      meetingsNoProfileReason,
	"openhands": meetingsNoProfileReason,
	"omnigent":  meetingsNoProfileReason,
	"watsonx": "watsonx needs a short-lived IAM bearer minted from the IBM Cloud key and a project header, which the meetings " +
		"OpenAI-compatible path does not send",
}

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
	// Warm reports whether the backend keeps one warm process per meeting;
	// otherwise every call starts the CLI.
	Warm bool
}

// MeetingsLLMBackendSupported reports whether backend can answer for
// meetings in the agent-backend mode on any hive (named gateways depend on
// the config: see Config.MeetingsLLMBackendRefusalFor).
func MeetingsLLMBackendSupported(backend string) bool {
	b := strings.ToLower(strings.TrimSpace(backend))
	return meetingsContains(MeetingsCompletionBackends, b) || meetingsContains(MeetingsOpenAIBackends, b)
}

// MeetingsLLMSupportedBackends lists the backends accepted on every hive,
// sorted.
func MeetingsLLMSupportedBackends() []string {
	out := append(append([]string{}, MeetingsCompletionBackends...), MeetingsOpenAIBackends...)
	sort.Strings(out)
	return out
}

// MeetingsLLMBackendRefusal explains why backend cannot answer for meetings.
func MeetingsLLMBackendRefusal(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	if why, ok := meetingsRefusedBackends[b]; ok {
		return meetingsRefusal(backend, why)
	}
	return fmt.Sprintf("meetings.llm.backend %q is neither an agent backend with a meetings completion profile nor a configured model gateway (supported: %s, or a gateway under Settings, Model Gateways); pick one of those, or use meetings.llm.mode openai or anthropic",
		backend, strings.Join(MeetingsLLMSupportedBackends(), ", "))
}

func meetingsRefusal(backend, why string) string {
	return fmt.Sprintf("meetings.llm.backend %q cannot answer for meetings: %s; pick one of %s or a model gateway, or use meetings.llm.mode openai or anthropic",
		backend, why, strings.Join(MeetingsLLMSupportedBackends(), ", "))
}

// MeetingsRefusedBackendReasons is every known agent backend that cannot
// answer for meetings, with the reason (for the Settings picker).
func MeetingsRefusedBackendReasons() map[string]string {
	out := make(map[string]string, len(meetingsRefusedBackends))
	for k, v := range meetingsRefusedBackends {
		out[k] = v
	}
	return out
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
// agent-backend and claude-cli modes (env included) without the rest of the
// config: a known refused backend fails here, and a name that may be a
// model gateway is checked by Config.validateMeetingsGatewayBackend. An
// empty backend is resolved from the hive's agents at boot.
func validateMeetingsAgentBackend(mode, backend string) error {
	b := strings.ToLower(meetingsEnvString(MeetingsLLMBackendEnv, backend, ""))
	if mode == MeetingsLLMModeClaudeCLI {
		if b != "" && b != MeetingsCLIBackendClaude {
			return fmt.Errorf("meetings.llm.mode claude-cli always uses the claude backend; set meetings.llm.mode to agent-backend to use %q", b)
		}
		return nil
	}
	if _, refused := meetingsRefusedBackends[b]; refused {
		return fmt.Errorf("%s", MeetingsLLMBackendRefusal(b))
	}
	return nil
}

// validateMeetingsGatewayBackend checks, with the gateways in view, that a
// meetings.llm.backend which is not a built-in backend names a usable model
// gateway.
func (c *Config) validateMeetingsGatewayBackend() error {
	if !c.Meetings.IsEnabled() {
		return nil
	}
	mode := strings.ToLower(meetingsEnvString(MeetingsLLMModeEnv, c.Meetings.LLM.Mode, MeetingsLLMModeAuto))
	if mode != MeetingsLLMModeAgentBackend {
		return nil
	}
	b := strings.ToLower(meetingsEnvString(MeetingsLLMBackendEnv, c.Meetings.LLM.Backend, ""))
	if why := c.MeetingsLLMBackendRefusalFor(b); why != "" {
		return fmt.Errorf("%s", why)
	}
	return nil
}

// MeetingsGatewayBackends are the named model gateways (Settings, Model
// Gateways) that can answer for meetings, sorted: every explicitly
// configured gateway with an endpoint except watsonx ones, under names that
// are not built-in backends.
func (c *Config) MeetingsGatewayBackends() []string {
	if c == nil {
		return nil
	}
	var out []string
	for _, gw := range c.Governor.Gateways {
		name := strings.ToLower(strings.TrimSpace(gw.Name))
		if name == "" || strings.TrimSpace(gw.Endpoint) == "" || strings.EqualFold(gw.Kind, GatewayKindWatsonx) ||
			MeetingsLLMBackendSupported(name) || meetingsRefusedBackends[name] != "" || meetingsContains(out, name) {
			continue
		}
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// MeetingsLLMSupportedBackendsFor is MeetingsLLMSupportedBackends plus the
// hive's named model gateways, sorted.
func (c *Config) MeetingsLLMSupportedBackendsFor() []string {
	out := append(MeetingsLLMSupportedBackends(), c.MeetingsGatewayBackends()...)
	sort.Strings(out)
	return out
}

// MeetingsLLMBackendRefusalFor is why backend cannot answer for meetings on
// this hive, or "" when it can (an empty backend follows the agents).
func (c *Config) MeetingsLLMBackendRefusalFor(backend string) string {
	b := strings.ToLower(strings.TrimSpace(backend))
	if b == "" || MeetingsLLMBackendSupported(b) || meetingsContains(c.MeetingsGatewayBackends(), b) {
		return ""
	}
	if c != nil {
		for _, gw := range c.Governor.Gateways {
			if !strings.EqualFold(strings.TrimSpace(gw.Name), b) {
				continue
			}
			if strings.EqualFold(gw.Kind, GatewayKindWatsonx) {
				return meetingsRefusal(backend, "it is a watsonx gateway: "+meetingsRefusedBackends[GatewayKindWatsonx])
			}
			if strings.TrimSpace(gw.Endpoint) == "" {
				return fmt.Sprintf("meetings.llm.backend %q is a model gateway with no endpoint; set its endpoint under Settings, Model Gateways", backend)
			}
		}
	}
	return MeetingsLLMBackendRefusal(b)
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
	case backend == MeetingsOpenAIBackendLiteLLM:
		def := c.MeetingsDefaultModelFor(backend)
		if def == "" {
			def = strings.TrimSpace(c.Governor.LiteLLM.DefaultModel)
		}
		return c.resolveMeetingsOpenAI(ctx, r, def)
	case backend == MeetingsOpenAIBackendVLLM || backend == MeetingsOpenAIBackendLLMD:
		return c.resolveMeetingsInference(r, backend)
	case meetingsContains(c.MeetingsGatewayBackends(), backend):
		return c.resolveMeetingsGateway(r, backend)
	case meetingsContains(MeetingsCompletionBackends, backend):
		r.Backend, r.AgentBackend = MeetingsLLMModeAgentBackend, backend
		r.Model = meetingsEnvString(MeetingsLLMModelEnv, l.Model, c.MeetingsDefaultModelFor(backend))
		if r.Model == "" && backend == MeetingsCLIBackendOMP {
			// omp runs in an isolated profile with no default model.
			none.Reason = "rules only, backend omp needs a model (set meetings.llm.model or give the omp agents one)"
			return none
		}
		r.CLI = resolveMeetingsCLI(backend)
		return r
	default:
		none.Reason = "rules only, backend " + backend + " cannot answer for meetings"
		return none
	}
}

// resolveMeetingsInference resolves vllm and llm-d onto the
// OpenAI-compatible path at the first endpoint the agents use. These
// endpoints are unauthenticated, like the agents' route to them.
func (c *Config) resolveMeetingsInference(r ResolvedMeetingsLLM, backend string) ResolvedMeetingsLLM {
	raw := strings.TrimSpace(os.Getenv(VLLMEndpointEnv))
	if backend == MeetingsOpenAIBackendLLMD {
		raw = meetingsEnvString(LLMDEndpointEnv, "", DefaultLLMDEndpoint)
	}
	endpoint, _, _ := strings.Cut(raw, ",")
	return c.meetingsEndpointBackend(r, backend, strings.TrimSpace(endpoint), "", "", "")
}

// resolveMeetingsGateway resolves a named model gateway onto the
// OpenAI-compatible path with the gateway's endpoint, key and CA bundle.
func (c *Config) resolveMeetingsGateway(r ResolvedMeetingsLLM, name string) ResolvedMeetingsLLM {
	gw := c.Governor.ResolveGateway(name)
	if gw == nil {
		return ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone, Reason: "rules only, model gateway " + name + " is not configured"}
	}
	return c.meetingsEndpointBackend(r, name, gw.Endpoint, gw.ResolveAPIKey(), gw.DefaultModel, gw.CABundle)
}

// meetingsEndpointBackend fills an OpenAI-compatible model for an agent
// backend: the model is meetings.llm.model, else the one the hive's agents
// on that backend use, else fallbackModel.
func (c *Config) meetingsEndpointBackend(r ResolvedMeetingsLLM, backend, endpoint, key, fallbackModel, caBundle string) ResolvedMeetingsLLM {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	model := meetingsEnvString(MeetingsLLMModelEnv, c.Meetings.LLM.Model, c.MeetingsDefaultModelFor(backend))
	if model == "" {
		model = strings.TrimSpace(fallbackModel)
	}
	switch {
	case endpoint == "":
		return ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone, Reason: "rules only, backend " + backend + " has no endpoint configured"}
	case model == "":
		return ResolvedMeetingsLLM{Backend: MeetingsLLMModeNone, Reason: "rules only, backend " + backend + " needs a model (set meetings.llm.model)"}
	}
	r.Backend, r.AgentBackend = MeetingsLLMModeOpenAI, backend
	r.Endpoint, r.APIKey, r.Model, r.CABundle = endpoint, key, model, strings.TrimSpace(caBundle)
	return r
}

// resolveMeetingsCLI resolves the CLI launch settings for backend. A
// backend with no warm mode starts the CLI on every call, so its persona
// timeouts default higher.
func resolveMeetingsCLI(backend string) ResolvedMeetingsCLI {
	home := meetingsEnvString(MeetingsCLIHomeEnv, "", "")
	if home == "" {
		if st, err := os.Stat(DefaultMeetingsCLIHome); err == nil && st.IsDir() {
			home = DefaultMeetingsCLIHome
		}
	}
	warm := meetingsContains(MeetingsWarmCLIBackends, backend)
	addressed, unprompted, restyle := DefaultMeetingsCLIAddressedTimeoutMS, DefaultMeetingsCLIUnpromptedTimeoutMS, DefaultMeetingsCLIRestyleTimeoutMS
	if !warm {
		addressed, unprompted, restyle = DefaultMeetingsColdCLIAddressedTimeoutMS, DefaultMeetingsColdCLIUnpromptedTimeoutMS, DefaultMeetingsColdCLIRestyleTimeoutMS
	}
	return ResolvedMeetingsCLI{
		Binary:              meetingsEnvString(MeetingsCLIBinaryEnv, "", backend),
		Home:                home,
		AddressedTimeoutMS:  meetingsEnvInt(MeetingsCLIAddressedTimeoutEnv, 0, addressed),
		UnpromptedTimeoutMS: meetingsEnvInt(MeetingsCLIUnpromptedTimeoutEnv, 0, unprompted),
		RestyleTimeoutMS:    meetingsEnvInt(MeetingsCLIRestyleTimeoutEnv, 0, restyle),
		OneShotTimeoutMS:    meetingsEnvInt(MeetingsCLIOneShotTimeoutEnv, 0, DefaultMeetingsCLIOneShotTimeoutMS),
		RecycleTurns:        meetingsEnvInt(MeetingsCLIRecycleTurnsEnv, 0, DefaultMeetingsCLIRecycleTurns),
		Warm:                warm,
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

// ValidateMeetingsLLMSelection is ValidateMeetingsLLMChoice with this hive's
// model gateways accepted as backends.
func (c *Config) ValidateMeetingsLLMSelection(mode, backend string) error {
	m := strings.ToLower(strings.TrimSpace(mode))
	b := strings.ToLower(strings.TrimSpace(backend))
	if m == MeetingsLLMModeAgentBackend && b != "" {
		if why := c.MeetingsLLMBackendRefusalFor(b); why != "" {
			return fmt.Errorf("%s", why)
		}
		return nil
	}
	return ValidateMeetingsLLMChoice(mode, backend)
}
