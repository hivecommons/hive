package testutil

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// This file is the architecture ratchet for the Crux boundary
// (hivecommons/hive#13536, src/docs/design/crux.md). Crux is crux/agent and
// its ring, the nested Go module at src/crux (before the module cut,
// pkg/agent and pkg/crux); everything else in Hive is a consumer and should
// reach it only through the exported runtime surface. Two numbers measure how far the code
// is from that today, and neither may grow:
//
//  1. cruxSymbolBaseline — per importing package, the number of DISTINCT
//     `agent.<Symbol>` selectors referenced from Go files (tests included:
//     a test that builds an agent.Manager to drive a dashboard handler is
//     coupling the runtime surface has to serve too) outside the Crux
//     module. A new symbol is new coupling; the way to add behaviour is to
//     widen crux.Runtime (or an equivalent exported seam in the crux
//     package), not to reach for another internal.
//  2. cruxTmuxBaseline — per package, the number of NON-TEST Go files outside
//     the Crux module that invoke tmux: a string literal exactly "tmux"
//     (exec.Command("tmux", ...), exec.LookPath("tmux"), or a constant that
//     feeds one). Every production tmux call belongs behind one package
//     inside Crux; hermetic tests that drive a real tmux are not the target.
//
// Both are ratchets, not targets, and both are exact: a count above its
// baseline fails, and so does a count BELOW it. When you remove a symbol or a
// tmux call site, lower the baseline in the same change (the failure names the
// new value); never raise one. Holding the baseline at the measured count is
// what stops removed coupling from quietly growing back into the headroom. Packages are keyed by their path relative to src/ (e.g. "pkg/dashboard",
// "cmd/hive"); a package with no entry has a baseline of zero, so coupling in
// a brand-new consumer fails closed.
//
// Re-measured on v5 when the baselines were made exact, with exactly the
// AST walk below (so a re-measure is `go test ./internal/testutil -run
// TestCruxBoundaryRatchet -v`, not a grep). pkg/dashboard went 68 → 51 when
// it moved onto crux.Runtime and the crux value aliases (#13536 Stage 1,
// item 3); what remains there is audit, trailer, provider-status, Copilot
// token and agent-mode helpers plus the test-only manager constructors.
// Provider-status and backend-auth types then moved to pkg/crux/types
// (re-exported by pkg/crux, aliased by pkg/agent): hub 18 → 1, spoke 15 → 5,
// dashboard 51 → 39, cmd/hive 37 → 33. The dashboard's audit, lease-stage,
// commit-trailer and artifact-link references then moved to pkg/agentaudit,
// the governor-side leaf they were always aliases of: dashboard 39 → 19.
// The remaining runtime value types (ProcessState, Kick*, RestartEvent,
// SubAgentModel, StartFailureClass, TurnLoss, KickRecord) then moved to
// pkg/crux/types too: cmd/hive 33 → 22, loginscan 7 → 4, spoke 5 → 3. What
// is left everywhere is the manager and its constructors (Manager,
// NewManager, ProjectContext), agent-mode and capability parsing, UID maps,
// and pane classification — implementation, not observation.
//
// Since the module cut (Stage 2 step 4) the ring lives in the nested module
// at src/crux and this walk covers the root module only (src/pkg, src/cmd,
// src/internal): the consumers. pkg/proxy's 14 references moved with it to
// crux/proxy, an intra-module import the module boundary now owns, so its
// row left the table without any consumer's count changing.
var cruxSymbolBaseline = map[string]int{
	"cmd/hive":      22,
	"pkg/dashboard": 19,
	"pkg/discord":   1,
	"pkg/hub":       1,
	"pkg/hub/spoke": 3,
	"pkg/ioscan":    5,
	"pkg/loginscan": 4,
}

