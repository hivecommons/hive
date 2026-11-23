package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"testing"

	"github.com/hivecommons/hive/pkg/escalation"
	"github.com/hivecommons/hive/pkg/github"
	"github.com/hivecommons/hive/pkg/github/automerge"
	"github.com/hivecommons/hive/pkg/review"
)

// Two drivers run the stage-label pass: the engine loop (bootAgentsWith,
// main.go) and the governor's automerge sweep (governorAutoMergeOptions,
// main_sweeps.go). The governor's options lacked FixDispatchStatus and
// RebaseEpisode, so its pass routed a hive PR whose fixer gave up to 070
// while the engine loop routed the same head to 085 fixer-exhausted:N; the
// dampener then froze it at 070 with nobody on it
// (projectbluefin/utah-packages#1278, hivecommons/hive#15744).

// stageDriverOnlyAtOneSite lists the automerge.Options fields exactly one
// driver sets, and why. Anything else a driver sets must be set by both,
// or come from applyStageDriverOptions. A field added at one site only
// fails TestStageDriversShareStageOptions until it moves into the helper
// (stage-relevant) or is listed here with its reason (driver-specific).
var stageDriverOnlyAtOneSite = map[string]string{
	// Stage-pass inputs whose source is the driver's own:
	"OpenPRs":          "engine loop: the forgestate cache only bootAgentsWith holds; the governor's pass lists /pulls for the same open set",
	"MutationBoundary": "engine loop: b.mutationBoundary; the governor's engine gets the same boundary from the client (automerge.New falls back to transport.MutationBoundary)",
	// Lane-only (the stage pass does not read them):
	"SweepTrigger":                 "engine loop: event-driven sweep wake-ups",
	"IntegrationSelfAllowed":       "engine loop: integration self-merge admission",
	"ApprovalDesk":                 "engine loop: self-merge approval desk",
	"SelfAuthorizationHoldEnabled": "engine loop: self-authored lane hold",
	"BotTakeoverEnabled":           "engine loop: bot takeover lane",
	"OnTerminalHold":               "engine loop: dependency-bot terminal hold hand-off",
	"IntentGate":                   "engine loop: self-merge intent gate",
	"HotspotRecorder":              "engine loop: merge-hotspot advisor intake",
	"QueueOnApproveAndUnhold":      "governor sweep: queued lane's approve-and-unhold",
}

// optionsFieldsSet returns the automerge.Options fields function fn of
// file sets on variable v (composite-literal keys of an automerge.Options
// assigned to v, and v.Field = …), and whether fn calls
// applyStageDriverOptions(&v, …).
func optionsFieldsSet(t *testing.T, file, fn, v string) (map[string]bool, bool) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	var decl *ast.FuncDecl
	for _, d := range f.Decls {
		if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Name == fn {
			decl = fd
		}
	}
	if decl == nil {
		t.Fatalf("%s: func %s not found", file, fn)
	}
	fields := map[string]bool{}
	callsHelper := false
	ast.Inspect(decl.Body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			for i, lhs := range n.Lhs {
				switch l := lhs.(type) {
				case *ast.Ident:
					if l.Name != v || i >= len(n.Rhs) {
						continue
					}
					lit, ok := n.Rhs[i].(*ast.CompositeLit)
					if !ok {
						continue
					}
					if sel, ok := lit.Type.(*ast.SelectorExpr); !ok || sel.Sel.Name != "Options" {
						continue
					}
					for _, el := range lit.Elts {
						if kv, ok := el.(*ast.KeyValueExpr); ok {
							if key, ok := kv.Key.(*ast.Ident); ok {
								fields[key.Name] = true
							}
						}
					}
				case *ast.SelectorExpr:
					if x, ok := l.X.(*ast.Ident); ok && x.Name == v {
						fields[l.Sel.Name] = true
					}
				}
			}
		case *ast.CallExpr:
			name, ok := n.Fun.(*ast.Ident)
			if !ok || name.Name != "applyStageDriverOptions" || len(n.Args) == 0 {
				return true
			}
			if u, ok := n.Args[0].(*ast.UnaryExpr); ok && u.Op == token.AND {
				if x, ok := u.X.(*ast.Ident); ok && x.Name == v {
					callsHelper = true
				}
			}
		}
		return true
	})
	return fields, callsHelper
}

