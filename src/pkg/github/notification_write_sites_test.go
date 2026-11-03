package github

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// notificationWritePrimitives are the method names whose call sites must be
// inventoried in notificationWriteSites.
var notificationWritePrimitives = map[string]bool{
	"CreateIssueComment":         true,
	"CreateIssueCommentBudgeted": true,
	"PostNoteOrFold":             true,
	"PostClaimNote":              true,
	"CreateReview":               true,
	"RequestReviewers":           true,
	"noteCommentSuppressed":      true,
}

// TestNotificationWriteSitesInventory is the class-tag guard
// (hivecommons/hive#16264): every non-test function under src/pkg and
// src/cmd that calls a notification-capable write primitive must have an
// entry in notificationWriteSites, and every entry must still name such a
// function. Add the new producer to the inventory (and to the table in
// src/docs/notifications.md) with its class.
func TestNotificationWriteSitesInventory(t *testing.T) {
	found := map[string]string{}
	fset := token.NewFileSet()
	for _, root := range []string{"..", filepath.Join("..", "..", "cmd")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				switch d.Name() {
				case "testdata", "vendor", "node_modules":
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
			if perr != nil {
				return perr
			}
			for _, decl := range f.Decls {
				var name string
				var body ast.Node
				switch d := decl.(type) {
				case *ast.FuncDecl:
					if d.Body == nil {
						continue
					}
					name, body = f.Name.Name+"."+funcDeclProducerName(d), d.Body
				case *ast.GenDecl:
					name, body = f.Name.Name+".<package-level>", d
				}
				if body == nil {
					continue
				}
				ast.Inspect(body, func(n ast.Node) bool {
					call, ok := n.(*ast.CallExpr)
					if !ok {
						return true
					}
					sel, ok := call.Fun.(*ast.SelectorExpr)
					if ok && notificationWritePrimitives[sel.Sel.Name] {
						if _, dup := found[name]; !dup {
							found[name] = fset.Position(call.Pos()).String()
						}
					}
					return true
				})
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	if len(found) == 0 {
		t.Fatal("found no notification write call sites; is the walk rooted correctly?")
	}
	var missing, stale []string
	for name, pos := range found {
		if _, ok := notificationWriteSites[name]; !ok {
			missing = append(missing, name+" ("+pos+")")
		}
	}
	for name, site := range notificationWriteSites {
		if _, ok := found[name]; !ok {
			stale = append(stale, name)
		}
		switch site.Class {
		case NotificationClassStatus, NotificationClassAttention, NotificationClassRequiredReview,
			NotificationClassProtocolCommand, NotificationClassSecurityEvent:
		default:
			t.Errorf("%s: invalid class %q", name, site.Class)
		}
	}
	sort.Strings(missing)
	sort.Strings(stale)
	for _, m := range missing {
		t.Errorf("notification-capable write without a class tag: %s — add it to notificationWriteSites and src/docs/notifications.md", m)
	}
	for _, s := range stale {
		t.Errorf("notificationWriteSites entry %s no longer calls a write primitive; remove it", s)
	}
}

// funcDeclProducerName renders d the way runtime names it:
// "F", "T.M" or "(*T).M".
func funcDeclProducerName(d *ast.FuncDecl) string {
	if d.Recv == nil || len(d.Recv.List) == 0 {
		return d.Name.Name
	}
	typ := d.Recv.List[0].Type
	star := false
	if s, ok := typ.(*ast.StarExpr); ok {
		star, typ = true, s.X
	}
	switch g := typ.(type) {
	case *ast.IndexExpr:
		typ = g.X
	case *ast.IndexListExpr:
		typ = g.X
	}
	recv := "?"
	if id, ok := typ.(*ast.Ident); ok {
		recv = id.Name
	}
	if star {
		return "(*" + recv + ")." + d.Name.Name
	}
	return recv + "." + d.Name.Name
}

// Protocol commands, required reviews and security events are never
// would-suppress candidates.
func TestNotificationClassSuppressible(t *testing.T) {
	for class, want := range map[NotificationClass]bool{
		NotificationClassStatus:          true,
		NotificationClassAttention:       true,
		NotificationClassRequiredReview:  false,
		NotificationClassProtocolCommand: false,
		NotificationClassSecurityEvent:   false,
		NotificationClassUnclassified:    false,
	} {
		if got := class.Suppressible(); got != want {
			t.Errorf("%s.Suppressible() = %v, want %v", class, got, want)
		}
	}
}

func TestNotificationProducerName(t *testing.T) {
	for in, want := range map[string]string{
		"github.com/hivecommons/hive/pkg/github.(*Client).PostNoteOrFold":              "github.(*Client).PostNoteOrFold",
		"github.com/hivecommons/hive/pkg/github.(*Client).handleOneIssueRequest.func2": "github.(*Client).handleOneIssueRequest",
		"github.com/hivecommons/hive/pkg/github.runRecheckSoak.func1.3":                "github.runRecheckSoak",
		"main.githubClaimHooks.func4.1":                                                "main.githubClaimHooks",
		"main.liveTriageCommenter.CreateIssueComment":                                  "main.liveTriageCommenter.CreateIssueComment",
		"github.com/hivecommons/hive/pkg/x.F[...]":                                     "x.F",
	} {
		if got := notificationProducerName(in); got != want {
			t.Errorf("notificationProducerName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestExtractMentionLogins(t *testing.T) {
	got := extractMentionLogins("cc @Alice and @bob-2, again @alice; mail me at x@example.com; team @org/team\n@carol")
	want := []string{"alice", "bob-2", "org/team", "carol"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("mentions = %v, want %v", got, want)
	}
	if extractMentionLogins("no mentions here") != nil {
		t.Fatal("want no mentions")
	}
}
