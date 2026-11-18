package authz

import (
	"strings"
	"testing"
)

var testGuards = []string{".github/workflows/", "OWNERS", "policies/"}

func brokerRequest(paths []string, guards []string) Request {
	return Request{
		Actor:    Actor{Agent: "scanner", Identified: true},
		Action:   Action{Kind: KindBranchPush, Surface: SurfaceBroker, Forge: ForgeGitHub},
		Resource: Resource{Repo: "org/name", Branch: "work", Paths: paths},
		Context:  Context{ProtectedPaths: guards},
	}
}

func TestDecideBrokerChangedFiles(t *testing.T) {
	cases := []struct {
		name    string
		paths   []string
		guards  []string
		effect  Effect
		rule    string
		reason  string
		matched int
	}{
		{"empty push is refused first", nil, testGuards, EffectDeny, RuleBrokerEmptyPush, ReasonEmptyPush, 0},
		{"empty push wins over a protected guard", []string{}, testGuards, EffectDeny, RuleBrokerEmptyPush, ReasonEmptyPush, 0},
		{"directory guard covers the tree", []string{".github/workflows/ci.yml"}, testGuards, EffectDeny, RuleBrokerProtectedPath, ReasonProtectedPath, 1},
		{"file guard covers exactly that file", []string{"OWNERS"}, testGuards, EffectDeny, RuleBrokerProtectedPath, ReasonProtectedPath, 1},
		{"file guard does not cover a nested path", []string{"OWNERS/sub.txt"}, testGuards, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
		{"guard without slash does not cover a tree", []string{".github/workflows/ci.yml"}, []string{".github/workflows"}, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
		{"dot-slash prefix is cleaned", []string{"./policies/a.rego"}, testGuards, EffectDeny, RuleBrokerProtectedPath, ReasonProtectedPath, 1},
		{"dot-dot segments are cleaned", []string{"docs/../policies/a.rego"}, testGuards, EffectDeny, RuleBrokerProtectedPath, ReasonProtectedPath, 1},
		{"matching is case sensitive", []string{"owners"}, testGuards, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
		{"a lookalike directory is allowed", []string{"policies-old/a.rego"}, testGuards, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
		{"unprotected push passes", []string{"pkg/safe.go", "README.md"}, testGuards, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
		{"every protected file is counted", []string{"OWNERS", "pkg/safe.go", "policies/x"}, testGuards, EffectDeny, RuleBrokerProtectedPath, ReasonProtectedPath, 2},
		{"no guards protects nothing", []string{"OWNERS"}, nil, EffectAllow, RuleBrokerChangedFilesPass, ReasonOK, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Decide(brokerRequest(tc.paths, tc.guards))
			if got.Effect != tc.effect || got.RuleID != tc.rule || got.ReasonCode != tc.reason || got.MatchedPaths != tc.matched {
				t.Fatalf("Decide = %+v, want effect=%s rule=%s reason=%s matched=%d", got, tc.effect, tc.rule, tc.reason, tc.matched)
			}
			if got.Shadow {
				t.Fatal("Decide must not mark a decision shadow; only a Shadow observer does")
			}
			if !strings.HasPrefix(got.PolicyVersion, FloorVersion+":") {
				t.Fatalf("PolicyVersion = %q, want the floor version prefix", got.PolicyVersion)
			}
		})
	}
}

func TestDecideFailsClosedOnMissingAttribute(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"no repo":         func(r *Request) { r.Resource.Repo = "" },
		"blank repo":      func(r *Request) { r.Resource.Repo = "  " },
		"no branch":       func(r *Request) { r.Resource.Branch = "" },
		"blank branch":    func(r *Request) { r.Resource.Branch = "\t" },
		"no repo, benign": func(r *Request) { r.Resource.Repo = ""; r.Resource.Paths = []string{"a.go"} },
	} {
		t.Run(name, func(t *testing.T) {
			req := brokerRequest([]string{"a.go"}, testGuards)
			mutate(&req)
			got := Decide(req)
			if got.Effect != EffectDeny || got.ReasonCode != ReasonMissingAttribute || got.RuleID != RuleMissingAttribute {
				t.Fatalf("Decide = %+v, want deny missing_attribute", got)
			}
		})
	}
}

func TestDecideUnsupportedActionDenies(t *testing.T) {
	for name, mutate := range map[string]func(*Request){
		"other kind":    func(r *Request) { r.Action.Kind = "pull_request_create" },
		"other surface": func(r *Request) { r.Action.Surface = SurfaceProxy },
		"zero action":   func(r *Request) { r.Action = Action{} },
	} {
		t.Run(name, func(t *testing.T) {
			req := brokerRequest([]string{"a.go"}, testGuards)
			mutate(&req)
			got := Decide(req)
			if got.Effect != EffectDeny || got.ReasonCode != ReasonUnsupportedAction || got.RuleID != RuleUnsupportedAction {
				t.Fatalf("Decide = %+v, want deny unsupported_action", got)
			}
		})
	}
}

func TestDecideWithRecoversPanicAsDeny(t *testing.T) {
	req := brokerRequest([]string{"a.go"}, testGuards)
	got := decideWith(req, func(Request) Decision { panic("evaluator bug") })
	if got.Effect != EffectDeny || got.ReasonCode != ReasonAuthzError || got.RuleID != RuleEvaluatorError {
		t.Fatalf("decideWith = %+v, want deny authz_error", got)
	}
	if got.PolicyVersion == "" {
		t.Fatal("a fail-closed decision still names its policy version")
	}
}

func TestDecideNeverReturnsHoldOrAllowOnError(t *testing.T) {
	for _, req := range []Request{
		{},
		brokerRequest(nil, nil),
		brokerRequest([]string{"a"}, nil),
	} {
		if got := Decide(req); got.Effect == EffectHold {
			t.Fatalf("no built-in rule returns hold in this phase: %+v", got)
		}
	}
	if got := Decide(Request{}); got.Effect != EffectDeny {
		t.Fatalf("the zero request must deny, got %+v", got)
	}
}

func TestPolicyVersionIsOrderIndependentAndInputSensitive(t *testing.T) {
	a := policyVersion(brokerRequest(nil, []string{"x/", "y", "y"}))
	b := policyVersion(brokerRequest(nil, []string{"y", "x/"}))
	if a != b {
		t.Fatalf("order or duplicates changed the version: %q vs %q", a, b)
	}
	if c := policyVersion(brokerRequest(nil, []string{"x/"})); c == a {
		t.Fatalf("a different list must change the version: %q", c)
	}
}

func TestVerdictMatches(t *testing.T) {
	d := Decision{Effect: EffectDeny, ReasonCode: ReasonProtectedPath}
	if !(Verdict{Effect: EffectDeny, ReasonCode: ReasonProtectedPath}).Matches(d) {
		t.Fatal("same effect and reason must match")
	}
	if (Verdict{Effect: EffectDeny, ReasonCode: ReasonEmptyPush}).Matches(d) {
		t.Fatal("a different reason must not match")
	}
	if (Verdict{Effect: EffectAllow, ReasonCode: ReasonProtectedPath}).Matches(d) {
		t.Fatal("a different effect must not match")
	}
}
