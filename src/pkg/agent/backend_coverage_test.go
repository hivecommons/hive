package agent

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hivecommons/hive/pkg/config"
)

// TestAgentCopilotConfigPaths_PerUIDFirst locks down the ORDERING contract of
// agentCopilotConfigPaths: the per-UID files must be probed before the shared
// path, because the per-UID file is the one the agent's own CLI writes. If the
// shared path came first, a stale shared credential would mask a fresh per-agent
// login and the dashboard would report the wrong auth state.
func TestAgentCopilotConfigPaths_PerUIDFirst(t *testing.T) {
	emptySharedPaths(t)

	// A gateway backend routes AgentHome to the per-agent inference home, which
	// is the case where per-UID paths actually differ from the shared one.
	paths := agentCopilotConfigPaths("ci-maintainer", 2007, "watsonx")
	if len(paths) < 2 {
		t.Fatalf("want per-UID paths plus the shared path, got %v", paths)
	}
	home := AgentHome("ci-maintainer", 2007, "watsonx")
	if home == "" {
		t.Fatal("AgentHome returned empty for a per-UID gateway agent")
	}
	want := []string{
		filepath.Join(home, ".copilot", "config.json"),
		filepath.Join(home, ".config", "github-copilot", "apps.json"),
		filepath.Join(home, ".config", "github-copilot", "hosts.json"),
	}
	for i, w := range want {
		if paths[i] != w {
			t.Errorf("paths[%d] = %q, want %q", i, paths[i], w)
		}
	}
	// The shared path must be present, and must come LAST.
	if got := paths[len(paths)-1]; got != sharedCopilotConfigPath {
		t.Errorf("last path = %q, want the shared path %q", got, sharedCopilotConfigPath)
	}
}

// TestAgentCopilotConfigPaths_NoDuplicateSharedPath: when AgentHome resolves to
// the same place the shared credential lives, the shared path must not be
// appended twice. containsPath exists precisely to prevent that.
func TestAgentCopilotConfigPaths_NoDuplicateSharedPath(t *testing.T) {
	emptySharedPaths(t)
	for _, backend := range []string{"claude", "copilot", "watsonx", ""} {
		paths := agentCopilotConfigPaths("ci-maintainer", 2007, backend)
		seen := map[string]int{}
		for _, p := range paths {
			seen[p]++
		}
		for p, n := range seen {
			if n > 1 {
				t.Errorf("backend %q: path %q appears %d times, want once", backend, p, n)
			}
		}
	}
}

// TestAgentAuthAvailable_UnknownAgent: the dashboard registers
// AgentAuthAvailable as a per-agent provider and calls it by name alone. An
// agent the manager has never heard of must return known=false ("no opinion")
// so the UI renders NO badge, rather than a false "not authenticated" badge.
func TestAgentAuthAvailable_UnknownAgent(t *testing.T) {
	m := &Manager{agents: map[string]*AgentProcess{}}
	avail, known := m.AgentAuthAvailable("no-such-agent")
	if known {
		t.Errorf("known = true for an unknown agent, want false (no badge)")
	}
	if avail {
		t.Errorf("available = true for an unknown agent, want false")
	}
}

// TestAgentAuthAvailable_UsesBackendOverride is the substantive behaviour of
// AgentAuthAvailable: it resolves the agent's EFFECTIVE backend itself. An agent
// configured for an interactive CLI but overridden onto watsonx authenticates by
// IAM, so it must read as authenticated — if the override were ignored, a
// switched agent would show a spurious login badge forever.
func TestAgentAuthAvailable_UsesBackendOverride(t *testing.T) {
	emptySharedPaths(t)
	proc := &AgentProcess{
		Name:            "ci-maintainer",
		UID:             2007,
		State:           StateRunning,
		BackendOverride: "watsonx",
	}
	proc.Config.Backend = "claude"
	proc.NeedsLogin = true // pane looks like a login prompt; must be ignored

	m := &Manager{agents: map[string]*AgentProcess{"ci-maintainer": proc}}

	avail, known := m.AgentAuthAvailable("ci-maintainer")
	if !known || !avail {
		t.Fatalf("watsonx-overridden agent: got (avail=%v, known=%v), want authenticated+known", avail, known)
	}

	// Sanity: the same result must come from the lower-level call with the
	// override applied, proving AgentAuthAvailable resolved the override and
	// not the configured backend.
	wantAvail, wantKnown := m.AgentAuthState("ci-maintainer", proc.UID, "watsonx", true, true)
	if avail != wantAvail || known != wantKnown {
		t.Errorf("AgentAuthAvailable = (%v,%v), AgentAuthState(watsonx) = (%v,%v); override was not applied",
			avail, known, wantAvail, wantKnown)
	}
}

