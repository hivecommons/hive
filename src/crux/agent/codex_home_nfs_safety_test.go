package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// #5379 — NFS-safety structural pin
// ---------------------------------------------------------------------------
//
// WHY THIS TEST IS STRUCTURAL RATHER THAN BEHAVIOURAL.
//
// The bug it guards is invisible to every filesystem CI can mount. The heal
// used os.RemoveAll, which works perfectly on tmpfs, ext4, overlayfs and APFS
// — so a behavioural unit test passed for months while the product stayed
// broken. /data on hosted spokes is an NFSv3 PVC, and there os.RemoveAll
// descends with openat-based directory file descriptors that fail
// "openfdat ...: permission denied" even as root, leaving a renamed agent's
// codex backend dead until someone shells into the pod.
//
// Reproducing that requires a real NFSv3 export, which this suite does not
// have. Rather than write a test that merely proves the new code path runs
// (the failure shape of #5360 and #5370), this pins the MECHANISM: the codex
// home heal must reach the filesystem through an exec of chown/rm, never
// through Go's own tree walkers. If a future refactor "simplifies" it back to
// os.RemoveAll or filepath.WalkDir, this fails immediately and loudly instead
// of shipping a silent regression that only hosted spokes discover.
//
// This test does not skip under any condition. See #5380.

// codexHealMechanismFuncs are the declarations that perform, or decide on, the
// filesystem repair of a wrong-owner CODEX_HOME.
var codexHealMechanismFuncs = map[string]bool{
	"healCodexHomeOwnership": true,
	"chownCodexHomeToAgent":  true,
	"chownTreeAsRoot":        true,
	"removeTreeAsRoot":       true,
	"healForeignCodexConfig": true,
}

// goTreeWalkers are Go stdlib calls that descend a directory tree using
// openat-based directory file descriptors, which NFSv3 does not reliably
// support. None of them may appear in the codex home heal.
var goTreeWalkers = map[string]string{
	"os.RemoveAll":     "descends with openat dirfds; fails on the NFSv3 /data PVC (#5379)",
	"filepath.Walk":    "walks with openat dirfds; use an exec'd chown/rm instead (#5379)",
	"filepath.WalkDir": "walks with openat dirfds; use an exec'd chown/rm instead (#5379)",
	"os.ReadDir":       "opens a dirfd to enumerate; use an exec'd chown/rm instead (#5379)",
	"ioutil.ReadDir":   "opens a dirfd to enumerate; use an exec'd chown/rm instead (#5379)",
}

// TestCodexHomeHealUsesNFSSafeMechanisms parses manager.go and asserts that no
// function responsible for repairing a wrong-owner CODEX_HOME calls a Go
// stdlib tree walker.
func TestCodexHomeHealUsesNFSSafeMechanisms(t *testing.T) {
	const src = "manager_homes.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", src, err)
	}

	seen := map[string]bool{}
	for _, decl := range file.Decls {
		name, body := codexHealDeclBody(decl)
		if body == nil || !codexHealMechanismFuncs[name] {
			continue
		}
		seen[name] = true
		ast.Inspect(body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			qualified, ok := qualifiedCallName(call.Fun)
			if !ok {
				return true
			}
			if why, banned := goTreeWalkers[qualified]; banned {
				t.Errorf("%s calls %s at %s: %s\n"+
					"The codex home heal must reach the filesystem via an exec'd "+
					"chown/rm (su-exec), not a Go tree walk.",
					name, qualified, fset.Position(call.Pos()), why)
			}
			return true
		})
	}

	// Guard the guard: if a function is renamed away, this test would silently
	// stop checking anything.
	for name := range codexHealMechanismFuncs {
		if !seen[name] {
			t.Errorf("expected to find %s in %s — was it renamed? "+
				"Update codexHealMechanismFuncs so this pin keeps covering the heal.", name, src)
		}
	}
}

