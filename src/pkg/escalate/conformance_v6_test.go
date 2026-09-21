package escalate

// v6 guard-invariant conformance for the push / on-call surface
// (ntfy / Pushover / PagerDuty), shipped in #7613 / #7618.
//
// src/docs/v6-readiness.md §2 states the line's single non-negotiable: every
// non-dashboard surface routes through the *same* authorization and safety
// machinery, and a surface's conformance row is checked only with a link to a
// test that FAILS if the surface bypasses any of five mechanisms:
//
//  1. ioscan enforcement on inbound acknowledgement/action text;
//  2. the Converse gate/capability on reply/action paths that affect state;
//  3. canary/secret scrubbing on outbound payloads;
//  4. the dashboard role floor on routing and operator action authorization;
//  5. the proxy mode ladder/capability check before an action drives agent work.
//
// Push / on-call is an EGRESS-ONLY surface: it delivers escalations outward
// and has no inbound path, so mechanisms 1, 2, 4, and 5 are conformed to by
// *unreachability* rather than by a check. That is only a real guarantee if
// the unreachability is pinned, so this file asserts it two ways:
//
//   - mechanism 3 behaviourally, against all three providers, end to end;
//   - mechanisms 1/2/4/5 structurally, by parsing this package and failing if
//     an inbound entry point or a state-reaching dependency appears. When one
//     does, the invariant stops being "unreachable" and the guards have to be
//     wired and asserted for real — which is exactly what the failure says.
//
// Deleting a scrub call, or growing an ntfy action handler / a PagerDuty
// webhook / a Pushover receipt callback that skips the guards, fails this
// file. Issue: hivecommons/hive#8048. Tracker: hivecommons/hive#7683.

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

// Secret and canary shapes that must never reach a provider. Each is a
// synthetic value matching a pattern in pkg/logscrub's closed category set —
// the same scrubber the rest of the hive publishes text through.
var conformanceSecrets = map[string]string{
	"github-token": "ghp_conformance000000000000000000000000",
	"jwt":          "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dBjftJeZ4CVPmB92K27uhbUJU1p1r_wW1gFWFOEjXkc",
	"aws-key":      "AKIAIOSFODNN7EXAMPLE",
	"hive-canary":  "HIVE-CANARY-0123456789abcdef0123456789abcdef0123456789abcdef",
}

// leakyEvent spreads the secrets across every operator-visible field, so a
// scrub that covers only the body (or only the title) still fails.
func leakyEvent() Event {
	return Event{
		Severity: SeverityPage,
		Title:    "requires_human: " + conformanceSecrets["github-token"],
		Body:     "reason: token " + conformanceSecrets["jwt"] + "\nkey " + conformanceSecrets["aws-key"] + "\n" + conformanceSecrets["hive-canary"],
		Link:     "https://github.com/hivecommons/hive/pull/1?t=" + conformanceSecrets["github-token"],
	}
}

// assertNoSecrets fails if any synthetic secret survived into what the
// provider received — request line, headers, or body.
func assertNoSecrets(t *testing.T, provider, wire string) {
	t.Helper()
	for name, secret := range conformanceSecrets {
		if strings.Contains(wire, secret) {
			t.Errorf("v6 conformance (canary/secret scrubbing): %s payload carries an unscrubbed %s; "+
				"every escalation payload must pass logscrub.ScrubString before it leaves the process "+
				"(src/docs/v6-readiness.md §2)", provider, name)
		}
	}
}

// capture records everything a provider server saw, flattened into one string.
type capture struct {
	ch chan string
}

func newCapture() *capture { return &capture{ch: make(chan string, 4)} }

func (c *capture) server() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var b strings.Builder
		b.WriteString(r.Method + " " + r.URL.RequestURI() + "\n")
		for key, values := range r.Header {
			b.WriteString(key + ": " + strings.Join(values, ",") + "\n")
		}
		b.Write(body)
		select {
		case c.ch <- b.String():
		default:
		}
	}))
}

func (c *capture) wire(t *testing.T) string {
	t.Helper()
	select {
	case got := <-c.ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatal("provider received no request")
		return ""
	}
}