// cruxTmuxBaseline is the per-package count of non-test files that invoke
// tmux outside the Crux module (see above). The RFC's larger "dashboard 17,
// hub 5" figures counted files that MENTION tmux anywhere (comments, JSON
// field names, user-facing messages); this ratchet counts invocations, which
// is what a funnel has to absorb. Likewise the RFC's "dashboard 109 symbols"
// was a grep that also matched local variables named agent; the AST walk
// here counts only selectors on the pkg/agent import.
//
// Empty since the tmux funnel landed: pkg/agent/tmux is the only package
// outside pkg/agent's manager that names the binary, and it is inside the
// Crux tree the walk skips. Any new entry here is a regression.
var cruxTmuxBaseline = map[string]int{}

const (
	cruxAgentImportPath = "github.com/hivecommons/hive/crux/agent"
	cruxTmuxLiteral     = "tmux"
)

// cruxBoundaryRoots are the trees the ratchet walks, relative to this
// package's directory (src/internal/testutil): the root module's packages,
// which are Crux's consumers. src/crux is the nested Crux module and is not
// walked (an import of crux/agent from inside the runtime is not coupling to
// measure). src/test is the integration suite and is excluded for the same
// reason sleep_ratchet_test.go excludes it.
var cruxBoundaryRoots = []string{
	filepath.Join("..", "..", "pkg"),
	filepath.Join("..", "..", "cmd"),
	filepath.Join("..", "..", "internal"),
}

// cruxInsidePrefixes are the Crux tree itself, relative to src/. Files under
// them are the thing being protected, not consumers, and are skipped. Since
// the module cut this is the nested module's directory; it is outside
// cruxBoundaryRoots anyway, and the prefix stays so a root that gains the
// tree (or a stray copy) is still skipped rather than counted.
var cruxInsidePrefixes = []string{"crux"}

// cruxInside reports whether rel (a src/-relative directory) is one of
// cruxInsidePrefixes or nested under one.
func cruxInside(rel string) bool {
	for _, p := range cruxInsidePrefixes {
		if rel == p || strings.HasPrefix(rel, p+"/") {
			return true
		}
	}
	return false
}

type cruxMeasurement struct {
	// symbols maps package → distinct agent.<Symbol> names referenced.
	symbols map[string]map[string]bool
	// tmuxFiles maps package → files that invoke tmux.
	tmuxFiles map[string][]string
	files     int
}

// TestCruxBoundaryRatchet fails when either coupling count differs from its
// recorded baseline: above it names the offending symbols or files, below it
// names the baseline to lower. It is a parse
// of every non-test Go file under src/pkg, src/cmd and src/internal with no
// type-checking, so it runs in a second or two and is fully deterministic.
func TestCruxBoundaryRatchet(t *testing.T) {
	m := measureCruxBoundary(t)
	if m.files == 0 {
		t.Fatalf("no non-test .go files found under %v — is the test running from src/internal/testutil?", cruxBoundaryRoots)
	}

	var failures []string

	for _, pkg := range sortedSetKeys(mapKeysOfSetsAsSet(m.symbols)) {
		t.Logf("agent.<Symbol> references: %-16s %3d", pkg, len(m.symbols[pkg]))
	}
	for _, pkg := range sortedSetKeys(mapKeysOfSlicesAsSet(m.tmuxFiles)) {
		t.Logf("tmux-invoking files:       %-16s %3d", pkg, len(m.tmuxFiles[pkg]))
	}

	for _, pkg := range sortedKeysUnion(cruxSymbolBaseline, mapKeysOfSetsAsSet(m.symbols)) {
		got := len(m.symbols[pkg])
		want := cruxSymbolBaseline[pkg]
		switch {
		case got > want:
			failures = append(failures, fmt.Sprintf(
				"%s references %d distinct agent.<Symbol> identifiers, above its baseline of %d:\n%s",
				pkg, got, want, indentList(sortedSetKeys(m.symbols[pkg]))))
		case got < want:
			failures = append(failures, cruxStaleBaseline("cruxSymbolBaseline", pkg, want, got))
		}
	}

	for _, pkg := range sortedKeysUnion(cruxTmuxBaseline, mapKeysOfSlicesAsSet(m.tmuxFiles)) {
		files := m.tmuxFiles[pkg]
		got := len(files)
		want := cruxTmuxBaseline[pkg]
		switch {
		case got > want:
			sort.Strings(files)
			failures = append(failures, fmt.Sprintf(
				"%s has %d non-test files invoking tmux, above its baseline of %d:\n%s",
				pkg, got, want, indentList(files)))
		case got < want:
			failures = append(failures, cruxStaleBaseline("cruxTmuxBaseline", pkg, want, got))
		}
	}

	if len(failures) > 0 {
		t.Fatalf("Crux boundary ratchet (hivecommons/hive#13536, src/docs/design/crux.md):\n\n%s\n"+
			"New agent.<Symbol> references belong on the exported runtime surface (the crux package); new tmux calls belong "+
			"inside crux/agent/... Remove the coupling, or move an existing reference out so the count does not grow. "+
			"A baseline above the measured count must be lowered to it in the same change.",
			strings.Join(failures, "\n\n"))
	}
}

