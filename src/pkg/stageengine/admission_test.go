package stageengine

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestAdmissionOnlyStageWriterWritesStageLabels is the ADR-0025 admission
// test: pkg/github/stage_writer.go is the only code that puts a stage/*
// label on, or takes one off, a forge item. It walks every non-test Go
// file under src/ and fails when a label add/remove call reaches the
// forge with a stage label in hand — a `stage/` literal,
// `stagelabels.<Stage>.Label()`, the claims package's stage label
// constants or CoveredByPRLabel — either directly in an argument or
// built elsewhere in the same function; and when any type other than
// stageWriter implements stageengine.StageWriter.
//
// The allow-list below is the set of out-of-cycle writers plan PR-D
// (hivecommons/hive#13009) moves onto FactSink. PR-D empties it; a new
// entry is a design regression, not a fix.
func TestAdmissionOnlyStageWriterWritesStageLabels(t *testing.T) {
	// allowed are file:func sites (file relative to src/) that still name a
	// stage label to the forge. PR-D empties this list.
	allowed := map[string]string{
		"pkg/github/stage_labeler.go:ApplyStageLabel":                                 "claim-ledger entry point → FactSink (PR-D deletes it)",
		"pkg/github/stage_labeler.go:AddLabelsFresh":                                  "claim hook stage parameter (PR-D removes it)",
		"cmd/hive/claimswire.go:*":                                                    "ledger hooks → FactSink.Claimed/ClaimReleased (PR-D)",
		"pkg/github/issue_inprogress.go:*":                                            "sweep 040 swap → FactSink.CoveredBy (PR-D)",
		"pkg/github/issue_request_watcher.go:handleOneIssueRequest":                   "agent claim request → FactSink.InProgress (PR-D)",
		"pkg/github/prclaims.go:*":                                                    "040 add/remove → FactSink.CoveredBy/Uncovered (PR-D)",
		"pkg/dashboard/contribute_verdict_settle.go:*":                                "→ FactSink.CoveredBy (PR-D)",
		"pkg/dashboard/claims_api.go:removeLegacyClaimLabelForLease":                  "lease release → FactSink.ClaimReleased (PR-D)",
		"pkg/dashboard/contribute_claims.go:recordAgentClaim":                         "dashboard agent claim → FactSink.Claimed (PR-D)",
		"pkg/github/issue_needs_decision_migration.go:MigrateIssueNeedsDecisionStage": "one-shot 085→045 migration; retires with the legacy paths (PR-F)",
	}

	root := srcRoot(t)
	var violations []string
	used := map[string]bool{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "testdata", "node_modules", ".git", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		rel = filepath.ToSlash(rel)
		if rel == stageWriterFile || strings.HasPrefix(rel, "pkg/stagelabels/") || strings.HasPrefix(rel, "pkg/stageengine/") {
			// The writer, the planning library it wraps, and this package.
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		for _, v := range admissionViolations(f) {
			site := rel + ":" + v.fn
			if why, ok := allowed[site]; ok {
				used[site] = true
				_ = why
				continue
			}
			if why, ok := allowed[rel+":*"]; ok {
				used[rel+":*"] = true
				_ = why
				continue
			}
			violations = append(violations, fset.Position(v.pos).String()+" "+site+": "+v.what)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(violations)
	for _, v := range violations {
		t.Errorf("stage label reaches the forge outside %s: %s", stageWriterFile, v)
	}
	for site := range allowed {
		if !used[site] {
			t.Errorf("allow-list entry %q matched nothing: PR-D landed it, delete the entry", site)
		}
	}
}

const stageWriterFile = "pkg/github/stage_writer.go"

// labelMutators are the method names through which a label reaches the
// forge (go-github, hgithub.Client, forge.Forge, stagelabels.Labeler and
// the stagelabels enforcement entry points).
var labelMutators = map[string]bool{
	"AddLabelsToIssue": true, "RemoveLabelForIssue": true, "ReplaceLabelsForIssue": true, "RemoveLabelsForIssue": true,
	"AddLabels": true, "RemoveLabel": true, "RemoveLabels": true, "AddLabel": true,
	"AddLabelsFresh": true, "RemoveLabelsFresh": true, "addLabelsObserved": true, "removeLabelObserved": true,
	"EnsureLabel": true, "EnsureLabels": true, "SetLabels": true,
	"Enforce": true, "EnforceStage": true, "EnforceStageAdding": true, "ApplyStageLabel": true,
}

// stageLabelIdents are package-level identifiers whose value is a stage
// label (plan PR-D replaces the literals behind them).
var stageLabelIdents = map[string]bool{
	"LabelClaimed": true, "LabelPRInProgress": true, "CoveredByPRLabel": true, "claimStageFor": true,
}

// stageEntryPoints take a stagelabels.Stage and set it: any call is a stage
// write whatever the argument's spelling. AddLabelsFresh is one unless its
// stage argument is literally stagelabels.None.
var stageEntryPoints = map[string]bool{"EnforceStage": true, "EnforceStageAdding": true, "ApplyStageLabel": true}

func isStageWrite(call *ast.CallExpr) bool {
	sel := call.Fun.(*ast.SelectorExpr)
	if stageEntryPoints[sel.Sel.Name] {
		return true
	}
	if sel.Sel.Name == "AddLabelsFresh" && len(call.Args) >= 4 {
		if x, ok := call.Args[3].(*ast.SelectorExpr); ok {
			if id, ok := x.X.(*ast.Ident); ok && id.Name == "stagelabels" && x.Sel.Name == "None" {
				return false
			}
		}
		return true
	}
	return false
}

type admissionViolation struct {
	fn   string
	pos  token.Pos
	what string
}

// admissionViolations reports, per function in f: every label-mutator
// call with a stage label in an argument; every function that both builds
// a stage label and calls a mutator (the label travelled through a local);
// and every method set that implements StageWriter.
func admissionViolations(f *ast.File) []admissionViolation {
	var out []admissionViolation
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		name := fn.Name.Name
		if fn.Recv != nil && name == "Apply" && len(fn.Type.Params.List) == 4 && isStageWriterApply(fn.Type) {
			out = append(out, admissionViolation{fn: name, pos: fn.Pos(), what: "implements stageengine.StageWriter"})
		}
		var mutatorCalls []*ast.CallExpr
		stageSeen := false
		var stagePos token.Pos
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && labelMutators[sel.Sel.Name] {
					mutatorCalls = append(mutatorCalls, call)
				}
			}
			if !stageSeen {
				if p, ok := stageLabelExpr(n); ok {
					stageSeen, stagePos = true, p
				}
			}
			return true
		})
		for _, call := range mutatorCalls {
			direct := isStageWrite(call)
			for _, arg := range call.Args {
				ast.Inspect(arg, func(n ast.Node) bool {
					if _, ok := stageLabelExpr(n); ok {
						direct = true
					}
					return !direct
				})
			}
			if direct {
				out = append(out, admissionViolation{fn: name, pos: call.Pos(), what: "stage label passed to " + call.Fun.(*ast.SelectorExpr).Sel.Name})
			}
		}
		if stageSeen && len(mutatorCalls) > 0 && !hasDirect(out, name) {
			out = append(out, admissionViolation{fn: name, pos: stagePos, what: "builds a stage label and calls " + mutatorCalls[0].Fun.(*ast.SelectorExpr).Sel.Name})
		}
	}
	return out
}

