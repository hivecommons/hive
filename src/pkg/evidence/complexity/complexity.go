// Package complexity produces the monthly, report-only complexity and
// static-analysis snapshot of a managed repo's default branch (RFC #12062,
// #12072). A snapshot is written next to the per-PR evidence bundles at
// <root>/<org>/<repo>/complexity/<YYYY-MM>.json. Nothing here gates a merge.
//
// The package is stdlib-only. Cyclomatic complexity is measured over Go
// sources with go/ast; static-analysis findings are counted from the
// line-oriented output of a tool such as go vet that the caller ran.
package complexity

import (
	"encoding/json"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the value of Snapshot.SchemaVersion.
const SchemaVersion = "v1"

// Snapshot is one repo's maintainability reading for one month.
type Snapshot struct {
	SchemaVersion  string         `json:"schema_version"`
	Repo           string         `json:"repo"`
	Month          string         `json:"month"`
	Ref            string         `json:"ref,omitempty"`
	CapturedAt     time.Time      `json:"captured_at"`
	Files          int            `json:"files"`
	Functions      int            `json:"functions"`
	TotalCyclo     int            `json:"total_cyclomatic"`
	MaxCyclo       int            `json:"max_cyclomatic"`
	MeanCyclo      float64        `json:"mean_cyclomatic"`
	OverThreshold  int            `json:"functions_over_threshold"`
	Threshold      int            `json:"threshold"`
	Warnings       int            `json:"static_warnings"`
	WarningsByTool map[string]int `json:"static_warnings_by_tool,omitempty"`
	Hotspots       []Function     `json:"hotspots,omitempty"`
}

// Function is one function's cyclomatic complexity.
type Function struct {
	File  string `json:"file"`
	Name  string `json:"name"`
	Cyclo int    `json:"cyclomatic"`
}

// DefaultThreshold is the cyclomatic value above which a function is counted
// in Snapshot.OverThreshold.
const DefaultThreshold = 15

const maxHotspots = 10

// Month formats t as the snapshot key, YYYY-MM in UTC.
func Month(t time.Time) string { return t.UTC().Format("2006-01") }

// Cyclomatic returns the cyclomatic complexity of a function body: one plus
// the number of branching constructs.
func Cyclomatic(fn *ast.FuncDecl) int {
	if fn.Body == nil {
		return 1
	}
	n := 1
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch x := node.(type) {
		case *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt:
			n++
		case *ast.CaseClause:
			if x.List != nil {
				n++
			}
		case *ast.CommClause:
			if x.Comm != nil {
				n++
			}
		case *ast.BinaryExpr:
			if x.Op == token.LAND || x.Op == token.LOR {
				n++
			}
		}
		return true
	})
	return n
}

// Measure walks root and measures every non-test, non-generated-vendor Go
// file. Paths in the result are relative to root. Unparseable files are
// skipped; they are counted by the compiler, not by this report.
func Measure(root string, threshold int) (Snapshot, error) {
	if threshold <= 0 {
		threshold = DefaultThreshold
	}
	s := Snapshot{SchemaVersion: SchemaVersion, Threshold: threshold}
	var funcs []Function
	fset := token.NewFileSet()
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case "vendor", "node_modules", "testdata", ".git":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if perr != nil {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		rel = filepath.ToSlash(rel)
		s.Files++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			funcs = append(funcs, Function{File: rel, Name: funcName(fn), Cyclo: Cyclomatic(fn)})
		}
		return nil
	})
	if err != nil {
		return Snapshot{}, err
	}
	for _, f := range funcs {
		s.Functions++
		s.TotalCyclo += f.Cyclo
		if f.Cyclo > s.MaxCyclo {
			s.MaxCyclo = f.Cyclo
		}
		if f.Cyclo > threshold {
			s.OverThreshold++
		}
	}
	if s.Functions > 0 {
		s.MeanCyclo = float64(s.TotalCyclo*100/s.Functions) / 100
	}
	sort.Slice(funcs, func(i, j int) bool {
		if funcs[i].Cyclo != funcs[j].Cyclo {
			return funcs[i].Cyclo > funcs[j].Cyclo
		}
		if funcs[i].File != funcs[j].File {
			return funcs[i].File < funcs[j].File
		}
		return funcs[i].Name < funcs[j].Name
	})
	if len(funcs) > maxHotspots {
		funcs = funcs[:maxHotspots]
	}
	s.Hotspots = funcs
	return s, nil
}

func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	t := fn.Recv.List[0].Type
	if star, ok := t.(*ast.StarExpr); ok {
		t = star.X
	}
	if idx, ok := t.(*ast.IndexExpr); ok {
		t = idx.X
	}
	if id, ok := t.(*ast.Ident); ok {
		return id.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}

// CountFindings counts file:line:col diagnostics in a static-analysis tool's
// output, ignoring "#" package headers and other noise.
func CountFindings(out string) int {
	n := 0
	for _, line := range strings.Split(out, "\n") {
		parts := strings.SplitN(strings.TrimSpace(line), ":", 4)
		if len(parts) < 3 || !strings.HasSuffix(parts[0], ".go") {
			continue
		}
		if isDigits(parts[1]) && isDigits(parts[2]) {
			n++
		}
	}
	return n
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// AddWarnings records a tool's finding count on the snapshot.
func (s *Snapshot) AddWarnings(tool string, n int) {
	if s.WarningsByTool == nil {
		s.WarningsByTool = map[string]int{}
	}
	s.WarningsByTool[tool] += n
	s.Warnings += n
}

// Path returns where the snapshot for repo ("org/name") and month lives under
// the evidence root.
func Path(root, repo, month string) (string, error) {
	org, name, ok := strings.Cut(repo, "/")
	if !ok || org == "" || name == "" || strings.Contains(name, "/") ||
		org == "." || org == ".." || name == "." || name == ".." {
		return "", fmt.Errorf("complexity: invalid repo %q", repo)
	}
	if _, err := time.Parse("2006-01", month); err != nil {
		return "", fmt.Errorf("complexity: invalid month %q", month)
	}
	return filepath.Join(root, org, name, "complexity", month+".json"), nil
}

// ErrExists is returned by Write when the month's snapshot already exists;
// monthly snapshots are never overwritten.
var ErrExists = errors.New("complexity: snapshot already exists")

// Write stores s under root, keyed by s.Repo and s.Month.
func Write(root string, s Snapshot) (string, error) {
	p, err := Path(root, s.Repo, s.Month)
	if err != nil {
		return "", err
	}
	if _, err := os.Stat(p); err == nil {
		return p, ErrExists
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(p, append(b, '\n'), 0o644); err != nil {
		return "", err
	}
	return p, nil
}

// Load reads a snapshot back.
func Load(path string) (Snapshot, error) {
	var s Snapshot
	b, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(b, &s)
	return s, err
}
