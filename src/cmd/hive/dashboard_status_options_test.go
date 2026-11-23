package main

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"reflect"
	"strings"
	"testing"
)

// The dashboard computes the Overview card on every read from the rows being
// served, so the two publishers of the status snapshot (the eval cycle and
// the refresh rebuild) give the same card only if they build the rows the
// same way. This pins that both BuildFrontendStatus calls pass the same
// options; the receiver they read them from (dashSrv, cfg, ghClient or the
// rebuilder's fields) is not part of the comparison.
func TestBuildFrontendStatusCallersPassTheSameOptions(t *testing.T) {
	eval := buildFrontendStatusOptions(t, "main_eval_cycle.go")
	refresh := buildFrontendStatusOptions(t, "main.go")
	if len(eval) == 0 {
		t.Fatal("main_eval_cycle.go: no dashboard.BuildFrontendStatus call with options found")
	}
	if !reflect.DeepEqual(eval, refresh) {
		t.Fatalf("BuildFrontendStatus options differ:\n  main_eval_cycle.go: %v\n  main.go:            %v", eval, refresh)
	}
}

// buildFrontendStatusOptions returns the options (the arguments after the
// ten positional ones) of the one dashboard.BuildFrontendStatus call in
// file, with every "b." receiver prefix dropped.
func buildFrontendStatusOptions(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "BuildFrontendStatus" {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != "dashboard" {
			return true
		}
		var opts []string
		for i, arg := range call.Args {
			if i < 10 {
				continue
			}
			var b strings.Builder
			if err := printer.Fprint(&b, fset, arg); err != nil {
				t.Fatal(err)
			}
			opts = append(opts, strings.ReplaceAll(b.String(), "b.", ""))
		}
		calls = append(calls, opts)
		return true
	})
	if len(calls) != 1 {
		t.Fatalf("%s: %d dashboard.BuildFrontendStatus calls, want 1", file, len(calls))
	}
	return calls[0]
}
