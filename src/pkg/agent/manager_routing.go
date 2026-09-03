// Backend/model identity and routing overrides: backend name
// validation and binaries, model name normalization, CLI/model pins,
// model/backend overrides, and inference route refresh.
package agent

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/hivecommons/hive/pkg/config"
)

// effectiveBackend returns the agent's backend accounting for any override.
func effectiveBackend(agent *AgentProcess) string {
	if agent.BackendOverride != "" {
		return agent.BackendOverride
	}
	return agent.Config.Backend
}

// IsInferenceBackend returns true if the backend is a self-hosted inference
// backend (vllm, llm-d, litellm) rather than a CLI tool. Delegates to the
// canonical list in the config package (shared with the proxy package,
// which cannot be imported from here without a cycle).
func IsInferenceBackend(backend string) bool {
	return config.IsInferenceBackend(backend)
}

// SetInferenceCallbacks registers callbacks that the manager uses to
// configure/clear inference routing on the proxy when launching agents.
func (m *Manager) SetInferenceCallbacks(
	setRoute func(agentName, backend, model string),
	clearRoute func(agentName string),
) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.inferenceRouteCallback = setRoute
	m.clearInferenceRouteCallback = clearRoute
}

// SetGatewayBackendChecker injects a predicate that reports whether a backend
// string names a configured model gateway. This makes an agent whose backend is
// a gateway name inference-routable, so its route is resolved via the inference
// callback exactly like the built-in litellm/vllm/llm-d backends.
func (m *Manager) SetGatewayBackendChecker(fn func(backend string) bool) {
	// Atomic store — no m.mu — so routableBackend can read it lock-free from the
	// lock-holding launch path without deadlocking (see isGatewayBackend docs).
	m.isGatewayBackend.Store(&fn)
}

// routableBackend reports whether a backend should be routed through the
// inference proxy: either a built-in inference backend, or a configured gateway
// name. Safe to call while holding m.mu (isGatewayBackend is read atomically).
func (m *Manager) routableBackend(backend string) bool {
	if IsInferenceBackend(backend) {
		return true
	}
	// Lock-free atomic read: this is invoked from the launch path while m.mu is
	// already held, so it MUST NOT take m.mu (non-reentrant RWMutex → deadlock).
	fnp := m.isGatewayBackend.Load()
	return fnp != nil && *fnp != nil && (*fnp)(backend)
}

// validateBackendName reports whether backend is one the launcher can actually
// dispatch: an agentic CLI, a model-gateway backend, or a configured gateway
// name. An empty backend is valid (it means "the hive default").
//
// This is the manager-side half of the accept-then-fail fix. It dispatches on
// the SAME canonical lists as config.ValidateBackend and backendBinary, so a
// backend accepted by any write path is one the launch path can start.
// Safe to call while holding m.mu (routableBackend reads atomically).
func (m *Manager) validateBackendName(backend string) error {
	if backend == "" || config.IsCLIBackend(backend) || m.routableBackend(backend) {
		return nil
	}
	return fmt.Errorf("unsupported backend %q (supported: %s; or the name of a configured model gateway)",
		backend, strings.Join(config.SupportedBackends(), ", "))
}

// effectiveBackend is the backend this agent will actually launch with: the
// per-agent override when set, otherwise its configured backend.
func (a *AgentProcess) effectiveBackend() string {
	if a.BackendOverride != "" {
		return a.BackendOverride
	}
	return a.Config.Backend
}

// effectiveModel is the model this agent will actually launch with: the
// per-agent override when set, otherwise its configured model. Returns the
// raw (un-normalized) name — the audit log should show what was ASKED for,
// since a bad model name is exactly the kind of misconfiguration being
// audited.
func (a *AgentProcess) effectiveModel() string {
	if a.ModelOverride != "" {
		return a.ModelOverride
	}
	return a.Config.Model
}

// backendBinaryAliases names the backends whose binary is NOT simply the
// backend name. Only genuine aliases belong here: every other CLI backend is
// derived from config.CLIBackends by identity, and every routable model-gateway
// backend is resolved by Manager.backendBinaryName. Keeping this map to aliases
// only is what makes the accept-then-fail class of bug structurally impossible.
var backendBinaryAliases = map[string]string{
	// pi was previously aliased to "goose", which made every pi-configured
	// agent exec the goose CLI instead of pi (the backend launch command
	// switch now has a real pi case). pi is a first-class CLI backend
	// (config.CLIBackends includes "pi"), so identity mapping applies.
}

