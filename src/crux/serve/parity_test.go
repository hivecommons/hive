package serve

import (
	"bytes"
	"context"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/hivecommons/hive/crux"
	"github.com/hivecommons/hive/crux/api"
	"github.com/hivecommons/hive/crux/watchdog"
)

// The parity ratchet of hivecommons/hive#14138 stage 3: every crux.Runtime
// method and every watchdog.Fleet method is either answered by a route in
// api.Routes (served here, called by an api.Client method) or named in the
// exact allowlists below. Both directions fail: a method neither routed nor
// listed is a seam the API cannot carry; a listed method that is now routed
// (or gone from the interface) must be deleted from the list in the same
// change. Each endpoint family that lands deletes its rows; the default can
// flip to sidecar (stage 6) only once both lists are empty or every
// remaining entry is one the sidecar answers locally.

// unroutedRuntimeMethods are the crux.Runtime methods no route answers yet,
// grouped by the interface that declares them.
var unroutedRuntimeMethods = []string{
	// Lifecycle: restart, pause and resume are not routed yet.
	"IsPaused", "Pause", "PauseBy", "ResetRestartCount", "Restart", "RestartWithBootstrap", "Resume",
	// Observer.
	"GetStatusFast",
	// Kicker: async dispatch and kick-log reads.
	"KickDispatchState", "ListKickLogs", "ReadKickLog", "SendKickAsync", "ValidateKick",
	// Definitions (AddAgent is routed by run).
	"ClearModeOverrides", "PinCLI", "PinModel", "ReconcileAgents", "RefreshInferenceRoutes", "RemoveAgent",
	"SetBackendOverride", "SetModelOverride", "SyncModeFiles", "UnpinCLI", "UnpinModel", "UpdateConfig",
	// Fleet: breaker and ACMM level.
	"BreakerState", "EngageBreaker", "GetACMMLevel", "RelaunchBobAgentsAwaitingKey", "ReleaseBreaker",
	"SetACMMLevel", "SharedDevUID",
	// Credentials.
	"ActivateCopilotToken", "ClaudeCredentialCandidatePaths", "CopilotToken", "CopilotTokenSource",
	"ReloadClaudeToken", "SetCopilotToken", "SubmitLoginCode",
	// Resumer: resume with busy/gone/unsupported states.
	"ResumeHandle", "SendResumeKick", "SessionID", "SessionMissing",
	// Supervisor.
	"AgentAuthAvailable", "AgentCapabilities", "AgentCredentialRefreshable", "AgentHasValidCredential",
	"ArchiveAllKickLogs", "BackendAuthAvailable", "CheckAndRestartCrashedAgents", "ConsentWedgedAgents",
	"CountAgentsWithModel", "CrashOOMSuspected", "EffectiveBackend", "GovernorMayStart", "InvocationMetadata",
	"MarkStartupLaunchQueued", "PauseByCause", "ProviderErrorBackoffRemaining", "RecordAudit",
	"RefreshAgentTokenFor", "RefreshAgentTokens", "RestartTelemetry", "SendGitHubAPIBudgetNudge",
	"StartFailureState", "TrajectoryAgents",
	// Restorer: a bulk seed on connect.
	"RestoreBreaker", "SeedKickHistory", "SeedLastKick", "SeedPauseState", "SeedRestartTelemetry", "SeedTurnLoss",
	// Background: loops that run inside the serving process; a sidecar
	// client implements them as no-ops, so they may stay listed.
	"StartAgentTokenRefresh", "StartCopilotSessionRefresh", "StartCredentialWatchdog", "StartTranscriptActivityRefresh",
	// Authorizer: synchronous on the relay request path.
	"AuthorizeIssueOpen", "AuthorizeMerge", "AuthorizePROpen", "AuthorizePushBranch", "AuthorizeReviewRequest",
}

// unroutedFleetMethods are the watchdog.Fleet methods the watchdog family
// does not route. Restart and Pause are Lifecycle actions (they land with
// the lifecycle routes, which delete Restart and Pause above too);
// QueuedWork is the governor's queue depth, a host-side hook
// (crux/runtime.WatchdogHooks.Queued) the runtime never answers.
var unroutedFleetMethods = []string{"Pause", "QueuedWork", "Restart"}

// clientHelpers are the exported *api.Client methods that call no route.
var clientHelpers = map[string]bool{"Address": true, "SetHTTPClient": true}

func methodNames(t reflect.Type) []string {
	out := make([]string, 0, t.NumMethod())
	for i := range t.NumMethod() {
		out = append(out, t.Method(i).Name)
	}
	sort.Strings(out)
	return out
}