func sortedFieldNames(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestStageDriversShareStageOptions(t *testing.T) {
	helper, _ := optionsFieldsSet(t, "stage_driver_options.go", "applyStageDriverOptions", "opts")
	engine, engineCalls := optionsFieldsSet(t, "main.go", "bootAgentsWith", "autoMergeOpts")
	governor, governorCalls := optionsFieldsSet(t, "main_sweeps.go", "governorAutoMergeOptions", "opts")
	if !engineCalls {
		t.Error("bootAgentsWith does not call applyStageDriverOptions(&autoMergeOpts, ...): the engine loop's stage pass would drift from the governor's")
	}
	if !governorCalls {
		t.Error("governorAutoMergeOptions does not call applyStageDriverOptions(&opts, ...): the governor's stage pass would drift from the engine loop's")
	}
	// The fields the bug was about.
	for _, want := range []string{"FixDispatchStatus", "RebaseEpisode", "RebaseResolver"} {
		if !helper[want] {
			t.Errorf("applyStageDriverOptions does not set %s", want)
		}
	}
	// A helper field set again at a site would let that site drift.
	for _, f := range sortedFieldNames(helper) {
		if engine[f] {
			t.Errorf("bootAgentsWith sets %s, which applyStageDriverOptions owns", f)
		}
		if governor[f] {
			t.Errorf("governorAutoMergeOptions sets %s, which applyStageDriverOptions owns", f)
		}
	}
	// One site only: must be a listed driver-specific field.
	for _, f := range sortedFieldNames(engine) {
		if !governor[f] {
			if _, ok := stageDriverOnlyAtOneSite[f]; !ok {
				t.Errorf("automerge.Options.%s is set by the engine loop (bootAgentsWith) only: if the stage pass reads it, set it in applyStageDriverOptions; otherwise list it in stageDriverOnlyAtOneSite with its reason", f)
			}
		}
	}
	for _, f := range sortedFieldNames(governor) {
		if !engine[f] {
			if _, ok := stageDriverOnlyAtOneSite[f]; !ok {
				t.Errorf("automerge.Options.%s is set by the governor sweep (governorAutoMergeOptions) only: if the stage pass reads it, set it in applyStageDriverOptions; otherwise list it in stageDriverOnlyAtOneSite with its reason", f)
			}
		}
	}
	// Keep the list honest: every entry is set by exactly one site.
	for f := range stageDriverOnlyAtOneSite {
		if engine[f] == governor[f] {
			t.Errorf("stageDriverOnlyAtOneSite lists %s, but engine loop sets it=%v and governor sweep sets it=%v", f, engine[f], governor[f])
		}
	}
	// Every field named must exist on automerge.Options.
	typ := reflect.TypeOf(automerge.Options{})
	for _, set := range []map[string]bool{helper, engine, governor} {
		for f := range set {
			if _, ok := typ.FieldByName(f); !ok {
				t.Errorf("automerge.Options has no field %s", f)
			}
		}
	}
}

// Both drivers, given the same exhausted fixer on the same head, read it as
// exhausted: the input the stage pass's 085 fixer-exhausted:N route
// (automerge_stage_labels.go, fixerKnown && fixer.Exhausted()) needs. The
// engine loop's options are the ones bootAgentsWith really builds; the
// governor's are governorAutoMergeOptions'.
func TestStageDriversSeeTheSameExhaustedFixer(t *testing.T) {
	redirectReviewPaths(t)
	const head = "873d3e6f00000000000000000000000000000000"
	if err := review.WriteDispatchState("", review.DispatchState{FixAttempts: []review.FixAttempt{{
		Repo: "acme/widgets", Number: 1278, HeadSHA: head, Agent: "fixer", Attempts: escalation.MaxReEngagements,
	}}}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	f := newBootAgentsFake()
	cfg := bootAgentsConfig()
	cfg.GitHub.AppID = 4242
	cfg.GitHub.InstallationID = 99
	cfg.AutoMerge.RequiredChecks = []string{"build"}
	b := newBootAgentsBoot(t, f, cfg)
	b.ghClient = github.NewClientForTest(srv.URL, "acme", []string{"widgets"}, b.logger)
	b.bootAgentsWith(f.deps)
	f.waitRelaysStarted(t)
	f.relayMu.Lock()
	engineOpts := f.sweepOpts
	f.relayMu.Unlock()
	governorOpts := governorAutoMergeOptions(cfg, nil, b.logger)

	helper, _ := optionsFieldsSet(t, "stage_driver_options.go", "applyStageDriverOptions", "opts")
	for name, opts := range map[string]automerge.Options{"engine loop": engineOpts, "governor sweep": governorOpts} {
		// Every callback the shared helper installs is present on both.
		v := reflect.ValueOf(opts)
		for _, field := range sortedFieldNames(helper) {
			fv := v.FieldByName(field)
			switch fv.Kind() {
			case reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer:
				if fv.IsNil() {
					t.Errorf("%s: automerge.Options.%s is nil", name, field)
				}
			}
		}
		if opts.FixDispatchStatus == nil || opts.RebaseEpisode == nil || opts.RebaseResolver == nil {
			t.Fatalf("%s: FixDispatchStatus/RebaseEpisode/RebaseResolver not wired", name)
		}
		st, known := opts.FixDispatchStatus("acme/widgets", 1278, head)
		if !known || !st.Exhausted() || st.Attempts != escalation.MaxReEngagements {
			t.Errorf("%s: fixer status = %+v known=%v, want exhausted after %d attempts (085 fixer-exhausted:%d)", name, st, known, escalation.MaxReEngagements, escalation.MaxReEngagements)
		}
	}
}
