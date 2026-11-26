package hub

import (
	"bytes"
	"errors"
	"io"
	"os"
	"slices"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Paths of the copy-transport manifests (#19621, #19694), relative to this
// package.
const (
	copyAdmissionPath = "../../deploy/k8s/sandbox-job-copy-admission.yaml"
	copyRBACPath      = "../../deploy/k8s/sandbox-job-copy-rbac.yaml"
	// hiveSALabel is the namespace label whose value names hive's service
	// account; the admission policy matches by it instead of a fixed name.
	hiveSALabel = "hivecommons.dev/service-account"
)

// k8sDoc is the part of a manifest these tests read.
type k8sDoc struct {
	Kind     string `yaml:"kind"`
	Metadata struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Rules []struct {
		APIGroups []string `yaml:"apiGroups"`
		Resources []string `yaml:"resources"`
		Verbs     []string `yaml:"verbs"`
	} `yaml:"rules"`
	Spec struct {
		PolicyName        string   `yaml:"policyName"`
		FailurePolicy     string   `yaml:"failurePolicy"`
		ValidationActions []string `yaml:"validationActions"`
		MatchResources    struct {
			NamespaceSelector struct {
				MatchLabels      map[string]string `yaml:"matchLabels"`
				MatchExpressions []struct {
					Key      string `yaml:"key"`
					Operator string `yaml:"operator"`
				} `yaml:"matchExpressions"`
			} `yaml:"namespaceSelector"`
		} `yaml:"matchResources"`
		MatchConstraints struct {
			ResourceRules []struct {
				Resources  []string `yaml:"resources"`
				Operations []string `yaml:"operations"`
			} `yaml:"resourceRules"`
		} `yaml:"matchConstraints"`
		MatchConditions []struct {
			Expression string `yaml:"expression"`
		} `yaml:"matchConditions"`
		Variables []struct {
			Name       string `yaml:"name"`
			Expression string `yaml:"expression"`
		} `yaml:"variables"`
		Validations []struct {
			Expression string `yaml:"expression"`
		} `yaml:"validations"`
	} `yaml:"spec"`
}

func parseDocs(t *testing.T, raw []byte, what string) []k8sDoc {
	t.Helper()
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []k8sDoc
	for {
		var d k8sDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("parse %s: %v", what, err)
		}
		docs = append(docs, d)
	}
	return docs
}

func readDocs(t *testing.T, path string) []k8sDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return parseDocs(t, raw, path)
}

func docOfKind(t *testing.T, docs []k8sDoc, kind, name string) k8sDoc {
	t.Helper()
	for _, d := range docs {
		if d.Kind == kind && (name == "" || d.Metadata.Name == name) {
			return d
		}
	}
	t.Fatalf("no %s %q in manifest", kind, name)
	return k8sDoc{}
}

