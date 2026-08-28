package dashboard

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// openAPISpecPath is the published API contract dashboard clients (README,
// the `hive tui` client tracked by #4907) code against. It lives two
// directories above the repo's src/ tree, at the repo-root dashboard/
// directory — see dashboard/README.md for why that legacy-looking directory
// is still where the live v2 spec is published from.
const openAPISpecPath = "../../../dashboard/openapi.json"

// dashboardRouteSourceDir is scanned for every `s.mux.HandleFunc(...)` /
// `s.mux.Handle(...)` route registration. Parsing the actual registration
// call sites with go/ast, rather than hand-listing routes, is the whole
// point: a hand-written list is exactly the kind of thing that drifted and
// produced #4912 (32 of ~300 registered dashboard operations documented).
const dashboardRouteSourceDir = "."

// routeParityException documents one registered dashboard route that
// legitimately does not appear as a normal JSON operation in
// dashboard/openapi.json, and why. This is NOT a place to silence real
// drift: a route added here without the guard actually being true (i.e.
// without it being a probe/websocket/legacy-catchall/docs-page route)
// defeats the whole test. See the PR that introduced this file for the bar
// a new entry must clear.
type routeParityException struct {
	method string
	path   string
	reason string
}

// routeParityExceptions is the complete, closed set of registered dashboard
// routes allowed to be absent from dashboard/openapi.json's paths. Anything
// not listed here must appear in the spec (with a matching method) or the
// test fails.
var routeParityExceptions = []routeParityException{
	{
		method: "GET", path: "/api/health",
		reason: "Kubernetes readiness probe. Returns a bare {\"status\":...} " +
			"liveness signal, not a documented client-facing data operation.",
	},
	{
		method: "GET", path: "/api/health/deep",
		reason: "Deep health/dependency probe for operators, same category as " +
			"/api/health — not part of the client data contract.",
	},
	{
		method: "GET", path: "/api/livez",
		reason: "Kubernetes livenessProbe target (see handleLivez's doc comment " +
			"in server.go for why it's distinct from /api/health). Probe " +
			"endpoint, not a client data operation.",
	},
	{
		method: "GET", path: "/api/v1/",
		reason: "Legacy GitHub-PAT-authenticated catch-all (handleAPIv1) that " +
			"dispatches on the request subpath internally via its own switch " +
			"statement rather than exposing discrete ServeMux routes. Its " +
			"sub-operations are not independently documentable path entries " +
			"under this spec's per-path model.",
	},
	{
		method: "POST", path: "/api/v1/",
		reason: "Same handler (handleAPIv1) and same reasoning as the GET " +
			"/api/v1/ entry above.",
	},
	{
		method: "GET", path: "/api/contribute/ws",
		reason: "WebSocket upgrade endpoint (ContributeWSHub.HandleWS), not a " +
			"JSON request/response REST operation OpenAPI can describe.",
	},
	{
		method: "POST", path: "/api/terminal/assertion/renew",
		reason: "Internal browser-session mechanism: re-mints the caller's " +
			"spoke-local terminal-assertion cookie from their existing " +
			"hive_session cookie (see terminal_assertion_renew.go's file " +
			"comment). Not a public data API a client calls for a response " +
			"body; it has none.",
	},
	{
		method: "GET", path: "/api/docs",
		reason: "Serves an HTML API-docs page (handleAPIDocs) that renders " +
			"this very spec for humans, not a JSON data operation.",
	},
}

// exceptedRouteKeys returns the set of "METHOD path" keys routeParityExceptions
// permits to be absent from the spec.
func exceptedRouteKeys() map[string]bool {
	out := make(map[string]bool, len(routeParityExceptions))
	for _, e := range routeParityExceptions {
		out[routeKey(e.method, e.path)] = true
	}
	return out
}

func routeKey(method, path string) string {
	return method + " " + path
}