// cruxStaleBaseline is the failure for a baseline above the measured count.
// The ratchet fails closed on it so headroom left by removed coupling cannot
// be silently re-used by a later change.
func cruxStaleBaseline(table, pkg string, want, got int) string {
	return fmt.Sprintf("%s[%q] is %d but the measured count is %d: lower it to %d in "+
		"internal/testutil/crux_boundary_ratchet_test.go (delete the entry when it reaches 0)",
		table, pkg, want, got, got)
}

// measureCruxBoundary parses every non-test Go file under cruxBoundaryRoots
// (skipping the Crux tree and testdata) and records, per package, the distinct
// agent.<Symbol> selectors and the files that invoke tmux.
func measureCruxBoundary(t *testing.T) cruxMeasurement {
	t.Helper()
	m := cruxMeasurement{
		symbols:   map[string]map[string]bool{},
		tmuxFiles: map[string][]string{},
	}
	fset := token.NewFileSet()
	for _, root := range cruxBoundaryRoots {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			rel := cruxRelPath(root, path)
			if d.IsDir() {
				if d.Name() == "testdata" || cruxInside(rel) {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(d.Name(), ".go") {
				return nil
			}
			m.files++
			pkg := filepath.ToSlash(filepath.Dir(rel))
			file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if err != nil {
				return fmt.Errorf("parsing %s: %w", path, err)
			}
			if alias := cruxAgentAlias(file); alias != "" {
				set := m.symbols[pkg]
				if set == nil {
					set = map[string]bool{}
					m.symbols[pkg] = set
				}
				for sym := range cruxSelectorsOf(file, alias) {
					set[sym] = true
				}
			}
			if !strings.HasSuffix(d.Name(), "_test.go") && cruxInvokesTmux(file) {
				m.tmuxFiles[pkg] = append(m.tmuxFiles[pkg], filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	return m
}

// cruxRelPath returns path relative to src/, given that root is one of
// cruxBoundaryRoots (../../<tree>).
func cruxRelPath(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return filepath.ToSlash(filepath.Join(filepath.Base(root), rel))
}

// cruxAgentAlias returns the local name under which file imports crux/agent,
// or "" when it does not. A dot or blank import contributes no selectors and
// is reported as "" too; neither occurs today.
func cruxAgentAlias(file *ast.File) string {
	for _, spec := range file.Imports {
		p, err := strconv.Unquote(spec.Path.Value)
		if err != nil || p != cruxAgentImportPath {
			continue
		}
		if spec.Name == nil {
			return "agent"
		}
		if spec.Name.Name == "." || spec.Name.Name == "_" {
			return ""
		}
		return spec.Name.Name
	}
	return ""
}

// cruxSelectorsOf collects the distinct <alias>.<Sel> identifiers in file.
// Without type information a local variable shadowing the alias would be
// miscounted; the package is never shadowed in practice and a false positive
// only makes the ratchet stricter.
func cruxSelectorsOf(file *ast.File, alias string) map[string]bool {
	out := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == alias {
			out[sel.Sel.Name] = true
		}
		return true
	})
	return out
}

// cruxInvokesTmux reports whether file contains a string literal whose value
// is exactly "tmux" — the argv[0] of every exec.Command / exec.LookPath call
// that drives tmux, whether written inline or hoisted into a constant.
// Comments and prose mentioning tmux do not count.
func cruxInvokesTmux(file *ast.File) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		if found {
			return false
		}
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		if v, err := strconv.Unquote(lit.Value); err == nil && v == cruxTmuxLiteral {
			found = true
		}
		return true
	})
	return found
}