func hasDirect(vs []admissionViolation, fn string) bool {
	for _, v := range vs {
		if v.fn == fn && strings.HasPrefix(v.what, "stage label passed") {
			return true
		}
	}
	return false
}

// stageLabelExpr reports whether n evaluates to a stage label: a "stage/"
// string literal, stagelabels.<X>.Label(), <X>.Label() on a stagelabels
// stage expression, or one of stageLabelIdents.
func stageLabelExpr(n ast.Node) (token.Pos, bool) {
	switch e := n.(type) {
	case *ast.BasicLit:
		if e.Kind == token.STRING && strings.HasPrefix(strings.Trim(e.Value, "\"`"), "stage/") {
			return e.Pos(), true
		}
	case *ast.CallExpr:
		sel, ok := e.Fun.(*ast.SelectorExpr)
		if !ok {
			return 0, false
		}
		if sel.Sel.Name == "Label" && len(e.Args) == 0 && mentionsStagelabels(sel.X) {
			return e.Pos(), true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == "claims" && stageLabelIdents[sel.Sel.Name] {
			return e.Pos(), true
		}
	case *ast.SelectorExpr:
		if id, ok := e.X.(*ast.Ident); ok && id.Name == "claims" && stageLabelIdents[e.Sel.Name] {
			return e.Pos(), true
		}
	case *ast.Ident:
		if stageLabelIdents[e.Name] {
			return e.Pos(), true
		}
	}
	return 0, false
}

// mentionsStagelabels reports whether x is rooted in the stagelabels
// package (stagelabels.Claimed, stagelabels.Stage(n), …) or is a stage
// value (an identifier named stage / ending in Stage, or a .Stage field).
func mentionsStagelabels(x ast.Expr) bool {
	switch e := x.(type) {
	case *ast.Ident:
		return e.Name == "stagelabels" || e.Name == "stage" || strings.HasSuffix(e.Name, "Stage")
	case *ast.SelectorExpr:
		if e.Sel.Name == "Stage" || strings.HasSuffix(e.Sel.Name, "Stage") {
			return true
		}
		return mentionsStagelabels(e.X)
	case *ast.CallExpr:
		return mentionsStagelabels(e.Fun)
	case *ast.ParenExpr:
		return mentionsStagelabels(e.X)
	case *ast.IndexExpr:
		return mentionsStagelabels(e.X)
	}
	return false
}

// isStageWriterApply matches Apply(ctx, Ref, []string, Desired) (Outcome, error).
func isStageWriterApply(t *ast.FuncType) bool {
	if t.Results == nil || len(t.Results.List) != 2 {
		return false
	}
	var types []string
	for _, p := range t.Params.List {
		types = append(types, exprString(p.Type))
	}
	return len(types) == 4 && strings.HasSuffix(types[1], "Ref") && types[2] == "[]string" && strings.HasSuffix(types[3], "Desired")
}

func exprString(e ast.Expr) string {
	switch x := e.(type) {
	case *ast.Ident:
		return x.Name
	case *ast.SelectorExpr:
		return exprString(x.X) + "." + x.Sel.Name
	case *ast.ArrayType:
		return "[]" + exprString(x.Elt)
	case *ast.StarExpr:
		return "*" + exprString(x.X)
	}
	return ""
}

// srcRoot finds the module root (the directory holding go.mod) from the
// test's working directory.
func srcRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found above " + dir)
		}
		dir = parent
	}
}