// pathConstants maps package-level string constant names (e.g.
// terminalPathPrefix) declared anywhere in the dashboard package to their
// literal values, so route registrations built from
// `"METHOD "+someConstant` or `someConstant+"/suffix"` can be resolved to a
// concrete path the same way the Go compiler would.
func pathConstants(t *testing.T, fset *token.FileSet, files []*ast.File) map[string]string {
	t.Helper()
	out := make(map[string]string)
	for _, f := range files {
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					val, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out[name.Name] = val
				}
			}
		}
	}
	return out
}

// registeredDashboardRoutes parses every non-test .go file directly under
// src/pkg/dashboard for `s.mux.HandleFunc(...)` / `s.mux.Handle(...)` calls
// and returns the "METHOD path" registered by each, resolving simple
// string-literal-plus-constant path expressions the same handful of files
// use (see terminal_proxy.go, openrouter.go, claude_auth.go's callback
// paths). A registration this cannot confidently resolve fails the test
// loudly (via t.Fatalf) rather than silently skipping it — an unresolvable
// route is exactly the kind of blind spot this guard exists to close.
//
// Only routes under /api/ are returned: dashboard/openapi.json documents the
// JSON API surface (see its info.title), not the HTML pages the same mux
// also serves (/contribute, /leaderboard, /snapshot, /{$}, OAuth callback
// redirects, etc.) — those are a different kind of resource and were never
// in scope for this spec.
func registeredDashboardRoutes(t *testing.T) map[string]bool {
	t.Helper()

	entries, err := os.ReadDir(dashboardRouteSourceDir)
	if err != nil {
		t.Fatalf("reading %s: %v", dashboardRouteSourceDir, err)
	}

	fset := token.NewFileSet()
	var files []*ast.File
	var goFiles []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		full := filepath.Join(dashboardRouteSourceDir, name)
		f, err := parser.ParseFile(fset, full, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", full, err)
		}
		files = append(files, f)
		goFiles = append(goFiles, full)
	}
	if len(files) == 0 {
		t.Fatalf("found no non-test .go files under %s; route scan would be vacuously empty", dashboardRouteSourceDir)
	}

	consts := pathConstants(t, fset, files)

	routes := make(map[string]bool)
	var unresolved []string

	// Declared before assignment so the BinaryExpr case can recurse into
	// itself: a closure bound with := is not in scope inside its own body.
	var resolveString func(e ast.Expr) (string, bool)
	resolveString = func(e ast.Expr) (string, bool) {
		switch v := e.(type) {
		case *ast.BasicLit:
			if v.Kind != token.STRING {
				return "", false
			}
			s, err := strconv.Unquote(v.Value)
			if err != nil {
				return "", false
			}
			return s, true
		case *ast.Ident:
			if val, ok := consts[v.Name]; ok {
				return val, true
			}
			return "", false
		case *ast.BinaryExpr:
			if v.Op != token.ADD {
				return "", false
			}
			left, lok := resolveString(v.X)
			right, rok := resolveString(v.Y)
			if !lok || !rok {
				return "", false
			}
			return left + right, true
		}
		return "", false
	}

	for fi, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if sel.Sel.Name != "HandleFunc" && sel.Sel.Name != "Handle" {
				return true
			}
			// Only calls on something ending in ".mux" (s.mux, hub.mux, etc.)
			// — the dashboard package's own route table. This deliberately
			// excludes unrelated *http.ServeMux users if any are ever added.
			muxSel, ok := sel.X.(*ast.SelectorExpr)
			if !ok || muxSel.Sel.Name != "mux" {
				return true
			}
			if len(call.Args) == 0 {
				return true
			}
			full, ok := resolveString(call.Args[0])
			if !ok {
				pos := fset.Position(call.Pos())
				unresolved = append(unresolved, pos.String())
				return true
			}
			method := "GET"
			path := full
			if idx := strings.IndexByte(full, ' '); idx >= 0 {
				method = full[:idx]
				path = full[idx+1:]
			}
			// dashboard/openapi.json is scoped to the JSON /api/* surface (see
			// its info.title/description) — HTML page routes like /contribute,
			// /leaderboard, /snapshot, /{$}, and the terminal/OAuth-callback
			// redirects are a different kind of resource (rendered pages, not
			// data operations) that this spec was never meant to catalogue.
			// Routes outside /api/ are intentionally excluded from the parity
			// check entirely, rather than requiring 16+ page routes to be
			// listed as exceptions for reasons that are all "it's a page".
			if !strings.HasPrefix(path, "/api/") {
				return true
			}
			routes[routeKey(method, path)] = true
			return true
		})
		_ = goFiles[fi]
	}

	if len(unresolved) > 0 {
		sort.Strings(unresolved)
		t.Fatalf("could not statically resolve the route path for mux registration(s) at:\n  %s\n"+
			"this guard requires every registration to resolve to a literal method+path so it can be "+
			"checked against dashboard/openapi.json; teach resolveString about the new pattern or "+
			"rewrite the registration as a plain string literal",
			strings.Join(unresolved, "\n  "))
	}

	return routes
}

