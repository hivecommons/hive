package policies

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// policySourceDir is the human-editable mirror of the embedded templates,
// relative to this package. Operators deploy straight from it: hive-quickstart
// sets policies.path to src/policies/, so these files are read from disk at
// runtime, while DefaultPolicies below is the compiled-in fallback used when no
// policies directory resolves.
const policySourceDir = "../../policies"

// knownDivergentPolicies lists the templates whose two copies are NOT yet
// byte-identical, with the reason each is still outstanding.
//
// Every entry is a deliberate, reviewed exception — not a licence to drift. The
// two copies hold COMPLEMENTARY guardrails that a directional copy would
// silently delete (see TestPolicyGuardrailsPresent for the specific rules being
// protected), so reconciling them is a semantic merge that needs a maintainer's
// judgement about agent behaviour, not a mechanical sync.
//
// This map may only ever shrink. Adding a name to it re-opens the exact hole
// this test exists to close, so a new entry needs the same scrutiny as deleting
// the test.
var knownDivergentPolicies = map[string]string{
	"brainstorm-advisory.md":    "two generations of the capture prompt: embed has the hard DO-NOT-clone/init guards, source has the structured question categories",
	"ci-maintainer-advisory.md": "source replaced the literal bd-create example with a NEVER-execute-an-example rule; embed still carries the example",
	"guide-advisory.md":         "same bd-create example replacement as ci-maintainer-advisory",
	"scanner-automerge.md":      "embed holds baseline-triage, finish-existing-PRs and the hive-open-pr requirement; source holds the enhancement/feature framing",
	"scanner-full.md":           "source supersedes the triage wording; embed retains the older analyze-root-cause phrasing",
	"scanner-holdgated.md":      "embed holds the rejected_duplicate guard; source holds the work-list-is-an-implementation-queue rule",
	"scanner-issues.md":         "embed holds the rejected_duplicate guard; source holds the do-not-re-file-human-enhancements rule",
}

func readPolicyPair(t *testing.T, name string) (source, embedded string, ok bool) {
	t.Helper()
	srcBytes, err := os.ReadFile(filepath.Join(policySourceDir, name))
	if err != nil {
		t.Errorf("read policy source %s: %v", name, err)
		return "", "", false
	}
	embedBytes, err := DefaultPolicies.ReadFile("defaults/" + name)
	if err != nil {
		t.Errorf("read embedded policy %s: %v", name, err)
		return "", "", false
	}
	return string(srcBytes), string(embedBytes), true
}

