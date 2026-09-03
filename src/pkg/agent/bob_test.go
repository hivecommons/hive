package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kubestellar/hive/pkg/config"
)

// TestWatchedHomeDirsIncludesBob pins the fix for bob's startup failure:
//
//	EACCES: permission denied, mkdir '/data/home/.bob'
//
// bob derives its state dir as path.join(os.homedir(), ".bob") and agents run
// with HOME=/data/home, which is root-created — so the watcher must own it.
//
// It checks productionWatchedHomeDirs — the snapshot TestMain takes before
// pointing WatchedHomeDirs into the hermetic temp tree — so the pin stays on
// the production default, not the test override.
func TestWatchedHomeDirsIncludesBob(t *testing.T) {
	const bobStateDir = "/data/home/.bob"
	for _, dir := range productionWatchedHomeDirs {
		if dir == bobStateDir {
			return
		}
	}
	t.Fatalf("WatchedHomeDirs must contain %q so bob can create its state dir; got %v",
		bobStateDir, productionWatchedHomeDirs)
}

// TestPermissionsWatcher_BobNestedTree covers the nested case explicitly: bob
// writes .bob/tmp/<uuid>/chats, so a fix that only handled the top level would
// leave the chat-recording service broken. ensureWatchedDirs must create the
// root, and fixPermissions must walk (and correct) arbitrarily deep children.
func TestPermissionsWatcher_BobNestedTree(t *testing.T) {
	tests := []struct {
		name string
		// nested is created before the watcher runs, relative to the .bob root.
		nested string
		// file, when set, is created inside nested with restrictive perms.
		file string
	}{
		{name: "top level only", nested: ""},
		{name: "tmp subdir", nested: "tmp"},
		{name: "chats tree", nested: filepath.Join("tmp", "abc-123", "chats")},
		{name: "chats tree with file", nested: filepath.Join("tmp", "def-456", "chats"), file: "session.json"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			bobRoot := filepath.Join(dir, ".bob")

			target := bobRoot
			if tc.nested != "" {
				target = filepath.Join(bobRoot, tc.nested)
			}
			// 0o700 is what bob's mkdirSync default would leave behind: unusable
			// by other agent UIDs in the shared node group.
			if err := os.MkdirAll(target, 0o700); err != nil {
				t.Fatal(err)
			}
			if tc.file != "" {
				if err := os.WriteFile(filepath.Join(target, tc.file), []byte("{}"), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			origW, origG := WatchedHomeDirs, GooseLogsDir
			WatchedHomeDirs = []string{bobRoot}
			GooseLogsDir = filepath.Join(dir, "goose", "logs")
			t.Cleanup(func() { WatchedHomeDirs = origW; GooseLogsDir = origG })

			// Record every path the walk should visit, so we can assert the walk
			// actually reaches the full depth of bob's tree.
			var walked []string
			if err := filepath.Walk(bobRoot, func(p string, fi os.FileInfo, err error) error {
				if err == nil {
					walked = append(walked, p)
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}

			ensureWatchedDirs(discardLogger())
			fixPermissions(discardLogger())

			// The root must exist: ensureWatchedDirs creates it (MkdirAll) with
			// group bits, which is what fixes bob's "EACCES: mkdir '/data/home/.bob'".
			info, err := os.Stat(bobRoot)
			if err != nil {
				t.Fatalf("bob state dir should exist: %v", err)
			}
			if !info.IsDir() {
				t.Fatal("bob state dir should be a directory")
			}

			// fixPermissions must WALK the whole nested tree, not just the root.
			// This is the property that makes .bob/tmp/<uuid>/chats recoverable:
			// anything created root-owned underneath is chowned on the next tick.
			//
			// The mode assertions that would normally follow are deliberately
			// omitted: fixEntry only chmods entries owned by DevUID (1001) — see
			// the "Only fix permissions on files we own or just chowned" guard —
			// and an unprivileged test process owns these files as its own UID,
			// so production behavior here is correctly a no-op. Asserting modes
			// would require running as uid 1001 or as root.
			if tc.nested != "" {
				if _, err := os.Stat(target); err != nil {
					t.Fatalf("nested dir %q should exist: %v", tc.nested, err)
				}
				wantDepth := len(strings.Split(tc.nested, string(filepath.Separator)))
				// root + one entry per nested level (+1 more when a file exists).
				minWalked := 1 + wantDepth
				if tc.file != "" {
					minWalked++
				}
				if len(walked) < minWalked {
					t.Errorf("walk visited %d paths (%v), want at least %d — the nested "+
						"tree must be traversed, not just the root", len(walked), walked, minWalked)
				}
			}
			if tc.file != "" {
				if _, err := os.Stat(filepath.Join(target, tc.file)); err != nil {
					t.Fatalf("nested file should exist: %v", err)
				}
			}
		})
	}
}

// TestBobAPIKeyResolver covers the resolver seam: absent when never injected,
// absent when the resolver reports no key, present when it does.
func TestBobAPIKeyResolver(t *testing.T) {
	tests := []struct {
		name     string
		inject   bool
		resolver func() string
		want     string
	}{
		{name: "no resolver injected", inject: false, want: ""},
		{name: "nil resolver injected", inject: true, resolver: nil, want: ""},
		{name: "resolver returns empty", inject: true, resolver: func() string { return "" }, want: ""},
		{name: "resolver returns key", inject: true, resolver: func() string { return "bob-key-abc" }, want: "bob-key-abc"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			if tc.inject {
				m.SetBobAPIKeyResolver(tc.resolver)
			}
			if got := m.bobAPIKey(); got != tc.want {
				t.Errorf("bobAPIKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestBobEnvPairInjection verifies the key is plumbed into the agent env under
// the name bob actually reads, marked Secret so it never reaches the command
// line, and is confined to the bob backend.
func TestBobEnvPairInjection(t *testing.T) {
	tests := []struct {
		name       string
		backend    string
		key        string
		wantPair   bool
		wantSecret bool
	}{
		{name: "bob with key", backend: "bob", key: "bob-key-abc", wantPair: true, wantSecret: true},
		{name: "bob without key", backend: "bob", key: "", wantPair: false},
		{name: "claude never gets bob key", backend: "claude", key: "bob-key-abc", wantPair: false},
		{name: "copilot never gets bob key", backend: "copilot", key: "bob-key-abc", wantPair: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			key := tc.key
			m.SetBobAPIKeyResolver(func() string { return key })

			agent := &AgentProcess{
				Name:   "tester",
				Config: config.AgentConfig{Backend: tc.backend},
			}

			var found *agentEnvPair
			for _, p := range m.agentEnvPairs(agent) {
				if p.Key == config.BobAPIKeyEnvVar {
					pair := p
					found = &pair
				}
			}

			if tc.wantPair != (found != nil) {
				t.Fatalf("%s pair present = %v, want %v", config.BobAPIKeyEnvVar, found != nil, tc.wantPair)
			}
			if found == nil {
				return
			}
			if found.Value != tc.key {
				t.Errorf("pair value = %q, want %q", found.Value, tc.key)
			}
			if found.Secret != tc.wantSecret {
				t.Errorf("pair Secret = %v, want %v (a key must never reach the command line)",
					found.Secret, tc.wantSecret)
			}
		})
	}
}

// TestBobAuthTypeEnvPair guards one of the three layers that keep bob off the
// SSO prompt. BOBSHELL_DEFAULT_AUTH_TYPE is the weakest of them — per
// bundle/bob.js it is only the FALLBACK default and loses to a persisted
// security.auth.selectedType — but it still must be present on bob agents and
// absent everywhere else. The stronger layers are --auth-method (see
// TestBobLaunchCmd) and the settings file (see TestEnsureBobAuthSettings).
func TestBobAuthTypeEnvPair(t *testing.T) {
	tests := []struct {
		name     string
		backend  string
		key      string
		wantPair bool
	}{
		{"bob backend with key", "bob", "bob-key", true},
		// The auth type is not a credential, so it does not depend on the key
		// being resolvable — bob must still be told not to use SSO.
		{"bob backend without key", "bob", "", true},
		{"claude backend", "claude", "bob-key", false},
		{"copilot backend", "copilot", "bob-key", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			key := tc.key
			m.SetBobAPIKeyResolver(func() string { return key })

			agent := &AgentProcess{
				Name:   "tester",
				Config: config.AgentConfig{Backend: tc.backend},
			}

			var found *agentEnvPair
			for _, p := range m.agentEnvPairs(agent) {
				if p.Key == config.BobAuthTypeEnvVar {
					pair := p
					found = &pair
				}
			}
			if tc.wantPair != (found != nil) {
				t.Fatalf("%s pair present = %v, want %v", config.BobAuthTypeEnvVar, found != nil, tc.wantPair)
			}
			if found == nil {
				return
			}
			if found.Value != config.BobAuthTypeAPIKey {
				t.Errorf("pair value = %q, want %q", found.Value, config.BobAuthTypeAPIKey)
			}
			// Must be NON-secret: secret pairs only reach a freshly-created
			// pane via tmux set-environment, while non-secret pairs are
			// re-applied on every launch through buildEnvPrefix. See #2228.
			if found.Secret {
				t.Error("auth type must not be Secret: it is not a credential and must be re-applied on every launch")
			}
			prefix := m.buildEnvPrefix(agent)
			want := config.BobAuthTypeEnvVar + "="
			if !strings.Contains(prefix, want) {
				t.Errorf("env prefix = %q, want it to contain %q so every launch re-applies it", prefix, want)
			}
		})
	}
}

// TestBobKeyNotInEnvPrefix is the regression guard for leaking the key onto the
// shell command line, where it would show up in `ps` and pane scrollback.
func TestBobKeyNotInEnvPrefix(t *testing.T) {
	const secretKey = "bob-key-must-not-leak"
	m := &Manager{logger: discardLogger()}
	m.SetBobAPIKeyResolver(func() string { return secretKey })

	agent := &AgentProcess{
		Name:   "tester",
		Config: config.AgentConfig{Backend: "bob"},
	}
	prefix := m.buildEnvPrefix(agent)
	if strings.Contains(prefix, secretKey) {
		t.Errorf("env prefix must not contain the API key value; got %q", prefix)
	}
	if strings.Contains(prefix, config.BobAPIKeyEnvVar) {
		t.Errorf("env prefix must not mention %s at all; got %q", config.BobAPIKeyEnvVar, prefix)
	}
}

// TestBobLaunchCmd pins the flags that make headless auth work, keep the
// session interactive, and let an unattended agent actually run tool calls.
func TestBobLaunchCmd(t *testing.T) {
	got := bobLaunchCmd("bob")

	wantContain := []string{
		"bob",
		// Clears the license gate that would otherwise hard-error with no
		// human to answer it.
		"--accept-license",
		// The strongest auth control: a real (but --help-hidden) flag in
		// bobshell 1.0.6, and the only input that outranks the persisted,
		// fleet-shared settings file.
		"--auth-method api-key",
		// Without this bob reports `Auto-approve: Off` and blocks forever on
		// the first tool call. Verified live: with it, bob executed a shell
		// tool unattended.
		"--approval-mode yolo",
		// Clears "This folder is not trusted. Some features may be disabled."
		"--trust",
	}
	for _, want := range wantContain {
		if !strings.Contains(got, want) {
			t.Errorf("bobLaunchCmd() = %q, want it to contain %q", got, want)
		}
	}

	wantAbsent := []string{
		// --model is the BUG: normalizeModelName rewrote claude-sonnet-4-6 to
		// claude-sonnet-4.6, an id bob's backend does not know, and every
		// prompt died with "Cannot read properties of undefined (reading
		// 'maxTokens')". bob auto-selects its model; hive must not pass one.
		"--model",
		// Interactive mode must be preserved.
		" -p ",
		"--prompt",
	}
	for _, absent := range wantAbsent {
		if strings.Contains(got, absent) {
			t.Errorf("bobLaunchCmd() = %q, want it NOT to contain %q", got, absent)
		}
	}
}

// TestBobLaunchCmdApprovalIsFlat documents the deliberate design decision that
// bob's approval mode does NOT vary with the agent's ACMM mode.
//
// Every other backend already auto-approves tools at every level (claude gets
// --dangerously-skip-permissions, copilot gets --allow-all, in all three mode
// branches of launchInTmux). Hive contains low-mode agents by denying GitHub
// write tools and unsetting GH_TOKEN/GITHUB_TOKEN, not by making them ask
// permission for local tool calls. bob matches that posture.
func TestBobLaunchCmdApprovalIsFlat(t *testing.T) {
	// bobLaunchCmd takes no mode argument at all, so the command is identical
	// for every agent. Guard against a future signature change reintroducing
	// per-mode approval without updating this reasoning.
	if a, b := bobLaunchCmd("bob"), bobLaunchCmd("bob"); a != b {
		t.Fatalf("bobLaunchCmd() is not deterministic: %q vs %q", a, b)
	}
	for _, forbidden := range []string{"--approval-mode default", "--approval-mode auto_edit"} {
		if strings.Contains(bobLaunchCmd("bob"), forbidden) {
			t.Errorf("bobLaunchCmd() must not use %q: bob matches the fleet's "+
				"flat auto-approve posture", forbidden)
		}
	}
}

// TestNormalizeModelNameSkipsBob covers the dot-rewrite that corrupted bob's
// model id. bob is now excluded alongside claude and the inference backends.
// TestToolRulesToLaunchCmdBobHasNoModel guards the second path that could feed
// bob a --model: an agent with Config.Tools set bypasses the launchInTmux
// switch entirely and would otherwise fall to the default branch.
func TestToolRulesToLaunchCmdBobHasNoModel(t *testing.T) {
	tools := &config.ToolsConfig{}
	got := toolRulesToLaunchCmd("bob", "claude-sonnet-4.6", bobBackend, tools, false)
	if strings.Contains(got, "--model") {
		t.Errorf("toolRulesToLaunchCmd(bob) = %q must not contain --model", got)
	}
	for _, want := range []string{"--accept-license", "--approval-mode yolo", "--trust"} {
		if !strings.Contains(got, want) {
			t.Errorf("toolRulesToLaunchCmd(bob) = %q, want it to contain %q", got, want)
		}
	}
	// Non-bob backends must still receive their model.
	if c := toolRulesToLaunchCmd("copilot", "gpt-5", "copilot", tools, false); !strings.Contains(c, "--model gpt-5") {
		t.Errorf("toolRulesToLaunchCmd(copilot) = %q, want it to contain --model gpt-5", c)
	}
}

// TestBobLaunchRefusedWithoutKey verifies the fail-fast: with no key
// configured, launchInTmux must mark the agent failed and return rather than
// starting bob, which would otherwise hang for 3 minutes on a browser flow
// that cannot complete in a pod.
func TestBobLaunchRefusedWithoutKey(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		wantState ProcessState
	}{
		{name: "no key refuses launch", key: "", wantState: StateFailed},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := os.Stat("/usr/local/bin/bob"); err == nil {
				t.Skip("bob binary present; this test covers the no-key path only")
			}
			m := &Manager{logger: discardLogger()}
			key := tc.key
			m.SetBobAPIKeyResolver(func() string { return key })

			agent := &AgentProcess{
				Name:   "tester",
				Config: config.AgentConfig{Backend: "bob"},
			}
			// launchInTmux returns nil for both the missing-binary and missing-key
			// paths (one bad agent must never abort the fleet); StateFailed is the
			// observable signal in either case.
			if err := m.launchInTmux(t.Context(), agent); err != nil {
				t.Fatalf("launchInTmux should return nil, got %v", err)
			}
			if agent.State != tc.wantState {
				t.Errorf("agent.State = %v, want %v", agent.State, tc.wantState)
			}
			if agent.HasLaunched {
				t.Error("agent must not be marked launched when auth cannot succeed")
			}
		})
	}
}

// TestBobBackendAuthAvailable verifies the dashboard sees honest auth state for
// bob (previously "unknown", like every non-claude/copilot backend).
func TestBobBackendAuthAvailable(t *testing.T) {
	tests := []struct {
		name          string
		key           string
		wantAvailable bool
		wantKnown     bool
	}{
		{name: "key configured", key: "bob-key-abc", wantAvailable: true, wantKnown: true},
		{name: "no key configured", key: "", wantAvailable: false, wantKnown: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{logger: discardLogger()}
			key := tc.key
			m.SetBobAPIKeyResolver(func() string { return key })

			available, known := m.BackendAuthAvailable("bob")
			if available != tc.wantAvailable || known != tc.wantKnown {
				t.Errorf("BackendAuthAvailable(bob) = (%v, %v), want (%v, %v)",
					available, known, tc.wantAvailable, tc.wantKnown)
			}
		})
	}
}
