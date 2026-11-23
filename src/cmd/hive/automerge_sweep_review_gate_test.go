package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// hivecommons/hive#19421: the 1-minute automerge job built its engine with
// no review gate, so its trusted-author lane merged a head an enforcing
// governor.review_gate.mode (or review.require_approval) should have held.
// The job's options (built by governorAutoMergeOptions) must carry the same
// selfMergeReviewGate the ticker engine gets.
func TestRunAutoMergeSweepIfDueOptionsCarryTheReviewGate(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main_sweeps.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var fn *ast.FuncDecl
	for _, d := range file.Decls {
		if f, ok := d.(*ast.FuncDecl); ok && f.Name.Name == "governorAutoMergeOptions" {
			fn = f
		}
	}
	if fn == nil {
		t.Fatal("governorAutoMergeOptions not found")
	}
	found := false
	ast.Inspect(fn.Body, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		sel, ok := lit.Type.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Options" {
			return true
		}
		for _, el := range lit.Elts {
			kv, ok := el.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "ReviewGate" {
				continue
			}
			if call, ok := kv.Value.(*ast.CallExpr); ok {
				if name, ok := call.Fun.(*ast.Ident); ok && name.Name == "selfMergeReviewGate" {
					found = true
				}
			}
		}
		return true
	})
	if !found {
		t.Fatal("governorAutoMergeOptions' automerge.Options has no ReviewGate: selfMergeReviewGate(…)")
	}
}

// The gate that wiring installs reads review.require_approval live, which is
// what raises it to enforce on this engine as on the ticker's.
func TestSelfMergeReviewGateReadsRequireApproval(t *testing.T) {
	cfg := &config.Config{}
	gate := selfMergeReviewGate(cfg, nil, nil)
	if gate == nil || gate.RequireApproval == nil || gate.RequireApproval() {
		t.Fatalf("gate = %+v, want require_approval read as off", gate)
	}
	cfg.Review.RequireApproval = true
	if !gate.RequireApproval() {
		t.Fatal("a reload to review.require_approval: true is not seen by the gate")
	}
}
