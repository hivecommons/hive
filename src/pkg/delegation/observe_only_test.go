package delegation

import (
	"bytes"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestFlagOffIsByteIdenticalToBaseline is the REGRESSION PIN for the flag's OFF
// state.
//
// "Defaults off" is a claim that is trivially easy to believe and surprisingly
// easy to get wrong: a constructor that still resolves an identity, a log line
// that still fires at debug, a response field that appears as `null` instead of
// being absent. Any of those is a behavior change shipped to ~65 spokes under
// the banner of a disabled feature.
//
// So this asserts the strong form: with the flag off, the observable outputs
// are byte-identical to what a hive with no delegation code at all would
// produce — no token, no log line, no key material published, and crucially NO
// WORK DONE (the situation constructor is never invoked, proven by a side-effect
// counter).
func TestFlagOffIsByteIdenticalToBaseline(t *testing.T) {
	// Explicitly empty rather than unset, so the test also covers an operator
	// who set the var to "" rather than removing it.
	t.Setenv(EnvChainsEnabled, "")

	if Enabled() {
		t.Fatal("the flag defaults ON; it must default OFF")
	}

	var logBuf bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	m := NewMinter(testMaster, "acme", 1, logger)
	if m == nil {
		t.Fatal("NewMinter returned nil for a valid master")
	}

	// The situation constructor must never run. This counter is the difference
	// between "the flag suppresses output" and "the flag suppresses work" —
	// only the latter makes flag-off genuinely free on hot paths that would
	// otherwise read a bot-login file or resolve a session per action.
	builds := 0
	build := func() (Chain, error) {
		builds++
		return ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	}

	now := time.Now()
	if tok := m.MintFor("agent_kicked", now, build); tok != "" {
		t.Errorf("a chain was minted with the flag off: %q", tok)
	}
	if builds != 0 {
		t.Errorf("the situation constructor ran %d times with the flag off; it must not run at all", builds)
	}

	// Direct Mint is gated too, so a caller that assembled a chain by hand
	// cannot bypass the flag.
	c, _ := ScheduledWorkChain("acme", "scanner", "cadence:scanner")
	if tok := m.Mint(c, "agent_kicked", now); tok != "" {
		t.Errorf("Mint bypassed the flag: %q", tok)
	}

	// Observe with an empty token is a no-op, so a caller doing the natural
	// `tok := Mint(...); Observe(tok, ...)` emits nothing.
	m.Observe("", "agent_kicked")

	if logBuf.Len() != 0 {
		t.Errorf("the flag-off path wrote log output:\n%s", logBuf.String())
	}

	// The published document reports disabled and carries NO keys, so a fetcher
	// can tell "off" from "nothing happened" without a key being exposed.
	doc := BuildKeyDocument(false, 1, testPub(), []PublishedKey{{Generation: 0, PublicKey: testPub()}}, now)
	if doc.Enabled {
		t.Error("published document reports enabled with the flag off")
	}
	if len(doc.Keys) != 0 {
		t.Errorf("published document carries %d keys with the flag off; want 0", len(doc.Keys))
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshalling: %v", err)
	}
	if strings.Contains(string(raw), testPub()) {
		t.Error("a public key was published with the flag off")
	}
}

// TestFlagAcceptedValuesMatchHiveConvention pins the parsing so an operator who
// knows HIVE_METRICS_ENABLED's spelling is not surprised here.
func TestFlagAcceptedValuesMatchHiveConvention(t *testing.T) {
	on := []string{"1", "true", "yes", "on", "TRUE", "  On  "}
	off := []string{"", "0", "false", "no", "off", "maybe", "2", "enabled"}

	for _, v := range on {
		t.Setenv(EnvChainsEnabled, v)
		if !Enabled() {
			t.Errorf("value %q should enable the flag", v)
		}
	}
	for _, v := range off {
		t.Setenv(EnvChainsEnabled, v)
		if Enabled() {
			t.Errorf("value %q should NOT enable the flag", v)
		}
	}
}

// enforcementSignals are the identifiers that would indicate a caller is making
// an authorization DECISION from a chain.
//
// The list is about the SHAPE of a decision, not about specific function names:
// what makes a call site enforcing is that a chain's validity or root reaches a
// branch that refuses, degrades, or alters behavior. These are the tokens such
// a branch is overwhelmingly likely to contain in this codebase.
var enforcementSignals = []string{
	"http.Error",
	"StatusForbidden",
	"StatusUnauthorized",
	"WriteHeader",
	"Forbidden",
	"Unauthorized",
	"requireAuth",
	"requireAdmin",
	"AccessDenied",
	"PermissionDenied",
	"denied",
	"reject",
	"refuse",
	"block",
}

// chainDecisionAPIs are the delegation APIs whose RESULT is a statement about a
// chain's validity or authorship. If one of these feeds an enforcement signal,
// something is gating on the chain.
var chainDecisionAPIs = []string{
	"VerifyToken",
	"VerifyTokenAcrossKeys",
	"HasHumanRoot",
	"Root",
	"MintFor",
	"Mint",
	"Enabled",
}

// TestObserveOnlyInvariant_NoEnforcementConsultsChain is THE OBSERVE-ONLY
// INVARIANT, pinned at the source level.
//
// WHY A SOURCE-LEVEL TEST AND NOT A BEHAVIORAL ONE. A behavioral test can only
// prove that the code paths it happens to exercise do not enforce. The property
// we need is universal — NO path enforces — and it must survive a future PR
// written by someone who never read this file. Enforcement would be a small,
// locally-reasonable-looking diff (`if !chain.HasHumanRoot() { http.Error(...) }`)
// in a package far from here. Only a scan of the whole tree catches that, and
// only a test that FAILS on it prevents the observe phase from ending silently.
//
// This is the mechanism that makes "enforcement is a separate future decision"
// enforceable rather than aspirational: turning it on requires deleting or
// amending this test, which is a visible, reviewable, arguable act.
//
// WHAT IT ALLOWS. Emitting, logging, comparing, and publishing keys. The
// comparison harness reasons about chains extensively — that is its job — and
// it is exempt because its inputs are historical records and its output is a
// report, never a decision in a request path.
func TestObserveOnlyInvariant_NoEnforcementConsultsChain(t *testing.T) {
	root := repoSrcRoot(t)

	// Files exempt from the scan, each for a stated reason.
	exempt := map[string]string{
		// This package IS the chain implementation: VerifyToken necessarily
		// contains the validity logic, and its callers here are tests.
		filepath.Join("pkg", "delegation"): "the delegation package itself implements verification",
		// The hub endpoint publishes public keys; it reads no chain. Verified
		// separately below.
		filepath.Join("pkg", "hub", "delegation_keys.go"): "publishes public keys only; reads no chain",
	}

	var violations []string

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == "node_modules" || info.Name() == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return nil
		}
		for prefix := range exempt {
			if rel == prefix || strings.HasPrefix(rel, prefix+string(filepath.Separator)) {
				return nil
			}
		}

		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		// Cheap pre-filter: a file that never mentions delegation cannot gate
		// on a chain.
		if !bytes.Contains(src, []byte("delegation.")) {
			return nil
		}

		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, src, 0)
		if perr != nil {
			return nil
		}

		// For every function that touches a chain-decision API, check whether
		// the SAME function also contains an enforcement signal. Co-occurrence
		// within one function is the tightest cheap approximation of "this
		// decision is derived from that value", and it is deliberately
		// conservative: a false positive costs a comment explaining why a
		// function is exempt, while a false negative would let enforcement ship.
		ast.Inspect(f, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				return true
			}
			var buf bytes.Buffer
			start := fset.Position(fn.Pos()).Offset
			end := fset.Position(fn.End()).Offset
			if start < 0 || end > len(src) || start >= end {
				return true
			}
			buf.Write(src[start:end])
			body := buf.String()

			if !strings.Contains(body, "delegation.") {
				return true
			}
			usesChainDecision := false
			for _, api := range chainDecisionAPIs {
				if strings.Contains(body, "delegation."+api) {
					usesChainDecision = true
					break
				}
			}
			if !usesChainDecision {
				return true
			}
			for _, sig := range enforcementSignals {
				if strings.Contains(body, sig) {
					violations = append(violations, rel+":"+fn.Name.Name+
						" uses a delegation chain API alongside enforcement signal "+sig)
					break
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking source tree: %v", err)
	}

	if len(violations) > 0 {
		t.Fatalf("OBSERVE-ONLY INVARIANT VIOLATED — a code path appears to gate on a delegation chain.\n"+
			"This PR ships observation only; enforcement is a separate, deliberate decision\n"+
			"(see src/docs/delegation-chain.md). If enforcement is genuinely intended, that\n"+
			"decision must be made explicitly and this test amended as part of it.\n\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestObserveOnlyInvariant_MinterHasNoDecisionAPI pins the API surface itself.
//
// The source scan above catches a caller that gates. This catches the earlier
// mistake of ADDING an API that invites gating — an `Authorize`, `Allow`, or
// `Check` method whose very existence would make enforcement a one-line change
// somewhere else. Keeping the surface free of such a method is what makes the
// scan's job tractable.
func TestObserveOnlyInvariant_MinterHasNoDecisionAPI(t *testing.T) {
	root := repoSrcRoot(t)
	pkgDir := filepath.Join(root, "pkg", "delegation")

	forbidden := []string{"Authorize", "Allow", "Deny", "Enforce", "Permit", "Check", "MustHave", "Require"}

	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, filepath.Join(pkgDir, e.Name()), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", e.Name(), perr)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !fn.Name.IsExported() {
				continue
			}
			for _, bad := range forbidden {
				if strings.HasPrefix(fn.Name.Name, bad) {
					t.Errorf("%s: exported %q reads as an authorization decision. This package is "+
						"observe-only; an API shaped like a gate invites one to be built.", e.Name(), fn.Name.Name)
				}
			}
		}
	}
}

// repoSrcRoot locates the `src` directory this package lives under.
func repoSrcRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// .../src/pkg/delegation -> .../src
	root := filepath.Dir(filepath.Dir(wd))
	if filepath.Base(root) != "src" {
		t.Fatalf("expected to run under src/pkg/delegation, got %s", wd)
	}
	return root
}
