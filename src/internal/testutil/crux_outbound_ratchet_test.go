package testutil

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the OUTBOUND half of the Crux architecture ratchet
// (hivecommons/hive#13536, src/docs/design/crux.md). crux_boundary_ratchet_test.go
// measures how much of Hive reaches INTO the runtime; this one measures how
// much the runtime reaches OUT: for every package that Stage 2 moves into the
// nested `src/crux` module, the Hive packages outside that set it imports.
// A nested module can only be a module once this list is empty, so the list
// may shrink and never grow.
//
// cruxModulePrefixes is the package set the module will hold — pkg/agent and
// its ring, exactly the set .github/scripts/crux-scope.sh uses to skip the
// hub and dashboard shards on a crux-only PR, plus pkg/crux (the exported
// surface) and cmd/crux (the CLI). KEEP THE THREE IN SYNC. An import between
// two packages in the set is not an edge; an import of anything else under
// github.com/hivecommons/hive/ is. Third-party and stdlib imports are not
// measured.
//
// cruxOutboundBaseline is exact and fails closed in both directions, like the
// inbound tables: an edge not in the baseline fails the PR gate and names the
// importing files; an edge in the baseline that no longer exists fails too
// and names the entry to delete, so a removed dependency cannot grow back
// into stale headroom. Entries are "<dep>" when a non-test file imports it
// and "<dep> (test)" when only _test.go files do. Edges are removed in the
// same change that cuts them; they are never added. The way to give the
// runtime something from Hive is to move the value type into pkg/crux/types
// (stdlib-only; see PRResult, the audit vocabulary), invert it behind a
// Manager setter the host wires (SetSpanStarter, SetSandboxPRClient), or
// move the test to the consumer side (pkg/review's prompt trigger test).
//
// Measured on v5 when the ratchet landed, with exactly the parse below (so
// a re-measure is `go test ./internal/testutil -run TestCruxOutboundRatchet
// -v`). pkg/agent started at 12 outside packages plus 3 test-only; the same
// change cut pkg/github (PRResult and the token-cache path moved into
// pkg/crux/types; github aliases them), pkg/agentaudit (the runtime-emitted
// audit vocabulary, AuditSink and Fields moved into pkg/crux/types;
// agentaudit aliases them; pkg/toolapprove followed), pkg/tracing (the kick
// span goes through Manager.SetSpanStarter, which cmd/hive wires to
// tracing.StartSpan) and the test-only pkg/review edge (the prompt trigger
// test moved to pkg/review against crux.FatalNetworkErrorPatterns). The
// pkg/config edge (60 distinct symbols from pkg/agent: AgentConfig and the
// Bob*, Explain*, KickLogSource*, Sandbox* and backend vocabularies) was the
// first Stage 2 prerequisite: the runtime-relevant types moved into the
// stdlib-only pkg/crux/config leaf (pkg/config aliases them) and pkg/agent,
// pkg/crux and pkg/crux/cli import the leaf. pkg/crux/cli's hive.yaml and
// AgentDefinition reading moved behind cli.DefinitionLoader; the Hive-backed
// implementation lives in cmd/crux (the process boundary, where cmd/hive's
// equivalent wiring also lives), which is why cmd/crux now carries those two
// edges — they moved, they were not added. inference, proxy, toolapprove
// and watchdog followed: the LiteLLM route and gateway resolvers take the
// leaf's AgentRuntimeGovernorConfig, the origin-label vocabulary moved into
// the leaf, toolapprove.DeskFromConfig takes the ToolApprovalConfig block
// and reaches the ACMM-pack plan gates through Desk.SetPlanPolicy (cmd/hive
// wires config.PlanAutoApproveForLevel and PlanningConfig.PlanFromLabelEnabled
// and carries the parity tests), and the two tests that loaded a full
// hive.yaml moved to cmd/hive. No ring package imports pkg/config any more.
// The value-type edges went next (Stage 2 prerequisite 2): pkg/effects
// moved whole into pkg/crux/effects (stdlib-only; pkg/effects aliases every
// symbol), pkg/gitidentity moved to pkg/crux/gitidentity (its two callers
// repointed, nothing left to alias), the Jev environment contract and
// jev-decide skill moved to pkg/crux/jev (pkg/jev aliases them; the
// endpoint and CLI stay), the agent-report schema and validator moved to
// pkg/crux/outputschema with the receipt's provenance handle as
// pkg/crux/types.ProofProvenance (pkg/convergence/proof and
// pkg/outputschema alias; the report directory and path helpers stay host
// side), and pkg/jsonextract moved to pkg/crux/jsonextract (pkg/jsonextract
// aliases it), which also closed pkg/trajectory's only edge. The service
// edges followed: the runtime's self-reported error conditions go through
// pkg/crux/types.ConditionReporter (the seven ring-emitted Class* names live
// in types; fleetreport aliases them), wired by Manager.SetConditionReporter
// and watchdog.WithConditionReporter to fleetreport.DefaultReporter in
// cmd/hive; pkg/pushbroker's only Hive dependency, *github.AppAuth, became
// the pushbroker.TokenScoper interface (cmd/hive's appMinter adapts), and
// pushbroker joined the ring together with the stdlib-only pkg/watsonx,
// since both are runtime-side packages the ring already imported. What
// remains is toolapprove's ioscan call, the proxy's GitHub-side helpers and
// watchdog's rotation probe.
var cruxOutboundBaseline = map[string][]string{
	"cmd/crux": {
		"pkg/config",
		"pkg/defsrc",
	},
	"pkg/agent": {
		"internal/testutil (test)",
	},
	"pkg/inference": {
		"internal/testutil (test)",
	},
	"pkg/proxy": {
		"pkg/github",
		"pkg/inferencehealth",
		"pkg/ioscan",
		"pkg/issueshape",
		"pkg/tokens",
	},
	"pkg/toolapprove": {
		"pkg/ioscan",
	},
	"pkg/watchdog": {
		"pkg/rotation",
	},
}