func mapKeysOfSetsAsSet(m map[string]map[string]bool) map[string]bool {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return set
}

func mapKeysOfSlicesAsSet(m map[string][]string) map[string]bool {
	set := make(map[string]bool, len(m))
	for k := range m {
		set[k] = true
	}
	return set
}

// sortedKeysUnion returns the sorted union of the baseline's packages and
// the measured ones, so a package that appears on either side is checked.
func sortedKeysUnion(baseline map[string]int, measured map[string]bool) []string {
	set := make(map[string]bool, len(baseline)+len(measured))
	for k := range baseline {
		set[k] = true
	}
	for k := range measured {
		set[k] = true
	}
	return sortedSetKeys(set)
}

func sortedSetKeys(set map[string]bool) []string {
	keys := make([]string, 0, len(set))
	for k := range set {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func indentList(items []string) string {
	var b strings.Builder
	for _, it := range items {
		fmt.Fprintf(&b, "    %s\n", it)
	}
	return b.String()
}

// TestCruxBoundaryDetectors pins the two detectors on small sources so a
// refactor of the walk cannot silently stop counting.
func TestCruxBoundaryDetectors(t *testing.T) {
	parse := func(src string) *ast.File {
		t.Helper()
		f, err := parser.ParseFile(token.NewFileSet(), "x.go", src, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		return f
	}

	aliased := parse(`package x
import ag "github.com/hivecommons/hive/crux/agent"
var _ = ag.Manager{}
func f(a *ag.AgentProcess) { _ = a; _ = ag.StateRunning; _ = ag.Manager{} }
`)
	if got := cruxAgentAlias(aliased); got != "ag" {
		t.Fatalf("alias = %q, want ag", got)
	}
	if got := sortedSetKeys(cruxSelectorsOf(aliased, "ag")); strings.Join(got, ",") != "AgentProcess,Manager,StateRunning" {
		t.Fatalf("selectors = %v", got)
	}

	plain := parse(`package x
import "github.com/hivecommons/hive/crux/agent"
var _ = agent.Manager{}
`)
	if got := cruxAgentAlias(plain); got != "agent" {
		t.Fatalf("alias = %q, want agent", got)
	}
	if got := cruxAgentAlias(parse(`package x
import "os"
var _ = os.Args
`)); got != "" {
		t.Fatalf("alias for non-importer = %q, want empty", got)
	}

	if !cruxInvokesTmux(parse(`package x
import "os/exec"
var _ = exec.Command("tmux", "ls")
`)) {
		t.Fatal("exec.Command(\"tmux\") not detected")
	}
	if !cruxInvokesTmux(parse(`package x
const bin = "tmux"
`)) {
		t.Fatal("hoisted \"tmux\" constant not detected")
	}
	if cruxInvokesTmux(parse(`package x
// tmux is mentioned in a comment and in prose only.
var msg = "tmux session missing"
`)) {
		t.Fatal("prose mention of tmux must not count as an invocation")
	}

	if got := cruxStaleBaseline("cruxSymbolBaseline", "pkg/hub", 18, 17); !strings.Contains(got, `cruxSymbolBaseline["pkg/hub"] is 18`) || !strings.Contains(got, "lower it to 17") {
		t.Fatalf("stale-baseline message = %q", got)
	}
}
