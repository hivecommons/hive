package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/hub/spoke"
)

// The hub provisions the terminal signing key and the spoke self-derives it.
// Both sides feed the SAME domain-separation label into the same HMAC, so if
// the two labels ever differ the keys stop agreeing and every terminal
// assertion fails to verify — at runtime, on a live hive, not here.
//
// #6719: the hub side had been re-declared as a string literal. The values
// still matched, so nothing was broken; the invariant was simply no longer
// enforced by anything but coincidence. These tests enforce it.
func TestHubTerminalConstantsAliasTheSpokeDefinitions(t *testing.T) {
	if infoTerminalKey != spoke.InfoTerminalKey {
		t.Errorf("infoTerminalKey = %q, want spoke.InfoTerminalKey (%q)", infoTerminalKey, spoke.InfoTerminalKey)
	}
	if EnvTerminalKey != spoke.EnvTerminalKey {
		t.Errorf("EnvTerminalKey = %q, want spoke.EnvTerminalKey (%q)", EnvTerminalKey, spoke.EnvTerminalKey)
	}
}

// Equality alone cannot catch a regression: someone re-pasting the literal
// leaves the values equal and the test above green, which is precisely the
// state #6719 found. So assert the *link* rather than the value — these
// constants must be declared by referring to the spoke package, never by a
// literal.
func TestTerminalConstantsAreDeclaredAsAliasesNotLiterals(t *testing.T) {
	const file = "spoke_deleted_support.go"

	fset := token.NewFileSet()
	parsed, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}

	want := map[string]string{
		"infoTerminalKey": "spoke.InfoTerminalKey",
		"EnvTerminalKey":  "spoke.EnvTerminalKey",
	}
	found := map[string]bool{}

	ast.Inspect(parsed, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			wantExpr, tracked := want[name.Name]
			if !tracked || i >= len(spec.Values) {
				continue
			}
			found[name.Name] = true

			if lit, isLit := spec.Values[i].(*ast.BasicLit); isLit {
				t.Errorf("%s is declared as the literal %s — it must alias %s so the "+
					"two sides cannot drift (#6719)", name.Name, lit.Value, wantExpr)
				continue
			}

			var got strings.Builder
			if sel, isSel := spec.Values[i].(*ast.SelectorExpr); isSel {
				if pkg, isIdent := sel.X.(*ast.Ident); isIdent {
					got.WriteString(pkg.Name + "." + sel.Sel.Name)
				}
			}
			if got.String() != wantExpr {
				t.Errorf("%s is declared as %q, want %q", name.Name, got.String(), wantExpr)
			}
		}
		return true
	})

	for name := range want {
		if !found[name] {
			t.Errorf("%s declares no constant named %s — if it moved, move this guard with it", file, name)
		}
	}
}