func policyTemplateNames(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(policySourceDir)
	if err != nil {
		t.Fatalf("read policy source dir %s: %v", policySourceDir, err)
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") || e.Name() == "README.md" {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return names
}

// TestEmbeddedDefaultsMatchPolicySource is the drift guard.
//
// go:embed cannot reach outside its own package directory and refuses to follow
// symlinks ("cannot embed irregular file"), so the templates must physically
// exist twice. Nothing in the build enforces that the two copies agree, and
// historically they did not: ${GH_AUTH} reached src/policies in May 2026 but
// only partially reached the embedded tree, leaving 13 templates shipping
// without GitHub auth instructions and 14 without knowledge priming for months.
//
// That matters because the embedded copy is a FALLBACK, and "fallback" is only
// a meaningful word if it behaves the same. When the copies diverge, whether an
// agent receives a given instruction depends on which resolution path its
// deployment happened to take — an invisible behavioural fork that only shows
// up as two hives handling the same issue differently.
func TestEmbeddedDefaultsMatchPolicySource(t *testing.T) {
	names := policyTemplateNames(t)
	if len(names) == 0 {
		t.Fatalf("no policy templates found in %s", policySourceDir)
	}

	seen := make(map[string]bool, len(names))
	for _, name := range names {
		seen[name] = true
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		reason, exempt := knownDivergentPolicies[name]
		switch {
		case source == embedded && exempt:
			// Reconciled since the exemption was recorded: drop the entry so the
			// allowlist keeps shrinking and cannot quietly become permanent.
			t.Errorf("%s is now identical in both trees — remove it from knownDivergentPolicies (recorded reason: %s)", name, reason)
		case source != embedded && !exempt:
			t.Errorf("%s differs between src/policies and the embedded defaults.\n"+
				"Both copies ship: src/policies is read from disk by policies.path deployments, the embedded copy is the no-config fallback.\n"+
				"Apply the change to BOTH, or record a reviewed exception in knownDivergentPolicies with the reason.", name)
		}
	}

	// An entry naming a template that no longer exists is stale bookkeeping that
	// would mask a future divergence under the same filename.
	for name, reason := range knownDivergentPolicies {
		if !seen[name] {
			t.Errorf("knownDivergentPolicies names %s, which is not a policy template (reason: %s)", name, reason)
		}
	}
}

// TestEmbeddedDefaultsCoverPolicySource pins the file SETS together. Byte
// equality above says nothing about a template that exists in only one tree: a
// new role added to src/policies alone would ship with no embedded fallback and
// silently fall back to nothing.
func TestEmbeddedDefaultsCoverPolicySource(t *testing.T) {
	for _, name := range policyTemplateNames(t) {
		if _, err := DefaultPolicies.ReadFile("defaults/" + name); err != nil {
			t.Errorf("%s exists in src/policies but has no embedded counterpart: %v", name, err)
		}
	}

	entries, err := DefaultPolicies.ReadDir("defaults")
	if err != nil {
		t.Fatalf("read embedded defaults dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		if _, err := os.Stat(filepath.Join(policySourceDir, e.Name())); err != nil {
			t.Errorf("%s is embedded but absent from src/policies: %v", e.Name(), err)
		}
	}
}

// TestPolicyGuardrailsPresent pins individual safety rules to the tree that
// currently carries them.
//
// This exists because byte-equality alone is not enough. Reconciling the
// remaining divergent files by copying one tree over the other passes every
// other test in this package while silently deleting real safeguards — verified
// by doing exactly that: a wholesale src->embed copy removed the
// rejected_duplicate guard from both embedded templates and dropped a
// hive-open-pr reference, and the suite stayed green.
//
// Each entry names a rule whose loss changes agent behaviour in production.
func TestPolicyGuardrailsPresent(t *testing.T) {
	cases := []struct {
		name     string
		file     string
		fragment string
		why      string
	}{
		{
			name:     "scanner-issues keeps the rejected-duplicate guard",
			file:     "scanner-issues.md",
			fragment: "rejected_duplicate",
			why:      "stops agents re-filing findings a maintainer already closed as not-planned or duplicate",
		},
		{
			name:     "scanner-holdgated keeps the rejected-duplicate guard",
			file:     "scanner-holdgated.md",
			fragment: "rejected_duplicate",
			why:      "stops agents re-filing findings a maintainer already closed as not-planned or duplicate",
		},
		{
			name:     "scanner-automerge requires hive-open-pr",
			file:     "scanner-automerge.md",
			fragment: "hive-open-pr",
			why:      "PRs must be opened by the App via hive-open-pr, not raw gh or the GitHub MCP create_pull_request",
		},
		{
			name:     "brainstorm-advisory forbids setup during capture",
			file:     "brainstorm-advisory.md",
			fragment: "DO NOT clone repos",
			why:      "capture phase must not clone or run init commands",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			embedded, err := DefaultPolicies.ReadFile("defaults/" + tc.file)
			if err != nil {
				t.Fatalf("read embedded %s: %v", tc.file, err)
			}
			source, err := os.ReadFile(filepath.Join(policySourceDir, tc.file))
			if err != nil {
				t.Fatalf("read source %s: %v", tc.file, err)
			}
			if !strings.Contains(string(embedded), tc.fragment) && !strings.Contains(string(source), tc.fragment) {
				t.Errorf("guardrail %q is gone from BOTH copies of %s — %s", tc.fragment, tc.file, tc.why)
			}
		})
	}
}

// TestPromptVariableParityForSyncedTemplates pins the specific regression that
// motivated this file: a template carrying ${GH_AUTH} or ${KNOWLEDGE} in one
// tree but not the other. The scheduler builds one variable map per kick for
// every agent (pkg/scheduler/scheduler.go), so these substitute in any
// template — a missing occurrence is a silently weaker prompt, not an error.
func TestPromptVariableParityForSyncedTemplates(t *testing.T) {
	for _, name := range policyTemplateNames(t) {
		if _, exempt := knownDivergentPolicies[name]; exempt {
			continue
		}
		source, embedded, ok := readPolicyPair(t, name)
		if !ok {
			continue
		}
		for _, v := range []string{"${GH_AUTH}", "${KNOWLEDGE}"} {
			if strings.Contains(source, v) != strings.Contains(embedded, v) {
				t.Errorf("%s: %s present in only one copy (source=%t embedded=%t)",
					name, v, strings.Contains(source, v), strings.Contains(embedded, v))
			}
		}
	}
}