// specDocumentedOperations returns every "METHOD path" dashboard/openapi.json
// documents.
func specDocumentedOperations(t *testing.T) map[string]bool {
	t.Helper()

	raw, err := os.ReadFile(openAPISpecPath)
	if err != nil {
		t.Fatalf("reading %s: %v", openAPISpecPath, err)
	}

	var doc struct {
		Paths map[string]map[string]json.RawMessage `json:"paths"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parsing %s: %v", openAPISpecPath, err)
	}
	if len(doc.Paths) == 0 {
		t.Fatalf("%s has no paths; spec parsing likely broken silently", openAPISpecPath)
	}

	httpMethods := map[string]bool{
		"get": true, "post": true, "put": true, "delete": true,
		"patch": true, "options": true, "head": true,
	}

	ops := make(map[string]bool)
	for path, methods := range doc.Paths {
		for m := range methods {
			if !httpMethods[m] {
				continue // e.g. "parameters" siblings, "summary", etc.
			}
			ops[routeKey(strings.ToUpper(m), path)] = true
		}
	}
	return ops
}

// TestOpenAPISpecCoversEveryRegisteredRoute is the #4912 guard: every route
// the Go dashboard server actually registers must appear in
// dashboard/openapi.json (or be a documented exception above), and every
// operation the spec documents must correspond to a route the server
// actually registers. A route added to server.go/api.go/etc. without a
// matching spec entry — the exact shape of #4912 — fails here rather than
// silently shipping an undocumented endpoint.
func TestOpenAPISpecCoversEveryRegisteredRoute(t *testing.T) {
	registered := registeredDashboardRoutes(t)
	documented := specDocumentedOperations(t)
	excepted := exceptedRouteKeys()

	var problems []string

	for key := range registered {
		if documented[key] || excepted[key] {
			continue
		}
		problems = append(problems, "registered but undocumented: "+key+
			" — add it to dashboard/openapi.json or, if it genuinely should stay "+
			"unpublished, add a routeParityException with a reason")
	}

	for key := range documented {
		if registered[key] {
			continue
		}
		problems = append(problems, "documented in dashboard/openapi.json but no such route is "+
			"registered: "+key+" — the route was removed/renamed and the spec entry is now stale")
	}

	// A declared exception that no longer describes a real gap (the route is
	// now documented, or the route no longer exists at all) is stale
	// bookkeeping that would silently mask a future real regression. Fail
	// loudly rather than let it rot, mirroring
	// TestShellAndGoCLIBackendListsAgree's treatment of stale exceptions in
	// src/pkg/config/backend_list_parity_test.go.
	for key := range excepted {
		inRegistered, inDocumented := registered[key], documented[key]
		if !inRegistered {
			problems = append(problems, "declared routeParityException "+key+
				" does not match any registered route; remove it or fix the method/path")
		}
		if inDocumented {
			problems = append(problems, "declared routeParityException "+key+
				" is now documented in dashboard/openapi.json; remove the exception")
		}
	}

	if len(problems) > 0 {
		sort.Strings(problems)
		t.Fatalf("dashboard/openapi.json has drifted from the registered dashboard routes "+
			"(%d registered, %d documented, %d problem(s)):\n  %s",
			len(registered), len(documented), len(problems), strings.Join(problems, "\n  "))
	}
}