// TestCodexHomeHealShellsOutToChownAndRm is the positive half of the pin: the
// NFS-safe mechanisms must actually be exec'd, and the chown must use -h so it
// re-owns the auth.json SYMLINK itself rather than following it into the
// shared credential file.
func TestCodexHomeHealShellsOutToChownAndRm(t *testing.T) {
	const src = "manager_homes.go"
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, src, nil, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", src, err)
	}

	var chownArgs, rmArgs []string
	for _, decl := range file.Decls {
		name, body := codexHealDeclBody(decl)
		if body == nil {
			continue
		}
		switch name {
		case "chownTreeAsRoot":
			chownArgs = execCommandStringArgs(body)
		case "removeTreeAsRoot":
			rmArgs = execCommandStringArgs(body)
		}
	}

	if len(chownArgs) == 0 {
		t.Fatal("chownTreeAsRoot must exec a command; found no exec.Command call")
	}
	if len(rmArgs) == 0 {
		t.Fatal("removeTreeAsRoot must exec a command; found no exec.Command call")
	}

	joinedChown := strings.Join(chownArgs, " ")
	if !strings.Contains(joinedChown, "su-exec") {
		t.Errorf("chown must go through su-exec like the rest of setupCodexHome, got %q", joinedChown)
	}
	if !strings.Contains(joinedChown, "chown") {
		t.Errorf("chownTreeAsRoot must exec chown, got %q", joinedChown)
	}
	// -h / --no-dereference: auth.json is a symlink to the SHARED credential
	// file. Following it would rewrite ownership of every agent's login.
	if !strings.Contains(joinedChown, "-Rh") && !strings.Contains(joinedChown, "--no-dereference") {
		t.Errorf("chown must pass -h so it does not follow the shared auth.json symlink, got %q", joinedChown)
	}
	if !strings.Contains(joinedChown, "-R") {
		t.Errorf("chown must be recursive to re-own the whole tree, got %q", joinedChown)
	}

	joinedRm := strings.Join(rmArgs, " ")
	if !strings.Contains(joinedRm, "su-exec") {
		t.Errorf("rm must go through su-exec like the rest of setupCodexHome, got %q", joinedRm)
	}
	if !strings.Contains(joinedRm, "rm") || !strings.Contains(joinedRm, "-rf") {
		t.Errorf("removeTreeAsRoot must exec `rm -rf` (the mechanism verified on the affected NFS mount), got %q", joinedRm)
	}
}

// codexHealDeclBody returns the name and body of a decl that is either a
// func declaration or a `var name = func(...){...}` assignment.
func codexHealDeclBody(decl ast.Decl) (string, ast.Node) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		if d.Body == nil {
			return "", nil
		}
		return d.Name.Name, d.Body
	case *ast.GenDecl:
		if d.Tok != token.VAR {
			return "", nil
		}
		for _, spec := range d.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok || len(vs.Names) != 1 || len(vs.Values) != 1 {
				continue
			}
			lit, ok := vs.Values[0].(*ast.FuncLit)
			if !ok {
				continue
			}
			return vs.Names[0].Name, lit.Body
		}
	}
	return "", nil
}

// qualifiedCallName renders a call target as "pkg.Func" for selector calls.
func qualifiedCallName(fun ast.Expr) (string, bool) {
	sel, ok := fun.(*ast.SelectorExpr)
	if !ok {
		return "", false
	}
	ident, ok := sel.X.(*ast.Ident)
	if !ok {
		return "", false
	}
	return ident.Name + "." + sel.Sel.Name, true
}

// execCommandStringArgs collects the string literal arguments of any
// exec.Command call inside body.
func execCommandStringArgs(body ast.Node) []string {
	var args []string
	ast.Inspect(body, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if name, ok := qualifiedCallName(call.Fun); !ok || name != "exec.Command" {
			return true
		}
		for _, arg := range call.Args {
			if lit, ok := arg.(*ast.BasicLit); ok && lit.Kind == token.STRING {
				args = append(args, strings.Trim(lit.Value, `"`))
			}
		}
		return true
	})
	return args
}
