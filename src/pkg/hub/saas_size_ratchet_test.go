package hub

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

const (
	maxSaaSFileLines   = 6000
	maxSaaSFileFuncs   = 50
	maxSaaSFileTypes   = 8
	saasFileNamePrefix = "saas"
	goFileNameSuffix   = ".go"
	testFileNameSuffix = "_test.go"
)

func TestSaaSFilesStaySplit(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read pkg/hub: %v", err)
	}

	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, saasFileNamePrefix) || !strings.HasSuffix(name, goFileNameSuffix) || strings.HasSuffix(name, testFileNameSuffix) {
			continue
		}

		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if lines := countLines(src); lines > maxSaaSFileLines {
			t.Fatalf("%s has %d lines; split new SaaS surface before exceeding %d", name, lines, maxSaaSFileLines)
		}

		funcs, types := countTopLevelSaaSSymbols(t, name, src)
		if funcs > maxSaaSFileFuncs {
			t.Fatalf("%s has %d top-level funcs; split new SaaS behavior before exceeding %d", name, funcs, maxSaaSFileFuncs)
		}
		if types > maxSaaSFileTypes {
			t.Fatalf("%s has %d top-level types; split new SaaS types before exceeding %d", name, types, maxSaaSFileTypes)
		}
	}
}

func countLines(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	lines := strings.Count(string(src), "\n")
	if src[len(src)-1] != '\n' {
		lines++
	}
	return lines
}

func countTopLevelSaaSSymbols(t *testing.T, name string, src []byte) (int, int) {
	t.Helper()
	file, err := parser.ParseFile(token.NewFileSet(), name, src, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", name, err)
	}
	var funcs, types int
	for _, decl := range file.Decls {
		switch d := decl.(type) {
		case *ast.FuncDecl:
			funcs++
		case *ast.GenDecl:
			if d.Tok == token.TYPE {
				types += len(d.Specs)
			}
		}
	}
	return funcs, types
}