// TestV6Conformance_OutboundPayloadsAreScrubbed covers mechanism 3 for every
// shipped provider, on the direct-Deliver path.
func TestV6Conformance_OutboundPayloadsAreScrubbed(t *testing.T) {
	ctx := context.Background()

	t.Run("ntfy", func(t *testing.T) {
		rec := newCapture()
		srv := rec.server()
		defer srv.Close()
		sink := &NtfySink{URL: srv.URL, Token: "ntfy-credential"}
		if err := sink.Deliver(ctx, leakyEvent()); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		wire := rec.wire(t)
		assertNoSecrets(t, sink.Name(), wire)
		// The sink's own credential is configuration, not event text: it is
		// the authorization to deliver and must survive scrubbing intact.
		if !strings.Contains(wire, "Bearer ntfy-credential") {
			t.Error("ntfy delivery lost its configured bearer credential; scrubbing must cover the Event, not the sink config")
		}
	})

	t.Run("pushover", func(t *testing.T) {
		rec := newCapture()
		srv := rec.server()
		defer srv.Close()
		sink := &PushoverSink{URL: srv.URL, AppToken: "app", UserKey: "user"}
		if err := sink.Deliver(ctx, leakyEvent()); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		// Pushover is form-encoded; decode so a percent-escaped secret is
		// compared as plaintext rather than sliding past a substring check.
		wire := rec.wire(t)
		if _, encoded, ok := strings.Cut(wire, "\n\n"); ok {
			if decoded, err := decodeForm(encoded); err == nil {
				wire += "\n" + decoded
			}
		}
		assertNoSecrets(t, sink.Name(), wire)
	})

	t.Run("pagerduty", func(t *testing.T) {
		rec := newCapture()
		srv := rec.server()
		defer srv.Close()
		sink := &PagerDutySink{URL: srv.URL, RoutingKey: "rk"}
		if err := sink.Deliver(ctx, leakyEvent()); err != nil {
			t.Fatalf("deliver: %v", err)
		}
		// PagerDuty is JSON; unmarshal-and-remarshal so an escaped secret is
		// compared in its decoded form too.
		wire := rec.wire(t)
		if _, body, ok := strings.Cut(wire, "{"); ok {
			var payload any
			if err := json.Unmarshal([]byte("{"+body), &payload); err == nil {
				if flat, err := json.Marshal(payload); err == nil {
					wire += "\n" + string(flat)
				}
			}
		}
		assertNoSecrets(t, sink.Name(), wire)
	})
}

// TestV6Conformance_DispatchScrubsBeforeAnySink covers mechanism 3 on the
// path the runtime actually uses: cmd/hive registers the push sinks on a
// Dispatcher and calls Dispatch. A sink registered later — by a future
// provider, or by a test double — must not be able to observe raw text.
func TestV6Conformance_DispatchScrubsBeforeAnySink(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(chan Event, 1)
	d := NewDispatcher(ctx, nil, nil)
	defer d.Stop()
	d.Register(sinkFunc(func(ev Event) error {
		select {
		case seen <- ev:
		default:
		}
		return nil
	}), SeverityPage, 4)

	d.Dispatch(leakyEvent())

	select {
	case ev := <-seen:
		assertNoSecrets(t, "dispatcher", ev.Title+"\n"+ev.Body+"\n"+ev.Link)
	case <-time.After(5 * time.Second):
		t.Fatal("sink received no event")
	}
}

type sinkFunc func(Event) error

func (f sinkFunc) Name() string                              { return "conformance" }
func (f sinkFunc) Deliver(_ context.Context, ev Event) error { return f(ev) }

func decodeForm(encoded string) (string, error) {
	values, err := url.ParseQuery(strings.TrimSpace(encoded))
	if err != nil {
		return "", err
	}
	var b strings.Builder
	for key, vals := range values {
		b.WriteString(key + "=" + strings.Join(vals, ",") + "\n")
	}
	return b.String(), nil
}

// --- structural invariants: mechanisms 1, 2, 4 and 5 -----------------------

// stateReachingImports are packages a push/on-call sink must not depend on.
// Each one is a route to hive state that carries its own guard requirement:
// reaching it from this surface is how a bypass would be built.
var stateReachingImports = map[string]string{
	"github.com/hivecommons/hive/pkg/agent":     "kicking/pausing an agent requires the mode ladder and the Converse capability",
	"github.com/hivecommons/hive/pkg/dashboard": "dashboard actions require the role floor",
	"github.com/hivecommons/hive/pkg/proxy":     "tool calls require the proxy mode ladder/capability check",
	"github.com/hivecommons/hive/pkg/effects":   "mutations require an effects claim",
	"github.com/hivecommons/hive/pkg/github":    "writing to GitHub requires Converse and the mode ladder",
	"github.com/hivecommons/hive/pkg/scheduler": "scheduling agent work requires the mode ladder",
	"github.com/hivecommons/hive/pkg/hub":       "hub control requires the role floor",
	"os/exec":                                   "executing commands from a notification sink bypasses every guard at once",
}

// ioscanImport is the minimum bar an inbound path must clear before this test
// will let it exist at all.
const ioscanImport = "github.com/hivecommons/hive/pkg/ioscan"

