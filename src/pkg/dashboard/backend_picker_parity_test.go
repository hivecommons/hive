package dashboard

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// The dashboard's embedded JS carries a THIRD backend list, independent of the
// two #4987 brought into agreement (config/backends.conf's KNOWN_BACKENDS and
// config.CLIBackends). It is not decorative: backendOptionList() builds the
// backend/method dropdowns from it, so every name in it is a name an operator
// can select and save.
//
// It had drifted. `amazonq` was offered in the picker and named in the CLI Pin
// Value tooltip while living in NEITHER authoritative registry — it had been
// dropped from backends.conf in #1045 ("6 CLIs: Claude, Copilot, Goose, Codex,
// Agy, Bob") and the UI was never brought along. validateBackendName rejects
// it, so picking it produced a launch failure: the accept-then-fail class that
// function exists to prevent (#4988).
//
// This guard closes the loop the Go-side parity test could not reach.

const backendPickerHTMLPath = "static/index.html"

// jsBackendListRE extracts a `const NAME = ['a', 'b', ...];` array from the
// embedded page. Anchored on the const declaration so a mention of the same
// identifier in a comment or a .includes() call cannot be mistaken for the
// declaration.
func jsBackendList(t *testing.T, src, constName string) []string {
	t.Helper()
	re := regexp.MustCompile(`(?m)^\s*const ` + regexp.QuoteMeta(constName) + `\s*=\s*\[([^\]]*)\];`)
	m := re.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("could not find `const %s = [...]` in %s — the picker's source list moved and this guard would silently cover nothing",
			constName, backendPickerHTMLPath)
	}
	var out []string
	for _, raw := range strings.Split(m[1], ",") {
		name := strings.Trim(strings.TrimSpace(raw), "'\"")
		if name != "" {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		t.Fatalf("`const %s` parsed to an empty list; the regex matched but the contents did not", constName)
	}
	return out
}

func backendPickerSource(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(backendPickerHTMLPath)
	if err != nil {
		t.Fatalf("read %s: %v", backendPickerHTMLPath, err)
	}
	return string(raw)
}

// pickerOnlyException documents one name the picker may offer that is in
// neither config.CLIBackends nor config.InferenceBackends.
//
// The bar is the same as cliBackendExceptions in pkg/config: a name belongs
// here only when it is reachable by some OTHER dispatch mechanism. It is not a
// place to silence drift — `amazonq` was exactly the kind of name that must
// NOT be added here, because nothing could launch it at all.
type pickerOnlyException struct {
	name   string
	reason string
}

var pickerOnlyExceptions = []pickerOnlyException{
	{
		name: "openrouter",
		reason: "A Model Gateway preset, not a backend binary. Agent routing " +
			"resolves any CONFIGURED gateway name (main.go treats a gateway " +
			"name as inference-routable), and the page deliberately offers " +
			"openrouter before a matching gateway exists so an operator can " +
			"pick it and then fund/configure it — see the comment above " +
			"KNOWN_BACKENDS. So it is dispatchable, just not through the two " +
			"static registries.",
	},
}

// TestBackendPickerOffersOnlyDispatchableBackends is the #4988 regression.
//
// Direction matters: this asserts picker ⊆ registries, NOT the reverse. A name
// the picker offers but nothing can launch is a broken promise to the operator
// — the bug this issue reported. A supported backend the picker omits is only
// under-advertisement. Some registered CLIs still target contributor-only or
// otherwise separately constrained paths, so this generic safety guard must not
// turn registry membership into a promise that every backend belongs here.
// Backend-specific visibility requirements (including agy) get explicit tests.
func TestBackendPickerOffersOnlyDispatchableBackends(t *testing.T) {
	picker := jsBackendList(t, backendPickerSource(t), "KNOWN_BACKENDS")

	dispatchable := make(map[string]bool)
	for _, b := range config.CLIBackends {
		dispatchable[b] = true
	}
	for _, b := range config.InferenceBackends {
		dispatchable[b] = true
	}
	excepted := make(map[string]bool, len(pickerOnlyExceptions))
	for _, e := range pickerOnlyExceptions {
		excepted[e.name] = true
	}

	var bad []string
	for _, name := range picker {
		if !dispatchable[name] && !excepted[name] {
			bad = append(bad, "the dashboard backend picker offers "+name+
				" but it is in neither config.CLIBackends nor config.InferenceBackends, "+
				"so validateBackendName rejects it and selecting it fails at launch")
		}
	}

	// A declared exception that has since become a real registered backend is
	// stale bookkeeping, and would mask a future genuine removal from the
	// registries. Same rule as cliBackendExceptions in pkg/config.
	for _, e := range pickerOnlyExceptions {
		if dispatchable[e.name] {
			bad = append(bad, "declared picker exception "+e.name+
				" is now in an authoritative registry; remove it from pickerOnlyExceptions")
		}
		if !pickerOffers(picker, e.name) {
			bad = append(bad, "declared picker exception "+e.name+
				" is no longer offered by the picker; remove it from pickerOnlyExceptions")
		}
	}

	if len(bad) > 0 {
		sort.Strings(bad)
		t.Fatalf("dashboard backend picker has drifted from the dispatchable backends:\n  %s",
			strings.Join(bad, "\n  "))
	}
}