// backendBinaryName maps a config-independent agent backend to the NAME of the
// CLI binary that is exec'd for it, without touching the filesystem. Split out
// from backendBinary so the "every supported backend resolves" invariant can be
// tested without requiring each CLI to be installed on the test machine.
//
// Both canonical lists are derived rather than written out here:
//
//   - config.CLIBackends (claude, copilot, goose, codex, pi, bob, aider, gemini)
//     each launch a binary of the same name, except for the aliases above.
//   - config.InferenceBackends (vllm, llm-d, litellm, watsonx) all launch the
//     SAME claude CLI, pointed at hive's local OpenAI-compatible translator via
//     ANTHROPIC_BASE_URL — the backend name selects the upstream route, not the
//     binary.
//
// Deriving both means a backend added to either list can never again be
// accepted by config.ValidateBackend and then rejected hours later at kick time
// with "unknown backend". Previously only InferenceBackends was derived, so
// codex and aider were valid config values that failed at launch.
func backendBinaryName(backend string) (string, error) {
	binaries := make(map[string]string, len(config.CLIBackends)+len(config.InferenceBackends))
	for _, b := range config.CLIBackends {
		binaries[b] = b
	}
	for _, b := range config.InferenceBackends {
		binaries[b] = "claude"
	}
	for backend, binary := range backendBinaryAliases {
		binaries[backend] = binary
	}

	binary, ok := binaries[backend]
	if !ok {
		return "", fmt.Errorf("unknown backend: %s", backend)
	}
	return binary, nil
}

// backendBinaryName resolves both config-independent backends and live
// configured gateway names. A gateway name validates via Manager.routableBackend,
// so the launch path must use the same predicate and route it through claude.
func (m *Manager) backendBinaryName(backend string) (string, error) {
	if binary, err := backendBinaryName(backend); err == nil {
		return binary, nil
	}
	if m != nil && m.routableBackend(backend) {
		return "claude", nil
	}
	return "", fmt.Errorf("unknown backend: %s", backend)
}

// backendBinary resolves an agent backend to the absolute path of the CLI
// binary that is actually exec'd for it.
func backendBinary(backend string) (string, error) {
	binary, err := backendBinaryName(backend)
	if err != nil {
		return "", err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s not found in PATH: %w", backend, err)
	}

	return path, nil
}

func (m *Manager) backendBinary(backend string) (string, error) {
	binary, err := m.backendBinaryName(backend)
	if err != nil {
		return "", err
	}

	path, err := exec.LookPath(binary)
	if err != nil {
		return "", fmt.Errorf("backend %s binary %s not found in PATH: %w", backend, binary, err)
	}

	return path, nil
}

func (m *Manager) backendLaunchFailureMessage(backend string, err error) string {
	binary, nameErr := m.backendBinaryName(backend)
	if nameErr != nil {
		return fmt.Sprintf(
			"backend %s did not launch: %v. This backend is not a supported CLI, built-in inference backend, or configured model gateway; switch this agent to a supported backend or configure a matching model gateway.",
			backend, err)
	}
	return fmt.Sprintf(
		"backend %s did not launch: %v. The %s CLI required for this backend is not installed in this hive image — upgrade the hive image or switch this agent to a different backend.",
		backend, err, binary)
}

// codexBackend is the backend name for the OpenAI Codex CLI.
const codexBackend = "codex"

// bobBackend is the backend name for the IBM bobshell ("bob") CLI.
const bobBackend = "bob"

// normalizeModelName converts YAML-friendly model names to the format each
// CLI backend expects. Claude CLI uses hyphens (claude-opus-4-7), while
// gemini/goose/agy-style backends use dots (claude-opus-4.7).
//
// copilot does NOT take the blind trailing-digits dot-rewrite below: the
// Copilot CLI's --model nomenclature mixes separators per model family
// (claude-fable-5 is DASHED, claude-opus-4.6 is DOTTED), so the rewrite
// corrupted every dashed-family id — verified live, copilot CLI v1.0.78
// rejected the rewritten `claude-fable.5` ("is not available") and fell back
// to a different model (#4262). copilot instead uses the alias-based
// CanonicalizeCopilotModel (copilot_models.go), which normalizes separator
// drift against the known CLI-accepted list in both directions and passes
// unknown ids through verbatim. Applied here — at launch time — so an
// already-stored bad id self-corrects on existing spokes without operator
// action.
//
// Self-hosted inference backends (vllm, llm-d, litellm) and configured gateway
// names are the outbound gateway model id verbatim — the string must match an
// entitled model on the gateway EXACTLY (prefixes like "Azure/", dots vs
// hyphens, case). Rewriting it (e.g. "Azure/gpt-4" -> "Azure/gpt.4",
// "gpt-4o-2024-08-06" -> "gpt-4o-2024-08.06") produces a model the team is not
// entitled to and the gateway 403s ("team not allowed to access model") even
// for entitled models. So never normalize inference model names — pass them
// through untouched.
//
// bob is likewise excluded. bobLaunchCmd passes no --model at all (bob
// auto-selects), so this is defense-in-depth rather than the fix: the value is
// still computed and logged on the bob launch path, and the dot-rewrite is
// what turned a configured `claude-sonnet-4-6` into the unknown
// `claude-sonnet-4.6` that made bob die with "Cannot read properties of
// undefined (reading 'maxTokens')". Leaving it unrewritten keeps logs honest
// about what was configured and stops the corrupted id from being handed to a
// future bob consumer.
func normalizeModelNameForBackend(model, backend string, inferenceRoutable bool) string {
	if backend == "claude" || backend == bobBackend || inferenceRoutable {
		return model
	}
	if backend == "copilot" {
		return CanonicalizeCopilotModel(model)
	}
	idx := strings.LastIndex(model, "-")
	if idx < 0 || idx == len(model)-1 {
		return model
	}
	suffix := model[idx+1:]
	allDigits := true
	for _, c := range suffix {
		if c < '0' || c > '9' {
			allDigits = false
			break
		}
	}
	if allDigits {
		return model[:idx] + "." + suffix
	}
	return model
}