// parseSurface parses this package's non-test sources, keyed by filename.
// Tests are excluded deliberately: the invariant is about what the shipped
// surface can reach, and test files legitimately import more than it does.
func parseSurface(t *testing.T) map[string]*ast.File {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package directory: %v", err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		parsed, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		files[name] = parsed
	}
	if len(files) == 0 {
		t.Fatal("parsed no package sources; the conformance scan would pass vacuously")
	}
	return files
}

// TestV6Conformance_SurfaceCannotDriveAgentWork covers mechanisms 2, 4 and 5.
//
// The push/on-call surface holds no capability check of its own, and it must
// not need one: it can format text and POST it to a provider, and that is
// all. This test fails if the package grows a dependency that could reach
// hive state, because at that moment "no guard needed" stops being true and
// the Converse gate, the dashboard role floor, and the proxy mode ladder have
// to be wired — and asserted here — for real.
func TestV6Conformance_SurfaceCannotDriveAgentWork(t *testing.T) {
	for path, file := range parseSurface(t) {
		for _, spec := range file.Imports {
			imported := strings.Trim(spec.Path.Value, `"`)
			if why, blocked := stateReachingImports[imported]; blocked {
				t.Errorf("v6 conformance (Converse / role floor / mode ladder): %s imports %q — %s. "+
					"The push/on-call surface is egress-only; it must not grow its own authorization. "+
					"Route the action through the dashboard API (role floor) and the proxy (mode ladder + "+
					"Converse) instead, and extend this test to assert those checks on the new path "+
					"(src/docs/v6-readiness.md §2, hivecommons/hive#8048)", path, imported, why)
			}
		}
	}
}

// TestV6Conformance_NoUnscannedInboundPath covers mechanism 1.
//
// No push provider currently drives anything inbound here: ntfy action
// buttons, Pushover receipt callbacks, and PagerDuty webhooks are all
// unimplemented, so there is no inbound text to scan. This test fails the
// moment one appears without pkg/ioscan alongside it, so "we have no inbound
// path" cannot quietly become "we have an unscanned inbound path".
func TestV6Conformance_NoUnscannedInboundPath(t *testing.T) {
	files := parseSurface(t)

	scansInbound := false
	for _, file := range files {
		for _, spec := range file.Imports {
			if strings.Trim(spec.Path.Value, `"`) == ioscanImport {
				scansInbound = true
			}
		}
	}

	for path, file := range files {
		ast.Inspect(file, func(n ast.Node) bool {
			fn, ok := n.(*ast.FuncDecl)
			if !ok {
				return true
			}
			reason := inboundReason(fn)
			if reason == "" || scansInbound {
				return true
			}
			t.Errorf("v6 conformance (ioscan enforcement): %s declares %s, which %s, but the package does not "+
				"import %s. Inbound acknowledgement/action text from a push/on-call provider must pass "+
				"ioscan.EnforceInput before it can drive anything, and this test must be extended to assert "+
				"that — along with the Converse gate, the dashboard role floor, and the proxy mode ladder — "+
				"on the new path (src/docs/v6-readiness.md §2, hivecommons/hive#8048)",
				path, fn.Name.Name, reason, ioscanImport)
			return true
		})
	}
}

// inboundReason reports why fn looks like an inbound entry point, or "" if it
// does not. Two signals: an HTTP server signature, and a name from the
// vocabulary providers use for their callbacks.
func inboundReason(fn *ast.FuncDecl) string {
	if fn.Name.Name == "ServeHTTP" {
		return "serves HTTP requests"
	}
	// http.ResponseWriter, not *http.Request: an outbound sink builds
	// *http.Request values to POST with (doOK does), so only the writer
	// distinguishes serving a request from making one.
	if fn.Type.Params != nil {
		for _, param := range fn.Type.Params.List {
			if typeString(param.Type) == "http.ResponseWriter" {
				return "takes an http.ResponseWriter, so it serves requests"
			}
		}
	}
	lower := strings.ToLower(fn.Name.Name)
	for _, marker := range []string{"webhook", "inbound", "callback", "onaction", "handleaction", "receive", "acknowledge"} {
		if strings.Contains(lower, marker) {
			return "is named like a provider callback (" + marker + ")"
		}
	}
	return ""
}

func typeString(expr ast.Expr) string {
	switch t := expr.(type) {
	case *ast.StarExpr:
		return "*" + typeString(t.X)
	case *ast.SelectorExpr:
		return typeString(t.X) + "." + t.Sel.Name
	case *ast.Ident:
		return t.Name
	}
	return ""
}
