package settingsparity

import (
	"bufio"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Leaf is one operator-facing hive.yaml setting reachable from config.Config.
type Leaf struct {
	Path string
	Type string
}

// structDecl is one collected struct with the import table of the file that
// declared it, so a qualified field type (writeback.Config, theme.Overrides)
// resolves to the package it names rather than to whichever collected
// package happens to declare a struct with the same bare name.
type structDecl struct {
	st      *ast.StructType
	pkgName string
	imports map[string]string // local name (alias or package name) -> import path
}

// collected is the struct index built from the config package dirs.
type collected struct {
	module string                 // module path of src/go.mod ("" when absent)
	byName map[string]*structDecl // bare type name (last declaration wins)
	byPkg  map[string]*structDecl // "<pkg name>.<Type>"
	byDir  map[string]*structDecl // "<dir relative to src>.<Type>"
}

// ConfigLeaves parses pkg/config (and the stdlib-only crux/config leaf it
// aliases) and returns dotted YAML leaf paths rooted at Config.
func ConfigLeaves(repoRoot string) ([]Leaf, error) {
	fset := token.NewFileSet()
	c := &collected{
		module: modulePath(filepath.Join(repoRoot, "src", "go.mod")),
		byName: map[string]*structDecl{},
		byPkg:  map[string]*structDecl{},
		byDir:  map[string]*structDecl{},
	}
	for _, rel := range []string{
		filepath.Join("pkg", "config"),
		filepath.Join("crux", "config"),
		filepath.Join("pkg", "dashboard", "theme"),
		filepath.Join("pkg", "writeback"),
	} {
		if err := c.collect(fset, repoRoot, rel); err != nil {
			return nil, err
		}
	}
	var leaves []Leaf
	seenStack := map[string]bool{}
	// imports is the import table of the file declaring the struct whose
	// fields are being walked; qualified field types resolve against it.
	var imports map[string]string
	var walk func(ast.Expr, string)
	var walkCollection func(ast.Expr, string)
	var walkStruct func(string, *structDecl, string)

	walkStruct = func(typeName string, decl *structDecl, prefix string) {
		if typeName != "" {
			key := typeName + "\x00" + prefix
			if seenStack[key] {
				leaves = append(leaves, Leaf{Path: prefix, Type: typeName})
				return
			}
			seenStack[key] = true
			defer delete(seenStack, key)
			prev := imports
			imports = decl.imports
			defer func() { imports = prev }()
		}
		for _, field := range decl.st.Fields.List {
			if yamlInline(field) {
				// Embedded structs tagged `yaml:",inline"` flatten into the
				// parent; inline maps are catch-alls with no fixed leaves.
				if c.inlineStruct(field.Type, imports) {
					walk(field.Type, prefix)
				}
				continue
			}
			tagName, ok := yamlName(field)
			if !ok {
				continue
			}
			path := tagName
			if prefix != "" {
				path = prefix + "." + tagName
			}
			walk(field.Type, path)
		}
	}
	// walkNamed resolves an identifier or qualified type to a collected
	// struct, walking it under prefix, or records a leaf.
	walkNamed := func(expr ast.Expr, prefix string) {
		if decl, name, ok := c.resolve(expr, imports); ok {
			walkStruct(name, decl, prefix)
			return
		}
		leaves = append(leaves, Leaf{Path: prefix, Type: exprString(expr)})
	}
	walk = func(expr ast.Expr, prefix string) {
		switch t := expr.(type) {
		case *ast.Ident, *ast.SelectorExpr:
			walkNamed(expr, prefix)
		case *ast.StarExpr:
			walk(t.X, prefix)
		case *ast.ArrayType:
			walkCollection(t.Elt, prefix+"[]")
		case *ast.MapType:
			walkCollection(t.Value, prefix+".<key>")
		case *ast.StructType:
			walkStruct("", &structDecl{st: t, imports: imports}, prefix)
		default:
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		}
	}
	walkCollection = func(expr ast.Expr, prefix string) {
		switch t := expr.(type) {
		case *ast.Ident, *ast.SelectorExpr:
			walkNamed(expr, prefix)
		case *ast.StarExpr:
			walkCollection(t.X, prefix)
		case *ast.StructType:
			walkStruct("", &structDecl{st: t, imports: imports}, prefix)
		default:
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		}
	}
	root, ok := c.byPkg["config.Config"]
	if !ok {
		root = c.byName["Config"]
	}
	if root != nil {
		walkStruct("Config", root, "")
	}
	leaves = append(leaves, Leaf{Path: "agents.github_api_hourly_cap", Type: "int"})
	leaves = append(leaves,
		Leaf{Path: "agents.session_recycle.enabled", Type: "*bool"},
		Leaf{Path: "agents.session_recycle.max_rss_mb", Type: "int"},
		Leaf{Path: "agents.session_recycle.max_age", Type: "time.Duration"},
		Leaf{Path: "agents.session_recycle.min_idle", Type: "time.Duration"},
	)
	byPath := map[string]Leaf{}
	for _, leaf := range leaves {
		if leaf.Path == "" || strings.Contains(leaf.Path, ".-") {
			continue
		}
		byPath[leaf.Path] = leaf
	}
	leaves = leaves[:0]
	for _, leaf := range byPath {
		leaves = append(leaves, leaf)
	}
	sort.Slice(leaves, func(i, j int) bool { return leaves[i].Path < leaves[j].Path })
	return leaves, nil
}

// modulePath returns the module line of the go.mod at path, or "".
func modulePath(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "module "); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}

