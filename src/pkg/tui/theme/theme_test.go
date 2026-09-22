package theme

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"

	"github.com/charmbracelet/lipgloss"
)

// TestThemeTokensDefineBothBackgrounds pins that every token is adaptive in
// fact and not only in type.
//
// An AdaptiveColor with an empty half is not a compile error and not a panic:
// lipgloss resolves "" to no color, so the frame silently loses that element
// on exactly one of the two backgrounds — the failure T25 exists to fix,
// reintroduced by a typo.
//
// It reads the SOURCE rather than a hand-written list of tokens, so a token
// added later is covered without anyone remembering to add it here. (It did
// this by reflecting over a Theme struct while the palette lived in package
// tui; the tokens are package-level vars now that panes/ imports them too, and
// Go reflection cannot enumerate those. Parsing keeps the property that
// mattered — nobody has to maintain the list.)
func TestThemeTokensDefineBothBackgrounds(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "theme.go", nil, 0)
	if err != nil {
		t.Fatalf("parse theme.go: %v", err)
	}

	seen := 0
	ast.Inspect(file, func(n ast.Node) bool {
		spec, ok := n.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for i, name := range spec.Names {
			if i >= len(spec.Values) || !name.IsExported() {
				continue
			}
			lit, ok := spec.Values[i].(*ast.CompositeLit)
			if !ok {
				continue
			}
			sel, ok := lit.Type.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "AdaptiveColor" {
				t.Errorf("theme.%s is not a lipgloss.AdaptiveColor — every token must be "+
					"defined for both backgrounds", name.Name)
				continue
			}
			seen++
			halves := map[string]string{}
			for _, e := range lit.Elts {
				kv, ok := e.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				key, _ := kv.Key.(*ast.Ident)
				val, _ := kv.Value.(*ast.BasicLit)
				if key != nil && val != nil {
					halves[key.Name] = strings.Trim(val.Value, `"`)
				}
			}
			for _, half := range []string{"Light", "Dark"} {
				if halves[half] == "" {
					t.Errorf("theme.%s has no %s half — it renders as no color on that background",
						name.Name, half)
				}
			}
		}
		return true
	})

	if seen == 0 {
		t.Fatal("no tokens found in theme.go — this test is scanning nothing")
	}
}

// TestTokensAreDistinctPerBackground guards the pairing itself. A token whose
// two halves are equal is adaptive in type but not in effect: it was written
// for one background and copied to the other, which is exactly the state T25
// found the focus border in.
func TestTokensAreDistinctPerBackground(t *testing.T) {
	for name, c := range map[string]lipgloss.AdaptiveColor{
		"Border":      Border,
		"BorderFocus": BorderFocus,
		"Text":        Text,
		"Muted":       Muted,
		"Accent":      Accent,
		"Success":     Success,
		"Warning":     Warning,
		"Danger":      Danger,
		"Selected":    Selected,
	} {
		if c.Light == c.Dark {
			t.Errorf("theme.%s uses %q on both backgrounds — one of them was not chosen for its own background",
				name, c.Light)
		}
	}
}
