package kubejob

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"strings"
	"testing"

	"github.com/google/cel-go/cel"
	"gopkg.in/yaml.v3"

	"github.com/hivecommons/hive/crux/sandbox/substratebridge"
)

// copyAdmissionPolicyPath is the copy transport's admission policy, relative
// to this package (#19694).
const copyAdmissionPolicyPath = "../../deploy/k8s/sandbox-job-copy-admission.yaml"

const (
	celTestNamespace = "spoke"
	celTestSA        = "hive-sa"
	celTestJob       = "hive-kick-hive-deps-sandbox-1"
)

// admissionPolicy is the part of the ValidatingAdmissionPolicy the CEL test
// evaluates.
type admissionPolicy struct {
	Kind string `yaml:"kind"`
	Spec struct {
		Variables []struct {
			Name       string `yaml:"name"`
			Expression string `yaml:"expression"`
		} `yaml:"variables"`
		Validations []struct {
			Expression string `yaml:"expression"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}

// loadAdmissionPrograms compiles every validation of the policy, with its
// variables inlined, against object, oldObject, request and namespaceObject.
func loadAdmissionPrograms(t *testing.T) []cel.Program {
	t.Helper()
	raw, err := os.ReadFile(copyAdmissionPolicyPath)
	if errors.Is(err, os.ErrNotExist) {
		t.Skipf("%s not present (crux tested outside the hive tree)", copyAdmissionPolicyPath)
	}
	if err != nil {
		t.Fatal(err)
	}
	var policy admissionPolicy
	for _, doc := range strings.Split(string(raw), "\n---\n") {
		var p admissionPolicy
		if err := yaml.Unmarshal([]byte(doc), &p); err != nil {
			t.Fatalf("parse %s: %v", copyAdmissionPolicyPath, err)
		}
		if p.Kind == "ValidatingAdmissionPolicy" {
			policy = p
		}
	}
	if len(policy.Spec.Validations) == 0 {
		t.Fatal("no validations in the admission policy")
	}
	env, err := cel.NewEnv(
		cel.Variable("object", cel.DynType),
		cel.Variable("oldObject", cel.DynType),
		cel.Variable("request", cel.DynType),
		cel.Variable("namespaceObject", cel.DynType),
	)
	if err != nil {
		t.Fatal(err)
	}
	inline := func(expr string) string {
		// Variables may name earlier ones; substitute until none is left.
		for i := 0; i < len(policy.Spec.Variables)+1 && strings.Contains(expr, "variables."); i++ {
			for _, v := range policy.Spec.Variables {
				expr = strings.ReplaceAll(expr, "variables."+v.Name, "("+v.Expression+")")
			}
		}
		return expr
	}
	var progs []cel.Program
	for i, v := range policy.Spec.Validations {
		expr := inline(v.Expression)
		if strings.Contains(expr, "variables.") {
			t.Fatalf("validation %d names an undefined variable: %s", i, v.Expression)
		}
		ast, iss := env.Compile(expr)
		if iss.Err() != nil {
			t.Fatalf("validation %d does not compile: %v\n%s", i, iss.Err(), v.Expression)
		}
		prg, err := env.Program(ast)
		if err != nil {
			t.Fatalf("validation %d: %v", i, err)
		}
		progs = append(progs, prg)
	}
	return progs
}

// asObject round-trips v through JSON, as the API server would see it, with
// integral numbers as ints.
func asObject(t *testing.T, v any) any {
	t.Helper()
	if v == nil {
		return nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return integral(out)
}

func integral(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, e := range x {
			x[k] = integral(e)
		}
	case []any:
		for i, e := range x {
			x[i] = integral(e)
		}
	case float64:
		if x == math.Trunc(x) {
			return int64(x)
		}
	}
	return v
}

// admitted evaluates every validation; an evaluation error denies, as
// failurePolicy: Fail does.
func admitted(t *testing.T, progs []cel.Program, user, op, resource string, object, oldObject any) bool {
	t.Helper()
	vars := map[string]any{
		"object":    asObject(t, object),
		"oldObject": asObject(t, oldObject),
		"request": map[string]any{
			"userInfo":  map[string]any{"username": user},
			"operation": op,
			"namespace": celTestNamespace,
			"resource":  map[string]any{"resource": resource},
		},
		"namespaceObject": map[string]any{"metadata": map[string]any{
			"name":   celTestNamespace,
			"labels": map[string]any{"hivecommons.dev/service-account": celTestSA},
		}},
	}
	for _, prg := range progs {
		out, _, err := prg.Eval(vars)
		if err != nil {
			return false
		}
		if ok, isBool := out.Value().(bool); !isBool || !ok {
			return false
		}
	}
	return true
}

// mutate returns a JSON copy of v after edit.
func mutate(t *testing.T, v any, edit func(m map[string]any)) map[string]any {
	t.Helper()
	m, _ := asObject(t, v).(map[string]any)
	edit(m)
	return m
}

func specOf(m map[string]any) map[string]any {
	s, _ := m["spec"].(map[string]any)
	return s
}

// TestCopyAdmissionPolicyCEL (#19694 review): the policy's CEL, never run on
// a cluster, admits every object hive actually sends on both transports and
// denies the abuses it claims to block.
func TestCopyAdmissionPolicyCEL(t *testing.T) {
	progs := loadAdmissionPrograms(t)
	hive := "system:serviceaccount:" + celTestNamespace + ":" + celTestSA
	plan := &copyPlan{token: "t", hiveLabels: map[string]string{"app": "hive"}}

	secret := plan.tokenSecret(celTestNamespace, celTestJob)
	ownedBy(secret, celTestJob, "uid-1")
	denyAll := denyAllPolicy(celTestNamespace, celTestJob)
	copyNone := denyAllPolicy(celTestNamespace, celTestJob)
	plan.allowBridgeIngress(copyNone)
	ingressOnly := plan.ingressOnlyPolicy(celTestNamespace, celTestJob)
	for _, p := range []map[string]any{copyNone, ingressOnly} {
		ownedBy(p, celTestJob, "uid-1")
	}
	job := (&Launcher{Options: Options{WorkspaceClaim: "hive-data"}}).manifest(celTestNamespace, celTestJob, "agents/sandbox/x", specFor("/data/agents/sandbox/x"))
	suspended := mutate(t, job, func(m map[string]any) { specOf(m)["suspend"] = true })
	resumed := mutate(t, job, func(m map[string]any) { specOf(m)["suspend"] = false })

	allow := []struct {
		name, op, resource string
		object, oldObject  any
	}{
		{"token Secret create", "CREATE", "secrets", secret, nil},
		{"claim deny-all policy create", "CREATE", "networkpolicies", denyAll, nil},
		{"copy network-none policy create", "CREATE", "networkpolicies", copyNone, nil},
		{"copy ingress-only policy create", "CREATE", "networkpolicies", ingressOnly, nil},
		{"claim policy delete", "DELETE", "networkpolicies", nil, denyAll},
		{"job resume", "UPDATE", "jobs", resumed, suspended},
	}
	for _, c := range allow {
		if !admitted(t, progs, hive, c.op, c.resource, c.object, c.oldObject) {
			t.Errorf("%s: denied, want admitted", c.name)
		}
	}

	deny := []struct {
		name, op, resource string
		object, oldObject  any
	}{
		{"Secret delete", "DELETE", "secrets", nil, secret},
		{"Secret without owner", "CREATE", "secrets", mutate(t, secret, func(m map[string]any) {
			delete(m["metadata"].(map[string]any), "ownerReferences")
		}), nil},
		{"service-account-token Secret", "CREATE", "secrets", mutate(t, secret, func(m map[string]any) { m["type"] = "kubernetes.io/service-account-token" }), nil},
		{"Secret not named hive-kick-*", "CREATE", "secrets", mutate(t, secret, func(m map[string]any) { m["metadata"].(map[string]any)["name"] = "hive-secrets" }), nil},
		{"Secret without hive's label", "CREATE", "secrets", mutate(t, secret, func(m map[string]any) { delete(m["metadata"].(map[string]any), "labels") }), nil},
		{"allow-all podSelector", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) { specOf(m)["podSelector"] = map[string]any{} }), nil},
		{"selector of another Job", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["podSelector"] = map[string]any{"matchLabels": map[string]any{jobLabel: "hive-kick-other"}}
		}), nil},
		{"egress rule", "CREATE", "networkpolicies", mutate(t, copyNone, func(m map[string]any) { specOf(m)["egress"] = []any{map[string]any{}} }), nil},
		{"egress-only policy", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) { specOf(m)["policyTypes"] = []any{"Egress"} }), nil},
		{"ingress from any pod", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["ingress"] = []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{}}}, "ports": []any{map[string]any{"port": substratebridge.DefaultPort}}}}
		}), nil},
		{"ingress from an ipBlock", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["ingress"] = []any{map[string]any{"from": []any{map[string]any{"ipBlock": map[string]any{"cidr": "0.0.0.0/0"}}}, "ports": []any{map[string]any{"port": substratebridge.DefaultPort}}}}
		}), nil},
		{"ingress with no ports", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["ingress"] = []any{map[string]any{"from": []any{map[string]any{"podSelector": map[string]any{"matchLabels": map[string]any{"app": "hive"}}}}}}
		}), nil},
		{"port range", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["ingress"].([]any)[0].(map[string]any)["ports"] = []any{map[string]any{"port": substratebridge.DefaultPort, "endPort": 65535}}
		}), nil},
		{"UDP port", "CREATE", "networkpolicies", mutate(t, ingressOnly, func(m map[string]any) {
			specOf(m)["ingress"].([]any)[0].(map[string]any)["ports"] = []any{map[string]any{"port": substratebridge.DefaultPort, "protocol": "UDP"}}
		}), nil},
		{"policy update", "UPDATE", "networkpolicies", ingressOnly, ingressOnly},
		{"policy delete not hive's", "DELETE", "networkpolicies", nil, mutate(t, denyAll, func(m map[string]any) { m["metadata"].(map[string]any)["name"] = "default-deny" })},
		{"job template change", "UPDATE", "jobs", mutate(t, resumed, func(m map[string]any) {
			specOf(m)["template"].(map[string]any)["spec"].(map[string]any)["hostNetwork"] = true
		}), suspended},
		{"job that was not suspended", "UPDATE", "jobs", resumed, resumed},
		{"job suspended by hive", "UPDATE", "jobs", suspended, resumed},
	}
	for _, c := range deny {
		if admitted(t, progs, hive, c.op, c.resource, c.object, c.oldObject) {
			t.Errorf("%s: admitted, want denied", c.name)
		}
	}

	// Only hive's own service account is checked: the same allow-all policy
	// from another account (Argo CD, the hub, another namespace's hive) passes.
	allowAll := mutate(t, ingressOnly, func(m map[string]any) { specOf(m)["podSelector"] = map[string]any{} })
	for _, user := range []string{"system:serviceaccount:argocd:argocd-application-controller", "system:serviceaccount:other:" + celTestSA, "system:serviceaccount:" + celTestNamespace + ":default"} {
		if !admitted(t, progs, user, "CREATE", "networkpolicies", allowAll, nil) {
			t.Errorf("request from %s denied; the policy must apply only to hive's service account", user)
		}
	}
}