// TestAdmissionWalkerDetects pins the walker itself: each snippet is a
// way a stage label could reach the forge (or a second writer could
// appear) and must be reported; the clean snippet must not.
func TestAdmissionWalkerDetects(t *testing.T) {
	cases := map[string]struct {
		src  string
		want string // "" = clean
	}{
		"literal argument": {
			src:  `func f(ctx context.Context, c *C) { c.AddLabels(ctx, "o/r", 1, []string{"stage/020-claimed"}) }`,
			want: "stage label passed to AddLabels",
		},
		"Label() argument": {
			src:  `func f(ctx context.Context, c *C) { _ = c.RemoveLabel(ctx, "o/r", 1, stagelabels.Claimed.Label()) }`,
			want: "stage label passed to RemoveLabel",
		},
		"claims constant": {
			src:  `func f(ctx context.Context, c *C) { _ = c.RemoveLabel(ctx, "o/r", 1, claims.LabelClaimed) }`,
			want: "stage label passed to RemoveLabel",
		},
		"through a local": {
			src:  "func f(ctx context.Context, c *C) { ls := []string{stagelabels.Actionable.Label()}; _ = c.AddLabels(ctx, \"o/r\", 1, ls) }",
			want: "builds a stage label and calls AddLabels",
		},
		"stage entry point": {
			src:  `func f(ctx context.Context, c *C, s stagelabels.Stage) { _, _ = c.ApplyStageLabel(ctx, "o/r", 1, s) }`,
			want: "stage label passed to ApplyStageLabel",
		},
		"second writer": {
			src:  `func (w other) Apply(ctx context.Context, ref stageengine.Ref, hint []string, want stagelabels.Desired) (stagelabels.Outcome, error) { return stagelabels.Outcome{}, nil }`,
			want: "implements stageengine.StageWriter",
		},
		"non-stage label": {
			src:  `func f(ctx context.Context, c *C) { _ = c.AddLabelsFresh(ctx, "o/r", 1, stagelabels.None, "needs-human"); _ = c.RemoveLabel(ctx, "o/r", 1, "needs-human") }`,
			want: "",
		},
		"stage read only": {
			src:  `func f(ls []string) bool { return has(ls, stagelabels.Actionable.Label()) }`,
			want: "",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "x.go", "package x\n"+tc.src, 0)
			if err != nil {
				t.Fatal(err)
			}
			got := admissionViolations(f)
			if tc.want == "" {
				if len(got) != 0 {
					t.Fatalf("clean snippet reported %+v", got)
				}
				return
			}
			if len(got) != 1 || got[0].what != tc.want {
				t.Fatalf("got %+v, want one %q", got, tc.want)
			}
		})
	}
}