func (m *Manager) PinCLI(name, version string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = version
	m.logger.Info("agent CLI pinned", "name", name, "version", version)
	return nil
}

func (m *Manager) UnpinCLI(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedCLI = ""
	m.logger.Info("agent CLI unpinned", "name", name)
	return nil
}

func (m *Manager) PinModel(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	prevModel := agent.effectiveModel()
	agent.PinnedModel = model
	agent.ModelOverride = model
	m.logger.Info("agent model pinned", "name", name, "model", model)
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
			"trigger", "pin",
		))
	}
	return nil
}

func (m *Manager) UnpinModel(name string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	agent.PinnedModel = ""
	m.logger.Info("agent model unpinned", "name", name)
	return nil
}

func (m *Manager) SetModelOverride(name, model string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	// Store the CLI-accepted spelling for copilot so the persisted selection,
	// the dropdown preselect, and auto-heal all agree on one canonical id
	// (separator drift like claude-fable.5 vs claude-fable-5, #4262). Launch
	// re-applies the same canonicalization, so even ids stored before this
	// existed self-correct there.
	if agent.effectiveBackend() == "copilot" {
		model = CanonicalizeCopilotModel(model)
	}

	// A pin blocks the governor's auto-selection, never a user's explicit
	// switch: retarget the pin to the new model so the pin state is
	// unchanged (still pinned) while the change takes effect.
	if agent.PinnedModel != "" {
		agent.PinnedModel = model
		m.logger.Info("agent model pin retargeted by user switch", "name", name, "model", model)
	}

	prevModel := agent.effectiveModel()
	agent.ModelOverride = model
	m.logger.Info("agent model override set", "name", name, "model", model)
	// State CHANGES only — the governor re-asserts the current model on every
	// evaluation cycle, so auditing unchanged writes would flood the ring.
	if prevModel != model {
		m.audit(AuditAgentModelSet, name, auditFields(
			"outcome", "success",
			"backend", agent.effectiveBackend(),
			"model", model,
			"previous_model", prevModel,
		))
	}

	effectiveBackend := agent.Config.Backend
	if agent.BackendOverride != "" {
		effectiveBackend = agent.BackendOverride
	}
	if m.routableBackend(effectiveBackend) && m.inferenceRouteCallback != nil {
		m.inferenceRouteCallback(name, effectiveBackend, model)
	}
	return nil
}

func (m *Manager) SetBackendOverride(name, backend string) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	agent, ok := m.agents[name]
	if !ok {
		return fmt.Errorf("agent %s not found", name)
	}

	// Refuse a backend the launcher cannot dispatch, at SET time. Previously
	// any string was accepted here and the agent was then restarted into it,
	// failing only at launch with "unknown backend: <x>" — the agent stops
	// working and the operator gets no signal at the moment of the change.
	// routableBackend covers configured gateway names (resolved live), so a
	// gateway-named backend still passes.
	if err := m.validateBackendName(backend); err != nil {
		return err
	}

	// Captured after validation so a rejected switch records no audit event:
	// the override is only mutated below, once the backend is known routable.
	prevBackend := agent.effectiveBackend()
	agent.BackendOverride = backend
	m.logger.Info("agent backend override set", "name", name, "backend", backend)
	// Record only a real transition: /switch/{backend} is also re-applied on
	// config reload with the value already in effect, and auditing those
	// no-ops would bury the actual operator changes.
	if prevBackend != backend {
		m.audit(AuditAgentBackendSet, name, auditFields(
			"outcome", "success",
			"backend", backend,
			"model", agent.effectiveModel(),
			"previous_backend", prevBackend,
		))
	}

	if m.routableBackend(backend) && m.inferenceRouteCallback != nil {
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	} else if !IsInferenceBackend(backend) && m.clearInferenceRouteCallback != nil {
		m.clearInferenceRouteCallback(name)
	}
	return nil
}

// RefreshInferenceRoutes re-fires the inference route callback for every
// agent whose effective backend matches, so endpoint or credential changes
// (e.g. a governor LiteLLM config save) take effect on live agents without
// a restart.
func (m *Manager) RefreshInferenceRoutes(backend string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.inferenceRouteCallback == nil || !m.routableBackend(backend) {
		return
	}
	for name, agent := range m.agents {
		effective := agent.Config.Backend
		if agent.BackendOverride != "" {
			effective = agent.BackendOverride
		}
		if effective != backend {
			continue
		}
		model := agent.ModelOverride
		if model == "" {
			model = agent.Config.Model
		}
		m.inferenceRouteCallback(name, backend, model)
	}
}