// TestCheckBlockedThrash_TripsOnlyAtThreshold exercises the breaker end to end
// through checkBlockedThrash (the marker-matching + state-bookkeeping wrapper),
// not just the pure sliding-window helper. Below the threshold the agent must
// NOT be paused; the breaker only fires once the loop is established.
func TestCheckBlockedThrash_TripsOnlyAtThreshold(t *testing.T) {
	m := &Manager{
		agents: map[string]*AgentProcess{},
		logger: discardLogger(),
	}

	// A line containing no blocked-action marker must be ignored entirely — it
	// must not even allocate thrash state for the agent.
	m.checkBlockedThrash("ci-maintainer", "everything is fine, pushing now")
	if len(m.thrash) != 0 {
		t.Errorf("a non-blocked line recorded thrash state: %v", m.thrash)
	}

	// Feed threshold-1 real blocked lines: recorded, but not yet tripped.
	marker := blockedActionMarkers[0]
	for i := 0; i < thrashThreshold-1; i++ {
		m.checkBlockedThrash("ci-maintainer", marker+" refusing to push to main")
	}
	st := m.thrash["ci-maintainer"]
	if st == nil {
		t.Fatal("blocked lines did not record thrash state")
	}
	if len(st.times) != thrashThreshold-1 {
		t.Errorf("recorded %d blocked lines, want %d", len(st.times), thrashThreshold-1)
	}
	if !st.lastTrip.IsZero() {
		t.Error("breaker tripped before reaching the threshold")
	}
}

// TestCheckBlockedThrash_MatchesEveryMarker: every marker in
// blockedActionMarkers must actually be recognised. A marker that silently
// stopped matching would disable the breaker for that whole class of block.
func TestCheckBlockedThrash_MatchesEveryMarker(t *testing.T) {
	for _, marker := range blockedActionMarkers {
		m := &Manager{agents: map[string]*AgentProcess{}, logger: discardLogger()}
		m.checkBlockedThrash("a", "prefix text "+marker+" suffix text")
		if st := m.thrash["a"]; st == nil || len(st.times) != 1 {
			t.Errorf("marker %q was not recognised as a blocked-action line", marker)
		}
	}
}

// TestRecordBlockedAndCheck_WindowAndCooldown pins the pure decision function:
// stale entries fall out of the sliding window, and after a trip the cooldown
// suppresses a second trip so one bad loop does not pause an agent repeatedly.
func TestRecordBlockedAndCheck_WindowAndCooldown(t *testing.T) {
	now := time.Now()

	// Entries older than the window must not count toward the threshold.
	st := &thrashState{}
	for i := 0; i < thrashThreshold-1; i++ {
		st.times = append(st.times, now.Add(-2*thrashWindow))
	}
	if recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown) {
		t.Error("tripped on stale entries that should have aged out of the window")
	}
	if len(st.times) != 1 {
		t.Errorf("stale entries were not pruned: %d remain, want 1", len(st.times))
	}

	// Threshold reached inside the window: must trip.
	st = &thrashState{}
	var tripped bool
	for i := 0; i < thrashThreshold; i++ {
		tripped = recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown)
	}
	if !tripped {
		t.Fatalf("did not trip after %d blocked lines inside the window", thrashThreshold)
	}

	// Immediately after a trip, the cooldown must suppress another trip.
	if recordBlockedAndCheck(st, now, thrashWindow, thrashThreshold, thrashCooldown) {
		t.Error("tripped again inside the cooldown")
	}
	// Past the cooldown, it may trip again.
	later := now.Add(thrashCooldown + time.Second)
	st.times = nil
	for i := 0; i < thrashThreshold-1; i++ {
		st.times = append(st.times, later)
	}
	if !recordBlockedAndCheck(st, later, thrashWindow, thrashThreshold, thrashCooldown) {
		t.Error("did not trip after the cooldown expired")
	}
}

