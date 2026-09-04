package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// envVarsDocPath is the hand-maintained environment variable reference this
// guard polices. It lives under src/, which matters: v2-tests.yml gates PRs on
// `paths: ['src/**', ...]`, so both this test and the file it checks are inside
// the filter. The #5077 post-mortem (#5388) found two guards that could never
// run because the artifact they checked sat outside their workflow's path
// filter — dashboard/openapi.json and the repo-root NOTICE. This one does not
// have that hole, and an env-vars.md-only PR does run this test.
const envVarsDocPath = "../../docs/env-vars.md"

// envVarsDocScanRoots are the trees searched for evidence that a documented
// variable is real. Env vars in this repo are consumed from Go, from the
// deployment/entrypoint shell, from the Justfile, and from config templates, so
// all of them count as an implementation. Paths are relative to this package
// (src/pkg/config).
var envVarsDocScanRoots = []string{
	"../..", // the whole src/ tree: Go, src/deploy/entrypoint.sh, manifests
	"../../../bin",
	"../../../config",
	"../../../dashboard",
	"../../../Justfile",
	"../../../install.sh",
	"../../../uninstall.sh",
}

// TestEnvVarsDocDocumentsOnlyRealVariables is the #5407 guard.
//
// src/docs/env-vars.md is a hand-compiled reference whose own header says "the
// code is authoritative". Nothing kept it honest. That is the same shape of
// artifact as dashboard/openapi.json before #5077: a published reference
// consumers are told to trust, with no mechanical tie to the thing it
// describes. In #5077 the spec had drifted into documenting fields the server
// never sent, and three downstream client tasks carried the acceptance
// criterion "mirror the spec, do not invent fields" — mirroring the drift would
// have produced a wrong client.
//
// DIRECTION, and why this one.
//
// This guard is deliberately one-directional: it forbids the reference from
// documenting a variable that nothing reads. It does NOT require the converse
// (every variable read anywhere is documented). That choice is forced by what
// the source actually looks like, not by convenience:
//
//  1. Env var names reach os.Getenv through at least four indirections that no
//     static extractor here can follow — package constants (EnvBucket,
//     envOCICompartmentID), struct fields resolved from config at runtime
//     (gw.APIKeyEnv, c.APIKeyEnv), injected getenv func(string) string
//     parameters (src/cmd/apiproxy/main.go), and local helpers that wrap the
//     lookup (getEnvOrDefault in src/pkg/hub/oci_fss.go). An AST pass over
//     call sites resolves ~105 names; grepping every ALLCAPS string literal in
//     src/ yields ~311 candidates, most of which are not env vars at all.
//     There is no rule separating the two sets that does not itself need
//     hand-maintenance — which is the very failure mode being fixed.
//  2. So a "must be documented" guard would open red against a set of names it
//     cannot enumerate correctly, and would demand a large permanent exception
//     list of things that merely LOOK like env vars. Per #5384's reasoning, a
//     guard that cannot be made green gets skipped or deleted, and then it
//     protects nothing.
//
// The direction kept is the one that is both decidable and the one that
// actually bit in #5077: a reference entry that corresponds to nothing real.
// Deciding it needs only "does this exact name appear anywhere in the
// implementation?", which is exact, cheap, and has no false positives that
// require excusing.
//
// The uncovered direction — a newly added env var silently going undocumented —
// is left to the "Keeping this reference current" section of env-vars.md and to
// review. This is a real, acknowledged gap, not an oversight.
func TestEnvVarsDocDocumentsOnlyRealVariables(t *testing.T) {
	documented, proposed := documentedEnvVars(t)
	if len(documented) == 0 {
		t.Fatalf("%s yielded no documented variable rows; the table format changed and this "+
			"guard is no longer reading it — fix the parser, do not delete the test",
			envVarsDocPath)
	}

	corpus := envVarNameCorpus(t)
	if len(corpus) == 0 {
		t.Fatalf("scanned %v and found no identifier tokens at all; the scan roots are wrong "+
			"and this guard would vacuously pass", envVarsDocScanRoots)
	}

	var problems []string
	for _, name := range documented {
		if corpus[name] {
			continue
		}
		if _, ok := proposed[name]; ok {
			// A row explicitly marked "proposed"/"not implemented" is an
			// honest reservation of a name, not a claim that the code reads
			// it. Documenting a name as unimplemented is the opposite of the
			// #5077 defect, so it is allowed — but only when the row says so
			// in the Required column, which is why this is derived from the
			// table rather than from a hand-kept list that could go stale.
			continue
		}
		problems = append(problems, name+" is documented in "+envVarsDocPath+
			" but the name appears nowhere in the implementation "+
			"(no Go source, entrypoint/helper shell, Justfile, or config template reads it) — "+
			"remove the row, correct a typo in the name, or, if the variable is planned but "+
			"not yet wired, mark its Required column \"proposed\" the way the Credly rows do")
	}

	// A row marked "proposed" that the code now DOES read is stale bookkeeping
	// pointing operators at a variable the reference still calls unimplemented.
	// Failing on it mirrors how openapi_route_parity_test.go fails on stale
	// exceptions rather than letting them rot into cover for real drift.
	for name := range proposed {
		if corpus[name] {
			problems = append(problems, name+" is marked \"proposed\"/not-implemented in "+
				envVarsDocPath+" but the implementation now references it — the feature shipped; "+
				"move the row into the section for the component that reads it and give it a "+
				"real Required/Default")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("%s has drifted from the implementation (%d documented, %d marked proposed, "+
			"%d problem(s)):\n  %s",
			envVarsDocPath, len(documented), len(proposed), len(problems),
			strings.Join(problems, "\n  "))
	}
}

// envVarDocRowPattern matches a markdown table row whose first cell is a
// backticked ALLCAPS identifier — the shape every variable row in env-vars.md
// uses. Prose mentions of a variable elsewhere in the file are deliberately not
// matched: only a table row is a claim that the variable exists.
var envVarDocRowPattern = regexp.MustCompile("^\\|\\s*`([A-Z][A-Z0-9_]{2,})`\\s*\\|(.*)$")

// proposedMarker identifies the Required-column text env-vars.md uses for a
// name that is reserved but not yet implemented.
const proposedMarker = "proposed"

// documentedEnvVars returns every variable named in a table row of
// env-vars.md, plus the subset whose Required column marks them as proposed
// (mapped to the raw Required cell, for failure messages).
func documentedEnvVars(t *testing.T) (all []string, proposed map[string]string) {
	t.Helper()

	raw, err := os.ReadFile(envVarsDocPath)
	if err != nil {
		t.Fatalf("reading %s: %v", envVarsDocPath, err)
	}
	proposed = map[string]string{}
	seen := map[string]bool{}
	for _, line := range strings.Split(string(raw), "\n") {
		m := envVarDocRowPattern.FindStringSubmatch(strings.TrimSpace(line))
		if m == nil {
			continue
		}
		name := m[1]
		if !seen[name] {
			seen[name] = true
			all = append(all, name)
		}
		// m[2] is the remainder of the row; its first cell is Required.
		required, _, _ := strings.Cut(m[2], "|")
		if strings.Contains(strings.ToLower(required), proposedMarker) {
			proposed[name] = strings.TrimSpace(required)
		}
	}
	sort.Strings(all)
	return all, proposed
}

// envVarNameCorpus returns the set of ALLCAPS identifier tokens appearing
// anywhere in the implementation trees. Membership is the test's definition of
// "this variable is real": it is intentionally permissive, because a false
// "real" only weakens the guard for one name, while a false "fictional" would
// fail a PR over a lookup this scanner could not follow. Markdown is excluded
// so documentation cannot vouch for itself.
func envVarNameCorpus(t *testing.T) map[string]bool {
	t.Helper()

	tokenPattern := regexp.MustCompile(`[A-Z][A-Z0-9_]{2,}`)
	corpus := map[string]bool{}
	for _, root := range envVarsDocScanRoots {
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				// A missing optional root (e.g. a trimmed checkout) must not
				// silently shrink the corpus into false failures.
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			if info.IsDir() {
				if info.Name() == ".git" || info.Name() == "node_modules" {
					return filepath.SkipDir
				}
				return nil
			}
			if strings.EqualFold(filepath.Ext(path), ".md") {
				return nil
			}
			if info.Size() > maxEnvCorpusFileBytes {
				return nil
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return nil
			}
			for _, tok := range tokenPattern.FindAllString(string(data), -1) {
				corpus[tok] = true
			}
			return nil
		})
		if err != nil {
			t.Fatalf("scanning %s for env var references: %v", root, err)
		}
	}
	return corpus
}

// maxEnvCorpusFileBytes skips vendored blobs and build artifacts that would
// slow the scan without containing env var lookups. It is generous enough to
// cover every hand-written source file in the repo, including
// src/cmd/hive/main.go.
const maxEnvCorpusFileBytes = 4 << 20 // 4 MiB