// TestCopyAdmissionPolicyEnforcesForAnyHiveServiceAccount (#19694 review): the
// admission policy that guards the copy Role is enforcing, and it matches the
// namespace's hive service account through a namespace label, not one
// hard-coded system:serviceaccount:hive:hive that other spokes never match.
func TestCopyAdmissionPolicyEnforcesForAnyHiveServiceAccount(t *testing.T) {
	raw, err := os.ReadFile(copyAdmissionPath)
	if err != nil {
		t.Fatalf("read %s: %v", copyAdmissionPath, err)
	}
	if strings.Contains(string(raw), "system:serviceaccount:hive:") {
		t.Error("the admission policy hard-codes a service account in the hive namespace; match it through the namespace label")
	}
	docs := parseDocs(t, raw, copyAdmissionPath)
	policy := docOfKind(t, docs, "ValidatingAdmissionPolicy", "")
	binding := docOfKind(t, docs, "ValidatingAdmissionPolicyBinding", "")

	if binding.Spec.PolicyName != policy.Metadata.Name {
		t.Errorf("binding policyName = %q, want %q", binding.Spec.PolicyName, policy.Metadata.Name)
	}
	if !slices.Equal(binding.Spec.ValidationActions, []string{"Deny"}) {
		t.Errorf("binding validationActions = %v, want [Deny]: Audit and Warn alone block nothing", binding.Spec.ValidationActions)
	}
	if policy.Spec.FailurePolicy != "Fail" {
		t.Errorf("failurePolicy = %q, want Fail", policy.Spec.FailurePolicy)
	}

	// The binding selects namespaces by the label, not by a fixed name.
	sel := binding.Spec.MatchResources.NamespaceSelector
	if _, byName := sel.MatchLabels["kubernetes.io/metadata.name"]; byName {
		t.Error("binding selects one namespace by name; select by the service-account label instead")
	}
	labelled := false
	for _, e := range sel.MatchExpressions {
		if e.Key == hiveSALabel && e.Operator == "Exists" {
			labelled = true
		}
	}
	if !labelled {
		t.Errorf("binding namespaceSelector = %+v, want %s Exists", sel, hiveSALabel)
	}

	// The service account is derived from the namespace and its label, and
	// every validation applies only to requests from it.
	var user string
	for _, v := range policy.Spec.Variables {
		if v.Name == "hiveUser" {
			user = v.Expression
		}
	}
	for _, want := range []string{"namespaceObject.metadata.name", "namespaceObject.metadata.labels['" + hiveSALabel + "']"} {
		if !strings.Contains(user, want) {
			t.Errorf("hiveUser = %q, want it built from %s", user, want)
		}
	}
	if len(policy.Spec.Validations) == 0 {
		t.Fatal("policy has no validations")
	}
	for i, v := range policy.Spec.Validations {
		if !strings.HasPrefix(strings.TrimSpace(v.Expression), "!variables.fromHive ||") {
			t.Errorf("validation %d = %q, want it scoped to hive's service account", i, v.Expression)
		}
	}

	// Secrets: CREATE and DELETE only, so the hive-secrets-writer patch of
	// hive-secrets (an UPDATE) is never matched; DELETE is matched to deny it.
	for _, r := range policy.Spec.MatchConstraints.ResourceRules {
		if slices.Contains(r.Resources, "secrets") {
			if !slices.Equal(r.Operations, []string{"CREATE", "DELETE"}) {
				t.Errorf("secrets operations = %v, want [CREATE DELETE]", r.Operations)
			}
		}
	}
	all := string(raw)
	for _, want := range []string{"object.metadata.ownerReferences", "p.port == 8477", "object.spec.template == oldObject.spec.template"} {
		if !strings.Contains(all, want) {
			t.Errorf("policy does not check %q", want)
		}
	}
}

// copyRoleVerbs returns resource -> verbs of the hive-sandbox-jobs-copy Role.
func copyRoleVerbs(t *testing.T, docs []k8sDoc) map[string][]string {
	t.Helper()
	role := docOfKind(t, docs, "Role", "hive-sandbox-jobs-copy")
	got := map[string][]string{}
	for _, r := range role.Rules {
		for _, res := range r.Resources {
			got[res] = append(got[res], r.Verbs...)
		}
	}
	return got
}

// wantCopyRoleVerbs is everything the copy transport needs and nothing else:
// the Secret and the policy are owned by the Job and garbage-collected with
// it, so there is no delete (or read) on either.
var wantCopyRoleVerbs = map[string][]string{
	"jobs":            {"patch"},
	"secrets":         {"create"},
	"networkpolicies": {"create"},
}

func assertCopyRoleVerbs(t *testing.T, what string, got map[string][]string) {
	t.Helper()
	if len(got) != len(wantCopyRoleVerbs) {
		t.Errorf("%s grants %v, want exactly %v", what, got, wantCopyRoleVerbs)
	}
	for res, want := range wantCopyRoleVerbs {
		if !slices.Equal(got[res], want) {
			t.Errorf("%s %s verbs = %v, want %v", what, res, got[res], want)
		}
	}
}

// TestCopyRoleGrantsOnlyWhatCopyModeUses (#19694 review): no namespace-wide
// delete on Secrets or NetworkPolicies, in the kustomize sample and in the
// Role the hub provisions.
func TestCopyRoleGrantsOnlyWhatCopyModeUses(t *testing.T) {
	assertCopyRoleVerbs(t, copyRBACPath, copyRoleVerbs(t, readDocs(t, copyRBACPath)))

	var hubRole string
	for _, doc := range strings.Split(k8sManifestTemplate, "\n---\n") {
		if strings.Contains(doc, "kind: Role\n") && strings.Contains(doc, "name: hive-sandbox-jobs-copy\n") {
			hubRole = doc
		}
	}
	if hubRole == "" {
		t.Fatal("the hub manifest template has no hive-sandbox-jobs-copy Role")
	}
	hubRole = strings.ReplaceAll(hubRole, "{{.Namespace}}", "ns")
	assertCopyRoleVerbs(t, "hub hive-sandbox-jobs-copy Role", copyRoleVerbs(t, parseDocs(t, []byte(hubRole), "hub template")))
}
