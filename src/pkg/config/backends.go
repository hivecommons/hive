package config

import (
	"fmt"
	"path/filepath"
	"strings"
)

// InferenceBackends is the canonical list of model-gateway / inference backend
// IDs. It lives in the config package (a leaf in the import graph) so the
// agent and proxy packages can share it without an import cycle
// (proxy → agent → config).
//
// These are NOT agentic CLIs. Every one of them names a model endpoint that
// hive fronts with its own OpenAI-compatible translator and drives with the
// claude CLI; auth is an API key (or, for watsonx, an IAM bearer minted from
// one) supplied by config, never an interactive login.
//
// "watsonx" is here because IBM watsonx.ai is a model gateway in exactly that
// sense. It was previously supported ONLY as a `gateways:` kind and as an
// onboarding UI option, while the agent launcher had no case for it — so
// `backend: watsonx` was accepted by config and then rejected hours later at
// kick time with "unknown backend: watsonx". Anything that enumerates gateway
// backends must read THIS list rather than repeating the members inline.
var InferenceBackends = []string{"vllm", "llm-d", "litellm", "watsonx"}

// IsInferenceBackend returns true if the backend name is a self-hosted
// inference backend rather than a CLI tool.
func IsInferenceBackend(backend string) bool {
	for _, b := range InferenceBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// CLIBackends is the canonical list of agentic-CLI agent backends — backends
// that launch a real coding CLI binary rather than routing to a model gateway.
//
// This exists so the CONFIG VALIDATOR and the LAUNCHER dispatch on one list.
// They used to keep separate inline sets, which is the drift that let
// `backend: watsonx` be accepted at config-set time and then fail at kick time
// with "unknown backend: watsonx". Adding a backend in one place only is now a
// visible omission rather than a silent accept-then-fail.
//
// `agy` is the Antigravity CLI, Google's replacement for the Gemini CLI. Google
// consolidated its CLI tooling under Antigravity at I/O 2026 and Gemini CLI
// STOPPED SERVING personal and Google AI Pro accounts on 2026-06-18; only
// customers holding paid Gemini Code Assist licences can still invoke it. The
// `gemini` entry is therefore retained for those licence holders, but a hive
// whose Google access is a Gemini/AI Pro subscription can only reach Google
// through `agy`. It was already listed in config/backends.conf's
// KNOWN_BACKENDS, so omitting it here reproduced exactly the accept-in-one-
// place drift this list exists to prevent — except inverted: valid in the
// shell config, rejected by the hub.
//
// TestShellAndGoCLIBackendListsAgree (backend_list_parity_test.go) asserts
// this list against config/backends.conf's KNOWN_BACKENDS, with a closed,
// commented set of exceptions (cliBackendExceptions in that file) for the two
// names — litellm, gemini — that are known to belong on only one side. Adding
// a backend here without also updating the shell side (or, if it genuinely
// belongs on only one side, documenting why in cliBackendExceptions) fails
// that test.
var CLIBackends = []string{"claude", "copilot", "goose", "codex", "pi", "bob", "aider", "gemini", "agy", "opencode", "kilo", "muse", "omp"}

// IsCLIBackend returns true if the backend launches an agentic CLI binary.
func IsCLIBackend(backend string) bool {
	for _, b := range CLIBackends {
		if b == backend {
			return true
		}
	}
	return false
}

// SupportedBackends returns every backend name valid independent of this
// hive's configuration: the agentic CLIs plus the model-gateway backends. A
// configured gateway NAME is additionally valid as a backend, but that is
// config-dependent and so is checked separately by the validator.
func SupportedBackends() []string {
	out := make([]string, 0, len(CLIBackends)+len(InferenceBackends))
	out = append(out, CLIBackends...)
	out = append(out, InferenceBackends...)
	return out
}

// ValidateBackend reports whether backend is a usable agent backend for this
// config, returning a descriptive error naming the supported values when it is
// not. An empty backend is valid (it means "the hive default").
//
// This is the single gate the config write path uses so an unsupported backend
// is refused AT SET TIME with a clear message, instead of being persisted and
// surfacing later as an agent that silently never launches.
func (g GovernorConfig) ValidateBackend(backend string) error {
	if backend == "" {
		return nil
	}
	if IsCLIBackend(backend) || IsInferenceBackend(backend) {
		return nil
	}
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return nil
		}
	}
	var gatewayNames []string
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" {
			gatewayNames = append(gatewayNames, gw.Name)
		}
	}
	msg := fmt.Sprintf("unsupported backend %q (supported: %s",
		backend, strings.Join(SupportedBackends(), ", "))
	if len(gatewayNames) > 0 {
		msg += fmt.Sprintf("; or a configured gateway name: %s", strings.Join(gatewayNames, ", "))
	} else {
		msg += "; or the name of a gateway configured under the Model Gateways tab"
	}
	return fmt.Errorf("%s)", msg)
}

func LaunchCmdDeclaredBackend(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}
	i := 0
	for i < len(fields) && strings.Contains(fields[i], "=") && !strings.HasPrefix(fields[i], "-") {
		i++
	}
	if i >= len(fields) {
		return ""
	}
	bin := filepath.Base(fields[i])
	if bin == "agent-launch.sh" {
		for j := i + 1; j < len(fields); j++ {
			if fields[j] == "--backend" && j+1 < len(fields) {
				return fields[j+1]
			}
		}
		return ""
	}
	if IsCLIBackend(bin) {
		return bin
	}
	return ""
}

func (g GovernorConfig) ValidateLaunchCmdBackend(backend, launchCmd string) error {
	declared := LaunchCmdDeclaredBackend(launchCmd)
	if declared == "" || backend == "" {
		return nil
	}
	if strings.EqualFold(declared, backend) {
		return nil
	}
	if IsCLIBackend(backend) {
		return fmt.Errorf("backend %q contradicts launch_cmd %q (it launches %q): the agent would be launched as %s but health-checked and diagnosed as %s, then relaunched as \"hung\" forever — set backend to %s or fix launch_cmd",
			backend, launchCmd, declared, declared, backend, declared)
	}
	if IsInferenceBackend(backend) || g.isGatewayName(backend) {
		if declared == "claude" {
			return nil
		}
		return fmt.Errorf("backend %q routes through the claude CLI but launch_cmd %q launches %q — clear launch_cmd or point it at claude",
			backend, launchCmd, declared)
	}
	return nil
}

// agentSourceLabel renders an agent's name for a validation error, naming the
// per-agent overlay file it came from when there is one (#6024).
//
// "agent supervisor" alone is ambiguous: hive.yaml, the ConfigMap seed, the
// dashboard overlay and /data/agent-configs/<name>.yaml all land in the same
// agent map, so an operator reading the crash message has no way to know which
// file to edit - and the overlay directory is the one they are least likely to
// look in. "agent supervisor (from /data/agent-configs/supervisor.yaml)" turns
// an 8-hour hunt into a single edit.
func agentSourceLabel(name, sourceFile string) string {
	if sourceFile == "" {
		return name
	}
	return fmt.Sprintf("%s (from %s)", name, sourceFile)
}

// isGatewayName reports whether backend names a configured model gateway,
// matched case-insensitively to mirror ResolveGateway.
func (g GovernorConfig) isGatewayName(backend string) bool {
	for _, gw := range g.ResolvedGateways() {
		if gw.Name != "" && strings.EqualFold(gw.Name, backend) {
			return true
		}
	}
	return false
}
