package dashboard

import (
	"sort"
	"testing"
)

// Every static model list hive ships is a fallback a dropdown can land on, so
// each must be non-empty and duplicate-free, and the copies baked into
// static/index.html must match the Go lists the server serves. The weekly
// static-model-freshness workflow compares the CONTENTS against public
// sources; this test pins the SHAPE.
func TestStaticModelListsShape(t *testing.T) {
	lists := map[string][]string{
		"claudeStaticModels":          claudeStaticModels,
		"claudePinnedCLIModels":       claudePinnedCLIModels,
		"claudeAlwaysIncludeModels":   claudeAlwaysIncludeModels,
		"copilotStaticModels":         copilotStaticModels,
		"copilotPinnedCLIModels":      copilotPinnedCLIModels,
		"copilotAlwaysIncludeModels":  copilotAlwaysIncludeModels,
		"codexStaticModels":           codexStaticModels,
		"geminiStaticModels":          geminiStaticModels,
		"geminiPinnedCLIModels":       geminiPinnedCLIModels,
		"bobStaticModels":             bobStaticModels,
		"piKiroStaticModels":          piKiroStaticModels,
		"agyStaticModels":             agyStaticModels,
		"ompStaticModels":             ompStaticModels,
		"inferenceStaticModelAliases": inferenceStaticModelAliases,
	}
	for provider, models := range gooseProviderStaticModels {
		lists["gooseProviderStaticModels["+provider+"]"] = models
	}
	names := make([]string, 0, len(lists))
	for name := range lists {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		assertModelListShape(t, name, lists[name])
	}

	html := indexHTML(t)
	// Lists the browser shows before /api/config/backends answers. These are
	// exact copies of the Go fallbacks, order included.
	for goName, jsConst := range map[string]string{
		"claudeStaticModels": "CLAUDE_CLI_MODELS",
		"geminiStaticModels": "GEMINI_CLI_MODELS",
		"codexStaticModels":  "CODEX_CLI_MODELS",
		"agyStaticModels":    "AGY_CLI_MODELS",
		"ompStaticModels":    "OMP_CLI_MODELS",
		"bobStaticModels":    "BOB_CLI_MODELS",
	} {
		got := htmlModelValues(t, html, jsConst)
		assertModelListShape(t, jsConst, got)
		if !equalStrings(got, lists[goName]) {
			t.Errorf("static/index.html %s = %v, want %s = %v (keep cli_models*.go and backends-tokens.js in sync)", jsConst, got, goName, lists[goName])
		}
	}
	// Copilot's first-paint list is a curated subset (it leads with auto and
	// the picker model); every id it offers must be one the CLI accepts.
	copilotJS := htmlModelValues(t, html, "COPILOT_CLI_MODELS")
	assertModelListShape(t, "COPILOT_CLI_MODELS", copilotJS)
	pinned := map[string]bool{}
	for _, m := range copilotPinnedCLIModels {
		pinned[m] = true
	}
	for _, m := range copilotJS {
		if !pinned[m] {
			t.Errorf("COPILOT_CLI_MODELS offers %q, which is not in copilotPinnedCLIModels", m)
		}
	}
	assertModelListShape(t, "GOOSE_CLI_MODELS", htmlModelValues(t, html, "GOOSE_CLI_MODELS"))
}

func assertModelListShape(t *testing.T, name string, models []string) {
	t.Helper()
	if len(models) == 0 {
		t.Errorf("%s is empty: a dropdown falling back to it would offer nothing", name)
		return
	}
	seen := map[string]bool{}
	for _, m := range models {
		if m == "" {
			t.Errorf("%s contains an empty id", name)
		}
		if seen[m] {
			t.Errorf("%s lists %q twice", name, m)
		}
		seen[m] = true
	}
}