// checkRouted compares the methods of iface against the routed set and the
// allowlist, and returns one failure per disagreement.
func checkRouted(iface string, methods []string, routed map[string]bool, allow []string, listName string) []string {
	allowed := map[string]bool{}
	var failures []string
	for _, m := range allow {
		if allowed[m] {
			failures = append(failures, fmt.Sprintf("%s lists %s twice", listName, m))
		}
		allowed[m] = true
	}
	declared := map[string]bool{}
	for _, m := range methods {
		declared[m] = true
		switch {
		case routed[m] && allowed[m]:
			failures = append(failures, fmt.Sprintf(
				"%s.%s is routed by api.Routes now: delete it from %s in crux/serve/parity_test.go", iface, m, listName))
		case !routed[m] && !allowed[m]:
			failures = append(failures, fmt.Sprintf(
				"%s.%s has no API route: add the route to api.Routes with its server handler and api.Client method, "+
					"or, if its endpoint family has not landed yet, add it to %s in crux/serve/parity_test.go", iface, m, listName))
		}
	}
	for _, m := range allow {
		if !declared[m] {
			failures = append(failures, fmt.Sprintf(
				"%s lists %s, which is not a %s method any more: delete the entry", listName, m, iface))
		}
	}
	return failures
}

func TestRuntimeAndFleetMethodsHaveRoutes(t *testing.T) {
	rt := reflect.TypeOf((*crux.Runtime)(nil)).Elem()
	fl := reflect.TypeOf((*watchdog.Fleet)(nil)).Elem()
	routedRuntime, routedFleet := map[string]bool{}, map[string]bool{}
	for _, r := range api.Routes {
		for _, m := range r.Runtime {
			routedRuntime[m] = true
		}
		for _, m := range r.Fleet {
			routedFleet[m] = true
		}
	}
	failures := checkRouted("crux.Runtime", methodNames(rt), routedRuntime, unroutedRuntimeMethods, "unroutedRuntimeMethods")
	failures = append(failures, checkRouted("watchdog.Fleet", methodNames(fl), routedFleet, unroutedFleetMethods, "unroutedFleetMethods")...)
	if len(failures) > 0 {
		t.Fatalf("Crux API parity ratchet (hivecommons/hive#14138 stage 3):\n\n%s\n\n"+
			"Every runtime method a host uses must be reachable over `crux serve` before the sidecar default flips. "+
			"An endpoint family deletes its methods from the allowlist in the PR that routes them; the list never grows.",
			strings.Join(failures, "\n"))
	}
}

func TestRoutesNameRealMethods(t *testing.T) {
	rt := reflect.TypeOf((*crux.Runtime)(nil)).Elem()
	fl := reflect.TypeOf((*watchdog.Fleet)(nil)).Elem()
	client := reflect.TypeOf((*api.Client)(nil))
	seen := map[string]bool{}
	called := map[string]bool{}
	for _, r := range api.Routes {
		key := r.Method + " " + r.Pattern
		if seen[key] {
			t.Errorf("api.Routes lists %s twice", key)
		}
		seen[key] = true
		if len(r.Client) == 0 {
			t.Errorf("%s has no api.Client method", key)
		}
		for _, m := range r.Client {
			called[m] = true
			if _, ok := client.MethodByName(m); !ok {
				t.Errorf("%s names api.Client.%s, which does not exist", key, m)
			}
		}
		if len(r.Runtime)+len(r.Fleet) == 0 {
			t.Errorf("%s names no runtime method", key)
		}
		for _, m := range r.Runtime {
			if _, ok := rt.MethodByName(m); !ok {
				t.Errorf("%s names %s, which is not a crux.Runtime method", key, m)
			}
		}
		for _, m := range r.Fleet {
			if _, ok := fl.MethodByName(m); !ok {
				t.Errorf("%s names %s, which is not a watchdog.Fleet method", key, m)
			}
		}
	}
	for _, m := range methodNames(client) {
		if !called[m] && !clientHelpers[m] {
			t.Errorf("api.Client.%s is in no api.Routes entry: list it on the route it calls", m)
		}
	}
}

// TestRoutesAreServed sends each api.Routes entry to the Handler and checks
// the server answered it (JSON or an event stream) rather than the mux
// (a text/plain 404 or 405), and that Handler registers exactly as many
// routes as the table lists.
func TestRoutesAreServed(t *testing.T) {
	rt := newFakeRuntime()
	rt.add("a", "a1", crux.StateRunning)
	fleet := newFakeFleet()
	fleet.obs["a"] = watchdog.Observation{Backend: "claude"}
	srv := New(rt)
	srv.Watchdog = fleet
	h := srv.Handler()
	for _, r := range api.Routes {
		path := strings.ReplaceAll(r.Pattern, "{name}", "a")
		var body *bytes.Reader
		if r.Method == http.MethodPut {
			body = bytes.NewReader([]byte(`{"conditions":[]}`))
		} else {
			body = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(r.Method, path, body)
		ctx, cancel := context.WithCancel(context.WithValue(req.Context(), networkKey, networkUnix))
		cancel() // ends the event stream as soon as it has started
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req.WithContext(ctx))
		ct := rec.Header().Get("Content-Type")
		if rec.Code == http.StatusMethodNotAllowed || (ct != api.ContentTypeJSON && ct != api.ContentTypeEvent) {
			t.Errorf("%s %s is not served: %d %s %q", r.Method, path, rec.Code, ct, rec.Body)
		}
	}

	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "serve.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	registered := 0
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "Handler" || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "HandleFunc" {
					registered++
				}
			}
			return true
		})
	}
	if registered != len(api.Routes) {
		t.Fatalf("Handler registers %d routes, api.Routes lists %d: add the new route to api.Routes (or remove the stale entry)", registered, len(api.Routes))
	}
}
