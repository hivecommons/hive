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

// ConfigLeaves parses pkg/config and returns dotted YAML leaf paths rooted at Config.
func ConfigLeaves(repoRoot string) ([]Leaf, error) {
	fset := token.NewFileSet()
	structs := map[string]*ast.StructType{}
	for _, dir := range []string{
		filepath.Join(repoRoot, "src", "pkg", "config"),
		filepath.Join(repoRoot, "src", "pkg", "dashboard", "theme"),
	} {
		if err := collectStructs(fset, dir, structs); err != nil {
			return nil, err
		}
	}
	var leaves []Leaf
	seenStack := map[string]bool{}
	var walk func(ast.Expr, string)
	var walkCollection func(ast.Expr, string)
	var walkStruct func(string, *ast.StructType, string)

	walkStruct = func(typeName string, st *ast.StructType, prefix string) {
		if typeName != "" {
			key := typeName + "\x00" + prefix
			if seenStack[key] {
				leaves = append(leaves, Leaf{Path: prefix, Type: typeName})
				return
			}
			seenStack[key] = true
			defer delete(seenStack, key)
		}
		for _, field := range st.Fields.List {
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
	walk = func(expr ast.Expr, prefix string) {
		switch t := expr.(type) {
		case *ast.Ident:
			if st, ok := structs[t.Name]; ok {
				walkStruct(t.Name, st, prefix)
				return
			}
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		case *ast.StarExpr:
			walk(t.X, prefix)
		case *ast.ArrayType:
			walkCollection(t.Elt, prefix+"[]")
		case *ast.MapType:
			walkCollection(t.Value, prefix+".<key>")
		case *ast.StructType:
			walkStruct("", t, prefix)
		case *ast.SelectorExpr:
			if st, ok := structs[t.Sel.Name]; ok {
				walkStruct(t.Sel.Name, st, prefix)
				return
			}
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		default:
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		}
	}
	walkCollection = func(expr ast.Expr, prefix string) {
		switch t := expr.(type) {
		case *ast.Ident:
			if st, ok := structs[t.Name]; ok {
				walkStruct(t.Name, st, prefix)
				return
			}
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		case *ast.StarExpr:
			walkCollection(t.X, prefix)
		case *ast.StructType:
			walkStruct("", t, prefix)
		case *ast.SelectorExpr:
			if st, ok := structs[t.Sel.Name]; ok {
				walkStruct(t.Sel.Name, st, prefix)
				return
			}
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		default:
			leaves = append(leaves, Leaf{Path: prefix, Type: exprString(t)})
		}
	}
	walk(structs["Config"], "")
	leaves = append(leaves, Leaf{Path: "agents.github_api_hourly_cap", Type: "int"})
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

func collectStructs(fset *token.FileSet, dir string, structs map[string]*ast.StructType) error {
	return filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		file, err := parser.ParseFile(fset, path, nil, parser.ParseComments)
		if err != nil {
			return err
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
				if st, ok := ts.Type.(*ast.StructType); ok {
					structs[ts.Name.Name] = st
				}
			}
		}
		return nil
	})
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