// TestBackendBinary_GatewayBackendsAllLaunchClaude pins the derivation added by
// this PR: every model-gateway backend launches the SAME claude CLI, because the
// backend name selects the upstream route (via ANTHROPIC_BASE_URL), not the
// binary. This is what makes adding a backend to config.InferenceBackends
// sufficient — no second edit in the launcher.
func TestBackendBinary_GatewayBackendsAllLaunchClaude(t *testing.T) {
	// backendBinary may return a resolved absolute path (CI installs stub
	// binaries on PATH), so compare the basename — the identity of the CLI is
	// what matters here, not where it was found.
	for _, b := range config.InferenceBackends {
		got, err := backendBinary(b)
		if err != nil {
			t.Errorf("backendBinary(%q) err = %v, want nil", b, err)
			continue
		}
		if filepath.Base(got) != "claude" {
			t.Errorf("backendBinary(%q) = %q, want the claude CLI (gateways share one binary)", b, got)
		}
	}
	// The agentic CLIs installed in the unit-test image must still map to their own binaries.
	// Pi is covered by backendBinaryName and Dockerfile tests because the host
	// unit-test environment does not install the pi CLI.
	for backend, want := range map[string]string{
		"claude": "claude", "copilot": "copilot", "gemini": "gemini",
		"goose": "goose", "bob": "bob",
	} {
		got, err := backendBinary(backend)
		if err != nil || filepath.Base(got) != want {
			t.Errorf("backendBinary(%q) = (%q, %v), want basename %q with nil error", backend, got, err, want)
		}
	}
}

// TestSetBackendOverride_RejectsUndispatchableBackend covers the door this PR
// closed: /switch/{backend} used to accept ANY string and set it, so the agent
// was restarted into a backend the launcher could not start and died with
// "unknown backend" — with no signal at the moment of the change. The override
// must be rejected AND left unmodified.
func TestSetBackendOverride_RejectsUndispatchableBackend(t *testing.T) {
	proc := &AgentProcess{Name: "ci-maintainer", BackendOverride: "claude"}
	m := &Manager{
		agents: map[string]*AgentProcess{"ci-maintainer": proc},
		logger: discardLogger(),
	}

	err := m.SetBackendOverride("ci-maintainer", "totally-made-up")
	if err == nil {
		t.Fatal("SetBackendOverride accepted a backend the launcher cannot dispatch")
	}
	if !strings.Contains(err.Error(), "totally-made-up") {
		t.Errorf("error %q should name the rejected backend", err.Error())
	}
	if proc.BackendOverride != "claude" {
		t.Errorf("a rejected override mutated state: BackendOverride = %q, want %q unchanged",
			proc.BackendOverride, "claude")
	}

	// watsonx — the backend this PR registered — must be ACCEPTED and applied.
	if err := m.SetBackendOverride("ci-maintainer", "watsonx"); err != nil {
		t.Fatalf("SetBackendOverride(watsonx) = %v, want nil", err)
	}
	if proc.BackendOverride != "watsonx" {
		t.Errorf("BackendOverride = %q, want \"watsonx\"", proc.BackendOverride)
	}

	// A missing agent must still be reported as not found, not as a bad backend.
	if err := m.SetBackendOverride("no-such-agent", "claude"); err == nil ||
		!strings.Contains(err.Error(), "not found") {
		t.Errorf("SetBackendOverride on a missing agent = %v, want a 'not found' error", err)
	}
}

// TestBackendBinaryName_EverySupportedBackendResolves is the guard for the
// accept-then-fail class of bug. config.ValidateBackend accepts every member of
// config.SupportedBackends() at config-set time, so the launcher MUST be able to
// dispatch every one of them — otherwise a backend is persisted happily and then
// dies hours later at kick time with "unknown backend".
//
// This regression was real: config.CLIBackends listed codex and aider, but
// backendBinary's hand-written map did not, so `backend: codex` was accepted and
// then failed at launch. The old test enumerated only the backends that were
// already in the map, so it could never catch a missing one. This test derives
// its cases from the canonical list instead, which is what makes it able to.
//
// backendBinaryName is used rather than backendBinary so this asserts the
// mapping is COMPLETE without also requiring every CLI to be installed here.
func TestBackendBinaryName_EverySupportedBackendResolves(t *testing.T) {
	supported := config.SupportedBackends()
	if len(supported) == 0 {
		t.Fatal("config.SupportedBackends() is empty — this test would vacuously pass")
	}
	for _, b := range supported {
		binary, err := backendBinaryName(b)
		if err != nil {
			t.Errorf("config accepts backend %q but the launcher cannot dispatch it: %v", b, err)
			continue
		}
		if binary == "" {
			t.Errorf("backendBinaryName(%q) returned an empty binary name", b)
		}
	}
}

func TestManagerBackendBinaryName_ConfiguredGatewayResolvesToClaude(t *testing.T) {
	m := NewManager(nil, discardLogger(), ProjectContext{})
	m.SetGatewayBackendChecker(func(backend string) bool {
		return strings.EqualFold(backend, "openrouter")
	})
	for _, backend := range []string{"openrouter", "OpenRouter"} {
		binary, err := m.backendBinaryName(backend)
		if err != nil {
			t.Errorf("configured gateway backend %q must launch, got %v", backend, err)
			continue
		}
		if binary != "claude" {
			t.Errorf("m.backendBinaryName(%q) = %q, want claude", backend, binary)
		}
	}
}

