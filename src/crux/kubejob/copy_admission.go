package kubejob

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
)

// The copy transport's admission check (hivecommons/hive#19971).
//
// The copy Role grants namespace-wide create on Secrets and NetworkPolicies
// and patch on Jobs; only the ValidatingAdmissionPolicy in
// deploy/k8s/sandbox-job-copy-admission.yaml narrows that to hive's own
// per-Job objects, and its binding matches only namespaces labelled
// hivecommons.dev/service-account. A missing policy, binding or label leaves
// the grants unconstrained, so before copy mode runs hive asks the API server
// to create, as a server-side dry run, a Secret the policy must deny. Copy
// mode runs only when the policy denies it; an admitted probe, or one that
// cannot run, refuses copy mode.

const (
	// CopyAdmissionPolicyName is the ValidatingAdmissionPolicy the probe
	// expects to deny it (sandbox-job-copy-admission.yaml).
	CopyAdmissionPolicyName = "hive-sandbox-copy-objects"
	// copyAdmissionProbeName is the probe Secret. It is not hive-kick-* and
	// carries neither hive's label nor an owner, so the policy's first rule
	// denies it.
	copyAdmissionProbeName = "hive-copy-admission-probe"
)

// copyAdmissionVerified holds "<api server>|<namespace>" keys whose probe the
// policy denied. Only a pass is remembered, so a spoke whose operator installs
// the policy later needs no restart, and one that removes it is caught at the
// next boot.
var copyAdmissionVerified sync.Map

// VerifyCopyAdmission probes that the copy transport's admission policy is
// enforced in hive's namespace and logs an ERROR when it is not. Hive calls it
// at boot when an agent uses workspace_transport: copy; every copy kick runs
// the same check before it creates anything.
func (l *Launcher) VerifyCopyAdmission(ctx context.Context) error {
	ns, err := l.namespace()
	if err != nil {
		err = fmt.Errorf("kubejob: workspace_transport: copy is refused: the admission policy check cannot run: %w", err)
		slog.Error("kubejob: refusing the copy workspace transport", "err", err)
		return err
	}
	return l.verifyCopyAdmission(ctx, ns)
}

// verifyCopyAdmission is VerifyCopyAdmission for a known namespace, with the
// pass cached per API server and namespace.
func (l *Launcher) verifyCopyAdmission(ctx context.Context, ns string) error {
	key := l.APIServer + "|" + ns
	if _, ok := copyAdmissionVerified.Load(key); ok {
		return nil
	}
	if err := l.probeCopyAdmission(ctx, ns); err != nil {
		slog.Error("kubejob: refusing the copy workspace transport: its admission policy is not enforced", "namespace", ns, "policy", CopyAdmissionPolicyName, "err", err)
		return err
	}
	copyAdmissionVerified.Store(key, struct{}{})
	return nil
}

// probeCopyAdmission sends the dry-run create and reads the answer: nil only
// when CopyAdmissionPolicyName denied it.
func (l *Launcher) probeCopyAdmission(ctx context.Context, ns string) error {
	probe, err := json.Marshal(map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"type":       "Opaque",
		"metadata":   map[string]any{"name": copyAdmissionProbeName, "namespace": ns},
	})
	if err != nil {
		return err
	}
	path := fmt.Sprintf("/api/v1/namespaces/%s/secrets?dryRun=All", ns)
	_, err = l.do(ctx, http.MethodPost, path, probe)
	fix := fmt.Sprintf("apply deploy/k8s/sandbox-job-copy-admission.yaml and run `kubectl label namespace %s hivecommons.dev/service-account=<hive's service account>`, or use workspace_transport: claim", ns)
	if err == nil {
		return fmt.Errorf("kubejob: workspace_transport: copy is refused: a server-side dry run of a Secret the admission policy %s must deny was admitted in namespace %s, so the copy Role's create and patch grants are unconstrained; %s", CopyAdmissionPolicyName, ns, fix)
	}
	if deniedByCopyAdmission(err.Error()) {
		return nil
	}
	return fmt.Errorf("kubejob: workspace_transport: copy is refused: could not confirm the admission policy %s is enforced in namespace %s (the dry-run probe Secret was refused by something else: %v); %s", CopyAdmissionPolicyName, ns, err, fix)
}

// deniedByCopyAdmission reports whether an API error is the copy admission
// policy's denial, which the API server words as
// "ValidatingAdmissionPolicy '<policy>' with binding '<binding>' denied request: ...".
func deniedByCopyAdmission(msg string) bool {
	return strings.Contains(msg, "ValidatingAdmissionPolicy '"+CopyAdmissionPolicyName+"'") && strings.Contains(msg, "denied request")
}