// TestBackendPickerInferenceListMatchesGo pins the page's second list. The two
// JS arrays are used together — INFERENCE_BACKENDS decides which picker entries
// are treated as gateways rather than CLIs — so a name drifting between them
// mislabels the entry even when both lists are individually plausible.
func TestBackendPickerInferenceListMatchesGo(t *testing.T) {
	js := jsBackendList(t, backendPickerSource(t), "INFERENCE_BACKENDS")

	want := make(map[string]bool)
	for _, b := range config.InferenceBackends {
		want[b] = true
	}
	// The gateway presets are inference methods in the UI without being Go
	// InferenceBackends entries, for the reason recorded in
	// pickerOnlyExceptions.
	for _, e := range pickerOnlyExceptions {
		want[e.name] = true
	}

	got := make(map[string]bool, len(js))
	for _, name := range js {
		got[name] = true
	}

	var bad []string
	for name := range got {
		if !want[name] {
			bad = append(bad, "JS INFERENCE_BACKENDS has "+name+" but Go InferenceBackends does not")
		}
	}
	for name := range want {
		if !got[name] {
			bad = append(bad, "Go InferenceBackends has "+name+" but JS INFERENCE_BACKENDS does not, "+
				"so the picker renders it as a CLI backend")
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		t.Fatalf("inference-backend lists have drifted:\n  %s", strings.Join(bad, "\n  "))
	}
}

// TestCLIPinTooltipNamesNoUndispatchableBackend covers the OTHER place #4988
// found amazonq: the CLI Pin Value tooltip enumerates backends in prose, and
// prose is not reached by the array guard above. It is checked loosely — the
// tooltip legitimately lists a readable subset — so this only fails when it
// names something that cannot be launched at all.
func TestCLIPinTooltipNamesNoUndispatchableBackend(t *testing.T) {
	src := backendPickerSource(t)
	const marker = "Which CLI backend to pin this agent to when CLI Pinned is on. Options:"
	i := strings.Index(src, marker)
	if i < 0 {
		// Fail rather than skip (#5388): the tooltip text is repo content, so
		// this condition is identical on every machine and a skip could never
		// mean "unsuitable environment" — it means the marker moved and the
		// prose guard #4988 asked for silently stopped covering anything.
		// Rewording the tooltip is fine; re-point the marker in the same PR.
		t.Fatal("CLI Pin Value tooltip marker not found; the tooltip was reworded — " +
			"re-point the marker in TestCLIPinTooltipNamesNoUndispatchableBackend " +
			"in the PR that reworded it")
	}
	rest := src[i+len(marker):]
	if end := strings.Index(rest, "."); end >= 0 {
		rest = rest[:end]
	}

	dispatchable := make(map[string]bool)
	for _, b := range config.CLIBackends {
		dispatchable[b] = true
	}
	for _, b := range config.InferenceBackends {
		dispatchable[b] = true
	}
	for _, e := range pickerOnlyExceptions {
		dispatchable[e.name] = true
	}

	for _, raw := range strings.Split(rest, ",") {
		name := strings.TrimSpace(raw)
		if name == "" {
			continue
		}
		if !dispatchable[name] {
			t.Errorf("the CLI Pin Value tooltip offers %q, which is in no authoritative backend registry — "+
				"pinning an agent to it fails at launch (#4988)", name)
		}
	}
}

// pickerOffers reports whether the parsed picker list contains name.
func pickerOffers(picker []string, name string) bool {
	for _, s := range picker {
		if s == name {
			return true
		}
	}
	return false
}