func TestAcceptedBackendsAreLaunchableByConstruction(t *testing.T) {
	g := config.GovernorConfig{
		Gateways: []config.GatewayConfig{
			{Name: "openrouter", Kind: config.GatewayKindOpenRouter, Endpoint: "https://openrouter.ai/api/v1"},
			{Name: "Corp-Watsonx", Kind: config.GatewayKindWatsonx, Endpoint: "https://us-south.ml.cloud.ibm.com/ml/gateway"},
		},
	}
	m := NewManager(nil, discardLogger(), ProjectContext{})
	m.SetGatewayBackendChecker(func(backend string) bool {
		return g.ResolveGateway(backend) != nil && backend != ""
	})

	backends := append([]string{""}, config.SupportedBackends()...)
	backends = append(backends, "openrouter", "OPENROUTER", "corp-watsonx")
	for _, backend := range backends {
		if err := g.ValidateBackend(backend); err != nil {
			t.Errorf("test backend %q should be accepted by config: %v", backend, err)
			continue
		}
		if err := m.validateBackendName(backend); err != nil {
			t.Errorf("backend %q accepted by config but rejected by manager validation: %v", backend, err)
			continue
		}
		if backend == "" {
			continue
		}
		if binary, err := m.backendBinaryName(backend); err != nil || binary == "" {
			t.Errorf("backend %q validates but is not launchable: binary=%q err=%v", backend, binary, err)
		}
	}
}

// TestBackendBinaryName_AgyLaunchesAntigravityCLI pins agy to its own binary.
//
// agy is the Antigravity CLI, Google's replacement for the Gemini CLI: Google
// stopped serving Gemini CLI for personal and AI Pro accounts on 2026-06-18, so
// for any hive whose Google access is a subscription rather than metered API
// credits, agy is the ONLY reachable Google backend. It is emphatically not an
// alias for gemini — different binary, different auth, different flags — so a
// future tidy-up that folds it into the gemini derivation would silently break
// every subscription-backed Google agent. This catches that.
func TestBackendBinaryName_AgyLaunchesAntigravityCLI(t *testing.T) {
	if !config.IsCLIBackend("agy") {
		t.Fatal("precondition failed: \"agy\" is not a config.CLIBackends member")
	}
	got, err := backendBinaryName("agy")
	if err != nil {
		t.Fatalf("backendBinaryName(\"agy\") err = %v, want nil", err)
	}
	if got != "agy" {
		t.Errorf("backendBinaryName(\"agy\") = %q, want \"agy\"", got)
	}
}

// TestBackendBinaryName_CodexAndAiderLaunchTheirOwnBinaries pins the specific
// regression: codex and aider are agentic CLIs of their own, not aliases and not
// gateway backends, so each must exec a binary of its own name. If either were
// ever folded into the claude derivation this would catch it.
func TestBackendBinaryName_CodexAndAiderLaunchTheirOwnBinaries(t *testing.T) {
	for _, backend := range []string{"codex", "aider"} {
		if !config.IsCLIBackend(backend) {
			t.Fatalf("precondition failed: %q is no longer a config.CLIBackends member", backend)
		}
		got, err := backendBinaryName(backend)
		if err != nil {
			t.Errorf("backendBinaryName(%q) err = %v, want nil", backend, err)
			continue
		}
		if got != backend {
			t.Errorf("backendBinaryName(%q) = %q, want %q", backend, got, backend)
		}
	}
}

// TestBackendBinaryName_PiLaunchesPiBinary pins the regression that made
// backend: pi launch goose. Pi is a first-class CLI backend, so the launcher
// must exec the pi binary directly rather than reaching an alias.
func TestBackendBinaryName_PiLaunchesPiBinary(t *testing.T) {
	got, err := backendBinaryName("pi")
	if err != nil {
		t.Fatalf("backendBinaryName(\"pi\") err = %v, want nil", err)
	}
	if got != "pi" {
		t.Errorf("backendBinaryName(\"pi\") = %q, want \"pi\"", got)
	}
}

// TestBackendBinaryName_RejectsUnknown keeps the negative path honest: deriving
// from the canonical lists must not turn backendBinaryName into a function that
// accepts anything.
func TestBackendBinaryName_RejectsUnknown(t *testing.T) {
	if _, err := backendBinaryName("totally-made-up"); err == nil {
		t.Fatal("backendBinaryName accepted a backend that is in neither canonical list")
	}
}
