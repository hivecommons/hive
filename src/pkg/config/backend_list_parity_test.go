package config

import (
	"os"
	"os/exec"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/internal/testutil"
)

// backendListsConfPath is the shell half of the CLI-backend list. config.go's
// CLIBackends/InferenceBackends are the Go half, read by the config validator
// and the hub-side launcher; config/backends.conf's KNOWN_BACKENDS is read by
// the relay's tmux launch path (bin/agent-launch.sh, bin/hive.sh). A backend
// added to one and not the other is accepted at config-set time and then
// fails at kick time on whichever path was skipped — exactly the watsonx bug
// TestValidateBackend_AcceptsWatsonx above exists to prevent, just for a
// different pair of lists.
const backendListsConfPath = "../../../config/backends.conf"

// cliBackendListException documents one name that legitimately lives in only
// one of KNOWN_BACKENDS (shell) or CLIBackends (Go), and why. This is NOT a
// place to silence real drift: a name added here without the guard actually
// having verified the asymmetry defeats the whole test. See REQUIREMENTS in
// the PR that introduced this file for the bar a new entry must clear.
type cliBackendListException struct {
	name   string
	reason string
}

// cliBackendExceptions is the complete, closed set of asymmetries between
// KNOWN_BACKENDS and CLIBackends. Anything not listed here must appear in
// both lists or the test fails.
var cliBackendExceptions = []cliBackendListException{
	{
		name: "litellm",
		reason: "shell-only. litellm is an inference gateway (it launches the " +
			"claude binary pointed at a LiteLLM proxy via ANTHROPIC_BASE_URL), " +
			"not an agentic CLI, so it belongs in InferenceBackends, not " +
			"CLIBackends. It IS asserted against InferenceBackends below.",
	},
	{
		name: "gemini",
		reason: "Go-only. The relay never dispatches on the literal name " +
			"\"gemini\" — detect_backend_from_model() in backends.conf routes " +
			"gemini-* model names to the gemini case by PREFIX MATCH, not by a " +
			"KNOWN_BACKENDS membership check, so gemini has no entry in " +
			"KNOWN_BACKENDS, backend_binary, or backend_perm_flag to begin " +
			"with. CLIBackends lists it because the hub-side config validator " +
			"and launcher (which have no equivalent prefix-routing step) need " +
			"it to accept `backend: gemini` for paid Gemini Code Assist " +
			"licence holders (see the CLIBackends doc comment in config.go).",
	},
}

// exceptedCLINames returns the set of names cliBackendExceptions permits to
// live in only one list.
func exceptedCLINames() map[string]bool {
	out := make(map[string]bool, len(cliBackendExceptions))
	for _, e := range cliBackendExceptions {
		out[e.name] = true
	}
	return out
}

// shellKnownBackends sources config/backends.conf and returns KNOWN_BACKENDS
// split on whitespace, mirroring how TestShellAndGoDenyListsAgree
// (src/pkg/agent/host_state_deny_test.go) reads CLAUDE_HOST_DENY_TOOLS out of
// the same file: shell out to bash, source the file, print the variable.
func shellKnownBackends(t *testing.T) []string {
	t.Helper()
	if _, err := os.Stat(backendListsConfPath); err != nil {
		testutil.SkipfUnlessRequired(t, "backends.conf not reachable from the test working directory: %v", err)
	}
	out, err := exec.Command("bash", "-c",
		"source "+backendListsConfPath+" && printf '%s' \"$KNOWN_BACKENDS\"").Output()
	if err != nil {
		t.Fatalf("sourcing backends.conf: %v", err)
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		t.Fatalf("KNOWN_BACKENDS came back empty from %s; sourcing likely failed silently", backendListsConfPath)
	}
	return fields
}

