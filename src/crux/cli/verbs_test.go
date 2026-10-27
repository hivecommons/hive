package cli

import (
	"go/ast"
	"go/parser"
	"go/token"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
)

// nonRuntimeVerbs are dispatch cases that do not operate on a runtime: serve
// exposes one, help prints usage.
var nonRuntimeVerbs = map[string]bool{"serve": true, "help": true, "-h": true, "--help": true}

func TestVerbsHaveRuntimeMethods(t *testing.T) {
	rt := reflect.TypeOf((*crux.Runtime)(nil)).Elem()
	for verb, methods := range RuntimeVerbs {
		if len(methods) == 0 {
			t.Errorf("verb %q lists no runtime methods", verb)
		}
		for _, m := range methods {
			if _, ok := rt.MethodByName(m); !ok {
				t.Errorf("verb %q calls %s, which is not a crux.Runtime method", verb, m)
			}
		}
	}
}

func TestEveryDispatchedVerbIsListed(t *testing.T) {
	verbs := dispatchVerbs(t)
	for _, v := range verbs {
		if nonRuntimeVerbs[v] {
			continue
		}
		if _, ok := RuntimeVerbs[v]; !ok {
			t.Errorf("dispatch handles %q but RuntimeVerbs does not list it", v)
		}
	}
	seen := map[string]bool{}
	for _, v := range verbs {
		seen[v] = true
	}
	for v := range RuntimeVerbs {
		if !seen[v] {
			t.Errorf("RuntimeVerbs lists %q but dispatch does not handle it", v)
		}
	}
}

// TestVerbsMatchCommandBodies checks the table against the code: the rt.<M>
// selectors in each cmd<Verb> function are exactly the methods listed.
func TestVerbsMatchCommandBodies(t *testing.T) {
	fset := token.NewFileSet()
	// Every (*Env) method body, so a cmd<Verb> that delegates to a helper
	// (run -> stream) is credited with the helper's runtime calls.
	bodies := map[string]*ast.BlockStmt{}
	for _, file := range []string{"commands.go", "control.go"} {
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv != nil && fn.Body != nil {
				bodies[fn.Name.Name] = fn.Body
			}
		}
	}
	var collect func(body *ast.BlockStmt, into map[string]bool, seen map[string]bool)
	collect = func(body *ast.BlockStmt, into map[string]bool, seen map[string]bool) {
		ast.Inspect(body, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			id, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			switch {
			case id.Name == "rt":
				into[sel.Sel.Name] = true
			case id.Name == "e" && bodies[sel.Sel.Name] != nil && !seen[sel.Sel.Name]:
				seen[sel.Sel.Name] = true
				collect(bodies[sel.Sel.Name], into, seen)
			}
			return true
		})
	}
	calls := map[string]map[string]bool{}
	for name, body := range bodies {
		if !strings.HasPrefix(name, "cmd") {
			continue
		}
		verb := strings.ToLower(strings.TrimPrefix(name, "cmd"))
		calls[verb] = map[string]bool{}
		collect(body, calls[verb], map[string]bool{name: true})
	}
	for verb, methods := range RuntimeVerbs {
		got := sortedKeys(calls[verb])
		want := append([]string(nil), methods...)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("verb %q: code calls rt.%v, RuntimeVerbs lists %v", verb, got, want)
		}
	}
}

func sortedKeys(m map[string]bool) []string {
	// Methods the Process slice helper exposes are not runtime methods; a
	// local named rt of another type would show up here and should be
	// renamed rather than filtered.
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dispatchVerbs reads the string literals of the case clauses in
// (*Env).dispatch, so the test tracks the real switch rather than a copy.
func dispatchVerbs(t *testing.T) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "cli.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "dispatch" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			cc, ok := n.(*ast.CaseClause)
			if !ok {
				return true
			}
			for _, e := range cc.List {
				lit, ok := e.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Fatal(err)
				}
				verbs = append(verbs, v)
			}
			return false
		})
	}
	if len(verbs) == 0 {
		t.Fatal("found no case clauses in (*Env).dispatch")
	}
	return verbs
}