// cruxModulePrefixes are the src/-relative package directories (and their
// subpackages) that Stage 2 moves into the nested module. See the file
// comment for the sync rule with .github/scripts/crux-scope.sh.
var cruxModulePrefixes = []string{
	"pkg/agent",
	"pkg/crux",
	"cmd/crux",
	"pkg/agentmode",
	"pkg/agentparse",
	"pkg/agentsmd",
	"pkg/claude",
	"pkg/sandbox",
	"pkg/kubejob",
	"pkg/toolapprove",
	"pkg/skillreg",
	"pkg/inference",
	"pkg/openrouter",
	"pkg/watchdog",
	"pkg/trajectory",
	"pkg/logscrub",
	"pkg/proxy",
	"pkg/pushbroker",
	"pkg/watsonx",
}

const cruxHiveModulePath = "github.com/hivecommons/hive/"

// cruxInModule reports whether rel (a src/-relative directory) is one of
// cruxModulePrefixes or nested under one.
func cruxInModule(rel string) bool {
	for _, p := range cruxModulePrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

// cruxOutboundEdge is one measured import of an outside Hive package.
type cruxOutboundEdge struct {
	nonTest bool
	files   []string
}

// TestCruxOutboundRatchet fails when the set of outside-Hive packages any
// module-set package imports differs from cruxOutboundBaseline: a new edge
// names the importing files; a vanished edge names the baseline entry to
// delete. It parses import declarations only, so it runs in well under a
// second and is fully deterministic.
func TestCruxOutboundRatchet(t *testing.T) {
	edges, files := measureCruxOutbound(t)
	if files == 0 {
		t.Fatalf("no .go files found under %v — is the test running from src/internal/testutil?", cruxModulePrefixes)
	}

	measured := map[string][]string{}
	for pkg, deps := range edges {
		for dep, e := range deps {
			entry := dep
			if !e.nonTest {
				entry += " (test)"
			}
			measured[pkg] = append(measured[pkg], entry)
		}
		sort.Strings(measured[pkg])
		t.Logf("outbound edges: %-18s %s", pkg, strings.Join(measured[pkg], ", "))
	}

	var failures []string
	for _, pkg := range sortedKeysUnion(cruxLenMap(cruxOutboundBaseline), cruxKeySet(measured)) {
		want := cruxSetOf(cruxOutboundBaseline[pkg])
		got := cruxSetOf(measured[pkg])
		for _, entry := range sortedSetKeys(got) {
			if want[entry] {
				continue
			}
			dep := strings.TrimSuffix(entry, " (test)")
			importers := edges[pkg][dep].files
			sort.Strings(importers)
			failures = append(failures, fmt.Sprintf(
				"%s imports %s, which is outside the Crux module set and not in cruxOutboundBaseline:\n%s",
				pkg, entry, indentList(importers)))
		}
		for _, entry := range sortedSetKeys(want) {
			if got[entry] {
				continue
			}
			failures = append(failures, fmt.Sprintf(
				"cruxOutboundBaseline[%q] lists %q but no file imports it any more: delete the entry in "+
					"internal/testutil/crux_outbound_ratchet_test.go (delete the package key when its list is empty)",
				pkg, entry))
		}
	}

	if len(failures) > 0 {
		t.Fatalf("Crux outbound ratchet (hivecommons/hive#13536, src/docs/design/crux.md):\n\n%s\n"+
			"A package that Stage 2 moves into the crux module may not grow a dependency on the rest of Hive. "+
			"Move the value type into pkg/crux/types, invert it behind a Manager setter the host wires, or move the "+
			"test to the consumer side; a baseline entry whose edge is gone must be deleted in the same change.",
			strings.Join(failures, "\n\n"))
	}
}

// measureCruxOutbound parses the import declarations of every Go file under
// cruxModulePrefixes (skipping testdata) and records, per package, every
// import of a Hive package outside the set, with the importing files and
// whether any of them is a non-test file.
func measureCruxOutbound(t *testing.T) (map[string]map[string]*cruxOutboundEdge, int) {
	t.Helper()
	edges := map[string]map[string]*cruxOutboundEdge{}
	files := 0
	fset := token.NewFileSet()
	srcRoot := filepath.Join("..", "..")
	for _, prefix := range cruxModulePrefixes {
		root := filepath.Join(srcRoot, filepath.FromSlash(prefix))
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			files++
			rel, err := filepath.Rel(srcRoot, path)
			if err != nil {
				return err
			}
			rel = filepath.ToSlash(rel)
			pkg := filepath.ToSlash(filepath.Dir(rel))
			file, err := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			isTest := strings.HasSuffix(d.Name(), "_test.go")
			for _, spec := range file.Imports {
				p, err := strconv.Unquote(spec.Path.Value)
				if err != nil || !strings.HasPrefix(p, cruxHiveModulePath) {
					continue
				}
				dep := strings.TrimPrefix(p, cruxHiveModulePath)
				if cruxInModule(dep) {
					continue
				}
				deps := edges[pkg]
				if deps == nil {
					deps = map[string]*cruxOutboundEdge{}
					edges[pkg] = deps
				}
				e := deps[dep]
				if e == nil {
					e = &cruxOutboundEdge{}
					deps[dep] = e
				}
				e.nonTest = e.nonTest || !isTest
				e.files = append(e.files, rel)
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	return edges, files
}

func cruxLenMap(m map[string][]string) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = len(v)
	}
	return out
}

func cruxKeySet(m map[string][]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}

func cruxSetOf(list []string) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, s := range list {
		out[s] = true
	}
	return out
}