// TestShellAndGoCLIBackendListsAgree is the parity check REQUIREMENTS #1-#3
// ask for: every CLI backend known to Go (CLIBackends) is known to the shell
// (KNOWN_BACKENDS) and vice versa, except for the documented exceptions
// above, and a mismatch names the specific backend and the specific list it
// is missing from.
//
// This does go red if a backend is added to CLIBackends only: the reverse
// direction below checks precisely that "every Go-known CLI name is in the
// shell set or excepted", so a bare `append(CLIBackends, "newbackend")` with
// no shell-side or exception-list change fails on the first `go test` run
// touching this file.
func TestShellAndGoCLIBackendListsAgree(t *testing.T) {
	shellSet := make(map[string]bool)
	for _, b := range shellKnownBackends(t) {
		shellSet[b] = true
	}
	goSet := make(map[string]bool)
	for _, b := range CLIBackends {
		goSet[b] = true
	}
	excepted := exceptedCLINames()

	var missing []string
	for name := range shellSet {
		if !goSet[name] && !excepted[name] {
			missing = append(missing, "config/backends.conf KNOWN_BACKENDS has "+name+
				" but config.CLIBackends does not, and it is not a declared exception")
		}
	}
	for name := range goSet {
		if !shellSet[name] && !excepted[name] {
			missing = append(missing, "config.CLIBackends has "+name+
				" but config/backends.conf KNOWN_BACKENDS does not, and it is not a declared exception")
		}
	}

	// A declared exception that no longer describes a real asymmetry (the
	// name is now in BOTH lists, or in NEITHER) is stale bookkeeping that
	// would silently mask a future real removal on one side. Fail loudly
	// rather than let it rot.
	for name := range excepted {
		inShell, inGo := shellSet[name], goSet[name]
		if inShell && inGo {
			missing = append(missing, "declared exception "+name+
				" is now present in BOTH KNOWN_BACKENDS and CLIBackends; remove it from cliBackendExceptions")
		}
		if !inShell && !inGo {
			missing = append(missing, "declared exception "+name+
				" is present in NEITHER KNOWN_BACKENDS nor CLIBackends; remove it from cliBackendExceptions")
		}
	}

	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("shell and Go CLI backend lists have drifted:\n  %s", strings.Join(missing, "\n  "))
	}
}

// TestLiteLLMExceptionIsAnInferenceBackend pins the reason the litellm
// exception is allowed to exist: it must actually be present in
// InferenceBackends, not just absent from CLIBackends. If it were removed
// from InferenceBackends too, litellm would be a name the shell accepts and
// nothing in Go recognizes at all — silently unsupported rather than
// deliberately routed elsewhere.
func TestLiteLLMExceptionIsAnInferenceBackend(t *testing.T) {
	if !IsInferenceBackend("litellm") {
		t.Fatal(`litellm is excepted from CLIBackends parity on the theory that it belongs ` +
			`in InferenceBackends instead, but IsInferenceBackend("litellm") = false. ` +
			`Either restore it to InferenceBackends or drop the exception and treat this as real drift.`)
	}
}

// TestGeminiExceptionRoutesByModelPrefix pins the reason the gemini exception
// is allowed to exist: the shell config must still route gemini-* models to
// the gemini backend by prefix match, even though gemini has no entry in
// KNOWN_BACKENDS. If that routing were deleted, "gemini" would be a Go-only
// name the shell has no path to at all.
func TestGeminiExceptionRoutesByModelPrefix(t *testing.T) {
	if _, err := os.Stat(backendListsConfPath); err != nil {
		testutil.SkipfUnlessRequired(t, "backends.conf not reachable from the test working directory: %v", err)
	}
	out, err := exec.Command("bash", "-c",
		"source "+backendListsConfPath+" && detect_backend_from_model gemini-2.5-pro").Output()
	if err != nil {
		t.Fatalf("sourcing backends.conf: %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "gemini" {
		t.Fatalf(`detect_backend_from_model("gemini-2.5-pro") = %q, want "gemini" — `+
			`the gemini exception in cliBackendExceptions depends on this prefix routing existing`, got)
	}
}

// TestBackendBinaryAndPermFlagCoverKnownBackends is REQUIREMENT #5: a name in
// KNOWN_BACKENDS with no case in backend_binary/backend_perm_flag falls
// through to that function's `*)` default, which is wrong for a supported
// backend (backend_binary's default echoes the name back verbatim as if it
// were also the binary name, which is only sometimes true, and
// backend_perm_flag's default is an empty flag string, silently granting no
// permissions). Every declared backend must hit an explicit case, not the
// fallback.
func TestBackendBinaryAndPermFlagCoverKnownBackends(t *testing.T) {
	if _, err := os.Stat(backendListsConfPath); err != nil {
		testutil.SkipfUnlessRequired(t, "backends.conf not reachable from the test working directory: %v", err)
	}
	for _, name := range shellKnownBackends(t) {
		script := "source " + backendListsConfPath + " && " +
			"grep -qE '^\\s*" + name + "\\)' <(sed -n '/^backend_binary\\(\\)/,/^}/p' " + backendListsConfPath + ")"
		if err := exec.Command("bash", "-c", script).Run(); err != nil {
			t.Errorf("backend_binary in %s has no explicit case for %q (falls through to the default)",
				backendListsConfPath, name)
		}
		script = "source " + backendListsConfPath + " && " +
			"grep -qE '^\\s*" + name + "\\)' <(sed -n '/^backend_perm_flag\\(\\)/,/^}/p' " + backendListsConfPath + ")"
		if err := exec.Command("bash", "-c", script).Run(); err != nil {
			t.Errorf("backend_perm_flag in %s has no explicit case for %q (falls through to the default)",
				backendListsConfPath, name)
		}
	}
}
