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
// cruxModuleDir is the nested Crux module, src/crux, relative to this
// package's directory. Since the module cut (Stage 2 step 4) the module
// boundary is the ring: every package under src/crux is in the set and
// nothing else is. Before the cut this file carried cruxModulePrefixes, the
// src/-relative list that .github/scripts/crux-scope.sh and the v2-tests
// boundary comment mirrored; all three now name the one directory. An import
// between two packages in the module is not an edge; an import of anything
// else under github.com/hivecommons/hive/ is. Third-party and stdlib imports
// are not measured.
//
// cmd/crux is NOT in the module. It is the crux binary's process boundary
// and, like cmd/hive, a host: its only job besides signals and the exit code
// is the Hive-backed cli.DefinitionLoader (hive.yaml via pkg/config, the
// portable AgentDefinition via pkg/defsrc, Hive's per-agent defaults), which
// is host knowledge by definition and has no layer above main to be injected
// from. A nested module that held it would import pkg/config and so need the
// root module, the cycle Stage 2 exists to avoid; the binary therefore stays
// in the root module and builds against the nested one the way cmd/hive
// does. The inbound ratchet still measures it (baseline zero `agent.<Symbol>`
// references), so it can reach the runtime only through the crux package.
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
// since both are runtime-side packages the ring already imported. The
// sibling edges followed: the watchdog's RotationAuthProbe reads a
// two-method watchdog.UsageProber (cmd/hive adapts each rotation prober)
// and toolapprove's payload scanning goes through toolapprove.TextScanner,
// which pkg/ioscan implements (ToolApproveScanner) and cmd/hive and
// pkg/turn install at init; a desk with no text scanner fails closed.
// Prerequisite 3 started with the proxy's value-type edges: the
// inference-gateway health store moved whole into pkg/crux/inferencehealth
// and the issue/comment shape validators into pkg/crux/issueshape (both
// stdlib-only; pkg/inferencehealth and pkg/issueshape alias every symbol so
// hub, spoke, dashboard, github and classify keep their import path). Its
// three service edges were inverted behind seams in pkg/proxy/host_seams.go:
// proxy.CanaryScanner (pkg/ioscan's *CanaryRegistry implements it; the leak
// value is pkg/crux/types.CanaryLeak, which ioscan aliases),
// proxy.UsageSink (*tokens.InferenceSink implements it) and
// proxy.GitHubRESTAccounting (cmd/hive's githubRESTAccounting adapts
// pkg/github's package-level recorders and core-bucket snapshot; a proxy
// without one records nothing and the agent GitHub budget applies the
// per-agent cap alone). The proxy's tests drive the seams with in-package
// fakes. The two cmd/crux rows (pkg/config, pkg/defsrc: the DefinitionLoader
// implementation) left when cmd/crux left the set — see the file comment —
// which completed prerequisite 3: no ring package imports anything from
// Hive in non-test code. The last two rows were test-only: pkg/agent and
// pkg/inference tests used this package's Eventually* and
// Skip[f]UnlessRequired helpers. Those copied into the ring as
// pkg/crux/testutil (crux/internal/testutil after the module cut), so the
// baseline is empty: no ring package imports anything from Hive, in test
// code or otherwise. After step 4 the module boundary enforces the same
// thing (the nested module has no replace back to the root), and this test
// keeps measuring it from the root module's side.
var cruxOutboundBaseline = map[string][]string{}

// cruxModuleDir is the src/-relative directory of the nested Crux module;
// cruxModuleRoots is the same as a path from this package's directory. See
// the file comment for the sync rule with .github/scripts/crux-scope.sh.
const cruxModuleDir = "crux"

var cruxModuleRoots = []string{filepath.Join("..", "..", cruxModuleDir)}

const cruxHiveModulePath = "github.com/hivecommons/hive/"

// cruxInModule reports whether rel (a src/-relative directory, or an import
// path with cruxHiveModulePath trimmed) is inside the nested module.
func cruxInModule(rel string) bool {
	return rel == cruxModuleDir || strings.HasPrefix(rel, cruxModuleDir+"/")
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
		t.Fatalf("no .go files found under %v — is the test running from src/internal/testutil?", cruxModuleRoots)
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
			"A package in the crux module may not grow a dependency on the rest of Hive. "+
			"Move the value type into crux/types, invert it behind a Manager setter the host wires, or move the "+
			"test to the consumer side; a baseline entry whose edge is gone must be deleted in the same change.",
			strings.Join(failures, "\n\n"))
	}
}

// measureCruxOutbound parses the import declarations of every Go file under
// the nested module (skipping testdata) and records, per package, every
// import of a Hive package outside the module, with the importing files and
// whether any of them is a non-test file. The Go toolchain enforces the same
// thing at build time (the nested module has no replace back to the root,
// so such an import does not resolve); the parse here reports it by file
// and keeps the record in one place with the inbound ratchet.
func measureCruxOutbound(t *testing.T) (map[string]map[string]*cruxOutboundEdge, int) {
	t.Helper()
	edges := map[string]map[string]*cruxOutboundEdge{}
	files := 0
	fset := token.NewFileSet()
	srcRoot := filepath.Join("..", "..")
	for _, root := range cruxModuleRoots {
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
