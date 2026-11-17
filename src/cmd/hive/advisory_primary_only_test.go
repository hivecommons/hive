package main

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hivecommons/hive/pkg/config"
)

// #18568: a hive keeps exactly ONE advisory report, in project.primary_repo.
// A candidate build ensured (and so created) a report in every repo of
// project.repos each eval cycle, opening one in every monitored repo of a
// multi-repo hive. These tests pin the primary-only rule at the cycle seam and
// guard the eval-cycle source against a per-repo fan-out coming back.

func multiRepoAdvisoryConfig(primary string, repos ...string) *config.Config {
	cfg := &config.Config{}
	cfg.Project.Org = "hivecommons"
	cfg.Project.PrimaryRepo = primary
	cfg.Project.Repos = repos
	return cfg
}

// TestAdvisoryEnsure_MultiRepoHiveEnsuresOnlyThePrimary drives the eval
// cycle's ensure over several cycles of a multi-repo hive, and of a standalone
// hive whose primary is not listed first, and asserts only the primary repo is
// ever asked for an advisory issue.
func TestAdvisoryEnsure_MultiRepoHiveEnsuresOnlyThePrimary(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		want string
	}{
		{
			name: "hive with many monitored repos",
			cfg:  multiRepoAdvisoryConfig("hive", "hive", "crux", "dibs", "docs", "pluk", "quorum"),
			want: "hive",
		},
		{
			name: "standalone hive, primary listed after another repo",
			cfg:  multiRepoAdvisoryConfig("spyre-inference", "torch-spyre", "spyre-inference"),
			want: "spyre-inference",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			deps := advisoryEnsureDeps{
				ensure: func(_ context.Context, repo string) (int, error) {
					asked = append(asked, repo)
					return 42, nil
				},
				setenv: func(string, string) error { return nil },
			}
			issues := map[string]int{}
			const cycles = 5
			for i := 0; i < cycles; i++ {
				if err := ensurePinnedAdvisoryIssue(context.Background(), issues, primaryAdvisoryRepo(tc.cfg), deps, nil); err != nil {
					t.Fatalf("cycle %d: %v", i, err)
				}
			}
			if len(asked) != 1 || asked[0] != tc.want {
				t.Fatalf("ensure asked for %v, want exactly [%s] once", asked, tc.want)
			}
			if len(issues) != 1 || issues[tc.want] != 42 {
				t.Fatalf("advisory issues = %v, want only %s", issues, tc.want)
			}
		})
	}
}

// TestAdvisoryEnsure_RestartResolvesTheSameSingleReport models a restart as a
// fresh, empty advisoryIssues map against a forge that already holds the
// report: the restarted process asks once, for the primary only, and adopts
// the existing number. Creation idempotency itself (find by title and label
// before create) is pinned in pkg/github.
func TestAdvisoryEnsure_RestartResolvesTheSameSingleReport(t *testing.T) {
	cfg := multiRepoAdvisoryConfig("hive", "hive", "crux", "dibs")
	const existing = 2364
	forge := map[string]int{"hive": existing}
	var asked []string
	deps := advisoryEnsureDeps{
		ensure: func(_ context.Context, repo string) (int, error) {
			asked = append(asked, repo)
			return forge[repo], nil
		},
		setenv: func(string, string) error { return nil },
	}
	const restarts = 3
	for i := 0; i < restarts; i++ {
		issues := map[string]int{}
		if err := ensurePinnedAdvisoryIssue(context.Background(), issues, primaryAdvisoryRepo(cfg), deps, nil); err != nil {
			t.Fatalf("restart %d: %v", i, err)
		}
		if len(issues) != 1 || issues["hive"] != existing {
			t.Fatalf("restart %d: advisory issues = %v, want only hive=#%d", i, issues, existing)
		}
	}
	for _, repo := range asked {
		if repo != "hive" {
			t.Fatalf("restart asked for an advisory issue in non-primary repo %q", repo)
		}
	}
	if len(asked) != restarts {
		t.Fatalf("ensure ran %d times over %d restarts, want once per restart", len(asked), restarts)
	}
}

// advisoryFanOutCalls are the calls that create or write an advisory report.
// None of them may run inside a loop in package main: the hive has one report
// repo, so iterating repos around any of them is the #18568 fan-out.
var advisoryFanOutCalls = map[string]bool{
	"EnsureAdvisoryIssue":       true,
	"PostAdvisoryDigest":        true,
	"ensurePinnedAdvisoryIssue": true,
}

// TestAdvisoryWrites_NeverIssuedInsideALoop parses every non-test source file
// of package main and fails when an advisory ensure or digest post sits inside
// a for or range loop.
func TestAdvisoryWrites_NeverIssuedInsideALoop(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			var body *ast.BlockStmt
			switch loop := n.(type) {
			case *ast.RangeStmt:
				body = loop.Body
			case *ast.ForStmt:
				body = loop.Body
			default:
				return true
			}
			ast.Inspect(body, func(inner ast.Node) bool {
				call, ok := inner.(*ast.CallExpr)
				if !ok {
					return true
				}
				if fn := calledName(call.Fun); advisoryFanOutCalls[fn] {
					t.Errorf("%s: %s called inside a loop; a hive has exactly one advisory report, in project.primary_repo (#18568)",
						fset.Position(call.Pos()), fn)
				}
				return true
			})
			return false
		})
	}
	if checked == 0 {
		t.Fatal("no source files checked; the guard is not running")
	}
}

// advisoryWriterWiring is how many times each advisory writer may be wired as
// a value (not called directly) in package main. The eval cycle hands the
// client's EnsureAdvisoryIssue and PostAdvisoryDigest to its primary-repo
// seams exactly once each, and reads advisoryEnsureDepsForCycle.ensure only
// where it assigns it. The #18568 regression wired a second copy of each into
// a per-repo loop helper, which raised all of these counts to two.
//
// The opt-in per-repo report (#18448, governor.advisory.per_repo) adds exactly
// one more PostAdvisoryDigest wiring and one EnsureSecondaryAdvisoryIssue
// wiring, both handed to the secondary helpers, whose repo list is pinned to
// advisoryPerRepoSecondaryRepos (empty unless the owner opted in) by
// TestAdvisorySecondaryHelpers_OnlyFedThePerRepoGatedList.
var advisoryWriterWiring = map[string]int{
	"EnsureAdvisoryIssue":               1,
	"EnsureSecondaryAdvisoryIssue":      1,
	"PostAdvisoryDigest":                2,
	"advisoryEnsureDepsForCycle.ensure": 1,
}

// TestAdvisoryWriters_WiredOnceForThePrimaryOnly counts the value references
// above across every non-test source file of package main.
func TestAdvisoryWriters_WiredOnceForThePrimaryOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	got := map[string]int{}
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		called := map[ast.Expr]bool{}
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				called[call.Fun] = true
			}
			return true
		})
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			switch sel.Sel.Name {
			case "EnsureAdvisoryIssue", "EnsureSecondaryAdvisoryIssue", "PostAdvisoryDigest":
				if !called[sel] {
					got[sel.Sel.Name]++
				}
			case "ensure":
				if x, isIdent := sel.X.(*ast.Ident); isIdent && x.Name == "advisoryEnsureDepsForCycle" {
					got["advisoryEnsureDepsForCycle.ensure"]++
				}
			}
			return true
		})
	}
	for key, want := range advisoryWriterWiring {
		if got[key] != want {
			t.Errorf("%s wired %d times, want %d: a hive has exactly one advisory report, in project.primary_repo (#18568)",
				key, got[key], want)
		}
	}
}

func calledName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		return f.Sel.Name
	}
	return ""
}