// resolve maps an identifier or qualified type expression to a collected
// struct. A bare identifier matches by name across every collected package
// (pkg/config re-exports crux/config types through aliases). A qualified
// type matches only the package it names: by import path when the declaring
// file imports it, else by package name; it never falls back to a bare-name
// match, so writeback.Config can not resolve to config.Config.
func (c *collected) resolve(expr ast.Expr, imports map[string]string) (*structDecl, string, bool) {
	switch t := expr.(type) {
	case *ast.Ident:
		decl, ok := c.byName[t.Name]
		return decl, t.Name, ok
	case *ast.SelectorExpr:
		qual, ok := t.X.(*ast.Ident)
		if !ok {
			return nil, "", false
		}
		name := qual.Name + "." + t.Sel.Name
		if path, ok := imports[qual.Name]; ok {
			if c.module != "" {
				if rel, ok := strings.CutPrefix(path, c.module+"/"); ok {
					decl, ok := c.byDir[filepath.ToSlash(rel)+"."+t.Sel.Name]
					return decl, name, ok
				}
			}
			return nil, "", false
		}
		decl, ok := c.byPkg[name]
		return decl, name, ok
	}
	return nil, "", false
}

// collect indexes the structs under src/<rel>. Only pkg/config must exist;
// the packages it reaches into are skipped when absent (test fixtures).
func (c *collected) collect(fset *token.FileSet, repoRoot, rel string) error {
	dir := filepath.Join(repoRoot, "src", rel)
	if _, err := os.Stat(dir); err != nil && rel != filepath.Join("pkg", "config") {
		return nil
	}
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
		}
		imports := map[string]string{}
		for _, imp := range file.Imports {
			ipath := strings.Trim(imp.Path.Value, "\"")
			local := ipath[strings.LastIndex(ipath, "/")+1:]
			if imp.Name != nil {
				local = imp.Name.Name
			}
			imports[local] = ipath
		}
		for _, decl := range file.Decls {
			gd, ok := decl.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, spec := range gd.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok {
					continue
				}
				st, ok := ts.Type.(*ast.StructType)
				if !ok {
					continue
				}
				sd := &structDecl{st: st, pkgName: file.Name.Name, imports: imports}
				c.byName[ts.Name.Name] = sd
				c.byPkg[file.Name.Name+"."+ts.Name.Name] = sd
				c.byDir[filepath.ToSlash(rel)+"."+ts.Name.Name] = sd
			}
		}
		return nil
	})
}

func (c *collected) inlineStruct(expr ast.Expr, imports map[string]string) bool {
	switch t := expr.(type) {
	case *ast.Ident, *ast.SelectorExpr:
		_, _, ok := c.resolve(expr, imports)
		return ok
	case *ast.StarExpr:
		return c.inlineStruct(t.X, imports)
	case *ast.StructType:
		return true
	}
	return false
}

func yamlInline(f *ast.Field) bool {
	if f.Tag == nil {
		return false
	}
	tag := strings.Trim(f.Tag.Value, "`")
	for _, part := range strings.Split(tag, " ") {
		if !strings.HasPrefix(part, "yaml:\"") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(part, "yaml:\""), "\"")
		for _, opt := range strings.Split(v, ",")[1:] {
			if opt == "inline" {
				return true
			}
		}
	}
	return false
}

func yamlName(f *ast.Field) (string, bool) {
	if f.Tag == nil {
		return "", false
	}
	tag := strings.Trim(f.Tag.Value, "`")
	for _, part := range strings.Split(tag, " ") {
		if !strings.HasPrefix(part, "yaml:\"") {
			continue
		}
		v := strings.TrimPrefix(part, "yaml:\"")
		v = strings.TrimSuffix(v, "\"")
		name := strings.Split(v, ",")[0]
		if name == "" || name == "-" {
			return "", false
		}
		return name, true
	}
	return "", false
}

func exprString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.SelectorExpr:
		return exprString(t.X) + "." + t.Sel.Name
	case *ast.StarExpr:
		return "*" + exprString(t.X)
	case *ast.ArrayType:
		return "[]" + exprString(t.Elt)
	case *ast.MapType:
		return "map[" + exprString(t.Key) + "]" + exprString(t.Value)
	default:
		return "unknown"
	}
}

// Allowlist reads one path per non-comment line. Inline comments are allowed.
func Allowlist(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]string{}
	s := bufio.NewScanner(f)
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path := strings.TrimSpace(strings.SplitN(line, "#", 2)[0])
		out[path] = line
	}
	return out, s.Err()
}
