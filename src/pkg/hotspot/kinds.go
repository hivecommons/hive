package hotspot

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"path"
	"regexp"
	"sort"
	"strings"
)

// Classify decides a file's Kind from its path and (when available) its
// first lines. Generated files also return the source tree that produces
// them, when known.
func Classify(p string, src []byte) (Kind, string) {
	base := path.Base(p)
	lower := strings.ToLower(p)
	ext := strings.ToLower(path.Ext(base))

	switch base {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock", "poetry.lock", "Gemfile.lock", "composer.lock":
		return KindLock, ""
	}
	if strings.HasSuffix(lower, ".pb.go") || strings.HasSuffix(lower, "_generated.go") || strings.HasSuffix(lower, ".gen.go") {
		return KindGenerated, ""
	}
	// hive's dashboard SPA: pkg/dashboard/static/** is `go generate`d from
	// pkg/dashboard/static-src/**.
	if i := strings.Index(p, "/static/"); i >= 0 && strings.Contains(p, "dashboard") {
		return KindGenerated, p[:i] + "/static-src/"
	}
	if strings.HasPrefix(p, "static/") && strings.Contains(p, "dashboard") {
		return KindGenerated, "static-src/"
	}
	if head := firstLines(src, 5); bytes.Contains(head, []byte("Code generated")) && bytes.Contains(head, []byte("DO NOT EDIT")) {
		return KindGenerated, generatedFromComment(head)
	}
	switch ext {
	case ".md", ".rst", ".txt", ".adoc":
		return KindDoc, ""
	case ".json", ".yaml", ".yml", ".toml", ".csv", ".txt.golden", ".golden":
		return KindData, ""
	}
	return KindCode, ""
}

func firstLines(src []byte, n int) []byte {
	idx := 0
	for i := 0; i < n && idx < len(src); i++ {
		j := bytes.IndexByte(src[idx:], '\n')
		if j < 0 {
			return src
		}
		idx += j + 1
	}
	return src[:idx]
}

var generatedByRe = regexp.MustCompile(`(?i)generated (?:by|from) ([^\s;.]+)`)

func generatedFromComment(head []byte) string {
	if m := generatedByRe.FindSubmatch(head); m != nil {
		return string(m[1])
	}
	return ""
}

// Groups lists the file's distinct top-level units: funcs and types for Go
// (methods are grouped under their receiver type), top-level declarations
// for JS/TS/Python, and `##` headings for Markdown. The result is sorted.
// It is nil when the source is unavailable or the language is unknown.
func Groups(p string, src []byte) []string {
	if len(src) == 0 {
		return nil
	}
	ext := strings.ToLower(path.Ext(p))
	var groups map[string]bool
	switch ext {
	case ".go":
		groups = goGroups(src)
	case ".js", ".mjs", ".cjs", ".ts", ".tsx", ".jsx":
		groups = regexGroups(src, jsDeclRe)
	case ".py":
		groups = regexGroups(src, pyDeclRe)
	case ".md", ".rst":
		groups = regexGroups(src, mdHeadingRe)
	default:
		return nil
	}
	if len(groups) == 0 {
		return nil
	}
	out := make([]string, 0, len(groups))
	for g := range groups {
		out = append(out, g)
	}
	sort.Strings(out)
	return out
}

func goGroups(src []byte) map[string]bool {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "", src, parser.SkipObjectResolution)
	if err != nil || f == nil {
		return nil
	}
	groups := map[string]bool{}
	for _, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv != nil && len(d.Recv.List) > 0 {
				if name := receiverName(d.Recv.List[0].Type); name != "" {
					groups[name] = true
					continue
				}
			}
			if d.Name != nil {
				groups[d.Name.Name] = true
			}
		case *ast.GenDecl:
			if d.Tok != token.TYPE {
				continue
			}
			for _, s := range d.Specs {
				if ts, ok := s.(*ast.TypeSpec); ok && ts.Name != nil {
					groups[ts.Name.Name] = true
				}
			}
		}
	}
	return groups
}

func receiverName(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.StarExpr:
		return receiverName(t.X)
	case *ast.Ident:
		return t.Name
	case *ast.IndexExpr:
		return receiverName(t.X)
	case *ast.IndexListExpr:
		return receiverName(t.X)
	}
	return ""
}

var (
	// Top-level JS/TS declarations. The dashboard static-src fragments are
	// spliced into a <script> block and indented four spaces, so function
	// and class declarations may carry that indentation; const/let/var must
	// start at column 0 to avoid counting locals.
	jsDeclRe    = regexp.MustCompile(`(?m)^(?:(?: {4}|\t)?(?:export\s+)?(?:async\s+)?(?:function\*?|class)|(?:export\s+)?(?:const|let|var))\s+([A-Za-z_$][\w$]*)`)
	pyDeclRe    = regexp.MustCompile(`(?m)^(?:async\s+)?(?:def|class)\s+([A-Za-z_]\w*)`)
	mdHeadingRe = regexp.MustCompile(`(?m)^##\s+(.+?)\s*$`)
)

func regexGroups(src []byte, re *regexp.Regexp) map[string]bool {
	groups := map[string]bool{}
	for _, m := range re.FindAllSubmatch(src, -1) {
		if len(m) > 1 {
			groups[strings.TrimSpace(string(m[1]))] = true
		}
	}
	return groups
}

// CountLines counts newline-terminated lines plus a trailing partial line.
func CountLines(src []byte) int {
	if len(src) == 0 {
		return 0
	}
	n := bytes.Count(src, []byte{'\n'})
	if src[len(src)-1] != '\n' {
		n++
	}
	return n
}
